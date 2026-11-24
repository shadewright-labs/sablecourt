package coding

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
