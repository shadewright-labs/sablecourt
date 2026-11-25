package docker

import (
	"context"
	"errors"
	"fmt"
	"net"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"
	"github.com/shadewright-labs/sablecourt/internal/gate/councilproxy"
)

type proxyEngineAPI interface {
	NetworkConnect(ctx context.Context, networkID string, options client.NetworkConnectOptions) (client.NetworkConnectResult, error)
	NetworkRemove(ctx context.Context, networkID string, options client.NetworkRemoveOptions) (client.NetworkRemoveResult, error)
}

type proxyExecAPI interface {
	ExecCreate(ctx context.Context, containerID string, options client.ExecCreateOptions) (client.ExecCreateResult, error)
	ExecStart(ctx context.Context, execID string, options client.ExecStartOptions) (client.ExecStartResult, error)
	ExecInspect(ctx context.Context, execID string, options client.ExecInspectOptions) (client.ExecInspectResult, error)
}

var _ councilproxy.Docker = (*Adapter)(nil)
var _ proxyEngineAPI = (*client.Client)(nil)
var _ proxyExecAPI = (*client.Client)(nil)

func (a *Adapter) CreateInternalNetwork(ctx context.Context, name string) error {
	if !councilName.MatchString(name) {
		return errors.New("invalid council network name")
	}
	_, err := a.Client.NetworkCreate(ctx, name, client.NetworkCreateOptions{
		Driver:   "bridge",
		Internal: true,
		Labels: map[string]string{
			"app.kubernetes.io/managed-by": "sablecourt",
			councilproxy.ResourceLabel:     name,
		},
	})
	return err
}

func (a *Adapter) CreateProxy(ctx context.Context, spec councilproxy.ProxyContainer) error {
	if !councilName.MatchString(spec.Name) || !councilName.MatchString(spec.InternalNetwork) {
		return errors.New("invalid council proxy container name or network")
	}
	img, err := a.localImage(ctx, spec.ImageID)
	if err != nil {
		return err
	}
	if img.ID != spec.ImageID {
		return errors.New("council proxy image ID mismatch")
	}
	netID, err := a.internalNetworkNamed(ctx, spec.InternalNetwork, false)
	if err != nil {
		return fmt.Errorf("inspect council proxy network: %w", err)
	}
	hostCfg := &container.HostConfig{
		NetworkMode:    container.NetworkMode(netID),
		ReadonlyRootfs: spec.ReadOnlyRootFS,
		LogConfig:      container.LogConfig{Type: "none"},
		ExtraHosts:     []string{spec.HostGateway + ":host-gateway"},
	}
	if spec.DropAllCapabilities {
		hostCfg.CapDrop = []string{"ALL"}
	}
	if spec.NoNewPrivileges {
		hostCfg.SecurityOpt = []string{"no-new-privileges:true"}
	}
	netCfg := &network.NetworkingConfig{
		EndpointsConfig: map[string]*network.EndpointSettings{
			netID: {Aliases: []string{spec.Name}},
		},
	}
	created, err := a.Client.ContainerCreate(ctx, client.ContainerCreateOptions{
		Name: spec.Name,
		Config: &container.Config{
			Image: img.ID,
			User:  spec.User,
			Cmd:   spec.Args,
			Labels: map[string]string{
				"app.kubernetes.io/managed-by": "sablecourt",
				councilproxy.ResourceLabel:     spec.Name,
			},
			Healthcheck: &container.HealthConfig{Test: []string{"NONE"}},
		},
		HostConfig:       hostCfg,
		NetworkingConfig: netCfg,
	})
	if err != nil {
		return err
	}
	if created.ID == "" {
		return errors.New("Docker returned no proxy container ID")
	}
	if len(created.Warnings) > 0 {
		return errors.New("Docker reported a warning creating council proxy")
	}
	return nil
}

