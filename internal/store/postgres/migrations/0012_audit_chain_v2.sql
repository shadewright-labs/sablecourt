-- Audit chain version 2: every row records its position in the chain (seq) and
-- the hash rule it was written with (hash_version). Rows written before this
-- migration keep their version 1 hash and are numbered in ID order; rows with
-- no hash (written before 0005_audit_chain.sql) stay blank and fail verification.
ALTER TABLE audit_log ADD COLUMN IF NOT EXISTS seq BIGINT NOT NULL DEFAULT 0;
ALTER TABLE audit_log ADD COLUMN IF NOT EXISTS hash_version INTEGER NOT NULL DEFAULT 1;
UPDATE audit_log SET seq = numbered.n
FROM (SELECT id, ROW_NUMBER() OVER (ORDER BY id) AS n FROM audit_log) AS numbered
WHERE audit_log.id = numbered.id;
CREATE UNIQUE INDEX IF NOT EXISTS idx_audit_log_seq ON audit_log(seq);
