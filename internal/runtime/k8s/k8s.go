package k8s

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/remotecommand"

	"github.com/shadewright-labs/sablecourt/internal/attach"
	"github.com/shadewright-labs/sablecourt/internal/domain"
	"github.com/shadewright-labs/sablecourt/internal/redact"
	scruntime "github.com/shadewright-labs/sablecourt/internal/runtime"
)

type Options struct {
	Client           kubernetes.Interface
	RestConfig       *rest.Config
	Namespace        string
	RuntimeClassName string
	AttachHub        *attach.Hub
}

// Adapter executes Sablecourt workloads as hardened Kubernetes batch/v1 Jobs
// with zero ServiceAccount token automount, dropped Linux capabilities, and optional
// RuntimeClassName (for gVisor / Kata / agent-sandbox microVMs).
type Adapter struct {
	client           kubernetes.Interface
	restConfig       *rest.Config
	namespace        string
	runtimeClassName string
	attachHub        *attach.Hub
	attachCtx        context.Context
	// attachPod attaches to the workload container of a pod; nil means the
	// SPDY attach through restConfig. Tests replace it.
	attachPod func(ctx context.Context, pod string, stdin io.Reader, out io.Writer) error

	mu sync.Mutex
	// undelivered holds the external IDs of launches whose input could not
	// be written to the pod's stdin; Observe reports them failed.
	undelivered map[string]bool
}

func (a *Adapter) SetAttachHub(ctx context.Context, hub *attach.Hub) {
	a.attachCtx, a.attachHub = ctx, hub
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
	if err := errors.Join(
		redact.RegisterSetting("kubernetes bearer token", cfg.BearerToken),
		redact.RegisterSetting("kubernetes password", cfg.Password),
		redact.RegisterSettingBytes("kubernetes client key", cfg.KeyData),
	); err != nil {
		return nil, err
	}
	clientset, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("create kubernetes clientset: %w", err)
	}
	return NewAdapter(Options{
		Client:           clientset,
		RestConfig:       cfg,
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
		restConfig:       opts.RestConfig,
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
	// The input goes in on stdin through a pod attach, never in the pod
	// spec, where anyone who can read pods sees it (issue 261).
	inputLine, err := scruntime.StdinInputLine(spec.InputJSON)
	if err != nil {
		return scruntime.Handle{}, err
	}
	deliver := inputLine != ""
	if deliver {
		if !a.canAttach() {
			return scruntime.Handle{}, fmt.Errorf("no pod attach configured: %w", scruntime.ErrInputNotDelivered)
		}
		env = append(env, corev1.EnvVar{Name: scruntime.InputStdinEnv, Value: "1"})
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
	ttlAfterFinished := int32(3600)
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
				Stdin: a.attachHub != nil || deliver,
				// Without Takeover, stdin closes when the input attach ends.
				StdinOnce: deliver && a.attachHub == nil,
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

	// The watcher outlives Start, whose context ends when Start returns.
	logCtx := context.WithoutCancel(ctx)
	if a.attachCtx != nil {
		logCtx = a.attachCtx
	}

	// Persistent Deployment mode for long-lived agents/controllers when runtime.type == "k8s-deployment".
	if spec.Workload.Spec.Runtime.Type == "k8s-deployment" || (spec.Workload.Spec.Runtime.Type == "deployment" && spec.Workload.Spec.Class != domain.ClassCodingAgent) {
		podSpec.RestartPolicy = corev1.RestartPolicyAlways
		replicas := int32(1)
		depName := sanitizeK8sName("sc-dep-" + spec.TaskID)
		matchLabels := map[string]string{
			"app.kubernetes.io/managed-by": "sablecourt",
			"sablecourt.io/deployment":     depName,
			"sablecourt.io/workload":       sanitizeK8sName(spec.Workload.Metadata.Name),
			"sablecourt.io/zone":           string(spec.Workload.Metadata.Zone),
		}
		dep := &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{
				Name:      depName,
				Namespace: a.namespace,
				Labels:    matchLabels,
			},
			Spec: appsv1.DeploymentSpec{
				Replicas: &replicas,
				Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"sablecourt.io/deployment": depName}},
				Template: corev1.PodTemplateSpec{
					ObjectMeta: metav1.ObjectMeta{Labels: matchLabels},
					Spec:       podSpec,
				},
			},
		}
		created, err := a.client.AppsV1().Deployments(a.namespace).Create(ctx, dep, metav1.CreateOptions{})
		if err != nil {
			return scruntime.Handle{}, fmt.Errorf("create kubernetes deployment: %w", err)
		}
		handle := scruntime.Handle{
			AdapterName: "k8s",
			ExternalID:  "deploy/" + created.Name,
		}
		if a.attachHub != nil {
			a.attachHub.Register(spec.TaskID, nil)
		}
		if a.attachHub != nil || deliver {
			go a.watchPod(logCtx, spec.TaskID, "sablecourt.io/deployment="+created.Name, handle, inputLine)
		}
		return handle, nil
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
			BackoffLimit:            &backoffLimit,
			TTLSecondsAfterFinished: &ttlAfterFinished,
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
	handle := scruntime.Handle{
		AdapterName: "k8s",
		ExternalID:  created.Name,
	}
	if a.attachHub != nil {
		a.attachHub.Register(spec.TaskID, nil)
	}
	if a.attachHub != nil || deliver {
		go a.watchPod(logCtx, spec.TaskID, "job-name="+created.Name, handle, inputLine)
	}
	return handle, nil
}

