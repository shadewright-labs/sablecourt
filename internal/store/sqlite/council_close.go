package sqlite

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/shadewright-labs/sablecourt/internal/domain"
	"github.com/shadewright-labs/sablecourt/internal/redact"
	"github.com/shadewright-labs/sablecourt/internal/store"
)

// CloseCouncil renders the evidence under the same write lock as answer acceptance.
func (s *SQLiteStore) CloseCouncil(ctx context.Context, id string, terminal domain.ConclaveState, reason string) (closedRelic domain.ConclaveRelic, err error) {
	defer func() { store.NoteEffectIf(ctx, err, "council %s was closed as %s", id, terminal) }()
	return s.closeCouncil(ctx, id, terminal, reason, false)
}

func (s *SQLiteStore) closeCouncil(ctx context.Context, id string, terminal domain.ConclaveState, reason string, requireMatch bool) (domain.ConclaveRelic, error) {
	var zero domain.ConclaveRelic
	if !store.ValidCouncilText(id) || !store.ValidCouncilText(reason) || redact.String(id) != id || (terminal != domain.ConclaveStateCompleted && terminal != domain.ConclaveStateCancelled) {
		return zero, errors.New("invalid council close")
	}
	reason = redact.String(reason)
	if !store.ValidCouncilText(reason) {
		return zero, errors.New("invalid redacted stop reason")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return zero, err
	}
	defer tx.Rollback()
	var state domain.ConclaveState
	var savedReason string
	err = tx.QueryRowContext(ctx, `UPDATE conclaves SET updated_at=updated_at WHERE id=? AND id IN (SELECT id FROM councils)
		RETURNING state,ward_reason`, id).Scan(&state, &savedReason)
	if errors.Is(err, sql.ErrNoRows) {
		return zero, store.ErrNotFound
	}
	if err != nil {
		return zero, err
	}
	switch state {
	case domain.ConclaveStateCompleted, domain.ConclaveStateCancelled:
		if requireMatch && (state != terminal || reason != savedReason) {
			return zero, store.ErrConflict
		}
		r, err := readConclaveRelic(ctx, tx, id, store.CouncilDigestRelicName)
		if err != nil {
			return zero, err
		}
		if r.AuthorRole != "system" || r.ContentType != "text/plain" || r.SHA256 != fmt.Sprintf("%x", sha256.Sum256([]byte(r.Body))) {
			return zero, store.ErrConflict
		}
		return r, nil
	case domain.ConclaveStateActive, domain.ConclaveStateInputRequired:
	default:
		return zero, store.ErrConflict
	}
	snap, err := readCouncilSnapshot(ctx, tx, id)
	if err != nil {
		return zero, err
	}
	r, err := snap.Digest(reason)
	if err != nil {
		return zero, err
	}
	r.CreatedAt = time.Now().UTC()
	_, err = tx.ExecContext(ctx, "UPDATE conclaves SET state=?,ward_reason=?,updated_at=? WHERE id=?", terminal, reason, formatTime(r.CreatedAt), id)
	if err != nil {
		return zero, err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO conclave_relics(conclave_id,name,author_role,content_type,body,sha256,created_at)
		VALUES(?,?,?,?,?,?,?)`, id, r.Name, r.AuthorRole, r.ContentType, r.Body, r.SHA256, formatTime(r.CreatedAt))
	if err != nil {
		return zero, err
	}
	if err := s.appendAuditTx(ctx, tx, domain.AuditRecord{
		Category:   "conclave",
		Action:     "conclave.close",
		Outcome:    string(terminal),
		DetailJSON: fmt.Sprintf(`{"conclave_id":%q}`, id),
		CreatedAt:  r.CreatedAt,
	}); err != nil {
		return zero, err
	}
	if err := tx.Commit(); err != nil {
		return zero, err
	}
	return r, nil
}
