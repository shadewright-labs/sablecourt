package coding

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	scruntime "github.com/shadewright-labs/sablecourt/internal/runtime"
)

type failingLogCloser struct {
	file     *os.File
	closeErr error
}

func (f *failingLogCloser) WriteString(s string) (int, error) {
	return f.file.WriteString(s)
}

func (f *failingLogCloser) Close() error {
	_ = f.file.Close()
	return f.closeErr
}

type discardWriteCloser struct {
	bytes.Buffer
}

func (d *discardWriteCloser) Close() error { return nil }

func TestWrapperOutputLogCloseErrors(t *testing.T) {
	errStdout := errors.New("stdout disk full")
	errStderr := errors.New("stderr io error")

	for _, tc := range []struct {
		name           string
		script         string
		stdoutCloseErr error
		stderrCloseErr error
		wantState      scruntime.ObservedState
		wantExitCode   int
		wantLogClose   bool
		wantFinalMsg   string
		wantMsgSubs    []string
	}{
		{
			name:         "exit_0_clean_close",
			script:       "printf 'hello from harness\\n'",
			wantState:    scruntime.StateCompleted,
			wantExitCode: 0,
			wantFinalMsg: "hello from harness",
		},
		{
			name:           "exit_0_stdout_close_fails",
			script:         "printf 'hello from harness\\n'",
			stdoutCloseErr: errStdout,
			wantState:      scruntime.StateFailed,
			wantExitCode:   scruntime.ExitCodeLogClose,
			wantLogClose:   true,
			wantMsgSubs:    []string{"stdout", "stdout disk full"},
		},
		{
			name:           "exit_0_stderr_close_fails",
			script:         "printf 'hello from harness\\n'",
			stderrCloseErr: errStderr,
			wantState:      scruntime.StateFailed,
			wantExitCode:   scruntime.ExitCodeLogClose,
			wantLogClose:   true,
			wantMsgSubs:    []string{"stderr", "stderr io error"},
		},
		{
			name:           "exit_0_both_closes_fail",
			script:         "printf 'hello from harness\\n'",
			stdoutCloseErr: errStdout,
			stderrCloseErr: errStderr,
			wantState:      scruntime.StateFailed,
			wantExitCode:   scruntime.ExitCodeLogClose,
			wantLogClose:   true,
			wantMsgSubs:    []string{"stdout", "stdout disk full", "stderr", "stderr io error"},
		},
		{
			name:           "exit_nonzero_stdout_close_fails_task_status_wins",
			script:         "printf 'harness failed detail\\n' >&2; exit 17",
			stdoutCloseErr: errStdout,
			wantState:      scruntime.StateFailed,
			wantExitCode:   17,
			wantLogClose:   true,
			wantMsgSubs:    []string{"harness failed detail", "stdout", "stdout disk full"},
		},
		{
			name:           "exit_nonzero_stderr_close_fails_task_status_wins",
			script:         "printf 'harness failed detail\\n' >&2; exit 23",
			stderrCloseErr: errStderr,
			wantState:      scruntime.StateFailed,
			wantExitCode:   23,
			wantLogClose:   true,
			wantMsgSubs:    []string{"harness failed detail", "stderr", "stderr io error"},
		},
		{
			name:           "exit_0_large_stderr_and_close_fails_stays_bounded",
			script:         "head -c 5000 /dev/zero | tr '\\0' 'x' >&2",
			stdoutCloseErr: errStdout,
			wantState:      scruntime.StateFailed,
			wantExitCode:   scruntime.ExitCodeLogClose,
			wantLogClose:   true,
			wantMsgSubs:    []string{"stdout", "stdout disk full"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			a, spec := codingFixture(t)
			a.DryRun = true
			h := startTestTask(t, a, spec)

			runDir := h.WorkDir + ".run"
			if err := os.Mkdir(runDir, 0o700); err != nil {
				t.Fatalf("mkdir .run: %v", err)
			}
			scratch := t.TempDir()
			ws := wrapperSpec{
				Args:      []string{"/bin/sh", "-c", tc.script},
				Dir:       h.WorkDir,
				Scratch:   scratch,
				StateBase: h.WorkDir,
				Limit:     10 * time.Second,
			}
			rawSpec, err := json.Marshal(ws)
			if err != nil {
				t.Fatalf("marshal spec: %v", err)
			}

			readyBuf := &discardWriteCloser{}
			err = runWrapperWith(wrapperIO{
				stdin: bytes.NewReader(rawSpec),
				ready: readyBuf,
				live:  io.Discard,
				term:  make(chan os.Signal, 1),
				openLog: func(path string) (wrapperLogFile, error) {
					f, openErr := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
					if openErr != nil {
						return nil, openErr
					}
					switch filepath.Base(path) {
					case "stdout":
						if tc.stdoutCloseErr != nil {
							return &failingLogCloser{file: f, closeErr: tc.stdoutCloseErr}, nil
						}
					case "stderr":
						if tc.stderrCloseErr != nil {
							return &failingLogCloser{file: f, closeErr: tc.stderrCloseErr}, nil
						}
					}
					return f, nil
				},
			})
			if err != nil {
				t.Fatalf("runWrapperWith = %v", err)
			}

			var status readyStatus
			if err := json.Unmarshal(readyBuf.Bytes(), &status); err != nil {
				t.Fatalf("decode ready status %q: %v", readyBuf.String(), err)
			}
			h.PID = status.PID
			h.ProcessStartTime = status.Start

			obs, err := a.Observe(context.Background(), h)
			if err != nil {
				t.Fatalf("Observe = %v", err)
			}
			if obs.State != tc.wantState || obs.ExitCode != tc.wantExitCode || obs.LogCloseFailed != tc.wantLogClose {
				t.Fatalf("Observe = (state=%s, exit=%d, logClose=%v, msg=%q), want (state=%s, exit=%d, logClose=%v)",
					obs.State, obs.ExitCode, obs.LogCloseFailed, obs.Message, tc.wantState, tc.wantExitCode, tc.wantLogClose)
			}
			for _, sub := range tc.wantMsgSubs {
				if !strings.Contains(obs.Message, sub) {
					t.Errorf("obs.Message = %q, want substring %q", obs.Message, sub)
				}
			}

			arts, err := a.CollectArtifacts(context.Background(), h)
			if err != nil {
				t.Fatalf("CollectArtifacts = %v", err)
			}
			var finalMsg string
			for _, art := range arts {
				if art.Name == "final-message" {
					finalMsg = strings.TrimSpace(art.Text)
				}
			}
			if finalMsg != tc.wantFinalMsg {
				t.Fatalf("final-message = %q, want %q", finalMsg, tc.wantFinalMsg)
			}
		})
	}
}

