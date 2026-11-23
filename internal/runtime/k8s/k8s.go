package k8s

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/shadewright-labs/sablecourt/internal/attach"
	scruntime "github.com/shadewright-labs/sablecourt/internal/runtime"
)

type Options struct {
	Client           kubernetes.Interface
	Namespace        string
	RuntimeClassName string
	AttachHub        *attach.Hub
}

// Adapter executes Sablecourt workloads as hardened Kubernetes batch/v1 Jobs
// with zero ServiceAccount token automount, dropped Linux capabilities, and optional
// RuntimeClassName (for gVisor / Kata / agent-sandbox microVMs).
type Adapter struct {
	client           kubernetes.Interface
	namespace        string
	runtimeClassName string
	attachHub        *attach.Hub
}

func (a *Adapter) SetAttachHub(hub *attach.Hub) {
	a.attachHub = hub
}

var _ scruntime.Adapter = (*Adapter)(nil)

func NewFromConfig(kubeconfigPath, namespace, runtimeClassName string) (*Adapter, error) {
	var cfg *rest.Config
	var err error
	if kubeconfigPath != "" {
		cfg, err = clientcmd.BuildConfigFromFlags("", kubeconfigPath)
	} else {
		cfg, err = rest.InClusterConfig()
		if err != nil {
			loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
			cfg, err = clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loadingRules, &clientcmd.ConfigOverrides{}).ClientConfig()
		}
	}
	if err != nil {
		return nil, fmt.Errorf("load kubernetes config: %w", err)
	}
	clientset, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("create kubernetes clientset: %w", err)
	}
	return NewAdapter(Options{
		Client:           clientset,
		Namespace:        namespace,
		RuntimeClassName: runtimeClassName,
	}), nil
}

func NewAdapter(opts Options) *Adapter {
	ns := opts.Namespace
	if ns == "" {
		ns = "default"
	}
	return &Adapter{
		client:           opts.Client,
		namespace:        ns,
		runtimeClassName: opts.RuntimeClassName,
		attachHub:        opts.AttachHub,
	}
}

func (a *Adapter) Start(ctx context.Context, spec scruntime.LaunchSpec) (scruntime.Handle, error) {
	if a.client == nil {
		return scruntime.Handle{}, errors.New("kubernetes client is nil")
	}
	if spec.Workload.Spec.Runtime.Image == "" {
		return scruntime.Handle{}, errors.New("kubernetes runtime requires runtime.image")
	}

	env := []corev1.EnvVar{
		{Name: "SABLECOURT_MANAGED", Value: "1"},
		{Name: "SABLECOURT_TASK_ID", Value: spec.TaskID},
		{Name: "SABLECOURT_WORKLOAD", Value: spec.Workload.Metadata.Name},
	}
	if spec.InputJSON != "" {
		env = append(env, corev1.EnvVar{Name: "SABLECOURT_INPUT_JSON", Value: spec.InputJSON})
	}
	if spec.DataGateURL != "" {
		env = append(env, corev1.EnvVar{Name: "SABLECOURT_DATA_GATE_URL", Value: spec.DataGateURL})
	}
	if spec.TaskToken != "" {
		env = append(env, corev1.EnvVar{Name: "SABLECOURT_TASK_TOKEN", Value: spec.TaskToken})
	}
	if spec.ModelGateURL != "" && spec.ModelGateToken != "" {
		env = append(env, corev1.EnvVar{Name: "SABLECOURT_MODEL_GATE_URL", Value: spec.ModelGateURL})
	}

	falseVal := false
	trueVal := true
	backoffLimit := int32(0)
	readOnlyRoot := spec.Workload.Spec.Runtime.ReadOnlyRootfs

	jobName := sanitizeK8sName("sc-" + spec.TaskID)
	podSpec := corev1.PodSpec{
		RestartPolicy:                corev1.RestartPolicyNever,
		AutomountServiceAccountToken: &falseVal,
		SecurityContext: &corev1.PodSecurityContext{
			RunAsNonRoot: &trueVal,
			SeccompProfile: &corev1.SeccompProfile{
				Type: corev1.SeccompProfileTypeRuntimeDefault,
			},
		},
		Containers: []corev1.Container{
			{
				Name:  "workload",
				Image: spec.Workload.Spec.Runtime.Image,
				Env:   env,
				SecurityContext: &corev1.SecurityContext{
					AllowPrivilegeEscalation: &falseVal,
					ReadOnlyRootFilesystem:   &readOnlyRoot,
					Capabilities: &corev1.Capabilities{
						Drop: []corev1.Capability{"ALL"},
					},
				},
			},
		},
	}
	if a.runtimeClassName != "" {
		rc := a.runtimeClassName
		podSpec.RuntimeClassName = &rc
	}

	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      jobName,
			Namespace: a.namespace,
			Labels: map[string]string{
				"app.kubernetes.io/managed-by": "sablecourt",
				"sablecourt.io/task-id":        sanitizeK8sName(spec.TaskID),
				"sablecourt.io/workload":       sanitizeK8sName(spec.Workload.Metadata.Name),
				"sablecourt.io/zone":           string(spec.Workload.Metadata.Zone),
			},
		},
		Spec: batchv1.JobSpec{
			BackoffLimit: &backoffLimit,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"app.kubernetes.io/managed-by": "sablecourt",
						"sablecourt.io/workload":       sanitizeK8sName(spec.Workload.Metadata.Name),
						"sablecourt.io/zone":           string(spec.Workload.Metadata.Zone),
					},
				},
				Spec: podSpec,
			},
		},
	}

	created, err := a.client.BatchV1().Jobs(a.namespace).Create(ctx, job, metav1.CreateOptions{})
	if err != nil {
		return scruntime.Handle{}, fmt.Errorf("create kubernetes job: %w", err)
	}
	if a.attachHub != nil {
		a.attachHub.Register(spec.TaskID, nil)
		go a.streamPodLogs(spec.TaskID, created.Name)
	}
	return scruntime.Handle{
		AdapterName: "k8s",
		ExternalID:  created.Name,
	}, nil
}

