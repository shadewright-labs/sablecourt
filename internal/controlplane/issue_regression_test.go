package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/shadewright-labs/sablecourt/internal/contextpack"
	"github.com/shadewright-labs/sablecourt/internal/domain"
	"github.com/shadewright-labs/sablecourt/internal/gate/model"
	scruntime "github.com/shadewright-labs/sablecourt/internal/runtime"
	"github.com/shadewright-labs/sablecourt/internal/store"
)

func TestSlowLaunchDoesNotBlockCompletion(t *testing.T) {
	for _, limit := range []int{0, 2} {
		t.Run(fmt.Sprint(limit), func(t *testing.T) {
			ctx := context.Background()
			r, st, a, first := regressionFixture(t)
			r.maxConcurrent = limit
			reconcile(t, r)
			a.state = scruntime.StateCompleted
			a.started = make(chan struct{}, 4)
			a.release = make(chan struct{})
			defer func() {
				close(a.release)
				if err := waitForLaunches(t, r); err != nil {
					t.Error(err)
				}
			}()
			if _, err := r.SubmitTask(ctx, "worker", "slow", "{}"); err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() { done <- r.ReconcileOnce(ctx) }()
			select {
			case <-a.started:
			case <-time.After(3 * time.Second):
				t.Fatal("slow task did not start")
			}
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(300 * time.Millisecond):
				t.Fatal("reconcile waited for the slow launch")
			}
			requireState(t, st, first.ID, domain.TaskStateCompleted)
		})
	}
}

func TestRepositoryPrecheckUsesConfiguredPath(t *testing.T) {
	configured, submitted := t.TempDir(), t.TempDir()
	for dir, marker := range map[string]string{configured: "configured-marker", submitted: "submitted-marker"} {
		if out, err := exec.Command("git", "-C", dir, "init", "--quiet").CombinedOutput(); err != nil {
			t.Fatalf("git init: %v (%s)", err, out)
		}
		if err := os.WriteFile(filepath.Join(dir, marker), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		name, key, prompt string
		configured        bool
	}{
		{"configured", "repo_path", "check", true},
		{"case insensitive", "REPO_PATH", "check", true},
		{"no configured checkout", "repo_path", "check", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, _, _, task := regressionFixture(t)
			wl := r.bundle.Workloads["worker"]
			wl.Spec.Class = domain.ClassLongLivedAgent
			if tc.configured {
				wl.Spec.Coding = &domain.CodingConfig{RepoPath: configured}
			}
			raw, err := json.Marshal(map[string]string{tc.key: submitted, "prompt": tc.prompt})
			if err != nil {
				t.Fatal(err)
			}
			task.InputJSON = string(raw)
			a := &fakeAdapter{}
			if err := launchForTest(context.Background(), r, task, wl, a); err != nil {
				t.Fatal(err)
			}
			var input struct {
				ContextPacket string `json:"context_packet"`
			}
			if err := json.Unmarshal([]byte(a.lastSpec.InputJSON), &input); err != nil {
				t.Fatal(err)
			}
			if strings.Contains(input.ContextPacket, "submitted-marker") || strings.Contains(input.ContextPacket, "configured-marker") != tc.configured {
				t.Fatalf("precheck inspected the wrong checkout: %s", input.ContextPacket)
			}
		})
	}
}

type deadlineAdapter struct {
	fakeAdapter
	calls map[string]bool
}

func (a *deadlineAdapter) record(ctx context.Context, call string) {
	_, bounded := ctx.Deadline()
	a.calls[call] = bounded
}

func (a *deadlineAdapter) Start(ctx context.Context, spec scruntime.LaunchSpec) (scruntime.Handle, error) {
	a.record(ctx, "start")
	return a.fakeAdapter.Start(ctx, spec)
}

func (a *deadlineAdapter) Observe(ctx context.Context, handle scruntime.Handle) (scruntime.Observation, error) {
	a.record(ctx, "observe")
	return a.fakeAdapter.Observe(ctx, handle)
}

