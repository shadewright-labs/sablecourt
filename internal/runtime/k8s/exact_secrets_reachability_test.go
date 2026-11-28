package k8s

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/shadewright-labs/sablecourt/internal/domain"
	"github.com/shadewright-labs/sablecourt/internal/operatortoken"
	"github.com/shadewright-labs/sablecourt/internal/redact"
	scruntime "github.com/shadewright-labs/sablecourt/internal/runtime"
)

func synthK8sSecret(parts ...string) string {
	return strings.Join(parts, "")
}

func TestK8sWorkloadsCannotReachDaemonSecrets(t *testing.T) {
	ctx := context.Background()
	stateDir := filepath.Join(t.TempDir(), "k8s-state")
	opToken, err := operatortoken.LoadOrCreate(stateDir)
	if err != nil {
		t.Fatalf("operatortoken.LoadOrCreate: %v", err)
	}
	providerKey := synthK8sSecret("prov", "_k8s_", "reach_112233")
	mcpSecret := synthK8sSecret("upmcp", "_k8s_", "reach_445566")
	pgPassword := synthK8sSecret("pgpw", "_k8s_", "reach_778899")
	otherToken := synthK8sSecret("sc_", "task_", "99887766554433221100ffeeddccbbaa")

	t.Setenv("OPENAI_API_KEY", providerKey)
	t.Setenv("GITHUB_TOKEN", mcpSecret)
	t.Setenv("SABLECOURT_OPERATOR_TOKEN", opToken)
	t.Setenv("SABLECOURT_POSTGRES_DSN", "postgres://svc:"+pgPassword+"@127.0.0.1:5432/sablecourt")

	daemonSecrets := []string{opToken, providerKey, mcpSecret, pgPassword, otherToken}
	redact.Register(daemonSecrets...)

	for _, runtimeType := range []string{"docker", "k8s-deployment"} {
		t.Run(runtimeType, func(t *testing.T) {
			fakeClient := fake.NewSimpleClientset()
			adapter := NewAdapter(Options{Client: fakeClient, Namespace: "sablecourt-workloads"})
			ownTok := synthK8sSecret("sc_", "task_", "11223344556677889900aabbccddeeff")
			handle, err := adapter.Start(ctx, scruntime.LaunchSpec{
				TaskID:         "k8s-reach-" + runtimeType,
				InputJSON:      `{"prompt":"run"}`,
				ModelGateURL:   "http://sablecourtd:7432/gate/model",
				ModelGateToken: ownTok,
				DataGateURL:    "http://sablecourtd:7432/gate/mcp",
				TaskToken:      ownTok,
				Workload: domain.Workload{
					Metadata: domain.ObjectMeta{Name: "agent-k8s", Zone: domain.ZoneRuntime},
					Spec: domain.WorkloadSpec{
						Class:   domain.ClassLongLivedAgent,
						Runtime: domain.RuntimeConfig{Type: runtimeType, Image: "sablecourt-agent:0.1.0", ReadOnlyRootfs: true},
					},
				},
			})
			if err != nil {
				t.Fatalf("Start: %v", err)
			}

			var envVals []string
			var volCount int
			var automount *bool
			if runtimeType == "k8s-deployment" {
				depName := strings.TrimPrefix(handle.ExternalID, "deploy/")
				dep, err := fakeClient.AppsV1().Deployments("sablecourt-workloads").Get(ctx, depName, metav1.GetOptions{})
				if err != nil {
					t.Fatalf("Get Deployment: %v", err)
				}
				pod := dep.Spec.Template.Spec
				automount = pod.AutomountServiceAccountToken
				volCount = len(pod.Volumes) + len(pod.Containers[0].VolumeMounts)
				for _, e := range pod.Containers[0].Env {
					envVals = append(envVals, e.Name+"="+e.Value)
				}
			} else {
				job, err := fakeClient.BatchV1().Jobs("sablecourt-workloads").Get(ctx, handle.ExternalID, metav1.GetOptions{})
				if err != nil {
					t.Fatalf("Get Job: %v", err)
				}
				pod := job.Spec.Template.Spec
				automount = pod.AutomountServiceAccountToken
				volCount = len(pod.Volumes) + len(pod.Containers[0].VolumeMounts)
				for _, e := range pod.Containers[0].Env {
					envVals = append(envVals, e.Name+"="+e.Value)
				}
			}
			if automount == nil || *automount {
				t.Fatal("expected AutomountServiceAccountToken=false")
			}
			if volCount != 0 {
				t.Fatalf("expected 0 host/secret volumes, got %d", volCount)
			}
			joined := strings.Join(envVals, "\n")
			for _, sec := range daemonSecrets {
				if strings.Contains(joined, sec) {
					t.Fatalf("k8s %s env exposed daemon-held secret", runtimeType)
				}
			}
		})
	}
}
