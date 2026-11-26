package k8s

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/synctest"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/shadewright-labs/sablecourt/internal/attach"
	"github.com/shadewright-labs/sablecourt/internal/domain"
	scruntime "github.com/shadewright-labs/sablecourt/internal/runtime"
)

func TestNewFromConfig(t *testing.T) {
	t.Run("missing kubeconfig returns error", func(t *testing.T) {
		if _, err := NewFromConfig(filepath.Join(t.TempDir(), "absent"), "ns", ""); err == nil || !strings.Contains(err.Error(), "load kubernetes config") {
			t.Fatalf("NewFromConfig missing = %v", err)
		}
	})

	t.Run("valid kubeconfig constructs adapter", func(t *testing.T) {
		cfgPath := filepath.Join(t.TempDir(), "kubeconfig")
		const kubeYAML = `apiVersion: v1
kind: Config
clusters:
- name: local
  cluster:
    server: https://127.0.0.1:6443
contexts:
- name: local
  context:
    cluster: local
    user: local
current-context: local
users:
- name: local
  user:
    token: fixture-token
`
		if err := os.WriteFile(cfgPath, []byte(kubeYAML), 0o600); err != nil {
			t.Fatal(err)
		}
		a, err := NewFromConfig(cfgPath, "sablecourt-ns", "kata")
		if err != nil {
			t.Fatalf("NewFromConfig valid: %v", err)
		}
		if a.namespace != "sablecourt-ns" || a.runtimeClassName != "kata" || a.client == nil || a.restConfig == nil {
			t.Fatalf("adapter = %+v", a)
		}
	})

	t.Run("default loader fails when KUBECONFIG is absent", func(t *testing.T) {
		t.Setenv("KUBECONFIG", filepath.Join(t.TempDir(), "none"))
		t.Setenv("HOME", t.TempDir())
		t.Setenv("KUBERNETES_SERVICE_HOST", "")
		t.Setenv("KUBERNETES_SERVICE_PORT", "")
		if _, err := NewFromConfig("", "", ""); err == nil || !strings.Contains(err.Error(), "load kubernetes config") {
			t.Fatalf("NewFromConfig empty = %v", err)
		}
	})
}

