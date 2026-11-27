package coding

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/shadewright-labs/sablecourt/internal/attach"
	"github.com/shadewright-labs/sablecourt/internal/gate/councilproxy"
	scruntime "github.com/shadewright-labs/sablecourt/internal/runtime"
)

type CouncilProxyLauncher interface {
	ProvisionCouncilProxy(context.Context, string, councilproxy.Config) (councilproxy.LaunchSession, error)
	RemoveProxy(context.Context, string) error
	RemoveInternalNetwork(context.Context, string) error
}

// Adapter runs coding CLIs (claude, codex or agy) against dedicated Git
// worktrees. Standalone tasks and council seats run in harness containers.
// The host launcher (stripped environment, detached process group, OS
// sandbox) runs only when HostModeUnsafe is set and no container runner is
// configured; such a run is outside invariants 2 and 3.
type Adapter struct {
	WorktreeRoot       string // Directory for worktrees and their state.
	DryRun             bool
	AttachHub          *attach.Hub
	BranchRetention    time.Duration     // Prune merged branches after this age on the next start.
	HarnessPaths       map[string]string // Absolute executable paths supplied by the operator.
	WallTime           time.Duration     // Zero uses the default task limit.
	CouncilRunner      scruntime.CouncilRunner
	CouncilProxy       CouncilProxyLauncher
	CouncilGateHandler http.Handler
	CouncilSocketRoot  string
	CouncilProxyImage  string
	DaemonListen       string
	CouncilLimits      scruntime.CouncilLimits
	// HostModeUnsafe allows the host launcher when no container runner is
	// configured. Off by default; the daemon sets it only from an explicit flag.
	HostModeUnsafe bool
	Logger         *slog.Logger // Receives the warning logged at each host launch.

	allowUnconfined bool // In-process fixture only.
	setupTimeout    time.Duration
	beforeTaskState func(taskID string) // Test hook invoked before writing task state.

	mu         sync.Mutex
	gates      map[string]*taskGate
	running    sync.Map // Worktree path to *codingProcess.
	containers sync.Map // Task base path to *containerProcess.
}

