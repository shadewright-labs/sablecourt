CREATE TABLE IF NOT EXISTS budget_reservations (
    id TEXT PRIMARY KEY,
    task_id TEXT NOT NULL,
    workload_name TEXT NOT NULL,
    input_tokens INTEGER NOT NULL,
    output_tokens INTEGER NOT NULL,
    cost_microusd INTEGER NOT NULL,
    expires_at TEXT NOT NULL,
    created_at TEXT NOT NULL,
    FOREIGN KEY (task_id) REFERENCES tasks(id)
);

CREATE INDEX IF NOT EXISTS idx_budget_reservations_task ON budget_reservations(task_id);
CREATE INDEX IF NOT EXISTS idx_budget_reservations_workload_day ON budget_reservations(workload_name, created_at);