func (a *deadlineAdapter) Suspend(ctx context.Context, handle scruntime.Handle) (scruntime.CheckpointRef, error) {
	a.record(ctx, "suspend")
	return a.fakeAdapter.Suspend(ctx, handle)
}

func (a *deadlineAdapter) Resume(ctx context.Context, handle scruntime.Handle, cp scruntime.CheckpointRef) error {
	a.record(ctx, "resume")
	return a.fakeAdapter.Resume(ctx, handle, cp)
}

func (a *deadlineAdapter) Stop(ctx context.Context, handle scruntime.Handle) error {
	a.record(ctx, "stop")
	return a.fakeAdapter.Stop(ctx, handle)
}

func (a *deadlineAdapter) CollectArtifacts(ctx context.Context, handle scruntime.Handle) ([]scruntime.Artifact, error) {
	a.record(ctx, "collect")
	return a.fakeAdapter.CollectArtifacts(ctx, handle)
}

func TestAdapterCallsHaveDeadlines(t *testing.T) {
	ctx := context.Background()
	for _, call := range []string{"start", "observe", "suspend", "resume", "stop", "collect"} {
		t.Run(call, func(t *testing.T) {
			r, _, _, task := regressionFixture(t)
			wl := r.bundle.Workloads["worker"]
			a := &deadlineAdapter{calls: make(map[string]bool)}
			a.nextState = scruntime.StateCompleted
			r.adapters[wl.Spec.Class] = a
			if err := launchForTest(ctx, r, task, wl, a); err != nil {
				t.Fatal(err)
			}
			if err := r.SuspendTask(ctx, task.ID, ""); err != nil {
				t.Fatal(err)
			}
			if err := r.ResumeTask(ctx, task.ID); err != nil {
				t.Fatal(err)
			}
			reconcile(t, r)
			if !a.calls[call] {
				t.Errorf("%s received no deadline", call)
			}
		})
	}
}

func TestWorkflowRequiresFinalVerifierVerdict(t *testing.T) {
	promptEcho := contextpack.BuildVerifierPrompt(domain.Task{}, "Review")
	for _, tc := range []struct {
		name, final, log, message string
		want                      int
	}{
		{name: "missing", final: "Reviewed the change."},
		{name: "empty"},
		{name: "prompt echo", log: promptEcho},
		{name: "pass in session log", log: "VERDICT: PASS"},
		{name: "pass in intermediate message", message: "VERDICT: PASS"},
		{name: "pass followed by text", final: "VERDICT: PASS\nEnd of review."},
		{name: "inline pass", final: "I found VERDICT: PASS in the instructions."},
		{name: "pass prefix", final: "VERDICT: PASSING"},
		{name: "lowercase", final: "verdict: pass"},
		{name: "oversized", final: strings.Repeat("x", 1<<17) + "\nVERDICT: PASS"},
		{name: "changes requested", final: "Missing coverage.\nVERDICT: CHANGES_REQUESTED", want: 1},
		{name: "explicit pass with prompt echo", final: "Verified the change.\r\nVERDICT: PASS\r\n", log: promptEcho, want: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			r, st, _, _ := regressionFixture(t)
			r.bundle.Workflows = map[string]domain.Workflow{
				"review": {Spec: domain.WorkflowSpec{Steps: []domain.WorkflowStep{
					{Name: "author", Workload: "worker", Role: "author"},
					{Name: "review", Workload: "worker", Role: "verifier", MaxRetries: 1},
					{Name: "deliver", Workload: "worker"},
				}}},
			}
			task, err := r.SubmitTask(ctx, "worker", "review", `{"workflow":"review","step_index":1,"role":"verifier","max_retries":1}`)
			if err != nil {
				t.Fatal(err)
			}
			if tc.message != "" {
				if err := st.AppendTaskMessage(ctx, domain.TaskMessage{TaskID: task.ID, Role: "agent", Content: tc.message}); err != nil {
					t.Fatal(err)
				}
			}
			var artifacts []scruntime.Artifact
			if tc.final != "" {
				artifacts = append(artifacts, scruntime.Artifact{Name: "final-message", ContentType: "text/plain", Text: tc.final})
			}
			if tc.log != "" {
				artifacts = append(artifacts, scruntime.Artifact{Name: "session-log", ContentType: "text/plain", Text: tc.log})
			}
			raw, err := json.Marshal(artifacts)
			if err != nil {
				t.Fatal(err)
			}
			if err := st.UpdateTaskArtifacts(ctx, task.ID, string(raw)); err != nil {
				t.Fatal(err)
			}
			if err := r.advanceWorkflowStep(ctx, task); err != nil {
				t.Fatal(err)
			}
			tasks, err := st.ListTasks(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if len(tasks) != 2+tc.want {
				t.Fatalf("created %d successors, want %d", len(tasks)-2, tc.want)
			}
			if tc.want == 1 {
				want := "deliver"
				if strings.HasSuffix(tc.final, "CHANGES_REQUESTED") {
					want = "author"
				}
				var successor domain.Task
				for _, candidate := range tasks {
					if candidate.ID != task.ID && candidate.IdempotencyKey != "original" {
						successor = candidate
					}
				}
				var metadata struct{ Step string }
				if err := json.Unmarshal([]byte(successor.InputJSON), &metadata); err != nil || metadata.Step != want {
					t.Fatalf("successor step = %q, want %q (%v)", metadata.Step, want, err)
				}
			}
		})
	}
}

