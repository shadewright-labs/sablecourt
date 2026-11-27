package docker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/containerd/errdefs"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"

	"github.com/shadewright-labs/sablecourt/internal/attach"
	scruntime "github.com/shadewright-labs/sablecourt/internal/runtime"
	"github.com/shadewright-labs/sablecourt/internal/runtime/mountguard"
)

type logStreamer interface {
	ContainerLogs(ctx context.Context, containerID string, options client.ContainerLogsOptions) (client.ContainerLogsResult, error)
}

type containerAttacher interface {
	ContainerAttach(ctx context.Context, containerID string, options client.ContainerAttachOptions) (client.ContainerAttachResult, error)
}

const DefaultNetworkName = "sablecourt-internal"

// EngineClient is the narrow subset of github.com/moby/moby/client.Client used by the Docker adapter.
type EngineClient interface {
	NetworkInspect(ctx context.Context, networkID string, options client.NetworkInspectOptions) (client.NetworkInspectResult, error)
	NetworkCreate(ctx context.Context, name string, options client.NetworkCreateOptions) (client.NetworkCreateResult, error)
	ImageInspect(ctx context.Context, imageID string, inspectOpts ...client.ImageInspectOption) (client.ImageInspectResult, error)
	ContainerCreate(ctx context.Context, options client.ContainerCreateOptions) (client.ContainerCreateResult, error)
	ContainerStart(ctx context.Context, containerID string, options client.ContainerStartOptions) (client.ContainerStartResult, error)
	ContainerInspect(ctx context.Context, containerID string, options client.ContainerInspectOptions) (client.ContainerInspectResult, error)
	ContainerStop(ctx context.Context, containerID string, options client.ContainerStopOptions) (client.ContainerStopResult, error)
	ContainerRemove(ctx context.Context, containerID string, options client.ContainerRemoveOptions) (client.ContainerRemoveResult, error)
}

type Adapter struct {
	CouncilHarnesses map[string]CouncilHarness
	// cli is unexported so that only NewFromEnv and NewAdapter, which refuse
	// a remote engine, can set it. Every engine call reads it through
	// engineClient, so a zero-value Adapter fails instead of running.
	cli         EngineClient
	NetworkName string
	AttachHub   *attach.Hub
	// Mounts decides which host directories a harness container may bind
	// mount: the worktree root handle and the daemon's own protected paths.
	// Without it, no harness checkout is mounted.
	Mounts         *mountguard.Guard
	attachCtx      context.Context
	discoverSocket func() string
}

// errRemoteEngine is the refusal of an engine not reached over a local Unix
// socket or a Windows named pipe on this machine.
var errRemoteEngine = errors.New("Sablecourt runs only on a local Docker engine (a unix:// socket or a named pipe on this machine, npipe:////./pipe/<name>); a remote engine such as tcp://, ssh:// or a pipe on another machine is refused")

// checkLocalEngine refuses a Docker host that is not a local Unix socket or
// a named pipe on the local machine. The mount rule compares host file
// identities, which say nothing about the files of a remote engine (tcp://,
// ssh://, or a pipe on another machine such as npipe:////server/pipe/name),
// so no Docker adapter exists on one. An empty host is the client's local
// default.
func checkLocalEngine(host string) error {
	if host == "" {
		return nil
	}
	u, err := client.ParseHostURL(host)
	if err != nil || u.Host == "" {
		return errRemoteEngine
	}
	switch u.Scheme {
	case "unix":
		return nil
	case "npipe":
		if localPipe(u.Host) {
			return nil
		}
	}
	return errRemoteEngine
}

// localPipe reports whether a named pipe address names a pipe on the local
// machine: //./pipe/<name> (or the same with backslashes), where the name is
// one path element without . or .. segments, which Windows would collapse
// into another path such as a UNC path to a remote machine.
func localPipe(addr string) bool {
	name, ok := strings.CutPrefix(strings.ReplaceAll(addr, "\\", "/"), "//./pipe/")
	if !ok || name == "" || name == "." || name == ".." {
		return false
	}
	for _, r := range name {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-' || r == '.') {
			return false
		}
	}
	return true
}

// errNoDaemonHost refuses an engine client that does not say which host it
// dials, so the remote engine check cannot be made.
var errNoDaemonHost = errors.New("Docker engine client does not report its daemon host, so a remote engine cannot be ruled out")

