package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/shadewright-labs/sablecourt/internal/domain"
)

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
// unrevoked lease, or a runtime exit that still needs processing.
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

func RenewTaskLeases(ctx context.Context, db *sql.DB, taskID string, now, expiresAt time.Time) error {
	// Both stores write UTC RFC3339Nano. Strip Z so a whole second sorts before
	// a fractional second and a shorter fraction sorts before its extensions.
	_, err := db.ExecContext(ctx, `
		UPDATE leases SET expires_at = $1
		WHERE task_id = $2 AND revoked_at IS NULL
		  AND rtrim(expires_at, 'Z') > rtrim($3, 'Z')
		  AND rtrim(expires_at, 'Z') < rtrim($1, 'Z')
		  AND EXISTS (SELECT 1 FROM tasks WHERE id = $2 AND state IN ('working', 'input-required'))
	`, expiresAt.UTC().Format(time.RFC3339Nano), taskID, now.UTC().Format(time.RFC3339Nano))
	return err
}
