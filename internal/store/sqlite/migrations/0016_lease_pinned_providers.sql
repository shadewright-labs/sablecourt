-- A lease whose packet carried learnings keeps the providers it was admitted
-- under (ADR-0012). NULL means the packet carried none.
ALTER TABLE leases ADD COLUMN pinned_providers_json TEXT;
