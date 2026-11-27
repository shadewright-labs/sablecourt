package mcpgate

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/shadewright-labs/sablecourt/internal/domain"
	"github.com/shadewright-labs/sablecourt/internal/secret"
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

// Every evaluated check needs its own completion time: one check with a time
// must not cover a required check without one.
func TestMergePRRefusesAnyCheckWithoutResultTime(t *testing.T) {
	sha := strings.Repeat("a1", 20)
	done := time.Date(2026, 11, 27, 13, 0, 0, 0, time.UTC)
	cases := []struct {
		name     string
		required []string
		checks   []CheckRunState
	}{
		{"required check without time", []string{"ci-ok", "ci-notime"}, []CheckRunState{
			{Name: "ci-ok", Status: "completed", Conclusion: "success", CompletedAt: done},
			{Name: "ci-notime", Status: "completed", Conclusion: "success"},
		}},
		{"timed check is not required", []string{"ci-notime"}, []CheckRunState{
			{Name: "lint", Status: "completed", Conclusion: "success", CompletedAt: done},
			{Name: "ci-notime", Status: "completed", Conclusion: "success"},
		}},
		{"no required checks", nil, []CheckRunState{
			{Name: "ci-ok", Status: "completed", Conclusion: "success", CompletedAt: done},
			{Name: "ci-notime", Status: "completed", Conclusion: "success"},
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rep := passingReport(sha)
			rep.RequiredChecks = tc.required
			rep.Checks = tc.checks
			backend := &fakePRBackend{openNumber: 42, noResultTimes: true, report: rep}
			gate, _, _, token := setupPRGate(t, backend, domain.EffectApprove, []string{"shadewright-labs/sablecourt"})
			h := gate.ActionHandler()
			openTestPR(t, h, token)
			rec := postAction(t, h, http.MethodPost, "/git.merge_pr", token,
				`{"payload":{"owner":"shadewright-labs","repo":"sablecourt","pr_number":42,"commit_sha":"`+sha+`"}}`)
			if rec.Code != http.StatusConflict || !strings.Contains(errorText(t, rec), "no completion time") {
				t.Fatalf("check without result time: want 409, got %d %s", rec.Code, rec.Body.String())
			}
		})
	}
}

// An approval that does not authorize the request is refused before any
// live check: no GitHub read and no evidence write happen for it.
func TestMergePRBogusApprovalReadsNoChecks(t *testing.T) {
	sha := strings.Repeat("a1", 20)
	backend := &fakePRBackend{openNumber: 42, report: passingReport(sha)}
	gate, _, _, token := setupPRGate(t, backend, domain.EffectApprove, []string{"shadewright-labs/sablecourt"})
	h := gate.ActionHandler()
	openTestPR(t, h, token)
	rec := postAction(t, h, http.MethodPost, "/git.merge_pr", token,
		`{"approvalId":"apr_bogus","payload":{"owner":"shadewright-labs","repo":"sablecourt","pr_number":42,"commit_sha":"`+sha+`"}}`)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("bogus approval: want 403, got %d %s", rec.Code, rec.Body.String())
	}
	if backend.checkCalls != 0 || backend.mergeCalls != 0 {
		t.Fatalf("bogus approval reached GitHub: check calls %d, merge calls %d", backend.checkCalls, backend.mergeCalls)
	}
}

// fakeGitHubChecks serves the check endpoints of one pull request. Check
// runs are paged by per_page (GitHub's default 30) and page.
type fakeGitHubChecks struct {
	sha        string
	runs       []map[string]string
	totalRuns  int // reported total_count; len(runs) when zero
	statusCode int // of the commit status endpoint; 200 when zero
	protCode   int // of the required status checks endpoint; 200 when zero
	protMsg    string
}

