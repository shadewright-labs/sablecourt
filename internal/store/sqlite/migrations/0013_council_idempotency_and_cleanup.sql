ALTER TABLE councils ADD COLUMN idempotency_key TEXT;
ALTER TABLE councils ADD COLUMN request_sha256 TEXT NOT NULL DEFAULT '';
ALTER TABLE councils ADD COLUMN cleanup_pending INTEGER NOT NULL DEFAULT 0 CHECK (cleanup_pending IN (0, 1));
CREATE UNIQUE INDEX idx_councils_idempotency_key ON councils(idempotency_key) WHERE idempotency_key IS NOT NULL;
