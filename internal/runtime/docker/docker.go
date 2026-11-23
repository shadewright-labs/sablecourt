package docker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"

	"github.com/shadewright-labs/sablecourt/internal/attach"
	scruntime "github.com/shadewright-labs/sablecourt/internal/runtime"
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
	ImagePull(ctx context.Context, refStr string, options client.ImagePullOptions) (client.ImagePullResponse, error)
	ContainerCreate(ctx context.Context, options client.ContainerCreateOptions) (client.ContainerCreateResult, error)
	ContainerStart(ctx context.Context, containerID string, options client.ContainerStartOptions) (client.ContainerStartResult, error)
	ContainerInspect(ctx context.Context, containerID string, options client.ContainerInspectOptions) (client.ContainerInspectResult, error)
	ContainerStop(ctx context.Context, containerID string, options client.ContainerStopOptions) (client.ContainerStopResult, error)
	ContainerRemove(ctx context.Context, containerID string, options client.ContainerRemoveOptions) (client.ContainerRemoveResult, error)
}

type Adapter struct {
	Client      EngineClient
	NetworkName string
	AttachHub   *attach.Hub
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
		if _, err := os.Stat(c); err == nil {
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

func (a *Adapter) EnsureInternalNetwork(ctx context.Context) error {
	if _, err := a.Client.NetworkInspect(ctx, a.NetworkName, client.NetworkInspectOptions{}); err == nil {
		return nil
	}
	_, err := a.Client.NetworkCreate(ctx, a.NetworkName, client.NetworkCreateOptions{
		Driver:   "bridge",
		Internal: true,
		Labels: map[string]string{
			"app.kubernetes.io/managed-by": "sablecourt",
		},
	})
	if err != nil {
		return fmt.Errorf("create internal network %q: %w", a.NetworkName, err)
	}
	return nil
}

func (a *Adapter) EnsureImage(ctx context.Context, imageRef string) error {
	if _, err := a.Client.ImageInspect(ctx, imageRef); err == nil {
		return nil
	}
	resp, err := a.Client.ImagePull(ctx, imageRef, client.ImagePullOptions{})
	if err != nil {
		return fmt.Errorf("pull image %q: %w", imageRef, err)
	}
	if resp != nil {
		defer resp.Close()
		_, _ = io.Copy(io.Discard, resp)
	}
	return nil
}

func (a *Adapter) Start(ctx context.Context, spec scruntime.LaunchSpec) (scruntime.Handle, error) {
	if spec.Workload.Spec.Runtime.Image == "" {
		return scruntime.Handle{}, errors.New("docker runtime requires runtime.image")
	}
	if err := a.EnsureInternalNetwork(ctx); err != nil {
		return scruntime.Handle{}, err
	}
	if err := a.EnsureImage(ctx, spec.Workload.Spec.Runtime.Image); err != nil {
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
		NetworkMode:    container.NetworkMode(a.NetworkName),
		ReadonlyRootfs: spec.Workload.Spec.Runtime.ReadOnlyRootfs,
		CapDrop:        []string{"ALL"},
		SecurityOpt:    []string{"no-new-privileges:true"},
		ExtraHosts:     []string{"host.docker.internal:host-gateway"},
	}

	netCfg := &network.NetworkingConfig{
		EndpointsConfig: map[string]*network.EndpointSettings{
			a.NetworkName: {},
		},
	}

	created, err := a.Client.ContainerCreate(ctx, client.ContainerCreateOptions{
		Name: "sablecourt-" + spec.TaskID,
		Config: &container.Config{
			Image:     spec.Workload.Spec.Runtime.Image,
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
		return scruntime.Handle{}, fmt.Errorf("start container %s: %w", created.ID, err)
	}

	if a.AttachHub != nil {
		if ls, ok := a.Client.(logStreamer); ok {
			a.AttachHub.Register(spec.TaskID, stdinCloser)
			go a.streamLogs(spec.TaskID, created.ID, ls)
		} else if stdinCloser != nil {
			a.AttachHub.Register(spec.TaskID, stdinCloser)
		}
	}

	return scruntime.Handle{
		AdapterName: "docker",
		ExternalID:  created.ID,
		WorkDir:     spec.WorkDir,
	}, nil
}

func (a *Adapter) streamLogs(taskID, containerID string, ls logStreamer) {
	defer a.AttachHub.Close(taskID)
	rc, err := ls.ContainerLogs(context.Background(), containerID, client.ContainerLogsOptions{
		ShowStdout: true,
		ShowStderr: true,
		Follow:     true,
	})
	if err != nil || rc == nil {
		return
	}
	defer rc.Close()
	buf := make([]byte, 4096)
	for {
		n, readErr := rc.Read(buf)
		if n > 0 {
			chunk := stripDockerMuxHeader(buf[:n])
			a.AttachHub.WriteOutput(taskID, chunk)
		}
		if readErr != nil {
			return
		}
	}
}

func stripDockerMuxHeader(p []byte) []byte {
	// Docker non-TTY multiplexed stream frames start with [stream(1), 0, 0, 0, size(4)].
	if len(p) >= 8 && (p[0] == 1 || p[0] == 2) && p[1] == 0 && p[2] == 0 && p[3] == 0 {
		return p[8:]
	}
	return p
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
	_, _ = a.Client.ContainerRemove(ctx, handle.ExternalID, client.ContainerRemoveOptions{Force: true})
	return err
}

func (a *Adapter) CollectArtifacts(ctx context.Context, handle scruntime.Handle) ([]scruntime.Artifact, error) {
	// Clean up exited container after final observation to avoid accumulating stopped containers.
	_, _ = a.Client.ContainerRemove(ctx, handle.ExternalID, client.ContainerRemoveOptions{Force: true})
	return nil, nil
}
