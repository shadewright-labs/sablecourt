package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/shadewright-labs/sablecourt/internal/domain"
)

type taskMemoryKey struct{}

// CreateTaskWithMemory publishes a task and its registration in one transaction.
func CreateTaskWithMemory(ctx context.Context, st Store, task domain.Task, entry domain.MemoryEntry) error {
	if entry.Key != task.ID {
		return errors.New("registration does not match task")
	}
	return st.CreateTask(context.WithValue(ctx, taskMemoryKey{}, entry), task)
}

func insertRegisteredTask(ctx context.Context, db *sql.DB, entry domain.MemoryEntry, insert func(TaskInserter) error) error {
	ctx = HoldEffects(ctx)
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := insert(tx); err != nil {
		return err
	}
	if err := putMemoryTx(ctx, tx, entry); err != nil {
		return err
	}
	NoteEffect(ctx, "memory entry %s/%s was stored", entry.Namespace, entry.Key)
	return Commit(ctx, tx)
}

func putMemoryTx(ctx context.Context, tx TaskInserter, entry domain.MemoryEntry) error {
	_, err := tx.ExecContext(ctx, `
		INSERT INTO court_memory (namespace, key, value, author, updated_at)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT(namespace, key) DO UPDATE SET
			value = excluded.value, author = excluded.author, updated_at = excluded.updated_at
	`, entry.Namespace, entry.Key, entry.Value, entry.Author, entry.UpdatedAt.UTC().Format(time.RFC3339Nano))
	return err
}

type transitionEffectsKey struct{}

// ErrAlreadyRecorded means UpdateTaskStateWithEffects found the event it
// would add already on the task and wrote nothing.
var ErrAlreadyRecorded = errors.New("transition already recorded")

// TransitionEffects are the writes UpdateTaskStateWithEffects commits in the
// same transaction as the task event: all of them or none.
type TransitionEffects struct {
	// Recorded reports whether an existing event of the task is the one
	// this transition would add. It runs inside the transaction once the
	// task row is updated, so in PostgreSQL after the row lock is held and
	// in SQLite under the write lock: two concurrent calls cannot both
	// miss each other's event.
	Recorded func(reason, factJSON string) bool
	Audit    *domain.AuditRecord
	Memory   *domain.MemoryEntry

	applied bool
}

// UpdateTaskStateWithEffects is UpdateTaskState with the audit row and the
// memory entry of effects written in its transaction. When effects.Recorded
// matches an event the task already has, it writes nothing and returns
// ErrAlreadyRecorded. A store that does not apply the effects fails the call.
func UpdateTaskStateWithEffects(ctx context.Context, st Store, taskID string, from, to domain.TaskState, reason, factJSON string, effects TransitionEffects) error {
	effects.applied = false
	err := st.UpdateTaskState(context.WithValue(ctx, transitionEffectsKey{}, &effects), taskID, from, to, reason, factJSON)
	if err == nil && !effects.applied {
		return errors.New("store did not apply the transition effects")
	}
	return err
}

// ApplyTransitionEffects writes the effects carried by ctx inside tx. Each
// store's UpdateTaskState calls it after the task row update and before the
// event insert, and rolls back on any error.
func ApplyTransitionEffects(ctx context.Context, tx *sql.Tx, taskID string, audit AuditTxFunc) error {
	effects, ok := ctx.Value(transitionEffectsKey{}).(*TransitionEffects)
	if !ok {
		return nil
	}
	if effects.Recorded != nil {
		rows, err := tx.QueryContext(ctx, "SELECT reason, fact_json FROM task_events WHERE task_id = $1", taskID)
		if err != nil {
			return err
		}
		recorded := false
		for rows.Next() {
			var reason, fact string
			if err := rows.Scan(&reason, &fact); err != nil {
				_ = rows.Close()
				return err
			}
			if effects.Recorded(reason, fact) {
				recorded = true
				break
			}
		}
		if err := errors.Join(rows.Err(), rows.Close()); err != nil {
			return err
		}
		if recorded {
			return ErrAlreadyRecorded
		}
	}
	if effects.Audit != nil {
		if err := audit(ctx, tx, *effects.Audit); err != nil {
			return err
		}
	}
	if effects.Memory != nil {
		if err := putMemoryTx(ctx, tx, *effects.Memory); err != nil {
			return err
		}
		NoteEffect(ctx, "memory entry %s/%s was stored", effects.Memory.Namespace, effects.Memory.Key)
	}
	effects.applied = true
	return nil
}

func ListTasks(ctx context.Context, db *sql.DB) ([]domain.Task, error) {
	return queryTasks(ctx, db, "")
}

const selectTasks = `
	SELECT id, parent_task_id, idempotency_key, workload_name, state,
	       input_json, artifacts_json, budget_ceiling_json, last_heartbeat_at,
	       created_at, updated_at
	FROM tasks `

func GetTask(ctx context.Context, db *sql.DB, id string) (domain.Task, error) {
	return scanTask(db.QueryRowContext(ctx, selectTasks+"WHERE id = $1", id))
}

