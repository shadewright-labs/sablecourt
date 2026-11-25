package docker

import (
	"context"
	"errors"
	"github.com/moby/moby/api/types/image"
	"github.com/moby/moby/api/types/network"
	"slices"
	"strings"
	"testing"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"

	"github.com/shadewright-labs/sablecourt/internal/domain"
	scruntime "github.com/shadewright-labs/sablecourt/internal/runtime"
)

type fakeEngine struct {
	networkExists  bool
	createdNetwork client.NetworkCreateOptions
	createdOpts    client.ContainerCreateOptions
	startedID      string
	inspectState   *container.State
}

func (f *fakeEngine) NetworkInspect(_ context.Context, _ string, _ client.NetworkInspectOptions) (client.NetworkInspectResult, error) {
	if !f.networkExists {
		return client.NetworkInspectResult{}, missing{errors.New("not found")}
	}
	return client.NetworkInspectResult{Network: network.Inspect{Network: network.Network{ID: "net-1", Name: DefaultNetworkName, Internal: true, Driver: "bridge"}}}, nil
}

func (f *fakeEngine) NetworkCreate(_ context.Context, _ string, options client.NetworkCreateOptions) (client.NetworkCreateResult, error) {
	f.createdNetwork = options
	f.networkExists = true
	return client.NetworkCreateResult{ID: "net-1"}, nil
}

func (f *fakeEngine) ImageInspect(_ context.Context, _ string, _ ...client.ImageInspectOption) (client.ImageInspectResult, error) {
	return client.ImageInspectResult{InspectResponse: image.InspectResponse{ID: "sha256:" + strings.Repeat("a", 64)}}, nil
}

func (f *fakeEngine) ImagePull(_ context.Context, _ string, _ client.ImagePullOptions) (client.ImagePullResponse, error) {
	return nil, nil
}

func (f *fakeEngine) ContainerCreate(_ context.Context, options client.ContainerCreateOptions) (client.ContainerCreateResult, error) {
	f.createdOpts = options
	return client.ContainerCreateResult{ID: "ctr-123"}, nil
}

func (f *fakeEngine) ContainerStart(_ context.Context, containerID string, _ client.ContainerStartOptions) (client.ContainerStartResult, error) {
	f.startedID = containerID
	return client.ContainerStartResult{}, nil
}

func (f *fakeEngine) ContainerInspect(_ context.Context, _ string, _ client.ContainerInspectOptions) (client.ContainerInspectResult, error) {
	return client.ContainerInspectResult{
		Container: container.InspectResponse{State: f.inspectState},
	}, nil
}

func (f *fakeEngine) ContainerStop(_ context.Context, _ string, _ client.ContainerStopOptions) (client.ContainerStopResult, error) {
	return client.ContainerStopResult{}, nil
}

func (f *fakeEngine) ContainerRemove(_ context.Context, _ string, _ client.ContainerRemoveOptions) (client.ContainerRemoveResult, error) {
	return client.ContainerRemoveResult{}, nil
}

func TestDockerAdapterEnforcesInternalNetworkAndHardening(t *testing.T) {
	ctx := context.Background()
	eng := &fakeEngine{
		inspectState: &container.State{Status: "exited", ExitCode: 0},
	}
	adapter := NewAdapter(eng, DefaultNetworkName)

	handle, err := adapter.Start(ctx, scruntime.LaunchSpec{
		TaskID: "task-9",
		Workload: domain.Workload{
			Metadata: domain.ObjectMeta{Name: "triage-agent", Zone: domain.ZoneRuntime},
			Spec: domain.WorkloadSpec{
				Class: domain.ClassLongLivedAgent,
				Runtime: domain.RuntimeConfig{
					Type:           "docker",
					Image:          "ghcr.io/shadewright-labs/triage:0.1.0",
					ReadOnlyRootfs: true,
				},
			},
		},
		WorkDir:        "/tmp/task-9",
		ModelGateURL:   "http://host.docker.internal:7432/gate/model",
		ModelGateToken: "sc_task_1234",
		TaskToken:      "sc_task_1234",
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if handle.ExternalID != "ctr-123" || eng.startedID != "ctr-123" {
		t.Fatalf("unexpected container start state: handle=%+v started=%q", handle, eng.startedID)
	}
	if !eng.createdNetwork.Internal {
		t.Fatal("expected internal-only Docker network (no internet egress)")
	}
	hc := eng.createdOpts.HostConfig
	if !hc.ReadonlyRootfs {
		t.Fatal("expected ReadonlyRootfs=true")
	}
	if !slices.Contains(hc.CapDrop, "ALL") {
		t.Fatalf("expected CapDrop ALL, got %v", hc.CapDrop)
	}
	if !slices.Contains(hc.SecurityOpt, "no-new-privileges:true") {
		t.Fatalf("expected no-new-privileges:true, got %v", hc.SecurityOpt)
	}
	if !slices.Contains(hc.ExtraHosts, "host.docker.internal:host-gateway") {
		t.Fatalf("expected host.docker.internal:host-gateway ExtraHost, got %v", hc.ExtraHosts)
	}

	obs, err := adapter.Observe(ctx, handle)
	if err != nil || obs.State != scruntime.StateCompleted {
		t.Fatalf("expected StateCompleted, got %+v err=%v", obs, err)
	}
}

type timedOutEngine struct {
	fakeEngine
	removed bool
}

func (f *timedOutEngine) ContainerStart(context.Context, string, client.ContainerStartOptions) (client.ContainerStartResult, error) {
	return client.ContainerStartResult{}, context.DeadlineExceeded
}
func (f *timedOutEngine) ContainerRemove(context.Context, string, client.ContainerRemoveOptions) (client.ContainerRemoveResult, error) {
	f.removed = true
	return client.ContainerRemoveResult{}, nil
}

func TestStartTimeoutKeepsHandle(t *testing.T) {
	e := &timedOutEngine{}
	a := NewAdapter(e, "")
	var spec scruntime.LaunchSpec
	spec.Workload.Spec.Runtime.Image = "fixture"
	h, err := a.Start(context.Background(), spec)
	if !errors.Is(err, context.DeadlineExceeded) || h.ExternalID != "ctr-123" {
		t.Fatalf("handle=%+v err=%v", h, err)
	}
	if err := a.Stop(context.Background(), h); err != nil || !e.removed {
		t.Fatalf("cleanup failed: %v", err)
	}
}

func TestCollectPreservesExit(t *testing.T) {
	e := &timedOutEngine{}
	a := NewAdapter(e, "")
	if _, err := a.CollectArtifacts(context.Background(), scruntime.Handle{ExternalID: "ctr-123"}); err != nil {
		t.Fatal(err)
	}
	if e.removed {
		t.Fatal("collection removed container before exit was recorded")
	}
}
