ALTER TABLE budget_ledger ADD COLUMN cache_write_tokens INTEGER NOT NULL DEFAULT 0;
ALTER TABLE budget_ledger ADD COLUMN cache_read_tokens INTEGER NOT NULL DEFAULT 0;