// localEngine checks DOCKER_HOST and the host the client itself dials. The
// constructors call it, so an adapter, and with it every engine call, exists
// only on a local engine. A client that does not report its host is refused.
// The host is read once, here: a client whose reported host changes later is
// not checked again.
func localEngine(cli EngineClient) error {
	if err := checkLocalEngine(os.Getenv("DOCKER_HOST")); err != nil {
		return err
	}
	c, ok := cli.(interface{ DaemonHost() string })
	if !ok {
		return errNoDaemonHost
	}
	return checkLocalEngine(c.DaemonHost())
}

// errNoEngine is returned by every engine call on an adapter that was not
// built by NewFromEnv or NewAdapter.
var errNoEngine = errors.New("Docker engine client unavailable: the adapter was not built by docker.NewFromEnv or docker.NewAdapter")

// engineClient returns the client the constructor checked, or, on a
// zero-value Adapter, a client that refuses every call with errNoEngine.
func (a *Adapter) engineClient() EngineClient {
	if a == nil || a.cli == nil {
		return noEngine{}
	}
	return a.cli
}

// lacks is the error of a call whose client misses an interface it needs:
// errNoEngine on a zero-value Adapter, otherwise what the client cannot do.
func (a *Adapter) lacks(what string) error {
	if a == nil || a.cli == nil {
		return errNoEngine
	}
	return errors.New(what)
}

// noEngine is the client of an adapter no constructor built. It implements
// only EngineClient, so the wider interfaces the council calls assert on are
// missing too, and those calls fail as well.
type noEngine struct{}

func (noEngine) NetworkInspect(context.Context, string, client.NetworkInspectOptions) (client.NetworkInspectResult, error) {
	return client.NetworkInspectResult{}, errNoEngine
}
func (noEngine) NetworkCreate(context.Context, string, client.NetworkCreateOptions) (client.NetworkCreateResult, error) {
	return client.NetworkCreateResult{}, errNoEngine
}
func (noEngine) ImageInspect(context.Context, string, ...client.ImageInspectOption) (client.ImageInspectResult, error) {
	return client.ImageInspectResult{}, errNoEngine
}
func (noEngine) ContainerCreate(context.Context, client.ContainerCreateOptions) (client.ContainerCreateResult, error) {
	return client.ContainerCreateResult{}, errNoEngine
}
func (noEngine) ContainerStart(context.Context, string, client.ContainerStartOptions) (client.ContainerStartResult, error) {
	return client.ContainerStartResult{}, errNoEngine
}
func (noEngine) ContainerInspect(context.Context, string, client.ContainerInspectOptions) (client.ContainerInspectResult, error) {
	return client.ContainerInspectResult{}, errNoEngine
}
func (noEngine) ContainerStop(context.Context, string, client.ContainerStopOptions) (client.ContainerStopResult, error) {
	return client.ContainerStopResult{}, errNoEngine
}
func (noEngine) ContainerRemove(context.Context, string, client.ContainerRemoveOptions) (client.ContainerRemoveResult, error) {
	return client.ContainerRemoveResult{}, errNoEngine
}

func (a *Adapter) dockerSocket() string {
	if a.discoverSocket != nil {
		return a.discoverSocket()
	}
	return discoverDockerSocket()
}

// dockerSockets lists every local runtime endpoint a checkout must not
// expose: the Unix socket the client itself resolved (DaemonHost), the one
// DOCKER_HOST names, and the discovered socket. Both host strings are read
// with the Moby client's own ParseHostURL, which takes everything after
// unix:// as a literal path, so a #, ? or %20 in it is not reinterpreted.
func (a *Adapter) dockerSockets() []string {
	var out []string
	add := func(host string) {
		if path := unixSocketPath(host); path != "" {
			out = append(out, path)
		}
	}
	add(a.DaemonHost())
	add(os.Getenv("DOCKER_HOST"))
	if sock := a.dockerSocket(); sock != "" {
		out = append(out, sock)
	}
	return out
}

// unixSocketPath returns the socket path of a unix:// host as written, or "".
// It is not cleaned: the client dials the raw spelling, so the guard resolves
// that same spelling.
func unixSocketPath(host string) string {
	if host == "" {
		return ""
	}
	u, err := client.ParseHostURL(host)
	if err != nil || u.Scheme != "unix" || u.Host == "" {
		return ""
	}
	return u.Host
}

