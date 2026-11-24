package coding

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/shadewright-labs/sablecourt/internal/attach"
	scruntime "github.com/shadewright-labs/sablecourt/internal/runtime"
)

// Adapter runs local coding CLIs (claude or codex) inside dedicated Git worktrees
// with stripped environment variables, detached process groups, and OS sandboxing.
type Adapter struct {
	WorktreeRoot    string // Directory for worktrees and their state.
	DryRun          bool
	AttachHub       *attach.Hub
	BranchRetention time.Duration     // Prune merged branches after this age on the next start.
	HarnessPaths    map[string]string // Absolute executable paths supplied by the operator.
	WallTime        time.Duration     // Zero uses the default task limit.

	allowUnconfined bool // In-process fixture only.

	mu      sync.Mutex
	gates   map[string]*taskGate
	running sync.Map // Worktree path to *codingProcess.
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

	input, err := parseLaunchInput(spec.InputJSON)
	if err != nil {
		return scruntime.Handle{}, err
	}
	limit := a.WallTime
	if limit == 0 {
		limit = defaultWallTime
	}
	if limit < 0 {
		return scruntime.Handle{}, errors.New("coding wall time must be positive")
	}
	if !a.DryRun {
		if err := a.validateSandbox(cfg.Sandbox, readOnly); err != nil {
			return scruntime.Handle{}, err
		}
	}

	release, err := a.lockTask(ctx, spec.TaskID, false)
	if err != nil {
		return scruntime.Handle{}, err
	}
	defer release()

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
	if err := writeSessionMarker(dir, spec, input); err != nil {
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

	if input.context != "" {
		if err := writeSessionFile(dir, sessionContextFile, []byte(input.context)); err != nil {
			return scruntime.Handle{}, err
		}
	}
	scratch, err := os.MkdirTemp(filepath.Dir(dir), ".coding-scratch-")
	if err != nil {
		return scruntime.Handle{}, err
	}
	defer func() {
		if startErr != nil {
			startErr = errors.Join(startErr, os.RemoveAll(scratch))
		}
	}()

	cmd, profile, err := a.buildCommand(spec, input, dir, scratch, state, readOnly)
	if err != nil {
		return scruntime.Handle{}, err
	}
	if profile != "" {
		if err := checkSeatbelt(ctx, profile, dir); err != nil {
			return scruntime.Handle{}, err
		}
	}
	if err := ctx.Err(); err != nil {
		return scruntime.Handle{}, err
	}

	process, err := startProcess(cmd, dir, scratch, state, limit, a.AttachHub)
	if err != nil {
		return scruntime.Handle{}, fmt.Errorf("start coding harness %q at %q: %w", cfg.Harness, cmd.Path, err)
	}
	handle.PID = cmd.Process.Pid
	a.running.Store(dir, process)
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

func writeSessionMarker(worktreeDir string, spec scruntime.LaunchSpec, input launchInput) error {
	marker := map[string]any{
		"managed_by": "sablecourt",
		"task_id":    spec.TaskID,
		"workload":   spec.Workload.Metadata.Name,
		"zone":       string(spec.Workload.Metadata.Zone),
		"started_at": time.Now().UTC().Format(time.RFC3339),
	}
	if input.conclaveID != "" {
		marker["conclave_id"] = input.conclaveID
		marker["role"] = input.role
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
	release, err := a.lockTask(ctx, handle.ExternalID, false)
	if err != nil {
		return scruntime.Observation{}, err
	}
	defer release()
	state, err := a.worktreeState(handle)
	if err != nil {
		return scruntime.Observation{}, err
	}
	obs := scruntime.Observation{State: scruntime.StateRunning, ObservedAt: time.Now().UTC()}
	ready, err := a.loadResult(handle, state)
	if err != nil || !ready {
		return obs, err
	}
	if err := a.finishWorktree(ctx, handle.WorkDir, state); err != nil {
		return scruntime.Observation{}, err
	}
	obs.State = scruntime.StateCompleted
	if state.Result != nil && state.Result.ExitCode != 0 {
		obs.State, obs.ExitCode, obs.Message = scruntime.StateFailed, state.Result.ExitCode, state.Result.Message
	}
	return obs, nil
}

func (a *Adapter) Suspend(_ context.Context, _ scruntime.Handle) (scruntime.CheckpointRef, error) {
	return scruntime.CheckpointRef{}, errors.New("coding CLI adapter does not support live suspend")
}

func (a *Adapter) Resume(_ context.Context, _ scruntime.Handle, _ scruntime.CheckpointRef) error {
	return errors.New("coding CLI adapter does not support live resume")
}

func (a *Adapter) Stop(ctx context.Context, handle scruntime.Handle) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
	defer cancel()
	release, err := a.lockTask(ctx, handle.ExternalID, false)
	if err != nil {
		return err
	}
	defer release()
	state, err := a.worktreeState(handle)
	if err != nil {
		return err
	}
	if !state.FinishedAt.IsZero() {
		return a.finishWorktree(ctx, handle.WorkDir, state)
	}
	if process := a.process(handle.WorkDir); process != nil {
		process.requestStop()
		select {
		case <-process.done:
		case <-ctx.Done():
			return ctx.Err()
		}
		if process.stopErr != nil {
			if err := process.shutdown(ctx); err != nil {
				return err
			}
			process.stopErr = nil
		}
	}
	if _, err := a.loadResult(handle, state); err != nil {
		return err
	}
	return a.finishWorktree(ctx, handle.WorkDir, state)
}

func (a *Adapter) CollectArtifacts(ctx context.Context, handle scruntime.Handle) ([]scruntime.Artifact, error) {
	release, err := a.lockTask(ctx, handle.ExternalID, false)
	if err != nil {
		return nil, err
	}
	defer release()
	state, err := a.worktreeState(handle)
	if err != nil {
		return nil, err
	}
	if state.FinishedAt.IsZero() {
		ready, err := a.loadResult(handle, state)
		if err != nil {
			return nil, err
		}
		if !ready {
			return nil, errors.New("coding worktree is still running")
		}
	}
	if err := a.finishWorktree(ctx, handle.WorkDir, state); err != nil {
		return nil, err
	}
	return append(state.Artifacts, scruntime.Artifact{Name: "git-commit", ContentType: "text/plain", Text: state.Tip}), nil
}

// loadResult waits for the local waiter to finish publishing before using its result.
func (a *Adapter) loadResult(handle scruntime.Handle, state *worktreeState) (bool, error) {
	if process := a.process(handle.WorkDir); process != nil {
		if !outputClosed(process.done) {
			return false, nil
		}
		if process.stopErr != nil {
			return false, process.stopErr
		}
		if err := process.cleanScratch(); err != nil {
			return false, err
		}
		if state.Result == nil {
			state.Result = &process.result
			if err := writeWorktreeState(handle.WorkDir, state); err != nil {
				return false, err
			}
		}
	}
	if handle.PID != 0 && (state.Result == nil || state.Result.PID != handle.PID) {
		return false, errors.New("coding process exit was not recorded")
	}
	return true, nil
}

func (a *Adapter) process(dir string) *codingProcess {
	value, _ := a.running.Load(dir)
	process, _ := value.(*codingProcess)
	return process
}

type taskGate struct {
	token chan struct{}
	users int
}

// Only operations on the same worktree wait for each other.
func (a *Adapter) lockTask(ctx context.Context, id string, try bool) (func(), error) {
	return a.lockScope(ctx, "task:"+sanitizeSegment(id), try)
}

func (a *Adapter) lockScope(ctx context.Context, key string, try bool) (func(), error) {
	a.mu.Lock()
	if a.gates == nil {
		a.gates = make(map[string]*taskGate)
	}
	gate := a.gates[key]
	if gate == nil {
		gate = &taskGate{token: make(chan struct{}, 1)}
		a.gates[key] = gate
	}
	gate.users++
	a.mu.Unlock()

	drop := func() {
		a.mu.Lock()
		defer a.mu.Unlock()
		gate.users--
		if gate.users == 0 {
			delete(a.gates, key)
		}
	}
	release := func() {
		<-gate.token
		drop()
	}
	if try {
		select {
		case gate.token <- struct{}{}:
			return release, nil
		default:
			drop()
			return nil, errors.New("coding worktree is busy")
		}
	}
	select {
	case gate.token <- struct{}{}:
		return release, nil
	case <-ctx.Done():
		drop()
		return nil, ctx.Err()
	}
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
