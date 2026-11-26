package coding

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/shadewright-labs/sablecourt/internal/attach"
	scruntime "github.com/shadewright-labs/sablecourt/internal/runtime"
)

func TestCodingAdapterHandleAlive(t *testing.T) {
	worktreeRoot := t.TempDir()
	resolvedRoot, err := resolvedPath(worktreeRoot)
	if err != nil {
		t.Fatal(err)
	}
	taskID := "task_recover_probe"
	workDir := filepath.Join(resolvedRoot, sanitizeSegment(taskID))
	branch := "sablecourt/" + sanitizeSegment(taskID)
	baseCommit := strings.Repeat("a", 40)

	state := &worktreeState{
		TaskID: taskID,
		Branch: branch,
		Base:   baseCommit,
	}
	if err := writeWorktreeState(workDir, state); err != nil {
		t.Fatal(err)
	}

	stopFile := filepath.Join(t.TempDir(), "stop")
	cmd := exec.Command("/bin/sh", "-c", `while [ ! -f "$1" ]; do sleep 0.02; done`, "coding-probe", stopFile)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start stub process: %v", err)
	}
	done := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(done)
	}()
	t.Cleanup(func() {
		_ = os.WriteFile(stopFile, []byte("stop"), 0o600)
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			_ = cmd.Process.Kill()
			<-done
		}
	})

	start, err := scruntime.ProcessStartTime(cmd.Process.Pid)
	if err != nil {
		t.Fatalf("ProcessStartTime: %v", err)
	}

	adapter := NewAdapter(worktreeRoot)
	validHandle := scruntime.Handle{
		AdapterName:      "coding:claude",
		ExternalID:       taskID,
		WorkDir:          workDir,
		Branch:           branch,
		PID:              cmd.Process.Pid,
		ProcessStartTime: start,
		LogPath:          workDir + ".log",
	}

	for _, tc := range []struct {
		name    string
		mutate  func(scruntime.Handle) scruntime.Handle
		want    bool
		wantErr bool
	}{
		{name: "matching_live_process", mutate: func(h scruntime.Handle) scruntime.Handle { return h }, want: true},
		{name: "reused_pid_different_start_time", mutate: func(h scruntime.Handle) scruntime.Handle {
			h.ProcessStartTime = h.ProcessStartTime.Add(-2 * time.Second)
			return h
		}, want: false},
		{name: "zero_pid", mutate: func(h scruntime.Handle) scruntime.Handle { h.PID = 0; return h }, want: false},
		{name: "zero_start_time", mutate: func(h scruntime.Handle) scruntime.Handle { h.ProcessStartTime = time.Time{}; return h }, want: false},
		{name: "wrong_workdir", mutate: func(h scruntime.Handle) scruntime.Handle { h.WorkDir = filepath.Join(resolvedRoot, "other"); return h }, want: false},
		{name: "wrong_log_path", mutate: func(h scruntime.Handle) scruntime.Handle { h.LogPath = "/tmp/other.log"; return h }, want: false},
		{name: "empty_external_id", mutate: func(h scruntime.Handle) scruntime.Handle { h.ExternalID = ""; return h }, want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			alive, err := adapter.HandleAlive(context.Background(), tc.mutate(validHandle))
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr = %t", err, tc.wantErr)
			}
			if alive != tc.want {
				t.Fatalf("alive = %t, want %t", alive, tc.want)
			}
		})
	}

	// Cancelled context returns context error.
	canceledCtx, cancel := context.WithCancel(context.Background())
	cancel()
	if alive, err := adapter.HandleAlive(canceledCtx, validHandle); alive || err == nil {
		t.Fatalf("cancelled ctx returned (%t, %v), want (false, err)", alive, err)
	}

	// Exited process returns false.
	if err := os.WriteFile(stopFile, []byte("stop"), 0o600); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("stub process did not exit")
	}
	if alive, err := adapter.HandleAlive(context.Background(), validHandle); err != nil || alive {
		t.Fatalf("exited process returned (%t, %v), want (false, nil)", alive, err)
	}
}