func TestStartValidationEnvAndWorkloadKind(t *testing.T) {
	ctx := context.Background()

	t.Run("validation refusals", func(t *testing.T) {
		if _, err := NewAdapter(Options{}).Start(ctx, scruntime.LaunchSpec{}); err == nil || !strings.Contains(err.Error(), "client is nil") {
			t.Fatalf("nil client Start = %v", err)
		}
		a := NewAdapter(Options{Client: fake.NewSimpleClientset()})
		if _, err := a.Start(ctx, scruntime.LaunchSpec{TaskID: "t1"}); err == nil || !strings.Contains(err.Error(), "runtime.image") {
			t.Fatalf("missing image Start = %v", err)
		}
	})

	t.Run("env population and workload kind selection", func(t *testing.T) {
		cases := []struct {
			name        string
			runtimeType string
			class       domain.WorkloadClass
			modelToken  string
			wantDeploy  bool
			wantModel   bool
		}{
			{"k8s-deployment creates deployment", "k8s-deployment", domain.ClassLongLivedAgent, "tok", true, true},
			{"deployment for non-coding creates deployment", "deployment", domain.ClassLongLivedAgent, "", true, false},
			{"deployment for coding agent stays job", "deployment", domain.ClassCodingAgent, "tok", false, true},
			{"default runtime creates job", "k8s", domain.ClassLongLivedAgent, "", false, false},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				client := fake.NewSimpleClientset()
				a := NewAdapter(Options{Client: client})
				spec := scruntime.LaunchSpec{
					TaskID:         "task_01",
					InputJSON:      `{"x":1}`,
					DataGateURL:    "http://gate.local/mcp",
					TaskToken:      "sc_task_1",
					ModelGateURL:   "http://gate.local/model",
					ModelGateToken: tc.modelToken,
					Workload: domain.Workload{
						Metadata: domain.ObjectMeta{Name: "Worker_1", Zone: domain.ZoneRuntime},
						Spec: domain.WorkloadSpec{
							Class: tc.class,
							Runtime: domain.RuntimeConfig{
								Type:           tc.runtimeType,
								Image:          "agent:1",
								ReadOnlyRootfs: true,
							},
						},
					},
				}
				h, err := a.Start(ctx, spec)
				if err != nil {
					t.Fatalf("Start: %v", err)
				}
				if isDeploy := strings.HasPrefix(h.ExternalID, "deploy/"); isDeploy != tc.wantDeploy {
					t.Fatalf("ExternalID = %q, wantDeploy=%t", h.ExternalID, tc.wantDeploy)
				}
				var env []corev1.EnvVar
				if tc.wantDeploy {
					depName := strings.TrimPrefix(h.ExternalID, "deploy/")
					dep, err := client.AppsV1().Deployments("default").Get(ctx, depName, metav1.GetOptions{})
					if err != nil {
						t.Fatalf("Get deployment: %v", err)
					}
					if dep.Spec.Template.Spec.RestartPolicy != corev1.RestartPolicyAlways {
						t.Errorf("RestartPolicy = %s, want Always", dep.Spec.Template.Spec.RestartPolicy)
					}
					env = dep.Spec.Template.Spec.Containers[0].Env
				} else {
					job, err := client.BatchV1().Jobs("default").Get(ctx, h.ExternalID, metav1.GetOptions{})
					if err != nil {
						t.Fatalf("Get job: %v", err)
					}
					if job.Spec.Template.Spec.RestartPolicy != corev1.RestartPolicyNever {
						t.Errorf("RestartPolicy = %s, want Never", job.Spec.Template.Spec.RestartPolicy)
					}
					env = job.Spec.Template.Spec.Containers[0].Env
				}
				envMap := make(map[string]string, len(env))
				for _, e := range env {
					envMap[e.Name] = e.Value
				}
				if envMap["SABLECOURT_INPUT_JSON"] != spec.InputJSON || envMap["SABLECOURT_DATA_GATE_URL"] != spec.DataGateURL || envMap["SABLECOURT_TASK_TOKEN"] != spec.TaskToken {
					t.Errorf("unexpected env map: %+v", envMap)
				}
				_, hasModel := envMap["SABLECOURT_MODEL_GATE_URL"]
				if hasModel != tc.wantModel {
					t.Errorf("has SABLECOURT_MODEL_GATE_URL = %t, want %t", hasModel, tc.wantModel)
				}
			})
		}
	})

	t.Run("create API errors propagate", func(t *testing.T) {
		for _, kind := range []struct {
			rtType   string
			resource string
			want     string
		}{
			{"k8s-deployment", "deployments", "create kubernetes deployment"},
			{"k8s", "jobs", "create kubernetes job"},
		} {
			t.Run(kind.resource, func(t *testing.T) {
				client := fake.NewSimpleClientset()
				client.PrependReactor("create", kind.resource, func(k8stesting.Action) (bool, k8sruntime.Object, error) {
					return true, nil, errors.New("quota exceeded")
				})
				a := NewAdapter(Options{Client: client})
				spec := scruntime.LaunchSpec{
					TaskID: "t-err",
					Workload: domain.Workload{Spec: domain.WorkloadSpec{
						Runtime: domain.RuntimeConfig{Type: kind.rtType, Image: "img:1"},
					}},
				}
				if _, err := a.Start(ctx, spec); err == nil || !strings.Contains(err.Error(), kind.want) {
					t.Fatalf("Start error = %v, want %q", err, kind.want)
				}
			})
		}
	})
}

