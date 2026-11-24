package store

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/shadewright-labs/sablecourt/internal/domain"
)

// CouncilStore is an optional capability. Callers must acquire it before opening a council.
type CouncilStore interface {
	CreateCouncil(context.Context, domain.ConclaveSession, CouncilSpec) error
	CouncilSnapshot(context.Context, string) (CouncilSnapshot, error)
	AcceptCouncilAnswer(context.Context, string, int64, bool) error
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
	Session  domain.ConclaveSession
	Decrees  []domain.ConclaveMessage
	Turns    []CouncilTurn
	Attempts []CouncilAttempt
}

// CouncilPolicyReader loads the pinned policy in the transaction holding the room lock.
// A nil reader denotes a backend that cannot create councils.
type CouncilPolicyReader func(context.Context, *sql.Tx, string) (*CouncilPolicy, error)

func (room lockedConclave) limits(limits ConclaveLimits) ConclaveLimits {
	if p := room.policy; p != nil {
		limits.Now = time.Now
		limits.MaxMessages = lowerCeiling(limits.MaxMessages, p.Wards.MaxMessages)
		limits.MaxPingPong = lowerCeiling(limits.MaxPingPong, p.Wards.MaxPingPong)
		limits.MaxRuns = lowerCeiling(limits.MaxRuns, p.Wards.MaxRuns)
		switch {
		case limits.Budget == nil:
			limits.Budget = p.Budget
		case p.Budget != nil:
			limits.Budget = &domain.PerTaskBudget{
				MaxCostUSD:      lowerCeiling(limits.Budget.MaxCostUSD, p.Budget.MaxCostUSD),
				MaxInputTokens:  lowerCeiling(limits.Budget.MaxInputTokens, p.Budget.MaxInputTokens),
				MaxOutputTokens: lowerCeiling(limits.Budget.MaxOutputTokens, p.Budget.MaxOutputTokens),
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
	if a.Attempt != previous+1 || previous > 0 && (answered || state != domain.TaskStateFailed && state != domain.TaskStateCancelled) {
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
