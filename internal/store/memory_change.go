package store

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/shadewright-labs/sablecourt/internal/domain"
)

// MemoryKey names one Court Memory entry.
type MemoryKey struct {
	Namespace string
	Key       string
}

// MemoryChange is a change to Court Memory and the audit row that records it.
// Store.ChangeMemory commits all of it or none of it.
type MemoryChange struct {
	// Audit is the row that records the change. It is appended to the audit
	// chain in the same transaction as the memory writes.
	Audit domain.AuditRecord
	// Put lists the entries to store or replace. An empty namespace is
	// "default" and an empty author "operator", as for PutMemory.
	Put []domain.MemoryEntry
	// Delete lists the entries to remove. An entry that does not exist fails
	// the whole change with ErrNotFound.
	Delete []MemoryKey
}

// ErrEmptyMemoryChange is returned for a MemoryChange with nothing to write.
var ErrEmptyMemoryChange = errors.New("memory change has no entries")

// ApplyMemoryChange writes change in one transaction of db: the audit row
// first, through audit, then each put and each delete. Any failure rolls the
// whole transaction back, so a memory write whose audit row cannot be written
// never happens; that failure wraps ErrAuditWrite (see AuditWriteError). The
// effects of the change are noted only once the transaction commits.
func ApplyMemoryChange(ctx context.Context, db *sql.DB, change MemoryChange, audit AuditTxFunc) error {
	if len(change.Put) == 0 && len(change.Delete) == 0 {
		return ErrEmptyMemoryChange
	}
	ctx = HoldEffects(ctx)
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := audit(ctx, tx, change.Audit); err != nil {
		return err
	}
	now := time.Now().UTC()
	for _, entry := range change.Put {
		if entry.Namespace == "" {
			entry.Namespace = "default"
		}
		if entry.Author == "" {
			entry.Author = "operator"
		}
		if entry.UpdatedAt.IsZero() {
			entry.UpdatedAt = now
		}
		if err := putMemoryTx(ctx, tx, entry); err != nil {
			return err
		}
		NoteEffect(ctx, "memory entry %s/%s was stored", entry.Namespace, entry.Key)
	}
	for _, k := range change.Delete {
		ns := k.Namespace
		if ns == "" {
			ns = "default"
		}
		res, err := tx.ExecContext(ctx, `DELETE FROM court_memory WHERE namespace = $1 AND key = $2`, ns, k.Key)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n == 0 {
			return ErrNotFound
		}
		NoteEffect(ctx, "memory entry %s/%s was deleted", ns, k.Key)
	}
	return Commit(ctx, tx)
}