// inputPodPolls is how many one-second polls the input delivery waits for
// the pod to leave Pending (image pulls included) before it gives up.
const inputPodPolls = 300

func (a *Adapter) canAttach() bool {
	return a.attachPod != nil || a.restConfig != nil
}

// watchPod waits for the launch's pod, writes the task input as the first
// line of its stdin, and serves Scry and Takeover when a hub is set. When
// the input cannot be written in full, the launch is stopped and Observe
// reports it failed with reason input_not_delivered, so the workload never
// runs on without its input.
func (a *Adapter) watchPod(parent context.Context, taskID, selector string, handle scruntime.Handle, input string) {
	if input == "" {
		a.streamPodLogsBySelector(parent, taskID, selector)
		return
	}
	if a.attachHub != nil {
		defer a.attachHub.Close(taskID)
	}
	ctx, cancel := context.WithTimeout(parent, 30*time.Minute)
	defer cancel()
	podName := a.waitForPod(ctx, selector, inputPodPolls)
	in := &inputReader{data: input}
	if podName != "" {
		var stdin io.Reader = in
		var out io.Writer
		if a.attachHub != nil {
			stdinR, stdinW := io.Pipe()
			a.attachHub.Register(taskID, stdinW)
			defer func() { _ = stdinR.Close() }()
			stdin = io.MultiReader(in, stdinR)
			out = &hubWriter{hub: a.attachHub, taskID: taskID}
		}
		_ = a.attach(ctx, podName, stdin, out)
	}
	if !in.done() {
		a.inputNotDelivered(handle)
	}
}

// inputReader yields the input once and records whether it was read in full.
type inputReader struct {
	mu   sync.Mutex
	data string
	off  int
}

func (r *inputReader) Read(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.off >= len(r.data) {
		return 0, io.EOF
	}
	n := copy(p, r.data[r.off:])
	r.off += n
	return n, nil
}

func (r *inputReader) done() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.off >= len(r.data)
}

func (a *Adapter) inputNotDelivered(handle scruntime.Handle) {
	a.mu.Lock()
	if a.undelivered == nil {
		a.undelivered = map[string]bool{}
	}
	a.undelivered[handle.ExternalID] = true
	a.mu.Unlock()
	stopCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_ = a.Stop(stopCtx, handle)
}

func (a *Adapter) inputFailed(externalID string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.undelivered[externalID]
}

func (a *Adapter) attach(ctx context.Context, podName string, stdin io.Reader, out io.Writer) error {
	if a.attachPod != nil {
		return a.attachPod(ctx, podName, stdin, out)
	}
	if a.restConfig == nil {
		return errors.New("no pod attach configured")
	}
	rc := a.client.CoreV1().RESTClient()
	if rc == nil {
		return errors.New("no REST client for pod attach")
	}
	req := rc.Post().
		Resource("pods").
		Name(podName).
		Namespace(a.namespace).
		SubResource("attach").
		VersionedParams(&corev1.PodAttachOptions{
			Container: "workload",
			Stdin:     true,
			Stdout:    out != nil,
			Stderr:    out != nil,
		}, scheme.ParameterCodec)
	exec, err := remotecommand.NewSPDYExecutor(a.restConfig, http.MethodPost, req.URL())
	if err != nil {
		return err
	}
	opts := remotecommand.StreamOptions{Stdin: stdin}
	if out != nil {
		opts.Stdout, opts.Stderr = out, out
	}
	return exec.StreamWithContext(ctx, opts)
}

