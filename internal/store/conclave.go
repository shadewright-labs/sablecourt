package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/shadewright-labs/sablecourt/internal/domain"
)

var (
	ErrConclaveClosed   = errors.New("conclave is closed")
	ErrConclavePaused   = errors.New("conclave is paused by a ward")
	ErrWardTripped      = errors.New("conclave ward tripped")
	ErrConclaveRunLimit = errors.New("conclave run limit reached")
)

const conclaveDeadline = "wall_time_reached"

type ConclaveLimits struct {
	MaxMessages   int
	MaxPingPong   int
	MaxRuns       int
	MaxAge        time.Duration
	Now           func() time.Time
	Budget        *domain.PerTaskBudget
	BudgetMissing bool
}

type conclavePostKey struct{}
type conclavePost struct {
	limits  ConclaveLimits
	applied bool
	reason  string
}

// AppendConclavePost carries the guard through Store decorators to the SQL backend.
func AppendConclavePost(ctx context.Context, st Store, msg domain.ConclaveMessage, limits ConclaveLimits) (int64, string, error) {
	post := &conclavePost{limits: limits}
	id, err := st.AppendConclaveMessage(context.WithValue(ctx, conclavePostKey{}, post), msg)
	if err == nil && !post.applied {
		return 0, "", errors.New("conclave store did not apply the message guard")
	}
	return id, post.reason, err
}

// AppendGuardedConclavePost handles broker writes. Bare Store appends are also
// used for importing stored messages and do not carry a guard.
func AppendGuardedConclavePost(ctx context.Context, db *sql.DB, msg domain.ConclaveMessage) (bool, int64, error) {
	post, ok := ctx.Value(conclavePostKey{}).(*conclavePost)
	if !ok {
		return false, 0, nil
	}
	id, reason, err := appendConclavePost(ctx, db, msg, post.limits)
	post.applied, post.reason = true, reason
	return true, id, err
}

func appendConclavePost(ctx context.Context, db *sql.DB, msg domain.ConclaveMessage, limits ConclaveLimits) (int64, string, error) {
	tx, room, err := lockConclave(ctx, db, msg.ConclaveID)
	if err != nil {
		return 0, "", err
	}
	defer tx.Rollback()

	if room.State != domain.ConclaveStateActive && room.State != domain.ConclaveStateInputRequired {
		return 0, "", ErrConclaveClosed
	}
	operator := msg.FromRole == "operator"
	if room.State == domain.ConclaveStateInputRequired && (!operator || room.WardReason == conclaveDeadline) {
		return 0, "", ErrConclavePaused
	}

	now := limits.Now().UTC()
	state := domain.ConclaveStateActive
	reason := ""
	var decree int64
	var count int
	var refusal error
	switch {
	case conclaveExpired(room, limits, now):
		reason = conclaveDeadline
	case limits.BudgetMissing:
		reason = "budget_evidence_unavailable"
	default:
		if !operator {
			decree, count, err = conclaveMessageCount(ctx, tx, msg.ConclaveID)
			if err != nil {
				return 0, "", err
			}
			if count >= limits.MaxMessages {
				reason = fmt.Sprintf("max_messages_reached(%d)", limits.MaxMessages)
			}
		}
	}
	if reason != "" {
		refusal = fmt.Errorf("%w: %s", ErrWardTripped, reason)
	}

	var id int64
	if refusal == nil {
		id, err = insertConclaveMessage(ctx, tx, msg)
		if err != nil {
			return 0, "", err
		}
		reason, err = conclaveBudgetWard(ctx, tx, msg.ConclaveID, limits.Budget)
		if err != nil {
			return 0, "", err
		}
		if !operator && reason == "" {
			reason, err = conclaveMessageWard(ctx, tx, msg.ConclaveID, decree, count+1, limits)
			if err != nil {
				return 0, "", err
			}
		}
	}
	if reason != "" {
		state = domain.ConclaveStateInputRequired
		_, err = insertConclaveMessage(ctx, tx, domain.ConclaveMessage{
			ConclaveID: msg.ConclaveID, FromRole: "system", ToRole: "operator", Kind: "system",
			Body: fmt.Sprintf("Ward tripped (%s). Conclave paused.", reason), CreatedAt: now,
		})
		if err != nil {
			return 0, "", err
		}
	}
	_, err = tx.ExecContext(ctx, `
		UPDATE conclaves SET state = $1, ward_reason = $2, updated_at = $3 WHERE id = $4
	`, string(state), reason, now.Format(time.RFC3339Nano), msg.ConclaveID)
	if err != nil {
		return 0, "", err
	}
	if err := tx.Commit(); err != nil {
		return 0, "", err
	}
	return id, reason, refusal
}

