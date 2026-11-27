package coding

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/shadewright-labs/sablecourt/internal/gate/councilproxy"
	"github.com/shadewright-labs/sablecourt/internal/redact"
	scruntime "github.com/shadewright-labs/sablecourt/internal/runtime"
)

const (
	defaultCouncilNanoCPUs    = int64(2_000_000_000)
	defaultCouncilMemoryBytes = int64(2 << 30)
	defaultCouncilPIDs        = int64(256)
)

type containerIdentity struct {
	ContainerName string `json:"container_name"`
	CreateAttempt string `json:"create_attempt"`
	ContainerID   string `json:"container_id,omitempty"`
	ProxyName     string `json:"proxy_name,omitempty"`
	NetworkName   string `json:"network_name,omitempty"`
}

type containerProcess struct {
	identity containerIdentity
	mu       sync.Mutex
	done     chan struct{}
	cancel   context.CancelFunc
	stopped  bool
	session  councilproxy.LaunchSession
}

func isContainerHandle(h scruntime.Handle) bool {
	return h.ContainerName != "" || h.ContainerID != ""
}

func matchesContainerHandle(c *containerIdentity, h scruntime.Handle) bool {
	if c == nil {
		return false
	}
	if h.ContainerID != "" && h.ContainerID != c.ContainerID {
		return false
	}
	if h.ContainerName != "" && h.ContainerName != c.ContainerName {
		return false
	}
	if h.CreateAttempt != "" && h.CreateAttempt != c.CreateAttempt {
		return false
	}
	return h.ContainerID != "" || (h.ContainerName != "" && h.CreateAttempt != "")
}

func (a *Adapter) containerProc(taskBase string) *containerProcess {
	v, _ := a.containers.Load(taskBase)
	p, _ := v.(*containerProcess)
	return p
}

func (a *Adapter) validateCouncilContainerLaunch(spec scruntime.LaunchSpec, input launchInput) error {
	if a.CouncilRunner == nil {
		return errors.New("council container runner unavailable")
	}
	if a.CouncilProxy == nil {
		return errors.New("council proxy engine unavailable")
	}
	if strings.TrimSpace(a.CouncilProxyImage) == "" {
		return errors.New("council proxy image is required")
	}
	if strings.TrimSpace(a.DaemonListen) == "" {
		return errors.New("council daemon listen address is required")
	}
	if spec.TaskToken == "" {
		return errors.New("council container requires a scoped task lease")
	}
	switch input.checkoutMode {
	case "", checkoutModeLive, checkoutModeRef, checkoutModeWriter:
	default:
		return errors.New("harness container requires a known checkout mode")
	}
	// The reconciler sets CouncilDeadline only for a council round, so the
	// deadline, not the input, says whether this launch is a seat. A seat needs
	// a council checkout; any other task gets only its own worktree.
	if input.checkoutMode == "" && !spec.CouncilDeadline.IsZero() {
		return errors.New("council container requires an explicit council checkout mode")
	}
	if input.checkoutMode != "" && spec.CouncilDeadline.IsZero() {
		return errors.New("council checkout mode requires a council seat")
	}
	if _, err := a.councilLimits(spec.CouncilDeadline); err != nil {
		return err
	}
	return nil
}

func (a *Adapter) councilLimits(deadline time.Time) (scruntime.CouncilLimits, error) {
	l := a.CouncilLimits
	if l.NanoCPUs <= 0 {
		l.NanoCPUs = defaultCouncilNanoCPUs
	}
	if l.MemoryBytes <= 0 {
		l.MemoryBytes = defaultCouncilMemoryBytes
	}
	if l.PIDs <= 0 {
		l.PIDs = defaultCouncilPIDs
	}
	if l.WallTime <= 0 {
		if a.WallTime > 0 {
			l.WallTime = a.WallTime
		} else {
			l.WallTime = defaultWallTime
		}
	}
	if !deadline.IsZero() {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return scruntime.CouncilLimits{}, errors.New("council deadline reached")
		}
		if remaining < l.WallTime {
			l.WallTime = remaining
		}
	}
	return l, nil
}

