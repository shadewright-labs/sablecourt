package config

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestGitSyncerClonesAndDetectsUpdates(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()
	origin := filepath.Join(tmp, "origin.git")
	work := filepath.Join(tmp, "work")
	syncDir := filepath.Join(tmp, "synced")

	run := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.CommandContext(ctx, "git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=test",
			"GIT_AUTHOR_EMAIL=test@example.com",
			"GIT_COMMITTER_NAME=test",
			"GIT_COMMITTER_EMAIL=test@example.com",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v in %s: %v\n%s", args, dir, err, string(out))
		}
	}

	if err := os.MkdirAll(origin, 0o700); err != nil {
		t.Fatal(err)
	}
	run(origin, "init", "--bare", "-b", "main")
	if err := os.MkdirAll(work, 0o700); err != nil {
		t.Fatal(err)
	}
	run(work, "init", "-b", "main")
	run(work, "remote", "add", "origin", origin)
	if err := os.WriteFile(filepath.Join(work, "manifest.yaml"), []byte("v1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	run(work, "add", ".")
	run(work, "commit", "-m", "v1")
	run(work, "push", "origin", "main")

	syncer := NewGitSyncer(origin, "main", syncDir)

	changed, sha1, err := syncer.SyncOnce(ctx)
	if err != nil || !changed || sha1 == "" {
		t.Fatalf("initial SyncOnce: changed=%v sha=%q err=%v", changed, sha1, err)
	}

	changed, sha2, err := syncer.SyncOnce(ctx)
	if err != nil || changed || sha2 != sha1 {
		t.Fatalf("noop SyncOnce: changed=%v sha=%q err=%v", changed, sha2, err)
	}

	if err := os.WriteFile(filepath.Join(work, "manifest.yaml"), []byte("v2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	run(work, "add", ".")
	run(work, "commit", "-m", "v2")
	run(work, "push", "origin", "main")

	changed, sha3, err := syncer.SyncOnce(ctx)
	if err != nil || !changed || sha3 == sha1 {
		t.Fatalf("update SyncOnce: changed=%v sha3=%q sha1=%q err=%v", changed, sha3, sha1, err)
	}
}
