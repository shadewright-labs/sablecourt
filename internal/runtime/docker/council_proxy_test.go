package docker

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"testing"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"
	"github.com/shadewright-labs/sablecourt/internal/gate/councilproxy"
)

type fakeProxyEngine struct {
	*councilEngine
	nets             map[string]network.Network
	containers       map[string]container.InspectResponse
	connectedEgress  []string
	removedNetworks  []string
	removedContainer []string
}

func newFakeProxyEngine() *fakeProxyEngine {
	return &fakeProxyEngine{
		councilEngine: newCouncilEngine(),
		nets:          make(map[string]network.Network),
		containers:    make(map[string]container.InspectResponse),
	}
}

func (f *fakeProxyEngine) NetworkCreate(_ context.Context, name string, opts client.NetworkCreateOptions) (client.NetworkCreateResult, error) {
	f.nets[name] = network.Network{
		ID:       "net-" + name,
		Name:     name,
		Driver:   opts.Driver,
		Internal: opts.Internal,
		Labels:   opts.Labels,
		Options:  opts.Options, IPAM: network.IPAM{Config: []network.IPAMConfig{{Subnet: netip.PrefixFrom(netip.IPv4Unspecified(), 24)}}},
	}
	return client.NetworkCreateResult{ID: "net-" + name}, nil
}

func (f *fakeProxyEngine) NetworkInspect(_ context.Context, name string, _ client.NetworkInspectOptions) (client.NetworkInspectResult, error) {
	n, ok := f.nets[name]
	if !ok {
		return client.NetworkInspectResult{}, missing{errors.New("network not found")}
	}
	return client.NetworkInspectResult{Network: network.Inspect{Network: n}}, nil
}

func (f *fakeProxyEngine) NetworkConnect(_ context.Context, netID string, opts client.NetworkConnectOptions) (client.NetworkConnectResult, error) {
	if netID != "bridge" || opts.EndpointConfig == nil || opts.EndpointConfig.GwPriority != 1 {
		return client.NetworkConnectResult{}, errors.New("unexpected egress connect options")
	}
	f.connectedEgress = append(f.connectedEgress, opts.Container)
	return client.NetworkConnectResult{}, nil
}

func (f *fakeProxyEngine) NetworkRemove(_ context.Context, id string, _ client.NetworkRemoveOptions) (client.NetworkRemoveResult, error) {
	for name, n := range f.nets {
		if n.ID == id || name == id {
			delete(f.nets, name)
			f.removedNetworks = append(f.removedNetworks, name)
			return client.NetworkRemoveResult{}, nil
		}
	}
	return client.NetworkRemoveResult{}, missing{errors.New("network not found")}
}

func (f *fakeProxyEngine) ContainerCreate(_ context.Context, opts client.ContainerCreateOptions) (client.ContainerCreateResult, error) {
	id := "cid-" + opts.Name
	f.containers[opts.Name] = container.InspectResponse{
		ID:     id,
		Config: opts.Config,
		State:  &container.State{Status: "created", Running: false},
	}
	return client.ContainerCreateResult{ID: id}, nil
}

func (f *fakeProxyEngine) ContainerStart(_ context.Context, name string, _ client.ContainerStartOptions) (client.ContainerStartResult, error) {
	c, ok := f.containers[name]
	if !ok {
		return client.ContainerStartResult{}, missing{errors.New("container not found")}
	}
	c.State = &container.State{Status: "running", Running: true}
	f.containers[name] = c
	return client.ContainerStartResult{}, nil
}

func (f *fakeProxyEngine) ContainerInspect(_ context.Context, id string, _ client.ContainerInspectOptions) (client.ContainerInspectResult, error) {
	if c, ok := f.containers[id]; ok {
		return client.ContainerInspectResult{Container: c}, nil
	}
	for _, c := range f.containers {
		if c.ID == id {
			return client.ContainerInspectResult{Container: c}, nil
		}
	}
	return client.ContainerInspectResult{}, missing{errors.New("container not found")}
}

