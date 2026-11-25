package store

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"math"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/shadewright-labs/sablecourt/internal/domain"
)

// CouncilStore is an optional capability. Callers must acquire it before opening a council.
type CouncilStore interface {
	CreateCouncil(context.Context, domain.ConclaveSession, CouncilSpec) error
	CouncilByIdempotencyKey(context.Context, string) (domain.ConclaveSession, string, error)
	ListReconcileCouncils(context.Context) ([]string, error)
	CouncilSnapshot(context.Context, string) (CouncilSnapshot, error)
	AcceptCouncilAnswer(context.Context, string, int64, bool) error
	RecordCouncilAnswer(context.Context, domain.ConclaveMessage, bool) (int64, string, error)
	CloseCouncil(context.Context, string, domain.ConclaveState, string) (domain.ConclaveRelic, error)
	MarkCouncilCleanedUp(context.Context, string) error
	PutCouncilRelic(context.Context, string, domain.ConclaveRelic) error
}

var ErrCouncilUnsupported = errors.New("store does not support atomic council reservations")

func Councils(st Store) (CouncilStore, error) {
	if c, ok := st.(CouncilStore); ok {
		return c, nil
	}
	return nil, ErrCouncilUnsupported
}

type CouncilMember struct {
	Vassal   domain.ConclaveVassal
	Zone     domain.WorkloadZone
	RoleCard string
}

// Source identifies the daemon-selected checkout. An empty ref and commit select the working tree.
type CouncilCheckout struct {
	Source, Ref, Commit, WriterRole string
	SourceDev                       uint64 `json:",omitempty"`
	SourceIno                       uint64 `json:",omitempty"`
}

type CouncilSpec struct {
	Question             string
	Members              []CouncilMember
	Sequential           bool
	Checkout             CouncilCheckout
	MaxRounds, MaxStarts *int
	Duration             *time.Duration
	Wards                domain.ConclaveWards
	Budget               *domain.PerTaskBudget
	IdempotencyKey       string
	RequestSHA256        string
}

type CouncilPolicy struct {
	Question             string
	Members              []CouncilMember
	Sequential           bool
	Checkout             CouncilCheckout
	MaxRounds, MaxStarts int
	Deadline             time.Time
	Wards                domain.ConclaveWards
	Budget               *domain.PerTaskBudget
}

type CouncilTurn struct {
	Round        int
	Role         string
	Starts       int
	TaskID       string
	State        domain.TaskState
	Answer       *domain.ConclaveMessage
	NothingToAdd bool
}

type CouncilAttempt struct {
	Round        int
	Role         string
	Number       int
	TaskID       string
	State        domain.TaskState
	Answer       *domain.ConclaveMessage
	NothingToAdd bool
	Relics       []domain.ConclaveRelic
}

type CouncilSnapshot struct {
	CouncilPolicy
	Session        domain.ConclaveSession
	Decrees        []domain.ConclaveMessage
	Turns          []CouncilTurn
	Attempts       []CouncilAttempt
	CleanupPending bool
}

// CouncilPolicyReader loads the pinned policy in the transaction holding the room lock.
// A nil reader denotes a backend that cannot create councils.
type CouncilPolicyReader func(context.Context, *sql.Tx, string) (*CouncilPolicy, error)

const (
	MaxCouncilText             = 64 << 10
	CouncilDigestRelicName     = "digest"
	MaxCouncilRelicName        = 255
	MaxCouncilRelicsPerAttempt = 16
)

// ValidCouncilText reports whether text is nonblank, valid UTF-8, and within MaxCouncilText.
func ValidCouncilText(s string) bool {
	return len(s) <= MaxCouncilText && utf8.ValidString(s) && strings.TrimSpace(s) != ""
}

