package store

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

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
	return AuditWriteError(ctx, rec, a.AppendAudit(ctx, rec))
}

// AuditWriteError returns nil when err is nil, and otherwise logs err and
// returns an error wrapping ErrAuditWrite that names rec's action and outcome.
// RecordAudit, LogAudit and the rows a store writes inside its own
// transactions all go through it, so a failed write there wraps ErrAuditWrite
// (inside a transaction, the store then rolls back). The drift rows of the MCP
// Gate's repository binding call AppendAudit directly and keep a failed row
// owed for the next reload. A failed commit of a transaction that wrote a row
// is a store error, not ErrAuditWrite. Callers of RecordAudit answer an
// ErrAuditWrite with AuditFailureText; LogAudit only logs it. The store error
// goes to the log only, never to callers that may show it to an operator or a
// workload.
func AuditWriteError(ctx context.Context, rec domain.AuditRecord, err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, ErrAuditWrite) {
		return err
	}
	slog.Default().ErrorContext(ctx, "audit write failed", "category", rec.Category, "action", rec.Action, "outcome", rec.Outcome, "task_id", rec.TaskID, "error", err)
	return fmt.Errorf("%w (%s %s)", ErrAuditWrite, rec.Action, rec.Outcome)
}

// LogAudit writes rec and logs a failed write at error level. It is for
// records that cannot change what happens next: refusals that stand either
// way, reads, and background records whose effect is recorded elsewhere.
func LogAudit(ctx context.Context, a AuditAppender, rec domain.AuditRecord) {
	_ = RecordAudit(ctx, a, rec)
}

// ClosingAuditBudget bounds the closing rows of one failure together. Tests
// shorten it.
var ClosingAuditBudget = 5 * time.Second

// LogClosingAudit writes the rows that close earlier rows after the step they
// named failed (not_created, error), in order, and logs each failed write.
// That step may have failed because ctx ended, so the rows are written with
// ctx's values but not its cancellation. All rows of one call share one
// ClosingAuditBudget, so a caller passes every closing row of a failure in one
// call and waits at most one budget. Callers write closing rows after every
// lock they hold is released.
func LogClosingAudit(ctx context.Context, a AuditAppender, recs ...domain.AuditRecord) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), ClosingAuditBudget)
	defer cancel()
	for _, rec := range recs {
		_ = RecordAudit(ctx, a, rec)
	}
}

// AuditFailureText reports whether err is a failed audit write and returns the
// text a caller answers with (HTTP 500, an error result for a tool, an error
// redirect in the web UI). It is the only place that text is written. The
// text is built from the effects recorded on ctx (see TrackEffects): it lists
// every effect the request had when the write failed, in the order they
// happened, or, when it had none, says that the request named by what was
// refused and nothing was changed. A ctx that tracks no effects gets a text
// that says the changes are not known, never that nothing was changed.
func AuditFailureText(ctx context.Context, err error, what string) (string, bool) {
	if !errors.Is(err, ErrAuditWrite) {
		return "", false
	}
	effects := effectsFrom(ctx)
	if effects == nil {
		return what + " failed: an audit record could not be written, and what had already changed was not tracked", true
	}
	if done := effects.List(); len(done) != 0 {
		return strings.Join(done, "; ") + ", but an audit record could not be written", true
	}
	return what + " refused: its audit record could not be written, nothing was changed", true
}