func (f *fakeProxyEngine) ContainerRemove(_ context.Context, id string, _ client.ContainerRemoveOptions) (client.ContainerRemoveResult, error) {
	for name, c := range f.containers {
		if c.ID == id || name == id {
			delete(f.containers, name)
			f.removedContainer = append(f.removedContainer, name)
			return client.ContainerRemoveResult{}, nil
		}
	}
	return client.ContainerRemoveResult{}, missing{errors.New("container not found")}
}

func TestCouncilProxyDockerLifecycleAndLabelRefusals(t *testing.T) {
	eng := &readinessEngine{fakeProxyEngine: newFakeProxyEngine()}
	a := NewAdapter(eng, "")
	imgID := eng.imageID

	session, err := councilproxy.Provision(t.Context(), a, imgID, councilproxy.Config{
		Harness:      "claude",
		DaemonListen: net.JoinHostPort(net.IPv6loopback.String(), "7432"),
	})
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	ep := session.Workload()
	if session.Evidence().Proxy == "" || ep.Network == "" || ep.DataGateURL != "" {
		t.Fatalf("unexpected endpoint: %+v", ep)
	}
	if len(eng.connectedEgress) != 1 || eng.connectedEgress[0] != session.Evidence().Proxy {
		t.Fatalf("egress connections = %v, want [%s]", eng.connectedEgress, session.Evidence().Proxy)
	}
	proxyName := session.Evidence().Proxy
	if err := session.Close(t.Context()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// Idempotent close on absent resources succeeds.
	if err := a.RemoveProxy(t.Context(), proxyName); err != nil {
		t.Fatalf("RemoveProxy absent: %v", err)
	}
	if err := a.RemoveInternalNetwork(t.Context(), ep.Network); err != nil {
		t.Fatalf("RemoveInternalNetwork absent: %v", err)
	}

	// Refusal paths: unlabelled container or network must never be removed.
	eng.containers["foreign-proxy"] = container.InspectResponse{
		ID:     "cid-foreign",
		Config: &container.Config{Labels: map[string]string{councilproxy.ResourceLabel: "other-name"}},
	}
	if err := a.RemoveProxy(t.Context(), "foreign-proxy"); err == nil {
		t.Fatal("removed proxy with mismatched ownership label")
	}
	eng.nets["foreign-net"] = network.Network{
		ID:     "net-foreign",
		Name:   "foreign-net",
		Labels: map[string]string{councilproxy.ResourceLabel: "other-net"},
	}
	if err := a.RemoveInternalNetwork(t.Context(), "foreign-net"); err == nil {
		t.Fatal("removed network with mismatched ownership label")
	}
}

func TestProxyReadinessRequiresExecEvidence(t *testing.T) {
	for _, fault := range []string{"create", "empty id", "start", "inspect", "failed probe", "missing binary", "empty response", "wrong status", "malformed response"} {
		t.Run(fault, func(t *testing.T) {
			eng := &readinessEngine{fakeProxyEngine: newFakeProxyEngine(), fault: fault}
			a := NewAdapter(eng, "")
			eng.containers["proxy"] = container.InspectResponse{ID: "cid", Config: &container.Config{}}
			if err := a.StartProxy(t.Context(), "proxy"); err == nil {
				t.Fatal("started without a successful readiness probe")
			}
		})
	}
}

type readinessEngine struct {
	*fakeProxyEngine
	fault       string
	containerID string
}

func (e *readinessEngine) ExecCreate(_ context.Context, id string, _ client.ExecCreateOptions) (client.ExecCreateResult, error) {
	e.containerID = id
	if e.fault == "create" {
		return client.ExecCreateResult{}, errors.New("exec create")
	}
	if e.fault == "empty id" {
		return client.ExecCreateResult{}, nil
	}
	return client.ExecCreateResult{ID: "exec"}, nil
}
func (e *readinessEngine) ExecAttach(context.Context, string, client.ExecAttachOptions) (client.ExecAttachResult, error) {
	if e.fault == "start" {
		return client.ExecAttachResult{}, errors.New("attach failed")
	}
	data := frame(1, "401")
	switch e.fault {
	case "empty response":
		data = nil
	case "wrong status":
		data = frame(1, "200")
	case "malformed response":
		data = []byte{1}
	}
	return client.ExecAttachResult{HijackedResponse: client.HijackedResponse{Conn: &fixtureConn{}, Reader: bufio.NewReader(bytes.NewReader(data))}}, nil
}
func (e *readinessEngine) ExecInspect(context.Context, string, client.ExecInspectOptions) (client.ExecInspectResult, error) {
	switch e.fault {
	case "inspect":
		return client.ExecInspectResult{}, errors.New("exec inspect")
	case "failed probe":
		return client.ExecInspectResult{ID: "exec", ContainerID: e.containerID, ExitCode: 1}, nil
	case "missing binary":
		return client.ExecInspectResult{ID: "exec", ContainerID: e.containerID, ExitCode: 127}, nil
	}
	return client.ExecInspectResult{ID: "exec", ContainerID: e.containerID}, nil
}

func (f *fakeProxyEngine) ServerVersion(context.Context, client.ServerVersionOptions) (client.ServerVersionResult, error) {
	return client.ServerVersionResult{Version: "28.0"}, nil
}

func TestProxyRejectsReplacedNetwork(t *testing.T) {
	e := newFakeProxyEngine()
	a := NewAdapter(e, "")
	spec := councilproxy.InternalNetworkSpec{Name: "private-network"}
	info, err := a.CreateInternalNetwork(t.Context(), spec)
	if err != nil {
		t.Fatal(err)
	}
	net := e.nets[spec.Name]
	net.ID = "replacement"
	e.nets[spec.Name] = net
	err = a.CreateProxy(t.Context(), councilproxy.ProxyContainer{Name: "proxy", ImageID: e.imageID, InternalNetwork: spec.Name, InternalNetworkID: info.ID})
	if err == nil || len(e.containers) != 0 {
		t.Fatal("proxy joined a replacement network")
	}
}

func TestSocketProbeChecksExitAndCleansUp(t *testing.T) {
	for _, tc := range []struct {
		name, step           string
		code                 int
		wantErr, unsupported bool
	}{
		{"verified", "", 0, false, false}, {"unsupported", "", councilproxy.ProbeSocketUnsupportedExitCode, true, true},
		{"probe refused", "", 9, true, false}, {"start error", "start", 0, true, false},
		{"inspect error", "inspect transient", 0, true, false}, {"cleanup error", "remove", councilproxy.ProbeSocketUnsupportedExitCode, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			engine := &socketProbeEngine{councilEngine: newCouncilEngine(), exit: tc.code}
			engine.step = tc.step
			a := NewAdapter(engine, "")
			spec := councilproxy.GateSocketProbe{Name: "probe", ImageID: engine.imageID, User: "10001:10001", HostSocketPath: "/source.sock", ContainerSocketPath: councilproxy.ContainerGateSocket}
			err := a.ProbeGateSocket(t.Context(), spec)
			if (err != nil) != tc.wantErr || errors.Is(err, councilproxy.ErrGateSocketUnsupported) != tc.unsupported {
				t.Fatalf("probe result %v", err)
			}
			h := engine.createdOpts.HostConfig
			if h.NetworkMode != "none" || !h.ReadonlyRootfs || h.LogConfig.Type != "none" || len(h.Mounts) != 1 || !h.Mounts[0].ReadOnly || h.Mounts[0].Source != spec.HostSocketPath || engine.createdOpts.Config.User != spec.User {
				t.Fatal("socket probe isolation mismatch")
			}
			if tc.step != "remove" && tc.step != "inspect transient" && engine.exists {
				t.Fatal("socket probe survived cleanup")
			}
		})
	}
}