// DaemonHost returns the engine host the adapter's client dials, as the
// constructor checked it, or "" on an adapter no constructor built.
func (a *Adapter) DaemonHost() string {
	if c, ok := a.engineClient().(interface{ DaemonHost() string }); ok {
		return c.DaemonHost()
	}
	return ""
}

// Close releases the engine client, when it holds connections.
func (a *Adapter) Close() error {
	if c, ok := a.engineClient().(io.Closer); ok {
		return c.Close()
	}
	return nil
}

// SetAttachHub ties log streams to the daemon lifetime across individual Start calls.
func (a *Adapter) SetAttachHub(ctx context.Context, hub *attach.Hub) {
	a.attachCtx, a.AttachHub = ctx, hub
}

var _ scruntime.Adapter = (*Adapter)(nil)

// NewFromEnv builds the adapter on the engine DOCKER_HOST names, or on a
// discovered local socket. It is the only place a Docker engine client is
// created (a test holds the module to that), and it refuses a remote engine
// before creating one.
func NewFromEnv() (*Adapter, error) {
	if err := checkLocalEngine(os.Getenv("DOCKER_HOST")); err != nil {
		return nil, err
	}
	opts := []client.Opt{client.FromEnv}
	if os.Getenv("DOCKER_HOST") == "" {
		if sock := discoverDockerSocket(); sock != "" {
			opts = append(opts, client.WithHost("unix://"+sock))
		}
	}
	cli, err := client.New(opts...)
	if err != nil {
		return nil, fmt.Errorf("create moby client: %w", err)
	}
	return NewAdapter(cli, DefaultNetworkName)
}

func discoverDockerSocket() string {
	candidates := []string{
		"/var/run/docker.sock",
		"/run/podman/podman.sock",
	}
	if uid := os.Getuid(); uid >= 0 {
		candidates = append(candidates, fmt.Sprintf("/run/user/%d/podman/podman.sock", uid))
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		candidates = append(candidates,
			filepath.Join(home, ".docker", "run", "docker.sock"),
			filepath.Join(home, ".orbstack", "run", "docker.sock"),
			filepath.Join(home, ".colima", "default", "docker.sock"),
			filepath.Join(home, ".rd", "docker.sock"),
			filepath.Join(home, ".local", "share", "containers", "podman", "machine", "podman.sock"),
		)
	}
	for _, c := range candidates {
		if _, err := os.Stat(c); !errors.Is(err, os.ErrNotExist) {
			return c
		}
	}
	return ""
}

// NewAdapter wraps an engine client. It refuses a remote engine: a remote
// DOCKER_HOST, a client whose DaemonHost is not a local socket or pipe, or a
// client that reports no DaemonHost at all.
func NewAdapter(cli EngineClient, networkName string) (*Adapter, error) {
	if err := localEngine(cli); err != nil {
		return nil, err
	}
	if networkName == "" {
		networkName = DefaultNetworkName
	}
	return &Adapter{
		cli:         cli,
		NetworkName: networkName,
	}, nil
}

// CouncilRuntimeConfigured distinguishes a missing local runtime from a Docker
// endpoint that must succeed before a council turn can start.
func (a *Adapter) CouncilRuntimeConfigured() bool {
	return os.Getenv("DOCKER_HOST") != "" || a.dockerSocket() != ""
}

// Ping checks whether the configured Docker engine is reachable.
func (a *Adapter) Ping(ctx context.Context) error {
	if a == nil || a.cli == nil {
		return errNoEngine
	}
	type pinger interface {
		Ping(context.Context, client.PingOptions) (client.PingResult, error)
	}
	p, ok := a.engineClient().(pinger)
	if !ok {
		return nil
	}
	_, err := p.Ping(ctx, client.PingOptions{})
	return err
}

func (a *Adapter) EnsureInternalNetwork(ctx context.Context) error {
	_, err := a.internalNetwork(ctx, true)
	return err
}

func isMissing(err error) bool {
	return errdefs.IsNotFound(err)
}

// Council runs use the network already provisioned for their proxy.
func (a *Adapter) internalNetwork(ctx context.Context, create bool) (string, error) {
	return a.internalNetworkNamed(ctx, a.NetworkName, create)
}