func TestWorkflowTierMatchesHarness(t *testing.T) {
	for _, tc := range []struct {
		name, provider, model string
		wantErr               bool
	}{
		{"matching", "cli:claude", "claude-sonnet-5-5", false},
		{"wrong provider", "cli:agy", "gemini-3.8-flash-high", true},
		{"wrong model", "cli:claude", "gemini-3.8-flash-high", true},
		{"missing model", "cli:claude", "", true},
		{"oversized model", "cli:claude", "claude-" + strings.Repeat("x", 256), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			r, st, _, _ := regressionFixture(t)
			wl := r.bundle.Workloads["worker"]
			wl.Spec.Class = domain.ClassCodingAgent
			wl.Spec.Coding = &domain.CodingConfig{Harness: "claude"}
			r.bundle.Workloads["worker"] = wl
			r.bundle.Workflows = map[string]domain.Workflow{
				"review": {Spec: domain.WorkflowSpec{Steps: []domain.WorkflowStep{
					{Name: "author", Workload: "worker"},
					{Name: "review", Workload: "worker", Tier: "fast"},
				}}},
			}
			if err := contextpack.SaveModelTiers(ctx, st, []contextpack.TierRoute{{Tier: "fast", Provider: tc.provider, Model: tc.model}}); err != nil {
				t.Fatal(err)
			}
			_, err := r.StartWorkflow(ctx, "review", "")
			if (err != nil) != tc.wantErr {
				t.Fatalf("StartWorkflow returned %v, want error %t", err, tc.wantErr)
			}
		})
	}
}

