package postgres

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/shadewright-labs/sablecourt/internal/domain"
	"github.com/shadewright-labs/sablecourt/internal/store"
)

func (s *PostgresStore) CreateConclave(ctx context.Context, c domain.ConclaveSession) (err error) {
	defer func() { store.NoteEffectIf(ctx, err, "conclave %s was opened", c.ID) }()
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO conclaves (id, template_name, title, state, ward_reason, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
	`, c.ID, c.TemplateName, c.Title, string(c.State), c.WardReason, formatTime(c.CreatedAt), formatTime(c.UpdatedAt))
	return err
}

func (s *PostgresStore) GetConclave(ctx context.Context, id string) (domain.ConclaveSession, error) {
	var c domain.ConclaveSession
	var state, createdAt, updatedAt string
	err := s.db.QueryRowContext(ctx, `
		SELECT id, template_name, title, state, ward_reason, created_at, updated_at
		FROM conclaves WHERE id = $1
	`, id).Scan(&c.ID, &c.TemplateName, &c.Title, &state, &c.WardReason, &createdAt, &updatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.ConclaveSession{}, store.ErrNotFound
	}
	if err != nil {
		return domain.ConclaveSession{}, err
	}
	c.State = domain.ConclaveState(state)
	c.CreatedAt, _ = time.Parse(time.RFC3339Nano, createdAt)
	c.UpdatedAt, _ = time.Parse(time.RFC3339Nano, updatedAt)
	return c, nil
}

func (s *PostgresStore) ListConclaves(ctx context.Context) ([]domain.ConclaveSession, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, template_name, title, state, ward_reason, created_at, updated_at
		FROM conclaves ORDER BY created_at DESC
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.ConclaveSession
	for rows.Next() {
		var c domain.ConclaveSession
		var state, createdAt, updatedAt string
		if err := rows.Scan(&c.ID, &c.TemplateName, &c.Title, &state, &c.WardReason, &createdAt, &updatedAt); err != nil {
			return nil, err
		}
		c.State = domain.ConclaveState(state)
		c.CreatedAt, _ = time.Parse(time.RFC3339Nano, createdAt)
		c.UpdatedAt, _ = time.Parse(time.RFC3339Nano, updatedAt)
		out = append(out, c)
	}
	return out, rows.Err()
}

// UpdateConclaveState notes its effect inside the transaction that makes it,
// in store.UpdateConclaveState.
func (s *PostgresStore) UpdateConclaveState(ctx context.Context, id string, state domain.ConclaveState, wardReason string) error {
	_, err := store.UpdateConclaveState(ctx, s.db, nil, id, state, wardReason, time.Now().UTC(), s.appendAuditTx)
	return err
}

// AppendConclaveMessage stores a guarded post through
// store.AppendGuardedConclavePost, which notes each change it commits, and a
// bare message (an import) here.
func (s *PostgresStore) AppendConclaveMessage(ctx context.Context, msg domain.ConclaveMessage) (int64, error) {
	if handled, id, err := store.AppendGuardedConclavePost(ctx, s.db, nil, msg, s.appendAuditTx); handled {
		return id, err
	}
	id, err := s.appendConclaveMessage(ctx, msg)
	store.NoteEffectIf(ctx, err, "message %d was stored in conclave %s", id, msg.ConclaveID)
	return id, err
}

func (s *PostgresStore) appendConclaveMessage(ctx context.Context, msg domain.ConclaveMessage) (int64, error) {
	var id int64
	err := s.db.QueryRowContext(ctx, `
		INSERT INTO conclave_messages (conclave_id, from_role, to_role, kind, body, task_id, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		RETURNING id
	`, msg.ConclaveID, msg.FromRole, msg.ToRole, msg.Kind, msg.Body, msg.TaskID, formatTime(msg.CreatedAt)).Scan(&id)
	if err != nil {
		return 0, err
	}
	_, _ = s.db.ExecContext(ctx, `UPDATE conclaves SET updated_at = $1 WHERE id = $2`, nowRFC3339(), msg.ConclaveID)
	return id, nil
}

func (s *PostgresStore) ListConclaveMessages(ctx context.Context, conclaveID string, sinceID int64) ([]domain.ConclaveMessage, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, conclave_id, from_role, to_role, kind, body, task_id, created_at
		FROM conclave_messages
		WHERE conclave_id = $1 AND id > $2
		ORDER BY id ASC
	`, conclaveID, sinceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.ConclaveMessage
	for rows.Next() {
		var m domain.ConclaveMessage
		var createdAt string
		if err := rows.Scan(&m.ID, &m.ConclaveID, &m.FromRole, &m.ToRole, &m.Kind, &m.Body, &m.TaskID, &createdAt); err != nil {
			return nil, err
		}
		m.CreatedAt, _ = time.Parse(time.RFC3339Nano, createdAt)
		out = append(out, m)
	}
	return out, rows.Err()
}

// PutConclaveRelic writes a report relic through
// store.PutGuardedConclaveReportRelic, which notes it only when it inserted
// the relic, and any other relic here.
func (s *PostgresStore) PutConclaveRelic(ctx context.Context, relic domain.ConclaveRelic) error {
	if handled, err := store.PutGuardedConclaveReportRelic(ctx, s.db, relic); handled {
		return err
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO conclave_relics (conclave_id, name, author_role, content_type, body, sha256, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		ON CONFLICT(conclave_id, name) DO UPDATE SET
			author_role = EXCLUDED.author_role,
			content_type = EXCLUDED.content_type,
			body = EXCLUDED.body,
			sha256 = EXCLUDED.sha256,
			created_at = EXCLUDED.created_at
	`, relic.ConclaveID, relic.Name, relic.AuthorRole, relic.ContentType, relic.Body, relic.SHA256, formatTime(relic.CreatedAt))
	store.NoteEffectIf(ctx, err, "relic %s was written to conclave %s", relic.Name, relic.ConclaveID)
	return err
}

func (s *PostgresStore) GetConclaveRelic(ctx context.Context, conclaveID, name string) (domain.ConclaveRelic, error) {
	var r domain.ConclaveRelic
	var createdAt string
	err := s.db.QueryRowContext(ctx, `
		SELECT conclave_id, name, author_role, content_type, body, sha256, created_at
		FROM conclave_relics WHERE conclave_id = $1 AND name = $2
	`, conclaveID, name).Scan(&r.ConclaveID, &r.Name, &r.AuthorRole, &r.ContentType, &r.Body, &r.SHA256, &createdAt)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.ConclaveRelic{}, store.ErrNotFound
	}
	if err != nil {
		return domain.ConclaveRelic{}, err
	}
	r.CreatedAt, _ = time.Parse(time.RFC3339Nano, createdAt)
	return r, nil
}

func (s *PostgresStore) ListConclaveRelics(ctx context.Context, conclaveID string) ([]domain.ConclaveRelic, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT conclave_id, name, author_role, content_type, body, sha256, created_at
		FROM conclave_relics WHERE conclave_id = $1 ORDER BY name ASC
	`, conclaveID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.ConclaveRelic
	for rows.Next() {
		var r domain.ConclaveRelic
		var createdAt string
		if err := rows.Scan(&r.ConclaveID, &r.Name, &r.AuthorRole, &r.ContentType, &r.Body, &r.SHA256, &createdAt); err != nil {
			return nil, err
		}
		r.CreatedAt, _ = time.Parse(time.RFC3339Nano, createdAt)
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *PostgresStore) FinishConclaveWake(ctx context.Context, id int64) error {
	res, err := store.FinishConclaveWake(ctx, s.db, id)
	store.NoteEffectRows(ctx, res, err, "the wake for message %d was finished", id)
	return err
}
