package docker

import (
	"context"
	"errors"
	"strings"
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
	eng := newFakeProxyEngine()
	a := NewAdapter(eng, "")
	imgID := eng.imageID
	leaseHash := strings.Repeat("a", 64)

	session, err := councilproxy.Provision(t.Context(), a, imgID, councilproxy.Config{
		Harness:      "claude",
		DaemonListen: "127.0.0.1:7432",
		LeaseSHA256:  leaseHash,
	})
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	ep := session.Workload()
	if ep.ProxyName() == "" || ep.Network == "" || ep.DataGateURL == "" {
		t.Fatalf("unexpected endpoint: %+v", ep)
	}
	if len(eng.connectedEgress) != 1 || eng.connectedEgress[0] != ep.ProxyName() {
		t.Fatalf("egress connections = %v, want [%s]", eng.connectedEgress, ep.ProxyName())
	}
	if err := session.Close(t.Context()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// Idempotent close on absent resources succeeds.
	if err := a.RemoveProxy(t.Context(), ep.ProxyName()); err != nil {
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
