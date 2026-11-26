package mcpgate

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/shadewright-labs/sablecourt/internal/domain"
)

// The repository git.open_pr may target is fixed at start; a later change of
// the checkout's origin is refused, and each action's audit row names it.
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

// A binding whose start read found no GitHub origin refuses every target, and
// a later reload with a GitHub origin does not pin one: the pin is taken only
// at start.
func TestRepoBindingNeverPinsOnReload(t *testing.T) {
	tg := setupTypedGate(t, typedGateOptions{effect: domain.EffectAllow})
	tg.setOrigin(t, "https://gitlab.example/acme/widgets.git")
	tg.gate.SetAllowedRepo("")
	binding := &RepoBinding{gate: tg.gate, repoRoot: tg.repo}
	if _, err := binding.Pinned(); err == nil {
		t.Fatal("a non-GitHub origin was pinned")
	}
	tg.setOrigin(t, "https://github.com/acme/widgets.git")
	if err := binding.Recheck(context.Background()); err == nil {
		t.Fatal("Recheck after a failed start pin must report that nothing is pinned")
	}
	if rec := tg.post("ops-agent", "git.open_pr", `{"payload":`+validOpenPR+`}`); rec.Code != http.StatusForbidden {
		t.Fatalf("nothing pinned at start: want 403 after reload, got %d: %s", rec.Code, rec.Body.String())
	}
}

func driftRows(t *testing.T, tg *typedGate) []string {
	t.Helper()
	records, err := tg.st.ListAudit(context.Background(), 100)
	if err != nil {
		t.Fatal(err)
	}
	var rows []string
	for _, r := range records {
		if r.Outcome == "denied" && (strings.Contains(r.DetailJSON, "repo_origin_changed") || strings.Contains(r.DetailJSON, "repo_origin_unreadable")) {
			rows = append(rows, r.DetailJSON)
		}
	}
	return rows
}

// A reload re-reads the origin only to compare it: a changed origin stays
// refused, and the drift is written to the audit log once per change of the
// observed origin, not on every reload.
func TestReloadNeverRepinsAndRecordsEachDriftOnce(t *testing.T) {
	tg := setupTypedGate(t, typedGateOptions{effect: domain.EffectAllow})
	other := strings.Replace(validOpenPR, `"widgets"`, `"other"`, 1)
	reload := func() error {
		err := tg.binding.Recheck(context.Background()) // what every daemon reload calls
		tg.gate.Reload(tg.bundle, tg.policy)
		return err
	}
	tg.setOrigin(t, "https://github.com/acme/other.git")
	for i := 0; i < 3; i++ {
		if err := reload(); err == nil {
			t.Fatalf("reload %d: Recheck must report the drift", i+1)
		}
		if rec := tg.post("ops-agent", "git.open_pr", `{"payload":`+other+`}`); rec.Code != http.StatusForbidden {
			t.Fatalf("reload %d: origin changed to acme/other, want 403, got %d: %s", i+1, rec.Code, rec.Body.String())
		}
	}
	if n := len(tg.pr.calls()); n != 0 {
		t.Fatalf("%d requests reached GitHub", n)
	}
	rows := driftRows(t, tg)
	if len(rows) != 1 || !strings.Contains(rows[0], `"allowed_repo":"acme/widgets"`) || !strings.Contains(rows[0], "acme/other") {
		t.Fatalf("want one drift row naming the pin and acme/other after three reloads, got %v", rows)
	}

	// Another origin is another change and gets its own row.
	tg.setOrigin(t, "https://github.com/acme/third.git")
	_ = reload()
	_ = reload()
	if rows := driftRows(t, tg); len(rows) != 2 {
		t.Fatalf("want a second drift row for acme/third, got %v", rows)
	}

	// Back on the pinned origin the action works again without a restart, and
	// drifting away again is a new change.
	tg.setOrigin(t, "https://github.com/acme/widgets.git")
	if err := reload(); err != nil {
		t.Fatalf("Recheck on the pinned origin: %v", err)
	}
	if rec := tg.post("ops-agent", "git.open_pr", `{"payload":`+validOpenPR+`}`); rec.Code != http.StatusOK {
		t.Fatalf("pinned origin restored: want 200, got %d: %s", rec.Code, rec.Body.String())
	}
	tg.setOrigin(t, "https://github.com/acme/other.git")
	_ = reload()
	if rows := driftRows(t, tg); len(rows) != 3 {
		t.Fatalf("want a third drift row after drifting again, got %v", rows)
	}
}
