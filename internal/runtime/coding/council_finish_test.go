package coding

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func startWriterTask(t *testing.T, conclave string) (*Adapter, string, string, *worktreeState, func() error) {
	t.Helper()
	a, spec := codingFixture(t)
	a.DryRun = true
	spec.InputJSON = mustMarshalInput(t, map[string]any{
		"checkoutMode": "writer", "checkoutWriter": "claude", "conclaveId": conclave, "role": "claude", "prompt": "edit",
	})
	h := startTestTask(t, a, spec)
	state, err := a.worktreeState(h)
	if err != nil {
		t.Fatal(err)
	}
	root, err := resolvedPath(a.WorktreeRoot)
	if err != nil {
		t.Fatal(err)
	}
	return a, root, spec.Workload.Spec.Coding.RepoPath, state, func() error { return a.Stop(context.Background(), h) }
}

func TestFinishCouncilWriterRefusalIsRetryable(t *testing.T) {
	for _, tc := range []struct {
		name   string
		damage func(t *testing.T, root string, state *worktreeState) func()
		want   string
	}{
		{name: "nested repository in the writer checkout", damage: func(t *testing.T, _ string, state *worktreeState) func() {
			nested := filepath.Join(state.CheckoutDir, "inner")
			testGit(t, state.CheckoutDir, "init", "--quiet", "inner")
			testWrite(t, nested, "draft.txt", "keep", 0o600)
			return func() {
				if err := os.RemoveAll(nested); err != nil {
					t.Fatal(err)
				}
			}
		}, want: "nested Git repository"},
		{name: "corrupt council state", damage: func(t *testing.T, root string, state *worktreeState) func() {
			path := councilStatePath(root, state.CouncilKey)
			saved, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			testWrite(t, filepath.Dir(path), filepath.Base(path), "{", 0o600)
			return func() { testWrite(t, filepath.Dir(path), filepath.Base(path), string(saved), 0o600) }
		}, want: "decode council checkout state"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, root, _, state, stop := startWriterTask(t, "ccl-finish")
			testWrite(t, state.CheckoutDir, "work.txt", "writer output\n", 0o600)
			restore := tc.damage(t, root, state)
			err := stop()
			if err == nil || !strings.Contains(err.Error(), "cleanup incomplete") || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Stop = %v, want incomplete cleanup naming %q", err, tc.want)
			}
			saved, readErr := readWorktreeState(filepath.Join(root, state.TaskID) + stateSuffix)
			if readErr != nil || !strings.Contains(saved.CleanupError, tc.want) {
				t.Fatalf("recorded cleanup error = %+v, %v", saved, readErr)
			}
			if _, err := os.Stat(filepath.Join(state.CheckoutDir, "work.txt")); err != nil {
				t.Errorf("writer output was lost after a refused cleanup: %v", err)
			}
			restore()
			if err := stop(); err != nil {
				t.Fatalf("Stop retry after repair = %v", err)
			}
			cs, err := readCouncilState(councilStatePath(root, state.CouncilKey))
			if err != nil {
				t.Fatal(err)
			}
			if cs.ActiveWriterTaskID != "" || len(cs.ActiveTasks) != 0 || !isCommitID(cs.WriterTip) || cs.WriterTip == cs.BaseCommit {
				t.Errorf("council state after retry: writer %q tasks %v tip %q base %q",
					cs.ActiveWriterTaskID, cs.ActiveTasks, cs.WriterTip, cs.BaseCommit)
			}
			if got := testGit(t, cs.Repo, "show", cs.WriterTip+":work.txt"); got != "writer output\n" {
				t.Errorf("writer tip content = %q", got)
			}
			if _, err := os.Stat(writerLockPath(root, state.CouncilKey)); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("writer lock remains after cleanup: %v", err)
			}
		})
	}
}

func TestCleanupCouncilRefusals(t *testing.T) {
	ctx := context.Background()
	t.Run("missing conclave ID", func(t *testing.T) {
		if err := NewAdapter(t.TempDir()).CleanupCouncil(ctx, " "); err == nil {
			t.Fatal("cleaned up a blank conclave")
		}
		if err := NewAdapter("").CleanupCouncil(ctx, "ccl"); err == nil {
			t.Fatal("cleaned up without a worktree root")
		}
	})
	t.Run("unknown council", func(t *testing.T) {
		if err := NewAdapter(t.TempDir()).CleanupCouncil(ctx, "ccl-unknown"); err != nil {
			t.Fatalf("cleanup of an unknown council = %v", err)
		}
	})
	t.Run("active writer", func(t *testing.T) {
		a, root, repo, state, stop := startWriterTask(t, "ccl-active")
		err := a.CleanupCouncil(ctx, "ccl-active")
		if err == nil || !strings.Contains(err.Error(), "still has active tasks") {
			t.Fatalf("CleanupCouncil = %v, want active task refusal", err)
		}
		if _, err := os.Stat(state.CheckoutDir); err != nil {
			t.Errorf("active writer checkout was removed: %v", err)
		}
		if err := stop(); err != nil {
			t.Fatal(err)
		}
		if err := a.CleanupCouncil(ctx, "ccl-active"); err != nil {
			t.Fatalf("CleanupCouncil after stop = %v", err)
		}
		if _, err := os.Stat(councilStatePath(root, state.CouncilKey)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("council state remains: %v", err)
		}
		if _, err := os.Stat(state.CheckoutDir); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("writer checkout remains: %v", err)
		}
		if list := testGit(t, repo, "worktree", "list", "--porcelain"); strings.Count(list, "worktree ") != 1 {
			t.Errorf("council worktree still registered:\n%s", list)
		}
		// A second cleanup is a no-op.
		if err := a.CleanupCouncil(ctx, "ccl-active"); err != nil {
			t.Fatalf("repeated CleanupCouncil = %v", err)
		}
	})
	t.Run("corrupt council state", func(t *testing.T) {
		root := t.TempDir()
		a := NewAdapter(root)
		resolved, err := resolvedPath(root)
		if err != nil {
			t.Fatal(err)
		}
		path := councilStatePath(resolved, councilKey("ccl-corrupt"))
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		testWrite(t, filepath.Dir(path), filepath.Base(path), `{"Key":"x"}`, 0o600)
		if err := a.CleanupCouncil(ctx, "ccl-corrupt"); err == nil || !strings.Contains(err.Error(), "invalid council checkout state") {
			t.Fatalf("CleanupCouncil = %v, want invalid state refusal", err)
		}
		if _, err := os.Stat(path); err != nil {
			t.Errorf("corrupt state was discarded: %v", err)
		}
	})
}
