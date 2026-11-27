package mcpgate

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/shadewright-labs/sablecourt/internal/approval"
	"github.com/shadewright-labs/sablecourt/internal/config"
	"github.com/shadewright-labs/sablecourt/internal/domain"
	"github.com/shadewright-labs/sablecourt/internal/gate/model"
	"github.com/shadewright-labs/sablecourt/internal/policy"
	"github.com/shadewright-labs/sablecourt/internal/secret"
	"github.com/shadewright-labs/sablecourt/internal/store/sqlite"
)

type fakePRBackend struct {
	// noResultTimes leaves CompletedAt as the report has it; otherwise a
	// completed check without one gets a time an hour ago.
	noResultTimes bool
	openNumber    int
	report        PRCheckReport
	checkErr      error
	mergeErr      error
	mergeCalls    int
	checkCalls    int
	lastMerge     MergePRRequest
}

func (f *fakePRBackend) OpenPR(_ context.Context, in OpenPRInput) (string, error) {
	num := f.openNumber
	if num == 0 {
		num = 42
	}
	return "https://github.com/" + in.Owner + "/" + in.Repo + "/pull/" + json.Number(string(rune('0'+num/10))+string(rune('0'+num%10))).String(), nil
}

func (f *fakePRBackend) CheckPR(_ context.Context, _ PRChecksInput) (PRCheckReport, error) {
	f.checkCalls++
	if f.checkErr != nil {
		return PRCheckReport{}, f.checkErr
	}
	rep := f.report
	rep.Checks = append([]CheckRunState(nil), f.report.Checks...)
	if !f.noResultTimes {
		for i := range rep.Checks {
			if rep.Checks[i].Status == "completed" && rep.Checks[i].CompletedAt.IsZero() {
				rep.Checks[i].CompletedAt = time.Now().UTC().Add(-time.Hour)
			}
		}
	}
	return rep, nil
}

func (f *fakePRBackend) MergePR(_ context.Context, req MergePRRequest) (MergePRResult, error) {
	f.mergeCalls++
	f.lastMerge = req
	if f.mergeErr != nil {
		return MergePRResult{}, f.mergeErr
	}
	return MergePRResult{
		Owner:       req.Owner,
		Repo:        req.Repo,
		PRNumber:    req.PRNumber,
		CommitSHA:   req.CommitSHA,
		MergeSHA:    strings.Repeat("9", 40),
		MergeMethod: req.MergeMethod,
		Status:      "merged",
	}, nil
}

