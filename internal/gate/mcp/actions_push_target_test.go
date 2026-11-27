package mcpgate

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/shadewright-labs/sablecourt/internal/domain"
)

// pw is a fake password, split so no credential-shaped literal is tracked.
var pw = "s3" + "cr3t"

// acceptedPushTargets are the destination forms the gate accepts, each
// returned unchanged.
var acceptedPushTargets = []string{
	"https://github.com/shadewright-labs/sablecourt.git",
	"https://git.example:8443/team/repo.git",
	"ssh://git.example/team/repo.git",
	"ssh://git" + userAt + "git.example:2222/team/repo.git",
	"git" + userAt + "github.com:shadewright-labs/sablecourt.git",
	"git" + userAt + "git.example:/srv/git/repo.git",
	"/srv/git/sablecourt.git",
}

// refusedPushTargets are forms outside the allow-list: other users,
// passwords, helper syntax, other schemes and opaque or odd URLs.
var refusedPushTargets = []string{
	// scp-like with a password or another user
	"user:" + pw + userAt + "git.example:team/repo.git",
	"git:" + pw + userAt + "git.example:team/repo.git",
	"token" + userAt + "github.com:shadewright-labs/sablecourt.git",
	"git" + userAt + ":sablecourt.git",
	"git.example:team/repo.git",
	"git" + userAt + "user" + userAt + "git.example:team/repo.git",
	// remote helpers and opaque URLs
	"https::https://user:" + pw + userAt + "git.example/team/repo.git",
	"https::https://git.example/team/repo.git",
	"ext::sh -c touch% /tmp/x",
	"ext::git-remote-x",
	"fd::3",
	"https:git.example/team/repo.git",
	"ssh:git.example/team/repo.git",
	// URL userinfo other than plain git on ssh
	"https://user:" + pw + userAt + "github.com/shadewright-labs/sablecourt.git",
	"https://token" + userAt + "github.com/shadewright-labs/sablecourt.git",
	"https://git" + userAt + "github.com/shadewright-labs/sablecourt.git",
	"ssh://user" + userAt + "git.example/team/repo.git",
	"ssh://git:" + pw + userAt + "git.example/team/repo.git",
	"ssh://git:" + userAt + "git.example/team/repo.git",
	// other schemes
	"file:///srv/git/sablecourt.git",
	"http://github.com/shadewright-labs/sablecourt.git",
	"git://git.example/team/repo.git",
	"ftp://git.example/team/repo.git",
	// odd URLs
	"https://git.example",
	"https://git.example/",
	"https:///team/repo.git",
	"https://git.example/team/repo.git?x=1",
	"https://git.example/team/repo.git#frag",
	"https://git.example/team/%72epo.git",
	"https://git.example:x/team/repo.git",
	"https://[::1]/team/repo.git",
	"HTTPS://git.example/team/repo.git",
	"ssh://-oProxyCommand=x/team/repo.git",
	// local forms
	"relative/path.git",
	"./repo.git",
	"/srv/git/a::b.git",
	"/srv/git/a:b.git",
	"",
}

// One function decides for both the destination and the resolved push URL.
func TestCheckPushTargetAllowList(t *testing.T) {
	for _, raw := range acceptedPushTargets {
		if err := checkPushTarget(raw); err != nil {
			t.Errorf("accepted form %q: %v", raw, err)
		}
	}
	for _, raw := range refusedPushTargets {
		err := checkPushTarget(raw)
		if err == nil {
			t.Errorf("refused form %q: accepted, want an error", raw)
			continue
		}
		if strings.Contains(err.Error(), pw) {
			t.Errorf("refused form %q: error text carries the password: %v", raw, err)
		}
	}
}

// A destination outside the allow-list is refused with 400 before an
// approval exists, so no credential reaches the approved value or the dialog.
func TestGitPushRefusesDestinationOutsideAllowList(t *testing.T) {
	branch := "sablecourt/task-push-1"
	repoDir, bareDir, commitSHA := initGitRepoWithBareRemote(t, branch)
	gate, _, _, token := setupGitPushGate(t, repoDir, pushWorkload(repoDir, bareDir), domain.EffectApprove, nil)
	for _, dest := range refusedPushTargets {
		if dest == "" || strings.ContainsAny(dest, " %") {
			continue
		}
		rec := postAction(t, gate.ActionHandler(), http.MethodPost, "/git.push", token, `{"payload":`+pushPayload(branch, commitSHA, dest, "")+`}`)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("destination %q: got %d, want 400: %s", dest, rec.Code, rec.Body.String())
		}
		if strings.Contains(rec.Body.String(), pw) {
			t.Errorf("destination %q: response carries the password: %s", dest, rec.Body.String())
		}
	}
}

// A push URL outside the allow-list is refused with 403 even when a grant
// names it exactly, and the response never quotes it.
func TestGitPushRefusesPushURLOutsideAllowList(t *testing.T) {
	branch := "sablecourt/task-push-1"
	for _, pushURL := range []string{
		"user:" + pw + userAt + "git.example:team/repo.git",
		"https::https://user:" + pw + userAt + "git.example/team/repo.git",
		"ext::git-remote-x",
		"file:///srv/git/sablecourt.git",
		"https://token" + userAt + "github.com/shadewright-labs/sablecourt.git",
		"ssh://user" + userAt + "git.example/team/repo.git",
	} {
		t.Run(pushURL, func(t *testing.T) {
			repoDir, bareDir, commitSHA := initGitRepoWithBareRemote(t, branch)
			runGit(t, repoDir, "remote", "set-url", "--push", "origin", pushURL)
			gate, _, _, token := setupGitPushGate(t, repoDir, pushWorkload(repoDir, bareDir, pushURL), domain.EffectApprove, nil)
			rec := postAction(t, gate.ActionHandler(), http.MethodPost, "/git.push", token, `{"payload":`+pushPayload(branch, commitSHA, bareDir, "")+`}`)
			if rec.Code != http.StatusForbidden {
				t.Errorf("push URL %q: got %d, want 403: %s", pushURL, rec.Code, rec.Body.String())
			}
			if strings.Contains(rec.Body.String(), pw) {
				t.Errorf("push URL %q: response carries the password: %s", pushURL, rec.Body.String())
			}
		})
	}
}

