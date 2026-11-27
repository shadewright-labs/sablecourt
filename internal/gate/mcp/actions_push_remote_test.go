package mcpgate

import (
	"context"
	"encoding/json"
	"net/http"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/shadewright-labs/sablecourt/internal/approval"
	"github.com/shadewright-labs/sablecourt/internal/domain"
)

// A remote other than the granted one must not be a push target, even when it
// is configured in the repository: a workload that can edit the repository's
// git config could otherwise point a push at any URL.
func TestGitPushRefusesUngrantedConfiguredRemote(t *testing.T) {
	branch := "sablecourt/task-push-1"
	repoDir, bareDir, commitSHA := initGitRepoWithBareRemote(t, branch)
	other := filepath.Join(t.TempDir(), "other.git")
	if out, err := exec.Command("git", "init", "--bare", other).CombinedOutput(); err != nil {
		t.Fatalf("git init --bare: %v (%s)", err, out)
	}
	runGit(t, repoDir, "remote", "add", "fork", other)

	cases := []struct {
		name   string
		grants domain.GrantSet
	}{
		{"coding repo only", domain.GrantSet{Actions: []string{"git.push"}}},
		{"path grant", domain.GrantSet{Actions: []string{"git.push"}, Repos: []string{repoDir}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			wl := domain.Workload{
				Metadata: domain.ObjectMeta{Name: "coder", Zone: domain.ZoneRuntime},
				Spec: domain.WorkloadSpec{
					Class:   domain.ClassCodingAgent,
					Runtime: domain.RuntimeConfig{Type: "worktree-cli"},
					Coding:  &domain.CodingConfig{Harness: "claude", RepoPath: repoDir},
					Grants:  tc.grants,
				},
			}
			gate, _, _, token := setupGitPushGate(t, repoDir, wl, domain.EffectApprove, nil)
			rec := postAction(t, gate.ActionHandler(), http.MethodPost, "/git.push", token,
				`{"payload":{"branch":"`+branch+`","commit_sha":"`+commitSHA+`","destination":"`+bareDir+`","remote":"fork"}}`)
			if rec.Code != http.StatusForbidden {
				t.Fatalf("push to ungranted remote: got %d, want 403: %s", rec.Code, rec.Body.String())
			}
		})
	}
}

// userAt joins a user name and a host in test URLs.
const userAt = "@"

// requestAndApprovePush asks for a git.push approval, approves it and returns
// the body of the executing request.
func requestAndApprovePush(t *testing.T, h http.Handler, queue *approval.Queue, token, payload string) string {
	t.Helper()
	rec := postAction(t, h, http.MethodPost, "/git.push", token, `{"payload":`+payload+`}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("request approval: got %d, want 202: %s", rec.Code, rec.Body.String())
	}
	var pending struct {
		ApprovalID    string `json:"approval_id"`
		PayloadSHA256 string `json:"payload_sha256"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &pending); err != nil {
		t.Fatal(err)
	}
	if _, err := queue.Approve(context.Background(), pending.ApprovalID, pending.PayloadSHA256, "operator"); err != nil {
		t.Fatalf("Approve: %v", err)
	}
	return `{"approvalId":"` + pending.ApprovalID + `","payload":` + payload + `}`
}

func pushWorkload(repoDir string, repos ...string) domain.Workload {
	return domain.Workload{
		Metadata: domain.ObjectMeta{Name: "coder", Zone: domain.ZoneRuntime},
		Spec: domain.WorkloadSpec{
			Class:   domain.ClassCodingAgent,
			Runtime: domain.RuntimeConfig{Type: "worktree-cli"},
			Coding:  &domain.CodingConfig{Harness: "claude", RepoPath: repoDir},
			Grants:  domain.GrantSet{Actions: []string{"git.push"}, Repos: repos},
		},
	}
}

func remoteHasBranch(dir, branch string) bool {
	return exec.Command("git", "-C", dir, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch).Run() == nil
}

