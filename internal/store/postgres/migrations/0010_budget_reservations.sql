CREATE TABLE IF NOT EXISTS budget_reservations (
    id TEXT PRIMARY KEY,
    task_id TEXT NOT NULL REFERENCES tasks(id),
    workload_name TEXT NOT NULL,
    input_tokens BIGINT NOT NULL,
    output_tokens BIGINT NOT NULL,
    cost_microusd BIGINT NOT NULL,
    expires_at TEXT NOT NULL,
    created_at TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_budget_reservations_task ON budget_reservations(task_id);
CREATE INDEX IF NOT EXISTS idx_budget_reservations_workload_day ON budget_reservations(workload_name, created_at);