func TestWorkflowRefusesUnpinnedAuthorCheckout(t *testing.T) {
	for _, tc := range []struct {
		name, key, input string
	}{
		{"verifier", "verifier", `{"workflow":"review","step_index":1,"role":"verifier"}`},
		{"remediation", "wf:review:remediate:review-attempt:1", `{"workflow":"review","step_index":0,"role":"author","retry_count":1,"failure_handoff":"Fix the finding."}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			r, st, a, unused := regressionFixture(t)
			if err := r.CancelTask(ctx, unused.ID, ""); err != nil {
				t.Fatal(err)
			}
			wl := r.bundle.Workloads["worker"]
			wl.Spec.Class = domain.ClassCodingAgent
			wl.Spec.Coding = &domain.CodingConfig{Harness: "claude"}
			r.bundle.Workloads["worker"] = wl
			r.adapters[wl.Spec.Class] = a
			r.bundle.Workflows = map[string]domain.Workflow{
				"review": {Spec: domain.WorkflowSpec{Steps: []domain.WorkflowStep{
					{Name: "author", Workload: "worker", Role: "author"},
					{Name: "review", Workload: "worker", Role: "verifier"},
				}}},
			}
			task, err := r.SubmitTask(ctx, "worker", tc.key, tc.input)
			if err != nil {
				t.Fatal(err)
			}
			if err := launchForTest(ctx, r, task, wl, a); err == nil {
				t.Error("launch accepted a missing author checkout")
			}
			requireState(t, st, task.ID, domain.TaskStateFailed)
			if a.starts != 0 {
				t.Error("adapter started without the author's checkout")
			}
		})
	}
}

type artifactAdapter struct {
	fakeAdapter
	artifacts []scruntime.Artifact
}

func (a *artifactAdapter) CollectArtifacts(context.Context, scruntime.Handle) ([]scruntime.Artifact, error) {
	return a.artifacts, nil
}

type artifactWriteFailure struct{ store.Store }

func (artifactWriteFailure) UpdateTaskArtifacts(context.Context, string, string) error {
	return errors.New("artifact write failed")
}

func TestCompletionWaitsForDurableArtifacts(t *testing.T) {
	for _, state := range []scruntime.ObservedState{scruntime.StateCompleted, scruntime.StateFailed} {
		t.Run(string(state), func(t *testing.T) {
			ctx := context.Background()
			r, st, _, task := regressionFixture(t)
			wl := r.bundle.Workloads["worker"]
			a := &artifactAdapter{artifacts: []scruntime.Artifact{{Name: "output", Text: "review evidence"}}}
			a.nextState = state
			r.adapters[wl.Spec.Class] = a
			if err := launchForTest(ctx, r, task, wl, a); err != nil {
				t.Fatal(err)
			}
			r.store = artifactWriteFailure{st}
			if err := r.ReconcileOnce(ctx); err == nil {
				t.Error("artifact write failure was hidden")
			}
			requireState(t, st, task.ID, domain.TaskStateWorking)
			r.store = st
			reconcile(t, r)
			after, err := st.GetTask(ctx, task.ID)
			if err != nil || after.State != domain.TaskState(state) || !strings.Contains(after.ArtifactsJSON, "review evidence") {
				t.Fatalf("completion did not recover: %s, %s, %v", after.State, after.ArtifactsJSON, err)
			}
		})
	}
}

type incompleteStartAdapter struct {
	fakeAdapter
	st             store.Store
	duringApproval bool
	stops          int
}

func (a *incompleteStartAdapter) Start(ctx context.Context, spec scruntime.LaunchSpec) (scruntime.Handle, error) {
	handle, _ := a.fakeAdapter.Start(ctx, spec)
	if a.duringApproval {
		if err := a.st.UpdateTaskState(ctx, spec.TaskID, domain.TaskStateWorking, domain.TaskStateInputRequired, "approval_required", "{}"); err != nil {
			return handle, err
		}
	}
	return handle, errors.New(spec.TaskToken)
}

func (a *incompleteStartAdapter) Stop(context.Context, scruntime.Handle) error {
	a.stops++
	return nil
}

func TestPartialStartClosesLeaseAndRuntime(t *testing.T) {
	for _, approval := range []bool{false, true} {
		t.Run(fmt.Sprint(approval), func(t *testing.T) {
			ctx := context.Background()
			r, st, _, task := regressionFixture(t)
			a := &incompleteStartAdapter{st: st, duringApproval: approval}
			wl := r.bundle.Workloads["worker"]
			err := launchForTest(ctx, r, task, wl, a)
			if err == nil || strings.Contains(err.Error(), a.lastSpec.TaskToken) {
				t.Error("adapter error did not report failure safely")
			}
			requireState(t, st, task.ID, domain.TaskStateFailed)
			if a.stops != 1 {
				t.Fatalf("stops = %d, want 1", a.stops)
			}
			if _, err := st.GetActiveLeaseByTokenHash(ctx, model.HashToken(a.lastSpec.TaskToken), r.now()); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("lease survived failed start: %v", err)
			}
		})
	}
}

func TestSuspendRefusesInFlightLaunch(t *testing.T) {
	for _, reload := range []bool{false, true} {
		t.Run(fmt.Sprint(reload), func(t *testing.T) {
			ctx := context.Background()
			r, st, a, task := regressionFixture(t)
			a.started, a.release = make(chan struct{}, 1), make(chan struct{})
			defer func() {
				close(a.release)
				_ = waitForLaunches(t, r)
			}()
			if err := r.ReconcileOnce(ctx); err != nil {
				t.Fatal(err)
			}
			select {
			case <-a.started:
			case <-time.After(3 * time.Second):
				t.Fatal("launch did not start")
			}
			if reload {
				r.Reload(r.bundle, r.policy)
			}
			if err := r.SuspendTask(ctx, task.ID, ""); !errors.Is(err, store.ErrConflict) {
				t.Errorf("suspend during launch: %v", err)
			}
			requireState(t, st, task.ID, domain.TaskStateWorking)
		})
	}
}

func TestCapturedArtifactsRedactTaskToken(t *testing.T) {
	for _, field := range []string{"text", "uri"} {
		t.Run(field, func(t *testing.T) {
			ctx := context.Background()
			r, st, _, task := regressionFixture(t)
			wl := r.bundle.Workloads["worker"]
			a := &artifactAdapter{}
			a.nextState = scruntime.StateCompleted
			r.adapters[wl.Spec.Class] = a
			if err := launchForTest(ctx, r, task, wl, a); err != nil {
				t.Fatal(err)
			}
			a.artifacts = []scruntime.Artifact{{Name: "output"}}
			if field == "text" {
				a.artifacts[0].Text = a.lastSpec.TaskToken
			} else {
				a.artifacts[0].URI = a.lastSpec.TaskToken
			}
			reconcile(t, r)
			after, err := st.GetTask(ctx, task.ID)
			if err != nil || strings.Contains(after.ArtifactsJSON, a.lastSpec.TaskToken) || !strings.Contains(after.ArtifactsJSON, "[REDACTED:") {
				t.Fatalf("artifact token was not redacted: %v", err)
			}
		})
	}
}

type blockingLeaseStore struct {
	store.Store
	taskID  string
	entered chan struct{}
	release chan struct{}
}

func (s blockingLeaseStore) CreateLease(ctx context.Context, lease domain.Lease) error {
	if lease.TaskID == s.taskID {
		close(s.entered)
		<-s.release
	}
	return s.Store.CreateLease(ctx, lease)
}

func TestBlockedLeaseCreationDoesNotBlockObservation(t *testing.T) {
	for _, reload := range []bool{false, true} {
		t.Run(fmt.Sprint(reload), func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			r, st, a, first := regressionFixture(t)
			reconcile(t, r)
			slow, err := r.SubmitTask(ctx, "worker", "slow", "{}")
			if err != nil {
				t.Fatal(err)
			}
			blocked := blockingLeaseStore{Store: st, taskID: slow.ID, entered: make(chan struct{}), release: make(chan struct{})}
			r.store = blocked
			done := make(chan error, 1)
			finished := make(chan struct{})
			defer func() {
				cancel()
				close(blocked.release)
				<-finished
				_ = waitForLaunches(t, r)
			}()
			go func() {
				defer close(finished)
				if err := r.ReconcileOnce(ctx); err != nil {
					done <- err
					return
				}
				select {
				case <-blocked.entered:
				case <-ctx.Done():
					return
				}
				if reload {
					r.Reload(r.bundle, r.policy)
				}
				a.mu.Lock()
				a.state = scruntime.StateCompleted
				a.mu.Unlock()
				done <- r.ReconcileOnce(ctx)
			}()
			select {
			case <-blocked.entered:
			case <-time.After(3 * time.Second):
				t.Fatal("lease creation did not start")
			}
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(300 * time.Millisecond):
				t.Fatal("blocked lease creation delayed observation")
			}
			requireState(t, st, first.ID, domain.TaskStateCompleted)
		})
	}
}
