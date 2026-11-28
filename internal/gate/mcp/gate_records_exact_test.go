package mcpgate

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/shadewright-labs/sablecourt/internal/domain"
	"github.com/shadewright-labs/sablecourt/internal/redact"
	"github.com/shadewright-labs/sablecourt/internal/secret"
)

// The daemon wraps its secret store in an ObservingStore, whose Get returns the
// ShortSecretError itself. Every secret read of the GitOps actions and the
// upstream bridge surfaces that error and sends or pushes nothing.
func TestShortSecretThroughObservingStoreRefusedEverywhere(t *testing.T) {
	ctx := context.Background()
	const short = "short12"
	noCall := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		t.Error("request sent with a refused credential")
		return nil, errors.New("unreachable")
	})}
	obs := secret.NewObservingStore(
		secret.NewMemoryStore(map[string]string{"github_token": short, "linear_api_key": short}),
		func(name, val string) error { return redact.RegisterSetting(name, val) },
	)
	gh := &GitHubPROpener{Secrets: obs, Client: noCall}
	pr := OpenPRInput{Owner: "acme", Repo: "widgets", Head: "x", Base: "main", Title: "t"}

	cases := []struct {
		name string
		run  func(t *testing.T) error
	}{
		{"push", func(t *testing.T) error {
			branch := "sablecourt/task-short-obs"
			repoDir, approvedDir, commitSHA := initGitRepoWithBareRemote(t, branch)
			p := &GitBranchPusher{Secrets: obs}
			_, err := p.PushBranch(ctx, PushBranchRequest{RepoDir: repoDir, Remote: "origin", URL: approvedDir, Branch: branch, Commit: commitSHA})
			if remoteHasBranch(approvedDir, branch) {
				t.Error("push ran after the store refused a short token")
			}
			return err
		}},
		{"open_pr", func(*testing.T) error { _, err := gh.OpenPR(ctx, pr); return err }},
		{"find_open_pr", func(*testing.T) error { _, err := gh.FindOpenPR(ctx, pr); return err }},
		{"pr_checks", func(*testing.T) error {
			_, err := gh.CheckPR(ctx, PRChecksInput{Owner: "acme", Repo: "widgets", PRNumber: 1})
			return err
		}},
		{"merge_pr", func(*testing.T) error {
			_, err := gh.MergePR(ctx, MergePRRequest{Owner: "acme", Repo: "widgets", PRNumber: 1, CommitSHA: strings.Repeat("a", 40), MergeMethod: "squash"})
			return err
		}},
		{"upstream", func(*testing.T) error {
			_, err := NewUpstreamBridge(obs, noCall).CallTool(ctx, domain.UpstreamMCPServer{
				Name: "linear", URL: "https://mcp.linear.example/mcp", SecretName: "linear_api_key", AllowedTools: []string{"list_issues"},
			}, "list_issues", json.RawMessage(`{}`))
			return err
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var shortErr *redact.ShortSecretError
			if err := tc.run(t); !errors.As(err, &shortErr) {
				t.Fatalf("%s through ObservingStore = %v, want ShortSecretError", tc.name, err)
			}
		})
	}
}

// A push with no github_token in the store still runs (local remotes), but a
// store error other than not found refuses it.
func TestPushBranchStoreErrorRefuses(t *testing.T) {
	ctx := context.Background()
	branch := "sablecourt/task-store-err"
	repoDir, approvedDir, commitSHA := initGitRepoWithBareRemote(t, branch)
	broken := secret.NewObservingStore(secret.NewMemoryStore(map[string]string{"github_token": "long-enough-token"}),
		func(string, string) error { return errors.New("keychain locked") })
	p := &GitBranchPusher{Secrets: broken}
	if _, err := p.PushBranch(ctx, PushBranchRequest{RepoDir: repoDir, Remote: "origin", URL: approvedDir, Branch: branch, Commit: commitSHA}); err == nil {
		t.Fatal("push ran after a store error")
	}
	if remoteHasBranch(approvedDir, branch) {
		t.Fatal("push ran after a store error")
	}
	p = &GitBranchPusher{Secrets: secret.NewMemoryStore(nil)}
	if _, err := p.PushBranch(ctx, PushBranchRequest{RepoDir: repoDir, Remote: "origin", URL: approvedDir, Branch: branch, Commit: commitSHA}); err != nil {
		t.Fatalf("push without a github_token: %v", err)
	}
}

