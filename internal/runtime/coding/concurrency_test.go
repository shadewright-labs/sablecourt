package coding

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

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
	wrapper := `#!/bin/sh
case " $* " in
  *" branch --no-track "*)
    touch "$FIXTURE_GIT_SYNC/ready"
    while test ! -f "$FIXTURE_GIT_SYNC/release"; do sleep 0.01; done
    ;;
esac
exec "$FIXTURE_REAL_GIT" "$@"
`
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte(wrapper), 0o700); err != nil {
		t.Fatal(err)
	}
	release := func() error {
		return os.WriteFile(filepath.Join(bin, "release"), nil, 0o600)
	}
	started := make(chan error, 1)
	go func() {
		_, err := a.Start(ctx, spec)
		started <- err
	}()
	t.Cleanup(func() {
		if err := release(); err != nil {
			t.Error(err)
		}
		if err := <-started; err != nil {
			t.Error(err)
		}
	})
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(filepath.Join(bin, "ready")); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("second task did not reach its blocked Git command")
		}
		time.Sleep(10 * time.Millisecond)
	}

	observed := make(chan error, 1)
	go func() {
		_, err := a.Observe(ctx, running)
		observed <- err
	}()
	select {
	case err := <-observed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		if err := release(); err != nil {
			t.Fatal(err)
		}
		<-observed
		t.Fatal("observation waited for another task's Git command")
	}
}
