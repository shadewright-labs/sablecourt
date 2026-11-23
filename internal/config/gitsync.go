package config

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
)

// GitSyncer keeps a local checkout of a Git-tracked manifests repository in sync
// and reports when HEAD advances so sablecourtd can hot-reload manifests.
type GitSyncer struct {
	RepoURL  string
	Branch   string
	LocalDir string

	mu      sync.Mutex
	lastSHA string
}

func NewGitSyncer(repoURL, branch, localDir string) *GitSyncer {
	if strings.TrimSpace(branch) == "" {
		branch = "main"
	}
	return &GitSyncer{
		RepoURL:  strings.TrimSpace(repoURL),
		Branch:   strings.TrimSpace(branch),
		LocalDir: strings.TrimSpace(localDir),
	}
}

// SyncOnce clones the repository if missing or fast-forwards it if present.
// Returns (changed, headSHA, err).
func (g *GitSyncer) SyncOnce(ctx context.Context) (bool, string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()

	if g.RepoURL == "" || g.LocalDir == "" {
		return false, "", fmt.Errorf("git sync requires repo URL and local directory")
	}

	gitDir := filepath.Join(g.LocalDir, ".git")
	if _, err := os.Stat(gitDir); os.IsNotExist(err) {
		if err := os.MkdirAll(filepath.Dir(g.LocalDir), 0o700); err != nil {
			return false, "", fmt.Errorf("mkdir git sync parent: %w", err)
		}
		cmd := exec.CommandContext(ctx, "git", "clone", "--depth=1", "--branch", g.Branch, "--", g.RepoURL, g.LocalDir)
		if out, err := cmd.CombinedOutput(); err != nil {
			return false, "", fmt.Errorf("git clone %q: %s (%w)", g.RepoURL, strings.TrimSpace(string(out)), err)
		}
	} else {
		fetch := exec.CommandContext(ctx, "git", "-C", g.LocalDir, "fetch", "--depth=1", "origin", g.Branch)
		if out, err := fetch.CombinedOutput(); err != nil {
			return false, "", fmt.Errorf("git fetch: %s (%w)", strings.TrimSpace(string(out)), err)
		}
		reset := exec.CommandContext(ctx, "git", "-C", g.LocalDir, "reset", "--hard", "FETCH_HEAD")
		if out, err := reset.CombinedOutput(); err != nil {
			return false, "", fmt.Errorf("git reset: %s (%w)", strings.TrimSpace(string(out)), err)
		}
	}

	rev := exec.CommandContext(ctx, "git", "-C", g.LocalDir, "rev-parse", "HEAD")
	out, err := rev.Output()
	if err != nil {
		return false, "", fmt.Errorf("git rev-parse HEAD: %w", err)
	}
	sha := strings.TrimSpace(string(out))
	changed := sha != "" && sha != g.lastSHA
	g.lastSHA = sha
	return changed, sha, nil
}