func (a *Adapter) waitForPod(ctx context.Context, selector string, polls int) string {
	for range polls {
		pods, err := a.client.CoreV1().Pods(a.namespace).List(ctx, metav1.ListOptions{
			LabelSelector: selector,
		})
		if err == nil && len(pods.Items) > 0 && pods.Items[0].Status.Phase != corev1.PodPending {
			return pods.Items[0].Name
		}
		select {
		case <-ctx.Done():
			return ""
		case <-time.After(1 * time.Second):
		}
	}
	return ""
}

func (a *Adapter) streamPodLogsBySelector(parent context.Context, taskID, selector string) {
	defer a.attachHub.Close(taskID)
	ctx, cancel := context.WithTimeout(parent, 30*time.Minute)
	defer cancel()

	var podName string
	for range 60 {
		pods, err := a.client.CoreV1().Pods(a.namespace).List(ctx, metav1.ListOptions{
			LabelSelector: selector,
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

	// If RestConfig is present, attach live stdin/stdout/stderr over SPDY for full Scry + Takeover.
	if a.restConfig != nil {
		if rc := a.client.CoreV1().RESTClient(); rc != nil {
			req := rc.Post().
				Resource("pods").
				Name(podName).
				Namespace(a.namespace).
				SubResource("attach").
				VersionedParams(&corev1.PodAttachOptions{
					Container: "workload",
					Stdin:     true,
					Stdout:    true,
					Stderr:    true,
				}, scheme.ParameterCodec)
			if exec, err := remotecommand.NewSPDYExecutor(a.restConfig, http.MethodPost, req.URL()); err == nil {
				stdinR, stdinW := io.Pipe()
				a.attachHub.Register(taskID, stdinW)
				outWriter := &hubWriter{hub: a.attachHub, taskID: taskID}
				_ = exec.StreamWithContext(ctx, remotecommand.StreamOptions{
					Stdin:  stdinR,
					Stdout: outWriter,
					Stderr: outWriter,
				})
				_ = stdinR.Close()
				return
			}
		}
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
	if a.inputFailed(handle.ExternalID) {
		return scruntime.Observation{ObservedAt: time.Now().UTC(), State: scruntime.StateFailed, ExitCode: 1, Message: "input_not_delivered"}, nil
	}
	if depName, isDeploy := strings.CutPrefix(handle.ExternalID, "deploy/"); isDeploy {
		dep, err := a.client.AppsV1().Deployments(a.namespace).Get(ctx, depName, metav1.GetOptions{})
		if err != nil {
			return scruntime.Observation{}, fmt.Errorf("get kubernetes deployment %s: %w", depName, err)
		}
		obs := scruntime.Observation{ObservedAt: time.Now().UTC(), State: scruntime.StateRunning}
		if dep.Spec.Replicas != nil && *dep.Spec.Replicas == 0 {
			obs.State = scruntime.StateSuspended
		}
		return obs, nil
	}
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
	if depName, isDeploy := strings.CutPrefix(handle.ExternalID, "deploy/"); isDeploy {
		dep, err := a.client.AppsV1().Deployments(a.namespace).Get(ctx, depName, metav1.GetOptions{})
		if err != nil {
			return scruntime.CheckpointRef{}, err
		}
		zero := int32(0)
		dep.Spec.Replicas = &zero
		if _, err := a.client.AppsV1().Deployments(a.namespace).Update(ctx, dep, metav1.UpdateOptions{}); err != nil {
			return scruntime.CheckpointRef{}, err
		}
		return scruntime.CheckpointRef{ID: "k8s-cp-" + depName, CreatedAt: time.Now().UTC()}, nil
	}
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
	if depName, isDeploy := strings.CutPrefix(handle.ExternalID, "deploy/"); isDeploy {
		dep, err := a.client.AppsV1().Deployments(a.namespace).Get(ctx, depName, metav1.GetOptions{})
		if err != nil {
			return err
		}
		one := int32(1)
		dep.Spec.Replicas = &one
		_, err = a.client.AppsV1().Deployments(a.namespace).Update(ctx, dep, metav1.UpdateOptions{})
		return err
	}
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
	if depName, isDeploy := strings.CutPrefix(handle.ExternalID, "deploy/"); isDeploy {
		return a.client.AppsV1().Deployments(a.namespace).Delete(ctx, depName, metav1.DeleteOptions{
			PropagationPolicy: &policy,
		})
	}
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

type hubWriter struct {
	hub    *attach.Hub
	taskID string
}

func (w *hubWriter) Write(p []byte) (int, error) {
	w.hub.WriteOutput(w.taskID, p)
	return len(p), nil
}
