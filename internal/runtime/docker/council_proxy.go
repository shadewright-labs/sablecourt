package docker

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"slices"
	"sync"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"
	"github.com/shadewright-labs/sablecourt/internal/gate/councilproxy"
	rt "github.com/shadewright-labs/sablecourt/internal/runtime"
)

type proxyEngineAPI interface {
	NetworkConnect(ctx context.Context, networkID string, options client.NetworkConnectOptions) (client.NetworkConnectResult, error)
	NetworkRemove(ctx context.Context, networkID string, options client.NetworkRemoveOptions) (client.NetworkRemoveResult, error)
}

type proxyExecAPI interface {
	ExecCreate(ctx context.Context, containerID string, options client.ExecCreateOptions) (client.ExecCreateResult, error)
	ExecAttach(ctx context.Context, execID string, options client.ExecAttachOptions) (client.ExecAttachResult, error)
	ExecInspect(ctx context.Context, execID string, options client.ExecInspectOptions) (client.ExecInspectResult, error)
}

var _ councilproxy.Docker = (*Adapter)(nil)
var _ proxyEngineAPI = (*client.Client)(nil)
var _ proxyExecAPI = (*client.Client)(nil)

func (a *Adapter) ProvisionCouncilProxy(ctx context.Context, image string, cfg councilproxy.Config) (councilproxy.LaunchSession, error) {
	s, err := councilproxy.Provision(ctx, a, image, cfg)
	if s == nil {
		return nil, err
	}
	return s, err
}

func (a *Adapter) ProbeGateSocket(ctx context.Context, spec councilproxy.GateSocketProbe) (probeErr error) {
	engine, ok := a.engineClient().(councilEngineAPI)
	if !ok {
		return a.lacks("Docker cannot verify socket probe exit")
	}
	id, err := a.councilImageID(ctx, spec.ImageID)
	if err != nil {
		return err
	}
	if id != spec.ImageID {
		return errors.New("socket probe image ID mismatch")
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		if err := a.RemoveProxy(cleanup, spec.Name); err != nil {
			// Cleanup failure must never select the TLS fallback.
			probeErr = fmt.Errorf("remove socket probe: %w", err)
		}
	}()
	created, err := a.engineClient().ContainerCreate(ctx, client.ContainerCreateOptions{
		Name: spec.Name,
		Config: &container.Config{Image: spec.ImageID, User: spec.User, Cmd: spec.Args, Env: slices.Clone(councilBaseEnv),
			Labels:      map[string]string{councilproxy.ResourceLabel: spec.Name},
			Healthcheck: &container.HealthConfig{Test: []string{"NONE"}},
		},
		HostConfig: &container.HostConfig{NetworkMode: "none", ReadonlyRootfs: true,
			CapDrop: []string{"ALL"}, SecurityOpt: []string{"no-new-privileges:true"},
			LogConfig: container.LogConfig{Type: "none"},
			Mounts:    []mount.Mount{{Type: mount.TypeBind, Source: spec.HostSocketPath, Target: spec.ContainerSocketPath, ReadOnly: true}},
		},
	})
	if err == nil && (created.ID == "" || len(created.Warnings) != 0) {
		return errors.New("unverified socket probe creation")
	}
	if err == nil {
		_, err = a.engineClient().ContainerStart(ctx, created.ID, client.ContainerStartOptions{})
	}
	if err != nil {
		if councilproxy.IsUnshareableSocketBindError(spec.HostSocketPath, 125, err.Error()) {
			return councilproxy.ErrGateSocketUnsupported
		}
		return errors.New("Docker could not launch socket probe")
	}
	result := rt.CouncilResult{ContainerID: created.ID}
	if err := a.councilExit(ctx, engine, &result); err != nil {
		return err
	}
	switch result.ExitCode {
	case 0:
		return nil
	case councilproxy.ProbeSocketUnsupportedExitCode:
		return councilproxy.ErrGateSocketUnsupported
	default:
		return errors.New("Docker socket probe failed")
	}
}