type socketProbeEngine struct {
	*councilEngine
	exit int
}

func (e *socketProbeEngine) ContainerWait(context.Context, string, client.ContainerWaitOptions) client.ContainerWaitResult {
	ch := make(chan container.WaitResponse, 1)
	ch <- container.WaitResponse{StatusCode: int64(e.exit)}
	return client.ContainerWaitResult{Result: ch}
}
func (e *socketProbeEngine) ContainerInspect(ctx context.Context, id string, o client.ContainerInspectOptions) (client.ContainerInspectResult, error) {
	r, err := e.councilEngine.ContainerInspect(ctx, id, o)
	if err == nil {
		r.Container.State.ExitCode = e.exit
	}
	return r, err
}

func TestProxyImageRejectsInheritedSecretsAndMounts(t *testing.T) {
	for _, fault := range []string{"environment", "volume"} {
		for _, stage := range []string{"probe", "proxy"} {
			t.Run(fault+"/"+stage, func(t *testing.T) {
				engine := &proxyImageEngine{fakeProxyEngine: newFakeProxyEngine(), fault: fault}
				a := NewAdapter(engine, "")
				var err error
				if stage == "probe" {
					err = a.ProbeGateSocket(t.Context(), councilproxy.GateSocketProbe{Name: "probe", ImageID: engine.imageID})
				} else {
					net, createErr := a.CreateInternalNetwork(t.Context(), councilproxy.InternalNetworkSpec{Name: "private"})
					if createErr != nil {
						t.Fatal(createErr)
					}
					err = a.CreateProxy(t.Context(), councilproxy.ProxyContainer{Name: "proxy", ImageID: engine.imageID, InternalNetwork: "private", InternalNetworkID: net.ID})
				}
				if err == nil || engine.creates != 0 {
					t.Fatal("unsafe image reached Docker create")
				}
			})
		}
	}
}

