package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/shadewright-labs/sablecourt/internal/domain"
)

var (
	ErrConclaveClosed   = errors.New("conclave is closed")
	ErrConclavePaused   = errors.New("conclave paused by a ward")
	ErrWardTripped      = errors.New("conclave ward tripped")
	ErrConclaveRunLimit = errors.New("conclave run limit reached")
)

const (
	conclaveDeadline    = "wall_time_reached"
	ConclaveMaxRunsWard = "max_runs"
	ConclaveWallTime    = 15 * time.Minute
)

type ConclaveLimits struct {
	MaxMessages   int
	MaxPingPong   int
	MaxRuns       int
	MaxAge        time.Duration
	Now           func() time.Time
	Budget        *domain.PerTaskBudget
	BudgetMissing bool
}

func LimitsForConclave(tpl domain.Conclave, hasTemplate bool, budgets map[string]domain.Budget, now func() time.Time) ConclaveLimits {
	wards := tpl.Spec.Wards
	seats := len(tpl.Spec.Vassals)
	if !hasTemplate || seats == 0 {
		seats = 3
	}
	limits := ConclaveLimits{MaxMessages: wards.MaxMessages, MaxPingPong: wards.MaxPingPong, MaxRuns: wards.MaxRuns, Now: now}
	if limits.MaxMessages <= 0 {
		limits.MaxMessages = 2 * seats
	}
	if limits.MaxPingPong <= 0 {
		limits.MaxPingPong = 2 * seats
	}
	if limits.MaxRuns <= 0 {
		limits.MaxRuns = domain.ConclaveRunCeiling
	}
	if hasTemplate {
		limits.MaxAge = ConclaveWallTime
	}
	if tpl.Spec.BudgetRef != "" {
		budget, ok := budgets[tpl.Spec.BudgetRef]
		limits.BudgetMissing = !ok
		if ok {
			limits.Budget = &budget.Spec.PerTask
		}
	}
	return limits
}

const councilKeyPrefix = "conclave-round:"

const MaxConclaveTurnInput = 1 << 20

type ConclaveAssignment struct {
	Round      int                       `json:"round,omitempty"`
	Attempt    int                       `json:"attempt,omitempty"`
	ConclaveID string                    `json:"conclaveId"`
	Role       string                    `json:"role"`
	MessageID  int64                     `json:"messageId,omitempty"`
	Activation domain.ConclaveActivation `json:"activation"`
}

func (a ConclaveAssignment) TurnKey() string {
	if a.Round != 0 || a.Attempt != 0 {
		return fmt.Sprintf(councilKeyPrefix+"%d:%s:%d:%d:%s", len(a.ConclaveID), a.ConclaveID, a.Round, a.Attempt, a.Role)
	}
	if a.MessageID == 0 {
		return fmt.Sprintf("conclave-%s-init-%s", a.ConclaveID, a.Role)
	}
	return fmt.Sprintf("conclave-%s-msg-%d-%s", a.ConclaveID, a.MessageID, a.Role)
}

func (a ConclaveAssignment) RetryKey(taskID string, attempt int) string {
	return fmt.Sprintf("conclave-%s-retry-%s-%d", a.ConclaveID, taskID, attempt)
}