func TestWrapperPostCloseWriteErrors(t *testing.T) {
	errStdout := errors.New("stdout disk full")

	t.Run("stderr_write_fails_after_close_error_still_publishes_exit", func(t *testing.T) {
		t.Parallel()
		a, spec := codingFixture(t)
		a.DryRun = true
		h := startTestTask(t, a, spec)

		runDir := h.WorkDir + ".run"
		if err := os.Mkdir(runDir, 0o700); err != nil {
			t.Fatalf("mkdir .run: %v", err)
		}
		ws := wrapperSpec{
			Args:      []string{"/bin/sh", "-c", "printf 'hello\\n'"},
			Dir:       h.WorkDir,
			Scratch:   t.TempDir(),
			StateBase: h.WorkDir,
			Limit:     10 * time.Second,
		}
		rawSpec, err := json.Marshal(ws)
		if err != nil {
			t.Fatalf("marshal spec: %v", err)
		}

		readyBuf := &discardWriteCloser{}
		err = runWrapperWith(wrapperIO{
			stdin: bytes.NewReader(rawSpec),
			ready: readyBuf,
			live:  io.Discard,
			term:  make(chan os.Signal, 1),
			openLog: func(path string) (wrapperLogFile, error) {
				perm := os.FileMode(0o600)
				if filepath.Base(path) == "stderr" {
					// Creating 0400 succeeds for the open fd, then os.WriteFile fails with EACCES.
					perm = 0o400
				}
				f, openErr := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, perm)
				if openErr != nil {
					return nil, openErr
				}
				if filepath.Base(path) == "stdout" {
					return &failingLogCloser{file: f, closeErr: errStdout}, nil
				}
				return f, nil
			},
		})
		if err == nil {
			t.Fatalf("expected non-nil error when stderr write fails")
		}
		if !errors.Is(err, errStdout) {
			t.Fatalf("returned error %v dropped stdout close error %v", err, errStdout)
		}

		var status readyStatus
		if err := json.Unmarshal(readyBuf.Bytes(), &status); err != nil {
			t.Fatalf("decode ready status: %v", err)
		}
		h.PID = status.PID
		h.ProcessStartTime = status.Start

		obs, err := a.Observe(context.Background(), h)
		if err != nil {
			t.Fatalf("Observe = %v, want published exit record", err)
		}
		if obs.State != scruntime.StateFailed || obs.ExitCode != scruntime.ExitCodeLogClose || !obs.LogCloseFailed {
			t.Fatalf("Observe = (state=%s, exit=%d, logClose=%v), want (failed, %d, true)", obs.State, obs.ExitCode, obs.LogCloseFailed, scruntime.ExitCodeLogClose)
		}
		if !strings.Contains(obs.Message, "stdout") || !strings.Contains(obs.Message, "stderr") {
			t.Fatalf("obs.Message = %q, want both stdout close and stderr write named", obs.Message)
		}
	})

	t.Run("exit_tmp_write_fails_joins_close_and_write_errors", func(t *testing.T) {
		t.Parallel()
		a, spec := codingFixture(t)
		a.DryRun = true
		h := startTestTask(t, a, spec)

		runDir := h.WorkDir + ".run"
		if err := os.Mkdir(runDir, 0o700); err != nil {
			t.Fatalf("mkdir .run: %v", err)
		}
		ws := wrapperSpec{
			Args:      []string{"/bin/sh", "-c", "printf 'hello\\n'"},
			Dir:       h.WorkDir,
			Scratch:   t.TempDir(),
			StateBase: h.WorkDir,
			Limit:     10 * time.Second,
		}
		rawSpec, err := json.Marshal(ws)
		if err != nil {
			t.Fatalf("marshal spec: %v", err)
		}
		errDiskFull := errors.New("simulated enospc")
		readyBuf := &discardWriteCloser{}
		err = runWrapperWith(wrapperIO{
			stdin: bytes.NewReader(rawSpec),
			ready: readyBuf,
			live:  io.Discard,
			term:  make(chan os.Signal, 1),
			openLog: func(path string) (wrapperLogFile, error) {
				f, openErr := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
				if openErr != nil {
					return nil, openErr
				}
				if filepath.Base(path) == "stdout" {
					return &failingLogCloser{file: f, closeErr: errStdout}, nil
				}
				return f, nil
			},
			writeFile: func(name string, data []byte, perm os.FileMode) error {
				return errDiskFull
			},
		})
		if !errors.Is(err, errStdout) || !errors.Is(err, errDiskFull) {
			t.Fatalf("runWrapperWith = %v, want joined %v and %v", err, errStdout, errDiskFull)
		}
	})

	t.Run("stdout_write_fails_still_publishes_exit", func(t *testing.T) {
		t.Parallel()
		a, spec := codingFixture(t)
		a.DryRun = true
		h := startTestTask(t, a, spec)

		runDir := h.WorkDir + ".run"
		if err := os.Mkdir(runDir, 0o700); err != nil {
			t.Fatalf("mkdir .run: %v", err)
		}
		ws := wrapperSpec{
			Args:      []string{"/bin/sh", "-c", "printf 'hello\\n'"},
			Dir:       h.WorkDir,
			Scratch:   t.TempDir(),
			StateBase: h.WorkDir,
			Limit:     10 * time.Second,
		}
		rawSpec, err := json.Marshal(ws)
		if err != nil {
			t.Fatalf("marshal spec: %v", err)
		}

		readyBuf := &discardWriteCloser{}
		err = runWrapperWith(wrapperIO{
			stdin: bytes.NewReader(rawSpec),
			ready: readyBuf,
			live:  io.Discard,
			term:  make(chan os.Signal, 1),
			openLog: func(path string) (wrapperLogFile, error) {
				perm := os.FileMode(0o600)
				if filepath.Base(path) == "stdout" {
					perm = 0o400
				}
				return os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, perm)
			},
		})
		if err == nil {
			t.Fatalf("expected non-nil error when stdout write fails")
		}

		var status readyStatus
		if err := json.Unmarshal(readyBuf.Bytes(), &status); err != nil {
			t.Fatalf("decode ready status: %v", err)
		}
		h.PID = status.PID
		h.ProcessStartTime = status.Start

		obs, err := a.Observe(context.Background(), h)
		if err != nil {
			t.Fatalf("Observe = %v, want published exit record", err)
		}
		if obs.State != scruntime.StateFailed {
			t.Fatalf("obs.State = %s, want failed", obs.State)
		}
		if !strings.Contains(obs.Message, "stdout") {
			t.Fatalf("obs.Message = %q, want stdout named", obs.Message)
		}
	})
}