func (a *Adapter) streamPodLogs(taskID, jobName string) {
	defer a.attachHub.Close(taskID)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	var podName string
	for i := 0; i < 60; i++ {
		pods, err := a.client.CoreV1().Pods(a.namespace).List(ctx, metav1.ListOptions{
			LabelSelector: "job-name=" + jobName,
		})
		if err == nil && len(pods.Items) > 0 && pods.Items[0].Status.Phase != corev1.PodPending {
			podName = pods.Items[0].Name
			break
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(1 * time.Second):
		}
	}
	if podName == "" {
		return
	}
	stream, err := a.client.CoreV1().Pods(a.namespace).GetLogs(podName, &corev1.PodLogOptions{
		Container: "workload",
		Follow:    true,
	}).Stream(ctx)
	if err != nil {
		return
	}
	defer stream.Close()

	buf := make([]byte, 4096)
	for {
		n, readErr := stream.Read(buf)
		if n > 0 {
			a.attachHub.WriteOutput(taskID, buf[:n])
		}
		if readErr != nil {
			return
		}
	}
}

func (a *Adapter) Observe(ctx context.Context, handle scruntime.Handle) (scruntime.Observation, error) {
	job, err := a.client.BatchV1().Jobs(a.namespace).Get(ctx, handle.ExternalID, metav1.GetOptions{})
	if err != nil {
		return scruntime.Observation{}, fmt.Errorf("get kubernetes job %s: %w", handle.ExternalID, err)
	}
	obs := scruntime.Observation{
		ObservedAt: time.Now().UTC(),
		State:      scruntime.StateRunning,
	}
	switch {
	case job.Status.Succeeded > 0:
		obs.State = scruntime.StateCompleted
	case job.Status.Failed > 0:
		obs.State = scruntime.StateFailed
		obs.ExitCode = 1
	case job.Spec.Suspend != nil && *job.Spec.Suspend:
		obs.State = scruntime.StateSuspended
	}
	return obs, nil
}

func (a *Adapter) Suspend(ctx context.Context, handle scruntime.Handle) (scruntime.CheckpointRef, error) {
	job, err := a.client.BatchV1().Jobs(a.namespace).Get(ctx, handle.ExternalID, metav1.GetOptions{})
	if err != nil {
		return scruntime.CheckpointRef{}, err
	}
	trueVal := true
	job.Spec.Suspend = &trueVal
	if _, err := a.client.BatchV1().Jobs(a.namespace).Update(ctx, job, metav1.UpdateOptions{}); err != nil {
		return scruntime.CheckpointRef{}, err
	}
	return scruntime.CheckpointRef{
		ID:        "k8s-cp-" + handle.ExternalID,
		CreatedAt: time.Now().UTC(),
	}, nil
}

func (a *Adapter) Resume(ctx context.Context, handle scruntime.Handle, _ scruntime.CheckpointRef) error {
	job, err := a.client.BatchV1().Jobs(a.namespace).Get(ctx, handle.ExternalID, metav1.GetOptions{})
	if err != nil {
		return err
	}
	falseVal := false
	job.Spec.Suspend = &falseVal
	_, err = a.client.BatchV1().Jobs(a.namespace).Update(ctx, job, metav1.UpdateOptions{})
	return err
}

func (a *Adapter) Stop(ctx context.Context, handle scruntime.Handle) error {
	policy := metav1.DeletePropagationBackground
	return a.client.BatchV1().Jobs(a.namespace).Delete(ctx, handle.ExternalID, metav1.DeleteOptions{
		PropagationPolicy: &policy,
	})
}

func (a *Adapter) CollectArtifacts(_ context.Context, _ scruntime.Handle) ([]scruntime.Artifact, error) {
	return nil, nil
}

func sanitizeK8sName(s string) string {
	s = strings.ToLower(s)
	var b strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	out := strings.Trim(b.String(), "-")
	if len(out) > 63 {
		out = strings.TrimRight(out[:63], "-")
	}
	if out == "" {
		return "sablecourt-job"
	}
	return out
}
