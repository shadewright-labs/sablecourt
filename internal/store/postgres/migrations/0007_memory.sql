CREATE TABLE IF NOT EXISTS court_memory (
    namespace TEXT NOT NULL,
    key TEXT NOT NULL,
    value TEXT NOT NULL,
    author TEXT NOT NULL DEFAULT 'operator',
    updated_at TEXT NOT NULL,
    PRIMARY KEY (namespace, key)
);

CREATE INDEX IF NOT EXISTS idx_court_memory_ns ON court_memory(namespace, updated_at);