// SupportsCouncilContainers reports whether the adapter is configured to launch
// coding tasks and council turns inside hardened harness containers.
func (a *Adapter) SupportsCouncilContainers() bool {
	return a != nil && a.CouncilRunner != nil
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

func (a *Adapter) start(ctx context.Context, spec scruntime.LaunchSpec, commit string, readOnly bool) (handle scruntime.Handle, startErr error) {
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
	if commit != "" && input.checkoutMode != "" {
		return scruntime.Handle{}, errors.New("StartAtCommit cannot be combined with checkoutMode")
	}
	readOnly = readOnly || input.isReadOnly()
	limit := a.WallTime
	if limit == 0 {
		limit = defaultWallTime
	}
	if limit < 0 {
		return scruntime.Handle{}, errors.New("coding wall time must be positive")
	}
	if !a.DryRun {
		if spec.CouncilContainer {
			if err := a.validateCouncilContainerLaunch(spec, input); err != nil {
				return scruntime.Handle{}, err
			}
		} else {
			if err := a.hostLaunchAllowed(); err != nil {
				return scruntime.Handle{}, err
			}
			if err := a.validateSandbox(cfg.Sandbox, readOnly); err != nil {
				return scruntime.Handle{}, err
			}
		}
	}

	release, err := a.lockTask(ctx, spec.TaskID, false)
	if err != nil {
		return scruntime.Handle{}, err
	}
	defer release()

	var (
		dir           string
		taskBase      string
		state         *worktreeState
		rollback      func(context.Context) error
		commitJournal func() error
	)
	switch input.checkoutMode {
	case "":
		dir, state, err = a.prepareWorktree(ctx, cfg.RepoPath, spec.TaskID)
		if err != nil {
			return scruntime.Handle{}, err
		}
		taskBase = dir
		rollback = func(rbCtx context.Context) error { return rollbackWorktree(rbCtx, dir, state) }
	case checkoutModeLive:
		dir, taskBase, state, rollback, err = a.prepareLiveCheckout(ctx, cfg.RepoPath, spec.TaskID, input.checkoutSourceID)
		if err != nil {
			return scruntime.Handle{}, err
		}
	case checkoutModeRef:
		dir, taskBase, state, rollback, commitJournal, err = a.prepareRefCheckout(ctx, cfg.RepoPath, spec.TaskID, input)
		if err != nil {
			return scruntime.Handle{}, err
		}
	case checkoutModeWriter:
		dir, taskBase, state, rollback, commitJournal, err = a.prepareWriterCheckout(ctx, cfg.RepoPath, spec.TaskID, input)
		if err != nil {
			return scruntime.Handle{}, err
		}
	}
	defer func() {
		if startErr != nil && !isContainerHandle(handle) && rollback != nil {
			rbCtx, rbCancel := context.WithTimeout(context.WithoutCancel(ctx), a.councilSetupTimeout())
			defer rbCancel()
			startErr = errors.Join(startErr, rollback(rbCtx))
		}
	}()

	if input.checkoutMode == "" {
		if commit != "" && commit != state.Base {
			if err := pinWorktree(ctx, dir, state, commit); err != nil {
				return scruntime.Handle{}, err
			}
		}
		if err := writeSessionMarker(dir, spec, input); err != nil {
			return scruntime.Handle{}, err
		}
	}
	handle = scruntime.Handle{
		AdapterName: "coding:" + cfg.Harness,
		ExternalID:  spec.TaskID,
		WorkDir:     dir,
		Branch:      state.Branch,
	}
	if input.checkoutMode != "" {
		handle.LogPath = taskBase + ".log"
	}
	if a.DryRun {
		if commitJournal != nil {
			if err := commitJournal(); err != nil {
				return scruntime.Handle{}, err
			}
		}
		return handle, nil
	}

	if spec.CouncilContainer {
		return a.startCouncilContainer(ctx, spec, input, dir, taskBase, state, readOnly, commitJournal)
	}

	if input.checkoutMode == "" && input.context != "" {
		if err := writeSessionFile(dir, sessionContextFile, []byte(input.context)); err != nil {
			return scruntime.Handle{}, err
		}
	}
	scratch, err := os.MkdirTemp(filepath.Dir(taskBase), ".coding-scratch-")
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
		if err := checkSeatbelt(ctx, profile, cmd.Dir); err != nil {
			return scruntime.Handle{}, err
		}
	}
	if err := ctx.Err(); err != nil {
		return scruntime.Handle{}, err
	}

	if a.Logger != nil {
		a.Logger.Warn("UNSAFE: coding harness launched on the host, outside invariants 2 and 3", "task", spec.TaskID, "harness", cfg.Harness)
	}
	process, id, err := launchWrapper(cmd, taskBase, scratch, state.TaskID, limit, a.AttachHub)
	if err != nil {
		return scruntime.Handle{}, fmt.Errorf("start coding harness %q at %q: %w", cfg.Harness, sandboxTarget(cmd), err)
	}
	handle.PID, handle.ProcessStartTime = id.PID, id.Start
	a.running.Store(taskBase, process)
	if commitJournal != nil {
		_ = commitJournal()
	}
	return handle, nil
}

// ErrHostModeOff refuses a host launch when host mode was not opted in.
var ErrHostModeOff = fmt.Errorf("coding tasks run in harness containers only: no container runtime is configured and host mode is off (see sablectl doctor): %w", scruntime.ErrCodingRuntimeUnavailable)

// errHostWithContainers refuses a host launch while containers are available.
var errHostWithContainers = errors.New("coding host launch refused: harness containers are configured, so the task must run in a container")

