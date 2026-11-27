package coding

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/shadewright-labs/sablecourt/internal/attach"
	"github.com/shadewright-labs/sablecourt/internal/redact"
	scruntime "github.com/shadewright-labs/sablecourt/internal/runtime"
)

const (
	wrapperArg              = "--sablecourt-coding-wrapper"
	wrapperLogCloseExitCode = scruntime.ExitCodeLogClose
	maxExitRecordBytes      = 512
)

type wrapperSpec struct {
	Args         []string
	Target       string `json:",omitempty"`
	Env          []string
	Dir, Scratch string
	StateBase    string
	Limit        time.Duration
}

type workerIdentity struct {
	PID   int
	Start time.Time
}

type readyStatus struct {
	workerIdentity
	Error       string `json:",omitempty"`
	MissingPath string `json:",omitempty"`
	NotExist    bool   `json:",omitempty"`
}

type exitRecord struct {
	workerIdentity
	Code     *int
	LogClose bool   `json:",omitempty"`
	Reason   string `json:",omitempty"`
}

func init() {
	if len(os.Args) == 2 && os.Args[1] == wrapperArg {
		if err := runWrapper(); err != nil {
			os.Exit(1)
		}
		os.Exit(0)
	}
}

type wrapperLogFile interface {
	io.StringWriter
	io.Closer
}

type wrapperIO struct {
	stdin        io.Reader
	ready        io.WriteCloser
	openReady    func() (io.WriteCloser, error)
	live         io.Writer
	term         <-chan os.Signal
	setupSignals func() (<-chan os.Signal, func())
	openLog      func(path string) (wrapperLogFile, error)
	writeFile    func(name string, data []byte, perm os.FileMode) error
}

func openWrapperLog(path string) (wrapperLogFile, error) {
	return os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
}

// runWrapper runs the detached coding harness inside a wrapper process.
// The harness receives the original sandbox command and its stripped environment.
// Only its parent opens the sibling state directory and publishes the exit.
//
// Exit status contract: the harness's own exit code wins whenever it is
// non-zero (including -1 from timeout, stop, or output capture errors). When
// the harness exits 0 and closing or finalizing its stdout or stderr log
// fails, the wrapper records exit code -2 (wrapperLogCloseExitCode, which a
// harness cannot return), sets LogClose on the exit record, omits stdout as a
// final response, and records an error naming the failed log(s) so callers
// fail the task without auto-remediation. When the harness already exited
// non-zero and a log close or post-close write also fails, the harness exit
// code stays, LogClose is set, and the error is prepended to the diagnostic
// message so it stays within the task record's reason bound.
func runWrapper() error {
	return runWrapperWith(wrapperIO{
		stdin: os.Stdin,
		openReady: func() (io.WriteCloser, error) {
			ready := os.NewFile(3, "ready")
			syscall.CloseOnExec(3)
			return ready, nil
		},
		live: os.Stdout,
		setupSignals: func() (<-chan os.Signal, func()) {
			// A live attach reader may disappear when the daemon exits.
			signal.Ignore(syscall.SIGPIPE)
			term := make(chan os.Signal, 1)
			signal.Notify(term, syscall.SIGTERM)
			return term, func() { signal.Stop(term) }
		},
		openLog:   openWrapperLog,
		writeFile: os.WriteFile,
	})
}

