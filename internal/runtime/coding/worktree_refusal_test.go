package coding

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPrepareWorktreeRefusals(t *testing.T) {
	ctx := context.Background()
	for _, suffix := range []string{"", stateSuffix, ".log", excludeConfigSuffix} {
		t.Run("occupied path "+suffix, func(t *testing.T) {
			a, spec := codingFixture(t)
			a.DryRun = true
			if err := os.MkdirAll(a.WorktreeRoot, 0o700); err != nil {
				t.Fatal(err)
			}
			occupied := filepath.Join(a.WorktreeRoot, sanitizeSegment(spec.TaskID)) + suffix
			if err := os.WriteFile(occupied, []byte("precious"), 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := a.Start(ctx, spec)
			if err == nil || !strings.Contains(err.Error(), "worktree path unavailable") {
				t.Fatalf("Start = %v, want occupied path refusal", err)
			}
			if got, err := os.ReadFile(occupied); err != nil || string(got) != "precious" {
				t.Errorf("occupied path changed: %q, %v", got, err)
			}
			repo := spec.Workload.Spec.Coding.RepoPath
			if branches := testGit(t, repo, "branch", "--list", "sablecourt/*"); branches != "" {
				t.Errorf("refused launch created a branch: %s", branches)
			}
		})
	}
	t.Run("required arguments", func(t *testing.T) {
		if _, _, err := NewAdapter("").prepareWorktree(ctx, "repo", "task"); err == nil {
			t.Error("prepared a worktree without a root")
		}
		a := NewAdapter(t.TempDir())
		if _, _, err := a.prepareWorktree(ctx, "", "task"); err == nil {
			t.Error("prepared a worktree without a repository")
		}
		if _, _, err := a.prepareWorktree(ctx, initTempGitRepo(t), ""); err == nil {
			t.Error("prepared a worktree without a task ID")
		}
	})
	t.Run("source is a linked worktree", func(t *testing.T) {
		repo := initTempGitRepo(t)
		linked := filepath.Join(t.TempDir(), "linked")
		testGit(t, repo, "worktree", "add", "--quiet", "--detach", linked)
		a := NewAdapter(t.TempDir())
		_, _, err := a.prepareWorktree(ctx, linked, "task")
		if err == nil || !strings.Contains(err.Error(), "own .git directory") {
			t.Fatalf("prepareWorktree = %v, want linked worktree refusal", err)
		}
	})
	t.Run("source does not exist", func(t *testing.T) {
		a := NewAdapter(t.TempDir())
		if _, _, err := a.prepareWorktree(ctx, filepath.Join(t.TempDir(), "absent"), "task"); err == nil {
			t.Fatal("prepared a worktree from a missing repository")
		}
	})
}

func TestBoundedLogWriteString(t *testing.T) {
	file, err := os.CreateTemp(t.TempDir(), "log")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = file.Close() })
	var live strings.Builder
	l := &boundedLog{file: file, live: &live}
	write := func(s string) {
		t.Helper()
		if n, err := l.WriteString(s); err != nil || n != len(s) {
			t.Fatalf("WriteString(%d bytes) = %d, %v", len(s), n, err)
		}
	}
	// An incomplete line from one stream is ended before the next record.
	write("partial")
	write("next\n")
	write("full line\n")
	if got := live.String(); got != "partial\nnext\nfull line\n" {
		t.Fatalf("live output = %q", got)
	}
	// Filling the file writes one truncation marker, then drops file output
	// while live output continues.
	write(strings.Repeat("x", maxSessionLog))
	write("after\n")
	data, err := os.ReadFile(file.Name())
	if err != nil {
		t.Fatal(err)
	}
	if want := "partial\nnext\nfull line\n\n...[truncated]\n"; string(data) != want {
		t.Fatalf("file content = %q, want %q", data, want)
	}
	if !strings.HasSuffix(live.String(), "\nafter\n") {
		t.Errorf("live output stopped at the file limit")
	}
}

func TestBoundedLogMarkerDroppedWhenNoRoom(t *testing.T) {
	file, err := os.CreateTemp(t.TempDir(), "log")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = file.Close() })
	l := &boundedLog{file: file, written: maxSessionLog - 4}
	if _, err := l.WriteString("too long\n"); err != nil {
		t.Fatal(err)
	}
	if _, err := l.WriteString("more\n"); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(file.Name()); err != nil || len(data) != 0 {
		t.Fatalf("file content = %q, %v; want nothing past the limit", data, err)
	}
}