type conclaveTurnKey struct{}
type conclaveTurn struct {
	id      string
	key     string
	limits  ConclaveLimits
	applied bool
}

// SubmitConclaveTurn binds a task submission and its guard to the same call.
func SubmitConclaveTurn(ctx context.Context, id, key string, limits ConclaveLimits, submit func(context.Context) (domain.Task, error)) (domain.Task, error) {
	turn := &conclaveTurn{id: id, key: key, limits: limits}
	task, err := submit(context.WithValue(ctx, conclaveTurnKey{}, turn))
	if err == nil && !turn.applied {
		return domain.Task{}, errors.New("conclave store did not apply the turn guard")
	}
	return task, err
}

type TaskInserter interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

// InsertTask applies the council guard to the task insert in the same transaction.
// The persisted task is the run reservation. Failed inserts consume no slots.
func InsertTask(ctx context.Context, db *sql.DB, task domain.Task, insert func(TaskInserter) error) error {
	turn, ok := ctx.Value(conclaveTurnKey{}).(*conclaveTurn)
	if !ok {
		return insert(db)
	}
	turn.applied = true
	if task.IdempotencyKey != turn.key || task.ParentTaskID != "" ||
		(!strings.HasPrefix(turn.key, "conclave-"+turn.id+"-init-") &&
			!strings.HasPrefix(turn.key, "conclave-"+turn.id+"-msg-")) {
		return errors.New("task does not match the conclave turn reservation")
	}
	tx, room, err := lockConclave(ctx, db, turn.id)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// The reconciler handles idempotent conflicts after this transaction ends.
	var existing int
	err = tx.QueryRowContext(ctx, "SELECT 1 FROM tasks WHERE idempotency_key = $1", turn.key).Scan(&existing)
	if err == nil {
		return ErrConflict
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	switch room.State {
	case domain.ConclaveStateActive:
	case domain.ConclaveStateInputRequired:
		return ErrConclavePaused
	default:
		return ErrConclaveClosed
	}

	limits := turn.limits
	if conclaveExpired(room, limits, limits.Now()) || limits.BudgetMissing {
		return ErrConclavePaused
	}
	decree, count, err := conclaveMessageCount(ctx, tx, turn.id)
	if err != nil {
		return err
	}
	reason, err := conclaveMessageWard(ctx, tx, turn.id, decree, count, limits)
	if err != nil {
		return err
	}
	if reason != "" {
		return ErrConclavePaused
	}
	reason, err = conclaveBudgetWard(ctx, tx, turn.id, limits.Budget)
	if err != nil {
		return err
	}
	if reason != "" {
		return ErrConclavePaused
	}

	// The existing idempotency namespace also accounts for pre-upgrade turns.
	init, mentions := conclaveTaskPatterns(turn.id)
	var runs int
	err = tx.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM tasks WHERE parent_task_id = '' AND
			(idempotency_key LIKE $1 ESCAPE '!' OR idempotency_key LIKE $2 ESCAPE '!')
	`, init, mentions).Scan(&runs)
	if err != nil {
		return err
	}
	if runs >= limits.MaxRuns {
		return ErrConclaveRunLimit
	}
	if err := insert(tx); err != nil {
		return err
	}
	return tx.Commit()
}

func lockConclave(ctx context.Context, db *sql.DB, id string) (*sql.Tx, domain.ConclaveSession, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, domain.ConclaveSession{}, err
	}
	var room domain.ConclaveSession
	var created string
	// PostgreSQL locks this row. In SQLite the first statement takes the write lock.
	err = tx.QueryRowContext(ctx, `
		UPDATE conclaves SET updated_at = updated_at WHERE id = $1
		RETURNING state, ward_reason, created_at
	`, id).Scan(&room.State, &room.WardReason, &created)
	if err == nil {
		room.CreatedAt, err = time.Parse(time.RFC3339Nano, created)
	}
	if err != nil {
		_ = tx.Rollback()
		if errors.Is(err, sql.ErrNoRows) {
			err = ErrNotFound
		}
		return nil, domain.ConclaveSession{}, err
	}
	return tx, room, nil
}

func conclaveExpired(room domain.ConclaveSession, limits ConclaveLimits, now time.Time) bool {
	return limits.MaxAge > 0 && !now.Before(room.CreatedAt.Add(limits.MaxAge))
}

func insertConclaveMessage(ctx context.Context, tx *sql.Tx, msg domain.ConclaveMessage) (int64, error) {
	var id int64
	err := tx.QueryRowContext(ctx, `
		INSERT INTO conclave_messages (conclave_id, from_role, to_role, kind, body, task_id, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7) RETURNING id
	`, msg.ConclaveID, msg.FromRole, msg.ToRole, msg.Kind, msg.Body, msg.TaskID, msg.CreatedAt.UTC().Format(time.RFC3339Nano)).Scan(&id)
	return id, err
}

func conclaveMessageCount(ctx context.Context, tx *sql.Tx, id string) (int64, int, error) {
	var decree int64
	if err := tx.QueryRowContext(ctx, `
		SELECT COALESCE((
			SELECT id FROM conclave_messages WHERE conclave_id = $1 AND from_role = 'operator'
			ORDER BY id DESC LIMIT 1
		), 0)
	`, id).Scan(&decree); err != nil {
		return 0, 0, err
	}
	var count int
	err := tx.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM conclave_messages
		WHERE conclave_id = $1 AND id > $2 AND from_role NOT IN ('operator', 'system')
	`, id, decree).Scan(&count)
	return decree, count, err
}

