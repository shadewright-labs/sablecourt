package docker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
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
	Client           EngineClient
	NetworkName      string
	AttachHub        *attach.Hub
	// Mounts decides which host directories a harness container may bind
	// mount: the worktree root handle and the daemon's own protected paths.
	// Without it, no harness checkout is mounted.
	Mounts         *mountguard.Guard
	attachCtx      context.Context
	discoverSocket func() string
}

// remoteEngine refuses a harness launch on an engine that is not reached over
// a local Unix socket. The mount rule compares host file identities, which
// say nothing about the files of a remote engine (tcp://, ssh://).
func (a *Adapter) remoteEngine() error {
	hosts := []string{os.Getenv("DOCKER_HOST")}
	if c, ok := a.Client.(interface{ DaemonHost() string }); ok {
		hosts = append(hosts, c.DaemonHost())
	}
	for _, host := range hosts {
		if host == "" {
			continue
		}
		u, err := client.ParseHostURL(host)
		if err != nil || u.Scheme != "unix" {
			return errors.New("harness containers need a local Docker engine; a remote engine (tcp:// or ssh://) is refused")
		}
	}
	return nil
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
	if c, ok := a.Client.(interface{ DaemonHost() string }); ok {
		add(c.DaemonHost())
	}
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

// SetAttachHub ties log streams to the daemon lifetime across individual Start calls.
func (a *Adapter) SetAttachHub(ctx context.Context, hub *attach.Hub) {
	a.attachCtx, a.AttachHub = ctx, hub
}

var _ scruntime.Adapter = (*Adapter)(nil)

func NewFromEnv() (*Adapter, error) {
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
	return &Adapter{
		Client:      cli,
		NetworkName: DefaultNetworkName,
	}, nil
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

func NewAdapter(cli EngineClient, networkName string) *Adapter {
	if networkName == "" {
		networkName = DefaultNetworkName
	}
	return &Adapter{
		Client:      cli,
		NetworkName: networkName,
	}
}

// CouncilRuntimeConfigured distinguishes a missing local runtime from a Docker
// endpoint that must succeed before a council turn can start.
func (a *Adapter) CouncilRuntimeConfigured() bool {
	return os.Getenv("DOCKER_HOST") != "" || a.dockerSocket() != ""
}

// Ping checks whether the configured Docker engine is reachable.
func (a *Adapter) Ping(ctx context.Context) error {
	if a == nil || a.Client == nil {
		return errors.New("Docker client unavailable")
	}
	type pinger interface {
		Ping(context.Context, client.PingOptions) (client.PingResult, error)
	}
	p, ok := a.Client.(pinger)
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
		r, err := a.Client.NetworkInspect(ctx, name, client.NetworkInspectOptions{})
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
	_, createErr := a.Client.NetworkCreate(ctx, name, client.NetworkCreateOptions{
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
	r, err := a.Client.ImageInspect(ctx, ref)
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

	created, err := a.Client.ContainerCreate(ctx, client.ContainerCreateOptions{
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
		if att, ok := a.Client.(containerAttacher); ok {
			if hijacked, err := att.ContainerAttach(ctx, created.ID, client.ContainerAttachOptions{
				Stream: true,
				Stdin:  true,
			}); err == nil && hijacked.Conn != nil {
				stdinCloser = hijacked.Conn
			}
		}
	}

	if _, err := a.Client.ContainerStart(ctx, created.ID, client.ContainerStartOptions{}); err != nil {
		if stdinCloser != nil {
			_ = stdinCloser.Close()
		}
		return handle, fmt.Errorf("start container %s: %w", created.ID, err)
	}

	if a.AttachHub != nil {
		if ls, ok := a.Client.(logStreamer); ok {
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
	res, err := a.Client.ContainerInspect(ctx, handle.ExternalID, client.ContainerInspectOptions{})
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
	_, err := a.Client.ContainerStart(ctx, handle.ExternalID, client.ContainerStartOptions{})
	return err
}

func (a *Adapter) Stop(ctx context.Context, handle scruntime.Handle) error {
	timeout := 10
	_, err := a.Client.ContainerStop(ctx, handle.ExternalID, client.ContainerStopOptions{Timeout: &timeout})
	_, removeErr := a.Client.ContainerRemove(ctx, handle.ExternalID, client.ContainerRemoveOptions{Force: true})
	if removeErr == nil || isMissing(removeErr) {
		return nil
	}
	return errors.Join(err, removeErr)
}

func (a *Adapter) CollectArtifacts(ctx context.Context, handle scruntime.Handle) ([]scruntime.Artifact, error) {
	return nil, nil
}
