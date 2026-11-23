package controller

import (
	"context"
	"errors"

	"github.com/shadewright-labs/sablecourt/internal/domain"
	scruntime "github.com/shadewright-labs/sablecourt/internal/runtime"
)

var ErrModelGrantForbidden = errors.New("deterministic-controller cannot receive Model Gate credentials or URL")

// Adapter wraps an underlying runtime adapter and enforces the zero-LLM invariant
// before launching any deterministic controller workload.
type Adapter struct {
	Inner scruntime.Adapter
}

var _ scruntime.Adapter = (*Adapter)(nil)

func NewAdapter(inner scruntime.Adapter) *Adapter {
	return &Adapter{Inner: inner}
}

func (a *Adapter) Start(ctx context.Context, spec scruntime.LaunchSpec) (scruntime.Handle, error) {
	if spec.Workload.Spec.Class != domain.ClassDeterministicController {
		return scruntime.Handle{}, errors.New("controller adapter accepts only deterministic-controller workloads")
	}
	if spec.Workload.Spec.Grants.ModelGate.Allowed || spec.ModelGateURL != "" || spec.ModelGateToken != "" {
		return scruntime.Handle{}, ErrModelGrantForbidden
	}
	if a.Inner == nil {
		return scruntime.Handle{}, errors.New("inner runtime adapter is nil")
	}
	return a.Inner.Start(ctx, spec)
}

func (a *Adapter) Observe(ctx context.Context, handle scruntime.Handle) (scruntime.Observation, error) {
	return a.Inner.Observe(ctx, handle)
}

func (a *Adapter) Suspend(ctx context.Context, handle scruntime.Handle) (scruntime.CheckpointRef, error) {
	return a.Inner.Suspend(ctx, handle)
}

func (a *Adapter) Resume(ctx context.Context, handle scruntime.Handle, cp scruntime.CheckpointRef) error {
	return a.Inner.Resume(ctx, handle, cp)
}

func (a *Adapter) Stop(ctx context.Context, handle scruntime.Handle) error {
	return a.Inner.Stop(ctx, handle)
}

func (a *Adapter) CollectArtifacts(ctx context.Context, handle scruntime.Handle) ([]scruntime.Artifact, error) {
	return a.Inner.CollectArtifacts(ctx, handle)
}
