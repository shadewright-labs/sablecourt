package k8s

import (
	"context"
	"slices"
	"testing"
	"testing/synctest"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/shadewright-labs/sablecourt/internal/attach"
	"github.com/shadewright-labs/sablecourt/internal/domain"
	scruntime "github.com/shadewright-labs/sablecourt/internal/runtime"
)

func TestK8sAdapterHardenedJobLifecycle(t *testing.T) {
	ctx := context.Background()
	fakeClient := fake.NewSimpleClientset()
	adapter := NewAdapter(Options{
		Client:           fakeClient,
		Namespace:        "sablecourt-workloads",
		RuntimeClassName: "gvisor",
	})

	handle, err := adapter.Start(ctx, scruntime.LaunchSpec{
		TaskID: "task_abc123",
		Workload: domain.Workload{
			Metadata: domain.ObjectMeta{Name: "triage-agent", Zone: domain.ZoneRuntime},
			Spec: domain.WorkloadSpec{
				Class: domain.ClassLongLivedAgent,
				Runtime: domain.RuntimeConfig{
					Type:           "docker",
					Image:          "sablecourt-agent:0.1.0",
					ReadOnlyRootfs: true,
				},
			},
		},
		ModelGateURL:   "http://sablecourtd.sablecourt.svc:7432/gate/model",
		ModelGateToken: "sc_task_scoped",
		TaskToken:      "sc_task_scoped",
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	job, err := fakeClient.BatchV1().Jobs("sablecourt-workloads").Get(ctx, handle.ExternalID, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Get created job: %v", err)
	}

	podSpec := job.Spec.Template.Spec
	if podSpec.AutomountServiceAccountToken == nil || *podSpec.AutomountServiceAccountToken {
		t.Fatal("expected AutomountServiceAccountToken=false")
	}
	if podSpec.RuntimeClassName == nil || *podSpec.RuntimeClassName != "gvisor" {
		t.Fatalf("expected RuntimeClassName=gvisor, got %v", podSpec.RuntimeClassName)
	}
	sc := podSpec.Containers[0].SecurityContext
	if sc == nil || sc.AllowPrivilegeEscalation == nil || *sc.AllowPrivilegeEscalation {
		t.Fatal("expected AllowPrivilegeEscalation=false")
	}
	if sc.ReadOnlyRootFilesystem == nil || !*sc.ReadOnlyRootFilesystem {
		t.Fatal("expected ReadOnlyRootFilesystem=true")
	}
	if !slices.Contains(sc.Capabilities.Drop, corev1.Capability("ALL")) {
		t.Fatalf("expected Capabilities.Drop ALL, got %v", sc.Capabilities.Drop)
	}

	// Suspend & Resume
	cp, err := adapter.Suspend(ctx, handle)
	if err != nil || cp.ID == "" {
		t.Fatalf("Suspend: %+v err=%v", cp, err)
	}
	obs, err := adapter.Observe(ctx, handle)
	if err != nil || obs.State != scruntime.StateSuspended {
		t.Fatalf("expected StateSuspended, got %+v err=%v", obs, err)
	}
	if err := adapter.Resume(ctx, handle, cp); err != nil {
		t.Fatalf("Resume: %v", err)
	}

	// Simulate completion
	job, _ = fakeClient.BatchV1().Jobs("sablecourt-workloads").Get(ctx, handle.ExternalID, metav1.GetOptions{})
	job.Status.Succeeded = 1
	_, _ = fakeClient.BatchV1().Jobs("sablecourt-workloads").UpdateStatus(ctx, job, metav1.UpdateOptions{})
	obs, err = adapter.Observe(ctx, handle)
	if err != nil || obs.State != scruntime.StateCompleted {
		t.Fatalf("expected StateCompleted, got %+v err=%v", obs, err)
	}

	if err := adapter.Stop(ctx, handle); err != nil {
		t.Fatalf("Stop: %v", err)
	}
}

func TestLogCancellation(t *testing.T) {
	for _, kind := range []string{"k8s-deployment", "job"} {
		t.Run(kind, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				hub := attach.NewHub()
				a := NewAdapter(Options{Client: fake.NewSimpleClientset(), AttachHub: hub})
				a.SetAttachHub(ctx, hub)
				start, finishStart := context.WithCancel(ctx)
				defer finishStart()
				spec := scruntime.LaunchSpec{TaskID: "task"}
				spec.Workload.Spec.Runtime = domain.RuntimeConfig{Type: kind, Image: "fixture"}
				if _, err := a.Start(start, spec); err != nil {
					t.Fatal(err)
				}
				finishStart()
				synctest.Wait()
				if active, _, _ := hub.Status(spec.TaskID); !active {
					t.Fatal("finishing Start canceled live attach")
				}
				cancel()
				synctest.Wait()
				if active, _, _ := hub.Status(spec.TaskID); active {
					t.Error("pod log lookup ignored cancellation")
				}
			})
		})
	}
}