func TaskConclaveAssignment(task domain.Task) (ConclaveAssignment, bool) {
	if task.ParentTaskID != "" || !strings.HasPrefix(task.IdempotencyKey, "conclave-") || len(task.InputJSON) > MaxConclaveTurnInput {
		return ConclaveAssignment{}, false
	}
	if strings.HasPrefix(task.IdempotencyKey, councilKeyPrefix) {
		var input struct {
			ConclaveAssignment
			RetriedFrom string `json:"retried_from"`
			RetryCount  int    `json:"retry_count"`
		}
		if json.Unmarshal([]byte(task.InputJSON), &input) == nil {
			a := input.ConclaveAssignment
			if a.Round > 0 && a.Attempt > 0 && a.ConclaveID != "" && a.Role != "" && a.MessageID == 0 && a.Activation == "" &&
				input.RetriedFrom == "" && input.RetryCount == 0 && a.TurnKey() == task.IdempotencyKey {
				return a, true
			}
		}
	}
	// Ordinary workloads may use round and attempt in their own input.
	var input struct {
		ConclaveID  string                    `json:"conclaveId"`
		Role        string                    `json:"role"`
		MessageID   int64                     `json:"messageId"`
		Activation  domain.ConclaveActivation `json:"activation"`
		RetriedFrom string                    `json:"retried_from"`
		RetryCount  int                       `json:"retry_count"`
	}
	if json.Unmarshal([]byte(task.InputJSON), &input) != nil {
		return ConclaveAssignment{}, false
	}
	a := ConclaveAssignment{ConclaveID: input.ConclaveID, Role: input.Role, MessageID: input.MessageID, Activation: input.Activation}
	if a.ConclaveID == "" || a.Role == "" ||
		!(a.Activation == domain.ActivationAlwaysOn && a.MessageID == 0 ||
			a.Activation == domain.ActivationOnMention && a.MessageID > 0) {
		return ConclaveAssignment{}, false
	}
	key := a.TurnKey()
	if input.RetriedFrom != "" && input.RetryCount > 0 {
		key = a.RetryKey(input.RetriedFrom, input.RetryCount)
	}
	return a, task.IdempotencyKey == key
}

type conclavePostKey struct{}
type conclavePost struct {
	answerVote   *bool
	limits       ConclaveLimits
	resume       bool
	wake         bool
	report       bool
	relic        *domain.ConclaveRelic
	store        Store
	budgetReason string
	applied      bool
	reason       string
	retryAt      time.Time
}

// AppendConclavePost carries the guard through Store decorators to the SQL backend.
func AppendConclavePost(ctx context.Context, st Store, msg domain.ConclaveMessage, limits ConclaveLimits, resume, wake bool) (int64, string, time.Time, error) {
	post := &conclavePost{limits: limits, resume: resume, wake: wake}
	return runConclavePost(ctx, st, msg, post)
}

// AppendReservedCouncilAnswer uses the same room lock and Wards as other posts.
func AppendReservedCouncilAnswer(ctx context.Context, st Store, msg domain.ConclaveMessage, vote bool) (int64, string, error) {
	id, reason, _, err := runConclavePost(ctx, st, msg, &conclavePost{answerVote: &vote})
	return id, reason, err
}

// AppendConclaveReport checks the Wards before recording a verification report and its relic.
func AppendConclaveReport(ctx context.Context, st Store, msg domain.ConclaveMessage, limits ConclaveLimits, relic *domain.ConclaveRelic) (int64, string, error) {
	if relic != nil && (relic.ConclaveID != msg.ConclaveID || relic.AuthorRole != msg.FromRole) {
		return 0, "", errors.New("conclave report and relic must have the same room and author")
	}
	post := &conclavePost{limits: limits, report: true, relic: relic, store: st}
	if limits.Budget != nil {
		totals, err := conclaveReportBudgetTotals(ctx, st, msg.ConclaveID)
		if err != nil {
			return 0, "", err
		}
		post.budgetReason = conclaveBudgetReason(totals, *limits.Budget)
	}
	id, reason, _, err := runConclavePost(ctx, st, msg, post)
	return id, reason, err
}

func conclaveReportBudgetTotals(ctx context.Context, st Store, id string) (domain.BudgetTotals, error) {
	tasks, err := st.ListTasks(ctx)
	if err != nil {
		return domain.BudgetTotals{}, err
	}
	var sum domain.BudgetTotals
	for _, task := range tasks {
		assignment, ok := TaskConclaveAssignment(task)
		if !ok || assignment.ConclaveID != id {
			continue
		}
		totals, err := st.TaskBudgetTotals(ctx, task.ID)
		if err != nil {
			return domain.BudgetTotals{}, err
		}
		sum.InputTokens += totals.InputTokens
		sum.OutputTokens += totals.OutputTokens
		sum.CostMicroUSD += totals.CostMicroUSD
	}
	return sum, nil
}

