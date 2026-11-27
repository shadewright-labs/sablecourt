package mcpgate

import (
	"net/http"
	"strings"
	"testing"

	"github.com/shadewright-labs/sablecourt/internal/domain"
)

// A check report that leaves out the mergeable state or the head branch is
// missing evidence, not a clean answer: merge must be refused (fail closed).
func TestMergePRRefusesMissingMergeEvidence(t *testing.T) {
	sha := strings.Repeat("a1", 20)
	base := PRCheckReport{
		HeadSHA:        sha,
		HeadBranch:     "sablecourt/task-pr-1",
		BaseBranch:     "main",
		PRState:        "open",
		MergeableState: "clean",
		Checks:         []CheckRunState{{Name: "ci-ok", Status: "completed", Conclusion: "success"}},
	}
	cases := []struct {
		name   string
		mutate func(*PRCheckReport)
	}{
		{"empty mergeable state", func(r *PRCheckReport) { r.MergeableState = "" }},
		{"empty head branch", func(r *PRCheckReport) { r.HeadBranch = "" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rep := base
			rep.Checks = append([]CheckRunState(nil), base.Checks...)
			tc.mutate(&rep)
			backend := &fakePRBackend{openNumber: 42, report: rep}
			gate, _, _, token := setupPRGate(t, backend, domain.EffectApprove, []string{"shadewright-labs/sablecourt"})
			h := gate.ActionHandler()
			rec := postAction(t, h, http.MethodPost, "/git.open_pr", token,
				`{"payload":{"owner":"shadewright-labs","repo":"sablecourt","head":"sablecourt/task-pr-1","base":"main","title":"Fix gate"}}`)
			if rec.Code != http.StatusOK {
				t.Fatalf("open_pr: %d %s", rec.Code, rec.Body.String())
			}
			rec = postAction(t, h, http.MethodPost, "/git.merge_pr", token,
				`{"payload":{"owner":"shadewright-labs","repo":"sablecourt","pr_number":42,"commit_sha":"`+sha+`"}}`)
			if rec.Code == http.StatusAccepted || rec.Code == http.StatusOK {
				t.Fatalf("merge with missing evidence not refused: %d %s", rec.Code, rec.Body.String())
			}
			if backend.mergeCalls != 0 {
				t.Fatalf("MergePR called %d times", backend.mergeCalls)
			}
		})
	}
}
