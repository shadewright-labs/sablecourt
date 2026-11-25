ALTER TABLE council_attempts ADD COLUMN policy_routed INTEGER NOT NULL DEFAULT 0 CHECK (policy_routed IN (0,1));

CREATE TRIGGER council_digest_reserved BEFORE INSERT ON conclave_relics
WHEN NEW.name = 'digest' AND EXISTS (SELECT 1 FROM councils WHERE id = NEW.conclave_id)
AND (NEW.council_task_id IS NOT NULL OR NEW.author_role <> 'system'
    OR NOT EXISTS (SELECT 1 FROM conclaves WHERE id = NEW.conclave_id AND state IN ('completed','cancelled')))
BEGIN
    SELECT RAISE(ABORT, 'digest requires a daemon close');
END;

CREATE TRIGGER council_digest_immutable BEFORE UPDATE ON conclave_relics
WHEN OLD.name = 'digest' AND EXISTS (SELECT 1 FROM councils WHERE id = OLD.conclave_id)
BEGIN
    SELECT RAISE(ABORT, 'closing digest is immutable');
END;