// A local ref named after the approved commit_sha must not change what is
// pushed: git matches a refspec source against ref names before it reads it
// as an object name, so the push must name the object itself.
func TestGitPushSendsApprovedObjectNotRefNamedLikeIt(t *testing.T) {
	branch := "sablecourt/task-push-1"
	for _, prefix := range []string{"refs/heads/", "refs/tags/", "refs/remotes/", "refs/"} {
		t.Run(prefix, func(t *testing.T) {
			repoDir, bareDir, commitSHA := initGitRepoWithBareRemote(t, branch)
			gate, _, queue, token := setupGitPushGate(t, repoDir, pushWorkload(repoDir, bareDir), domain.EffectApprove, nil)
			h := gate.ActionHandler()
			payload := `{"branch":"` + branch + `","commit_sha":"` + commitSHA + `","destination":"` + bareDir + `"}`
			body := requestAndApprovePush(t, h, queue, token, payload)

			evilSHA := runGit(t, repoDir, "commit-tree", "HEAD^{tree}", "-p", "HEAD", "-m", "unreviewed")
			runGit(t, repoDir, "update-ref", prefix+commitSHA, evilSHA)

			rec := postAction(t, h, http.MethodPost, "/git.push", token, body)
			if rec.Code != http.StatusOK {
				t.Fatalf("push of the approved commit refused: %d %s", rec.Code, rec.Body.String())
			}
			if got := runGit(t, bareDir, "rev-parse", "refs/heads/"+branch); got != commitSHA {
				t.Fatalf("pushed %s, approved %s", got, commitSHA)
			}
		})
	}
}