func TestDeploymentAndJobLifecycleErrors(t *testing.T) {
	ctx := context.Background()

	t.Run("deployment observe, suspend, resume, stop", func(t *testing.T) {
		client := fake.NewSimpleClientset()
		a := NewAdapter(Options{Client: client, Namespace: "agents"})
		h, err := a.Start(ctx, scruntime.LaunchSpec{
			TaskID: "dep-1",
			Workload: domain.Workload{
				Metadata: domain.ObjectMeta{Name: "ctrl", Zone: domain.ZoneRuntime},
				Spec:     domain.WorkloadSpec{Runtime: domain.RuntimeConfig{Type: "k8s-deployment", Image: "ctrl:1"}},
			},
		})
		if err != nil {
			t.Fatalf("Start deployment: %v", err)
		}

		obs, err := a.Observe(ctx, h)
		if err != nil || obs.State != scruntime.StateRunning {
			t.Fatalf("initial Observe = %+v, %v", obs, err)
		}

		cp, err := a.Suspend(ctx, h)
		if err != nil || !strings.HasPrefix(cp.ID, "k8s-cp-") {
			t.Fatalf("Suspend = %+v, %v", cp, err)
		}
		obs, err = a.Observe(ctx, h)
		if err != nil || obs.State != scruntime.StateSuspended {
			t.Fatalf("suspended Observe = %+v, %v", obs, err)
		}

		if err := a.Resume(ctx, h, cp); err != nil {
			t.Fatalf("Resume: %v", err)
		}
		obs, err = a.Observe(ctx, h)
		if err != nil || obs.State != scruntime.StateRunning {
			t.Fatalf("resumed Observe = %+v, %v", obs, err)
		}

		if arts, err := a.CollectArtifacts(ctx, h); err != nil || arts != nil {
			t.Fatalf("CollectArtifacts = %v, %v", arts, err)
		}

		if err := a.Stop(ctx, h); err != nil {
			t.Fatalf("Stop deployment: %v", err)
		}
		if _, err := a.Observe(ctx, h); err == nil || !strings.Contains(err.Error(), "get kubernetes deployment") {
			t.Fatalf("Observe after Stop = %v", err)
		}
		if _, err := a.Suspend(ctx, h); err == nil {
			t.Error("Suspend succeeded on deleted deployment")
		}
		if err := a.Resume(ctx, h, cp); err == nil {
			t.Error("Resume succeeded on deleted deployment")
		}
	})

	t.Run("job failed state and missing/update error paths", func(t *testing.T) {
		client := fake.NewSimpleClientset()
		a := NewAdapter(Options{Client: client})
		h, err := a.Start(ctx, scruntime.LaunchSpec{
			TaskID:   "job-fail",
			Workload: domain.Workload{Spec: domain.WorkloadSpec{Runtime: domain.RuntimeConfig{Image: "img:1"}}},
		})
		if err != nil {
			t.Fatal(err)
		}
		job, err := client.BatchV1().Jobs("default").Get(ctx, h.ExternalID, metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		job.Status.Failed = 1
		if _, err := client.BatchV1().Jobs("default").UpdateStatus(ctx, job, metav1.UpdateOptions{}); err != nil {
			t.Fatal(err)
		}
		obs, err := a.Observe(ctx, h)
		if err != nil || obs.State != scruntime.StateFailed || obs.ExitCode != 1 {
			t.Fatalf("failed job Observe = %+v, %v", obs, err)
		}

		// Update failure reactor on Suspend.
		client.PrependReactor("update", "jobs", func(k8stesting.Action) (bool, k8sruntime.Object, error) {
			return true, nil, errors.New("conflict")
		})
		if _, err := a.Suspend(ctx, h); err == nil || !strings.Contains(err.Error(), "conflict") {
			t.Fatalf("Suspend update failure = %v", err)
		}
		if err := a.Resume(ctx, h, scruntime.CheckpointRef{}); err == nil || !strings.Contains(err.Error(), "conflict") {
			t.Fatalf("Resume update failure = %v", err)
		}

		missing := scruntime.Handle{ExternalID: "sc-absent"}
		if _, err := a.Observe(ctx, missing); err == nil {
			t.Error("Observe succeeded on missing job")
		}
		if _, err := a.Suspend(ctx, missing); err == nil {
			t.Error("Suspend succeeded on missing job")
		}
		if err := a.Resume(ctx, missing, scruntime.CheckpointRef{}); err == nil {
			t.Error("Resume succeeded on missing job")
		}

		// Update failure reactor on Deployment Suspend.
		client2 := fake.NewSimpleClientset()
		a2 := NewAdapter(Options{Client: client2})
		hd, err := a2.Start(ctx, scruntime.LaunchSpec{
			TaskID:   "dep-conflict",
			Workload: domain.Workload{Spec: domain.WorkloadSpec{Runtime: domain.RuntimeConfig{Type: "k8s-deployment", Image: "img:1"}}},
		})
		if err != nil {
			t.Fatal(err)
		}
		client2.PrependReactor("update", "deployments", func(k8stesting.Action) (bool, k8sruntime.Object, error) {
			return true, nil, errors.New("dep conflict")
		})
		if _, err := a2.Suspend(ctx, hd); err == nil || !strings.Contains(err.Error(), "dep conflict") {
			t.Fatalf("Deployment Suspend conflict = %v", err)
		}
	})
}

func TestSanitizeK8sName(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"Simple-Name-1", "simple-name-1"},
		{"___Task/With Spaces!___", "task-with-spaces"},
		{"!!!", "sablecourt-job"},
		{"", "sablecourt-job"},
		{strings.Repeat("a", 62) + "---extra", strings.Repeat("a", 62)},
		{strings.Repeat("b", 80), strings.Repeat("b", 63)},
	}
	for _, tc := range cases {
		if got := sanitizeK8sName(tc.in); got != tc.want {
			t.Errorf("sanitizeK8sName(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestStreamPodLogsAndHubWriter(t *testing.T) {
	t.Run("streams logs from running pod into attach hub", func(t *testing.T) {
		ctx := context.Background()
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "sc-task-log-pod",
				Namespace: "default",
				Labels:    map[string]string{"job-name": "sc-task-log"},
			},
			Status: corev1.PodStatus{Phase: corev1.PodRunning},
		}
		client := fake.NewSimpleClientset(pod)
		hub := attach.NewHub()
		a := NewAdapter(Options{Client: client, AttachHub: hub})
		hub.Register("task-log", nil)
		history, ch, _, unsub, err := hub.Subscribe("task-log")
		if err != nil {
			t.Fatalf("Subscribe: %v", err)
		}
		defer unsub()

		a.streamPodLogsBySelector(ctx, "task-log", "job-name=sc-task-log")

		var got strings.Builder
		got.WriteString(history)
		for chunk := range ch {
			got.WriteString(chunk)
		}
		if got.String() != "fake logs" {
			t.Fatalf("streamed pod logs = %q, want %q", got.String(), "fake logs")
		}
		if active, _, _ := hub.Status("task-log"); active {
			t.Error("hub session remained active after streamPodLogsBySelector returned")
		}
	})

	t.Run("gives up after 60 poll attempts when no pod becomes ready", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			hub := attach.NewHub()
			a := NewAdapter(Options{Client: fake.NewSimpleClientset(), AttachHub: hub})
			hub.Register("task-timeout", nil)
			done := make(chan struct{})
			go func() {
				defer close(done)
				a.streamPodLogsBySelector(t.Context(), "task-timeout", "job-name=missing")
			}()
			<-done
			if active, _, _ := hub.Status("task-timeout"); active {
				t.Error("hub session remained active after poll budget exhausted")
			}
		})
	})

	t.Run("hubWriter writes to attach hub", func(t *testing.T) {
		hub := attach.NewHub()
		hub.Register("t-hw", nil)
		history, ch, _, unsub, err := hub.Subscribe("t-hw")
		if err != nil {
			t.Fatal(err)
		}
		defer unsub()
		w := &hubWriter{hub: hub, taskID: "t-hw"}
		n, err := w.Write([]byte("line1\n"))
		if err != nil || n != 6 {
			t.Fatalf("Write = %d, %v", n, err)
		}
		hub.Close("t-hw")
		var got strings.Builder
		got.WriteString(history)
		for chunk := range ch {
			got.WriteString(chunk)
		}
		if got.String() != "line1\n" {
			t.Fatalf("hubWriter output = %q, want %q", got.String(), "line1\n")
		}
	})
}
