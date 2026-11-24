package coding

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/shadewright-labs/sablecourt/internal/attach"
	"github.com/shadewright-labs/sablecourt/internal/redact"
	scruntime "github.com/shadewright-labs/sablecourt/internal/runtime"
)

const wrapperArg = "--sablecourt-coding-wrapper"

type wrapperSpec struct {
	Args         []string
	Env          []string
	Dir, Scratch string
	Limit        time.Duration
}

type workerIdentity struct {
	PID   int
	Start time.Time
}

type exitRecord struct {
	workerIdentity
	Code *int
}

func init() {
	if len(os.Args) == 2 && os.Args[1] == wrapperArg {
		if err := runWrapper(); err != nil {
			os.Exit(1)
		}
		os.Exit(0)
	}
}

// The harness receives the original sandbox command and its stripped environment.
// Only its parent opens the sibling state directory and publishes the exit.
func runWrapper() error {
	var spec wrapperSpec
	if err := json.NewDecoder(os.Stdin).Decode(&spec); err != nil {
		return err
	}
	if len(spec.Args) == 0 {
		return errors.New("missing coding command")
	}
	dir, err := os.Getwd()
	if err != nil {
		return err
	}
	run := dir + ".run"
	start, err := scruntime.ProcessStartTime(os.Getpid())
	if err != nil {
		return err
	}
	id := workerIdentity{os.Getpid(), start}
	state, err := readWorktreeState(dir + stateSuffix)
	if err != nil {
		return err
	}
	state.Worker = &id
	if err := writeWorktreeState(dir, state); err != nil {
		return err
	}
	ready := os.NewFile(3, "ready")
	defer ready.Close()
	syscall.CloseOnExec(3)
	// A live attach reader may disappear when the daemon exits.
	signal.Ignore(syscall.SIGPIPE)
	term := make(chan os.Signal, 1)
	signal.Notify(term, syscall.SIGTERM)
	defer signal.Stop(term)
	cmd := exec.Command(spec.Args[0], spec.Args[1:]...)
	cmd.Dir, cmd.Env = spec.Dir, spec.Env
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stdout, err := os.OpenFile(filepath.Join(run, "stdout"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer stdout.Close()
	stderr, err := os.OpenFile(filepath.Join(run, "stderr"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer stderr.Close()
	p, err := startProcess(cmd, dir, spec.Scratch, spec.Limit, os.Stdout, &boundedLog{file: stdout}, &boundedLog{file: stderr})
	if err != nil {
		return err
	}
	if err := json.NewEncoder(ready).Encode(id); err != nil {
		p.requestStop()
	}
	select {
	case <-term:
		p.requestStop()
		<-p.done
	case <-p.done:
	}
	if p.stopErr != nil {
		return p.stopErr
	}
	// Finalize the response and diagnostic tail before making the exit visible.
	if p.result.ExitCode == 0 {
		if len(p.result.Response) > 64*1024 {
			p.result.Response = ""
		}
		if err := os.WriteFile(stdout.Name(), []byte(p.result.Response), 0600); err != nil {
			return err
		}
	}
	if err := os.WriteFile(filepath.Join(run, "stderr"), []byte(p.result.Message), 0600); err != nil {
		return err
	}
	raw, err := json.Marshal(exitRecord{id, &p.result.ExitCode})
	if err != nil {
		return err
	}
	tmp := filepath.Join(run, "exit.tmp")
	if err := os.WriteFile(tmp, raw, 0600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(run, "exit"))
}

type attachWriter struct {
	hub    *attach.Hub
	taskID string
}

func (w attachWriter) Write(b []byte) (int, error) {
	w.hub.WriteOutput(w.taskID, b)
	return len(b), nil
}

func launchWrapper(cmd *exec.Cmd, dir, scratch, taskID string, limit time.Duration, hub *attach.Hub) (_ *codingProcess, id workerIdentity, err error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, id, err
	}
	run := dir + ".run"
	if err = os.Mkdir(run, 0700); err != nil {
		return nil, id, err
	}
	defer func() {
		if err != nil {
			_ = os.RemoveAll(run)
		}
	}()
	data, err := json.Marshal(wrapperSpec{cmd.Args, cmd.Env, cmd.Dir, scratch, limit})
	if err != nil {
		return nil, id, err
	}
	ready, child, err := os.Pipe()
	if err != nil {
		return nil, id, err
	}
	defer ready.Close()
	defer child.Close()
	wrapper := exec.Command(exe, wrapperArg)
	wrapper.Dir = dir
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
	if err = json.NewDecoder(ready).Decode(&id); err != nil {
		_ = wrapper.Wait()
		return nil, id, err
	}
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

func readBounded(path string, limit int) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, int64(limit)+1))
	if len(b) > limit {
		return nil, errors.New("oversized coding worker record")
	}
	return b, err
}

func recordedExit(h scruntime.Handle) (*processResult, error) {
	dir := h.WorkDir + ".run"
	raw, err := readBounded(filepath.Join(dir, "exit"), 512)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var rec exitRecord
	if err := json.Unmarshal(raw, &rec); err != nil || rec.Code == nil || *rec.Code < -1 || *rec.Code > 255 || rec.PID != h.PID || rec.Start.IsZero() || !rec.Start.Equal(h.ProcessStartTime) {
		return nil, errors.New("invalid coding exit record")
	}
	diag, err := readBounded(filepath.Join(dir, "stderr"), maxDiagnosticTail+128)
	if err != nil {
		return nil, err
	}
	result := &processResult{PID: h.PID, Start: rec.Start, ExitCode: *rec.Code, Message: redact.String(string(diag))}
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

func stopVerified(ctx context.Context, h scruntime.Handle, state *worktreeState) error {
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
		return writeWorktreeState(h.WorkDir, state)
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
