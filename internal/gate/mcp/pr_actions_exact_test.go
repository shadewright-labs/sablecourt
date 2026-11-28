package mcpgate

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/shadewright-labs/sablecourt/internal/domain"
	"github.com/shadewright-labs/sablecourt/internal/redact"
	"github.com/shadewright-labs/sablecourt/internal/secret"
)

// The git.push, git.pr_checks and git.merge_pr paths load github_token and
// refuse one too short to redact before any request or push.
func TestShortGitHubTokenRefusedOnPushChecksAndMerge(t *testing.T) {
	ctx := context.Background()
	const short = "short12"
	noCall := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		t.Error("request sent with a refused credential")
		return nil, errors.New("unreachable")
	})}
	secrets := secret.NewMemoryStore(map[string]string{"github_token": short})
	gh := &GitHubPROpener{Secrets: secrets, Client: noCall}
	var shortErr *redact.ShortSecretError

	cases := []struct {
		name string
		run  func() error
	}{
		{"pr_checks", func() error {
			_, err := gh.CheckPR(ctx, PRChecksInput{Owner: "acme", Repo: "widgets", PRNumber: 1})
			return err
		}},
		{"merge_pr", func() error {
			_, err := gh.MergePR(ctx, MergePRRequest{Owner: "acme", Repo: "widgets", PRNumber: 1, CommitSHA: strings.Repeat("a", 40), MergeMethod: "squash"})
			return err
		}},
		{"find_open_pr", func() error {
			_, err := gh.FindOpenPR(ctx, OpenPRInput{Owner: "acme", Repo: "widgets", Head: "x", Base: "main", Title: "t"})
			return err
		}},
		{"push", func() error {
			branch := "sablecourt/task-short-1"
			repoDir, approvedDir, commitSHA := initGitRepoWithBareRemote(t, branch)
			p := &GitBranchPusher{Secrets: secrets}
			_, err := p.PushBranch(ctx, PushBranchRequest{RepoDir: repoDir, Remote: "origin", URL: approvedDir, Branch: branch, Commit: commitSHA})
			if remoteHasBranch(approvedDir, branch) {
				t.Error("push ran with a refused credential")
			}
			return err
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.run(); !errors.As(err, &shortErr) {
				t.Fatalf("%s with a short token = %v, want ShortSecretError", tc.name, err)
			}
		})
	}
}

// Strings GitHub returns in a check report reach the stored evidence, the
// audit record and the response only after exact redaction.
func TestPRChecksEvidenceRedactsHeldValues(t *testing.T) {
	ctx := context.Background()
	held := "prcheck_held_" + strings.Repeat("q7", 12)
	redact.Register(held)
	sha := strings.Repeat("b2", 20)

	cases := []struct {
		name   string
		mutate func(*PRCheckReport)
	}{
		{"head_branch", func(r *PRCheckReport) { r.HeadBranch = "feature/" + held }},
		{"base_branch", func(r *PRCheckReport) { r.BaseBranch = held }},
		{"mergeable_state", func(r *PRCheckReport) { r.MergeableState = held }},
		{"required_check", func(r *PRCheckReport) { r.RequiredChecks = []string{"ci-ok", "x " + held} }},
		{"required_check_app", func(r *PRCheckReport) {
			r.RequiredCheckApps = []RequiredCheckApp{{Name: held, AppID: 7}}
		}},
		{"check_status", func(r *PRCheckReport) {
			r.Checks = append(r.Checks, CheckRunState{Name: "lint", Status: held})
		}},
		{"check_conclusion", func(r *PRCheckReport) {
			r.Checks = append(r.Checks, CheckRunState{Name: "lint", Status: "completed", Conclusion: held})
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rep := PRCheckReport{
				HeadSHA: sha, HeadBranch: "sablecourt/task-pr-1", BaseBranch: "main",
				PRState: "open", MergeableState: "clean", RequiredChecks: []string{"ci-ok"},
				Checks: []CheckRunState{{Name: "ci-ok", Status: "completed", Conclusion: "success"}},
			}
			tc.mutate(&rep)
			backend := &fakePRBackend{openNumber: 42, report: rep}
			gate, st, _, token := setupPRGate(t, backend, domain.EffectAllow, []string{"shadewright-labs/sablecourt"})
			h := gate.ActionHandler()
			if rec := postAction(t, h, http.MethodPost, "/git.open_pr", token,
				`{"payload":{"owner":"shadewright-labs","repo":"sablecourt","head":"sablecourt/task-pr-1","base":"main","title":"Fix gate"}}`); rec.Code != http.StatusOK {
				t.Fatalf("open_pr: %d %s", rec.Code, rec.Body.String())
			}
			rec := postAction(t, h, http.MethodPost, "/git.pr_checks", token,
				`{"payload":{"owner":"shadewright-labs","repo":"sablecourt","pr_number":42}}`)
			if strings.Contains(rec.Body.String(), held) {
				t.Fatalf("pr_checks response showed a held value: %s", rec.Body.String())
			}
			entry, err := st.GetMemory(ctx, prChecksNamespace, "shadewright-labs/sablecourt#42")
			if err != nil {
				t.Fatalf("evidence not stored: %v", err)
			}
			if redact.DefaultRegistry().HeldInJSON(entry.Value) {
				t.Fatalf("stored evidence shows a held value: %s", entry.Value)
			}
			recs, err := st.ListAudit(ctx, 100)
			if err != nil {
				t.Fatal(err)
			}
			for _, a := range recs {
				if strings.Contains(a.Outcome, held) || redact.DefaultRegistry().HeldInJSON(a.DetailJSON) {
					t.Fatalf("audit record %q shows a held value", a.Action)
				}
			}
		})
	}
}
