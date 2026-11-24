CREATE TABLE councils (
    id TEXT PRIMARY KEY NOT NULL REFERENCES conclaves(id),
    policy TEXT NOT NULL CHECK (json_valid(policy) AND json_type(policy) = 'object')
);

CREATE TABLE council_attempts (
    council_id TEXT NOT NULL REFERENCES councils(id),
    round INTEGER NOT NULL CHECK (round > 0),
    role TEXT NOT NULL,
    attempt INTEGER NOT NULL CHECK (attempt > 0),
    task_id TEXT NOT NULL UNIQUE REFERENCES tasks(id) DEFERRABLE INITIALLY DEFERRED,
    answer_id INTEGER UNIQUE REFERENCES conclave_messages(id),
    nothing_to_add INTEGER NOT NULL DEFAULT 0 CHECK (nothing_to_add IN (0,1) AND (nothing_to_add = 0 OR answer_id IS NOT NULL)),
    PRIMARY KEY (council_id, round, role, attempt)
);

ALTER TABLE conclave_relics ADD COLUMN council_task_id TEXT REFERENCES council_attempts(task_id);
CREATE INDEX idx_relics_council_task ON conclave_relics(council_task_id) WHERE council_task_id IS NOT NULL;

CREATE TRIGGER council_relic_author BEFORE INSERT ON conclave_relics
WHEN EXISTS (SELECT 1 FROM councils WHERE id = NEW.conclave_id)
AND (NEW.council_task_id IS NOT NULL OR NEW.author_role NOT IN ('operator','system'))
AND NOT EXISTS (SELECT 1 FROM council_attempts
    WHERE task_id = NEW.council_task_id AND role = NEW.author_role AND council_id = NEW.conclave_id)
BEGIN
    SELECT RAISE(ABORT, 'council relic requires its author attempt');
END;

CREATE TRIGGER council_relic_immutable BEFORE UPDATE ON conclave_relics
WHEN OLD.council_task_id IS NOT NULL
BEGIN
    SELECT RAISE(ABORT, 'council attempt relic is immutable');
END;

CREATE TRIGGER council_closed BEFORE UPDATE OF state, ward_reason ON conclaves
WHEN OLD.state IN ('completed','cancelled') AND EXISTS (SELECT 1 FROM councils WHERE id = OLD.id)
AND (OLD.state <> NEW.state OR OLD.ward_reason <> NEW.ward_reason)
BEGIN
    SELECT RAISE(ABORT, 'council is closed');
END;

CREATE TRIGGER council_attempt_terminal BEFORE UPDATE OF state ON tasks
WHEN OLD.state IN ('completed','failed','cancelled') AND NEW.state <> OLD.state
AND EXISTS (SELECT 1 FROM council_attempts WHERE task_id = OLD.id)
BEGIN
    SELECT RAISE(ABORT, 'terminal council attempt requires a new reservation');
END;

CREATE TRIGGER council_attempt_revoke AFTER UPDATE OF state ON tasks
WHEN NEW.state IN ('completed','failed','cancelled')
AND EXISTS (SELECT 1 FROM council_attempts WHERE task_id = NEW.id)
BEGIN
    UPDATE leases SET revoked_at = NEW.updated_at WHERE task_id = NEW.id AND revoked_at IS NULL;
END;

CREATE TRIGGER council_attempt_lease BEFORE INSERT ON leases
WHEN EXISTS (SELECT 1 FROM council_attempts a JOIN tasks t ON t.id = a.task_id
    WHERE a.task_id = NEW.task_id AND t.state IN ('completed','failed','cancelled'))
BEGIN
    SELECT RAISE(ABORT, 'terminal council attempt cannot receive a lease');
END;