func councilCheckoutKind(mode string, readOnly bool) (scruntime.CouncilCheckoutKind, error) {
	switch mode {
	case "":
		// A standalone task: its own worktree, or a pinned read-only checkout for a verifier.
		if readOnly {
			return scruntime.CouncilReview, nil
		}
		return scruntime.CouncilTask, nil
	case checkoutModeLive:
		return scruntime.CouncilLive, nil
	case checkoutModeRef:
		return scruntime.CouncilRef, nil
	case checkoutModeWriter:
		if readOnly {
			return scruntime.CouncilReview, nil
		}
		return scruntime.CouncilWriter, nil
	default:
		return "", errors.New("unsupported council checkout mode")
	}
}

// harnessGitDirPath is the per-launch Git metadata directory of a task. The
// path is reserved with the task's other files before any checkout exists.
func harnessGitDirPath(taskBase string) string { return taskBase + ".git" }

// prepareHarnessGitDir gives a harness container Git metadata it can resolve.
// A linked worktree's .git file points at its registration in the shared
// repository, and the container never mounts that registration or the shared
// object store: other branches' refs and objects would be readable and, for a
// writable seat, rewritable. The launch gets its own directory instead, built
// the same way as a live checkout's (objects reachable from HEAD, HEAD, the
// checkout's index and info/exclude). The workload may change it; the daemon
// never runs Git against it, finishes from the host registration and the
// checkout files, and removes the directory when the task finishes. A live
// checkout is a primary worktree whose .git is mounted with it, so it needs
// none.
//
// The container user (10001) is not the daemon user on native Linux Docker,
// so the directory is made readable by every user (directories 0755, files
// 0644) once its parent is checked to be a directory the daemon user owns
// that grants nothing to group or others: the container runtime reaches the
// bind source as root, and no other host user can reach it through that
// parent. It is not made writable; see the uid 10001 limit on the harness
// containers page.
func prepareHarnessGitDir(ctx context.Context, dir, taskBase string, state *worktreeState) (string, error) {
	if state.Mode == checkoutModeLive {
		return "", nil
	}
	gitDir := harnessGitDirPath(taskBase)
	if err := checkPrivateParent(filepath.Dir(gitDir)); err != nil {
		return "", err
	}
	checkout := state.checkoutGit(dir)
	head, err := checkout.commitID(ctx, "HEAD")
	if err != nil {
		return "", err
	}
	if err := createLiveGitDir(ctx, checkout, gitDir, head); err != nil {
		return "", fmt.Errorf("create harness Git metadata: %w", err)
	}
	if err := openToHarnessUser(gitDir); err != nil {
		return "", errors.Join(fmt.Errorf("open harness Git metadata to the container user: %w", err), os.RemoveAll(gitDir))
	}
	return gitDir, nil
}

// checkPrivateParent refuses a directory that is a symlink, is owned by
// another user or grants any permission to group or others.
func checkPrivateParent(dir string) error {
	info, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !info.IsDir() || !ok || int64(st.Uid) != int64(os.Geteuid()) || info.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("harness Git metadata parent %s must be a directory owned by the daemon user with mode 0700", filepath.Base(dir))
	}
	return nil
}

// openToHarnessUser adds read permission for every user to each file, and
// read and search permission to each directory, below dir. It refuses a
// symlink or any other kind of file, which the daemon never writes there.
func openToHarnessUser(dir string) error {
	return filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		switch {
		case info.IsDir():
			return os.Chmod(path, info.Mode().Perm()|0o055)
		case info.Mode().IsRegular():
			return os.Chmod(path, info.Mode().Perm()|0o044)
		default:
			return fmt.Errorf("unexpected file type %v in harness Git metadata", info.Mode().Type())
		}
	})
}

func (a *Adapter) resolveProxyImageID(ctx context.Context) (string, error) {
	ref := strings.TrimSpace(a.CouncilProxyImage)
	if ref == "" {
		return "", errors.New("council proxy image is required")
	}
	if resolver, ok := a.CouncilRunner.(scruntime.LocalImageResolver); ok {
		return resolver.ResolveLocalImage(ctx, ref)
	}
	if strings.HasPrefix(ref, "sha256:") {
		return ref, nil
	}
	return "", errors.New("council proxy image requires local image ID resolution")
}

