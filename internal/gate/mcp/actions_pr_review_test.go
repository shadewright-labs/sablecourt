package mcpgate

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/shadewright-labs/sablecourt/internal/domain"
	"github.com/shadewright-labs/sablecourt/internal/gate/model"
	"github.com/shadewright-labs/sablecourt/internal/secret"
	"github.com/shadewright-labs/sablecourt/internal/store"
)

// The task lease of the PR test gate expires an hour after the gate's fixed
// clock, not an hour after the host clock, so the tests pass at any host time.
func TestPRGateLeaseFollowsGateClock(t *testing.T) {
	gate, st, _, token := setupPRGate(t, &fakePRBackend{}, domain.EffectApprove, nil)
	now := gate.now()
	lease, err := st.GetActiveLeaseByTokenHash(context.Background(), model.HashToken(token), now)
	if err != nil {
		t.Fatalf("lease not active at the gate clock: %v", err)
	}
	if want := now.Add(time.Hour); !lease.ExpiresAt.Equal(want) {
		t.Fatalf("lease expires at %s, want %s (gate clock plus one hour)", lease.ExpiresAt, want)
	}
}

// failOncePRStore fails the first write of an opened pull request record.
type failOncePRStore struct {
	store.Store
	failed atomic.Bool
}

func (s *failOncePRStore) PutMemory(ctx context.Context, e domain.MemoryEntry) error {
	if e.Namespace == openedPRsNamespace && s.failed.CompareAndSwap(false, true) {
		return errors.New("disk full")
	}
	return s.Store.PutMemory(ctx, e)
}