func GetTaskByIdempotencyKey(ctx context.Context, db *sql.DB, key string) (domain.Task, error) {
	return scanTask(db.QueryRowContext(ctx, selectTasks+"WHERE idempotency_key = $1", key))
}

// ListReconcileTasks includes terminal tasks with a retained handle, an
// unrevoked lease, a runtime exit that still needs processing, or a workflow
// halt that is not recorded yet (PendingHalts).
func ListReconcileTasks(ctx context.Context, db *sql.DB, handleIDs []string, handleQuery string) ([]domain.Task, error) {
	if handleIDs == nil {
		handleIDs = []string{}
	}
	handles, err := json.Marshal(handleIDs)
	if err != nil {
		return nil, err
	}
	const candidates = `WHERE id IN (
		SELECT id FROM tasks WHERE state IN ('submitted', 'working', 'input-required') OR exit_pending
		UNION ALL SELECT task_id FROM leases WHERE revoked_at IS NULL
		UNION ALL SELECT key FROM court_memory WHERE namespace = 'system:runtime-handles'
		UNION ALL SELECT key FROM court_memory WHERE namespace = '` + PendingHalts + `'
		UNION ALL `
	return queryTasks(ctx, db, candidates+handleQuery+")", string(handles))
}

func queryTasks(ctx context.Context, db *sql.DB, filter string, args ...any) ([]domain.Task, error) {
	rows, err := db.QueryContext(ctx, selectTasks+filter+" ORDER BY created_at DESC", args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var tasks []domain.Task
	for rows.Next() {
		task, err := scanTask(rows)
		if err != nil {
			return nil, err
		}
		tasks = append(tasks, task)
	}
	return tasks, rows.Err()
}

func scanTask(row interface{ Scan(...any) error }) (domain.Task, error) {
	var task domain.Task
	var created, updated string
	var heartbeat sql.NullString
	err := row.Scan(&task.ID, &task.ParentTaskID, &task.IdempotencyKey, &task.WorkloadName, &task.State,
		&task.InputJSON, &task.ArtifactsJSON, &task.BudgetCeilingJSON, &heartbeat, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.Task{}, ErrNotFound
	}
	if err != nil {
		return domain.Task{}, err
	}
	task.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
	task.UpdatedAt, _ = time.Parse(time.RFC3339Nano, updated)
	if heartbeat.Valid {
		if at, err := time.Parse(time.RFC3339Nano, heartbeat.String); err == nil {
			task.LastHeartbeatAt = &at
		}
	}
	return task, nil
}

func RenewTaskLeases(ctx context.Context, db *sql.DB, taskID string, now, expiresAt time.Time) (sql.Result, error) {
	// Both stores write UTC RFC3339Nano. Strip Z so a whole second sorts before
	// a fractional second and a shorter fraction sorts before its extensions.
	return db.ExecContext(ctx, `
		UPDATE leases SET expires_at = $1
		WHERE task_id = $2 AND revoked_at IS NULL
		  AND rtrim(expires_at, 'Z') > rtrim($3, 'Z')
		  AND rtrim(expires_at, 'Z') < rtrim($1, 'Z')
		  AND EXISTS (SELECT 1 FROM tasks WHERE id = $2 AND state IN ('working', 'input-required'))
	`, expiresAt.UTC().Format(time.RFC3339Nano), taskID, now.UTC().Format(time.RFC3339Nano))
}

const RecoveryOwners = "system:recovery-owners"

// PendingHalts holds, keyed by task ID, the failed workflow steps whose run
// halt the control plane has not recorded yet. ListReconcileTasks returns
// them until the control plane deletes the entry.
const PendingHalts = "system:workflow-halts-pending"

// ClaimRecovery replaces only the exact owner the caller verified had exited.
func ClaimRecovery(ctx context.Context, db *sql.DB, taskID, previous, owner string) (bool, error) {
	result, err := db.ExecContext(ctx, `
		INSERT INTO court_memory (namespace,key,value,author,updated_at)
		VALUES ($1,$2,$3,'controlplane',$4)
		ON CONFLICT(namespace,key) DO UPDATE SET value=excluded.value,updated_at=excluded.updated_at
		WHERE court_memory.value=$5 AND court_memory.author='controlplane'
	`, RecoveryOwners, taskID, owner, time.Now().UTC().Format(time.RFC3339Nano), previous)
	if err != nil {
		return false, err
	}
	n, err := result.RowsAffected()
	return n == 1, err
}

func CommitTaskTransition(ctx context.Context, tx *sql.Tx, taskID string, to domain.TaskState, now string) error {
	defer tx.Rollback()
	if to == domain.TaskStateFailed {
		if _, err := tx.ExecContext(ctx, "UPDATE leases SET revoked_at=$1 WHERE task_id=$2 AND revoked_at IS NULL", now, taskID); err != nil {
			return err
		}
	}
	return Commit(ctx, tx)
}
