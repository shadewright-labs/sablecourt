CREATE TABLE IF NOT EXISTS workloads (
    name TEXT PRIMARY KEY,
    zone TEXT NOT NULL,
    class TEXT NOT NULL,
    manifest_digest TEXT NOT NULL,
    agent_card_json TEXT NOT NULL,
    updated_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS tasks (
    id TEXT PRIMARY KEY,
    parent_task_id TEXT NOT NULL DEFAULT '',
    idempotency_key TEXT NOT NULL UNIQUE,
    workload_name TEXT NOT NULL REFERENCES workloads(name),
    state TEXT NOT NULL,
    input_json TEXT NOT NULL DEFAULT '{}',
    artifacts_json TEXT NOT NULL DEFAULT '[]',
    budget_ceiling_json TEXT NOT NULL DEFAULT '{}',
    last_heartbeat_at TEXT,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS task_events (
    id BIGSERIAL PRIMARY KEY,
    task_id TEXT NOT NULL REFERENCES tasks(id),
    from_state TEXT NOT NULL,
    to_state TEXT NOT NULL,
    reason TEXT NOT NULL DEFAULT '',
    fact_json TEXT NOT NULL DEFAULT '{}',
    created_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS task_messages (
    id BIGSERIAL PRIMARY KEY,
    task_id TEXT NOT NULL REFERENCES tasks(id),
    role TEXT NOT NULL,
    content TEXT NOT NULL,
    created_at TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_task_messages_task ON task_messages(task_id, id);

CREATE TABLE IF NOT EXISTS leases (
    token_sha256 TEXT PRIMARY KEY,
    task_id TEXT NOT NULL REFERENCES tasks(id),
    workload_name TEXT NOT NULL,
    expires_at TEXT NOT NULL,
    revoked_at TEXT,
    created_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS budget_ledger (
    id BIGSERIAL PRIMARY KEY,
    task_id TEXT NOT NULL REFERENCES tasks(id),
    workload_name TEXT NOT NULL,
    provider TEXT NOT NULL,
    model TEXT NOT NULL,
    input_tokens BIGINT NOT NULL,
    output_tokens BIGINT NOT NULL,
    cost_microusd BIGINT NOT NULL,
    created_at TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_budget_ledger_task ON budget_ledger(task_id);
CREATE INDEX IF NOT EXISTS idx_budget_ledger_workload_day ON budget_ledger(workload_name, created_at);

CREATE TABLE IF NOT EXISTS approvals (
    id TEXT PRIMARY KEY,
    task_id TEXT NOT NULL REFERENCES tasks(id),
    workload_name TEXT NOT NULL,
    capability TEXT NOT NULL,
    payload_canonical_json TEXT NOT NULL,
    payload_sha256 TEXT NOT NULL,
    status TEXT NOT NULL,
    decided_by TEXT NOT NULL DEFAULT '',
    proof_json TEXT NOT NULL DEFAULT '{}',
    expires_at TEXT NOT NULL,
    created_at TEXT NOT NULL,
    decided_at TEXT
);

CREATE TABLE IF NOT EXISTS checkpoints (
    id BIGSERIAL PRIMARY KEY,
    task_id TEXT NOT NULL REFERENCES tasks(id),
    workload_name TEXT NOT NULL,
    ref_json TEXT NOT NULL,
    created_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS audit_log (
    id BIGSERIAL PRIMARY KEY,
    category TEXT NOT NULL,
    task_id TEXT NOT NULL DEFAULT '',
    workload_name TEXT NOT NULL DEFAULT '',
    action TEXT NOT NULL,
    outcome TEXT NOT NULL,
    detail_json TEXT NOT NULL DEFAULT '{}',
    created_at TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_audit_log_created ON audit_log(created_at);

CREATE TABLE IF NOT EXISTS provider_prefs (
    provider_id TEXT PRIMARY KEY,
    default_model TEXT NOT NULL DEFAULT '',
    enabled_models_json TEXT NOT NULL DEFAULT '[]',
    updated_at TEXT NOT NULL
);
