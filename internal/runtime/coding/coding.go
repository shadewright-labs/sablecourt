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
	"path/filepath"
	goruntime "runtime"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/shadewright-labs/sablecourt/internal/attach"
	"github.com/shadewright-labs/sablecourt/internal/redact"
	scruntime "github.com/shadewright-labs/sablecourt/internal/runtime"
)

// Adapter runs local coding CLIs (claude or codex) inside dedicated Git worktrees
// with stripped environment variables, detached process groups, and OS sandboxing.
type Adapter struct {
	WorktreeRoot    string // Directory for worktrees and their state.
	DryRun          bool
	AttachHub       *attach.Hub
	BranchRetention time.Duration // Prune merged branches after this age on the next start.

	mu        sync.Mutex
	running   map[string]<-chan struct{}
	responses map[string]*harnessResponse
}

var _ scruntime.Adapter = (*Adapter)(nil)

func NewAdapter(worktreeRoot string) *Adapter {
	return &Adapter{WorktreeRoot: worktreeRoot}
}

func (a *Adapter) Start(ctx context.Context, spec scruntime.LaunchSpec) (scruntime.Handle, error) {
	return a.start(ctx, spec, "", false)
}

func (a *Adapter) StartAtCommit(ctx context.Context, spec scruntime.LaunchSpec, commit string, readOnly bool) (scruntime.Handle, error) {
	if !isCommitID(commit) {
		return scruntime.Handle{}, errors.New("checkout requires a full commit ID")
	}
	return a.start(ctx, spec, commit, readOnly)
}

