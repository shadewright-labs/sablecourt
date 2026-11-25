package coding

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
)

// openFIFO creates a named pipe and holds it open for reading and writing, so
// a shell that opens the other end never blocks on open and bytes written
// before the peer reads stay buffered.
func openFIFO(t *testing.T, path string) *os.File {
	t.Helper()
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })
	return f
}

func TestObserveDoesNotWaitForUnrelatedGit(t *testing.T) {
	a, spec := codingFixture(t)
	a.DryRun = true
	ctx := context.Background()
	running, err := a.Start(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	spec.TaskID += "-second"

	bin := t.TempDir()
	git, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("FIXTURE_REAL_GIT", git)
	t.Setenv("FIXTURE_GIT_SYNC", bin)
	// The wrapper announces itself on one pipe and blocks reading the other.
	// Both sides wait on the pipes, so no sleep or deadline is involved.
	ready := openFIFO(t, filepath.Join(bin, "ready"))
	release := openFIFO(t, filepath.Join(bin, "release"))
	wrapper := `#!/bin/sh
case " $* " in
  *" branch --no-track "*)
    echo ready > "$FIXTURE_GIT_SYNC/ready"
    read -r _ < "$FIXTURE_GIT_SYNC/release"
    ;;
esac
exec "$FIXTURE_REAL_GIT" "$@"
`
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte(wrapper), 0o700); err != nil {
		t.Fatal(err)
	}

	var startErr error
	startDone := make(chan struct{})
	go func() {
		defer close(startDone)
		_, startErr = a.Start(ctx, spec)
	}()
	t.Cleanup(func() {
		if _, err := release.WriteString("go\n"); err != nil {
			t.Error(err)
		}
		<-startDone
		if startErr != nil {
			t.Error(startErr)
		}
	})

	reached := make(chan error, 1)
	go func() {
		var b [1]byte
		_, err := ready.Read(b[:])
		reached <- err
	}()
	select {
	case err := <-reached:
		if err != nil {
			t.Fatal(err)
		}
	case <-startDone:
		t.Fatalf("second task ended before reaching its blocked Git command: %v", startErr)
	}

	// The second task now holds its own task lock inside a blocked Git command.
	// Observe of the first task must still return. If it ever waited on the
	// second task it would block here until the test binary's own timeout
	// dumps the stuck goroutines, which names the lock it is queued on.
	if _, err := a.Observe(ctx, running); err != nil {
		t.Fatal(err)
	}
}

// Worktree registration writes shared repository metadata, so it must hold the
// same per-repository gate as the config writers while Git runs.
func TestWorktreeRegistrationHoldsRepositoryGate(t *testing.T) {
	a, spec := codingFixture(t)
	a.DryRun = true
	ctx := context.Background()
	repo, err := resolvedPath(spec.Workload.Spec.Coding.RepoPath)
	if err != nil {
		t.Fatal(err)
	}

	bin := t.TempDir()
	git, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("FIXTURE_REAL_GIT", git)
	t.Setenv("FIXTURE_GIT_SYNC", bin)
	ready := openFIFO(t, filepath.Join(bin, "ready"))
	release := openFIFO(t, filepath.Join(bin, "release"))
	wrapper := `#!/bin/sh
case " $* " in
  *" worktree add "*)
    echo ready > "$FIXTURE_GIT_SYNC/ready"
    read -r _ < "$FIXTURE_GIT_SYNC/release"
    ;;
esac
exec "$FIXTURE_REAL_GIT" "$@"
`
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte(wrapper), 0o700); err != nil {
		t.Fatal(err)
	}

	var startErr error
	startDone := make(chan struct{})
	go func() {
		defer close(startDone)
		_, startErr = a.Start(ctx, spec)
	}()
	t.Cleanup(func() {
		if _, err := release.WriteString("go\n"); err != nil {
			t.Error(err)
		}
		<-startDone
		if startErr != nil {
			t.Error(startErr)
		}
	})

	reached := make(chan error, 1)
	go func() {
		var b [1]byte
		_, err := ready.Read(b[:])
		reached <- err
	}()
	select {
	case err := <-reached:
		if err != nil {
			t.Fatal(err)
		}
	case <-startDone:
		t.Fatalf("launch ended before registering its worktree: %v", startErr)
	}

	unlock, err := a.lockScope(ctx, "config:"+filepath.Join(repo, ".git"), true)
	if err == nil {
		unlock()
		t.Fatal("repository gate was free while a worktree was being registered")
	}
}
