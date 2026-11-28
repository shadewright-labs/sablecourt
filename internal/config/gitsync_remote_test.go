package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestGitSyncerRefusesPlainTextRemotes checks the request-time half of the
// outbound URL rule: SyncOnce runs no git command for a remote that would
// fetch manifests in plain text off the machine, and its error never carries
// the remote's credentials.
func TestGitSyncerRefusesPlainTextRemotes(t *testing.T) {
	cred := "hunter" + "2secret"
	for _, remote := range []string{
		"http://example.test/org/manifests.git",
		"http://x-access-token:" + cred + "@example.test/org/manifests.git",
		"git://example.test/org/manifests.git",
		"ext::sh -c true",
	} {
		dir := filepath.Join(t.TempDir(), "checkout")
		g := NewGitSyncer(remote, "main", dir)
		_, _, err := g.SyncOnce(t.Context())
		if err == nil || !strings.HasPrefix(err.Error(), "manifests Git remote: ") {
			t.Errorf("SyncOnce(%q) = %v, want a refusal naming the remote setting", remote, err)
			continue
		}
		if strings.Contains(err.Error(), cred) || strings.Contains(err.Error(), "example.test") {
			t.Errorf("SyncOnce(%q) error %q carries the remote", remote, err)
		}
		if _, statErr := os.Stat(dir); !os.IsNotExist(statErr) {
			t.Errorf("SyncOnce(%q) created the checkout: %v", remote, statErr)
		}
	}
}