func TestCodingAdapterProbeInaccessibleWorktreeRootReturnsError(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "worktrees")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	resolvedRoot, err := resolvedPath(root)
	if err != nil {
		t.Fatal(err)
	}
	taskID := "task_inaccessible_root"
	workDir := filepath.Join(resolvedRoot, sanitizeSegment(taskID))
	branch := "sablecourt/" + sanitizeSegment(taskID)
	if err := writeWorktreeState(workDir, &worktreeState{
		TaskID: taskID,
		Branch: branch,
		Base:   strings.Repeat("a", 40),
	}); err != nil {
		t.Fatal(err)
	}
	start, err := scruntime.ProcessStartTime(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	handle := scruntime.Handle{
		AdapterName:      "coding:claude",
		ExternalID:       taskID,
		WorkDir:          workDir,
		Branch:           branch,
		PID:              os.Getpid(),
		ProcessStartTime: start,
		LogPath:          workDir + ".log",
	}
	if err := os.Chmod(parent, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(parent, 0o700) })

	adapter := NewAdapter(root)
	if alive, err := adapter.HandleAlive(context.Background(), handle); err == nil || alive {
		t.Fatalf("HandleAlive on inaccessible root = (%t, %v), want (false, error)", alive, err)
	}
	if exited, err := adapter.HandleExited(context.Background(), handle); err == nil || exited {
		t.Fatalf("HandleExited on inaccessible root = (%t, %v), want (false, error)", exited, err)
	}
}

func TestAdapterLaunchIdentity(t *testing.T) {
	shellHarness(t, "exit 0")
	a, spec := codingFixture(t)
	h := startTestTask(t, a, spec)
	t.Cleanup(func() { _ = a.Stop(context.Background(), h) })
	if h.ProcessStartTime.IsZero() {
		t.Fatal("Start returned no process identity")
	}
}

func TestFreshAdapterWorker(t *testing.T) {
	for _, mode := range []string{"exit", "stop"} {
		t.Run(mode, func(t *testing.T) {
			shellHarness(t, "printf 'password=fixture-value\\n'; while test ! -f release; do sleep 0.02; done; printf 'exit detail\\n' >&2; exit 17")
			a, spec := codingFixture(t)
			h := startTestTask(t, a, spec)
			t.Cleanup(func() { _ = syscall.Kill(-h.PID, syscall.SIGKILL) })
			if h.ProcessStartTime.IsZero() {
				var err error
				h.ProcessStartTime, err = scruntime.ProcessStartTime(h.PID)
				if err != nil {
					t.Fatal(err)
				}
			}
			fresh := NewAdapter(a.WorktreeRoot)
			if mode == "stop" {
				if err := fresh.Stop(t.Context(), h); err != nil {
					t.Error(err)
				}
				if err := syscall.Kill(-h.PID, 0); err != syscall.ESRCH {
					t.Fatal("verified group survived stop", err)
				}
				return
			}
			obs, err := fresh.Observe(t.Context(), h)
			if err != nil || obs.State != scruntime.StateRunning {
				t.Fatal(obs, err)
			}
			testWrite(t, h.WorkDir, "release", "", 0o600)
			waitUntil(t, func() bool {
				obs, err = fresh.Observe(t.Context(), h)
				return err != nil || obs.State != scruntime.StateRunning
			})
			if err != nil || obs.ExitCode != 17 || !strings.Contains(obs.Message, "exit detail") {
				t.Fatal(obs, err)
			}
		})
	}
}