func setupPRGate(t *testing.T, backend *fakePRBackend, mergeEffect domain.PolicyEffect, repos []string) (*Gate, *sqlite.SQLiteStore, *approval.Queue, string) {
	t.Helper()
	ctx := context.Background()
	st, err := sqlite.Open(filepath.Join(t.TempDir(), "sablecourt.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}

	wl := domain.Workload{
		Metadata: domain.ObjectMeta{Name: "release-agent", Zone: domain.ZoneRuntime},
		Spec: domain.WorkloadSpec{
			Class: domain.ClassLongLivedAgent,
			Grants: domain.GrantSet{
				MCPTools: []string{"git.pr_checks"},
				Actions:  []string{"git.open_pr", "git.pr_checks", "git.merge_pr"},
				Repos:    repos,
			},
		},
	}
	bundle := &config.Bundle{
		Policies: map[string]domain.Policy{
			"default": {
				Metadata: domain.ObjectMeta{Name: "default"},
				Spec: domain.PolicySpec{
					DefaultEffect: domain.EffectDeny,
					Rules: []domain.PolicyRule{
						{Workload: "release-agent", Capability: "action:git.open_pr", Effect: domain.EffectAllow},
						{Workload: "release-agent", Capability: "action:git.pr_checks", Effect: domain.EffectAllow},
						{Workload: "release-agent", Capability: "mcp:git.pr_checks", Effect: domain.EffectAllow},
						{Workload: "release-agent", Capability: "action:git.merge_pr", Effect: mergeEffect},
					},
				},
			},
		},
		Workloads: map[string]domain.Workload{wl.Metadata.Name: wl},
	}

	_ = st.UpsertWorkload(ctx, domain.WorkloadRecord{
		Name: wl.Metadata.Name, Zone: wl.Metadata.Zone, Class: wl.Spec.Class,
		ManifestDigest: "m1", AgentCardJSON: "{}",
	})
	_ = st.CreateTask(ctx, domain.Task{
		ID: "task-pr-1", IdempotencyKey: "idem-pr-1", WorkloadName: wl.Metadata.Name, State: domain.TaskStateWorking,
	})
	token, err := model.MintTaskToken(ctx, st, "task-pr-1", wl.Metadata.Name, time.Hour)
	if err != nil {
		t.Fatal(err)
	}

	now := time.Date(2026, 11, 27, 14, 0, 0, 0, time.UTC)
	eng := policy.NewEngine([]domain.Policy{bundle.Policies["default"]}, nil)
	queue := approval.NewQueue(st, autoApprover{})
	gate := New(Options{
		Store:     st,
		Bundle:    bundle,
		Policy:    eng,
		Approvals: queue,
		Now:       func() time.Time { return now },
	})
	RegisterBuiltinGitOpsWithOptions(gate, GitOpsOptions{
		RepoRoot:  initGitHubRepo(t, "https://github.com/shadewright-labs/sablecourt.git"),
		PROpener:  backend,
		PRChecker: backend,
		PRMerger:  backend,
	})
	return gate, st, queue, token
}

func TestPRChecksEvidenceAndMergePRApprovalFlow(t *testing.T) {
	ctx := context.Background()
	sha := strings.Repeat("a1", 20)
	backend := &fakePRBackend{
		openNumber: 42,
		report: PRCheckReport{
			HeadSHA:        sha,
			HeadBranch:     "sablecourt/task-pr-1",
			BaseBranch:     "main",
			PRState:        "open",
			MergeableState: "clean",
			RequiredChecks: []string{"ci-ok"},
			Checks: []CheckRunState{
				{Name: "ci-ok", Status: "completed", Conclusion: "success"},
			},
		},
	}
	// Policy says allow, yet git.merge_pr catalog still requires out-of-band approval.
	gate, st, queue, token := setupPRGate(t, backend, domain.EffectAllow, []string{"shadewright-labs/sablecourt"})
	h := gate.ActionHandler()

	// 1. Open PR through Sablecourt so provenance is recorded.
	rec := postAction(t, h, http.MethodPost, "/git.open_pr", token,
		`{"payload":{"owner":"shadewright-labs","repo":"sablecourt","head":"sablecourt/task-pr-1","base":"main","title":"Fix gate"}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("open_pr: %d %s", rec.Code, rec.Body.String())
	}

	// 2. Read PR checks and confirm timestamped evidence is persisted.
	rec = postAction(t, h, http.MethodPost, "/git.pr_checks", token,
		`{"payload":{"owner":"shadewright-labs","repo":"sablecourt","pr_number":42}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("pr_checks: %d %s", rec.Code, rec.Body.String())
	}
	var report PRCheckReport
	if err := json.Unmarshal(rec.Body.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if !report.AllRequiredPass || report.Summary != "passing" || report.ObservedAt.IsZero() {
		t.Fatalf("unexpected report: %+v", report)
	}
	evEntry, err := st.GetMemory(ctx, prChecksNamespace, "shadewright-labs/sablecourt#42")
	if err != nil || evEntry.Author != "gate" || !strings.Contains(evEntry.Value, sha) {
		t.Fatalf("missing timestamped check evidence: entry=%+v err=%v", evEntry, err)
	}

	// 3. Merge without approval must pause with 202 and NOT call MergePR.
	mergePayload := `{"owner":"shadewright-labs","repo":"sablecourt","pr_number":42,"commit_sha":"` + sha + `","merge_method":"squash"}`
	rec = postAction(t, h, http.MethodPost, "/git.merge_pr", token, `{"payload":`+mergePayload+`}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("merge without approval must return 202, got %d: %s", rec.Code, rec.Body.String())
	}
	if backend.mergeCalls != 0 {
		t.Fatalf("MergePR called without approval: %d", backend.mergeCalls)
	}

	var pending struct {
		ApprovalID    string `json:"approval_id"`
		PayloadSHA256 string `json:"payload_sha256"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &pending)
	if _, err := queue.Approve(ctx, pending.ApprovalID, pending.PayloadSHA256, "operator"); err != nil {
		t.Fatalf("Approve: %v", err)
	}

	// 4. Merge with out-of-band approval succeeds and pins commit SHA.
	rec = postAction(t, h, http.MethodPost, "/git.merge_pr", token,
		`{"approvalId":"`+pending.ApprovalID+`","payload":`+mergePayload+`}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("merge with approval: %d %s", rec.Code, rec.Body.String())
	}
	if backend.mergeCalls != 1 || backend.lastMerge.CommitSHA != sha || backend.lastMerge.MergeMethod != "squash" {
		t.Fatalf("unexpected merge call: calls=%d req=%+v", backend.mergeCalls, backend.lastMerge)
	}

	// 5. Reusing the consumed merge approval must fail closed.
	rec = postAction(t, h, http.MethodPost, "/git.merge_pr", token,
		`{"approvalId":"`+pending.ApprovalID+`","payload":`+mergePayload+`}`)
	if rec.Code != http.StatusForbidden || backend.mergeCalls != 1 {
		t.Fatalf("reused merge approval: code=%d calls=%d body=%s", rec.Code, backend.mergeCalls, rec.Body.String())
	}
}

func passingReport(sha string) PRCheckReport {
	return PRCheckReport{
		HeadSHA:        sha,
		HeadBranch:     "sablecourt/task-pr-1",
		BaseBranch:     "main",
		PRState:        "open",
		MergeableState: "clean",
		RequiredChecks: []string{"ci-ok"},
		Checks:         []CheckRunState{{Name: "ci-ok", Status: "completed", Conclusion: "success"}},
	}
}

func openTestPR(t *testing.T, h http.Handler, token string) {
	t.Helper()
	rec := postAction(t, h, http.MethodPost, "/git.open_pr", token,
		`{"payload":{"owner":"shadewright-labs","repo":"sablecourt","head":"sablecourt/task-pr-1","base":"main","title":"Fix"}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("open_pr: %d %s", rec.Code, rec.Body.String())
	}
}

// Approvals expire after 15 minutes, so git.merge_pr asks for one only once
// the checks have passed: while they run, the request is refused and no
// approval is created.
func TestMergePRRequestsApprovalOnlyAfterChecksPass(t *testing.T) {
	sha := strings.Repeat("a1", 20)
	backend := &fakePRBackend{openNumber: 42, report: passingReport(sha)}
	backend.report.Checks[0] = CheckRunState{Name: "ci-ok", Status: "in_progress"}
	gate, st, _, token := setupPRGate(t, backend, domain.EffectApprove, []string{"shadewright-labs/sablecourt"})
	h := gate.ActionHandler()
	openTestPR(t, h, token)

	payload := `{"payload":{"owner":"shadewright-labs","repo":"sablecourt","pr_number":42,"commit_sha":"` + sha + `"}}`
	rec := postAction(t, h, http.MethodPost, "/git.merge_pr", token, payload)
	if rec.Code != http.StatusConflict || !strings.Contains(errorText(t, rec), "pending") {
		t.Fatalf("merge while checks run: %d %s", rec.Code, rec.Body.String())
	}
	pendingApprovals, err := st.ListApprovals(context.Background(), domain.ApprovalPending)
	if err != nil {
		t.Fatal(err)
	}
	if len(pendingApprovals) != 0 {
		t.Fatalf("an approval was requested before the checks passed: %+v", pendingApprovals)
	}

	backend.report.Checks[0] = CheckRunState{Name: "ci-ok", Status: "completed", Conclusion: "success"}
	rec = postAction(t, h, http.MethodPost, "/git.merge_pr", token, payload)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("merge after checks passed: want 202, got %d %s", rec.Code, rec.Body.String())
	}
}

// A check result newer than the approval means the operator approved results
// that are no longer the latest: the merge is refused and needs a new approval.
func TestMergePRRefusesApprovalOlderThanLastCheckResult(t *testing.T) {
	ctx := context.Background()
	sha := strings.Repeat("a1", 20)
	backend := &fakePRBackend{openNumber: 42, report: passingReport(sha)}
	gate, st, queue, token := setupPRGate(t, backend, domain.EffectApprove, []string{"shadewright-labs/sablecourt"})
	h := gate.ActionHandler()
	openTestPR(t, h, token)

	payload := `{"owner":"shadewright-labs","repo":"sablecourt","pr_number":42,"commit_sha":"` + sha + `"}`
	rec := postAction(t, h, http.MethodPost, "/git.merge_pr", token, `{"payload":`+payload+`}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("request approval: %d %s", rec.Code, rec.Body.String())
	}
	var pending struct {
		ApprovalID    string `json:"approval_id"`
		PayloadSHA256 string `json:"payload_sha256"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &pending)
	if _, err := queue.Approve(ctx, pending.ApprovalID, pending.PayloadSHA256, "operator"); err != nil {
		t.Fatal(err)
	}
	apr, err := st.GetApproval(ctx, pending.ApprovalID)
	if err != nil {
		t.Fatal(err)
	}

	// The check runs again after the approval was requested and passes again.
	backend.report.Checks[0].CompletedAt = apr.CreatedAt.Add(time.Second)
	rec = postAction(t, h, http.MethodPost, "/git.merge_pr", token, `{"approvalId":"`+pending.ApprovalID+`","payload":`+payload+`}`)
	if rec.Code != http.StatusConflict || !strings.Contains(errorText(t, rec), "before the latest check result") {
		t.Fatalf("approval older than the last check result: want 409, got %d %s", rec.Code, rec.Body.String())
	}
	if backend.mergeCalls != 0 {
		t.Fatalf("MergePR called %d times", backend.mergeCalls)
	}
}

// A passing check without a completion time is missing evidence: the age of
// the approval cannot be compared with it, so the merge is refused.
func TestMergePRRefusesChecksWithoutResultTime(t *testing.T) {
	sha := strings.Repeat("a1", 20)
	backend := &fakePRBackend{openNumber: 42, report: passingReport(sha), noResultTimes: true}
	gate, _, _, token := setupPRGate(t, backend, domain.EffectApprove, []string{"shadewright-labs/sablecourt"})
	h := gate.ActionHandler()
	openTestPR(t, h, token)
	rec := postAction(t, h, http.MethodPost, "/git.merge_pr", token,
		`{"payload":{"owner":"shadewright-labs","repo":"sablecourt","pr_number":42,"commit_sha":"`+sha+`"}}`)
	if rec.Code != http.StatusConflict || !strings.Contains(errorText(t, rec), "no completion time") {
		t.Fatalf("checks without result time: want 409, got %d %s", rec.Code, rec.Body.String())
	}
}

func TestPRChecksAndMergeRefusalPaths(t *testing.T) {
	ctx := context.Background()
	sha := strings.Repeat("b2", 20)
	otherSHA := strings.Repeat("c3", 20)

	cases := []struct {
		name      string
		openFirst bool
		report    PRCheckReport
		path      string
		body      string
		approve   bool
		repos     []string
		wantCode  int
		wantErr   string
	}{
		{
			name:      "pr_checks refuses PR not opened by Sablecourt",
			openFirst: false,
			path:      "/git.pr_checks",
			body:      `{"payload":{"owner":"shadewright-labs","repo":"sablecourt","pr_number":99}}`,
			wantCode:  http.StatusForbidden,
			wantErr:   "not opened by Sablecourt",
		},
		{
			name:      "merge_pr refuses PR not opened by Sablecourt",
			openFirst: false,
			path:      "/git.merge_pr",
			body:      `{"payload":{"owner":"shadewright-labs","repo":"sablecourt","pr_number":99,"commit_sha":"` + sha + `"}}`,
			wantCode:  http.StatusForbidden,
			wantErr:   "not opened by Sablecourt",
		},
		{
			name:      "merge_pr refuses failing required checks",
			openFirst: true,
			report: PRCheckReport{
				HeadSHA:        sha,
				HeadBranch:     "sablecourt/task-pr-1",
				BaseBranch:     "main",
				PRState:        "open",
				MergeableState: "clean",
				RequiredChecks: []string{"ci-ok"},
				Checks:         []CheckRunState{{Name: "ci-ok", Status: "completed", Conclusion: "failure"}},
			},
			path:     "/git.merge_pr",
			body:     `{"payload":{"owner":"shadewright-labs","repo":"sablecourt","pr_number":42,"commit_sha":"` + sha + `"}}`,
			wantCode: http.StatusConflict,
			wantErr:  "failing",
		},
		{
			name:      "merge_pr refuses pending required checks",
			openFirst: true,
			report: PRCheckReport{
				HeadSHA:        sha,
				HeadBranch:     "sablecourt/task-pr-1",
				BaseBranch:     "main",
				PRState:        "open",
				MergeableState: "clean",
				RequiredChecks: []string{"ci-ok"},
				Checks:         []CheckRunState{{Name: "ci-ok", Status: "in_progress", Conclusion: ""}},
			},
			path:     "/git.merge_pr",
			body:     `{"payload":{"owner":"shadewright-labs","repo":"sablecourt","pr_number":42,"commit_sha":"` + sha + `"}}`,
			wantCode: http.StatusConflict,
			wantErr:  "pending",
		},
		{
			name:      "merge_pr refuses missing required check",
			openFirst: true,
			report: PRCheckReport{
				HeadSHA:        sha,
				HeadBranch:     "sablecourt/task-pr-1",
				BaseBranch:     "main",
				PRState:        "open",
				MergeableState: "clean",
				RequiredChecks: []string{"ci-ok"},
				Checks:         []CheckRunState{{Name: "other", Status: "completed", Conclusion: "success"}},
			},
			path:     "/git.merge_pr",
			body:     `{"payload":{"owner":"shadewright-labs","repo":"sablecourt","pr_number":42,"commit_sha":"` + sha + `"}}`,
			wantCode: http.StatusConflict,
			wantErr:  "missing_required",
		},
		{
			name:      "merge_pr refuses zero checks",
			openFirst: true,
			report: PRCheckReport{
				HeadSHA:        sha,
				HeadBranch:     "sablecourt/task-pr-1",
				BaseBranch:     "main",
				PRState:        "open",
				MergeableState: "clean",
			},
			path:     "/git.merge_pr",
			body:     `{"payload":{"owner":"shadewright-labs","repo":"sablecourt","pr_number":42,"commit_sha":"` + sha + `"}}`,
			wantCode: http.StatusConflict,
			wantErr:  "pending",
		},
		{
			name:      "merge_pr refuses blocked branch rules",
			openFirst: true,
			report: PRCheckReport{
				HeadSHA:        sha,
				HeadBranch:     "sablecourt/task-pr-1",
				BaseBranch:     "main",
				PRState:        "open",
				MergeableState: "blocked",
				RequiredChecks: []string{"ci-ok"},
				Checks:         []CheckRunState{{Name: "ci-ok", Status: "completed", Conclusion: "success"}},
			},
			path:     "/git.merge_pr",
			body:     `{"payload":{"owner":"shadewright-labs","repo":"sablecourt","pr_number":42,"commit_sha":"` + sha + `"}}`,
			wantCode: http.StatusConflict,
			wantErr:  "blocked_by_branch_rules",
		},
		{
			name:      "merge_pr refuses head commit mismatch",
			openFirst: true,
			report: PRCheckReport{
				HeadSHA:        otherSHA,
				HeadBranch:     "sablecourt/task-pr-1",
				BaseBranch:     "main",
				PRState:        "open",
				MergeableState: "clean",
				RequiredChecks: []string{"ci-ok"},
				Checks:         []CheckRunState{{Name: "ci-ok", Status: "completed", Conclusion: "success"}},
			},
			path:     "/git.merge_pr",
			body:     `{"payload":{"owner":"shadewright-labs","repo":"sablecourt","pr_number":42,"commit_sha":"` + sha + `"}}`,
			wantCode: http.StatusConflict,
			wantErr:  "does not match the approved commit_sha",
		},
		{
			name:      "merge_pr refuses unknown bypass field",
			openFirst: true,
			path:      "/git.merge_pr",
			body:      `{"payload":{"owner":"shadewright-labs","repo":"sablecourt","pr_number":42,"commit_sha":"` + sha + `","bypass":true}}`,
			wantCode:  http.StatusBadRequest,
			wantErr:   "invalid action payload",
		},
		{
			name:      "merge_pr refuses a repository other than the pinned one",
			openFirst: false,
			path:      "/git.merge_pr",
			body:      `{"payload":{"owner":"other-org","repo":"other-repo","pr_number":42,"commit_sha":"` + sha + `"}}`,
			wantCode:  http.StatusForbidden,
			wantErr:   "not the configured repository",
		},
		{
			name:      "merge_pr refuses the pinned repository when grants.repos leaves it out",
			openFirst: false,
			repos:     []string{"other-org/other-repo"},
			path:      "/git.merge_pr",
			body:      `{"payload":{"owner":"shadewright-labs","repo":"sablecourt","pr_number":42,"commit_sha":"` + sha + `"}}`,
			wantCode:  http.StatusForbidden,
			wantErr:   "not granted",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			backend := &fakePRBackend{openNumber: 42, report: tc.report}
			repos := tc.repos
			if repos == nil {
				repos = []string{"shadewright-labs/sablecourt"}
			}
			gate, _, _, token := setupPRGate(t, backend, domain.EffectApprove, repos)
			h := gate.ActionHandler()
			if tc.openFirst {
				rec := postAction(t, h, http.MethodPost, "/git.open_pr", token,
					`{"payload":{"owner":"shadewright-labs","repo":"sablecourt","head":"sablecourt/task-pr-1","base":"main","title":"Fix"}}`)
				if rec.Code != http.StatusOK {
					t.Fatalf("open_pr setup failed: %d %s", rec.Code, rec.Body.String())
				}
			}
			rec := postAction(t, h, http.MethodPost, tc.path, token, tc.body)
			if rec.Code != tc.wantCode {
				t.Fatalf("code = %d, want %d: %s", rec.Code, tc.wantCode, rec.Body.String())
			}
			if !strings.Contains(errorText(t, rec), tc.wantErr) {
				t.Fatalf("error = %q, want substring %q", errorText(t, rec), tc.wantErr)
			}
			if backend.mergeCalls != 0 {
				t.Fatalf("MergePR must not be called on refused path (calls=%d)", backend.mergeCalls)
			}
		})
	}

	// Verify live re-check catches a new commit pushed after approval.
	t.Run("commit changed between approval and merge execution fails closed", func(t *testing.T) {
		backend := &fakePRBackend{
			openNumber: 42,
			report: PRCheckReport{
				HeadSHA:        sha,
				HeadBranch:     "sablecourt/task-pr-1",
				BaseBranch:     "main",
				PRState:        "open",
				MergeableState: "clean",
				RequiredChecks: []string{"ci-ok"},
				Checks:         []CheckRunState{{Name: "ci-ok", Status: "completed", Conclusion: "success"}},
			},
		}
		gate, _, queue, token := setupPRGate(t, backend, domain.EffectApprove, []string{"shadewright-labs/sablecourt"})
		h := gate.ActionHandler()
		_ = postAction(t, h, http.MethodPost, "/git.open_pr", token,
			`{"payload":{"owner":"shadewright-labs","repo":"sablecourt","head":"sablecourt/task-pr-1","base":"main","title":"Fix"}}`)
		payload := `{"owner":"shadewright-labs","repo":"sablecourt","pr_number":42,"commit_sha":"` + sha + `"}`
		rec := postAction(t, h, http.MethodPost, "/git.merge_pr", token, `{"payload":`+payload+`}`)
		if rec.Code != http.StatusAccepted {
			t.Fatalf("expected 202, got %d: %s", rec.Code, rec.Body.String())
		}
		var pending struct {
			ApprovalID    string `json:"approval_id"`
			PayloadSHA256 string `json:"payload_sha256"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &pending)
		if _, err := queue.Approve(ctx, pending.ApprovalID, pending.PayloadSHA256, "operator"); err != nil {
			t.Fatal(err)
		}
		// Head moves to an unreviewed commit before execution.
		backend.report.HeadSHA = otherSHA
		rec = postAction(t, h, http.MethodPost, "/git.merge_pr", token, `{"approvalId":"`+pending.ApprovalID+`","payload":`+payload+`}`)
		if rec.Code != http.StatusConflict || backend.mergeCalls != 0 {
			t.Fatalf("expected 409 with zero merge calls, got code=%d calls=%d body=%s", rec.Code, backend.mergeCalls, rec.Body.String())
		}
	})
}

func TestGitHubPROpenerCheckAndMergeHTTPClient(t *testing.T) {
	sha := strings.Repeat("d4", 20)
	var gotMergeBody map[string]any
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok-test" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/org/app/pulls/7":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"state":           "open",
				"draft":           false,
				"mergeable_state": "clean",
				"head":            map[string]string{"sha": sha, "ref": "sablecourt/t7"},
				"base":            map[string]string{"ref": "main"},
			})
		case r.Method == http.MethodGet && r.URL.Path == "/repos/org/app/commits/"+sha+"/check-runs":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"total_count": 1,
				"check_runs": []map[string]string{
					{"name": "ci-ok", "status": "completed", "conclusion": "success", "completed_at": "2026-11-27T13:00:00Z"},
				},
			})
		case r.Method == http.MethodGet && r.URL.Path == "/repos/org/app/commits/"+sha+"/status":
			_ = json.NewEncoder(w).Encode(map[string]any{"total_count": 0, "statuses": []any{}})
		case r.Method == http.MethodGet && r.URL.Path == "/repos/org/app/branches/main/protection/required_status_checks":
			_ = json.NewEncoder(w).Encode(map[string]any{"contexts": []string{"ci-ok"}})
		case r.Method == http.MethodPut && r.URL.Path == "/repos/org/app/pulls/7/merge":
			_ = json.NewDecoder(r.Body).Decode(&gotMergeBody)
			_ = json.NewEncoder(w).Encode(map[string]any{"merged": true, "sha": strings.Repeat("e5", 20)})
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
	report, err := gh.CheckPR(context.Background(), PRChecksInput{Owner: "org", Repo: "app", PRNumber: 7})
	if err != nil {
		t.Fatalf("CheckPR: %v", err)
	}
	evaluatePRChecks(&report, time.Now().UTC())
	if !report.AllRequiredPass || report.HeadSHA != sha || !report.LastResultAt.Equal(time.Date(2026, 11, 27, 13, 0, 0, 0, time.UTC)) {
		t.Fatalf("report = %+v", report)
	}

	res, err := gh.MergePR(context.Background(), MergePRRequest{Owner: "org", Repo: "app", PRNumber: 7, CommitSHA: sha, MergeMethod: "squash"})
	if err != nil {
		t.Fatalf("MergePR: %v", err)
	}
	if res.Status != "merged" || gotMergeBody["sha"] != sha || gotMergeBody["merge_method"] != "squash" || len(gotMergeBody) != 2 {
		t.Fatalf("unexpected merge body=%+v res=%+v", gotMergeBody, res)
	}
}
