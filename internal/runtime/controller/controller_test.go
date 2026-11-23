package controller

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/shadewright-labs/sablecourt/internal/domain"
	scruntime "github.com/shadewright-labs/sablecourt/internal/runtime"
)

type stubRuntime struct {
	started bool
}

func (s *stubRuntime) Start(_ context.Context, spec scruntime.LaunchSpec) (scruntime.Handle, error) {
	s.started = true
	return scruntime.Handle{AdapterName: "stub", ExternalID: spec.TaskID}, nil
}
func (s *stubRuntime) Observe(_ context.Context, _ scruntime.Handle) (scruntime.Observation, error) {
	return scruntime.Observation{State: scruntime.StateRunning, ObservedAt: time.Now().UTC()}, nil
}
func (s *stubRuntime) Suspend(_ context.Context, _ scruntime.Handle) (scruntime.CheckpointRef, error) {
	return scruntime.CheckpointRef{}, nil
}
func (s *stubRuntime) Resume(_ context.Context, _ scruntime.Handle, _ scruntime.CheckpointRef) error {
	return nil
}
func (s *stubRuntime) Stop(_ context.Context, _ scruntime.Handle) error { return nil }
func (s *stubRuntime) CollectArtifacts(_ context.Context, _ scruntime.Handle) ([]scruntime.Artifact, error) {
	return nil, nil
}

func TestControllerAdapterRefusesModelGateCredentials(t *testing.T) {
	inner := &stubRuntime{}
	ctrl := NewAdapter(inner)

	badSpec := scruntime.LaunchSpec{
		TaskID: "t1",
		Workload: domain.Workload{
			Metadata: domain.ObjectMeta{Name: "health-ctrl", Zone: domain.ZoneRuntime},
			Spec: domain.WorkloadSpec{
				Class:  domain.ClassDeterministicController,
				Grants: domain.GrantSet{ModelGate: domain.ModelGateGrant{Allowed: false}},
			},
		},
		ModelGateToken: "sc_task_should_never_pass",
	}

	if _, err := ctrl.Start(context.Background(), badSpec); !errors.Is(err, ErrModelGrantForbidden) {
		t.Fatalf("expected ErrModelGrantForbidden, got %v", err)
	}
	if inner.started {
		t.Fatal("inner runtime must not start when ModelGateToken is set")
	}

	goodSpec := badSpec
	goodSpec.ModelGateToken = ""
	if _, err := ctrl.Start(context.Background(), goodSpec); err != nil {
		t.Fatalf("expected clean controller start, got %v", err)
	}
	if !inner.started {
		t.Fatal("expected inner runtime to start")
	}
}