// An accepted push URL with the plain git user is named as it is in the
// destination, and the precheck asks for an approval without a push.
func TestGitPushAcceptsGitUserDestination(t *testing.T) {
	branch := "sablecourt/task-push-1"
	for _, pushURL := range []string{
		"git" + userAt + "git.example:team/repo.git",
		"ssh://git" + userAt + "git.example:2222/team/repo.git",
		"https://git.example/team/repo.git",
	} {
		t.Run(pushURL, func(t *testing.T) {
			repoDir, _, commitSHA := initGitRepoWithBareRemote(t, branch)
			runGit(t, repoDir, "remote", "set-url", "--push", "origin", pushURL)
			gate, _, _, token := setupGitPushGate(t, repoDir, pushWorkload(repoDir, pushURL), domain.EffectApprove, nil)
			rec := postAction(t, gate.ActionHandler(), http.MethodPost, "/git.push", token, `{"payload":`+pushPayload(branch, commitSHA, pushURL, "")+`}`)
			if rec.Code != http.StatusAccepted {
				t.Fatalf("push URL %q: got %d, want 202: %s", pushURL, rec.Code, rec.Body.String())
			}
		})
	}
}

// A system or global gitconfig must not rewrite the checked push URL at push
// time: with a pushInsteadOf rule that sends the approved URL elsewhere, the
// push still lands only at the approved URL.
func TestGitBranchPusherIgnoresSystemAndGlobalConfig(t *testing.T) {
	branch := "sablecourt/task-push-1"
	repoDir, approvedDir, commitSHA := initGitRepoWithBareRemote(t, branch)
	evilDir := filepath.Join(t.TempDir(), "evil.git")
	runGit(t, t.TempDir(), "init", "--bare", evilDir)
	rule := "[url \"" + evilDir + "\"]\n\tpushInsteadOf = " + approvedDir + "\n"
	cfgDir := t.TempDir()
	systemCfg := filepath.Join(cfgDir, "system.gitconfig")
	globalCfg := filepath.Join(cfgDir, "global.gitconfig")
	for _, f := range []string{systemCfg, globalCfg} {
		if err := os.WriteFile(f, []byte(rule), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, extra := range [][]string{
		{"GIT_CONFIG_SYSTEM=" + systemCfg},
		{"GIT_CONFIG_GLOBAL=" + globalCfg},
	} {
		name, _, _ := strings.Cut(extra[0], "=")
		t.Run(name, func(t *testing.T) {
			p := &GitBranchPusher{extraEnv: extra}
			_, err := p.PushBranch(context.Background(), PushBranchRequest{
				RepoDir: repoDir, Remote: "origin", URL: approvedDir, Branch: branch, Commit: commitSHA,
			})
			if err != nil {
				t.Fatalf("push: %v", err)
			}
			if remoteHasBranch(evilDir, branch) {
				t.Fatal("the push was rewritten by a pushInsteadOf rule outside the gate")
			}
			if !remoteHasBranch(approvedDir, branch) {
				t.Fatal("the push did not reach the approved URL")
			}
			runGit(t, approvedDir, "update-ref", "-d", "refs/heads/"+branch)
		})
	}
}

// A SHA-256 task repository pushes to a SHA-256 receiver: the temporary bare
// repository takes the object format of the task repository, so it can read
// the borrowed objects. The SHA-1 case is covered by the tests above.
func TestGitBranchPusherPushesSHA256Repository(t *testing.T) {
	branch := "sablecourt/task-push-1"
	repoDir := t.TempDir()
	runGit(t, repoDir, "init", "--quiet", "--object-format=sha256")
	runGit(t, repoDir, "symbolic-ref", "HEAD", "refs/heads/"+branch)
	if err := os.WriteFile(filepath.Join(repoDir, "change.txt"), []byte("task change\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, repoDir, "add", "change.txt")
	runGit(t, repoDir, "-c", "user.name=Sablecourt Test", "-c", "user.email=test", "commit", "--quiet", "-m", "task work")
	commitSHA := runGit(t, repoDir, "rev-parse", "HEAD")
	if len(commitSHA) != 64 {
		t.Fatalf("commit %q is not a SHA-256 object name", commitSHA)
	}
	bareDir := filepath.Join(t.TempDir(), "remote.git")
	runGit(t, t.TempDir(), "init", "--quiet", "--bare", "--object-format=sha256", bareDir)

	p := &GitBranchPusher{}
	if _, err := p.PushBranch(context.Background(), PushBranchRequest{
		RepoDir: repoDir, Remote: "origin", URL: bareDir, Branch: branch, Commit: commitSHA,
	}); err != nil {
		t.Fatalf("push: %v", err)
	}
	if got := runGit(t, bareDir, "rev-parse", "refs/heads/"+branch); got != commitSHA {
		t.Fatalf("receiver has %q, want %q", got, commitSHA)
	}
}
