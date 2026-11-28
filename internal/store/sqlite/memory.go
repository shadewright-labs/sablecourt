package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	"github.com/shadewright-labs/sablecourt/internal/domain"
	"github.com/shadewright-labs/sablecourt/internal/store"
)

func (s *SQLiteStore) PutMemory(ctx context.Context, entry domain.MemoryEntry) (err error) {
	defer func() { store.NoteEffectIf(ctx, err, "memory entry %s/%s was stored", entry.Namespace, entry.Key) }()
	if entry.Namespace == "" {
		entry.Namespace = "default"
	}
	if entry.Author == "" {
		entry.Author = "operator"
	}
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO court_memory (namespace, key, value, author, updated_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(namespace, key) DO UPDATE SET
			value = excluded.value,
			author = excluded.author,
			updated_at = excluded.updated_at
	`, entry.Namespace, entry.Key, entry.Value, entry.Author, formatTime(entry.UpdatedAt))
	return err
}

func (s *SQLiteStore) GetMemory(ctx context.Context, namespace, key string) (domain.MemoryEntry, error) {
	if namespace == "" {
		namespace = "default"
	}
	var e domain.MemoryEntry
	var upd string
	err := s.db.QueryRowContext(ctx, `
		SELECT namespace, key, value, author, updated_at
		FROM court_memory WHERE namespace = ? AND key = ?
	`, namespace, key).Scan(&e.Namespace, &e.Key, &e.Value, &e.Author, &upd)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.MemoryEntry{}, store.ErrNotFound
	}
	if err != nil {
		return domain.MemoryEntry{}, err
	}
	e.UpdatedAt, _ = time.Parse(time.RFC3339Nano, upd)
	return e, nil
}

func (s *SQLiteStore) SearchMemory(ctx context.Context, namespace, query string, limit int) ([]domain.MemoryEntry, error) {
	if limit <= 0 {
		limit = 50
	}
	q := "%" + strings.TrimSpace(query) + "%"
	var rows *sql.Rows
	var err error
	if strings.TrimSpace(namespace) != "" {
		rows, err = s.db.QueryContext(ctx, `
			SELECT namespace, key, value, author, updated_at
			FROM court_memory
			WHERE namespace = ? AND (key LIKE ? OR value LIKE ?)
			ORDER BY updated_at DESC LIMIT ?
		`, namespace, q, q, limit)
	} else {
		rows, err = s.db.QueryContext(ctx, `
			SELECT namespace, key, value, author, updated_at
			FROM court_memory
			WHERE key LIKE ? OR value LIKE ?
			ORDER BY updated_at DESC LIMIT ?
		`, q, q, limit)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.MemoryEntry
	for rows.Next() {
		var e domain.MemoryEntry
		var upd string
		if err := rows.Scan(&e.Namespace, &e.Key, &e.Value, &e.Author, &upd); err != nil {
			return nil, err
		}
		e.UpdatedAt, _ = time.Parse(time.RFC3339Nano, upd)
		out = append(out, e)
	}
	return out, rows.Err()
}

func (s *SQLiteStore) DeleteMemory(ctx context.Context, namespace, key string) (err error) {
	defer func() { store.NoteEffectIf(ctx, err, "memory entry %s/%s was deleted", namespace, key) }()
	if namespace == "" {
		namespace = "default"
	}
	res, err := s.db.ExecContext(ctx, `DELETE FROM court_memory WHERE namespace = ? AND key = ?`, namespace, key)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return store.ErrNotFound
	}
	return nil
}