func TestExitedLauncher(t *testing.T) {
	const fixture = "SABLECOURT_TEST_LAUNCHER"
	if root := os.Getenv(fixture); root != "" {
		a := NewAdapter(filepath.Join(root, "worktrees"))
		a.allowUnconfined = true
		a.AttachHub = attach.NewHub()
		raw, err := os.ReadFile(filepath.Join(root, "spec"))
		if err != nil {
			t.Fatal(err)
		}
		var spec scruntime.LaunchSpec
		if err = json.Unmarshal(raw, &spec); err != nil {
			t.Fatal(err)
		}
		h := startTestTask(t, a, spec)
		raw, err = json.Marshal(h)
		if err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(filepath.Join(root, "handle"), raw, 0600); err != nil {
			t.Fatal(err)
		}
		return
	}
	for _, mode := range []string{"complete", "nonzero", "stop"} {
		t.Run(mode, func(t *testing.T) {
			root := t.TempDir()
			script := "echo $$ > harness-pid; while test ! -f release; do sleep 0.02; done; printf 'password=fixture-value\n'; printf 'diagnostic\n' >&2"
			if mode == "nonzero" {
				script += "; exit 31"
			}
			shellHarness(t, script)
			_, spec := codingFixture(t)
			raw, err := json.Marshal(spec)
			if err != nil {
				t.Fatal(err)
			}
			testWrite(t, root, "spec", string(raw), 0600)
			cmd := exec.Command(os.Args[0], "-test.run=^TestExitedLauncher$")
			cmd.Env = append(os.Environ(), fixture+"="+root, "GORACE=atexit_sleep_ms=0")
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("launcher: %s: %v", out, err)
			}
			raw, err = os.ReadFile(filepath.Join(root, "handle"))
			if err != nil {
				t.Fatal(err)
			}
			var h scruntime.Handle
			if err = json.Unmarshal(raw, &h); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = syscall.Kill(-h.PID, syscall.SIGTERM) })
			fresh := NewAdapter(filepath.Join(root, "worktrees"))
			obs, err := fresh.Observe(t.Context(), h)
			if err != nil || obs.State != scruntime.StateRunning {
				t.Fatal(obs, err)
			}
			if mode == "stop" {
				var pid int
				waitUntil(t, func() bool {
					b, _ := os.ReadFile(filepath.Join(h.WorkDir, "harness-pid"))
					pid, _ = strconv.Atoi(strings.TrimSpace(string(b)))
					return pid > 0
				})
				if err := fresh.Stop(t.Context(), h); err != nil {
					t.Fatal(err)
				}
				if alive, err := scruntime.ProcessAliveWithStartTime(h.PID, h.ProcessStartTime); err != nil || alive {
					t.Fatal("worker survived", err)
				}
				if err := syscall.Kill(-pid, 0); err != syscall.ESRCH {
					t.Fatal("harness group survived stop", err)
				}
				return
			}
			testWrite(t, h.WorkDir, "release", "", 0600)
			waitUntil(t, func() bool {
				alive, err := scruntime.ProcessAliveWithStartTime(h.PID, h.ProcessStartTime)
				return err == nil && !alive
			})
			if ok, err := fresh.HandleExited(t.Context(), h); err != nil || !ok {
				t.Fatal("missing exit evidence", err)
			}
			obs, err = fresh.Observe(t.Context(), h)
			if err != nil {
				t.Fatal(err)
			}
			if mode == "nonzero" {
				if obs.State != scruntime.StateFailed || obs.ExitCode != 31 || !strings.Contains(obs.Message, "diagnostic") {
					t.Fatal(obs)
				}
				return
			}
			if obs.State != scruntime.StateCompleted {
				t.Fatal(obs)
			}
			arts, err := fresh.CollectArtifacts(t.Context(), h)
			if err != nil {
				t.Fatal(err)
			}
			for _, art := range arts {
				if art.Name == "final-message" && strings.TrimSpace(art.Text) == "password=[REDACTED:secret]" {
					return
				}
			}
			t.Fatal("missing redacted stdout", arts)
		})
	}
}

