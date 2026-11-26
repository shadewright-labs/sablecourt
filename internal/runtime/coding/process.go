package coding

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"github.com/shadewright-labs/sablecourt/internal/redact"
)

const (
	defaultWallTime    = 15 * time.Minute
	processGracePeriod = time.Second
	maxDiagnosticTail  = 4 * 1024
)

type processResult struct {
	Start    time.Time
	PID      int
	ExitCode int
	LogClose bool
	Message  string
	Response string
}

type codingProcess struct {
	done    chan struct{}
	stop    chan struct{}
	exited  chan struct{}
	leader  *os.Process
	result  processResult
	stopErr error
	scratch string
}

func (p *codingProcess) requestStop() {
	select {
	case p.stop <- struct{}{}:
	default:
	}
}

func startProcess(cmd *exec.Cmd, dir, scratch string, limit time.Duration, live io.Writer, stdoutLog, stderrLog *boundedLog) (_ *codingProcess, startErr error) {
	logFile, err := os.OpenFile(dir+".log", os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	started := false
	defer func() {
		if !started {
			startErr = errors.Join(startErr, logFile.Close(), os.Remove(logFile.Name()))
		}
	}()

	stdout, stdoutChild, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	defer stdoutChild.Close() //nolint:errcheck // acknowledged dropped error: the parent's copy of the child's write end, closed on every return (after a failed Start too); the parent never writes to it, so no data depends on the result
	stderr, stderrChild, err := os.Pipe()
	if err != nil {
		_ = stdout.Close()
		return nil, err
	}
	defer stderrChild.Close() //nolint:errcheck // acknowledged dropped error: the parent's copy of the child's write end, closed on every return (after a failed Start too); the parent never writes to it, so no data depends on the result
	defer func() {
		if !started {
			_ = stdout.Close()
			_ = stderr.Close()
		}
	}()
	cmd.Stdout, cmd.Stderr = stdoutChild, stderrChild

	if err := cmd.Start(); err != nil {
		return nil, err
	}
	started = true
	wallDeadline := time.Now().Add(limit)

	response := &harnessResponse{}
	tail := &diagnosticTail{}
	log := &boundedLog{file: logFile, live: live}
	out := &sessionOutput{file: log, archive: stdoutLog}
	diagnostics := &sessionOutput{file: log, tail: tail, archive: stderrLog}
	outDone := copyOutput(stdout, io.MultiWriter(out, response), out)
	errDone := copyOutput(stderr, diagnostics, diagnostics)
	process := &codingProcess{
		done: make(chan struct{}), stop: make(chan struct{}, 1), exited: make(chan struct{}),
		leader: cmd.Process, result: processResult{PID: cmd.Process.Pid}, scratch: scratch,
	}
	go func() {
		defer close(process.done)
		process.wait(cmd, wallDeadline)
		deadline := time.Now().Add(processGracePeriod)
		for _, pipe := range []*os.File{stdout, stderr} {
			if err := pipe.SetReadDeadline(deadline); err != nil {
				_ = pipe.Close()
			}
		}
		stdoutErr, stderrErr := <-outDone, <-errDone
		if errors.Is(stdoutErr, os.ErrDeadlineExceeded) {
			response.invalid = true
			stdoutErr = nil
		}
		if errors.Is(stderrErr, os.ErrDeadlineExceeded) {
			stderrErr = nil
		}
		outputErr := errors.Join(stdoutErr, stderrErr, logFile.Close())

		if process.stopErr != nil && process.result.ExitCode == 0 {
			process.result.ExitCode = -1
			process.result.Message = "coding process group did not stop"
		}
		process.result.Message = strings.TrimSpace(strings.Join([]string{process.result.Message, tail.text}, "\n"))
		if outputErr != nil && process.result.ExitCode == 0 {
			process.result.ExitCode = -1
			process.result.Message = "could not capture coding process output"
		}
		if process.result.ExitCode == 0 {
			process.result.Response = response.text()
		}
		if process.stopErr == nil {
			process.stopErr = process.cleanScratch()
		}
	}()
	return process, nil
}

func (p *codingProcess) cleanScratch() error {
	if p.scratch == "" {
		return nil
	}
	if err := os.RemoveAll(p.scratch); err != nil {
		return fmt.Errorf("remove coding scratch: %w", err)
	}
	p.scratch = ""
	return nil
}

func copyOutput(pipe *os.File, writer io.Writer, output *sessionOutput) <-chan error {
	done := make(chan error, 1)
	go func() {
		defer pipe.Close() //nolint:errcheck // read end of an output pipe
		_, err := io.Copy(writer, pipe)
		if errors.Is(err, os.ErrDeadlineExceeded) {
			// An inherited descriptor can outlive its owner. Discard any
			// incomplete record before closing the pipe.
			done <- err
			return
		}
		done <- errors.Join(err, output.Flush())
	}()
	return done
}

func (p *codingProcess) wait(cmd *exec.Cmd, deadline time.Time) {
	timer := time.NewTimer(time.Until(deadline))
	defer timer.Stop()
	type exit struct {
		err error
		at  time.Time
	}
	waited := make(chan exit, 1)
	go func() {
		err := cmd.Wait()
		close(p.exited)
		waited <- exit{err: err, at: time.Now()}
	}()
	const timeout = "coding task exceeded its wall time limit"
	select {
	case ended := <-waited:
		if !ended.at.Before(deadline) {
			p.result.ExitCode, p.result.Message = -1, timeout
		} else if ended.err != nil {
			p.result.ExitCode = cmd.ProcessState.ExitCode()
			p.result.Message = fmt.Sprintf("coding harness exited with status %d", p.result.ExitCode)
		}
		// The lead process is reaped independently of both output pipes.
		p.stopErr = signalAndWait(context.Background(), cmd.Process.Pid, syscall.SIGKILL)
		return
	case <-timer.C:
		p.result.Message = timeout
	case <-p.stop:
		p.result.Message = "coding task was stopped"
	}
	p.result.ExitCode = -1
	p.stopErr = p.shutdown(context.Background())
}

func (p *codingProcess) shutdown(ctx context.Context) error {
	groupErr := stopProcessGroup(ctx, p.leader.Pid)
	if outputClosed(p.exited) {
		return groupErr
	}

	// The group may be gone while the original process is still running.
	// Use the owned process handle and bound the wait for it separately.
	for _, sig := range []syscall.Signal{syscall.SIGTERM, syscall.SIGKILL} {
		err := p.leader.Signal(sig)
		if err != nil && !errors.Is(err, os.ErrProcessDone) {
			return errors.Join(groupErr, fmt.Errorf("signal coding process: %w", err))
		}
		timer := time.NewTimer(processGracePeriod)
		select {
		case <-p.exited:
			timer.Stop()
			return groupErr
		case <-timer.C:
		}
	}
	return errors.Join(groupErr, errors.New("coding process did not exit"))
}

func processGroupRunning(pid int) (bool, error) {
	if pid <= 0 {
		return false, nil
	}
	err := syscall.Kill(-pid, 0)
	switch {
	case err == nil, errors.Is(err, syscall.EPERM):
		return true, nil
	case errors.Is(err, syscall.ESRCH):
		return false, nil
	default:
		return false, fmt.Errorf("check coding process group: %w", err)
	}
}

func outputClosed(done <-chan struct{}) bool {
	if done == nil {
		return true
	}
	select {
	case <-done:
		return true
	default:
		return false
	}
}

func stopProcessGroup(ctx context.Context, pid int) error {
	if err := signalAndWait(ctx, pid, syscall.SIGTERM); !errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	if err := signalAndWait(ctx, pid, syscall.SIGKILL); err != nil {
		return fmt.Errorf("coding process group did not stop: %w", err)
	}
	return nil
}

func signalAndWait(ctx context.Context, pid int, signal syscall.Signal) error {
	if pid <= 0 {
		return errors.New("invalid coding process group")
	}
	if err := syscall.Kill(-pid, signal); err != nil && !errors.Is(err, syscall.ESRCH) {
		return fmt.Errorf("signal coding process group: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), processGracePeriod)
	defer cancel()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		running, err := processGroupRunning(pid)
		if err != nil || !running {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-tick.C:
		}
	}
}

type harnessResponse struct {
	buf     bytes.Buffer
	invalid bool
}

func (h *harnessResponse) Write(p []byte) (int, error) {
	if !h.invalid && h.buf.Len()+len(p) <= 64*1024 {
		return h.buf.Write(p)
	}
	h.invalid = true
	return len(p), nil
}

func (h *harnessResponse) text() string {
	if h.invalid || hasUnfinishedPrivateKey(h.buf.String()) {
		return ""
	}
	return redact.String(h.buf.String())
}

type diagnosticTail struct {
	text string
}

func (d *diagnosticTail) add(line string) {
	if len(line) >= maxDiagnosticTail {
		d.text = line[len(line)-maxDiagnosticTail:]
		return
	}
	d.text += line
	if len(d.text) > maxDiagnosticTail {
		d.text = d.text[len(d.text)-maxDiagnosticTail:]
	}
}
