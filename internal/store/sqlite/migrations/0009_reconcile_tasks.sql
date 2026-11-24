CREATE INDEX idx_task_events_task_id ON task_events(task_id, id);
ALTER TABLE tasks ADD COLUMN exit_pending BOOLEAN NOT NULL DEFAULT FALSE;

UPDATE tasks SET exit_pending = TRUE
WHERE state IN ('completed', 'failed') AND (
    SELECT reason FROM task_events WHERE task_id = tasks.id ORDER BY id DESC LIMIT 1
) = 'runtime_exit_observed';

CREATE INDEX idx_tasks_reconcile ON tasks(id)
WHERE state IN ('submitted', 'working', 'input-required') OR exit_pending;
CREATE INDEX idx_leases_unrevoked ON leases(task_id) WHERE revoked_at IS NULL;
