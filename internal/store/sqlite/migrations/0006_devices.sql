CREATE TABLE IF NOT EXISTS paired_devices (
    id TEXT PRIMARY KEY,
    name TEXT NOT NULL,
    platform TEXT NOT NULL,
    public_key_pem TEXT NOT NULL,
    created_at TEXT NOT NULL,
    revoked_at TEXT
);