// hostLaunchAllowed is the one check for a launch outside a container. Host
// mode needs the explicit opt-in and is used only when no container runner is
// configured, so a container never silently falls back to the host.
func (a *Adapter) hostLaunchAllowed() error {
	if a.SupportsCouncilContainers() {
		return errHostWithContainers
	}
	if !a.HostModeUnsafe && !a.allowUnconfined {
		return ErrHostModeOff
	}
	return nil
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
	if err := state.checkSessionExcludes(ctx, dir); err != nil {
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
	file, err := os.OpenFile(filepath.Join(dir, name), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o400)
	if err != nil {
		return fmt.Errorf("create session file: %w", err)
	}
	_, err = file.Write(data)
	return errors.Join(err, file.Close())
}

func (a *Adapter) finishTask(ctx context.Context, taskBase string, state *worktreeState) error {
	if state.Mode != "" {
		return a.finishCouncilTask(ctx, taskBase, state)
	}
	return a.finishWorktree(ctx, taskBase, state)
}

func (a *Adapter) Observe(ctx context.Context, handle scruntime.Handle) (scruntime.Observation, error) {
	release, err := a.lockTask(ctx, handle.ExternalID, false)
	if err != nil {
		return scruntime.Observation{}, err
	}
	defer release()
	state, err := a.worktreeState(handle)
	if err != nil {
		if isContainerHandle(handle) && errors.Is(err, os.ErrNotExist) {
			return a.observeOrphanContainer(ctx, handle)
		}
		return scruntime.Observation{}, err
	}
	taskBase, err := a.taskBase(handle.ExternalID)
	if err != nil {
		return scruntime.Observation{}, err
	}
	obs := scruntime.Observation{State: scruntime.StateRunning, ObservedAt: time.Now().UTC()}
	ready, err := a.loadResult(ctx, taskBase, handle, state)
	if err != nil || !ready {
		return obs, err
	}
	if err := a.finishTask(ctx, taskBase, state); err != nil {
		return scruntime.Observation{}, err
	}
	obs.State = scruntime.StateCompleted
	if state.Result != nil && state.Result.ExitCode != 0 {
		obs.State = scruntime.StateFailed
		obs.ExitCode = state.Result.ExitCode
		obs.Message = state.Result.Message
		obs.LogCloseFailed = state.Result.LogClose || state.Result.ExitCode == wrapperLogCloseExitCode
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
	if isContainerHandle(handle) {
		if taskBase, err := a.taskBase(handle.ExternalID); err == nil {
			if err := a.stopInMemoryContainer(ctx, taskBase, handle); err != nil {
				return err
			}
		}
	}
	release, err := a.lockTask(ctx, handle.ExternalID, false)
	if err != nil {
		return err
	}
	defer release()
	state, err := a.worktreeState(handle)
	if err != nil {
		if isContainerHandle(handle) && errors.Is(err, os.ErrNotExist) {
			return a.cleanupContainerResources(ctx, &containerIdentity{
				ContainerName: handle.ContainerName,
				CreateAttempt: handle.CreateAttempt,
				ContainerID:   handle.ContainerID,
				ProxyName:     handle.ProxyName,
				NetworkName:   handle.NetworkName,
			}, nil)
		}
		return err
	}
	taskBase, err := a.taskBase(handle.ExternalID)
	if err != nil {
		return err
	}
	if state.Container != nil || isContainerHandle(handle) {
		return a.stopCouncilContainer(ctx, taskBase, handle, state)
	}
	if !state.FinishedAt.IsZero() {
		return a.finishTask(ctx, taskBase, state)
	}
	if handle.PID > 0 {
		if err := stopVerified(ctx, handle, taskBase, state); err != nil {
			return err
		}
		if process := a.process(taskBase); process != nil {
			select {
			case <-process.done:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	}
	ready, err := a.loadResult(ctx, taskBase, handle, state)
	if err != nil {
		return err
	}
	if !ready {
		return errors.New("coding process is still running")
	}
	return a.finishTask(ctx, taskBase, state)
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
	taskBase, err := a.taskBase(handle.ExternalID)
	if err != nil {
		return nil, err
	}
	if state.FinishedAt.IsZero() {
		ready, err := a.loadResult(ctx, taskBase, handle, state)
		if err != nil {
			return nil, err
		}
		if !ready {
			return nil, errors.New("coding worktree is still running")
		}
	}
	if err := a.finishTask(ctx, taskBase, state); err != nil {
		return nil, err
	}
	if state.Mode == checkoutModeWriter && !state.ReadOnly && state.Result != nil && state.Result.ExitCode != 0 {
		return state.Artifacts, nil
	}
	return append(state.Artifacts, scruntime.Artifact{Name: "git-commit", ContentType: "text/plain", Text: state.Tip}), nil
}

func (a *Adapter) loadResult(ctx context.Context, taskBase string, handle scruntime.Handle, state *worktreeState) (bool, error) {
	if state.Container != nil {
		return a.loadContainerResult(ctx, taskBase, handle, state)
	}
	if handle.PID == 0 {
		return true, nil
	}
	if state.Result != nil && state.Result.PID == handle.PID && (state.Result.Start.IsZero() || state.Result.Start.Equal(handle.ProcessStartTime)) {
		return true, nil
	}
	alive, err := scruntime.ProcessAliveWithStartTime(handle.PID, handle.ProcessStartTime)
	if err != nil {
		return false, err
	}
	result, err := recordedExitAt(taskBase, handle)
	if err != nil {
		return false, err
	}
	if result != nil {
		state.Result = result
		return true, writeWorktreeState(taskBase, state)
	}
	if alive {
		return false, nil
	}
	return false, scruntime.ErrProcessNotFound
}

func (a *Adapter) process(dir string) *codingProcess {
	value, _ := a.running.Load(dir)
	process, _ := value.(*codingProcess)
	return process
}

func (a *Adapter) processForHandle(h scruntime.Handle) *codingProcess {
	if base, err := a.taskBase(h.ExternalID); err == nil {
		if p := a.process(base); p != nil {
			return p
		}
	}
	return a.process(h.WorkDir)
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
