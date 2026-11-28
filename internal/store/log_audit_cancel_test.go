package store

import (
	"context"
	"testing"
	"time"

	"github.com/shadewright-labs/sablecourt/internal/domain"
)

// ctxAppender refuses a write whose context has ended, as a database driver
// does, and remembers the deadline and value it was given.
type ctxAppender struct {
	rows     []domain.AuditRecord
	deadline time.Time
	value    any
}

type logAuditKey struct{}

func (a *ctxAppender) AppendAudit(ctx context.Context, rec domain.AuditRecord) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	a.deadline, _ = ctx.Deadline()
	a.value = ctx.Value(logAuditKey{})
	a.rows = append(a.rows, rec)
	return nil
}

// A LogAudit row often closes a row written before a step that failed
// because the request ended. Every caller gets that row written: LogAudit
// does not share the ended context, keeps its values, and bounds the write
// (issue 179).
func TestLogAuditWritesAfterContextEnds(t *testing.T) {
	cancelled, cancel := context.WithCancel(context.WithValue(context.Background(), logAuditKey{}, "v"))
	cancel()
	expired, cancelExpired := context.WithDeadline(context.WithValue(context.Background(), logAuditKey{}, "v"), time.Now().Add(-time.Second))
	defer cancelExpired()
	for name, ctx := range map[string]context.Context{"cancelled": cancelled, "past deadline": expired} {
		t.Run(name, func(t *testing.T) {
			a := &ctxAppender{}
			before := time.Now()
			LogAudit(ctx, a, domain.AuditRecord{Action: "task.submit", Outcome: "not_created"})
			if len(a.rows) != 1 {
				t.Fatalf("rows = %d, want the closing row written", len(a.rows))
			}
			if a.deadline.Before(before) || a.deadline.After(before.Add(logAuditTimeout+time.Second)) {
				t.Fatalf("write deadline = %v, want a bound of about %v", a.deadline, logAuditTimeout)
			}
			if a.value != "v" {
				t.Fatalf("context value = %v, want it kept", a.value)
			}
		})
	}
}
