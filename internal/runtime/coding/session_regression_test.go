package coding

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"

	scruntime "github.com/shadewright-labs/sablecourt/internal/runtime"
)

func TestUnrelatedObservationDoesNotWaitForStop(t *testing.T) {
	for _, name := range []string{"observe", "collect"} {
		t.Run(name, func(t *testing.T) {
			shellHarness(t, `trap 'printf stopping > stopping; while test ! -f release; do :; done; exit 0' TERM
printf ready > ready
while :; do :; done
`)
			a, spec := codingFixture(t)
			first := startTestTask(t, a, spec)
			spec.TaskID += "-second"
			second := startTestTask(t, a, spec)
			for _, handle := range []scruntime.Handle{first, second} {
				t.Cleanup(func() { _ = syscall.Kill(-handle.PID, syscall.SIGKILL) })
				waitUntil(t, func() bool {
					_, err := os.Stat(filepath.Join(handle.WorkDir, "ready"))
					return err == nil
				})
			}
			ctx := context.Background()
			stopped := make(chan error, 1)
			go func() { stopped <- a.Stop(ctx, first) }()
			waitUntil(t, func() bool {
				_, err := os.Stat(filepath.Join(first.WorkDir, "stopping"))
				return err == nil
			})
			observed := make(chan error, 1)
			go func() {
				if name == "collect" {
					_, err := a.CollectArtifacts(ctx, second)
					if err == nil {
						observed <- errors.New("collected from a running process")
						return
					}
					observed <- nil
					return
				}
				obs, err := a.Observe(ctx, second)
				if err == nil && obs.State != scruntime.StateRunning {
					err = errors.New("second process was not running")
				}
				observed <- err
			}()
			var err error
			select {
			case err = <-observed:
				testWrite(t, first.WorkDir, "release", "", 0o600)
			case <-time.After(500 * time.Millisecond):
				t.Error("operation waited for an unrelated task to stop")
				testWrite(t, first.WorkDir, "release", "", 0o600)
				err = <-observed
			}
			if err != nil {
				t.Error(err)
			}
			if err := <-stopped; err != nil {
				t.Fatal(err)
			}
			testWrite(t, second.WorkDir, "release", "", 0o600)
			if err := a.Stop(ctx, second); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestSessionLogBound(t *testing.T) {
	for _, stream := range []string{"stdout", "stderr"} {
		t.Run(stream, func(t *testing.T) {
			command := "i=0; while test $i -lt 1500; do printf '%1024s\\n' x; i=$((i+1)); done"
			if stream == "stderr" {
				command = "{ " + command + "; } >&2"
			}
			shellHarness(t, command+"\nprintf done > emitted")
			a, spec := codingFixture(t)
			h := startTestTask(t, a, spec)
			t.Cleanup(func() { _ = syscall.Kill(-h.PID, syscall.SIGKILL) })
			deadline := time.Now().Add(30 * time.Second)
			for {
				if _, err := os.Stat(filepath.Join(h.WorkDir, "emitted")); err == nil {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("harness did not finish writing its fixture output")
				}
				time.Sleep(10 * time.Millisecond)
			}
			info, err := os.Stat(h.WorkDir + ".log")
			if err != nil {
				t.Fatal(err)
			}
			if info.Size() < 900*1024 || info.Size() > 1024*1024 {
				t.Fatalf("session log grew to %d bytes", info.Size())
			}
		})
	}
}

func TestParallelWorktreeRegistration(t *testing.T) {
	for _, count := range []int{4, 8} {
		t.Run(strconv.Itoa(count), func(t *testing.T) {
			a, spec := codingFixture(t)
			a.DryRun = true
			type launch struct {
				handle scruntime.Handle
				err    error
			}
			start := make(chan struct{})
			results := make(chan launch, count)
			for i := range count {
				task := spec
				task.TaskID += "-" + strconv.Itoa(i)
				go func() {
					<-start
					handle, err := a.Start(context.Background(), task)
					results <- launch{handle, err}
				}()
			}
			close(start)
			var handles []scruntime.Handle
			for range count {
				result := <-results
				if result.err != nil {
					t.Error(result.err)
				} else {
					handles = append(handles, result.handle)
				}
			}
			for _, handle := range handles {
				if err := a.Stop(context.Background(), handle); err != nil {
					t.Error(err)
				}
			}
		})
	}
}

func TestProcessShutdownReapsLeaderAfterGroupLoss(t *testing.T) {
	for _, reason := range []string{"wall time", "stop"} {
		t.Run(reason, func(t *testing.T) {
			a, spec := codingFixture(t)
			a.DryRun = true
			h := startTestTask(t, a, spec)
			if _, err := a.worktreeState(h); err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command("/bin/sh", "-c", "trap '' TERM; while :; do :; done")
			cmd.Dir = h.WorkDir
			// Start without a new group to exercise the missing-group path.
			limit := 150 * time.Millisecond
			if reason == "stop" {
				limit = time.Minute
			}
			process, err := startProcess(cmd, h.WorkDir, t.TempDir(), limit, nil, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				_ = cmd.Process.Kill()
				<-process.done
			})
			if reason == "stop" {
				process.requestStop()
			}
			select {
			case <-process.done:
				if process.stopErr != nil || process.result.ExitCode != -1 {
					t.Fatalf("shutdown = %+v, %v", process.result, process.stopErr)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("shutdown did not reap the leader")
			}
		})
	}
}