func (room lockedConclave) limits(limits ConclaveLimits) ConclaveLimits {
	if p := room.policy; p != nil {
		limits.Now = time.Now
		limits.MaxMessages = lowerCeiling(limits.MaxMessages, p.Wards.MaxMessages)
		limits.MaxPingPong = lowerCeiling(limits.MaxPingPong, p.Wards.MaxPingPong)
		limits.MaxRuns = lowerCeiling(limits.MaxRuns, p.Wards.MaxRuns)
		switch b := limits.Budget; {
		case b != nil && (math.IsNaN(b.MaxCostUSD) || b.MaxCostUSD < 0 || b.MaxInputTokens < 0 || b.MaxOutputTokens < 0):
			limits.BudgetMissing = true
		case b == nil:
			limits.Budget = p.Budget
		case p.Budget != nil:
			limits.Budget = &domain.PerTaskBudget{
				MaxCostUSD:      lowerCeiling(b.MaxCostUSD, p.Budget.MaxCostUSD),
				MaxInputTokens:  lowerCeiling(b.MaxInputTokens, p.Budget.MaxInputTokens),
				MaxOutputTokens: lowerCeiling(b.MaxOutputTokens, p.Budget.MaxOutputTokens),
			}
		}
	}
	return limits
}

func lowerCeiling[T cmp.Ordered](a, b T) T {
	var zero T
	if a == zero {
		return b
	}
	if b == zero {
		return a
	}
	return min(a, b)
}

func reserveCouncilAttempt(ctx context.Context, tx *sql.Tx, room lockedConclave, task domain.Task, a ConclaveAssignment) error {
	p := room.policy
	seat := slices.IndexFunc(p.Members, func(m CouncilMember) bool { return m.Vassal.Role == a.Role })
	if seat < 0 || p.Members[seat].Vassal.Workload != task.WorkloadName || task.State != domain.TaskStateSubmitted ||
		strings.TrimSpace(task.ID) == "" || a.Round > p.MaxRounds {
		return errors.New("invalid council task or seat")
	}

	if a.Round > 1 {
		var completed, stopVotes int
		if err := tx.QueryRowContext(ctx, `
			SELECT COUNT(*), COALESCE(SUM(nothing_to_add),0) FROM council_attempts a JOIN tasks t ON t.id=a.task_id
			WHERE council_id=$1 AND round=$2 AND answer_id IS NOT NULL AND t.state='completed'
		`, room.ID, a.Round-1).Scan(&completed, &stopVotes); err != nil {
			return err
		}
		if completed != len(p.Members) || stopVotes == completed {
			return errors.New("previous council round is incomplete or unanimous")
		}
	}
	if p.Sequential && seat > 0 {
		var ready bool
		err := tx.QueryRowContext(ctx, `
			SELECT EXISTS(SELECT 1 FROM council_attempts a JOIN tasks t ON t.id=a.task_id
			WHERE council_id=$1 AND round=$2 AND role=$3 AND answer_id IS NOT NULL AND t.state='completed')
		`, room.ID, a.Round, p.Members[seat-1].Vassal.Role).Scan(&ready)
		if err != nil {
			return err
		}
		if !ready {
			return errors.New("council predecessor has no completed answer")
		}
	}
	var previous int
	var state domain.TaskState
	var answered bool
	err := tx.QueryRowContext(ctx, `
		SELECT attempt,t.state,answer_id IS NOT NULL FROM council_attempts a JOIN tasks t ON t.id=a.task_id
		WHERE council_id=$1 AND round=$2 AND role=$3 ORDER BY attempt DESC LIMIT 1
	`, room.ID, a.Round, a.Role).Scan(&previous, &state, &answered)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	retryable := !answered && (state == domain.TaskStateFailed || state == domain.TaskStateCancelled || state == domain.TaskStateCompleted)
	if a.Attempt != previous+1 || previous > 0 && !retryable {
		return errors.New("council attempt is stale or turn cannot retry")
	}
	res, err := tx.ExecContext(ctx, `
		INSERT INTO council_attempts(council_id, round, role, attempt, task_id)
		SELECT $1, $2, $3, $4, $5 FROM workloads WHERE name=$6 AND zone=$7
	`, room.ID, a.Round, a.Role, a.Attempt, task.ID, task.WorkloadName, p.Members[seat].Zone)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err == nil && n != 1 {
		return errors.New("registered workload does not match the pinned seat zone")
	}
	return err
}