type proxyImageEngine struct {
	*fakeProxyEngine
	fault   string
	creates int
}

func (e *proxyImageEngine) ContainerCreate(ctx context.Context, opts client.ContainerCreateOptions) (client.ContainerCreateResult, error) {
	e.creates++
	return e.fakeProxyEngine.ContainerCreate(ctx, opts)
}
func (e *proxyImageEngine) ImageInspect(ctx context.Context, ref string, opts ...client.ImageInspectOption) (client.ImageInspectResult, error) {
	r, err := e.fakeProxyEngine.ImageInspect(ctx, ref, opts...)
	switch e.fault {
	case "environment":
		r.Config.Env = []string{"PRIVATE=fixture"}
	case "volume":
		r.Config.Volumes = map[string]struct{}{"/extra": {}}
	}
	return r, err
}

// lateListenerEngine refuses the first probes because the proxy has not bound its port yet.
type lateListenerEngine struct {
	*readinessEngine
	refusals int
	attempts int
}

func (e *lateListenerEngine) ExecCreate(ctx context.Context, id string, o client.ExecCreateOptions) (client.ExecCreateResult, error) {
	if _, err := e.readinessEngine.ExecCreate(ctx, id, o); err != nil {
		return client.ExecCreateResult{}, err
	}
	e.attempts++
	return client.ExecCreateResult{ID: fmt.Sprintf("exec-%d", e.attempts)}, nil
}

func (e *lateListenerEngine) ExecAttach(context.Context, string, client.ExecAttachOptions) (client.ExecAttachResult, error) {
	data := frame(1, "401")
	if e.attempts <= e.refusals {
		data = append(frame(1, "000"), frame(2, "curl: (7) Failed to connect")...)
	}
	return client.ExecAttachResult{HijackedResponse: client.HijackedResponse{Conn: &fixtureConn{}, Reader: bufio.NewReader(bytes.NewReader(data))}}, nil
}

func (e *lateListenerEngine) ExecInspect(_ context.Context, execID string, _ client.ExecInspectOptions) (client.ExecInspectResult, error) {
	code := 0
	if e.attempts <= e.refusals {
		code = 7
	}
	return client.ExecInspectResult{ID: execID, ContainerID: e.containerID, ExitCode: code}, nil
}

func TestProxyReadinessRetriesUntilListenerBinds(t *testing.T) {
	eng := &lateListenerEngine{readinessEngine: &readinessEngine{fakeProxyEngine: newFakeProxyEngine()}, refusals: 2}
	a := NewAdapter(eng, "")
	eng.containers["proxy"] = container.InspectResponse{ID: "cid", Config: &container.Config{}}
	if err := a.StartProxy(t.Context(), "proxy"); err != nil {
		t.Fatalf("probe did not retry a refused connection: %v", err)
	}
	if eng.attempts != 3 {
		t.Fatalf("attempts = %d, want 3", eng.attempts)
	}
}
