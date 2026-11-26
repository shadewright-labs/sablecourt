package mcpgate

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
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

// slowStore stands for a pause between the gate deciding to consume and the
// store running its statement, such as a suspended daemon.
type slowStore struct {
	store.Store
	wait time.Duration
}

func (s slowStore) ConsumeApproval(ctx context.Context, id, taskID, capability, payloadSHA256 string) error {
	time.Sleep(s.wait)
	return s.Store.ConsumeApproval(ctx, id, taskID, capability, payloadSHA256)
}

// The expiry is compared with the database's clock inside the consuming
// statement, so a pause that spans the expiry refuses the approval.
func TestApprovalExpiryUsesTheDatabaseClock(t *testing.T) {
	tg := setupTypedGate(t, typedGateOptions{
		effect: domain.EffectApprove,
		wrapStore: func(inner store.Store) store.Store {
			return slowStore{Store: inner, wait: 500 * time.Millisecond}
		},
	})
	id := requestAndApprove(t, tg, "git.open_pr", validOpenPR)
	soon := time.Now().UTC().Add(200 * time.Millisecond).Format(time.RFC3339Nano)
	if _, err := tg.rawDB(t).Exec(`UPDATE approvals SET expires_at = ? WHERE id = ?`, soon, id); err != nil {
		t.Fatal(err)
	}
	rec := tg.post("ops-agent", "git.open_pr", `{"approvalId":"`+id+`","payload":`+validOpenPR+`}`)
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "approval_expired") {
		t.Fatalf("a pause spanning expiry must refuse: got %d: %s", rec.Code, rec.Body.String())
	}
	if n := len(tg.pr.calls()); n != 0 {
		t.Fatalf("action ran %d times", n)
	}
}

// Consumption and its audit row are two steps, in the order that fails
// closed: when the audit write fails after the approval was consumed, the
// action does not run, the caller gets an error saying the approval is spent,
// and a retry with the same approval is refused.
func TestFailedConsumptionAuditStopsTheActionAndSpendsTheApproval(t *testing.T) {
	tg := setupTypedGate(t, typedGateOptions{effect: domain.EffectApprove})
	id := requestAndApprove(t, tg, "git.open_pr", validOpenPR)
	db := tg.rawDB(t)
	if _, err := db.Exec(`CREATE TRIGGER fail_consumed_audit BEFORE INSERT ON audit_log
		WHEN NEW.outcome = 'consumed' BEGIN SELECT RAISE(ABORT, 'audit unavailable'); END`); err != nil {
		t.Fatal(err)
	}
	rec := tg.post("ops-agent", "git.open_pr", `{"approvalId":"`+id+`","payload":`+validOpenPR+`}`)
	if rec.Code < 500 || !strings.Contains(rec.Body.String(), "approval is spent") || !strings.Contains(rec.Body.String(), "did not run") {
		t.Fatalf("want a 5xx saying the approval is spent and the action did not run, got %d: %s", rec.Code, rec.Body.String())
	}
	if n := len(tg.pr.calls()); n != 0 {
		t.Fatalf("action ran %d times", n)
	}
	got, err := tg.st.GetApproval(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != domain.ApprovalConsumed {
		t.Fatalf("status %q after a failed audit, want consumed", got.Status)
	}
	if _, err := db.Exec(`DROP TRIGGER fail_consumed_audit`); err != nil {
		t.Fatal(err)
	}
	rec = tg.post("ops-agent", "git.open_pr", `{"approvalId":"`+id+`","payload":`+validOpenPR+`}`)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("retry with the spent approval: want 403, got %d: %s", rec.Code, rec.Body.String())
	}
	if n := len(tg.pr.calls()); n != 0 {
		t.Fatalf("action ran %d times after the retry", n)
	}
}

// A body longer than the limit is refused, not cut at the limit. A valid
// envelope padded to exactly the limit and followed by more data must not
// pass as the envelope alone.
func TestBodyPastTheLimitIsRefused(t *testing.T) {
	tg := setupTypedGate(t, typedGateOptions{effect: domain.EffectAllow})
	env := `{"payload":` + validOpenPR + `}`
	padded := env + strings.Repeat(" ", maxActionBody-len(env))
	for _, tail := range []string{"null", "{}", " "} {
		if rec := tg.post("ops-agent", "git.open_pr", padded+tail); rec.Code == http.StatusOK {
			t.Fatalf("envelope padded to the limit plus %q accepted", tail)
		}
	}
	if n := len(tg.pr.calls()); n != 0 {
		t.Fatalf("%d requests reached GitHub", n)
	}
	if rec := tg.post("ops-agent", "git.open_pr", padded); rec.Code != http.StatusOK {
		t.Fatalf("envelope padded to exactly the limit: want 200, got %d: %s", rec.Code, rec.Body.String())
	}
}
