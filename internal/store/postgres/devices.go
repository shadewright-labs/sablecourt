package postgres

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/shadewright-labs/sablecourt/internal/domain"
	"github.com/shadewright-labs/sablecourt/internal/store"
)

func (s *PostgresStore) CreateDevice(ctx context.Context, d domain.PairedDevice) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO paired_devices (id, name, platform, public_key_pem, created_at)
		VALUES ($1, $2, $3, $4, $5)
	`, d.ID, d.Name, d.Platform, d.PublicKeyPEM, formatTime(d.CreatedAt))
	return err
}

func (s *PostgresStore) GetDevice(ctx context.Context, id string) (domain.PairedDevice, error) {
	var d domain.PairedDevice
	var created string
	var revoked sql.NullString
	err := s.db.QueryRowContext(ctx, `
		SELECT id, name, platform, public_key_pem, created_at, revoked_at
		FROM paired_devices WHERE id = $1
	`, id).Scan(&d.ID, &d.Name, &d.Platform, &d.PublicKeyPEM, &created, &revoked)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.PairedDevice{}, store.ErrNotFound
	}
	if err != nil {
		return domain.PairedDevice{}, err
	}
	d.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
	if revoked.Valid && revoked.String != "" {
		t, _ := time.Parse(time.RFC3339Nano, revoked.String)
		d.RevokedAt = &t
	}
	return d, nil
}

func (s *PostgresStore) ListDevices(ctx context.Context) ([]domain.PairedDevice, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, name, platform, public_key_pem, created_at, revoked_at
		FROM paired_devices ORDER BY created_at DESC
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.PairedDevice
	for rows.Next() {
		var d domain.PairedDevice
		var created string
		var revoked sql.NullString
		if err := rows.Scan(&d.ID, &d.Name, &d.Platform, &d.PublicKeyPEM, &created, &revoked); err != nil {
			return nil, err
		}
		d.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
		if revoked.Valid && revoked.String != "" {
			t, _ := time.Parse(time.RFC3339Nano, revoked.String)
			d.RevokedAt = &t
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

func (s *PostgresStore) RevokeDevice(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `UPDATE paired_devices SET revoked_at = $1 WHERE id = $2`, nowRFC3339(), id)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return store.ErrNotFound
	}
	return nil
}