func runWrapperWith(ioDeps wrapperIO) (runErr error) {
	var spec wrapperSpec
	if err := json.NewDecoder(ioDeps.stdin).Decode(&spec); err != nil {
		return err
	}
	if len(spec.Args) == 0 {
		return errors.New("missing coding command")
	}
	base := spec.StateBase
	if base == "" {
		var err error
		base, err = os.Getwd()
		if err != nil {
			return err
		}
	}
	run := base + ".run"
	start, err := scruntime.ProcessStartTime(os.Getpid())
	if err != nil {
		return err
	}
	id := workerIdentity{os.Getpid(), start}
	state, err := readWorktreeState(base + stateSuffix)
	if err != nil {
		return err
	}
	state.Worker = &id
	if err := writeWorktreeState(base, state); err != nil {
		return err
	}
	ready := ioDeps.ready
	if ready == nil && ioDeps.openReady != nil {
		opened, err := ioDeps.openReady()
		if err != nil {
			return err
		}
		ready = opened
		defer ready.Close() //nolint:errcheck // readiness pipe; a lost status is seen by the reader as a closed pipe
	}
	term := ioDeps.term
	if term == nil && ioDeps.setupSignals != nil {
		ch, stop := ioDeps.setupSignals()
		term = ch
		if stop != nil {
			defer stop()
		}
	}
	openLog := ioDeps.openLog
	if openLog == nil {
		openLog = openWrapperLog
	}
	writeFile := ioDeps.writeFile
	if writeFile == nil {
		writeFile = os.WriteFile
	}
	cmd := exec.Command(spec.Args[0], spec.Args[1:]...)
	cmd.Dir, cmd.Env = spec.Dir, spec.Env
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stdoutPath := filepath.Join(run, "stdout")
	stdout, err := openLog(stdoutPath)
	if err != nil {
		return err
	}
	stdoutClosed := false
	defer func() {
		if !stdoutClosed {
			runErr = errors.Join(runErr, stdout.Close())
		}
	}()
	stderrPath := filepath.Join(run, "stderr")
	stderr, err := openLog(stderrPath)
	if err != nil {
		return err
	}
	stderrClosed := false
	defer func() {
		if !stderrClosed {
			runErr = errors.Join(runErr, stderr.Close())
		}
	}()
	if spec.Target != "" && spec.Target != spec.Args[0] {
		if err := checkWrappedTarget(spec.Target, spec.Dir); err != nil {
			_ = json.NewEncoder(ready).Encode(readyStatus{
				Error:       redact.String(err.Error()),
				MissingPath: spec.Target,
				NotExist:    errors.Is(err, os.ErrNotExist),
			})
			return err
		}
	}
	p, err := startProcess(cmd, base, spec.Scratch, spec.Limit, ioDeps.live, &boundedLog{file: stdout}, &boundedLog{file: stderr})
	if err != nil {
		_ = json.NewEncoder(ready).Encode(readyStatus{
			Error:    redact.String(err.Error()),
			NotExist: errors.Is(err, os.ErrNotExist),
		})
		return err
	}
	if err := json.NewEncoder(ready).Encode(readyStatus{workerIdentity: id}); err != nil {
		p.requestStop()
	}
	select {
	case <-term:
		p.requestStop()
		<-p.done
	case <-p.done:
	}
	stdoutCloseErr := stdout.Close()
	stdoutClosed = true
	stderrCloseErr := stderr.Close()
	stderrClosed = true
	if p.stopErr != nil {
		return errors.Join(p.stopErr, stdoutCloseErr, stderrCloseErr)
	}
	var postCloseErrs []error
	if stdoutCloseErr != nil {
		postCloseErrs = append(postCloseErrs, fmt.Errorf("close coding stdout log: %w", stdoutCloseErr))
	}
	if stderrCloseErr != nil {
		postCloseErrs = append(postCloseErrs, fmt.Errorf("close coding stderr log: %w", stderrCloseErr))
	}
	logCloseFailed := stdoutCloseErr != nil || stderrCloseErr != nil
	closeMsg := formatLogCloseError(stdoutCloseErr, stderrCloseErr)
	if closeMsg != "" {
		if p.result.ExitCode == 0 {
			p.result.ExitCode = wrapperLogCloseExitCode
			p.result.Response = ""
		}
		p.result.Message = prependDiagnostic(closeMsg, p.result.Message)
	}
	// Finalize the response and diagnostic tail before making the exit visible.
	var writeFailMsgs []string
	if p.result.ExitCode == 0 {
		if len(p.result.Response) > 64*1024 {
			p.result.Response = ""
		}
		if err := writeFile(stdoutPath, []byte(p.result.Response), 0600); err != nil {
			postCloseErrs = append(postCloseErrs, fmt.Errorf("write coding stdout log: %w", err))
			logCloseFailed = true
			p.result.ExitCode = wrapperLogCloseExitCode
			p.result.Response = ""
			msg := fmt.Sprintf("write coding stdout log: %s", redact.String(err.Error()))
			writeFailMsgs = append(writeFailMsgs, msg)
			p.result.Message = prependDiagnostic(msg, p.result.Message)
		}
	}
	stderrWriteFailed := false
	if err := writeFile(stderrPath, []byte(p.result.Message), 0600); err != nil {
		stderrWriteFailed = true
		postCloseErrs = append(postCloseErrs, fmt.Errorf("write coding stderr log: %w", err))
		logCloseFailed = true
		if p.result.ExitCode == 0 {
			p.result.ExitCode = wrapperLogCloseExitCode
			p.result.Response = ""
		}
		msg := fmt.Sprintf("write coding stderr log: %s", redact.String(err.Error()))
		writeFailMsgs = append(writeFailMsgs, msg)
		p.result.Message = prependDiagnostic(msg, p.result.Message)
	}
	rec := exitRecord{
		workerIdentity: id,
		Code:           &p.result.ExitCode,
		LogClose:       logCloseFailed,
	}
	var inlineReason string
	if stderrWriteFailed {
		var parts []string
		if closeMsg != "" {
			parts = append(parts, closeMsg)
		}
		parts = append(parts, writeFailMsgs...)
		inlineReason = redact.String(strings.Join(parts, "; "))
	}
	raw, err := marshalExitRecord(rec, inlineReason)
	if err != nil {
		return errors.Join(append(postCloseErrs, err)...)
	}
	tmp := filepath.Join(run, "exit.tmp")
	if err := writeFile(tmp, raw, 0600); err != nil {
		return errors.Join(append(postCloseErrs, err)...)
	}
	if err := os.Rename(tmp, filepath.Join(run, "exit")); err != nil {
		return errors.Join(append(postCloseErrs, err)...)
	}
	if len(writeFailMsgs) > 0 {
		return errors.Join(postCloseErrs...)
	}
	return nil
}

