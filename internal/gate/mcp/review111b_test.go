package mcpgate

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/shadewright-labs/sablecourt/internal/store"
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

// lostCommitStore stands for a PostgreSQL COMMIT whose acknowledgement is lost:
// the consumption may or may not have committed, and the store says so.
type lostCommitStore struct {
	store.Store
	commit bool
}

func (s lostCommitStore) ConsumeApproval(ctx context.Context, req store.ConsumeRequest) error {
	if s.commit {
		if err := s.Store.ConsumeApproval(ctx, req); err != nil {
			return err
		}
	}
	return fmt.Errorf("%w: connection reset", store.ErrCommitUnknown)
}

// Medium 5: when the store cannot tell whether the consumption committed, the
// gate does not promise the approval was unused. It re-reads the approval and
// says what it found.
func TestUnknownCommitOutcomeIsNotReportedAsUnused(t *testing.T) {
	for _, tc := range []struct {
		name   string
		commit bool
		want   string
	}{
		{"committed", true, "was used"},
		{"not committed", false, "still approved"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			commit := tc.commit
			tg := setupTypedGate(t, typedGateOptions{effect: "approve", wrapStore: func(inner store.Store) store.Store {
				return lostCommitStore{Store: inner, commit: commit}
			}})
			id := requestAndApprove(t, tg, "git.open_pr", validOpenPR)
			rec := tg.post("ops-agent", "git.open_pr", `{"approvalId":"`+id+`","payload":`+validOpenPR+`}`)
			body := rec.Body.String()
			if rec.Code != http.StatusServiceUnavailable || strings.Contains(body, "was not used") || !strings.Contains(body, tc.want) {
				t.Fatalf("want 503 saying %q and not promising an unused approval, got %d: %s", tc.want, rec.Code, body)
			}
			if n := len(tg.pr.calls()); n != 0 {
				t.Fatalf("action ran %d times", n)
			}
		})
	}
}

// Low 7: a body longer than the limit is refused, not cut at the limit. A
// valid envelope padded to exactly the limit and followed by more data must
// not pass as the envelope alone.
func TestBodyPastTheLimitIsRefused(t *testing.T) {
	tg := setupTypedGate(t, typedGateOptions{effect: "allow"})
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
