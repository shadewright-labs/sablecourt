package controlplane

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/shadewright-labs/sablecourt/internal/config"
	"github.com/shadewright-labs/sablecourt/internal/domain"
	"github.com/shadewright-labs/sablecourt/internal/policy"
	scruntime "github.com/shadewright-labs/sablecourt/internal/runtime"
	"github.com/shadewright-labs/sablecourt/internal/runtime/controller"
	"github.com/shadewright-labs/sablecourt/internal/store/sqlite"
)

type fakeAdapter struct {
	nextState scruntime.ObservedState
	lastSpec  scruntime.LaunchSpec
}

func (f *fakeAdapter) Start(_ context.Context, spec scruntime.LaunchSpec) (scruntime.Handle, error) {
	f.lastSpec = spec
	return scruntime.Handle{AdapterName: "fake", ExternalID: spec.TaskID}, nil
}
func (f *fakeAdapter) Observe(_ context.Context, _ scruntime.Handle) (scruntime.Observation, error) {
	return scruntime.Observation{State: f.nextState, ObservedAt: time.Now().UTC()}, nil
}
func (f *fakeAdapter) Suspend(_ context.Context, _ scruntime.Handle) (scruntime.CheckpointRef, error) {
	return scruntime.CheckpointRef{}, nil
}
func (f *fakeAdapter) Resume(_ context.Context, _ scruntime.Handle, _ scruntime.CheckpointRef) error {
	return nil
}
func (f *fakeAdapter) Stop(_ context.Context, _ scruntime.Handle) error { return nil }
func (f *fakeAdapter) CollectArtifacts(_ context.Context, _ scruntime.Handle) ([]scruntime.Artifact, error) {
	return nil, nil
}

func TestReconcilerLifecycleAndHubSpokeDelegation(t *testing.T) {
	ctx := context.Background()
	st, err := sqlite.Open(filepath.Join(t.TempDir(), "sablecourt.db"))
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	bundle := &config.Bundle{
		Policies: map[string]domain.Policy{
			"default": {
				Metadata: domain.ObjectMeta{Name: "default"},
				Spec: domain.PolicySpec{
					DefaultEffect: domain.EffectDeny,
					Rules: []domain.PolicyRule{
						{Workload: "triage-agent", Capability: "delegate:go-coder", Effect: domain.EffectAllow},
					},
				},
			},
		},
		Workloads: map[string]domain.Workload{
			"triage-agent": {
				Metadata: domain.ObjectMeta{Name: "triage-agent", Zone: domain.ZoneRuntime},
				Spec: domain.WorkloadSpec{
					Class: domain.ClassLongLivedAgent,
					Grants: domain.GrantSet{
						ModelGate:  domain.ModelGateGrant{Allowed: true, BudgetRef: "b1"},
						DelegateTo: []string{"go-coder"},
					},
				},
			},
			"go-coder": {
				Metadata: domain.ObjectMeta{Name: "go-coder", Zone: domain.ZoneRuntime},
				Spec: domain.WorkloadSpec{
					Class:  domain.ClassCodingAgent,
					Grants: domain.GrantSet{ModelGate: domain.ModelGateGrant{Allowed: true, BudgetRef: "b1"}},
				},
			},
			"health-ctrl": {
				Metadata: domain.ObjectMeta{Name: "health-ctrl", Zone: domain.ZoneRuntime},
				Spec: domain.WorkloadSpec{
					Class:  domain.ClassDeterministicController,
					Grants: domain.GrantSet{ModelGate: domain.ModelGateGrant{Allowed: false}},
				},
			},
		},
	}

	for _, w := range bundle.Workloads {
		_ = st.UpsertWorkload(ctx, domain.WorkloadRecord{
			Name: w.Metadata.Name, Zone: w.Metadata.Zone, Class: w.Spec.Class,
			ManifestDigest: "m1", AgentCardJSON: "{}",
		})
	}

	longAdapter := &fakeAdapter{nextState: scruntime.StateRunning}
	codeAdapter := &fakeAdapter{nextState: scruntime.StateCompleted}
	ctrlInner := &fakeAdapter{nextState: scruntime.StateCompleted}
	ctrlAdapter := controller.NewAdapter(ctrlInner)

	eng := policy.NewEngine([]domain.Policy{bundle.Policies["default"]}, nil)
	rec := NewReconciler(Options{
		Store:        st,
		Bundle:       bundle,
		Policy:       eng,
		ModelGateURL: "http://127.0.0.1:7432/gate/model",
		DataGateURL:  "http://127.0.0.1:7432/gate/mcp",
		Adapters: map[domain.WorkloadClass]scruntime.Adapter{
			domain.ClassLongLivedAgent:          longAdapter,
			domain.ClassCodingAgent:             codeAdapter,
			domain.ClassDeterministicController: ctrlAdapter,
		},
	})

	parent, err := rec.SubmitTask(ctx, "triage-agent", "idem-p", `{"issue":7}`)
	if err != nil {
		t.Fatalf("SubmitTask parent: %v", err)
	}
	ctrlTask, err := rec.SubmitTask(ctx, "health-ctrl", "idem-c", `{}`)
	if err != nil {
		t.Fatalf("SubmitTask controller: %v", err)
	}

	// Pass 1: dispatch submitted tasks.
	if err := rec.ReconcileOnce(ctx); err != nil {
		t.Fatalf("ReconcileOnce 1: %v", err)
	}
	if ctrlInner.lastSpec.ModelGateToken != "" || ctrlInner.lastSpec.ModelGateURL != "" {
		t.Fatalf("deterministic controller received Model Gate credentials: %+v", ctrlInner.lastSpec)
	}
	if longAdapter.lastSpec.ModelGateToken == "" {
		t.Fatal("long-lived agent did not receive Model Gate token")
	}

	// Hub-and-spoke child delegation: allowed for go-coder, denied for health-ctrl.
	child, err := rec.ProposeChildTask(ctx, parent.ID, "go-coder", "idem-child", `{"fix":7}`, `{"maxCostUSD":0.50}`)
	if err != nil {
		t.Fatalf("ProposeChildTask allowed: %v", err)
	}
	if child.ParentTaskID != parent.ID {
		t.Fatalf("expected parent %q, got %q", parent.ID, child.ParentTaskID)
	}
	if _, err := rec.ProposeChildTask(ctx, parent.ID, "health-ctrl", "idem-denied", `{}`, `{}`); err == nil {
		t.Fatal("expected delegation to ungranted target to fail")
	}

	// Pass 2: observe working controller -> completed.
	if err := rec.ReconcileOnce(ctx); err != nil {
		t.Fatalf("ReconcileOnce 2: %v", err)
	}
	gotCtrl, _ := st.GetTask(ctx, ctrlTask.ID)
	if gotCtrl.State != domain.TaskStateCompleted {
		t.Fatalf("expected controller completed, got %q", gotCtrl.State)
	}
}