// A failed provenance write after the pull request was created must not
// strand it: a retry of git.open_pr finds the open pull request for the same
// head and base, records it and returns it, and checks then accept it.
func TestOpenPRRetryRecoversAfterFailedRecord(t *testing.T) {
	sha := strings.Repeat("a1", 20)
	backend := &fakePRBackend{openNumber: 42, report: passingReport(sha)}
	gate, _, _, token := setupPRGateWithStore(t, backend, domain.EffectApprove, []string{"shadewright-labs/sablecourt"},
		func(s store.Store) store.Store { return &failOncePRStore{Store: s} })
	h := gate.ActionHandler()
	body := `{"payload":{"owner":"shadewright-labs","repo":"sablecourt","head":"sablecourt/task-pr-1","base":"main","title":"Fix"}}`

	rec := postAction(t, h, http.MethodPost, "/git.open_pr", token, body)
	if rec.Code == http.StatusOK {
		t.Fatalf("first open_pr should fail on the record write: %s", rec.Body.String())
	}
	rec = postAction(t, h, http.MethodPost, "/git.open_pr", token, body)
	if rec.Code != http.StatusOK {
		t.Fatalf("retry of open_pr: %d %s", rec.Code, rec.Body.String())
	}
	var res map[string]string
	_ = json.Unmarshal(rec.Body.Bytes(), &res)
	if res["pr_number"] != "42" || !strings.HasSuffix(res["pr_url"], "/pull/42") {
		t.Fatalf("retry returned %v, want the existing pull request 42", res)
	}
	if backend.openCalls != 2 {
		t.Fatalf("create calls = %d, want 2", backend.openCalls)
	}
	rec = postAction(t, h, http.MethodPost, "/git.pr_checks", token,
		`{"payload":{"owner":"shadewright-labs","repo":"sablecourt","pr_number":42}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("pr_checks after recovery: %d %s", rec.Code, rec.Body.String())
	}
}

// The recovery adopts an open pull request only when it is the pinned
// repository's own task branch into the base asked for, open, and at the
// commit the gate pushed for the task. Anything else is refused with 409 and
// nothing is recorded.
func TestOpenPRRetryAdoptsOnlyMatchingPR(t *testing.T) {
	pushedSHA := strings.Repeat("a1", 20)
	cases := []struct {
		name     string
		head     string
		edit     func(*FoundPR)
		pushed   string // commit recorded as pushed for the task; none when empty
		wantCode int
		wantErr  string
	}{
		{name: "valid pull request is adopted", wantCode: http.StatusOK},
		{name: "valid pull request at the pushed commit is adopted", pushed: pushedSHA, wantCode: http.StatusOK},
		{name: "head is another branch", head: "sablecourt/other-task", wantCode: http.StatusConflict, wantErr: "task branch"},
		{name: "fork with the same branch name", edit: func(p *FoundPR) { p.HeadRepo, p.HeadRepoID = "fork-owner/sablecourt", 2 }, wantCode: http.StatusConflict, wantErr: "head repository"},
		{name: "fork reporting the pinned name with another id", edit: func(p *FoundPR) { p.HeadRepoID = 2 }, wantCode: http.StatusConflict, wantErr: "head repository"},
		{name: "head repository missing", edit: func(p *FoundPR) { p.HeadRepo, p.HeadRepoID = "", 0 }, wantCode: http.StatusConflict, wantErr: "head repository"},
		{name: "different base", edit: func(p *FoundPR) { p.BaseRef = "release" }, wantCode: http.StatusConflict, wantErr: "base"},
		{name: "closed pull request", edit: func(p *FoundPR) { p.State = "closed" }, wantCode: http.StatusConflict, wantErr: "not open"},
		{name: "head is not the pushed commit", pushed: strings.Repeat("b2", 20), wantCode: http.StatusConflict, wantErr: "pushed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			backend := &fakePRBackend{openNumber: 42, foundHeadSHA: pushedSHA, foundEdit: tc.edit}
			gate, st, _, token := setupPRGateWithStore(t, backend, domain.EffectApprove, []string{"shadewright-labs/sablecourt"},
				func(s store.Store) store.Store { return &failOncePRStore{Store: s} })
			if tc.pushed != "" {
				if err := recordPushedCommit(context.Background(), gate, "task-pr-1", PushBranchResult{Branch: "sablecourt/task-pr-1", Commit: tc.pushed}); err != nil {
					t.Fatal(err)
				}
			}
			h := gate.ActionHandler()
			head := tc.head
			if head == "" {
				head = "sablecourt/task-pr-1"
			}
			body := `{"payload":{"owner":"shadewright-labs","repo":"sablecourt","head":"` + head + `","base":"main","title":"Fix"}}`
			_ = postAction(t, h, http.MethodPost, "/git.open_pr", token, body)
			rec := postAction(t, h, http.MethodPost, "/git.open_pr", token, body)
			if rec.Code != tc.wantCode {
				t.Fatalf("retry: code %d, want %d: %s", rec.Code, tc.wantCode, rec.Body.String())
			}
			entry, err := st.GetMemory(context.Background(), openedPRsNamespace, "shadewright-labs/sablecourt#42")
			if tc.wantCode != http.StatusOK {
				if !strings.Contains(errorText(t, rec), tc.wantErr) {
					t.Fatalf("error %q, want it to name %q", errorText(t, rec), tc.wantErr)
				}
				if !errors.Is(err, store.ErrNotFound) {
					t.Fatalf("a refused pull request was recorded: %v", err)
				}
				return
			}
			var rec2 OpenedPRRecord
			if err != nil || json.Unmarshal([]byte(entry.Value), &rec2) != nil || !rec2.Adopted || rec2.HeadSHA != pushedSHA || entry.Author != "gate" {
				t.Fatalf("adopted pull request record = %+v (%v)", entry, err)
			}
			if !strings.Contains(rec.Body.String(), `"status":"adopted"`) {
				t.Fatalf("retry result does not say adopted: %s", rec.Body.String())
			}
		})
	}
}

// An adopted pull request still needs an approval to merge.
func TestAdoptedPRMergeStillNeedsApproval(t *testing.T) {
	sha := strings.Repeat("a1", 20)
	backend := &fakePRBackend{openNumber: 42, report: passingReport(sha)}
	gate, _, _, token := setupPRGateWithStore(t, backend, domain.EffectAllow, []string{"shadewright-labs/sablecourt"},
		func(s store.Store) store.Store { return &failOncePRStore{Store: s} })
	h := gate.ActionHandler()
	openTestPRExpect(t, h, token, false)
	openTestPRExpect(t, h, token, true)
	rec := postAction(t, h, http.MethodPost, "/git.merge_pr", token,
		`{"payload":{"owner":"shadewright-labs","repo":"sablecourt","pr_number":42,"commit_sha":"`+sha+`"}}`)
	if rec.Code != http.StatusAccepted || backend.mergeCalls != 0 {
		t.Fatalf("merge of an adopted pull request: want 202 and no merge, got %d calls=%d %s", rec.Code, backend.mergeCalls, rec.Body.String())
	}
}

func openTestPRExpect(t *testing.T, h http.Handler, token string, ok bool) {
	t.Helper()
	rec := postAction(t, h, http.MethodPost, "/git.open_pr", token,
		`{"payload":{"owner":"shadewright-labs","repo":"sablecourt","head":"sablecourt/task-pr-1","base":"main","title":"Fix"}}`)
	if (rec.Code == http.StatusOK) != ok {
		t.Fatalf("open_pr: %d %s", rec.Code, rec.Body.String())
	}
}

// A pull request whose base was changed after the gate opened it is refused:
// the merge would land on a branch nobody approved.
func TestMergePRRefusesChangedBase(t *testing.T) {
	sha := strings.Repeat("a1", 20)
	backend := &fakePRBackend{openNumber: 42, report: passingReport(sha)}
	gate, _, _, token := setupPRGate(t, backend, domain.EffectApprove, []string{"shadewright-labs/sablecourt"})
	h := gate.ActionHandler()
	openTestPR(t, h, token) // base main
	backend.report.BaseBranch = "release"
	rec := postAction(t, h, http.MethodPost, "/git.merge_pr", token,
		`{"payload":{"owner":"shadewright-labs","repo":"sablecourt","pr_number":42,"commit_sha":"`+sha+`"}}`)
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "base") {
		t.Fatalf("merge after base change: want 403 wrong_base, got %d %s", rec.Code, rec.Body.String())
	}
	if backend.mergeCalls != 0 {
		t.Fatalf("MergePR called %d times", backend.mergeCalls)
	}
}

// githubFake serves the pull request, its check runs, statuses and branch
// protection; the test overrides the pages it cares about.
func githubFake(t *testing.T, sha string, runs func(page string) any, protection any) *GitHubPROpener {
	t.Helper()
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/org/app/pulls/7":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"state": "open", "mergeable_state": "clean",
				"head": map[string]string{"sha": sha, "ref": "sablecourt/t7"},
				"base": map[string]string{"ref": "main"},
			})
		case "/repos/org/app/commits/" + sha + "/check-runs":
			_ = json.NewEncoder(w).Encode(runs(r.URL.Query().Get("page")))
		case "/repos/org/app/commits/" + sha + "/status":
			_ = json.NewEncoder(w).Encode(map[string]any{"total_count": 0, "statuses": []any{}})
		case "/repos/org/app/branches/main/protection/required_status_checks":
			_ = json.NewEncoder(w).Encode(protection)
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

func checkRun(id int64, name, conclusion string, appID int64) map[string]any {
	return map[string]any{
		"id": id, "name": name, "status": "completed", "conclusion": conclusion,
		"completed_at": "2026-11-27T13:00:00Z", "app": map[string]any{"id": appID},
	}
}

// A list whose total_count changes between pages, or that repeats an item,
// is refused even when the counts add up.
func TestGitHubCheckRunPagesMustBeConsistent(t *testing.T) {
	sha := strings.Repeat("d4", 20)
	page := func(from, n int) []map[string]any {
		var out []map[string]any
		for i := range n {
			out = append(out, checkRun(int64(from+i), "ci", "success", 1))
		}
		return out
	}
	cases := []struct {
		name    string
		runs    func(p string) any
		wantErr string
	}{
		{
			name: "total_count changes between pages",
			runs: func(p string) any {
				if p == "1" {
					return map[string]any{"total_count": 101, "check_runs": page(1, 100)}
				}
				return map[string]any{"total_count": 102, "check_runs": page(100, 2)}
			},
			wantErr: "total_count",
		},
		{
			name: "duplicate item across pages",
			runs: func(p string) any {
				if p == "1" {
					return map[string]any{"total_count": 101, "check_runs": page(1, 100)}
				}
				return map[string]any{"total_count": 101, "check_runs": page(100, 1)}
			},
			wantErr: "duplicate",
		},
		{
			name: "item without an id",
			runs: func(string) any {
				return map[string]any{"total_count": 1, "check_runs": []map[string]any{{"name": "ci", "status": "completed", "conclusion": "success"}}}
			},
			wantErr: "id",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gh := githubFake(t, sha, tc.runs, map[string]any{"contexts": []string{"ci"}})
			_, err := gh.CheckPR(context.Background(), PRChecksInput{Owner: "org", Repo: "app", PRNumber: 7})
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("CheckPR error = %v, want one naming %q", err, tc.wantErr)
			}
		})
	}
}

// Branch protection that names the app of a required check binds the check
// to that app: a result of the same name from another app neither fails nor
// satisfies it.
func TestRequiredChecksMatchApp(t *testing.T) {
	sha := strings.Repeat("d4", 20)
	protection := map[string]any{
		"contexts": []string{"ci"},
		"checks":   []map[string]any{{"context": "ci", "app_id": 1}},
	}
	cases := []struct {
		name string
		runs []map[string]any
		want string
	}{
		{"other app fails, required app passes", []map[string]any{checkRun(1, "ci", "success", 1), checkRun(2, "ci", "failure", 2)}, "passing"},
		{"only another app passes", []map[string]any{checkRun(2, "ci", "success", 2)}, "missing_required"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			runs := tc.runs
			gh := githubFake(t, sha, func(string) any {
				return map[string]any{"total_count": len(runs), "check_runs": runs}
			}, protection)
			report, err := gh.CheckPR(context.Background(), PRChecksInput{Owner: "org", Repo: "app", PRNumber: 7})
			if err != nil {
				t.Fatalf("CheckPR: %v", err)
			}
			evaluatePRChecks(&report, time.Date(2026, 11, 27, 14, 0, 0, 0, time.UTC))
			if report.Summary != tc.want {
				t.Fatalf("summary = %q, want %q (report %+v)", report.Summary, tc.want, report)
			}
		})
	}
}

// Required checks without an app (or app_id -1) still match by name only.
func TestRequiredChecksWithoutAppMatchAnyApp(t *testing.T) {
	sha := strings.Repeat("d4", 20)
	for _, protection := range []map[string]any{
		{"contexts": []string{"ci"}},
		{"checks": []map[string]any{{"context": "ci", "app_id": -1}}},
		{"checks": []map[string]any{{"context": "ci", "app_id": nil}}},
	} {
		gh := githubFake(t, sha, func(string) any {
			return map[string]any{"total_count": 1, "check_runs": []map[string]any{checkRun(1, "ci", "success", 5)}}
		}, protection)
		report, err := gh.CheckPR(context.Background(), PRChecksInput{Owner: "org", Repo: "app", PRNumber: 7})
		if err != nil {
			t.Fatalf("CheckPR: %v", err)
		}
		evaluatePRChecks(&report, time.Date(2026, 11, 27, 14, 0, 0, 0, time.UTC))
		if report.Summary != "passing" {
			t.Fatalf("protection %v: summary = %q, want passing", protection, report.Summary)
		}
	}
}

// OpenPR reports GitHub's status as a githubAPIError, and FindOpenPR asks for
// one page of at most two open pull requests of the same head and base and
// returns every field the adoption checks, including a fork's head repository.
func TestGitHubFindOpenPR(t *testing.T) {
	var gotQuery string
	pulls := []map[string]any{}
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/repos/org/app/pulls":
			w.WriteHeader(http.StatusUnprocessableEntity)
			_, _ = w.Write([]byte(`{"message":"Validation Failed"}`))
		case r.Method == http.MethodGet && r.URL.Path == "/repos/org/app/pulls":
			gotQuery = r.URL.RawQuery
			_ = json.NewEncoder(w).Encode(pulls)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
	gh := &GitHubPROpener{
		Secrets: secret.NewMemoryStore(map[string]string{"github_token": "tok-test"}),
		APIURL:  "https://api.github.test",
		Client: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, r)
			return rec.Result(), nil
		})},
	}
	in := OpenPRInput{Owner: "org", Repo: "app", Head: "sablecourt/t7", Base: "main", Title: "x"}
	_, err := gh.OpenPR(context.Background(), in)
	var apiErr *githubAPIError
	if !errors.As(err, &apiErr) || apiErr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("OpenPR error = %v, want a githubAPIError with 422", err)
	}

	pulls = []map[string]any{{
		"number": 7, "state": "open", "html_url": "https://github.com/org/app/pull/7",
		"head": map[string]any{"ref": "sablecourt/t7", "sha": strings.Repeat("AB", 20), "repo": map[string]any{"id": 11, "full_name": "fork/app"}},
		"base": map[string]any{"ref": "main", "repo": map[string]any{"id": 10, "full_name": "org/app"}},
	}}
	found, err := gh.FindOpenPR(context.Background(), in)
	if err != nil || len(found) != 1 {
		t.Fatalf("FindOpenPR = %+v, %v", found, err)
	}
	want := FoundPR{Number: 7, URL: "https://github.com/org/app/pull/7", State: "open", HeadRef: "sablecourt/t7",
		HeadSHA: strings.Repeat("ab", 20), HeadRepo: "fork/app", HeadRepoID: 11, BaseRef: "main", BaseRepoID: 10}
	if found[0] != want {
		t.Fatalf("FindOpenPR = %+v, want %+v", found[0], want)
	}
	for _, part := range []string{"state=open", "head=org%3Asablecourt%2Ft7", "base=main", "per_page=2"} {
		if !strings.Contains(gotQuery, part) {
			t.Fatalf("query %q lacks %q", gotQuery, part)
		}
	}
}