func (a *Adapter) CreateInternalNetwork(ctx context.Context, spec councilproxy.InternalNetworkSpec) (info councilproxy.InternalNetworkInfo, err error) {
	name := spec.Name
	if !councilName.MatchString(name) {
		return info, errors.New("invalid council network name")
	}
	server, ok := a.engineClient().(interface {
		ServerVersion(context.Context, client.ServerVersionOptions) (client.ServerVersionResult, error)
	})
	if !ok {
		return info, a.lacks("Docker engine version unavailable")
	}
	v, err := server.ServerVersion(ctx, client.ServerVersionOptions{})
	if err != nil {
		return info, err
	}
	created, err := a.engineClient().NetworkCreate(ctx, name, client.NetworkCreateOptions{
		Driver: "bridge", Internal: true, Options: spec.Options,
		Labels: map[string]string{councilproxy.ResourceLabel: name},
	})
	if err != nil {
		return info, err
	}
	n, err := a.engineClient().NetworkInspect(ctx, name, client.NetworkInspectOptions{})
	if err != nil {
		return info, err
	}
	if created.ID == "" || len(created.Warning) != 0 || n.Network.ID != created.ID || n.Network.Name != name || n.Network.Labels[councilproxy.ResourceLabel] != name || n.Network.ConfigOnly || n.Network.Ingress {
		return info, errors.New("unverified council network identity")
	}
	info = councilproxy.InternalNetworkInfo{ID: created.ID, EngineVersion: v.Version, Driver: n.Network.Driver, Internal: n.Network.Internal, Options: n.Network.Options}
	for _, cfg := range n.Network.IPAM.Config {
		if cfg.Subnet.IsValid() {
			info.Subnets = append(info.Subnets, cfg.Subnet.String())
		}
		if cfg.Gateway.IsValid() {
			info.Gateways = append(info.Gateways, cfg.Gateway.String())
		}
	}
	return info, nil
}

