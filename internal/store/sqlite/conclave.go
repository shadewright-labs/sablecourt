package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/shadewright-labs/sablecourt/internal/domain"
	"github.com/shadewright-labs/sablecourt/internal/store"
)

func (s *SQLiteStore) CreateConclave(ctx context.Context, c domain.ConclaveSession) error {
	now := formatTime(c.CreatedAt)
	upd := formatTime(c.UpdatedAt)
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO conclaves (id, template_name, title, state, ward_reason, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
	`, c.ID, c.TemplateName, c.Title, string(c.State), c.WardReason, now, upd)
	return err
}

func (s *SQLiteStore) GetConclave(ctx context.Context, id string) (domain.ConclaveSession, error) {
	var c domain.ConclaveSession
	var state, createdAt, updatedAt string
	err := s.db.QueryRowContext(ctx, `
		SELECT id, template_name, title, state, ward_reason, created_at, updated_at
		FROM conclaves WHERE id = ?
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

func (s *SQLiteStore) ListConclaves(ctx context.Context) ([]domain.ConclaveSession, error) {
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

func (s *SQLiteStore) UpdateConclaveState(ctx context.Context, id string, state domain.ConclaveState, wardReason string) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE conclaves SET state = ?, ward_reason = ?, updated_at = ? WHERE id = ?
	`, string(state), wardReason, nowRFC3339(), id)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return store.ErrNotFound
	}
	return nil
}

func (s *SQLiteStore) AppendConclaveMessage(ctx context.Context, msg domain.ConclaveMessage) (int64, error) {
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO conclave_messages (conclave_id, from_role, to_role, kind, body, task_id, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
	`, msg.ConclaveID, msg.FromRole, msg.ToRole, msg.Kind, msg.Body, msg.TaskID, formatTime(msg.CreatedAt))
	if err != nil {
		return 0, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, err
	}
	_, _ = s.db.ExecContext(ctx, `UPDATE conclaves SET updated_at = ? WHERE id = ?`, nowRFC3339(), msg.ConclaveID)
	return id, nil
}

func (s *SQLiteStore) ListConclaveMessages(ctx context.Context, conclaveID string, sinceID int64) ([]domain.ConclaveMessage, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, conclave_id, from_role, to_role, kind, body, task_id, created_at
		FROM conclave_messages
		WHERE conclave_id = ? AND id > ?
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

func (s *SQLiteStore) PutConclaveRelic(ctx context.Context, relic domain.ConclaveRelic) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO conclave_relics (conclave_id, name, author_role, content_type, body, sha256, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(conclave_id, name) DO UPDATE SET
			author_role = excluded.author_role,
			content_type = excluded.content_type,
			body = excluded.body,
			sha256 = excluded.sha256,
			created_at = excluded.created_at
	`, relic.ConclaveID, relic.Name, relic.AuthorRole, relic.ContentType, relic.Body, relic.SHA256, formatTime(relic.CreatedAt))
	return err
}

func (s *SQLiteStore) GetConclaveRelic(ctx context.Context, conclaveID, name string) (domain.ConclaveRelic, error) {
	var r domain.ConclaveRelic
	var createdAt string
	err := s.db.QueryRowContext(ctx, `
		SELECT conclave_id, name, author_role, content_type, body, sha256, created_at
		FROM conclave_relics WHERE conclave_id = ? AND name = ?
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

func (s *SQLiteStore) ListConclaveRelics(ctx context.Context, conclaveID string) ([]domain.ConclaveRelic, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT conclave_id, name, author_role, content_type, body, sha256, created_at
		FROM conclave_relics WHERE conclave_id = ? ORDER BY name ASC
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