func conclaveMessageWard(ctx context.Context, tx *sql.Tx, id string, decree int64, count int, limits ConclaveLimits) (string, error) {
	if count >= limits.MaxMessages {
		return fmt.Sprintf("max_messages_reached(%d)", limits.MaxMessages), nil
	}
	if count < limits.MaxPingPong {
		return "", nil
	}
	rows, err := tx.QueryContext(ctx, `
		SELECT DISTINCT from_role FROM (
			SELECT from_role FROM conclave_messages
			WHERE conclave_id = $1 AND id > $2 AND from_role NOT IN ('operator', 'system')
			ORDER BY id DESC LIMIT $3
		) AS recent LIMIT 3
	`, id, decree, limits.MaxPingPong)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	var roles int
	for rows.Next() {
		roles++
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	if roles <= 2 {
		return fmt.Sprintf("ping_pong_detected(%d)", limits.MaxPingPong), nil
	}
	return "", nil
}

func conclaveTaskPatterns(id string) (string, string) {
	escaped := strings.NewReplacer("!", "!!", "%", "!%", "_", "!_").Replace(id)
	return "conclave-" + escaped + "-init-%", "conclave-" + escaped + "-msg-%"
}

func conclaveBudgetWard(ctx context.Context, tx *sql.Tx, id string, budget *domain.PerTaskBudget) (string, error) {
	if budget == nil {
		return "", nil
	}
	init, mentions := conclaveTaskPatterns(id)
	var totals domain.BudgetTotals
	err := tx.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(input_tokens), 0), COALESCE(SUM(output_tokens), 0), COALESCE(SUM(cost_microusd), 0)
		FROM budget_ledger WHERE task_id IN (
			SELECT id FROM tasks WHERE parent_task_id = '' AND
				(idempotency_key LIKE $1 ESCAPE '!' OR idempotency_key LIKE $2 ESCAPE '!')
		)
	`, init, mentions).Scan(&totals.InputTokens, &totals.OutputTokens, &totals.CostMicroUSD)
	if err != nil {
		return "", err
	}
	cost := float64(totals.CostMicroUSD) / 1_000_000
	switch {
	case budget.MaxCostUSD > 0 && cost >= budget.MaxCostUSD:
		return fmt.Sprintf("budget_exceeded($%.4f >= $%.2f)", cost, budget.MaxCostUSD), nil
	case budget.MaxInputTokens > 0 && totals.InputTokens >= budget.MaxInputTokens:
		return fmt.Sprintf("input_tokens_exceeded(%d >= %d)", totals.InputTokens, budget.MaxInputTokens), nil
	case budget.MaxOutputTokens > 0 && totals.OutputTokens >= budget.MaxOutputTokens:
		return fmt.Sprintf("output_tokens_exceeded(%d >= %d)", totals.OutputTokens, budget.MaxOutputTokens), nil
	default:
		return "", nil
	}
}