func leaseSHA256Hex(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func (a *Adapter) startCouncilContainer(
	ctx context.Context,
	spec scruntime.LaunchSpec,
	input launchInput,
	dir string,
	taskBase string,
	gitDir string,
	state *worktreeState,
	readOnly bool,
	commitJournal func() error,
) (scruntime.Handle, error) {
	cfg := spec.Workload.Spec.Coding
	kind, err := councilCheckoutKind(input.checkoutMode, readOnly)
	if err != nil {
		return scruntime.Handle{}, err
	}
	limits, err := a.councilLimits(spec.CouncilDeadline)
	if err != nil {
		return scruntime.Handle{}, err
	}
	proxyImageID, err := a.resolveProxyImageID(ctx)
	if err != nil {
		return scruntime.Handle{}, fmt.Errorf("resolve council proxy image: %w", err)
	}
	attempt := spec.CreateAttempt
	if attempt == "" {
		attempt = rand.Text()
	}
	handle := scruntime.Handle{
		AdapterName:   "coding:" + cfg.Harness,
		ExternalID:    spec.TaskID,
		WorkDir:       dir,
		Branch:        state.Branch,
		LogPath:       taskBase + ".log",
		ContainerName: "sablecourt-council-" + spec.TaskID,
		CreateAttempt: attempt,
	}
	deadline := time.Now().Add(limits.WallTime)
	if !spec.CouncilDeadline.IsZero() && spec.CouncilDeadline.Before(deadline) {
		deadline = spec.CouncilDeadline
	}
	setupCtx, cancelSetup := context.WithDeadline(ctx, deadline)
	defer cancelSetup()
	session, err := a.CouncilProxy.ProvisionCouncilProxy(setupCtx, proxyImageID, councilproxy.Config{
		Harness: cfg.Harness, DaemonListen: a.DaemonListen, LeaseSHA256: leaseSHA256Hex(spec.TaskToken),
		GateHandler: a.CouncilGateHandler, SocketRoot: a.CouncilSocketRoot,
		RecordResources: func(proxy, network string) error {
			handle.ProxyName, handle.NetworkName = proxy, network
			state.Container = &containerIdentity{ContainerName: handle.ContainerName, CreateAttempt: attempt, ProxyName: proxy, NetworkName: network}
			if err := writeWorktreeState(taskBase, state); err != nil {
				return err
			}
			if commitJournal != nil {
				return commitJournal()
			}
			return nil
		},
	})
	proc := &containerProcess{identity: containerIdentity{ContainerName: handle.ContainerName, CreateAttempt: attempt}, done: make(chan struct{}), session: session}
	if session == nil && err == nil {
		err = errors.New("missing council proxy session")
	}
	if err != nil {
		if session != nil {
			close(proc.done)
			a.containers.Store(taskBase, proc)
			return handle, fmt.Errorf("provision council proxy: %w", err)
		}
		// The socket probe can fail before Provision creates a session.
		if handle.ProxyName != "" {
			setupCleanup, cancelCleanup := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
			defer cancelCleanup()
			if cleanupErr := a.cleanupContainerResources(setupCleanup, state.Container, nil); cleanupErr != nil {
				return handle, errors.Join(err, cleanupErr)
			}
		}
		return scruntime.Handle{}, fmt.Errorf("provision council proxy: %w", err)
	}
	endpoint := session.Workload()
	if (endpoint.GateMode != councilproxy.GateModeUnixSocket && endpoint.GateMode != councilproxy.GateModeHostGateway) ||
		state.Container == nil || endpoint.Network != handle.NetworkName || endpoint.Network == "" ||
		endpoint.ProxyURL != "http://"+handle.ProxyName+":"+councilproxy.Port || endpoint.DataGateURL != endpoint.ProxyURL+"/gate/mcp" {
		close(proc.done)
		a.containers.Store(taskBase, proc)
		return handle, errors.New("council proxy returned an unverified endpoint")
	}

	limits.WallTime = time.Until(deadline)
	if err := setupCtx.Err(); err != nil {
		close(proc.done)
		a.containers.Store(taskBase, proc)
		return handle, err
	}
	runCtx, runCancel := context.WithDeadline(context.Background(), deadline)
	proc.cancel = runCancel
	startedCh := make(chan scruntime.CouncilIdentity)
	handedOff := make(chan struct{})
	launch := scruntime.CouncilLaunch{
		TaskID:        spec.TaskID,
		Workload:      spec.Workload.Metadata.Name,
		Harness:       cfg.Harness,
		Model:         input.model,
		Checkout:      scruntime.CouncilCheckout{Path: dir, Kind: kind, GitDir: gitDir},
		Limits:        limits,
		Prompt:        input.prompt,
		TaskToken:     spec.TaskToken,
		Network:       endpoint.Network,
		NetworkID:     endpoint.NetworkID,
		ProxyURL:      endpoint.ProxyURL,
		DataGateURL:   endpoint.DataGateURL,
		CreateAttempt: attempt,
		OnCreated: func(_ context.Context, id scruntime.CouncilIdentity) error {
			state.Container.ContainerID = id.ContainerID
			state.Container.ContainerName = id.ContainerName
			state.Container.CreateAttempt = id.CreateAttempt
			return writeWorktreeState(taskBase, state)
		},
		OnStarted: func(_ context.Context, id scruntime.CouncilIdentity) error {
			select {
			case startedCh <- id:
				<-handedOff
			case <-runCtx.Done():
				return runCtx.Err()
			}
			return nil
		},
	}

	var runErr error
	recorded := false
	recorder := func(recCtx context.Context, res scruntime.CouncilResult) error {
		release, err := a.lockTask(recCtx, spec.TaskID, false)
		if err != nil {
			return err
		}
		defer release()
		cur, err := readWorktreeState(taskBase + stateSuffix)
		if err != nil {
			return err
		}
		if cur.Container == nil {
			cur.Container = &containerIdentity{
				ContainerName: res.ContainerName,
				CreateAttempt: res.CreateAttempt,
				ProxyName:     handle.ProxyName,
				NetworkName:   endpoint.Network,
			}
		}
		if res.ContainerID != "" {
			cur.Container.ContainerID = res.ContainerID
		}
		cur.Result = buildContainerProcessResult(res)
		if err := writeContainerLog(taskBase, res); err != nil {
			return err
		}
		if err := writeWorktreeState(taskBase, cur); err != nil {
			return err
		}
		recorded = true
		return nil
	}

	go func() {
		defer close(proc.done)
		defer runCancel()
		res, err := a.CouncilRunner.RunCouncil(runCtx, launch, recorder)
		runErr = err
		select {
		case <-handedOff:
			if !recorded {
				a.recordContainerFailure(taskBase, spec.TaskID, proc, res, err, runCtx.Err())
			}
		default:
		}
		// Stop and Observe retry Close if Docker or socket removal fails here.
		_ = session.Close(context.WithoutCancel(runCtx))
	}()
	a.containers.Store(taskBase, proc)
	select {
	case id := <-startedCh:
		handle.ContainerID = id.ContainerID
		proc.mu.Lock()
		proc.identity.ContainerID = id.ContainerID
		proc.mu.Unlock()
		close(handedOff)
		return handle, nil
	case <-proc.done:
		if runErr == nil {
			runErr = errors.New("council container exited before start confirmation")
		}
		return handle, fmt.Errorf("start council container: %w", runErr)
	case <-ctx.Done():
		runCancel()
		return handle, ctx.Err()
	}
}

func buildContainerProcessResult(res scruntime.CouncilResult) *processResult {
	var msg string
	if res.ExitCode != 0 {
		msg = fmt.Sprintf("coding harness exited with status %d", res.ExitCode)
	}
	if diag := strings.TrimSpace(redact.String(res.Stderr)); diag != "" {
		if len(diag) > maxDiagnosticTail {
			diag = diag[len(diag)-maxDiagnosticTail:]
		}
		msg = strings.TrimSpace(strings.Join([]string{msg, diag}, "\n"))
	}
	out := &processResult{
		ExitCode: res.ExitCode,
		Message:  msg,
	}
	if res.ExitCode == 0 {
		resp := harnessResponse{}
		_, _ = resp.Write([]byte(res.Stdout))
		out.Response = resp.text()
	}
	return out
}

func writeContainerLog(taskBase string, res scruntime.CouncilResult) error {
	var parts []string
	if s := strings.TrimSpace(redact.String(res.Stdout)); s != "" {
		parts = append(parts, s)
	}
	if s := strings.TrimSpace(redact.String(res.Stderr)); s != "" {
		parts = append(parts, s)
	}
	if len(parts) == 0 {
		return nil
	}
	body := strings.Join(parts, "\n") + "\n"
	if len(body) > maxSessionLog {
		body = body[:maxSessionLog]
	}
	return os.WriteFile(taskBase+".log", []byte(body), 0o600)
}

func (a *Adapter) recordContainerFailure(taskBase, taskID string, proc *containerProcess, res scruntime.CouncilResult, runErr, ctxErr error) {
	if errors.Is(runErr, scruntime.ErrProcessNotFound) {
		return
	}
	proc.mu.Lock()
	stopped := proc.stopped
	proc.mu.Unlock()
	if stopped {
		return
	}
	lockCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	release, err := a.lockTask(lockCtx, taskID, false)
	if err != nil {
		return
	}
	defer release()
	cur, err := readWorktreeState(taskBase + stateSuffix)
	if err != nil || cur.Result != nil {
		return
	}
	if cur.Container != nil && res.ContainerID != "" {
		cur.Container.ContainerID = res.ContainerID
	}
	msg := "coding container failed"
	if errors.Is(runErr, context.DeadlineExceeded) || errors.Is(ctxErr, context.DeadlineExceeded) {
		msg = "coding task exceeded its wall time limit"
	}
	cur.Result = &processResult{ExitCode: -1, Message: msg}
	_ = writeWorktreeState(taskBase, cur)
}

func (a *Adapter) observeOrphanContainer(ctx context.Context, handle scruntime.Handle) (scruntime.Observation, error) {
	inspector, ok := a.CouncilRunner.(scruntime.CouncilInspector)
	if !ok {
		return scruntime.Observation{}, scruntime.ErrProcessNotFound
	}
	insp, err := inspector.InspectCouncil(ctx, containerIdentityFor(nil, handle))
	if err != nil {
		return scruntime.Observation{}, err
	}
	obs := scruntime.Observation{ObservedAt: time.Now().UTC()}
	if !insp.Exists {
		return obs, scruntime.ErrProcessNotFound
	}
	if insp.Running {
		obs.State = scruntime.StateRunning
		return obs, nil
	}
	if insp.Exited {
		if insp.ExitCode == 0 {
			obs.State = scruntime.StateCompleted
		} else {
			obs.State = scruntime.StateFailed
			obs.ExitCode = insp.ExitCode
			obs.Message = fmt.Sprintf("coding harness exited with status %d", insp.ExitCode)
		}
		return obs, nil
	}
	return obs, scruntime.ErrProcessNotFound
}

func (a *Adapter) stopInMemoryContainer(ctx context.Context, taskBase string, h scruntime.Handle) error {
	proc := a.containerProc(taskBase)
	if proc == nil {
		return nil
	}
	proc.mu.Lock()
	if !matchesContainerHandle(&proc.identity, h) {
		proc.mu.Unlock()
		return errors.New("council stop identity mismatch")
	}
	proc.stopped = true
	proc.mu.Unlock()
	if proc.cancel != nil {
		proc.cancel()
	}
	select {
	case <-proc.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (a *Adapter) loadContainerResult(ctx context.Context, taskBase string, handle scruntime.Handle, state *worktreeState) (bool, error) {
	if proc := a.containerProc(taskBase); proc != nil {
		if !outputClosed(proc.done) {
			return false, nil
		}
		// Refresh state in case the background goroutine persisted a result before closing done.
		if fresh, err := readWorktreeState(taskBase + stateSuffix); err == nil && fresh.Result != nil {
			state.Result = fresh.Result
			if fresh.Container != nil {
				state.Container = fresh.Container
			}
		}
	}
	if state.Result != nil {
		return true, nil
	}
	inspector, ok := a.CouncilRunner.(scruntime.CouncilInspector)
	if !ok {
		return false, scruntime.ErrProcessNotFound
	}
	insp, err := inspector.InspectCouncil(ctx, containerIdentityFor(state.Container, handle))
	if err != nil {
		return false, err
	}
	if !insp.Exists {
		return false, scruntime.ErrProcessNotFound
	}
	if insp.Running {
		return false, nil
	}
	if insp.Exited {
		code, msg := insp.ExitCode, ""
		if code != 0 {
			msg = fmt.Sprintf("coding harness exited with status %d", code)
		} else {
			// Exit zero alone is not a completed turn: the output was lost with the daemon, so fail closed.
			code, msg = 1, "coding harness output was not recorded before the daemon restarted"
		}
		state.Result = &processResult{ExitCode: code, Message: msg}
		if insp.ContainerID != "" && state.Container != nil {
			state.Container.ContainerID = insp.ContainerID
		}
		return true, writeWorktreeState(taskBase, state)
	}
	return false, scruntime.ErrProcessNotFound
}

func containerIdentityFor(c *containerIdentity, h scruntime.Handle) scruntime.CouncilIdentity {
	id := scruntime.CouncilIdentity{
		ContainerName: h.ContainerName,
		CreateAttempt: h.CreateAttempt,
		ContainerID:   h.ContainerID,
	}
	if c != nil {
		if id.ContainerName == "" {
			id.ContainerName = c.ContainerName
		}
		if id.CreateAttempt == "" {
			id.CreateAttempt = c.CreateAttempt
		}
		if id.ContainerID == "" {
			id.ContainerID = c.ContainerID
		}
	}
	return id
}

func (a *Adapter) stopCouncilContainer(ctx context.Context, taskBase string, handle scruntime.Handle, state *worktreeState) error {
	c := state.Container
	if c == nil {
		c = &containerIdentity{
			ContainerName: handle.ContainerName,
			CreateAttempt: handle.CreateAttempt,
			ContainerID:   handle.ContainerID,
			ProxyName:     handle.ProxyName,
			NetworkName:   handle.NetworkName,
		}
		state.Container = c
	} else {
		if c.ProxyName == "" {
			c.ProxyName = handle.ProxyName
		}
		if c.NetworkName == "" {
			c.NetworkName = handle.NetworkName
		}
		if c.ContainerID == "" {
			c.ContainerID = handle.ContainerID
		}
	}
	if state.Result == nil {
		state.Result = &processResult{ExitCode: -1, Message: "coding task was stopped"}
		if err := writeWorktreeState(taskBase, state); err != nil {
			return err
		}
	}
	return a.finishTask(ctx, taskBase, state)
}

func (a *Adapter) cleanupContainerResources(ctx context.Context, c *containerIdentity, session councilproxy.LaunchSession) error {
	if c == nil {
		return nil
	}
	inspector, ok := a.CouncilRunner.(scruntime.CouncilInspector)
	if !ok {
		return errors.New("council container cleanup unavailable")
	}
	harnessErr := inspector.CleanupCouncil(ctx, scruntime.CouncilResult{
		ContainerName: c.ContainerName, CreateAttempt: c.CreateAttempt, ContainerID: c.ContainerID,
	})
	if session != nil {
		return errors.Join(harnessErr, session.Close(ctx))
	}
	if harnessErr != nil {
		return harnessErr
	}
	if c.ProxyName == "" && c.NetworkName == "" {
		return nil
	}
	if a.CouncilProxy == nil {
		return errors.New("council proxy cleanup unavailable")
	}
	if c.ProxyName != "" {
		for _, name := range []string{c.ProxyName + "-probe", c.ProxyName} {
			if err := a.CouncilProxy.RemoveProxy(ctx, name); err != nil {
				return err
			}
		}
	}
	return a.CouncilProxy.RemoveInternalNetwork(ctx, c.NetworkName)
}

func (a *Adapter) probeContainer(ctx context.Context, handle scruntime.Handle, exit bool) (bool, error) {
	if handle.ExternalID == "" {
		return false, nil
	}
	taskBase, err := a.taskBase(handle.ExternalID)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		var pathErr *os.PathError
		if errors.As(err, &pathErr) {
			return false, err
		}
		return false, nil
	}
	if taskBase != "" && handle.LogPath != "" && handle.LogPath != taskBase+".log" {
		return false, nil
	}
	release, err := a.lockTask(ctx, handle.ExternalID, false)
	if err != nil {
		return false, err
	}
	defer release()

	var state *worktreeState
	if taskBase != "" {
		state, err = a.worktreeState(handle)
		if err != nil {
			if !errors.Is(err, os.ErrNotExist) {
				var pathErr *os.PathError
				if errors.As(err, &pathErr) {
					return false, err
				}
				return false, nil
			}
			state = nil
		}
	}
	if exit {
		return state != nil && state.Result != nil, nil
	}
	// A daemon restart loses the private gate listener. A surviving harness
	// cannot resume the turn even if Docker still reports it running.
	if a.containerProc(taskBase) == nil {
		return false, nil
	}
	if state != nil && !state.FinishedAt.IsZero() {
		return false, nil
	}
	inspector, ok := a.CouncilRunner.(scruntime.CouncilInspector)
	if !ok {
		return false, nil
	}
	var cid *containerIdentity
	if state != nil {
		cid = state.Container
	}
	insp, err := inspector.InspectCouncil(ctx, containerIdentityFor(cid, handle))
	if err != nil {
		return false, err
	}
	return insp.Exists && insp.Running, nil
}
