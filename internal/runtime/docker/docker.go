package docker

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"

	scruntime "github.com/shadewright-labs/sablecourt/internal/runtime"
)

const DefaultNetworkName = "sablecourt-internal"

// EngineClient is the narrow subset of github.com/moby/moby/client.Client used by the Docker adapter.
type EngineClient interface {
	NetworkInspect(ctx context.Context, networkID string, options client.NetworkInspectOptions) (client.NetworkInspectResult, error)
	NetworkCreate(ctx context.Context, name string, options client.NetworkCreateOptions) (client.NetworkCreateResult, error)
	ContainerCreate(ctx context.Context, options client.ContainerCreateOptions) (client.ContainerCreateResult, error)
	ContainerStart(ctx context.Context, containerID string, options client.ContainerStartOptions) (client.ContainerStartResult, error)
	ContainerInspect(ctx context.Context, containerID string, options client.ContainerInspectOptions) (client.ContainerInspectResult, error)
	ContainerStop(ctx context.Context, containerID string, options client.ContainerStopOptions) (client.ContainerStopResult, error)
}

type Adapter struct {
	Client      EngineClient
	NetworkName string
}

var _ scruntime.Adapter = (*Adapter)(nil)

func NewFromEnv() (*Adapter, error) {
	cli, err := client.New(client.FromEnv)
	if err != nil {
		return nil, fmt.Errorf("create moby client: %w", err)
	}
	return &Adapter{
		Client:      cli,
		NetworkName: DefaultNetworkName,
	}, nil
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

func (a *Adapter) Start(ctx context.Context, spec scruntime.LaunchSpec) (scruntime.Handle, error) {
	if spec.Workload.Spec.Runtime.Image == "" {
		return scruntime.Handle{}, errors.New("docker runtime requires runtime.image")
	}
	if err := a.EnsureInternalNetwork(ctx); err != nil {
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
			Image: spec.Workload.Spec.Runtime.Image,
			Env:   env,
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

	if _, err := a.Client.ContainerStart(ctx, created.ID, client.ContainerStartOptions{}); err != nil {
		return scruntime.Handle{}, fmt.Errorf("start container %s: %w", created.ID, err)
	}

	return scruntime.Handle{
		AdapterName: "docker",
		ExternalID:  created.ID,
		WorkDir:     spec.WorkDir,
	}, nil
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
	return err
}

func (a *Adapter) CollectArtifacts(_ context.Context, _ scruntime.Handle) ([]scruntime.Artifact, error) {
	return nil, nil
}