func runConclavePost(ctx context.Context, st Store, msg domain.ConclaveMessage, post *conclavePost) (int64, string, time.Time, error) {
	id, err := st.AppendConclaveMessage(context.WithValue(ctx, conclavePostKey{}, post), msg)
	if err == nil && !post.applied {
		return 0, "", time.Time{}, errors.New("conclave store did not apply the message guard")
	}
	return id, post.reason, post.retryAt, err
}

// AppendGuardedConclavePost handles broker writes. Bare Store appends are also
// used for importing stored messages and do not carry a guard.
func AppendGuardedConclavePost(ctx context.Context, db *sql.DB, policy CouncilPolicyReader, msg domain.ConclaveMessage, audit func(context.Context, *sql.Tx, domain.AuditRecord) error) (bool, int64, error) {
	post, ok := ctx.Value(conclavePostKey{}).(*conclavePost)
	if !ok {
		return false, 0, nil
	}
	id, err := appendConclavePost(ctx, db, policy, msg, post, audit)
	post.applied = true
	return true, id, err
}

func appendConclavePost(ctx context.Context, db *sql.DB, policy CouncilPolicyReader, msg domain.ConclaveMessage, post *conclavePost, audit func(context.Context, *sql.Tx, domain.AuditRecord) error) (int64, error) {
	tx, room, err := lockConclave(ctx, db, policy, msg.ConclaveID)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	if post.answerVote != nil {
		id, err := reservedCouncilAnswer(ctx, tx, room, msg, *post.answerVote)
		if err != nil || id != 0 {
			return id, err
		}
	}

	if room.State != domain.ConclaveStateActive && room.State != domain.ConclaveStateInputRequired {
		return 0, ErrConclaveClosed
	}
	operator := msg.FromRole == "operator"
	if room.policy != nil && operator && (msg.Kind == "decree" || msg.Kind == "decision") {
		if !ValidCouncilText(msg.Body) || msg.ToRole != "all" && !slices.ContainsFunc(room.policy.Members, func(m CouncilMember) bool {
			return m.Vassal.Role == msg.ToRole
		}) {
			return 0, errors.New("invalid operator decree")
		}
	}
	limits := room.limits(post.limits)
	now := limits.Now().UTC()
	expired := conclaveExpired(room, limits, now)
	paused := room.State == domain.ConclaveStateInputRequired
	var admitted bool
	if !operator && msg.TaskID != "" && (expired || paused && room.WardReason == ConclaveMaxRunsWard) {
		admitted, err = admittedConclaveReply(ctx, tx, msg)
		if err != nil {
			return 0, err
		}
	}
	if paused && !(operator && post.resume && room.WardReason != conclaveDeadline ||
		admitted && (room.WardReason == ConclaveMaxRunsWard || room.WardReason == conclaveDeadline)) {
		// A post refused at the message ceiling also carries the persisted Ward reason.
		if strings.HasPrefix(room.WardReason, "max_messages_reached(") {
			return 0, fmt.Errorf("%w: %w: %s", ErrConclavePaused, ErrWardTripped, room.WardReason)
		}
		return 0, ErrConclavePaused
	}

	// An unfinished dispatch is replayed against the original message.
	var id, decree int64
	if post.wake {
		var created string
		err = tx.QueryRowContext(ctx, `
			SELECT id, created_at FROM conclave_messages
			WHERE conclave_id = $1 AND from_role = $2 AND to_role = $3 AND kind = $4 AND body = $5 AND task_id = $6
				AND wake_pending = 1 AND id >= (SELECT COALESCE(MAX(id), 0) FROM conclave_messages
					WHERE conclave_id = $1 AND from_role = 'operator')
			ORDER BY id DESC LIMIT 1
		`, msg.ConclaveID, msg.FromRole, msg.ToRole, msg.Kind, msg.Body, msg.TaskID).Scan(&id, &created)
		if err == nil {
			if paused {
				return 0, ErrConclavePaused
			}
			post.retryAt, err = time.Parse(time.RFC3339Nano, created)
			return id, err
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return 0, err
		}
	}

	var count int
	if !operator {
		decree, count, err = conclaveMessageCount(ctx, tx, msg.ConclaveID)
		if err != nil {
			return 0, err
		}
	}
	reason := ""
	var refusal bool
	switch {
	case expired:
		reason, refusal = conclaveDeadline, !admitted
	case limits.BudgetMissing:
		reason, refusal = "budget_evidence_unavailable", true
	case paused && !operator:
		reason = room.WardReason
	}
	if !operator && count >= limits.MaxMessages {
		if reason == "" {
			reason = fmt.Sprintf("max_messages_reached(%d)", limits.MaxMessages)
		}
		refusal = true
	}
	if post.report && !refusal {
		reason = post.budgetReason
		if reason == "" {
			// Usage can grow between the Store evidence reads and this transaction.
			reason, err = conclaveBudgetWard(ctx, tx, room, limits.Budget)
			if err != nil {
				return 0, err
			}
		}
		if reason == "" {
			reason, err = conclaveMessageWard(ctx, tx, msg.ConclaveID, decree, count, limits)
			if err != nil {
				return 0, err
			}
		}
		refusal = reason != ""
	}
	if !refusal {
		if post.relic != nil {
			if err := putConclaveReportRelic(ctx, db, tx, post.store, *post.relic); err != nil {
				return 0, err
			}
		}
		id, err = insertConclaveMessage(ctx, tx, msg)
		if err != nil {
			return 0, err
		}
		if post.answerVote != nil {
			if err := linkCouncilFinal(ctx, tx, msg.TaskID, id, *post.answerVote); err != nil {
				return 0, err
			}
		}

		if reason == "" && !post.report {
			reason, err = conclaveBudgetWard(ctx, tx, room, limits.Budget)
			if err != nil {
				return 0, err
			}
		}
		if !operator && reason == "" {
			reason, err = conclaveMessageWard(ctx, tx, msg.ConclaveID, decree, count+1, limits)
			if err != nil {
				return 0, err
			}
		}
	}
	if post.wake && id != 0 && reason == "" {
		if _, err := tx.ExecContext(ctx, "UPDATE conclave_messages SET wake_pending = 1 WHERE id = $1", id); err != nil {
			return 0, err
		}
	}
	switch {
	case reason != "":
		if err := pauseConclave(ctx, tx, room, reason, now); err != nil {
			return 0, err
		}
	case paused: // A successful operator decree opens one new run window.
		runs, err := conclaveRunCount(ctx, tx, room)
		if err != nil {
			return 0, err
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE conclaves SET state = $1, ward_reason = '', updated_at = $2, run_baseline = $3 WHERE id = $4
		`, string(domain.ConclaveStateActive), now.Format(time.RFC3339Nano), runs, room.ID); err != nil {
			return 0, err
		}
		if err := audit(ctx, tx, domain.AuditRecord{
			Category: "conclave", Action: "conclave.run_window_reset", Outcome: "active", CreatedAt: now,
			DetailJSON: fmt.Sprintf(`{"conclave_id":%q,"decree_id":%d,"previous_runs":%d,"max_runs":%d}`, room.ID, id, runs, limits.MaxRuns),
		}); err != nil {
			return 0, err
		}
	default:
		if _, err := tx.ExecContext(ctx, "UPDATE conclaves SET updated_at = $1 WHERE id = $2", now.Format(time.RFC3339Nano), room.ID); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	post.reason = reason
	if refusal {
		return 0, fmt.Errorf("%w: %s", ErrWardTripped, reason)
	}
	return id, nil
}

type conclaveReportRelicKey struct{}

type conclaveReportRelic struct {
	db      *sql.DB
	tx      *sql.Tx
	relic   domain.ConclaveRelic
	applied bool
}

func putConclaveReportRelic(ctx context.Context, db *sql.DB, tx *sql.Tx, st Store, relic domain.ConclaveRelic) error {
	write := &conclaveReportRelic{db: db, tx: tx, relic: relic}
	err := st.PutConclaveRelic(context.WithValue(ctx, conclaveReportRelicKey{}, write), relic)
	if err == nil && !write.applied {
		return errors.New("conclave store did not apply the report relic guard")
	}
	return err
}

// PutGuardedConclaveReportRelic writes the relic in the guarded report transaction.
func PutGuardedConclaveReportRelic(ctx context.Context, db *sql.DB, relic domain.ConclaveRelic) (bool, error) {
	write, ok := ctx.Value(conclaveReportRelicKey{}).(*conclaveReportRelic)
	if !ok {
		return false, nil
	}
	if write.db != db || write.relic != relic {
		return true, errors.New("conclave report relic does not match the guarded write")
	}
	res, err := write.tx.ExecContext(ctx, `
		INSERT INTO conclave_relics (conclave_id, name, author_role, content_type, body, sha256, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		ON CONFLICT (conclave_id, name) DO NOTHING
	`, relic.ConclaveID, relic.Name, relic.AuthorRole, relic.ContentType, relic.Body, relic.SHA256, relic.CreatedAt.UTC().Format(time.RFC3339Nano))
	if err != nil {
		return true, err
	}
	inserted, err := res.RowsAffected()
	if err != nil {
		return true, err
	}
	if inserted == 0 {
		var saved domain.ConclaveRelic
		if err := write.tx.QueryRowContext(ctx, `
			SELECT author_role, content_type, body, sha256 FROM conclave_relics WHERE conclave_id = $1 AND name = $2
		`, relic.ConclaveID, relic.Name).Scan(&saved.AuthorRole, &saved.ContentType, &saved.Body, &saved.SHA256); err != nil {
			return true, err
		}
		if saved.AuthorRole != relic.AuthorRole || saved.ContentType != relic.ContentType || saved.Body != relic.Body || saved.SHA256 != relic.SHA256 {
			return true, ErrConflict
		}
	}
	write.applied = true
	return true, nil
}

func admittedConclaveReply(ctx context.Context, tx *sql.Tx, msg domain.ConclaveMessage) (bool, error) {
	task := domain.Task{ID: msg.TaskID}
	err := tx.QueryRowContext(ctx, `
		SELECT parent_task_id, idempotency_key, input_json FROM tasks WHERE id = $1
	`, task.ID).Scan(&task.ParentTaskID, &task.IdempotencyKey, &task.InputJSON)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	a, ok := TaskConclaveAssignment(task)
	return ok && a.ConclaveID == msg.ConclaveID && a.Role == msg.FromRole, nil
}

func FinishConclaveWake(ctx context.Context, db *sql.DB, id int64) error {
	_, err := db.ExecContext(ctx, "UPDATE conclave_messages SET wake_pending = 0 WHERE id = $1", id)
	return err
}

func pauseConclave(ctx context.Context, tx *sql.Tx, room lockedConclave, reason string, now time.Time) error {
	if room.State == domain.ConclaveStateInputRequired && room.WardReason == reason {
		return nil
	}
	if _, err := insertConclaveMessage(ctx, tx, domain.ConclaveMessage{
		ConclaveID: room.ID, FromRole: "system", ToRole: "operator", Kind: "system",
		Body: fmt.Sprintf("Ward tripped (%s). Conclave paused.", reason), CreatedAt: now,
	}); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `
		UPDATE conclaves SET state = $1, ward_reason = $2, updated_at = $3 WHERE id = $4
	`, string(domain.ConclaveStateInputRequired), reason, now.Format(time.RFC3339Nano), room.ID)
	return err
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
func InsertTask(ctx context.Context, db *sql.DB, policy CouncilPolicyReader, task domain.Task, insert func(TaskInserter) error) error {
	turn, ok := ctx.Value(conclaveTurnKey{}).(*conclaveTurn)
	if !ok && strings.HasPrefix(task.IdempotencyKey, councilKeyPrefix) {
		if a, assigned := TaskConclaveAssignment(task); !assigned || a.Round != 0 {
			return errors.New("council attempt requires a reservation")
		}
	}
	if entry, hasMemory := ctx.Value(taskMemoryKey{}).(domain.MemoryEntry); hasMemory {
		if ok || entry.Key != task.ID {
			return errors.New("invalid task registration")
		}
		return insertRegisteredTask(ctx, db, entry, insert)
	}
	if !ok {
		return insert(db)
	}
	turn.applied = true
	a, assigned := TaskConclaveAssignment(task)
	if !assigned || a.ConclaveID != turn.id || task.IdempotencyKey != turn.key {
		return errors.New("task does not match the conclave turn reservation")
	}
	tx, room, err := lockConclave(ctx, db, policy, turn.id)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if (room.policy != nil) != (a.Round > 0) {
		return errors.New("turn does not match room mode")
	}
	if room.policy == nil {
		if err := checkTurnKey(ctx, tx, turn.key); err != nil {
			return err
		}
	}
	switch room.State {
	case domain.ConclaveStateActive:
	case domain.ConclaveStateInputRequired:
		return ErrConclavePaused
	default:
		return ErrConclaveClosed
	}

	if room.policy != nil && room.WardReason != "" {
		return ErrConclavePaused
	}
	limits := room.limits(turn.limits)
	now := limits.Now().UTC()
	if conclaveExpired(room, limits, now) {
		return refuseTurn(ctx, tx, room, conclaveDeadline, now, ErrConclavePaused)
	}
	if limits.BudgetMissing {
		if room.policy != nil {
			return refuseTurn(ctx, tx, room, "budget_evidence_unavailable", now, ErrConclavePaused)
		}
		return ErrConclavePaused
	}
	decree, count, err := conclaveMessageCount(ctx, tx, turn.id)
	if err != nil {
		return err
	}
	reason, err := conclaveMessageWard(ctx, tx, turn.id, decree, count, limits)
	if err == nil && reason == "" {
		reason, err = conclaveBudgetWard(ctx, tx, room, limits.Budget)
	}
	if err != nil {
		return err
	}
	if reason != "" {
		if room.policy != nil {
			return refuseTurn(ctx, tx, room, reason, now, ErrConclavePaused)
		}
		return ErrConclavePaused
	}

	runs, err := conclaveRunCount(ctx, tx, room)
	if err != nil {
		return err
	}
	if room.policy != nil && runs >= room.policy.MaxStarts {
		return ErrConclaveRunLimit
	}
	if runs-room.baseline >= min(limits.MaxRuns, domain.ConclaveRunCeiling) {
		return refuseTurn(ctx, tx, room, ConclaveMaxRunsWard, now, ErrConclaveRunLimit)
	}
	if room.policy != nil {
		if err := checkTurnKey(ctx, tx, turn.key); err != nil {
			return err
		}
		if err := reserveCouncilAttempt(ctx, tx, room, task, a); err != nil {
			return err
		}
	}
	if err := insert(tx); err != nil {
		return err
	}
	if room.baseline != 0 {
		if _, err := tx.ExecContext(ctx, "UPDATE tasks SET conclave_run_baseline = $1 WHERE id = $2", room.baseline, task.ID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func refuseTurn(ctx context.Context, tx *sql.Tx, room lockedConclave, reason string, now time.Time, refusal error) error {
	if err := pauseConclave(ctx, tx, room, reason, now); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	return refusal
}

// The reconciler recovers an existing task on ErrConflict.
func checkTurnKey(ctx context.Context, tx *sql.Tx, key string) error {
	var existing int
	err := tx.QueryRowContext(ctx, "SELECT 1 FROM tasks WHERE idempotency_key = $1", key).Scan(&existing)
	if err == nil {
		return ErrConflict
	}
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	return err
}

// HasConclaveReservation reports whether submission is inside the council guard.
func HasConclaveReservation(ctx context.Context, key string) bool {
	turn, ok := ctx.Value(conclaveTurnKey{}).(*conclaveTurn)
	return ok && turn.key == key
}

type conclaveClaimKey struct{}
type conclaveClaim struct {
	roomID, taskID     string
	now                func() time.Time
	applied, cancelled bool
}

// ClaimConclaveTask checks room state and deadline inside the task claim transaction.
func ClaimConclaveTask(ctx context.Context, st Store, taskID, roomID string, now func() time.Time) error {
	claim := &conclaveClaim{roomID: roomID, taskID: taskID, now: now}
	ctx = context.WithValue(ctx, conclaveClaimKey{}, claim)
	err := st.UpdateTaskState(ctx, taskID, domain.TaskStateSubmitted, domain.TaskStateWorking, "dispatch_claimed", `{}`)
	if err != nil {
		return err
	}
	if !claim.applied {
		return errors.New("conclave store did not apply the claim guard")
	}
	if claim.cancelled {
		return ErrConflict
	}
	return nil
}

func BeginTaskTransition(ctx context.Context, db *sql.DB, policy CouncilPolicyReader, to domain.TaskState, reason string) (*sql.Tx, domain.TaskState, string, error) {
	claim, guarded := ctx.Value(conclaveClaimKey{}).(*conclaveClaim)
	if !guarded {
		tx, err := db.BeginTx(ctx, nil)
		return tx, to, reason, err
	}
	tx, room, err := lockConclave(ctx, db, policy, claim.roomID)
	if err != nil {
		return nil, to, reason, err
	}
	claim.applied = true
	now := room.limits(ConclaveLimits{Now: claim.now}).Now().UTC()
	if room.State != domain.ConclaveStateActive {
		reason = "conclave_inactive"
	}
	if (room.State == domain.ConclaveStateActive || room.State == domain.ConclaveStateInputRequired) &&
		conclaveExpired(room, ConclaveLimits{MaxAge: ConclaveWallTime}, now) {
		reason = conclaveDeadline
		if err := pauseConclave(ctx, tx, room, reason, now); err != nil {
			_ = tx.Rollback()
			return nil, to, reason, err
		}
	}
	if reason == "dispatch_claimed" && room.baseline != 0 {
		var reservedBaseline int
		if err := tx.QueryRowContext(ctx, "SELECT conclave_run_baseline FROM tasks WHERE id = $1", claim.taskID).Scan(&reservedBaseline); err != nil {
			_ = tx.Rollback()
			return nil, to, reason, err
		}
		if reservedBaseline != room.baseline {
			reason = "conclave_run_window_ended"
		}
	}
	if reason != "dispatch_claimed" {
		claim.cancelled = true
		to = domain.TaskStateCancelled
	}
	return tx, to, reason, nil
}

type lockedConclave struct {
	domain.ConclaveSession
	baseline int
	policy   *CouncilPolicy
}

func lockConclave(ctx context.Context, db *sql.DB, policy CouncilPolicyReader, id string) (*sql.Tx, lockedConclave, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, lockedConclave{}, err
	}
	var room lockedConclave
	room.ID = id
	var created string
	// PostgreSQL locks this row. In SQLite the first statement takes the write lock.
	err = tx.QueryRowContext(ctx, `
		UPDATE conclaves SET updated_at = updated_at WHERE id = $1
		RETURNING state, ward_reason, created_at, run_baseline
	`, id).Scan(&room.State, &room.WardReason, &created, &room.baseline)
	if err == nil {
		room.CreatedAt, err = time.Parse(time.RFC3339Nano, created)
	}
	if err == nil && policy != nil {
		room.policy, err = policy(ctx, tx, id)
	}
	if err != nil {
		_ = tx.Rollback()
		if errors.Is(err, sql.ErrNoRows) {
			err = ErrNotFound
		}
		return nil, lockedConclave{}, err
	}
	return tx, room, nil
}

func conclaveExpired(room lockedConclave, limits ConclaveLimits, now time.Time) bool {
	if room.policy != nil {
		return !now.Before(room.policy.Deadline)
	}
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

func conclaveRunCount(ctx context.Context, tx *sql.Tx, room lockedConclave) (int, error) {
	if room.policy != nil {
		var n int
		err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM council_attempts WHERE council_id=$1", room.ID).Scan(&n)
		return n, err
	}
	id := room.ID
	init, mentions, retries := conclaveTaskPatterns(id)
	var runs int
	err := tx.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM tasks WHERE parent_task_id = '' AND
			(idempotency_key LIKE $1 ESCAPE '!' OR idempotency_key LIKE $2 ESCAPE '!' OR idempotency_key LIKE $3 ESCAPE '!')
	`, init, mentions, retries).Scan(&runs)
	return runs, err
}

func conclaveTaskPatterns(id string) (string, string, string) {
	prefix := "conclave-" + strings.NewReplacer("!", "!!", "%", "!%", "_", "!_").Replace(id)
	return prefix + "-init-%", prefix + "-msg-%", prefix + "-retry-%"
}

func conclaveBudgetWard(ctx context.Context, tx *sql.Tx, room lockedConclave, budget *domain.PerTaskBudget) (string, error) {
	if budget == nil {
		return "", nil
	}
	var totals domain.BudgetTotals
	tasks := `SELECT id FROM tasks WHERE parent_task_id = '' AND
		(idempotency_key LIKE $1 ESCAPE '!' OR idempotency_key LIKE $2 ESCAPE '!' OR idempotency_key LIKE $3 ESCAPE '!')`
	init, mentions, retries := conclaveTaskPatterns(room.ID)
	args := []any{init, mentions, retries}
	if room.policy != nil {
		tasks = "SELECT task_id FROM council_attempts WHERE council_id=$1"
		args = []any{room.ID}
	}
	err := tx.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(input_tokens), 0), COALESCE(SUM(output_tokens), 0), COALESCE(SUM(cost_microusd), 0)
		FROM budget_ledger WHERE task_id IN (`+tasks+")", args...).Scan(&totals.InputTokens, &totals.OutputTokens, &totals.CostMicroUSD)
	if err != nil {
		return "", err
	}
	return conclaveBudgetReason(totals, *budget), nil
}

func conclaveBudgetReason(totals domain.BudgetTotals, budget domain.PerTaskBudget) string {
	cost := float64(totals.CostMicroUSD) / 1_000_000
	switch {
	case budget.MaxCostUSD > 0 && cost >= budget.MaxCostUSD:
		return fmt.Sprintf("budget_exceeded($%.4f >= $%.2f)", cost, budget.MaxCostUSD)
	case budget.MaxInputTokens > 0 && totals.InputTokens >= budget.MaxInputTokens:
		return fmt.Sprintf("input_tokens_exceeded(%d >= %d)", totals.InputTokens, budget.MaxInputTokens)
	case budget.MaxOutputTokens > 0 && totals.OutputTokens >= budget.MaxOutputTokens:
		return fmt.Sprintf("output_tokens_exceeded(%d >= %d)", totals.OutputTokens, budget.MaxOutputTokens)
	default:
		return ""
	}
}
