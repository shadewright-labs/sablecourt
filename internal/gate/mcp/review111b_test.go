package mcpgate

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

// High 1: the repository pin is taken once at start. A reload re-reads the
// origin only to compare it: a changed origin stays refused, and the drift is
// written to the audit log.
func TestReloadNeverRepinsTheRepository(t *testing.T) {
	tg := setupTypedGate(t, typedGateOptions{effect: "allow"})
	other := strings.Replace(validOpenPR, `"widgets"`, `"other"`, 1)
	tg.setOrigin(t, "https://github.com/acme/other.git")
	for i := 0; i < 3; i++ {
		_ = tg.binding.Recheck(context.Background()) // what every daemon reload calls
		tg.gate.Reload(tg.bundle, tg.policy)
		if rec := tg.post("ops-agent", "git.open_pr", `{"payload":`+other+`}`); rec.Code != http.StatusForbidden {
			t.Fatalf("reload %d: origin changed to acme/other, want 403, got %d: %s", i+1, rec.Code, rec.Body.String())
		}
	}
	if n := len(tg.pr.calls()); n != 0 {
		t.Fatalf("%d requests reached GitHub", n)
	}
	records, err := tg.st.ListAudit(context.Background(), 50)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, r := range records {
		if r.Outcome == "denied" && strings.Contains(r.DetailJSON, "repo_origin_changed") &&
			strings.Contains(r.DetailJSON, `"allowed_repo":"acme/widgets"`) && strings.Contains(r.DetailJSON, "acme/other") {
			found = true
		}
	}
	if !found {
		t.Fatalf("no audit row records the origin drift at reload: %+v", records)
	}
	// Back on the pinned origin the action works again without a restart.
	tg.setOrigin(t, "https://github.com/acme/widgets.git")
	if err := tg.binding.Recheck(context.Background()); err != nil {
		t.Fatalf("Recheck on the pinned origin: %v", err)
	}
	if rec := tg.post("ops-agent", "git.open_pr", `{"payload":`+validOpenPR+`}`); rec.Code != http.StatusOK {
		t.Fatalf("pinned origin restored: want 200, got %d: %s", rec.Code, rec.Body.String())
	}
}
