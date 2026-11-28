package mcpgate

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/shadewright-labs/sablecourt/internal/domain"
	"github.com/shadewright-labs/sablecourt/internal/policy"
	"github.com/shadewright-labs/sablecourt/internal/store"
)

// cancelOnConsumedAudit cancels the request right after the approval's use
// is recorded, the last step before the gate dispatches the action.
type cancelOnConsumedAudit struct {
	store.Store
	cancel *atomic.Pointer[context.CancelFunc]
}

func (s cancelOnConsumedAudit) AppendAudit(ctx context.Context, rec domain.AuditRecord) error {
	err := s.Store.AppendAudit(ctx, rec)
	if rec.Outcome == "consumed" {
		if c := s.cancel.Load(); c != nil {
			(*c)()
		}
	}
	return err
}

// A request cancelled between the audit row of the approval's use and the
// dispatch does not run the action, and the caller is told the approval is
// spent. The promotion executor ignores cancellation, so only the gate's
// check stops it.
func TestCancelledRequestIsNotDispatched(t *testing.T) {
	var cancel atomic.Pointer[context.CancelFunc]
	tg := setupTypedGate(t, typedGateOptions{effect: domain.EffectApprove, wrapStore: func(in store.Store) store.Store {
		return cancelOnConsumedAudit{Store: in, cancel: &cancel}
	}})
	for _, tc := range []struct{ action, payload string }{
		{"sandbox.propose_promotion", `{"workload":"exp-sandbox","branch":"sablecourt/promote-exp","reason":"tests passed"}`},
		{"git.open_pr", validOpenPR},
	} {
		id := requestAndApprove(t, tg, tc.action, tc.payload)
		ctx, c := context.WithCancel(context.Background())
		cancel.Store(&c)
		req := httptest.NewRequest(http.MethodPost, "http://sablecourt.local/"+tc.action, strings.NewReader(`{"approvalId":"`+id+`","payload":`+tc.payload+`}`)).WithContext(ctx)
		req.Header.Set("Authorization", "Bearer "+tg.tokens["ops-agent"])
		rec := httptest.NewRecorder()
		tg.gate.ActionHandler().ServeHTTP(rec, req)
		if rec.Code != http.StatusInternalServerError || !strings.Contains(rec.Body.String(), "approval "+id+" is spent") || !strings.Contains(rec.Body.String(), "before the action ran") {
			t.Fatalf("%s: want a 500 saying the approval is spent and the action did not run, got %d: %s", tc.action, rec.Code, rec.Body.String())
		}
		got, err := tg.st.GetApproval(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		if got.Status != domain.ApprovalConsumed {
			t.Fatalf("%s: status %q, want consumed", tc.action, got.Status)
		}
	}
	if n := len(tg.pr.calls()); n != 0 {
		t.Fatalf("cancelled request reached GitHub %d times", n)
	}
}

// failDriftAudit fails the drift audit rows while fail is set.
type failDriftAudit struct {
	store.Store
	fail *atomic.Bool
}

func (s failDriftAudit) AppendAudit(ctx context.Context, rec domain.AuditRecord) error {
	if s.fail.Load() && strings.Contains(rec.DetailJSON, "repo_origin_changed") {
		return errors.New("audit unavailable")
	}
	return s.Store.AppendAudit(ctx, rec)
}

// A drift whose audit row could not be written is not taken as recorded: the
// next reload that sees the same origin writes the row.
func TestDriftIsRecordedOnlyOnceItsAuditRowIsWritten(t *testing.T) {
	var fail atomic.Bool
	tg := setupTypedGate(t, typedGateOptions{effect: domain.EffectAllow, wrapStore: func(in store.Store) store.Store {
		return failDriftAudit{Store: in, fail: &fail}
	}})
	tg.setOrigin(t, "https://github.com/acme/other.git")
	fail.Store(true)
	if err := tg.binding.Recheck(context.Background()); err == nil {
		t.Fatal("Recheck must report the drift")
	}
	if rows := driftRows(t, tg); len(rows) != 0 {
		t.Fatalf("rows written while the audit failed: %v", rows)
	}
	fail.Store(false)
	_ = tg.binding.Recheck(context.Background())
	_ = tg.binding.Recheck(context.Background())
	if rows := driftRows(t, tg); len(rows) != 1 {
		t.Fatalf("want one drift row once the audit works again, got %v", rows)
	}
}

// A drift row whose write failed stays owed even when the origin changes
// before the next reload, and a return to an earlier origin is a new change:
// acme/first (written), acme/second (write fails), acme/first again gives
// three rows, in that order.
func TestDriftRowStaysOwedAcrossAnotherChange(t *testing.T) {
	var fail atomic.Bool
	tg := setupTypedGate(t, typedGateOptions{effect: domain.EffectAllow, wrapStore: func(in store.Store) store.Store {
		return failDriftAudit{Store: in, fail: &fail}
	}})
	tg.setOrigin(t, "https://github.com/acme/first.git")
	_ = tg.binding.Recheck(context.Background())
	tg.setOrigin(t, "https://github.com/acme/second.git")
	fail.Store(true)
	_ = tg.binding.Recheck(context.Background())
	fail.Store(false)
	tg.setOrigin(t, "https://github.com/acme/first.git")
	_ = tg.binding.Recheck(context.Background())
	_ = tg.binding.Recheck(context.Background())
	rows := driftRows(t, tg)
	if len(rows) != 3 {
		t.Fatalf("want three drift rows (first, second, first), got %v", rows)
	}
	for i, want := range []string{"acme/first", "acme/second", "acme/first"} {
		if !strings.Contains(rows[i], `"origin":"`+want+`"`) {
			t.Fatalf("row %d: want origin %s, got %s", i+1, want, rows[i])
		}
	}
}

// The breaker counts what it counted before the typed catalog: on an allowed
// request every failure of the executor, its payload checks and a refused
// target included. A request that only asks for an approval ran no executor
// and is not counted.
func TestBreakerCountsExecutorFailuresAsBefore(t *testing.T) {
	badBase := strings.Replace(validOpenPR, `"base":"main"`, `"base":"-x"`, 1)
	otherRepo := strings.Replace(validOpenPR, `"widgets"`, `"other"`, 1)
	for _, tc := range []struct {
		name    string
		effect  domain.PolicyEffect
		payload string
		counted bool
	}{
		{"allowed, invalid base ref", domain.EffectAllow, badBase, true},
		{"allowed, target refused", domain.EffectAllow, otherRepo, true},
		{"approval required, invalid base ref", domain.EffectApprove, badBase, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			br := policy.NewCircuitBreaker(1, time.Minute)
			tg := setupTypedGate(t, typedGateOptions{effect: tc.effect, breaker: br})
			rec := tg.post("ops-agent", "git.open_pr", `{"payload":`+tc.payload+`}`)
			if rec.Code == http.StatusOK || rec.Code == http.StatusAccepted {
				t.Fatalf("accepted: %d %s", rec.Code, rec.Body.String())
			}
			if br.IsOpen("action:git.open_pr") != tc.counted {
				t.Fatalf("counted %v, want %v (status %d: %s)", !tc.counted, tc.counted, rec.Code, rec.Body.String())
			}
		})
	}
}