func TestWrapperNonZeroExitLongStderrKeepsCloseErrorInFirst1KiB(t *testing.T) {
	t.Parallel()
	errStdout := errors.New("stdout disk full")
	a, spec := codingFixture(t)
	a.DryRun = true
	h := startTestTask(t, a, spec)

	runDir := h.WorkDir + ".run"
	if err := os.Mkdir(runDir, 0o700); err != nil {
		t.Fatalf("mkdir .run: %v", err)
	}
	ws := wrapperSpec{
		Args:      []string{"/bin/sh", "-c", "head -c 3000 /dev/zero | tr '\\0' 'e' >&2; exit 17"},
		Dir:       h.WorkDir,
		Scratch:   t.TempDir(),
		StateBase: h.WorkDir,
		Limit:     10 * time.Second,
	}
	rawSpec, err := json.Marshal(ws)
	if err != nil {
		t.Fatalf("marshal spec: %v", err)
	}
	readyBuf := &discardWriteCloser{}
	if err := runWrapperWith(wrapperIO{
		stdin: bytes.NewReader(rawSpec),
		ready: readyBuf,
		live:  io.Discard,
		term:  make(chan os.Signal, 1),
		openLog: func(path string) (wrapperLogFile, error) {
			f, openErr := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
			if openErr != nil {
				return nil, openErr
			}
			if filepath.Base(path) == "stdout" {
				return &failingLogCloser{file: f, closeErr: errStdout}, nil
			}
			return f, nil
		},
	}); err != nil {
		t.Fatalf("runWrapperWith = %v", err)
	}

	var status readyStatus
	if err := json.Unmarshal(readyBuf.Bytes(), &status); err != nil {
		t.Fatalf("decode ready status: %v", err)
	}
	h.PID = status.PID
	h.ProcessStartTime = status.Start

	obs, err := a.Observe(context.Background(), h)
	if err != nil {
		t.Fatalf("Observe = %v", err)
	}
	if obs.ExitCode != 17 {
		t.Fatalf("obs.ExitCode = %d, want 17", obs.ExitCode)
	}
	head := obs.Message
	if len(head) > 1024 {
		head = head[:1024]
	}
	if !strings.Contains(head, "close coding stdout log") {
		t.Fatalf("close error was cut off past 1024 bytes; first 1024 bytes = %q", head)
	}
}