func (a *Adapter) start(ctx context.Context, spec scruntime.LaunchSpec, commit string, readOnly bool) (_ scruntime.Handle, startErr error) {
	if spec.Workload.Spec.Coding == nil {
		return scruntime.Handle{}, errors.New("coding config is required")
	}
	cfg := spec.Workload.Spec.Coding
	if cfg.Harness != "claude" && cfg.Harness != "codex" && cfg.Harness != "agy" {
		return scruntime.Handle{}, fmt.Errorf("unsupported coding harness %q", cfg.Harness)
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	dir, state, err := a.prepareWorktree(ctx, cfg.RepoPath, spec.TaskID)
	if err != nil {
		return scruntime.Handle{}, err
	}
	defer func() {
		if startErr != nil {
			startErr = errors.Join(startErr, rollbackWorktree(context.WithoutCancel(ctx), dir, state))
		}
	}()

	if commit != "" && commit != state.Base {
		if err := pinWorktree(ctx, dir, state, commit); err != nil {
			return scruntime.Handle{}, err
		}
	}
	if err := writeSessionMarker(dir, spec); err != nil {
		return scruntime.Handle{}, err
	}
	handle := scruntime.Handle{
		AdapterName: "coding:" + cfg.Harness,
		ExternalID:  spec.TaskID,
		WorkDir:     dir,
		Branch:      state.Branch,
	}
	if a.DryRun {
		return handle, nil
	}

	var scratch string
	if readOnly {
		scratch, err = os.MkdirTemp("", "sablecourt-verifier-")
		if err != nil {
			return scruntime.Handle{}, err
		}
		defer func() {
			if startErr != nil {
				_ = os.RemoveAll(scratch)
			}
		}()
	}
	bin, argv, err := buildCommand(cfg.Harness, cfg.Sandbox, dir, spec.InputJSON, scratch)
	if err != nil {
		return scruntime.Handle{}, err
	}
	cmd := exec.Command(bin, argv...)
	cmd.Dir = dir
	cmd.Env = BuildStrippedEnv(spec)
	if scratch != "" {
		cmd.Env = append(cmd.Env, "TMPDIR="+scratch, "HOME="+scratch)
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.WaitDelay = processGracePeriod

	logFile, err := os.OpenFile(dir+".log", os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return scruntime.Handle{}, fmt.Errorf("open session log: %w", err)
	}
	defer func() {
		if startErr != nil {
			_ = logFile.Close()
			startErr = errors.Join(startErr, os.Remove(logFile.Name()))
		}
	}()
	output := &sessionOutput{file: logFile, hub: a.AttachHub, taskID: spec.TaskID}
	stderr := &sessionOutput{file: logFile, hub: a.AttachHub, taskID: spec.TaskID}
	response := &harnessResponse{}
	// Print mode and exec send the final answer to stdout and diagnostics to stderr.
	cmd.Stdout, cmd.Stderr = io.MultiWriter(output, response), stderr
	if err := ctx.Err(); err != nil {
		return scruntime.Handle{}, err
	}
	if output.hub != nil {
		stdin, err := cmd.StdinPipe()
		if err != nil {
			return scruntime.Handle{}, fmt.Errorf("stdin pipe: %w", err)
		}
		output.hub.Register(spec.TaskID, stdin)
		defer func() {
			if startErr != nil {
				output.hub.Close(spec.TaskID)
			}
		}()
	}
	if err := cmd.Start(); err != nil {
		return scruntime.Handle{}, fmt.Errorf("start coding harness %q: %w", cfg.Harness, err)
	}

	handle.PID = cmd.Process.Pid
	done := make(chan struct{})
	if a.running == nil {
		a.running = make(map[string]<-chan struct{})
		a.responses = make(map[string]*harnessResponse)
	}
	a.running[dir] = done
	a.responses[dir] = response
	go func() {
		defer close(done)
		if scratch != "" {
			defer os.RemoveAll(scratch)
		}
		if err := cmd.Wait(); err != nil {
			response.invalid = true
		}
		if err := errors.Join(output.Flush(), stderr.Flush()); err != nil {
			response.invalid = true
		}
		_ = logFile.Close()
		if output.hub != nil {
			output.hub.Close(spec.TaskID)
		}
	}()
	return handle, nil
}

func pinWorktree(ctx context.Context, dir string, state *worktreeState, commit string) error {
	checkout, err := state.checkoutGit(dir).withoutFilters(ctx)
	if err != nil {
		return err
	}
	id, err := checkout.commitID(ctx, commit)
	if err != nil || id != commit {
		return errors.New("author commit unavailable in configured repository")
	}
	reserved, err := checkout.run(ctx, "ls-tree", "--name-only", "--end-of-options", commit, "--", sessionMarkerFile, sessionContextFile)
	if err != nil {
		return err
	}
	if reserved != "" {
		return errors.New("author commit tracks a reserved session file")
	}
	if _, err := checkout.run(ctx, "read-tree", "--reset", "-u", "--end-of-options", commit); err != nil {
		return err
	}
	if _, err := checkout.run(ctx, "update-ref", "--end-of-options", "HEAD", commit, state.Base); err != nil {
		return err
	}
	state.Base = commit
	return writeWorktreeState(dir, state)
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
	if h.invalid {
		return ""
	}
	raw := h.buf.Bytes()
	starts := privateKeyStart.FindAllIndex(raw, -1)
	if len(starts) > 0 && !privateKeyEnd.Match(raw[starts[len(starts)-1][1]:]) {
		return ""
	}
	return redact.String(h.buf.String())
}

func writeSessionMarker(worktreeDir string, spec scruntime.LaunchSpec) error {
	var meta struct {
		ConclaveID string `json:"conclaveId"`
		Role       string `json:"role"`
	}
	_ = json.Unmarshal([]byte(spec.InputJSON), &meta)
	marker := map[string]any{
		"managed_by": "sablecourt",
		"task_id":    spec.TaskID,
		"workload":   spec.Workload.Metadata.Name,
		"zone":       string(spec.Workload.Metadata.Zone),
		"started_at": time.Now().UTC().Format(time.RFC3339),
	}
	if meta.ConclaveID != "" {
		marker["conclave_id"] = meta.ConclaveID
		marker["role"] = meta.Role
	}
	raw, err := json.MarshalIndent(marker, "", "  ")
	if err != nil {
		return err
	}
	return writeSessionFile(worktreeDir, sessionMarkerFile, raw)
}

func writeSessionFile(dir, name string, data []byte) error {
	file, err := os.OpenFile(filepath.Join(dir, name), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o444)
	if err != nil {
		return fmt.Errorf("create session file: %w", err)
	}
	_, err = file.Write(data)
	return errors.Join(err, file.Close())
}

func (a *Adapter) Observe(ctx context.Context, handle scruntime.Handle) (scruntime.Observation, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	obs := scruntime.Observation{State: scruntime.StateRunning, ObservedAt: time.Now().UTC()}
	state, err := a.worktreeState(handle)
	if err != nil {
		return scruntime.Observation{}, err
	}
	if state.FinishedAt.IsZero() {
		running, err := processGroupRunning(handle.PID)
		if err != nil {
			return scruntime.Observation{}, err
		}
		if running || !outputClosed(a.running[handle.WorkDir]) {
			return obs, nil
		}
	}
	if err := a.finishSession(ctx, handle.WorkDir, state); err != nil {
		return scruntime.Observation{}, err
	}
	obs.State = scruntime.StateCompleted
	return obs, nil
}

func (a *Adapter) Suspend(_ context.Context, _ scruntime.Handle) (scruntime.CheckpointRef, error) {
	return scruntime.CheckpointRef{}, errors.New("coding CLI adapter does not support live suspend")
}

func (a *Adapter) Resume(_ context.Context, _ scruntime.Handle, _ scruntime.CheckpointRef) error {
	return errors.New("coding CLI adapter does not support live resume")
}

func (a *Adapter) Stop(ctx context.Context, handle scruntime.Handle) error {
	a.mu.Lock()
	defer a.mu.Unlock()

	state, err := a.worktreeState(handle)
	if err != nil {
		return err
	}
	if state.FinishedAt.IsZero() {
		if err := stopProcessGroup(ctx, handle.PID, a.running[handle.WorkDir]); err != nil {
			return err
		}
	}
	return a.finishSession(ctx, handle.WorkDir, state)
}

func (a *Adapter) CollectArtifacts(ctx context.Context, handle scruntime.Handle) ([]scruntime.Artifact, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	state, err := a.worktreeState(handle)
	if err != nil {
		return nil, err
	}
	if state.FinishedAt.IsZero() {
		running, err := processGroupRunning(handle.PID)
		if err != nil {
			return nil, err
		}
		if running || !outputClosed(a.running[handle.WorkDir]) {
			return nil, errors.New("coding worktree is still running")
		}
	}
	if err := a.finishSession(ctx, handle.WorkDir, state); err != nil {
		return nil, err
	}
	return append(state.Artifacts, scruntime.Artifact{Name: "git-commit", ContentType: "text/plain", Text: state.Tip}), nil
}

// finishSession saves the successful harness response alongside the worktree artifacts.
func (a *Adapter) finishSession(ctx context.Context, dir string, state *worktreeState) error {
	if err := a.finishWorktree(ctx, dir, state); err != nil {
		return err
	}
	if response := a.responses[dir]; response != nil {
		if text := response.text(); text != "" {
			state.Artifacts = append(state.Artifacts, scruntime.Artifact{Name: "final-message", ContentType: "text/plain", Text: text})
			if err := writeWorktreeState(dir, state); err != nil {
				return err
			}
		}
		delete(a.responses, dir)
	}
	return nil
}

// BuildStrippedEnv constructs a minimal allowlisted environment with zero raw provider keys
// and sets read-only starter metadata so Nightloom can identify Sablecourt-managed sessions.
func BuildStrippedEnv(spec scruntime.LaunchSpec) []string {
	env := []string{
		"PATH=/usr/local/bin:/opt/homebrew/bin:/usr/bin:/bin",
		"LANG=en_US.UTF-8",
		"SABLECOURT_MANAGED=1",
		"SABLECOURT_TASK_ID=" + spec.TaskID,
		"SABLECOURT_WORKLOAD=" + spec.Workload.Metadata.Name,
	}
	if spec.ModelGateURL != "" && spec.ModelGateToken != "" {
		env = append(env,
			"SABLECOURT_MODEL_GATE_URL="+spec.ModelGateURL,
			"SABLECOURT_TASK_TOKEN="+spec.ModelGateToken,
		)
	}
	if spec.DataGateURL != "" && spec.TaskToken != "" {
		env = append(env, "SABLECOURT_DATA_GATE_URL="+spec.DataGateURL)
	}
	return env
}

// SeatbeltProfile returns a macOS sandbox-exec profile restricting writes to workDir and /tmp.
func SeatbeltProfile(workDir string) string {
	clean := filepath.Clean(workDir)
	return fmt.Sprintf(`(version 1)
(allow default)
(deny file-write*)
(allow file-write* (subpath %q) (subpath "/tmp") (subpath "/private/tmp"))
`, clean)
}

func buildCommand(harness, sandbox, workDir, prompt, scratch string) (string, []string, error) {
	effectivePrompt := prompt
	var modelOverride string
	var meta struct {
		ContextPacket string `json:"context_packet"`
		Prompt        string `json:"prompt"`
		Model         string `json:"model"`
	}
	if err := json.Unmarshal([]byte(prompt), &meta); err == nil {
		if strings.TrimSpace(meta.ContextPacket) != "" {
			effectivePrompt = meta.ContextPacket
			if workDir != "" {
				if err := writeSessionFile(workDir, sessionContextFile, []byte(meta.ContextPacket)); err != nil {
					return "", nil, err
				}
			}
		} else if strings.TrimSpace(meta.Prompt) != "" {
			effectivePrompt = meta.Prompt
		}
		modelOverride = strings.TrimSpace(meta.Model)
	}

	var baseBin string
	var baseArgs []string
	switch harness {
	case "claude":
		baseBin = "claude"
		if modelOverride != "" {
			baseArgs = []string{"--model", modelOverride, "-p", effectivePrompt}
		} else {
			baseArgs = []string{"-p", effectivePrompt}
		}
	case "codex":
		baseBin = "codex"
		if modelOverride != "" {
			baseArgs = []string{"exec", "-m", modelOverride, effectivePrompt}
		} else {
			baseArgs = []string{"exec", effectivePrompt}
		}
	case "agy":
		baseBin = "agy"
		if modelOverride != "" {
			baseArgs = []string{"--model", modelOverride, "-p", effectivePrompt}
		} else {
			baseArgs = []string{"-p", effectivePrompt}
		}
	default:
		return "", nil, fmt.Errorf("unsupported harness %q", harness)
	}

	if scratch != "" {
		bin, err := exec.LookPath(baseBin)
		if err != nil {
			return "", nil, err
		}
		switch {
		case goruntime.GOOS == "darwin" && (sandbox == "" || sandbox == "seatbelt"):
			profile := fmt.Sprintf(`(version 1)(allow default)(deny file-write*)(allow file-write* (subpath %q) (literal "/dev/null"))`, scratch)
			return "/usr/bin/sandbox-exec", append([]string{"-p", profile, bin}, baseArgs...), nil
		case goruntime.GOOS == "linux" && (sandbox == "" || sandbox == "bubblewrap"):
			args := []string{"--die-with-parent", "--unshare-pid", "--ro-bind", "/", "/", "--proc", "/proc", "--dev", "/dev", "--bind", scratch, scratch, "--", bin}
			return "bwrap", append(args, baseArgs...), nil
		default:
			return "", nil, errors.New("read-only verifier sandbox unavailable")
		}
	}
	switch sandbox {
	case "seatbelt":
		if goruntime.GOOS == "darwin" {
			args := append([]string{"-p", SeatbeltProfile(workDir), baseBin}, baseArgs...)
			return "/usr/bin/sandbox-exec", args, nil
		}
	case "bubblewrap":
		if goruntime.GOOS == "linux" {
			args := append([]string{"--ro-bind", "/", "/", "--bind", workDir, workDir, "--tmpfs", "/tmp", "--", baseBin}, baseArgs...)
			return "bwrap", args, nil
		}
	}
	return baseBin, baseArgs, nil
}

func sanitizeSegment(s string) string {
	var b strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	if b.Len() == 0 {
		return "task"
	}
	return b.String()
}
