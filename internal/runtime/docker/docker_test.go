package docker

import (
	"context"
	"errors"
	"slices"
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
		return client.NetworkInspectResult{}, errors.New("not found")
	}
	return client.NetworkInspectResult{}, nil
}

func (f *fakeEngine) NetworkCreate(_ context.Context, _ string, options client.NetworkCreateOptions) (client.NetworkCreateResult, error) {
	f.createdNetwork = options
	f.networkExists = true
	return client.NetworkCreateResult{ID: "net-1"}, nil
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

	obs, err := adapter.Observe(ctx, handle)
	if err != nil || obs.State != scruntime.StateCompleted {
		t.Fatalf("expected StateCompleted, got %+v err=%v", obs, err)
	}
}