func marshalExitRecord(rec exitRecord, inlineReason string) ([]byte, error) {
	if inlineReason == "" {
		return json.Marshal(rec)
	}
	rec.Reason = inlineReason
	raw, err := json.Marshal(rec)
	if err != nil || len(raw) <= maxExitRecordBytes {
		return raw, err
	}
	// Shrink inlineReason on UTF-8 boundaries until the encoded record fits maxExitRecordBytes.
	hi := len(inlineReason)
	lo := 0
	for lo < hi {
		mid := (lo + hi + 1) / 2
		rec.Reason = truncateUTF8Prefix(inlineReason, mid)
		candidate, err := json.Marshal(rec)
		if err != nil {
			return nil, err
		}
		if len(candidate) <= maxExitRecordBytes {
			lo = mid
		} else {
			hi = mid - 1
		}
	}
	rec.Reason = truncateUTF8Prefix(inlineReason, lo)
	return json.Marshal(rec)
}

func formatLogCloseError(stdoutErr, stderrErr error) string {
	var parts []string
	if stdoutErr != nil {
		parts = append(parts, fmt.Sprintf("close coding stdout log: %s", redact.String(stdoutErr.Error())))
	}
	if stderrErr != nil {
		parts = append(parts, fmt.Sprintf("close coding stderr log: %s", redact.String(stderrErr.Error())))
	}
	return strings.Join(parts, "; ")
}

func prependDiagnostic(prefix, existing string) string {
	const limit = maxDiagnosticTail + 128
	if len(prefix) > limit {
		prefix = truncateUTF8Prefix(prefix, limit)
	}
	if existing == "" {
		return prefix
	}
	if len(prefix)+1+len(existing) <= limit {
		return prefix + "\n" + existing
	}
	keep := limit - len(prefix) - 1
	if keep <= 0 {
		return prefix
	}
	return prefix + "\n" + truncateUTF8Prefix(existing, keep)
}

