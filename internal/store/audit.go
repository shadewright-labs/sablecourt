package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/shadewright-labs/sablecourt/internal/domain"
)

// AuditChainNext reads the newest audit row inside tx and returns the previous
// hash and the sequence for the next row. It refuses to extend a chain whose
// newest row has no stored hash or no sequence, because the next row could not
// be linked to it; the caller's action then fails instead of starting a second
// chain at genesis. Both stores call it with the audit table locked or written
// in the same transaction.
func AuditChainNext(ctx context.Context, tx *sql.Tx) (string, int64, error) {
	var id, seq int64
	var hash string
	err := tx.QueryRowContext(ctx, `SELECT id, seq, row_hash FROM audit_log ORDER BY id DESC LIMIT 1`).Scan(&id, &seq, &hash)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.AuditGenesis, 1, nil
	}
	if err != nil {
		return "", 0, err
	}
	if hash == "" || seq < 1 {
		return "", 0, fmt.Errorf("%w: newest audit row %d has no stored hash or sequence; refusing to extend the chain", domain.ErrAuditChainBroken, id)
	}
	return hash, seq + 1, nil
}
