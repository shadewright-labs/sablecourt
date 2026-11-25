CREATE TABLE learnings (
    id TEXT PRIMARY KEY NOT NULL CHECK (length(trim(id)) > 0),
    version_digest TEXT NOT NULL CHECK (length(version_digest) = 64),
    statement TEXT NOT NULL CHECK (length(trim(statement)) > 0),
    scope_kind TEXT NOT NULL CHECK (scope_kind IN ('global', 'repo', 'path', 'incident', 'compound', 'legacy')),
    scope_key TEXT NOT NULL CHECK (length(trim(scope_key)) > 0),
    scope_specificity INTEGER NOT NULL DEFAULT 0 CHECK (scope_specificity >= 0),
    scope_global INTEGER NOT NULL DEFAULT 0 CHECK (scope_global IN (0, 1)),
    repo_remote_url TEXT NOT NULL DEFAULT '',
    path_globs_json TEXT NOT NULL DEFAULT '[]' CHECK (json_valid(path_globs_json) AND json_type(path_globs_json) = 'array'),
    incident_cluster TEXT NOT NULL DEFAULT '',
    incident_namespace TEXT NOT NULL DEFAULT '',
    incident_workload_kind TEXT NOT NULL DEFAULT '',
    incident_symptom TEXT NOT NULL DEFAULT '',
    needs_scope_mapping INTEGER NOT NULL DEFAULT 0 CHECK (needs_scope_mapping IN (0, 1)),
    scope_json TEXT NOT NULL CHECK (json_valid(scope_json) AND json_type(scope_json) = 'object'),
    source_kind TEXT NOT NULL CHECK (source_kind IN ('operator', 'agent', 'verifier', 'shadewright', 'task', 'council', 'incident', 'legacy')),
    source_id TEXT NOT NULL DEFAULT '',
    evidence_ref TEXT NOT NULL DEFAULT '',
    provenance_verified INTEGER NOT NULL DEFAULT 0 CHECK (provenance_verified IN (0, 1)),
    author TEXT NOT NULL CHECK (length(trim(author)) > 0),
    predecessor_id TEXT REFERENCES learnings(id) CHECK (predecessor_id IS NULL OR predecessor_id <> id),
    status TEXT NOT NULL CHECK (status IN ('proposed', 'accepted', 'retired')),
    accepted_by TEXT NOT NULL DEFAULT '',
    review_reason TEXT NOT NULL DEFAULT '',
    legacy_namespace TEXT NOT NULL DEFAULT '',
    legacy_key TEXT NOT NULL DEFAULT '',
    legacy_author TEXT NOT NULL DEFAULT '',
    legacy_updated_at TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL,
    last_confirmed_at TEXT,
    expires_at TEXT,
    updated_at TEXT NOT NULL,
    injection_count INTEGER NOT NULL DEFAULT 0 CHECK (injection_count >= 0),
    CHECK (status <> 'accepted' OR (needs_scope_mapping = 0 AND length(trim(accepted_by)) > 0 AND last_confirmed_at IS NOT NULL AND expires_at IS NOT NULL))
);

CREATE INDEX idx_learnings_scope_status
    ON learnings(scope_key, status, scope_specificity DESC, created_at DESC, id ASC);

CREATE INDEX idx_learnings_repo_status
    ON learnings(repo_remote_url, status, scope_specificity DESC, created_at DESC, id ASC);

CREATE INDEX idx_learnings_incident_status
    ON learnings(incident_cluster, incident_namespace, incident_workload_kind, incident_symptom, status, scope_specificity DESC, created_at DESC, id ASC);

CREATE INDEX idx_learnings_status_scope
    ON learnings(status, scope_global, needs_scope_mapping, scope_specificity DESC, created_at DESC, id ASC);

CREATE INDEX idx_learnings_expires
    ON learnings(status, expires_at) WHERE status = 'accepted';

CREATE INDEX idx_learnings_predecessor
    ON learnings(predecessor_id) WHERE predecessor_id IS NOT NULL;

-- Original namespace and key bytes determine the primary key. Redacted labels can coincide.
CREATE INDEX idx_learnings_legacy_origin
    ON learnings(legacy_namespace, legacy_key) WHERE legacy_namespace <> '' AND legacy_key <> '';

CREATE TABLE learning_history (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    learning_id TEXT NOT NULL REFERENCES learnings(id),
    from_status TEXT NOT NULL DEFAULT '',
    to_status TEXT NOT NULL CHECK (to_status IN ('proposed', 'accepted', 'retired')),
    event_kind TEXT NOT NULL CHECK (event_kind IN ('proposed', 'migrated', 'accepted', 'confirmed', 'contradicted', 'expired', 'retired')),
    actor TEXT NOT NULL CHECK (length(trim(actor)) > 0),
    reason TEXT NOT NULL DEFAULT '',
    evidence_ref TEXT NOT NULL DEFAULT '',
    predecessor_id TEXT NOT NULL DEFAULT '',
    superseded_by TEXT NOT NULL DEFAULT '',
    expires_at TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL
);

CREATE INDEX idx_learning_history_learning_id
    ON learning_history(learning_id, id);