type attachWriter struct {
	hub    *attach.Hub
	taskID string
}

func (w attachWriter) Write(b []byte) (int, error) {
	w.hub.WriteOutput(w.taskID, b)
	return len(b), nil
}

func launchWrapper(cmd *exec.Cmd, stateBase, scratch, taskID string, limit time.Duration, hub *attach.Hub) (_ *codingProcess, id workerIdentity, err error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, id, err
	}
	run := stateBase + ".run"
	if err = os.Mkdir(run, 0700); err != nil {
		return nil, id, err
	}
	defer func() {
		if err != nil {
			_ = os.RemoveAll(run)
		}
	}()
	data, err := json.Marshal(wrapperSpec{
		Args:      cmd.Args,
		Target:    sandboxTarget(cmd),
		Env:       cmd.Env,
		Dir:       cmd.Dir,
		Scratch:   scratch,
		StateBase: stateBase,
		Limit:     limit,
	})
	if err != nil {
		return nil, id, err
	}
	ready, child, err := os.Pipe()
	if err != nil {
		return nil, id, err
	}
	defer ready.Close() //nolint:errcheck // read end of the readiness pipe
	defer child.Close() //nolint:errcheck // acknowledged dropped error: the parent's copy of the readiness write end, closed by this defer on the returns before Start succeeds and explicitly after it (a second Close only reports os.ErrClosed); the parent never writes to it
	wrapper := exec.Command(exe, wrapperArg)
	wrapper.Dir = filepath.Dir(stateBase)
	wrapper.Env = []string{}
	wrapper.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	wrapper.Stdin = bytes.NewReader(data)
	wrapper.ExtraFiles = []*os.File{child}
	if hub != nil {
		hub.Register(taskID, nil)
		wrapper.Stdout = attachWriter{hub, taskID}
		defer func() {
			if err != nil {
				hub.Close(taskID)
			}
		}()
	}
	if err = wrapper.Start(); err != nil {
		return nil, id, err
	}
	_ = child.Close()
	var status readyStatus
	if err = json.NewDecoder(ready).Decode(&status); err != nil {
		_ = wrapper.Wait()
		return nil, id, err
	}
	if status.Error != "" {
		_ = wrapper.Wait()
		err = errors.New(status.Error)
		if status.NotExist {
			missingPath := status.MissingPath
			if missingPath == "" {
				missingPath = cmd.Path
			}
			err = &scruntime.MissingBinaryError{Path: missingPath, Err: err}
		}
		return nil, id, err
	}
	id = status.workerIdentity
	if id.PID != wrapper.Process.Pid || id.Start.IsZero() {
		_ = wrapper.Process.Signal(syscall.SIGTERM)
		_ = wrapper.Wait()
		return nil, id, errors.New("invalid coding wrapper identity")
	}
	p := &codingProcess{done: make(chan struct{}), leader: wrapper.Process}
	go func() {
		defer close(p.done)
		p.stopErr = wrapper.Wait()
		if hub != nil {
			hub.Close(taskID)
		}
	}()
	return p, id, nil
}