// The URL checked is the one git pushes to: a pushurl that differs from the
// granted URL is refused, before the approval and when the action runs.
func TestGitPushChecksPushURL(t *testing.T) {
	branch := "sablecourt/task-push-1"

	t.Run("pushurl set before the request", func(t *testing.T) {
		repoDir, bareDir, commitSHA := initGitRepoWithBareRemote(t, branch)
		evilDir := filepath.Join(t.TempDir(), "evil.git")
		runGit(t, t.TempDir(), "init", "--bare", evilDir)
		runGit(t, repoDir, "remote", "set-url", "--push", "origin", evilDir)
		gate, _, _, token := setupGitPushGate(t, repoDir, pushWorkload(repoDir, bareDir), domain.EffectApprove, nil)
		rec := postAction(t, gate.ActionHandler(), http.MethodPost, "/git.push", token,
			`{"payload":{"branch":"`+branch+`","commit_sha":"`+commitSHA+`","destination":"`+bareDir+`"}}`)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("push URL not granted: got %d, want 403: %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("pushurl set after the approval", func(t *testing.T) {
		repoDir, bareDir, commitSHA := initGitRepoWithBareRemote(t, branch)
		evilDir := filepath.Join(t.TempDir(), "evil.git")
		runGit(t, t.TempDir(), "init", "--bare", evilDir)
		gate, _, queue, token := setupGitPushGate(t, repoDir, pushWorkload(repoDir, bareDir), domain.EffectApprove, nil)
		h := gate.ActionHandler()
		body := requestAndApprovePush(t, h, queue, token, `{"branch":"`+branch+`","commit_sha":"`+commitSHA+`","destination":"`+bareDir+`"}`)
		runGit(t, repoDir, "remote", "set-url", "--push", "origin", evilDir)
		rec := postAction(t, h, http.MethodPost, "/git.push", token, body)
		if rec.Code != http.StatusForbidden || remoteHasBranch(evilDir, branch) {
			t.Fatalf("push URL changed after approval: got %d, evil has branch=%v: %s", rec.Code, remoteHasBranch(evilDir, branch), rec.Body.String())
		}
	})

	t.Run("GitHub pushurl to another repository", func(t *testing.T) {
		repoDir, bareDir, commitSHA := initGitRepoWithBareRemote(t, branch)
		runGit(t, repoDir, "remote", "set-url", "origin", "https://github.com/shadewright-labs/sablecourt.git")
		runGit(t, repoDir, "remote", "set-url", "--push", "origin", "https://github.com/attacker/exfil.git")
		gate, _, _, token := setupGitPushGate(t, repoDir, pushWorkload(repoDir, "shadewright-labs/sablecourt"), domain.EffectApprove, nil)
		rec := postAction(t, gate.ActionHandler(), http.MethodPost, "/git.push", token,
			`{"payload":{"branch":"`+branch+`","commit_sha":"`+commitSHA+`","destination":"`+bareDir+`"}}`)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("GitHub push URL of another repository: got %d, want 403: %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("origin of a coding repo with no remote grant", func(t *testing.T) {
		repoDir, bareDir, commitSHA := initGitRepoWithBareRemote(t, branch)
		for _, repos := range [][]string{nil, {repoDir}} {
			gate, _, _, token := setupGitPushGate(t, repoDir, pushWorkload(repoDir, repos...), domain.EffectApprove, nil)
			rec := postAction(t, gate.ActionHandler(), http.MethodPost, "/git.push", token,
				`{"payload":{"branch":"`+branch+`","commit_sha":"`+commitSHA+`","destination":"`+bareDir+`"}}`)
			if rec.Code != http.StatusForbidden {
				t.Fatalf("origin not granted (repos %v): got %d, want 403: %s", repos, rec.Code, rec.Body.String())
			}
		}
	})
}

// An owner/repo grant names a GitHub repository: a remote on another host or
// scheme with the same path does not match it.
func TestGitPushOwnerRepoGrantChecksSchemeAndHost(t *testing.T) {
	branch := "sablecourt/task-push-1"
	for _, remoteURL := range []string{
		"https://evil.example/shadewright-labs/sablecourt.git",
		"http://github.com/shadewright-labs/sablecourt.git",
		"git" + userAt + "evil.example:shadewright-labs/sablecourt.git",
		"file:///shadewright-labs/sablecourt.git",
	} {
		for _, remote := range []string{"origin", "fork"} {
			t.Run(remote+" "+remoteURL, func(t *testing.T) {
				repoDir, bareDir, commitSHA := initGitRepoWithBareRemote(t, branch)
				if remote == "origin" {
					runGit(t, repoDir, "remote", "set-url", "origin", remoteURL)
				} else {
					runGit(t, repoDir, "remote", "add", remote, remoteURL)
				}
				gate, _, _, token := setupGitPushGate(t, repoDir, pushWorkload(repoDir, "shadewright-labs/sablecourt"), domain.EffectApprove, nil)
				rec := postAction(t, gate.ActionHandler(), http.MethodPost, "/git.push", token,
					`{"payload":{"branch":"`+branch+`","commit_sha":"`+commitSHA+`","destination":"`+bareDir+`","remote":"`+remote+`"}}`)
				if rec.Code != http.StatusForbidden {
					t.Fatalf("remote %s: got %d, want 403: %s", remoteURL, rec.Code, rec.Body.String())
				}
			})
		}
	}
}

// When the head moves after the approval, the approval is spent before the
// live check refuses the push.
func TestGitPushDriftAfterApprovalSpendsApproval(t *testing.T) {
	ctx := context.Background()
	branch := "sablecourt/task-push-1"
	repoDir, bareDir, commitSHA := initGitRepoWithBareRemote(t, branch)
	gate, st, queue, token := setupGitPushGate(t, repoDir, pushWorkload(repoDir, bareDir), domain.EffectApprove, nil)
	h := gate.ActionHandler()
	body := requestAndApprovePush(t, h, queue, token, `{"branch":"`+branch+`","commit_sha":"`+commitSHA+`","destination":"`+bareDir+`"}`)
	var sent struct {
		ApprovalID string `json:"approvalId"`
	}
	_ = json.Unmarshal([]byte(body), &sent)
	runGit(t, repoDir, "commit", "--allow-empty", "-m", "unreviewed")

	rec := postAction(t, h, http.MethodPost, "/git.push", token, body)
	if rec.Code != http.StatusConflict {
		t.Fatalf("drift after approval: got %d, want 409: %s", rec.Code, rec.Body.String())
	}
	got, err := st.GetApproval(ctx, sent.ApprovalID)
	if err != nil || got.Status != domain.ApprovalConsumed {
		t.Fatalf("approval after a refused run: status %q err %v, want consumed", got.Status, err)
	}
	recs, _ := st.ListAudit(ctx, 100)
	if !hasAuditRecord(recs, "data_action_gate", "action:git.push", "denied", "commit_mismatch") {
		t.Fatal("no denied audit row for the drift refusal")
	}
}

// The token goes only to an https github.com URL, scoped to that one
// repository, never to every github.com URL or to another host.
func TestGitHubPushAuthScopesTokenToRepository(t *testing.T) {
	cases := []struct {
		url, wantURL, wantKey string
	}{
		{"https://github.com/shadewright-labs/sablecourt.git", "https://github.com/shadewright-labs/sablecourt.git", "GIT_CONFIG_KEY_0=http.https://github.com/shadewright-labs/sablecourt.git.extraHeader"},
		{"https://github.com/shadewright-labs/sablecourt", "https://github.com/shadewright-labs/sablecourt.git", "GIT_CONFIG_KEY_0=http.https://github.com/shadewright-labs/sablecourt.git.extraHeader"},
		{"https://user" + userAt + "github.com/shadewright-labs/sablecourt.git", "https://github.com/shadewright-labs/sablecourt.git", "GIT_CONFIG_KEY_0=http.https://github.com/shadewright-labs/sablecourt.git.extraHeader"},
		{"https://evil.example/shadewright-labs/sablecourt.git", "https://evil.example/shadewright-labs/sablecourt.git", ""},
		{"http://github.com/shadewright-labs/sablecourt.git", "http://github.com/shadewright-labs/sablecourt.git", ""},
		{"git@github.com:shadewright-labs/sablecourt.git", "git@github.com:shadewright-labs/sablecourt.git", ""},
		{"/srv/git/sablecourt.git", "/srv/git/sablecourt.git", ""},
	}
	for _, tc := range cases {
		gotURL, env := githubPushAuth(tc.url, "tok-test")
		var key string
		for _, e := range env {
			if strings.HasPrefix(e, "GIT_CONFIG_KEY_0=") {
				key = e
			}
		}
		if gotURL != tc.wantURL || key != tc.wantKey {
			t.Errorf("%s: url %q key %q, want %q %q", tc.url, gotURL, key, tc.wantURL, tc.wantKey)
		}
	}
	if _, env := githubPushAuth("https://github.com/o/r.git", ""); env != nil {
		t.Errorf("empty token produced git configuration %v", env)
	}
}

// The approved value names the push destination. When the push URL changes
// between the approval and the run, even to another granted destination, the
// run refuses with 409 and nothing is pushed anywhere.
func TestGitPushRefusesDestinationChangedAfterApproval(t *testing.T) {
	branch := "sablecourt/task-push-1"
	repoDir, bareDir, commitSHA := initGitRepoWithBareRemote(t, branch)
	otherDir := filepath.Join(t.TempDir(), "other.git")
	runGit(t, t.TempDir(), "init", "--bare", otherDir)
	gate, _, queue, token := setupGitPushGate(t, repoDir, pushWorkload(repoDir, bareDir, otherDir), domain.EffectApprove, nil)
	h := gate.ActionHandler()
	body := requestAndApprovePush(t, h, queue, token, pushPayload(branch, commitSHA, bareDir, ""))

	runGit(t, repoDir, "remote", "set-url", "--push", "origin", otherDir)
	rec := postAction(t, h, http.MethodPost, "/git.push", token, body)
	if rec.Code != http.StatusConflict || !strings.Contains(errorText(t, rec), "destination") {
		t.Fatalf("push URL changed after approval: got %d, want 409: %s", rec.Code, rec.Body.String())
	}
	if remoteHasBranch(otherDir, branch) || remoteHasBranch(bareDir, branch) {
		t.Fatalf("a push happened after the destination changed (other=%v approved=%v)", remoteHasBranch(otherDir, branch), remoteHasBranch(bareDir, branch))
	}
}

// The precheck resolves the destination before an approval exists: a value
// whose destination is not the push URL of the remote is refused with 409 and
// no approval is created.
func TestGitPushPrecheckRefusesOtherDestination(t *testing.T) {
	branch := "sablecourt/task-push-1"
	repoDir, bareDir, commitSHA := initGitRepoWithBareRemote(t, branch)
	otherDir := filepath.Join(t.TempDir(), "other.git")
	runGit(t, t.TempDir(), "init", "--bare", otherDir)
	gate, _, _, token := setupGitPushGate(t, repoDir, pushWorkload(repoDir, bareDir, otherDir), domain.EffectApprove, nil)
	rec := postAction(t, gate.ActionHandler(), http.MethodPost, "/git.push", token, `{"payload":`+pushPayload(branch, commitSHA, otherDir, "")+`}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("destination other than the push URL: got %d, want 409: %s", rec.Code, rec.Body.String())
	}
}

// A destination that carries user information other than the ssh user git
// is refused before an approval exists, so credentials never reach the
// approved value or the dialog.
func TestGitPushRefusesDestinationWithUserinfo(t *testing.T) {
	branch := "sablecourt/task-push-1"
	repoDir, bareDir, commitSHA := initGitRepoWithBareRemote(t, branch)
	gate, _, _, token := setupGitPushGate(t, repoDir, pushWorkload(repoDir, bareDir), domain.EffectApprove, nil)
	for _, dest := range []string{
		"https://user:pass" + userAt + "github.com/shadewright-labs/sablecourt.git",
		"https://token" + userAt + "github.com/shadewright-labs/sablecourt.git",
		"ssh://token" + userAt + "github.com/shadewright-labs/sablecourt.git",
		"token" + userAt + "github.com:shadewright-labs/sablecourt.git",
	} {
		rec := postAction(t, gate.ActionHandler(), http.MethodPost, "/git.push", token, `{"payload":`+pushPayload(branch, commitSHA, dest, "")+`}`)
		if rec.Code != http.StatusBadRequest || !strings.Contains(errorText(t, rec), "user") {
			t.Errorf("destination %q: got %d, want 400: %s", dest, rec.Code, rec.Body.String())
		}
	}
}

// pushPayload builds a git.push value; remote is left out when empty.
func pushPayload(branch, commitSHA, destination, remote string) string {
	p := `{"branch":"` + branch + `","commit_sha":"` + commitSHA + `","destination":"` + destination + `"`
	if remote != "" {
		p += `,"remote":"` + remote + `"`
	}
	return p + `}`
}