func (a *Adapter) ConnectProxyEgress(ctx context.Context, name string) error {
	eng, ok := a.Client.(proxyEngineAPI)
	if !ok {
		return errors.New("Docker client does not support connecting proxy egress")
	}
	_, err := eng.NetworkConnect(ctx, "bridge", client.NetworkConnectOptions{
		Container: name,
		EndpointConfig: &network.EndpointSettings{
			GwPriority: 1,
		},
	})
	return err
}

func (a *Adapter) StartProxy(ctx context.Context, name string) error {
	if _, err := a.Client.ContainerStart(ctx, name, client.ContainerStartOptions{}); err != nil {
		return err
	}
	readyCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	probeURL := "http://" + net.JoinHostPort(name, councilproxy.Port) + "/gate/task/messages"
	execAPI, hasExec := a.Client.(proxyExecAPI)
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()
	for {
		insp, err := a.Client.ContainerInspect(readyCtx, name, client.ContainerInspectOptions{})
		if err != nil {
			return fmt.Errorf("inspect council proxy: %w", err)
		}
		if insp.Container.State == nil || !insp.Container.State.Running {
			return errors.New("council proxy exited before becoming ready")
		}
		if !hasExec {
			return nil
		}
		if ready, supported := probeProxyHTTP(readyCtx, execAPI, insp.Container.ID, probeURL); ready || !supported {
			return nil
		}
		select {
		case <-readyCtx.Done():
			return fmt.Errorf("council proxy did not become ready: %w", readyCtx.Err())
		case <-tick.C:
		}
	}
}

func probeProxyHTTP(ctx context.Context, eng proxyExecAPI, containerID, target string) (ready bool, supported bool) {
	created, err := eng.ExecCreate(ctx, containerID, client.ExecCreateOptions{
		User: "10001:10001",
		Cmd: []string{
			"/bin/sh", "-c",
			`code=$(curl -sS --max-time 1 -o /dev/null -w '%{http_code}' "$1" 2>/dev/null); [ "$code" = "401" ]`,
			"probe", target,
		},
	})
	if err != nil || created.ID == "" {
		return false, false
	}
	if _, err := eng.ExecStart(ctx, created.ID, client.ExecStartOptions{}); err != nil {
		return false, false
	}
	for range 20 {
		insp, err := eng.ExecInspect(ctx, created.ID, client.ExecInspectOptions{})
		if err != nil {
			return false, true
		}
		if !insp.Running {
			if insp.ExitCode == 126 || insp.ExitCode == 127 {
				return false, false
			}
			return insp.ExitCode == 0, true
		}
		select {
		case <-ctx.Done():
			return false, true
		case <-time.After(25 * time.Millisecond):
		}
	}
	return false, true
}

func (a *Adapter) RemoveProxy(ctx context.Context, name string) error {
	if name == "" {
		return nil
	}
	found, err := a.Client.ContainerInspect(ctx, name, client.ContainerInspectOptions{})
	if isMissing(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if found.Container.Config == nil || found.Container.Config.Labels[councilproxy.ResourceLabel] != name {
		return errors.New("refused to remove unlabelled council proxy container")
	}
	_, err = a.Client.ContainerRemove(ctx, found.Container.ID, client.ContainerRemoveOptions{Force: true, RemoveVolumes: true})
	if isMissing(err) {
		return nil
	}
	return err
}

func (a *Adapter) RemoveInternalNetwork(ctx context.Context, name string) error {
	if name == "" {
		return nil
	}
	eng, ok := a.Client.(proxyEngineAPI)
	if !ok {
		return errors.New("Docker client does not support removing networks")
	}
	found, err := a.Client.NetworkInspect(ctx, name, client.NetworkInspectOptions{})
	if isMissing(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if found.Network.Labels[councilproxy.ResourceLabel] != name {
		return errors.New("refused to remove unlabelled council network")
	}
	_, err = eng.NetworkRemove(ctx, found.Network.ID, client.NetworkRemoveOptions{})
	if isMissing(err) {
		return nil
	}
	return err
}
