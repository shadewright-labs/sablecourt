CREATE TABLE IF NOT EXISTS conclaves (
    id TEXT PRIMARY KEY,
    template_name TEXT NOT NULL,
    title TEXT NOT NULL,
    state TEXT NOT NULL,
    ward_reason TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS conclave_messages (
    id BIGSERIAL PRIMARY KEY,
    conclave_id TEXT NOT NULL REFERENCES conclaves(id) ON DELETE CASCADE,
    from_role TEXT NOT NULL,
    to_role TEXT NOT NULL,
    kind TEXT NOT NULL,
    body TEXT NOT NULL,
    task_id TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_conclave_messages_conclave_id ON conclave_messages(conclave_id, id);

CREATE TABLE IF NOT EXISTS conclave_relics (
    conclave_id TEXT NOT NULL REFERENCES conclaves(id) ON DELETE CASCADE,
    name TEXT NOT NULL,
    author_role TEXT NOT NULL,
    content_type TEXT NOT NULL,
    body TEXT NOT NULL,
    sha256 TEXT NOT NULL,
    created_at TEXT NOT NULL,
    PRIMARY KEY (conclave_id, name)
);
