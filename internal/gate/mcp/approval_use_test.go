package mcpgate

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/shadewright-labs/sablecourt/internal/domain"
	"github.com/shadewright-labs/sablecourt/internal/store"
)

const validOpenPR = `{"owner":"acme","repo":"widgets","head":"sablecourt/fix-1","base":"main","title":"Fix"}`

func requestAndApprove(t *testing.T, tg *typedGate, action, payload string) string {
	t.Helper()
	rec := tg.post("ops-agent", action, `{"payload":`+payload+`}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("want 202, got %d: %s", rec.Code, rec.Body.String())
	}
	var pending struct {
		ApprovalID    string `json:"approval_id"`
		PayloadSHA256 string `json:"payload_sha256"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &pending); err != nil {
		t.Fatal(err)
	}
	if _, err := tg.queue.Approve(context.Background(), pending.ApprovalID, pending.PayloadSHA256, "operator"); err != nil {
		t.Fatalf("Approve: %v", err)
	}
	return pending.ApprovalID
}

// slowLockStore stands for a wait on the approval row's lock: the clock moves
// on while ConsumeApproval waits, before the store checks the expiry.
type slowLockStore struct {
	store.Store
	clock *atomic.Pointer[time.Time]
	wait  time.Duration
}

func (s slowLockStore) ConsumeApproval(ctx context.Context, req store.ConsumeRequest) error {
	later := s.clock.Load().Add(s.wait)
	s.clock.Store(&later)
	return s.Store.ConsumeApproval(ctx, req)
}

// M2: the expiry check uses the time after the row lock, not a time sampled
// before the gate waited for it.
func TestApprovalExpiryUsesTimeAfterTheLock(t *testing.T) {
	var clock atomic.Pointer[time.Time]
	start := time.Now().UTC()
	clock.Store(&start)
	tg := setupTypedGate(t, typedGateOptions{
		effect: domain.EffectApprove,
		clock:  &clock,
		wrapStore: func(inner store.Store) store.Store {
			return slowLockStore{Store: inner, clock: &clock, wait: 20 * time.Minute}
		},
	})
	id := requestAndApprove(t, tg, "git.open_pr", validOpenPR)
	rec := tg.post("ops-agent", "git.open_pr", `{"approvalId":"`+id+`","payload":`+validOpenPR+`}`)
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "approval_expired") {
		t.Fatalf("a wait spanning expiry must refuse: got %d: %s", rec.Code, rec.Body.String())
	}
	if n := len(tg.pr.calls()); n != 0 {
		t.Fatalf("action ran %d times", n)
	}
}

// M3: the consumption and its audit row commit together. When the audit
// insert fails the approval stays approved and the caller is told so.
func TestFailedConsumptionAuditLeavesApprovalUnused(t *testing.T) {
	tg := setupTypedGate(t, typedGateOptions{effect: domain.EffectApprove})
	id := requestAndApprove(t, tg, "git.open_pr", validOpenPR)
	db := tg.rawDB(t)
	if _, err := db.Exec(`CREATE TRIGGER fail_consumed_audit BEFORE INSERT ON audit_log
		WHEN NEW.outcome = 'consumed' BEGIN SELECT RAISE(ABORT, 'audit unavailable'); END`); err != nil {
		t.Fatal(err)
	}
	rec := tg.post("ops-agent", "git.open_pr", `{"approvalId":"`+id+`","payload":`+validOpenPR+`}`)
	if rec.Code < 500 || !strings.Contains(rec.Body.String(), "approval was not used") {
		t.Fatalf("want a 5xx saying the approval was not used, got %d: %s", rec.Code, rec.Body.String())
	}
	got, err := tg.st.GetApproval(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != domain.ApprovalApproved {
		t.Fatalf("status %q after a failed audit, want approved", got.Status)
	}
	if n := len(tg.pr.calls()); n != 0 {
		t.Fatalf("action ran %d times", n)
	}
	if _, err := db.Exec(`DROP TRIGGER fail_consumed_audit`); err != nil {
		t.Fatal(err)
	}
	if rec := tg.post("ops-agent", "git.open_pr", `{"approvalId":"`+id+`","payload":`+validOpenPR+`}`); rec.Code != http.StatusOK {
		t.Fatalf("retry after the audit recovered: want 200, got %d: %s", rec.Code, rec.Body.String())
	}
}

// M4: caller mistakes do not count on the breaker, so one workload cannot shut
// a capability for the others with bad payloads.
func TestCallerFaultsDoNotTripTheBreaker(t *testing.T) {
	tg := setupTypedGate(t, typedGateOptions{effect: domain.EffectAllow, workloads: []string{"alpha", "beta"}})
	bad := []string{
		`{"owner":"acme","repo":"widgets","head":"main","base":"main","title":"Fix"}`,
		`{"owner":"acme","repo":"widgets","head":"sablecourt/x","base":"main"}`,
		`{"owner":"acme","repo":"other","head":"sablecourt/x","base":"main","title":"Fix"}`,
		`{"owner":"acme","repo":"widgets","head":"sablecourt/x y","base":"main","title":"Fix"}`,
	}
	for i := 0; i < 3; i++ {
		for _, p := range bad {
			if rec := tg.post("alpha", "git.open_pr", `{"payload":`+p+`}`); rec.Code == http.StatusOK {
				t.Fatalf("bad payload accepted: %s", p)
			}
		}
	}
	for _, w := range []string{"beta", "alpha"} {
		if rec := tg.post(w, "git.open_pr", `{"payload":`+validOpenPR+`}`); rec.Code != http.StatusOK {
			t.Fatalf("%s blocked by caller faults: %d %s", w, rec.Code, rec.Body.String())
		}
	}
}

// M4: downstream failures trip the breaker of the workload that met them, not
// the capability for every workload.
func TestDownstreamFailuresTripOnlyThatWorkload(t *testing.T) {
	tg := setupTypedGate(t, typedGateOptions{effect: domain.EffectAllow, workloads: []string{"alpha", "beta"}})
	tg.pr.fail.Store(true)
	for i := 0; i < 3; i++ {
		if rec := tg.post("alpha", "git.open_pr", `{"payload":`+validOpenPR+`}`); rec.Code != http.StatusBadGateway {
			t.Fatalf("want 502, got %d: %s", rec.Code, rec.Body.String())
		}
	}
	tg.pr.fail.Store(false)
	if rec := tg.post("alpha", "git.open_pr", `{"payload":`+validOpenPR+`}`); rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "circuit_breaker_open") {
		t.Fatalf("alpha: want breaker open, got %d: %s", rec.Code, rec.Body.String())
	}
	if rec := tg.post("beta", "git.open_pr", `{"payload":`+validOpenPR+`}`); rec.Code != http.StatusOK {
		t.Fatalf("beta blocked by alpha's failures: %d %s", rec.Code, rec.Body.String())
	}
}

