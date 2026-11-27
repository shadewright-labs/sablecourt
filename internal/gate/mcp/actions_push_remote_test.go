package mcpgate

import (
	"net/http"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/shadewright-labs/sablecourt/internal/domain"
)

// A remote other than the granted one must not be a push target, even when it
// is configured in the repository: a workload that can edit the repository's
// git config could otherwise point a push at any URL.
func TestGitPushRefusesUngrantedConfiguredRemote(t *testing.T) {
	branch := "sablecourt/task-push-1"
	repoDir, _, commitSHA := initGitRepoWithBareRemote(t, branch)
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
				`{"payload":{"branch":"`+branch+`","commit_sha":"`+commitSHA+`","remote":"fork"}}`)
			if rec.Code != http.StatusForbidden {
				t.Fatalf("push to ungranted remote: got %d, want 403: %s", rec.Code, rec.Body.String())
			}
		})
	}
}
