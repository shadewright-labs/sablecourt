package k8s

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/shadewright-labs/sablecourt/internal/domain"
	scruntime "github.com/shadewright-labs/sablecourt/internal/runtime"
	"github.com/shadewright-labs/sablecourt/internal/store/storetest"
)

// discardAttach stands in for a pod attach that reads all of stdin.
func discardAttach(_ context.Context, _ string, stdin io.Reader, _ io.Writer) error {
	_, err := io.Copy(io.Discard, stdin)
	return err
}

func runningPod(name, selectorKey, selectorValue string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default", Labels: map[string]string{selectorKey: selectorValue}},
		Status:     corev1.PodStatus{Phase: corev1.PodRunning},
	}
}

func inputSpec(taskID, runtimeType, input string) scruntime.LaunchSpec {
	return scruntime.LaunchSpec{
		TaskID:    taskID,
		InputJSON: input,
		Workload: domain.Workload{
			Metadata: domain.ObjectMeta{Name: "worker", Zone: domain.ZoneRuntime},
			Spec:     domain.WorkloadSpec{Class: domain.ClassLongLivedAgent, Runtime: domain.RuntimeConfig{Type: runtimeType, Image: "agent:1"}},
		},
	}
}

// TestK8sInputOnStdinNeverInPodSpec launches every corpus input as a Job and
// a Deployment and checks that the created object (what anyone who can read
// pods sees) carries neither the input nor its secret, and that the pod's
// stdin receives the input as one JSON line (issue 261).
func TestK8sInputOnStdinNeverInPodSpec(t *testing.T) {
	ctx := context.Background()
	for _, runtimeType := range []string{"k8s", "k8s-deployment"} {
		for _, c := range storetest.TaskInputCorpus() {
			taskID := "task-input"
			key, value := "job-name", "sc-task-input"
			if runtimeType == "k8s-deployment" {
				key, value = "sablecourt.io/deployment", "sc-dep-task-input"
			}
			client := fake.NewSimpleClientset(runningPod("pod-1", key, value))
			a := NewAdapter(Options{Client: client})
			got := make(chan string, 1)
			a.attachPod = func(_ context.Context, pod string, stdin io.Reader, out io.Writer) error {
				line, err := bufio.NewReader(stdin).ReadString('\n')
				if pod != "pod-1" || out != nil {
					t.Errorf("attach pod=%q out=%v", pod, out)
				}
				got <- line
				return err
			}
			h, err := a.Start(ctx, inputSpec(taskID, runtimeType, c.Input))
			if err != nil {
				t.Fatalf("%s %s: Start: %v", runtimeType, c.Name, err)
			}
			var obj any
			if runtimeType == "k8s-deployment" {
				obj, err = client.AppsV1().Deployments("default").Get(ctx, strings.TrimPrefix(h.ExternalID, "deploy/"), metav1.GetOptions{})
			} else {
				obj, err = client.BatchV1().Jobs("default").Get(ctx, h.ExternalID, metav1.GetOptions{})
			}
			if err != nil {
				t.Fatalf("get created object: %v", err)
			}
			raw, _ := json.Marshal(obj)
			if strings.Contains(string(raw), c.Secret.Body) || strings.Contains(string(raw), "SABLECOURT_INPUT_JSON") {
				t.Errorf("%s %s: pod spec carries the input: %s", runtimeType, c.Name, raw)
			}
			select {
			case line := <-got:
				want, _ := scruntime.StdinInputLine(c.Input)
				if line != want {
					t.Errorf("%s %s: stdin line = %q, want %q", runtimeType, c.Name, line, want)
				}
			case <-time.After(5 * time.Second):
				t.Fatalf("%s %s: input never attached", runtimeType, c.Name)
			}
		}
	}
}

// TestK8sInputNotDeliveredFailsClosed: with no attach the launch is refused
// before anything is created; an attach that fails before the input is read
// stops the Job and Observe reports input_not_delivered.
func TestK8sInputNotDeliveredFailsClosed(t *testing.T) {
	ctx := context.Background()
	t.Run("no attach configured", func(t *testing.T) {
		client := fake.NewSimpleClientset()
		a := NewAdapter(Options{Client: client})
		if _, err := a.Start(ctx, inputSpec("task-noattach", "k8s", `{"prompt":"x"}`)); !errors.Is(err, scruntime.ErrInputNotDelivered) {
			t.Fatalf("Start = %v, want ErrInputNotDelivered", err)
		}
		jobs, _ := client.BatchV1().Jobs("default").List(ctx, metav1.ListOptions{})
		if len(jobs.Items) != 0 {
			t.Fatalf("a Job was created without a way to deliver its input: %d", len(jobs.Items))
		}
	})
	t.Run("attach fails", func(t *testing.T) {
		client := fake.NewSimpleClientset(runningPod("pod-2", "job-name", "sc-task-fail"))
		a := NewAdapter(Options{Client: client})
		attached := make(chan struct{})
		a.attachPod = func(context.Context, string, io.Reader, io.Writer) error {
			defer close(attached)
			return errors.New("attach refused")
		}
		h, err := a.Start(ctx, inputSpec("task-fail", "k8s", `{"prompt":"x"}`))
		if err != nil {
			t.Fatalf("Start: %v", err)
		}
		<-attached
		deadline := time.Now().Add(5 * time.Second)
		jobGone := func() bool {
			_, err := client.BatchV1().Jobs("default").Get(ctx, h.ExternalID, metav1.GetOptions{})
			return err != nil
		}
		for (!a.inputFailed(h.ExternalID) || !jobGone()) && time.Now().Before(deadline) {
			time.Sleep(10 * time.Millisecond)
		}
		obs, err := a.Observe(ctx, h)
		if err != nil || obs.State != scruntime.StateFailed || obs.Message != "input_not_delivered" {
			t.Fatalf("Observe = %+v, %v; want failed input_not_delivered", obs, err)
		}
		if !jobGone() {
			t.Fatal("the Job of an undelivered input was not stopped")
		}
	})
}