// M5: the repository git.open_pr may target is fixed at start; a later change
// of the checkout's origin is refused, and each action's audit row names it.
func TestOpenPRTargetIsPinnedAtStart(t *testing.T) {
	tg := setupTypedGate(t, typedGateOptions{effect: domain.EffectAllow})
	tg.setOrigin(t, "https://github.com/acme/other.git")
	other := strings.Replace(validOpenPR, `"widgets"`, `"other"`, 1)
	for _, p := range []string{other, validOpenPR} {
		if rec := tg.post("ops-agent", "git.open_pr", `{"payload":`+p+`}`); rec.Code == http.StatusOK {
			t.Fatalf("origin changed after start, %s accepted", p)
		}
	}
	if n := len(tg.pr.calls()); n != 0 {
		t.Fatalf("%d requests reached GitHub", n)
	}
	tg.setOrigin(t, "git@github.com:acme/widgets.git")
	if rec := tg.post("ops-agent", "git.open_pr", `{"payload":`+validOpenPR+`}`); rec.Code != http.StatusOK {
		t.Fatalf("pinned repository: want 200, got %d: %s", rec.Code, rec.Body.String())
	}
	records, err := tg.st.ListAudit(context.Background(), 20)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, r := range records {
		if r.Category == "data_action_gate" && r.Outcome == "allowed" && strings.Contains(r.DetailJSON, `"allowed_repo":"acme/widgets"`) {
			found = true
		}
	}
	if !found {
		t.Fatalf("no allowed action audit row names the pinned repository: %+v", records)
	}
}

// M5: an explicit reload re-reads the origin; a reload that cannot read a
// GitHub origin leaves no repository allowed.
func TestRepoBindingResolveOnReload(t *testing.T) {
	tg := setupTypedGate(t, typedGateOptions{effect: domain.EffectAllow})
	binding := &RepoBinding{gate: tg.gate, repoRoot: tg.repo}
	other := strings.Replace(validOpenPR, `"widgets"`, `"other"`, 1)

	tg.setOrigin(t, "https://github.com/acme/other.git")
	if err := binding.Resolve(context.Background()); err != nil {
		t.Fatal(err)
	}
	if rec := tg.post("ops-agent", "git.open_pr", `{"payload":`+other+`}`); rec.Code != http.StatusOK {
		t.Fatalf("after reload: want 200 for acme/other, got %d: %s", rec.Code, rec.Body.String())
	}
	if rec := tg.post("ops-agent", "git.open_pr", `{"payload":`+validOpenPR+`}`); rec.Code != http.StatusForbidden {
		t.Fatalf("after reload: want 403 for acme/widgets, got %d: %s", rec.Code, rec.Body.String())
	}

	tg.setOrigin(t, "https://gitlab.example/acme/other.git")
	if err := binding.Resolve(context.Background()); err == nil {
		t.Fatal("Resolve accepted a non-GitHub origin")
	}
	tg.setOrigin(t, "https://github.com/acme/other.git")
	if rec := tg.post("ops-agent", "git.open_pr", `{"payload":`+other+`}`); rec.Code != http.StatusForbidden {
		t.Fatalf("failed reload must leave nothing allowed, got %d: %s", rec.Code, rec.Body.String())
	}
}