func (a *Adapter) internalNetworkNamed(ctx context.Context, name string, create bool) (string, error) {
	inspect := func() (string, error) {
		r, err := a.engineClient().NetworkInspect(ctx, name, client.NetworkInspectOptions{})
		if err != nil {
			return "", err
		}
		n := r.Network
		if n.ID == "" || n.Name != name || !n.Internal || n.Driver != "bridge" || n.Ingress || n.ConfigOnly {
			return "", errors.New("expected a named internal bridge network")
		}
		return n.ID, nil
	}
	id, err := inspect()
	if !create || !isMissing(err) {
		return id, err
	}
	_, createErr := a.engineClient().NetworkCreate(ctx, name, client.NetworkCreateOptions{
		Driver: "bridge", Internal: true, Labels: map[string]string{"app.kubernetes.io/managed-by": "sablecourt"},
	})
	// A concurrent launcher may have created it. Validate the winner.
	id, err = inspect()
	if err != nil {
		return "", errors.Join(createErr, err)
	}
	return id, nil
}

var imageDigest = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

func (a *Adapter) localImage(ctx context.Context, ref string) (client.ImageInspectResult, error) {
	r, err := a.engineClient().ImageInspect(ctx, ref)
	if err != nil {
		return r, fmt.Errorf("inspect local image: %w", err)
	}
	if !imageDigest.MatchString(r.ID) {
		return r, errors.New("invalid local image ID")
	}
	return r, nil
}

func (a *Adapter) EnsureImage(ctx context.Context, ref string) error {
	_, err := a.localImage(ctx, ref)
	return err
}

// ResolveLocalImage verifies ref exists locally and returns its immutable sha256 ID.
func (a *Adapter) ResolveLocalImage(ctx context.Context, ref string) (string, error) {
	img, err := a.localImage(ctx, ref)
	if err != nil {
		return "", err
	}
	return img.ID, nil
}

func (a *Adapter) Start(ctx context.Context, spec scruntime.LaunchSpec) (scruntime.Handle, error) {
	if spec.Workload.Spec.Runtime.Image == "" {
		return scruntime.Handle{}, errors.New("docker runtime requires runtime.image")
	}
	image, err := a.localImage(ctx, spec.Workload.Spec.Runtime.Image)
	if err != nil {
		return scruntime.Handle{}, err
	}
	networkID, err := a.internalNetwork(ctx, true)
	if err != nil {
		return scruntime.Handle{}, err
	}

	env := []string{
		"SABLECOURT_MANAGED=1",
		"SABLECOURT_TASK_ID=" + spec.TaskID,
		"SABLECOURT_WORKLOAD=" + spec.Workload.Metadata.Name,
	}
	if spec.InputJSON != "" {
		env = append(env, "SABLECOURT_INPUT_JSON="+spec.InputJSON)
	}
	if spec.DataGateURL != "" {
		env = append(env, "SABLECOURT_DATA_GATE_URL="+spec.DataGateURL)
	}
	if spec.TaskToken != "" {
		env = append(env, "SABLECOURT_TASK_TOKEN="+spec.TaskToken)
	}
	if spec.ModelGateURL != "" && spec.ModelGateToken != "" {
		env = append(env, "SABLECOURT_MODEL_GATE_URL="+spec.ModelGateURL)
	}

	var binds []string
	if spec.WorkDir != "" {
		binds = append(binds, spec.WorkDir+":/workspace")
	}

	hostCfg := &container.HostConfig{
		Binds:          binds,
		NetworkMode:    container.NetworkMode(networkID),
		ReadonlyRootfs: spec.Workload.Spec.Runtime.ReadOnlyRootfs,
		CapDrop:        []string{"ALL"},
		SecurityOpt:    []string{"no-new-privileges:true"},
		ExtraHosts:     []string{"host.docker.internal:host-gateway"},
	}

	netCfg := &network.NetworkingConfig{
		EndpointsConfig: map[string]*network.EndpointSettings{
			networkID: {},
		},
	}

	created, err := a.engineClient().ContainerCreate(ctx, client.ContainerCreateOptions{
		Name: "sablecourt-" + spec.TaskID,
		Config: &container.Config{
			Image:     image.ID,
			Env:       env,
			OpenStdin: a.AttachHub != nil,
			Labels: map[string]string{
				"sablecourt.io/task-id":  spec.TaskID,
				"sablecourt.io/workload": spec.Workload.Metadata.Name,
				"sablecourt.io/zone":     string(spec.Workload.Metadata.Zone),
			},
		},
		HostConfig:       hostCfg,
		NetworkingConfig: netCfg,
	})
	if err != nil {
		return scruntime.Handle{}, fmt.Errorf("create container: %w", err)
	}

	handle := scruntime.Handle{
		AdapterName: "docker",
		ExternalID:  created.ID,
		WorkDir:     spec.WorkDir,
	}
	var stdinCloser io.WriteCloser
	if a.AttachHub != nil {
		if att, ok := a.engineClient().(containerAttacher); ok {
			if hijacked, err := att.ContainerAttach(ctx, created.ID, client.ContainerAttachOptions{
				Stream: true,
				Stdin:  true,
			}); err == nil && hijacked.Conn != nil {
				stdinCloser = hijacked.Conn
			}
		}
	}

	if _, err := a.engineClient().ContainerStart(ctx, created.ID, client.ContainerStartOptions{}); err != nil {
		if stdinCloser != nil {
			_ = stdinCloser.Close()
		}
		return handle, fmt.Errorf("start container %s: %w", created.ID, err)
	}

	if a.AttachHub != nil {
		if ls, ok := a.engineClient().(logStreamer); ok {
			a.AttachHub.Register(spec.TaskID, stdinCloser)
			if a.attachCtx != nil {
				ctx = a.attachCtx
			}
			go a.streamLogs(ctx, spec.TaskID, created.ID, ls)
		} else if stdinCloser != nil {
			a.AttachHub.Register(spec.TaskID, stdinCloser)
		}
	}

	return handle, nil
}

