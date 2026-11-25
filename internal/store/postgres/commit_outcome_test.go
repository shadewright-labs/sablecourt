package postgres

import (
	"errors"
	"io"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/shadewright-labs/sablecourt/internal/store"
)

// Medium 5: a COMMIT error the server sent is a rollback; any other error
// leaves the outcome unknown.
func TestCommitOutcome(t *testing.T) {
	if err := commitOutcome(&pgconn.PgError{Code: "40001"}); errors.Is(err, store.ErrCommitUnknown) {
		t.Fatalf("server-reported failure marked unknown: %v", err)
	}
	if err := commitOutcome(io.ErrUnexpectedEOF); !errors.Is(err, store.ErrCommitUnknown) {
		t.Fatalf("lost connection not marked unknown: %v", err)
	}
}