func (f *fakeGitHubChecks) opener() *GitHubPROpener {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/org/app/pulls/7":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"state": "open", "mergeable_state": "clean",
				"head": map[string]string{"sha": f.sha, "ref": "sablecourt/t7"},
				"base": map[string]string{"ref": "main"},
			})
		case "/repos/org/app/commits/" + f.sha + "/check-runs":
			per, _ := strconv.Atoi(r.URL.Query().Get("per_page"))
			if per <= 0 {
				per = 30
			}
			page, _ := strconv.Atoi(r.URL.Query().Get("page"))
			if page <= 0 {
				page = 1
			}
			lo := min((page-1)*per, len(f.runs))
			hi := min(lo+per, len(f.runs))
			total := f.totalRuns
			if total == 0 {
				total = len(f.runs)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"total_count": total, "check_runs": f.runs[lo:hi]})
		case "/repos/org/app/commits/" + f.sha + "/status":
			if f.statusCode != 0 {
				w.WriteHeader(f.statusCode)
				_ = json.NewEncoder(w).Encode(map[string]any{"message": "Not Found"})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"total_count": 0, "statuses": []any{}})
		case "/repos/org/app/branches/main/protection/required_status_checks":
			if f.protCode != 0 {
				w.WriteHeader(f.protCode)
				_ = json.NewEncoder(w).Encode(map[string]any{"message": f.protMsg})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"contexts": []string{"ci-ok"}})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
	return &GitHubPROpener{
		Secrets: secret.NewMemoryStore(map[string]string{"github_token": "tok-test"}),
		APIURL:  "https://api.github.test",
		Client: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, r)
			return rec.Result(), nil
		})},
	}
}

// CheckPR reads every page of check runs and refuses partial or unreadable
// check data instead of treating it as no checks.
func TestGitHubPROpenerCheckPRFailsClosedOnPartialData(t *testing.T) {
	sha := strings.Repeat("d4", 20)
	passing := func(n int) []map[string]string {
		runs := make([]map[string]string, n)
		for i := range runs {
			runs[i] = map[string]string{"name": "ci-ok", "status": "completed", "conclusion": "success", "completed_at": "2026-11-27T13:00:00Z"}
		}
		return runs
	}

	t.Run("failing run past the first pages", func(t *testing.T) {
		runs := append(passing(130), map[string]string{"name": "ci-ok", "status": "completed", "conclusion": "failure", "completed_at": "2026-11-27T13:05:00Z"})
		f := &fakeGitHubChecks{sha: sha, runs: runs}
		report, err := f.opener().CheckPR(context.Background(), PRChecksInput{Owner: "org", Repo: "app", PRNumber: 7})
		if err == nil {
			evaluatePRChecks(&report, time.Now().UTC())
			if report.AllRequiredPass || len(report.Checks) != len(runs) {
				t.Fatalf("read %d of %d check runs, all pass = %v", len(report.Checks), len(runs), report.AllRequiredPass)
			}
		}
	})

	errCases := []struct {
		name string
		f    *fakeGitHubChecks
	}{
		{"fewer runs than total_count", &fakeGitHubChecks{sha: sha, runs: passing(1), totalRuns: 31}},
		{"commit status 404", &fakeGitHubChecks{sha: sha, runs: passing(1), statusCode: http.StatusNotFound}},
		{"commit status 403", &fakeGitHubChecks{sha: sha, runs: passing(1), statusCode: http.StatusForbidden}},
		{"required checks 403", &fakeGitHubChecks{sha: sha, runs: passing(1), protCode: http.StatusForbidden, protMsg: "Resource not accessible by integration"}},
		{"required checks 404 without access", &fakeGitHubChecks{sha: sha, runs: passing(1), protCode: http.StatusNotFound, protMsg: "Not Found"}},
	}
	for _, tc := range errCases {
		t.Run(tc.name, func(t *testing.T) {
			report, err := tc.f.opener().CheckPR(context.Background(), PRChecksInput{Owner: "org", Repo: "app", PRNumber: 7})
			if err == nil {
				t.Fatalf("CheckPR accepted partial check data: %+v", report)
			}
		})
	}

	t.Run("unprotected base branch has no required checks", func(t *testing.T) {
		f := &fakeGitHubChecks{sha: sha, runs: passing(1), protCode: http.StatusNotFound, protMsg: "Branch not protected"}
		report, err := f.opener().CheckPR(context.Background(), PRChecksInput{Owner: "org", Repo: "app", PRNumber: 7})
		if err != nil || len(report.RequiredChecks) != 0 {
			t.Fatalf("unprotected branch: required %v err %v", report.RequiredChecks, err)
		}
	})
}
