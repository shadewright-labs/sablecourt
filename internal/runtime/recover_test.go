package runtime

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestProcessAliveWithStartTime(t *testing.T) {
	dir := t.TempDir()
	stopFile := filepath.Join(dir, "stop")
	cmd := exec.Command("/bin/sh", "-c", `while [ ! -f "$1" ]; do sleep 0.02; done`, "probe", stopFile)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start child: %v", err)
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

	start, err := ProcessStartTime(cmd.Process.Pid)
	if err != nil {
		t.Fatalf("ProcessStartTime(live): %v", err)
	}
	if start.IsZero() {
		t.Fatal("ProcessStartTime returned zero timestamp")
	}

	for _, tc := range []struct {
		name      string
		pid       int
		startTime time.Time
		want      bool
	}{
		{name: "matching_live_process", pid: cmd.Process.Pid, startTime: start, want: true},
		{name: "reused_pid_different_start_time", pid: cmd.Process.Pid, startTime: start.Add(-time.Second), want: false},
		{name: "zero_start_time", pid: cmd.Process.Pid, startTime: time.Time{}, want: false},
		{name: "zero_pid", pid: 0, startTime: start, want: false},
		{name: "negative_pid", pid: -1, startTime: start, want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			alive, err := ProcessAliveWithStartTime(tc.pid, tc.startTime)
			if err != nil {
				t.Fatalf("ProcessAliveWithStartTime: %v", err)
			}
			if alive != tc.want {
				t.Fatalf("alive = %t, want %t", alive, tc.want)
			}
		})
	}

	if err := os.WriteFile(stopFile, []byte("stop"), 0o600); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("child process did not exit")
	}

	if _, err := ProcessStartTime(cmd.Process.Pid); !errors.Is(err, ErrProcessNotFound) {
		t.Fatalf("ProcessStartTime(exited) = %v, want ErrProcessNotFound", err)
	}
	alive, err := ProcessAliveWithStartTime(cmd.Process.Pid, start)
	if err != nil || alive {
		t.Fatalf("ProcessAliveWithStartTime(exited) = (%t, %v), want (false, nil)", alive, err)
	}
}