func (a *Adapter) streamLogs(ctx context.Context, taskID, containerID string, ls logStreamer) {
	defer a.AttachHub.Close(taskID)
	rc, err := ls.ContainerLogs(ctx, containerID, client.ContainerLogsOptions{
		ShowStdout: true,
		ShowStderr: true,
		Follow:     true,
	})
	if err != nil || rc == nil {
		return
	}
	defer rc.Close()
	output := hubOutput{a.AttachHub, taskID}
	_ = copyDockerStreams(rc, output, output)
}

type hubOutput struct {
	hub  *attach.Hub
	task string
}

func (w hubOutput) Write(p []byte) (int, error) {
	w.hub.WriteOutput(w.task, p)
	return len(p), nil
}

func (a *Adapter) Observe(ctx context.Context, handle scruntime.Handle) (scruntime.Observation, error) {
	res, err := a.engineClient().ContainerInspect(ctx, handle.ExternalID, client.ContainerInspectOptions{})
	if err != nil {
		return scruntime.Observation{}, fmt.Errorf("inspect container %s: %w", handle.ExternalID, err)
	}
	obs := scruntime.Observation{
		ObservedAt: time.Now().UTC(),
		State:      scruntime.StateUnknown,
	}
	if res.Container.State == nil {
		return obs, nil
	}
	st := res.Container.State
	obs.ExitCode = st.ExitCode
	switch {
	case st.Paused:
		obs.State = scruntime.StateSuspended
	case st.Running:
		obs.State = scruntime.StateRunning
	case st.Status == "exited" && st.ExitCode == 0:
		obs.State = scruntime.StateCompleted
	case st.Status == "exited" || st.Status == "dead":
		obs.State = scruntime.StateFailed
	}
	return obs, nil
}

func (a *Adapter) Suspend(_ context.Context, handle scruntime.Handle) (scruntime.CheckpointRef, error) {
	return scruntime.CheckpointRef{
		ID:        "cp-" + handle.ExternalID,
		Path:      handle.WorkDir,
		CreatedAt: time.Now().UTC(),
	}, nil
}

func (a *Adapter) Resume(ctx context.Context, handle scruntime.Handle, _ scruntime.CheckpointRef) error {
	_, err := a.engineClient().ContainerStart(ctx, handle.ExternalID, client.ContainerStartOptions{})
	return err
}

func (a *Adapter) Stop(ctx context.Context, handle scruntime.Handle) error {
	timeout := 10
	_, err := a.engineClient().ContainerStop(ctx, handle.ExternalID, client.ContainerStopOptions{Timeout: &timeout})
	_, removeErr := a.engineClient().ContainerRemove(ctx, handle.ExternalID, client.ContainerRemoveOptions{Force: true})
	if removeErr == nil || isMissing(removeErr) {
		return nil
	}
	return errors.Join(err, removeErr)
}

func (a *Adapter) CollectArtifacts(ctx context.Context, handle scruntime.Handle) ([]scruntime.Artifact, error) {
	return nil, nil
}