CREATE TABLE learning_outcomes (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    learning_id TEXT NOT NULL REFERENCES learnings(id),
    kind TEXT NOT NULL CHECK (kind IN ('supporting', 'contradiction', 'inconclusive')),
    source_kind TEXT NOT NULL CHECK (source_kind IN ('operator', 'agent', 'verifier', 'shadewright', 'task', 'council', 'incident')),
    source_id TEXT NOT NULL DEFAULT '',
    evidence_ref TEXT NOT NULL DEFAULT '',
    summary TEXT NOT NULL DEFAULT '',
    actor TEXT NOT NULL CHECK (length(trim(actor)) > 0),
    confirmed_by TEXT NOT NULL DEFAULT '',
    confirmed_at TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL
);

CREATE INDEX idx_learning_outcomes_learning_id
    ON learning_outcomes(learning_id, id);

CREATE INDEX idx_learning_outcomes_contradiction
    ON learning_outcomes(learning_id, kind, created_at) WHERE kind = 'contradiction';

CREATE TABLE learning_injections (
    learning_id TEXT NOT NULL REFERENCES learnings(id),
    packet_id TEXT NOT NULL CHECK (length(trim(packet_id)) > 0),
    task_id TEXT NOT NULL DEFAULT '',
    injected_at TEXT NOT NULL,
    PRIMARY KEY (learning_id, packet_id)
);

CREATE INDEX idx_learning_injections_packet
    ON learning_injections(packet_id);

CREATE TRIGGER learnings_immutable BEFORE UPDATE ON learnings
WHEN OLD.id <> NEW.id
  OR OLD.version_digest <> NEW.version_digest
  OR OLD.statement <> NEW.statement
  OR OLD.scope_kind <> NEW.scope_kind
  OR OLD.scope_key <> NEW.scope_key
  OR OLD.scope_specificity <> NEW.scope_specificity
  OR OLD.scope_global <> NEW.scope_global
  OR OLD.repo_remote_url <> NEW.repo_remote_url
  OR OLD.path_globs_json <> NEW.path_globs_json
  OR OLD.incident_cluster <> NEW.incident_cluster
  OR OLD.incident_namespace <> NEW.incident_namespace
  OR OLD.incident_workload_kind <> NEW.incident_workload_kind
  OR OLD.incident_symptom <> NEW.incident_symptom
  OR OLD.needs_scope_mapping <> NEW.needs_scope_mapping
  OR OLD.scope_json <> NEW.scope_json
  OR OLD.source_kind <> NEW.source_kind
  OR OLD.source_id <> NEW.source_id
  OR OLD.evidence_ref <> NEW.evidence_ref
  OR OLD.provenance_verified <> NEW.provenance_verified
  OR OLD.author <> NEW.author
  OR COALESCE(OLD.predecessor_id, '') <> COALESCE(NEW.predecessor_id, '')
  OR OLD.legacy_namespace <> NEW.legacy_namespace
  OR OLD.legacy_key <> NEW.legacy_key
  OR OLD.legacy_author <> NEW.legacy_author
  OR OLD.legacy_updated_at <> NEW.legacy_updated_at
  OR OLD.created_at <> NEW.created_at
BEGIN
    SELECT RAISE(ABORT, 'learning statement, scope, and provenance are immutable');
END;

CREATE TRIGGER learnings_no_delete BEFORE DELETE ON learnings
BEGIN
    SELECT RAISE(ABORT, 'learnings cannot be deleted');
END;

CREATE TRIGGER learning_history_immutable BEFORE UPDATE ON learning_history
BEGIN
    SELECT RAISE(ABORT, 'learning history is immutable');
END;

CREATE TRIGGER learning_history_no_delete BEFORE DELETE ON learning_history
BEGIN
    SELECT RAISE(ABORT, 'learning history cannot be deleted');
END;

CREATE TRIGGER learning_outcomes_immutable BEFORE UPDATE ON learning_outcomes
WHEN OLD.id <> NEW.id
  OR OLD.learning_id <> NEW.learning_id
  OR OLD.kind <> NEW.kind
  OR OLD.source_kind <> NEW.source_kind
  OR OLD.source_id <> NEW.source_id
  OR OLD.evidence_ref <> NEW.evidence_ref
  OR OLD.summary <> NEW.summary
  OR OLD.actor <> NEW.actor
  OR OLD.created_at <> NEW.created_at
  OR OLD.confirmed_by <> ''
BEGIN
    SELECT RAISE(ABORT, 'learning outcomes are immutable');
END;

CREATE TRIGGER learning_outcomes_no_delete BEFORE DELETE ON learning_outcomes
BEGIN
    SELECT RAISE(ABORT, 'learning outcomes cannot be deleted');
END;

CREATE TRIGGER learning_injections_immutable BEFORE UPDATE ON learning_injections
BEGIN
    SELECT RAISE(ABORT, 'learning injections are immutable');
END;

CREATE TRIGGER learning_injections_no_delete BEFORE DELETE ON learning_injections
BEGIN
    SELECT RAISE(ABORT, 'learning injections cannot be deleted');
END;