func (a *Adapter) CreateProxy(ctx context.Context, spec councilproxy.ProxyContainer) error {
	if !councilName.MatchString(spec.Name) || !councilName.MatchString(spec.InternalNetwork) {
		return errors.New("invalid council proxy container name or network")
	}
	imageID, err := a.councilImageID(ctx, spec.ImageID)
	if err != nil {
		return err
	}
	if imageID != spec.ImageID {
		return errors.New("council proxy image ID mismatch")
	}
	netID, err := a.internalNetworkNamed(ctx, spec.InternalNetwork, false)
	if err != nil {
		return fmt.Errorf("inspect council proxy network: %w", err)
	}
	if spec.InternalNetworkID == "" || spec.InternalNetworkID != netID {
		return errors.New("council proxy network was replaced")
	}
	hostCfg := &container.HostConfig{
		NetworkMode:    container.NetworkMode(netID),
		ReadonlyRootfs: spec.ReadOnlyRootFS,
		LogConfig:      container.LogConfig{Type: "none"},
	}
	if spec.HostGateway != "" {
		hostCfg.ExtraHosts = []string{spec.HostGateway + ":" + spec.HostGatewayTarget}
	}
	if spec.GateSocketHostPath != "" {
		hostCfg.Mounts = []mount.Mount{{Type: mount.TypeBind, Source: spec.GateSocketHostPath, Target: spec.GateSocketContainerPath, ReadOnly: true}}
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
	created, err := a.engineClient().ContainerCreate(ctx, client.ContainerCreateOptions{
		Name: spec.Name,
		Config: &container.Config{
			Image: imageID,
			Env:   slices.Clone(councilBaseEnv),
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
	eng, ok := a.engineClient().(proxyEngineAPI)
	if !ok {
		return a.lacks("Docker client does not support connecting proxy egress")
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
	if _, err := a.engineClient().ContainerStart(ctx, name, client.ContainerStartOptions{}); err != nil {
		return err
	}
	readyCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	probeURL := "http://" + net.JoinHostPort(name, councilproxy.Port) + "/gate/task/messages"
	execAPI, hasExec := a.engineClient().(proxyExecAPI)
	if !hasExec {
		return a.lacks("Docker cannot probe council proxy readiness")
	}
	insp, err := a.engineClient().ContainerInspect(readyCtx, name, client.ContainerInspectOptions{})
	if err != nil {
		return err
	}
	if insp.Container.ID == "" || insp.Container.State == nil || !insp.Container.State.Running {
		return errors.New("council proxy is not running")
	}
	id := insp.Container.ID
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for {
		ready, err := probeProxyOnce(readyCtx, execAPI, id, probeURL)
		if err != nil {
			return err
		}
		if ready {
			return nil
		}
		// The listener may not be bound yet. Retry until the readiness deadline.
		select {
		case <-readyCtx.Done():
			return errors.New("council proxy did not become ready")
		case <-ticker.C:
		}
	}
}

// probeProxyOnce reports ready only for the exact authentication challenge and a clean exit.
// It reports not ready, without an error, when curl could not connect at all.
func probeProxyOnce(ctx context.Context, execAPI proxyExecAPI, id, probeURL string) (bool, error) {
	created, err := execAPI.ExecCreate(ctx, id, client.ExecCreateOptions{
		User: "10001:10001", AttachStdout: true, AttachStderr: true,
		Cmd: []string{"/usr/bin/curl", "--noproxy", "*", "--silent", "--show-error", "--max-time", "2", "--output", "/dev/null", "--write-out", "%{http_code}", probeURL},
	})
	if err != nil || created.ID == "" {
		return false, errors.New("cannot create proxy readiness probe")
	}
	stream, err := execAPI.ExecAttach(ctx, created.ID, client.ExecAttachOptions{})
	if err != nil || stream.Conn == nil {
		return false, errors.New("cannot attach proxy readiness probe")
	}
	closeStream := sync.OnceFunc(stream.Close)
	defer closeStream()
	stopClose := context.AfterFunc(ctx, closeStream)
	defer stopClose()
	if stream.Reader == nil {
		return false, errors.New("missing council proxy authentication challenge")
	}
	switch proxyProbeOutcome(stream.Reader) {
	case probeRefused:
		return false, nil
	case probeChallenge:
	default:
		return false, errors.New("missing council proxy authentication challenge")
	}
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for {
		res, err := execAPI.ExecInspect(ctx, created.ID, client.ExecInspectOptions{})
		if err != nil || res.ID != created.ID || res.ContainerID != id {
			return false, errors.New("unverified proxy readiness probe")
		}
		if !res.Running {
			if res.ExitCode != 0 {
				return false, errors.New("council proxy readiness probe failed")
			}
			return true, nil
		}
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-ticker.C:
		}
	}
}

type probeResult int

const (
	probeInvalid probeResult = iota
	probeChallenge
	probeRefused
)

// Docker multiplexes the probe's HTTP status and diagnostics on one stream.
// curl prints status 000 when no HTTP response arrived, such as a refused connection.
func proxyProbeOutcome(reader *bufio.Reader) probeResult {
	limited := &io.LimitedReader{R: reader, N: 256}
	var stdout, stderr bytes.Buffer
	if err := copyDockerStreams(limited, &stdout, &stderr); err != nil || limited.N <= 0 {
		return probeInvalid
	}
	switch {
	case stdout.String() == "401" && stderr.Len() == 0:
		return probeChallenge
	case stdout.String() == "000":
		return probeRefused
	}
	return probeInvalid
}

func (a *Adapter) RemoveProxy(ctx context.Context, name string) error {
	if name == "" {
		return nil
	}
	found, err := a.engineClient().ContainerInspect(ctx, name, client.ContainerInspectOptions{})
	if isMissing(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if found.Container.Config == nil || found.Container.Config.Labels[councilproxy.ResourceLabel] != name {
		return errors.New("refused to remove unlabelled council proxy container")
	}
	_, err = a.engineClient().ContainerRemove(ctx, found.Container.ID, client.ContainerRemoveOptions{Force: true, RemoveVolumes: true})
	if isMissing(err) {
		return nil
	}
	return err
}

func (a *Adapter) RemoveInternalNetwork(ctx context.Context, name string) error {
	if name == "" {
		return nil
	}
	eng, ok := a.engineClient().(proxyEngineAPI)
	if !ok {
		return a.lacks("Docker client does not support removing networks")
	}
	found, err := a.engineClient().NetworkInspect(ctx, name, client.NetworkInspectOptions{})
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
