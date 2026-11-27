package store

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/shadewright-labs/sablecourt/internal/domain"
)

// ErrAuditWrite marks an audit record that could not be written. Callers
// that refuse or report a failure because of it match it with errors.Is.
var ErrAuditWrite = errors.New("audit record could not be written")

// AuditAppender is the part of Store that writes audit records.
type AuditAppender interface {
	AppendAudit(ctx context.Context, rec domain.AuditRecord) error
}

// RecordAudit writes rec and returns an error wrapping ErrAuditWrite when the
// write fails. A mutating path calls it before its effect and refuses on an
// error, so the effect never runs unrecorded. A path whose effect already ran
// calls it afterwards and reports the error to its caller.
func RecordAudit(ctx context.Context, a AuditAppender, rec domain.AuditRecord) error {
	if a == nil {
		return fmt.Errorf("%w: no audit store", ErrAuditWrite)
	}
	if err := a.AppendAudit(ctx, rec); err != nil {
		slog.Default().ErrorContext(ctx, "audit write failed", "category", rec.Category, "action", rec.Action, "outcome", rec.Outcome, "task_id", rec.TaskID, "error", err)
		// The store error goes to the log only, not to callers that may
		// show it to a workload.
		return fmt.Errorf("%w (%s %s)", ErrAuditWrite, rec.Action, rec.Outcome)
	}
	return nil
}

// LogAudit writes rec and logs a failed write at error level. It is for
// records that cannot change what happens next: refusals that stand either
// way, reads, and background records whose effect is recorded elsewhere.
func LogAudit(ctx context.Context, a AuditAppender, rec domain.AuditRecord) {
	_ = RecordAudit(ctx, a, rec)
}