type urlLeakPROpener struct{ url string }

func (u urlLeakPROpener) OpenPR(context.Context, OpenPRInput) (string, error) {
	return u.url, nil
}

// The html_url GitHub returns for git.open_pr is stored only after redaction.
func TestOpenedPRRecordRedactsHeldValueInGitHubURL(t *testing.T) {
	ctx := context.Background()
	held := "openpr_held_" + strings.Repeat("k9", 10)
	redact.Register(held)

	gate, st, _, token := setupPRGate(t, &fakePRBackend{}, domain.EffectAllow, []string{"shadewright-labs/sablecourt"})
	RegisterBuiltinGitOpsWithOptions(gate, GitOpsOptions{
		RepoRoot: initGitHubRepo(t, "https://github.com/shadewright-labs/sablecourt.git"),
		PROpener: urlLeakPROpener{url: "https://github.com/shadewright-labs/sablecourt/pull/42?x=" + held},
	})

	rec := postAction(t, gate.ActionHandler(), http.MethodPost, "/git.open_pr", token,
		`{"payload":{"owner":"shadewright-labs","repo":"sablecourt","head":"sablecourt/task-pr-1","base":"main","title":"Fix"}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("open_pr: %d %s", rec.Code, rec.Body.String())
	}
	entry, err := st.GetMemory(ctx, openedPRsNamespace, "shadewright-labs/sablecourt#42")
	if err != nil {
		t.Fatalf("GetMemory: %v", err)
	}
	if redact.DefaultRegistry().HeldInJSON(entry.Value) {
		t.Fatalf("stored opened PR record contains held value: %s", entry.Value)
	}
	if _, err := lookupOpenedPR(ctx, st, "shadewright-labs", "sablecourt", 42); err != nil {
		t.Fatalf("redacted record no longer reads back: %v", err)
	}
}

// Every gate record in memory shows no held value, whichever field carries
// it: GitHub fields are redacted in place, and a record that still shows one
// is refused as a whole, so its reader fails closed.
func TestGateRecordsNeverStoreHeldValues(t *testing.T) {
	ctx := context.Background()
	held := "gaterec_held_" + strings.Repeat("z3", 10)
	redact.Register(held)
	gate, st, _, _ := setupPRGate(t, &fakePRBackend{}, domain.EffectAllow, []string{"shadewright-labs/sablecourt"})
	base := OpenedPRRecord{Owner: "acme", Repo: "widgets", Number: 7, URL: "https://github.com/acme/widgets/pull/7",
		Head: "sablecourt/task-7", Base: "main", TaskID: "task-7", Workload: "w"}

	cases := []struct {
		name     string
		mutate   func(*OpenedPRRecord)
		readable bool
	}{
		{"url", func(r *OpenedPRRecord) { r.URL += "#" + held }, true},
		{"adopted_head_sha", func(r *OpenedPRRecord) { r.Adopted, r.HeadSHA = true, held }, true},
		{"head", func(r *OpenedPRRecord) { r.Head = held }, false},
		{"task_id", func(r *OpenedPRRecord) { r.TaskID = held }, false},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := base
			rec.Number = 7 + i
			tc.mutate(&rec)
			if err := recordOpenedPR(ctx, gate, rec); err != nil {
				t.Fatal(err)
			}
			entry, err := st.GetMemory(ctx, openedPRsNamespace, prKey(rec.Owner, rec.Repo, rec.Number))
			if err != nil {
				t.Fatal(err)
			}
			if redact.DefaultRegistry().HeldInJSON(entry.Value) {
				t.Fatalf("stored record shows a held value: %s", entry.Value)
			}
			_, err = lookupOpenedPR(ctx, st, rec.Owner, rec.Repo, rec.Number)
			if tc.readable != (err == nil) {
				t.Fatalf("lookup err = %v, want readable %v", err, tc.readable)
			}
		})
	}

	if err := recordPushedCommit(ctx, gate, "task-push", PushBranchResult{Branch: "x/" + held, Commit: strings.Repeat("c", 40)}); err != nil {
		t.Fatal(err)
	}
	entry, err := st.GetMemory(ctx, pushedCommitsNamespace, "task-push")
	if err != nil {
		t.Fatal(err)
	}
	if redact.DefaultRegistry().HeldInJSON(entry.Value) {
		t.Fatalf("pushed commit record shows a held value: %s", entry.Value)
	}
	if _, _, err := lookupPushedCommit(ctx, st, "task-push"); err == nil {
		t.Fatal("a refused pushed commit record read back without error")
	}
}

// Check evidence is not a typed boundary under ADR-0015: a held value in a
// GitHub string is replaced in place and the rest of the record is kept.
func TestPRChecksEvidenceRedactsInPlace(t *testing.T) {
	ctx := context.Background()
	held := "prcheck_whole_" + strings.Repeat("m4", 10)
	redact.Register(held)
	sha := strings.Repeat("b2", 20)

	rep := passingReport(sha)
	rep.HeadBranch = "feature/" + held
	backend := &fakePRBackend{openNumber: 42, report: rep}
	gate, st, _, token := setupPRGate(t, backend, domain.EffectAllow, []string{"shadewright-labs/sablecourt"})
	h := gate.ActionHandler()
	openTestPR(t, h, token)

	postAction(t, h, http.MethodPost, "/git.pr_checks", token,
		`{"payload":{"owner":"shadewright-labs","repo":"sablecourt","pr_number":42}}`)
	entry, err := st.GetMemory(ctx, prChecksNamespace, "shadewright-labs/sablecourt#42")
	if err != nil {
		t.Fatalf("evidence not stored: %v", err)
	}
	if redact.DefaultRegistry().HeldInJSON(entry.Value) {
		t.Fatalf("stored evidence shows a held value: %s", entry.Value)
	}
	var got PRCheckReport
	if err := json.Unmarshal([]byte(entry.Value), &got); err != nil {
		t.Fatalf("evidence is not a report: %v (%s)", err, entry.Value)
	}
	if got.HeadBranch != "feature/"+redact.KnownSecretMarker || got.HeadSHA != sha {
		t.Fatalf("evidence = %+v, want the branch redacted in place", got)
	}
}

// Fields GitHub reports from a fixed vocabulary are lowercased for comparison.
// A mixed-case held value in any of them must not pass that change into the
// response or the evidence, where exact redaction no longer sees it.
func TestPRChecksMixedCaseHeldValueNotLowercasedPastRedaction(t *testing.T) {
	ctx := context.Background()
	held := "PRCheck_Mixed_" + strings.Repeat("Q7", 10)
	redact.Register(held)
	lower := strings.ToLower(held)
	sha := strings.Repeat("b2", 20)

	cases := []struct {
		name   string
		mutate func(*PRCheckReport)
	}{
		{"pr_state", func(r *PRCheckReport) { r.PRState = held }},
		{"mergeable_state", func(r *PRCheckReport) { r.MergeableState = held }},
		{"check_status", func(r *PRCheckReport) { r.Checks[0].Status = held }},
		{"check_conclusion", func(r *PRCheckReport) { r.Checks[0].Conclusion = held }},
		{"head_sha", func(r *PRCheckReport) { r.HeadSHA = held }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rep := passingReport(sha)
			tc.mutate(&rep)
			backend := &fakePRBackend{openNumber: 42, report: rep}
			gate, st, _, token := setupPRGate(t, backend, domain.EffectAllow, []string{"shadewright-labs/sablecourt"})
			h := gate.ActionHandler()
			openTestPR(t, h, token)

			rec := postAction(t, h, http.MethodPost, "/git.pr_checks", token,
				`{"payload":{"owner":"shadewright-labs","repo":"sablecourt","pr_number":42}}`)
			if strings.Contains(rec.Body.String(), lower) {
				t.Fatalf("pr_checks response leaked the lowercased held value: %s", rec.Body.String())
			}
			// head_sha is not a check result: an invalid one is cleared, and
			// git.merge_pr refuses an empty head against commit_sha.
			if tc.name != "head_sha" && strings.Contains(rec.Body.String(), `"summary":"passing"`) {
				t.Fatalf("a report with an unknown value passed: %s", rec.Body.String())
			}
			entry, err := st.GetMemory(ctx, prChecksNamespace, "shadewright-labs/sablecourt#42")
			if err != nil {
				t.Fatalf("GetMemory: %v", err)
			}
			if strings.Contains(entry.Value, lower) {
				t.Fatalf("stored evidence leaked the lowercased held value: %s", entry.Value)
			}
		})
	}
}