func TestRecoveredWorkerBinding(t *testing.T) {
	shellHarness(t, "sleep 30")
	a, spec := codingFixture(t)
	h := startTestTask(t, a, spec)
	t.Cleanup(func() { _ = a.Stop(context.Background(), h) })
	fresh := NewAdapter(a.WorktreeRoot)
	for _, name := range []string{"another_live_pid", "missing_pid"} {
		t.Run(name, func(t *testing.T) {
			other := h
			other.PID = os.Getpid()
			var err error
			other.ProcessStartTime, err = scruntime.ProcessStartTime(other.PID)
			if err != nil {
				t.Fatal(err)
			}
			if name == "missing_pid" {
				other.PID = 0
				other.ProcessStartTime = time.Time{}
			}
			if alive, _ := fresh.HandleAlive(t.Context(), other); alive {
				t.Fatal("accepted unrelated process")
			}
			if _, err := fresh.Observe(t.Context(), other); err == nil {
				t.Fatal("observed worktree using unrelated identity")
			}
		})
	}
}

func TestLegacyRecoveredStop(t *testing.T) {
	a, spec := codingFixture(t)
	a.DryRun = true
	h := startTestTask(t, a, spec)
	cmd := exec.Command("/bin/sh", "-c", `(trap '' TERM; echo ready > child-ready; while :; do sleep 0.1; done) & wait`)
	cmd.Dir = h.WorkDir
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	h.PID = cmd.Process.Pid
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()
	t.Cleanup(func() { _ = syscall.Kill(-h.PID, syscall.SIGKILL); <-done })
	var err error
	waitUntil(t, func() bool { _, err := os.Stat(filepath.Join(h.WorkDir, "child-ready")); return err == nil })
	h.ProcessStartTime, err = scruntime.ProcessStartTime(h.PID)
	if err != nil {
		t.Fatal(err)
	}
	fresh := NewAdapter(a.WorktreeRoot)
	if err := fresh.Stop(t.Context(), h); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Kill(-h.PID, 0); err != syscall.ESRCH {
		t.Fatal("group survived cancellation", err)
	}
}

func TestRecordedExit(t *testing.T) {
	start := time.Now().UTC()
	code := 0
	for _, tc := range []struct {
		name           string
		record         exitRecord
		stdout, stderr string
		wantError      bool
	}{
		{"redacted", exitRecord{workerIdentity: workerIdentity{42, start}, Code: &code}, "password=fixture-value\n", "password=fixture-value\n", false},
		{"other_pid", exitRecord{workerIdentity: workerIdentity{41, start}, Code: &code}, "", "", true},
		{"other_start", exitRecord{workerIdentity: workerIdentity{42, start.Add(time.Second)}, Code: &code}, "", "", true},
		{"missing_status", exitRecord{workerIdentity: workerIdentity{42, start}, Code: nil}, "", "", true},
		{"oversized_stdout", exitRecord{workerIdentity: workerIdentity{42, start}, Code: &code}, strings.Repeat("a", 64*1024+1), "", true},
		{"oversized_stderr", exitRecord{workerIdentity: workerIdentity{42, start}, Code: &code}, "", strings.Repeat("a", maxDiagnosticTail+129), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := scruntime.Handle{PID: 42, ProcessStartTime: start, WorkDir: filepath.Join(t.TempDir(), "work")}
			dir := h.WorkDir + ".run"
			if err := os.Mkdir(dir, 0700); err != nil {
				t.Fatal(err)
			}
			raw, err := json.Marshal(tc.record)
			if err != nil {
				t.Fatal(err)
			}
			testWrite(t, dir, "exit.tmp", string(raw), 0600)
			if result, err := recordedExit(h); result != nil || err != nil {
				t.Fatal("read unpublished exit", result, err)
			}
			testWrite(t, dir, "stdout", tc.stdout, 0600)
			testWrite(t, dir, "stderr", tc.stderr, 0600)
			if err := os.Rename(filepath.Join(dir, "exit.tmp"), filepath.Join(dir, "exit")); err != nil {
				t.Fatal(err)
			}
			result, err := recordedExit(h)
			if (err != nil) != tc.wantError {
				t.Fatal(result, err)
			}
			if !tc.wantError && (result.Message != "password=[REDACTED:secret]\n" || result.Response != result.Message) {
				t.Fatal(result)
			}
		})
	}
}