func TestWrapperLogCloseExitCodeDistinctFromHarness(t *testing.T) {
	t.Parallel()
	if wrapperLogCloseExitCode >= -1 && wrapperLogCloseExitCode <= 255 {
		t.Fatalf("wrapperLogCloseExitCode = %d collides with harness/signal range [-1, 255]", wrapperLogCloseExitCode)
	}
	now := time.Now().UTC().Truncate(time.Second)
	h := scruntime.Handle{PID: 42, ProcessStartTime: now}
	base := filepath.Join(t.TempDir(), "task")
	runDir := base + ".run"
	if err := os.MkdirAll(runDir, 0o700); err != nil {
		t.Fatal(err)
	}
	code := wrapperLogCloseExitCode
	raw, err := json.Marshal(exitRecord{workerIdentity: workerIdentity{42, now}, Code: &code})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(runDir, "exit"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(runDir, "stderr"), []byte("close coding stdout log: disk full"), 0o600); err != nil {
		t.Fatal(err)
	}
	res, err := recordedExitAt(base, h)
	if err != nil {
		t.Fatalf("recordedExitAt rejected wrapperLogCloseExitCode (%d): %v", code, err)
	}
	if res == nil || res.ExitCode != code || !res.LogClose {
		t.Fatalf("recordedExitAt = %+v, want exit code %d with LogClose=true", res, code)
	}
	for _, invalid := range []int{-3, 256} {
		badCode := invalid
		badRaw, err := json.Marshal(exitRecord{workerIdentity: workerIdentity{42, now}, Code: &badCode})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(runDir, "exit"), badRaw, 0o600); err != nil {
			t.Fatal(err)
		}
		if got, err := recordedExitAt(base, h); err == nil {
			t.Fatalf("recordedExitAt accepted out-of-range exit code %d: %+v", invalid, got)
		}
	}
}

func TestWrapperSIGTERMDuringSetupExitsImmediately(t *testing.T) {
	t.Parallel()
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	stdinR, stdinW, err := os.Pipe()
	if err != nil {
		t.Fatalf("stdin pipe: %v", err)
	}
	defer stdinR.Close()
	defer stdinW.Close()
	readyR, readyW, err := os.Pipe()
	if err != nil {
		t.Fatalf("ready pipe: %v", err)
	}
	defer readyR.Close()
	defer readyW.Close()

	cmd := exec.Command(exe, wrapperArg)
	cmd.Stdin = stdinR
	cmd.ExtraFiles = []*os.File{readyW}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start wrapper: %v", err)
	}
	_ = stdinR.Close()
	_ = readyW.Close()

	// Give the wrapper a moment to enter setup (blocked reading spec on stdin).
	time.Sleep(50 * time.Millisecond)
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		_ = cmd.Process.Kill()
		t.Fatalf("signal SIGTERM: %v", err)
	}

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	select {
	case waitErr := <-done:
		var exitErr *exec.ExitError
		if !errors.As(waitErr, &exitErr) {
			t.Fatalf("expected signal exit error, got %v", waitErr)
		}
		if ws, ok := exitErr.Sys().(syscall.WaitStatus); ok && ws.Signal() != syscall.SIGTERM {
			t.Fatalf("expected SIGTERM termination, got signal %v (exit %v)", ws.Signal(), waitErr)
		}
	case <-time.After(1 * time.Second):
		_ = cmd.Process.Kill()
		<-done
		t.Fatal("wrapper ignored/queued SIGTERM during setup instead of exiting")
	}
}