// reservedCouncilAnswer checks the current reservation before the message consumes a Ward.
func reservedCouncilAnswer(ctx context.Context, tx *sql.Tx, room lockedConclave, msg domain.ConclaveMessage, vote bool) (int64, error) {
	if room.policy == nil {
		return 0, ErrCouncilUnsupported
	}
	if msg.Kind != "review" || !ValidCouncilText(msg.Body) || RedactCouncilAnswer(msg.Body) != msg.Body || msg.CreatedAt.IsZero() {
		return 0, ErrConflict
	}
	seat := slices.IndexFunc(room.policy.Members, func(m CouncilMember) bool { return m.Vassal.Role == msg.FromRole })
	if seat < 0 {
		return 0, ErrConflict
	}
	routes := room.policy.Members[seat].Vassal.CanMessage
	if !slices.Contains(routes, msg.ToRole) && !slices.Contains(routes, "all") {
		return 0, ErrConflict
	}
	if msg.ToRole != "operator" && msg.ToRole != "all" && !slices.ContainsFunc(room.policy.Members, func(m CouncilMember) bool { return m.Vassal.Role == msg.ToRole }) {
		return 0, ErrConflict
	}
	var answer sql.NullInt64
	var savedVote bool
	err := tx.QueryRowContext(ctx, `
		SELECT a.answer_id,a.nothing_to_add FROM council_attempts a JOIN tasks t ON t.id=a.task_id
		WHERE a.task_id=$1 AND a.council_id=$2 AND a.role=$3 AND t.state='completed'
		AND NOT EXISTS (SELECT 1 FROM council_attempts newer WHERE newer.council_id=a.council_id
			AND newer.round=a.round AND newer.role=a.role AND newer.attempt>a.attempt)
	`, msg.TaskID, room.ID, msg.FromRole).Scan(&answer, &savedVote)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrConflict
	}
	if err != nil {
		return 0, err
	}
	if answer.Valid && savedVote != vote {
		return 0, ErrConflict
	}

	// A previous version could commit the exact exit-stamped post before linking it.
	var id int64
	err = tx.QueryRowContext(ctx, `
		SELECT id FROM conclave_messages WHERE conclave_id=$1 AND task_id=$2 AND from_role=$3 AND to_role=$4
		AND kind=$5 AND body=$6 AND created_at=$7 AND ($8 IS NULL OR id=$8) ORDER BY id LIMIT 1
	`, room.ID, msg.TaskID, msg.FromRole, msg.ToRole, msg.Kind, msg.Body, msg.CreatedAt.UTC().Format(time.RFC3339Nano), answer).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		if answer.Valid {
			return 0, ErrConflict
		}
		return 0, nil
	}
	if err != nil || answer.Valid {
		return id, err
	}
	if room.State != domain.ConclaveStateActive && room.State != domain.ConclaveStateInputRequired {
		return 0, ErrConclaveClosed
	}
	if err := linkCouncilFinal(ctx, tx, msg.TaskID, id, vote); err != nil {
		return 0, err
	}
	return id, tx.Commit()
}

func linkCouncilFinal(ctx context.Context, tx *sql.Tx, task string, id int64, vote bool) error {
	res, err := tx.ExecContext(ctx, "UPDATE council_attempts SET answer_id=$1,nothing_to_add=$2,policy_routed=1 WHERE task_id=$3 AND answer_id IS NULL", id, vote, task)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err == nil && n != 1 {
		return ErrConflict
	}
	return err
}
