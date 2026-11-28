package store

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"

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

// AfterEffectError is a failed audit write whose effect had already happened
// when the write failed. It matches ErrAuditWrite with errors.Is.
type AfterEffectError struct {
	// Done names the effect that happened, for example "task t1 was created".
	Done string
	err  error
}

func (e *AfterEffectError) Error() string { return e.Done + ", but " + e.err.Error() }

func (e *AfterEffectError) Unwrap() error { return e.err }

// AfterEffect marks err, a failed audit write, as one that came after the
// effect done names, so a caller does not report that nothing changed. It
// returns nil for a nil err.
func AfterEffect(done string, err error) error {
	if err == nil {
		return nil
	}
	return &AfterEffectError{Done: done, err: err}
}

// AuditFailureText reports whether err is a failed audit write and returns the
// text a caller answers with (HTTP 500). When err holds AfterEffectErrors,
// wrapped or joined at any depth, the text names every effect that already
// happened, outermost first; otherwise it says that the request, named by
// what, was refused and nothing was changed.
func AuditFailureText(err error, what string) (string, bool) {
	if !errors.Is(err, ErrAuditWrite) {
		return "", false
	}
	if done := afterEffects(err, nil); len(done) != 0 {
		return strings.Join(done, "; ") + ", but an audit record could not be written", true
	}
	return what + " refused: its audit record could not be written, nothing was changed", true
}

// afterEffects collects the Done text of every AfterEffectError in the tree
// of err, depth first, skipping repeats.
func afterEffects(err error, done []string) []string {
	if err == nil {
		return done
	}
	// The walk looks at each node of the tree itself, so it inspects the
	// node's own type, not what it wraps (errors.As would find only the
	// first match).
	node := any(err)
	if after, ok := node.(*AfterEffectError); ok && !slices.Contains(done, after.Done) {
		done = append(done, after.Done)
	}
	switch u := node.(type) {
	case interface{ Unwrap() error }:
		done = afterEffects(u.Unwrap(), done)
	case interface{ Unwrap() []error }:
		for _, e := range u.Unwrap() {
			done = afterEffects(e, done)
		}
	}
	return done
}
