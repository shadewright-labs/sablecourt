package store

import (
	"context"
	"testing"
	"time"

	"github.com/shadewright-labs/sablecourt/internal/domain"
)

// ctxAppender refuses a write whose context has ended, as a database driver
// does, and remembers the deadlines and value it was given.
type ctxAppender struct {
	rows      []domain.AuditRecord
	deadlines []time.Time
	value     any
}

type logAuditKey struct{}

func (a *ctxAppender) AppendAudit(ctx context.Context, rec domain.AuditRecord) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	deadline, _ := ctx.Deadline()
	a.deadlines = append(a.deadlines, deadline)
	a.value = ctx.Value(logAuditKey{})
	a.rows = append(a.rows, rec)
	return nil
}

// The closing rows of one failure are written after the request ended,
// keep its values, and share one ClosingAuditBudget; LogAudit keeps the
// request's cancellation (issue 179).
func TestLogClosingAuditSharesOneDetachedBudget(t *testing.T) {
	cancelled, cancel := context.WithCancel(context.WithValue(context.Background(), logAuditKey{}, "v"))
	cancel()
	expired, cancelExpired := context.WithDeadline(context.WithValue(context.Background(), logAuditKey{}, "v"), time.Now().Add(-time.Second))
	defer cancelExpired()
	for name, ctx := range map[string]context.Context{"cancelled": cancelled, "past deadline": expired} {
		t.Run(name, func(t *testing.T) {
			a := &ctxAppender{}
			LogAudit(ctx, a, domain.AuditRecord{Action: "conclave.post", Outcome: "denied"})
			if len(a.rows) != 0 {
				t.Fatalf("LogAudit wrote %d rows after its context ended, want it to keep the request's cancellation", len(a.rows))
			}
			before := time.Now()
			LogClosingAudit(ctx, a,
				domain.AuditRecord{Action: "task.retry", Outcome: "not_created"},
				domain.AuditRecord{Action: "task.submit", Outcome: "not_created"})
			if len(a.rows) != 2 {
				t.Fatalf("rows = %d, want both closing rows written", len(a.rows))
			}
			if len(a.deadlines) != 2 || !a.deadlines[0].Equal(a.deadlines[1]) {
				t.Fatalf("write deadlines = %v, want one shared deadline", a.deadlines)
			}
			if d := a.deadlines[0]; d.Before(before) || d.After(before.Add(ClosingAuditBudget+time.Second)) {
				t.Fatalf("write deadline = %v, want a budget of about %v", d, ClosingAuditBudget)
			}
			if a.value != "v" {
				t.Fatalf("context value = %v, want it kept", a.value)
			}
		})
	}
}