func checkWrappedTarget(target, dir string) error {
	info, err := os.Stat(target)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}
	if !info.Mode().IsRegular() {
		return nil
	}
	interp, ok := shebangInterpreter(target)
	if !ok {
		return nil
	}
	if !filepath.IsAbs(interp) && dir != "" {
		interp = filepath.Join(dir, interp)
	}
	if _, err := os.Stat(interp); errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func shebangInterpreter(path string) (string, bool) {
	f, err := os.Open(path)
	if err != nil {
		return "", false
	}
	defer f.Close() //nolint:errcheck // read-only file; its close error cannot lose data
	var buf [512]byte
	n, err := io.ReadFull(f, buf[:])
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return "", false
	}
	line := buf[:n]
	if len(line) < 2 || line[0] != '#' || line[1] != '!' {
		return "", false
	}
	line = line[2:]
	if end := bytes.IndexAny(line, "\r\n\x00"); end >= 0 {
		line = line[:end]
	} else if n == len(buf) {
		return "", false
	}
	if runtime.GOOS == "darwin" {
		// The Darwin kernel ends the interpreter line at '#' as well as at a newline.
		if end := bytes.IndexByte(line, '#'); end >= 0 {
			line = line[:end]
		}
	}
	trimmed := strings.TrimLeft(string(line), " \t")
	if trimmed == "" {
		return "", false
	}
	if i := strings.IndexAny(trimmed, " \t"); i >= 0 {
		trimmed = trimmed[:i]
	}
	if trimmed == "" {
		return "", false
	}
	return trimmed, true
}

func readBounded(path string, limit int) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close() //nolint:errcheck // read-only file; its close error cannot lose data
	b, err := io.ReadAll(io.LimitReader(f, int64(limit)+1))
	if len(b) > limit {
		return nil, errors.New("oversized coding worker record")
	}
	return b, err
}

func recordedExit(h scruntime.Handle) (*processResult, error) {
	return recordedExitAt(h.WorkDir, h)
}

func recordedExitAt(stateBase string, h scruntime.Handle) (*processResult, error) {
	dir := stateBase + ".run"
	raw, err := readBounded(filepath.Join(dir, "exit"), maxExitRecordBytes)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var rec exitRecord
	if err := json.Unmarshal(raw, &rec); err != nil || rec.Code == nil ||
		(*rec.Code < -1 && *rec.Code != wrapperLogCloseExitCode) || *rec.Code > 255 ||
		rec.PID != h.PID || rec.Start.IsZero() || !rec.Start.Equal(h.ProcessStartTime) {
		return nil, errors.New("invalid coding exit record")
	}
	diag, err := readBounded(filepath.Join(dir, "stderr"), maxDiagnosticTail+128)
	if err != nil {
		if rec.Reason == "" {
			return nil, err
		}
		diag = nil
	}
	msg := redact.String(string(diag))
	if rec.Reason != "" {
		msg = prependDiagnostic(redact.String(rec.Reason), msg)
	}
	result := &processResult{
		PID:      h.PID,
		Start:    rec.Start,
		ExitCode: *rec.Code,
		LogClose: rec.LogClose || *rec.Code == wrapperLogCloseExitCode,
		Message:  msg,
	}
	if result.ExitCode == 0 {
		out, err := readBounded(filepath.Join(dir, "stdout"), 64*1024)
		if err != nil {
			return nil, err
		}
		response := harnessResponse{}
		_, _ = response.Write(out)
		result.Response = response.text()
	}
	return result, nil
}

func stopVerified(ctx context.Context, h scruntime.Handle, stateBase string, state *worktreeState) error {
	alive, err := scruntime.ProcessAliveWithStartTime(h.PID, h.ProcessStartTime)
	if err != nil || !alive {
		return err
	}
	group, err := syscall.Getpgid(h.PID)
	if err != nil {
		return err
	}
	if group != h.PID {
		return errors.New("coding process is not the group leader")
	}
	if state.Worker == nil {
		// The legacy group has no wrapper to hold its identity during a
		// grace period. Terminate all members while the leader is verified.
		if err := signalAndWait(ctx, h.PID, syscall.SIGKILL); err != nil {
			return err
		}
		state.Result = &processResult{PID: h.PID, Start: h.ProcessStartTime, ExitCode: -1, Message: "coding task was stopped"}
		return writeWorktreeState(stateBase, state)
	}
	// The wrapper handles TERM, stops and reaps the harness group, then records exit.
	if err := syscall.Kill(-group, syscall.SIGTERM); err != nil && !errors.Is(err, syscall.ESRCH) {
		return err
	}
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		running, err := scruntime.ProcessAliveWithStartTime(h.PID, h.ProcessStartTime)
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
