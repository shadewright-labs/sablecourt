package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/shadewright-labs/sablecourt/internal/contextpack"
	"github.com/shadewright-labs/sablecourt/internal/domain"
	"github.com/shadewright-labs/sablecourt/internal/gate/model"
	scruntime "github.com/shadewright-labs/sablecourt/internal/runtime"
	"github.com/shadewright-labs/sablecourt/internal/runtime/coding"
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
			root, err := r.StartWorkflow(ctx, "review", "")
			if err != nil {
				t.Fatal(err)
			}
			commit := strings.Repeat("a", 40)
			task, err := r.submitTask(ctx, "worker", "review", `{"workflow":"review","step_index":1,"role":"verifier","max_retries":1}`, "", &workflowStepRecord{RunID: root.ID, Workflow: "review", Index: 1, AuthorCommit: commit})
			if err != nil {
				t.Fatal(err)
			}
			evidence, _ := json.Marshal(verifierLaunch{Commit: commit, PromptDigest: promptDigest(promptEcho)})
			if err := st.UpdateTaskState(ctx, task.ID, task.State, domain.TaskStateCompleted, "verifier_checkout_pinned", string(evidence)); err != nil {
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
			if len(tasks) != 3+tc.want {
				t.Fatalf("created %d successors, want %d", len(tasks)-3, tc.want)
			}
			if tc.want == 1 {
				want := "deliver"
				if strings.HasSuffix(tc.final, "CHANGES_REQUESTED") {
					want = "author"
				}
				var successor domain.Task
				for _, candidate := range tasks {
					if candidate.ID != root.ID && candidate.ID != task.ID && candidate.IdempotencyKey != "original" {
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
			// A task persisted before workflow keys were reserved still needs a pinned checkout.
			task := domain.Task{ID: "unpinned", WorkloadName: "worker", IdempotencyKey: tc.key, InputJSON: tc.input, State: domain.TaskStateSubmitted}
			if err := st.CreateTask(ctx, task); err != nil {
				t.Fatal(err)
			}
			if err := launchForTest(ctx, r, task, wl, a); !errors.Is(err, errAuthorCheckoutUnavailable) {
				t.Errorf("launch with missing author checkout: %v", err)
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

type frozenObservation struct {
	fakeAdapter
	at time.Time
}

func (a *frozenObservation) Observe(context.Context, scruntime.Handle) (scruntime.Observation, error) {
	return scruntime.Observation{State: scruntime.StateRunning, ObservedAt: a.at}, nil
}
func TestLeaseAtMinute61(t *testing.T) {
	ctx := context.Background()
	for _, mode := range []string{"fresh", "stale", "future", "expired", "revoked"} {
		t.Run(mode, func(t *testing.T) {
			r, st, _, task := regressionFixture(t)
			a := &frozenObservation{}
			r.adapters[r.bundle.Workloads["worker"].Spec.Class] = a
			reconcile(t, r)
			hash := model.HashToken(a.lastSpec.TaskToken)
			lease, err := st.GetActiveLeaseByTokenHash(ctx, hash, r.now())
			if err != nil {
				t.Fatal(err)
			}
			for m := 1; m <= 61; m++ {
				now := lease.CreatedAt.Add(time.Duration(m) * time.Minute)
				r.now = func() time.Time { return now }
				a.at = now
				switch mode {
				case "stale":
					if m < 10 {
						continue
					}
					a.at = lease.CreatedAt.Add(5 * time.Minute)
				case "future":
					a.at = now.Add(time.Hour)
				case "expired":
					if m < 61 {
						continue
					}
				case "revoked":
					if m == 1 {
						if err := st.RevokeTaskLeases(ctx, task.ID); err != nil {
							t.Fatal(err)
						}
					}
				}
				reconcile(t, r)
			}
			_, err = st.GetActiveLeaseByTokenHash(ctx, hash, r.now())
			if (err == nil) != (mode == "fresh") {
				t.Fatalf("original token at minute 61: %v", err)
			}
		})
	}
}

type resumeFailure struct{ fakeAdapter }

func (*resumeFailure) Resume(context.Context, scruntime.Handle, scruntime.CheckpointRef) error {
	return errors.New("resume failed")
}

type recordingLeases struct {
	store.Store
	leases []domain.Lease
}

func (s *recordingLeases) CreateLease(ctx context.Context, l domain.Lease) error {
	s.leases = append(s.leases, l)
	return s.Store.CreateLease(ctx, l)
}
func TestFailedResumeRevokesLease(t *testing.T) {
	ctx := context.Background()
	r, st, _, task := regressionFixture(t)
	r.adapters[r.bundle.Workloads["worker"].Spec.Class] = &resumeFailure{}
	reconcile(t, r)
	if err := r.SuspendTask(ctx, task.ID, ""); err != nil {
		t.Fatal(err)
	}
	s := &recordingLeases{Store: st}
	r.store = s
	for range 2 {
		if err := r.ResumeTask(ctx, task.ID); err == nil {
			t.Fatal("expected failure")
		}
	}
	requireState(t, st, task.ID, domain.TaskStateSuspended)
	for _, l := range s.leases {
		if _, err := st.GetActiveLeaseByTokenHash(ctx, l.TokenSHA256, r.now()); !errors.Is(err, store.ErrNotFound) {
			t.Error("failed resume left an active token")
		}
	}
}

type blockedResume struct {
	*controlledAdapter
	entered, release chan struct{}
}

func (a *blockedResume) Resume(ctx context.Context, _ scruntime.Handle, _ scruntime.CheckpointRef) error {
	close(a.entered)
	select {
	case <-a.release:
	case <-ctx.Done():
		return ctx.Err()
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.live = true
	return nil
}

type resumeWriteFailure struct{ store.Store }

func (s resumeWriteFailure) UpdateTaskState(ctx context.Context, id string, from, to domain.TaskState, reason, facts string) error {
	if reason == "operator_resumed" {
		return errors.New("resume write failed")
	}
	return s.Store.UpdateTaskState(ctx, id, from, to, reason, facts)
}

func TestResumeCleanup(t *testing.T) {
	for _, mode := range []string{"cancel", "write_failure"} {
		t.Run(mode, func(t *testing.T) {
			ctx := t.Context()
			r, st, base, task := regressionFixture(t)
			a := &blockedResume{base, make(chan struct{}), make(chan struct{})}
			r.adapters[r.bundle.Workloads["worker"].Spec.Class] = a
			reconcile(t, r)
			if err := r.SuspendTask(ctx, task.ID, ""); err != nil {
				t.Fatal(err)
			}
			leases := &recordingLeases{Store: st}
			r.store = leases
			if mode == "write_failure" {
				r.store = resumeWriteFailure{leases}
			}
			resumed := make(chan error, 1)
			go func() { resumed <- r.ResumeTask(ctx, task.ID) }()
			select {
			case <-a.entered:
			case err := <-resumed:
				t.Fatalf("resume returned before calling the adapter: %v", err)
			}
			hash := leases.leases[0].TokenSHA256
			wantState := domain.TaskStateSuspended
			var cancelled chan error
			if mode == "cancel" {
				wantState = domain.TaskStateCancelled
				cancelled = make(chan error, 1)
				go func() { cancelled <- r.CancelTask(ctx, task.ID, "") }()
				deadline := time.Now().Add(time.Second)
				for {
					_, err := st.GetActiveLeaseByTokenHash(ctx, hash, r.now())
					if errors.Is(err, store.ErrNotFound) {
						break
					}
					if err != nil || time.Now().After(deadline) {
						t.Errorf("resume lease still active during cancellation: %v", err)
						break
					}
					time.Sleep(time.Millisecond)
				}
			}
			close(a.release)
			if err := <-resumed; err == nil {
				t.Fatal("resume unexpectedly succeeded")
			}
			if cancelled != nil {
				if err := <-cancelled; err != nil {
					t.Fatal(err)
				}
			}
			requireState(t, st, task.ID, wantState)
			if a.live {
				t.Error("runtime still live after failed resume")
			}
			if _, err := st.GetActiveLeaseByTokenHash(ctx, hash, r.now()); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("resume lease after cleanup: %v", err)
			}
		})
	}
}

type blockingClaim struct {
	store.Store
	id               string
	entered, release chan struct{}
}

func (s blockingClaim) UpdateTaskState(ctx context.Context, id string, from, to domain.TaskState, reason, facts string) error {
	if id == s.id && reason == "dispatch_claimed" {
		close(s.entered)
		<-s.release
	}
	return s.Store.UpdateTaskState(ctx, id, from, to, reason, facts)
}
func TestBlockedClaimAllowsCompletion(t *testing.T) {
	ctx := context.Background()
	r, st, a, first := regressionFixture(t)
	reconcile(t, r)
	b, p := r.bundle, r.policy
	slow, err := r.SubmitTask(ctx, "worker", "slow", "{}")
	if err != nil {
		t.Fatal(err)
	}
	s := blockingClaim{st, slow.ID, make(chan struct{}), make(chan struct{})}
	r.store = s
	done := make(chan error, 1)
	go func() { done <- r.ReconcileOnce(ctx) }()
	<-s.entered
	finished := make(chan error, 1)
	go func() {
		r.Reload(b, p)
		a.mu.Lock()
		a.state = scruntime.StateCompleted
		a.mu.Unlock()
		finished <- r.ReconcileOnce(ctx)
	}()
	defer func() { close(s.release); <-done; _ = waitForLaunches(t, r) }()
	select {
	case err := <-finished:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("blocked claim delayed reload or observation")
	}
	requireState(t, st, first.ID, domain.TaskStateCompleted)
}

type forbidHistory struct{ store.Store }

func (forbidHistory) ListTasks(context.Context) ([]domain.Task, error) {
	return nil, errors.New("loaded historical task rows")
}
func TestTickSkipsHistory(t *testing.T) {
	r, st, _, _ := regressionFixture(t)
	r.store = forbidHistory{st}
	reconcile(t, r)
}

func TestWorkflowRequiresMembership(t *testing.T) {
	ctx := context.Background()
	for _, delegated := range []bool{true, false} {
		t.Run(fmt.Sprint(delegated), func(t *testing.T) {
			r, st, _, parent := regressionFixture(t)
			r.bundle.Workflows = map[string]domain.Workflow{"flow": {Spec: domain.WorkflowSpec{Steps: []domain.WorkflowStep{{Workload: "worker"}, {Workload: "worker"}}}}}
			const input = `{"workflow":"flow","step_index":0}`
			var task domain.Task
			var err error
			if delegated {
				task, err = r.ProposeChildTask(ctx, parent.ID, "worker", "child", input, "{}")
			} else {
				task, err = r.SubmitTask(ctx, "worker", "ordinary", input)
			}
			if err != nil {
				t.Fatal(err)
			}
			if err = st.UpdateTaskState(ctx, task.ID, task.State, domain.TaskStateCompleted, "runtime_exit_observed", "{}"); err != nil {
				t.Fatal(err)
			}
			if err = r.advanceWorkflowStep(ctx, task); err != nil {
				t.Fatal(err)
			}
			tasks, err := st.ListTasks(ctx)
			if err != nil || len(tasks) != 2 {
				t.Fatalf("unregistered step dispatched a successor: %d tasks, %v", len(tasks), err)
			}
		})
	}
}

func TestWorkflowKeyReservation(t *testing.T) {
	for _, submit := range []string{"direct", "child"} {
		t.Run(submit, func(t *testing.T) {
			ctx := t.Context()
			r, st, _, parent := regressionFixture(t)
			key := "wf:flow:" + parent.ID + ":step:1"
			var err error
			if submit == "child" {
				_, err = r.ProposeChildTask(ctx, parent.ID, "worker", key, "{}", "{}")
			} else {
				_, err = r.SubmitTask(ctx, "worker", key, "{}")
			}
			if err == nil {
				t.Error("accepted reserved workflow key")
			}
			if _, err := st.GetTaskByIdempotencyKey(ctx, key); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("reserved key lookup: %v", err)
			}
		})
	}
}

type failedTaskCreate struct{ store.Store }

func (failedTaskCreate) CreateTask(context.Context, domain.Task) error {
	return errors.New("task insert failed")
}

func TestWorkflowStepCreateFailure(t *testing.T) {
	for _, mode := range []string{"conflict", "write_failure"} {
		t.Run(mode, func(t *testing.T) {
			ctx := t.Context()
			r, st, _, parent := regressionFixture(t)
			key := "wf:flow:" + parent.ID + ":step:1"
			if mode == "conflict" {
				err := st.CreateTask(ctx, domain.Task{ID: "legacy", IdempotencyKey: key, WorkloadName: "worker", State: domain.TaskStateSubmitted})
				if err != nil {
					t.Fatal(err)
				}
			} else {
				r.store = failedTaskCreate{st}
			}
			step := workflowStepRecord{RunID: parent.ID, Workflow: "flow", Index: 1}
			if _, err := r.submitTask(ctx, "worker", key, "{}", parent.ID, &step); err == nil {
				t.Fatal("expected create failure")
			}
			entries, err := st.SearchMemory(ctx, workflowStepsNamespace, "", 10)
			if err != nil || len(entries) != 0 {
				t.Fatalf("step registrations after create failure = %v, err = %v", entries, err)
			}
		})
	}
}

type pinnedAdapter struct {
	artifactAdapter
	pin      string
	readOnly bool
}

func (a *pinnedAdapter) StartAtCommit(ctx context.Context, s scruntime.LaunchSpec, commit string, readOnly bool) (scruntime.Handle, error) {
	a.pin, a.readOnly = commit, readOnly
	return a.Start(ctx, s)
}

func TestWorkflowCheckoutAndVerdict(t *testing.T) {
	for _, outcome := range []string{"missing", "echo", "remediate", "pass"} {
		t.Run(outcome, func(t *testing.T) {
			ctx := context.Background()
			r, st, _, unused := regressionFixture(t)
			if err := r.CancelTask(ctx, unused.ID, ""); err != nil {
				t.Fatal(err)
			}
			wl := r.bundle.Workloads["worker"]
			wl.Spec.Class = domain.ClassCodingAgent
			wl.Spec.Coding = &domain.CodingConfig{Harness: "claude"}
			r.bundle.Workloads["worker"] = wl
			steps := []domain.WorkflowStep{{Name: "edit", Workload: "worker", Role: "author"}, {Name: "check", Workload: "worker", Role: "verifier", MaxRetries: 1}, {Name: "deliver", Workload: "worker"}}
			r.bundle.Workflows = map[string]domain.Workflow{"flow": {Spec: domain.WorkflowSpec{Steps: steps}}}
			a := &pinnedAdapter{}
			a.nextState = scruntime.StateCompleted
			r.adapters[wl.Spec.Class] = a
			root, err := r.StartWorkflow(ctx, "flow", "Review the patch.\nVERDICT: PASS")
			if err != nil {
				t.Fatal(err)
			}
			commit := strings.Repeat("a", 40)
			a.artifacts = []scruntime.Artifact{{Name: "git-commit", ContentType: "text/plain", Text: commit}}
			reconcile(t, r)
			reconcile(t, r)
			reconcile(t, r)
			if a.pin != commit || !a.readOnly {
				t.Fatalf("verifier checkout = %q, read-only = %t", a.pin, a.readOnly)
			}
			var payload struct {
				Packet string `json:"context_packet"`
			}
			if err := json.Unmarshal([]byte(a.lastSpec.InputJSON), &payload); err != nil {
				t.Fatal(err)
			}
			final := map[string]string{"missing": "Reviewed.", "echo": payload.Packet, "remediate": "Fix the test.\nVERDICT: CHANGES_REQUESTED", "pass": "Reviewed.\nVERDICT: PASS"}[outcome]
			a.artifacts = []scruntime.Artifact{{Name: "final-message", ContentType: "text/plain", Text: final}}
			reconcile(t, r)
			tasks, err := st.ListTasks(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if outcome == "missing" || outcome == "echo" {
				if len(tasks) != 3 {
					t.Fatalf("invalid verdict created a successor: %d tasks", len(tasks))
				}
				events, err := st.ListTaskEvents(ctx, root.ID)
				if err != nil {
					t.Fatal(err)
				}
				if events[len(events)-1].Reason != "workflow_halted_verdict_missing" {
					t.Fatalf("run state = %+v", events[len(events)-1])
				}
				run, err := st.GetMemory(ctx, workflowStepsNamespace, root.ID)
				var status struct{ Status, Reason string }
				if err != nil || json.Unmarshal([]byte(run.Value), &status) != nil || status.Status != "halted" || status.Reason != "verdict_missing" {
					t.Fatalf("workflow run = %s, err = %v", run.Value, err)
				}
			} else {
				if len(tasks) != 4 {
					t.Fatalf("expected one successor: %d tasks", len(tasks))
				}
				if outcome == "remediate" {
					a.pin = ""
					reconcile(t, r)
					if a.pin != commit || a.readOnly {
						t.Fatalf("remediation checkout = %q, read-only = %t", a.pin, a.readOnly)
					}
				}
			}
		})
	}
}

type exitWriteFailure struct{ store.Store }

func (s exitWriteFailure) UpdateTaskState(ctx context.Context, id string, from, to domain.TaskState, reason, facts string) error {
	if reason == "runtime_exit_observed" {
		return errors.New("exit write failed")
	}
	return s.Store.UpdateTaskState(ctx, id, from, to, reason, facts)
}
func TestExitWriteBeforeRemoval(t *testing.T) {
	ctx := context.Background()
	r, st, a, task := regressionFixture(t)
	reconcile(t, r)
	handle := r.handles[task.ID]
	handle.AdapterName = "docker"
	r.handles[task.ID] = handle
	a.state = scruntime.StateCompleted
	r.store = exitWriteFailure{st}
	if err := r.ReconcileOnce(ctx); err == nil {
		t.Fatal("exit persistence failure was hidden")
	}
	requireState(t, st, task.ID, domain.TaskStateWorking)
	if !a.live {
		t.Fatal("removed before persisting exit")
	}
	r.store = st
	reconcile(t, r)
	requireState(t, st, task.ID, domain.TaskStateCompleted)
	if a.live || a.stops != 1 {
		t.Fatal("persisted exit was not cleaned up")
	}
}

func TestAuthorRetryUsesCommit(t *testing.T) {
	for _, tc := range []struct {
		name       string
		maxRetries int
	}{
		{"automatic", 1},
		{"without_handoff", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			r, _, _, unused := regressionFixture(t)
			if err := r.CancelTask(ctx, unused.ID, ""); err != nil {
				t.Fatal(err)
			}
			wl := r.bundle.Workloads["worker"]
			wl.Spec.Class = domain.ClassCodingAgent
			wl.Spec.Coding = &domain.CodingConfig{Harness: "claude"}
			r.bundle.Workloads["worker"] = wl
			r.bundle.Workflows = map[string]domain.Workflow{"flow": {Spec: domain.WorkflowSpec{Steps: []domain.WorkflowStep{{Name: "edit", Workload: "worker", MaxRetries: tc.maxRetries}}}}}
			commit := strings.Repeat("a", 40)
			a := &pinnedAdapter{}
			a.nextState = scruntime.StateFailed
			a.artifacts = []scruntime.Artifact{{Name: "git-commit", ContentType: "text/plain", Text: commit}}
			r.adapters[wl.Spec.Class] = a
			root, err := r.StartWorkflow(ctx, "flow", "edit")
			if err != nil {
				t.Fatal(err)
			}
			reconcile(t, r)
			reconcile(t, r)
			if tc.maxRetries == 0 {
				if _, err := r.RetryTaskWithHandoff(ctx, root.ID, false, "runtime_failed"); err != nil {
					t.Fatal(err)
				}
			}
			reconcile(t, r)
			if a.pin != commit || a.readOnly {
				t.Fatalf("retry checkout = %q, read-only = %t", a.pin, a.readOnly)
			}
		})
	}
}

func TestTickReplaysExistingRemediationWithoutHistory(t *testing.T) {
	r, st, _, unused := regressionFixture(t)
	ctx := t.Context()
	if err := r.CancelTask(ctx, unused.ID, ""); err != nil {
		t.Fatal(err)
	}
	task, err := r.SubmitTask(ctx, "worker", "original-attempt", `{"auto_remediate":true}`)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.UpdateTaskState(ctx, task.ID, task.State, domain.TaskStateFailed, "runtime_exit_observed", "{}"); err != nil {
		t.Fatal(err)
	}
	retry, err := r.RetryTaskWithHandoff(ctx, task.ID, true, "runtime_failed")
	if err != nil {
		t.Fatal(err)
	}
	r.store = forbidHistory{st}
	reconcile(t, r)
	requireState(t, st, retry.ID, domain.TaskStateWorking)
	events, err := st.ListTaskEvents(ctx, task.ID)
	if err != nil || len(events) == 0 || events[len(events)-1].Reason != "runtime_failed" {
		t.Fatalf("pending exit after replay: %v, %v", events, err)
	}
}

func TestCodingMissingBinaryRecordsLaunchFailedPath(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("coding seatbelt sandbox validation requires darwin")
	}
	gitBin, err := exec.LookPath("git")
	if err != nil {
		t.Fatalf("look up git: %v", err)
	}
	missingConfigured := filepath.Join(t.TempDir(), "missing-claude")
	missingInterp := filepath.Join(t.TempDir(), "missing-interpreter")
	emptyBinDir := t.TempDir()
	for _, tc := range []struct {
		name     string
		pathEnv  string
		harness  string
		script   string
		wantPath string
	}{
		{name: "configured path", harness: missingConfigured, wantPath: missingConfigured},
		{name: "missing from PATH", pathEnv: emptyBinDir + string(os.PathListSeparator) + filepath.Dir(gitBin), wantPath: "claude"},
		{name: "missing interpreter", script: "#!" + missingInterp + "\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			wantPath := tc.wantPath
			if tc.script != "" {
				if err := coding.CheckSandbox(ctx); err != nil {
					if strings.Contains(err.Error(), "sandbox_apply: Operation not permitted") {
						t.Skip("host sandbox denies nested Seatbelt profiles")
					}
					t.Fatalf("CheckSandbox: %v", err)
				}
				binDir := t.TempDir()
				claudePath := filepath.Join(binDir, "claude")
				if err := os.WriteFile(claudePath, []byte(tc.script), 0o700); err != nil {
					t.Fatal(err)
				}
				resolved, err := filepath.EvalSymlinks(claudePath)
				if err != nil {
					t.Fatal(err)
				}
				wantPath = resolved
				t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
			}
			if tc.pathEnv != "" {
				t.Setenv("PATH", tc.pathEnv)
				if _, err := exec.LookPath("claude"); err == nil {
					t.Skip("claude binary exists in git directory")
				}
			}
			repoDir := t.TempDir()
			for _, args := range [][]string{
				{"init", "--quiet"},
				{"config", "user.email", "sablecourt-test"},
				{"config", "user.name", "Sablecourt Test"},
			} {
				if out, err := exec.Command("git", append([]string{"-C", repoDir}, args...)...).CombinedOutput(); err != nil {
					t.Fatalf("git %v: %v (%s)", args, err, out)
				}
			}
			if err := os.WriteFile(filepath.Join(repoDir, "README.md"), []byte("# test\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			for _, args := range [][]string{
				{"add", "README.md"},
				{"commit", "-m", "init"},
			} {
				if out, err := exec.Command("git", append([]string{"-C", repoDir}, args...)...).CombinedOutput(); err != nil {
					t.Fatalf("git %v: %v (%s)", args, err, out)
				}
			}

			r, st, _, unused := regressionFixture(t)
			if err := r.CancelTask(ctx, unused.ID, ""); err != nil {
				t.Fatal(err)
			}
			wl := r.bundle.Workloads["worker"]
			wl.Spec.Class = domain.ClassCodingAgent
			wl.Spec.Coding = &domain.CodingConfig{Harness: "claude", RepoPath: repoDir, Sandbox: "seatbelt"}
			r.bundle.Workloads["worker"] = wl
			adapter := coding.NewAdapter(filepath.Join(t.TempDir(), "worktrees"))
			if tc.harness != "" {
				adapter.HarnessPaths = map[string]string{"claude": tc.harness}
			}
			r.adapters[domain.ClassCodingAgent] = adapter

			task, err := r.SubmitTask(ctx, "worker", "missing-bin", `{"prompt":"check"}`)
			if err != nil {
				t.Fatal(err)
			}
			if err := r.ReconcileOnce(ctx); err != nil {
				t.Fatal(err)
			}
			if launchErr := waitForLaunches(t, r); launchErr == nil {
				t.Fatal("expected launch failure for missing binary")
			}
			requireState(t, st, task.ID, domain.TaskStateFailed)

			events, err := st.ListTaskEvents(ctx, task.ID)
			if err != nil {
				t.Fatal(err)
			}
			var launchEvent *domain.TaskEvent
			for i := range events {
				if events[i].Reason == "launch_failed" {
					launchEvent = &events[i]
				}
			}
			if launchEvent == nil {
				t.Fatalf("missing launch_failed event: %+v", events)
			}
			var facts struct {
				Reason string `json:"reason"`
				Path   string `json:"path"`
			}
			if err := json.Unmarshal([]byte(launchEvent.FactJSON), &facts); err != nil {
				t.Fatalf("unmarshal launch_failed FactJSON %q: %v", launchEvent.FactJSON, err)
			}
			if facts.Reason != "missing_binary" || facts.Path != wantPath {
				t.Fatalf("launch_failed facts = %+v (raw %s), want reason=missing_binary path=%q", facts, launchEvent.FactJSON, wantPath)
			}
		})
	}
}

type missingBinaryStartAdapter struct {
	fakeAdapter
	partial bool
	err     error
}

func (a *missingBinaryStartAdapter) Start(ctx context.Context, spec scruntime.LaunchSpec) (scruntime.Handle, error) {
	if a.partial {
		h, _ := a.fakeAdapter.Start(ctx, spec)
		return h, a.err
	}
	return scruntime.Handle{}, a.err
}

func TestLaunchFailedFactsRedactAndPreserveMissingPath(t *testing.T) {
	for _, tc := range []struct {
		name     string
		partial  bool
		path     string
		wantPath string
	}{
		{name: "missing path", path: "/opt/bin/missing-claude", wantPath: "/opt/bin/missing-claude"},
		{name: "partial start missing path", partial: true, path: "/opt/bin/missing-codex", wantPath: "/opt/bin/missing-codex"},
		{name: "token in path is redacted", path: "/tmp/sc_task_0123456789abcdef0123456789abcdef/claude", wantPath: "/tmp/[REDACTED:sablecourt_token]/claude"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			r, st, _, task := regressionFixture(t)
			wl := r.bundle.Workloads["worker"]
			a := &missingBinaryStartAdapter{
				partial: tc.partial,
				err:     fmt.Errorf("resolve harness: %w", &scruntime.MissingBinaryError{Path: tc.path, Err: errors.New("no such file")}),
			}
			err := launchForTest(ctx, r, task, wl, a)
			if err == nil || strings.Contains(err.Error(), tc.path) {
				t.Fatalf("launchForTest error = %v", err)
			}
			requireState(t, st, task.ID, domain.TaskStateFailed)
			events, err := st.ListTaskEvents(ctx, task.ID)
			if err != nil {
				t.Fatal(err)
			}
			var launchEvent *domain.TaskEvent
			for i := range events {
				if events[i].Reason == "launch_failed" {
					launchEvent = &events[i]
				}
			}
			if launchEvent == nil {
				t.Fatalf("missing launch_failed event: %+v", events)
			}
			var facts struct {
				Reason string `json:"reason"`
				Path   string `json:"path"`
			}
			if err := json.Unmarshal([]byte(launchEvent.FactJSON), &facts); err != nil {
				t.Fatalf("unmarshal FactJSON %q: %v", launchEvent.FactJSON, err)
			}
			if facts.Reason != "missing_binary" || facts.Path != tc.wantPath {
				t.Fatalf("facts = %+v (raw %s), want reason=missing_binary path=%q", facts, launchEvent.FactJSON, tc.wantPath)
			}
		})
	}
}

func TestHostCodingTaskGetsLoopbackGate(t *testing.T) {
	const containerGate = "http://host.docker.internal:7432/gate/mcp"
	const loopbackGate = "http://127.0.0.1:7432/gate/mcp"
	for _, tc := range []struct {
		name  string
		class domain.WorkloadClass
		host  string
		want  string
	}{
		{"host coding agent uses loopback", domain.ClassCodingAgent, loopbackGate, loopbackGate},
		{"container workload keeps the container address", domain.ClassDeterministicController, loopbackGate, containerGate},
		{"unset loopback falls back", domain.ClassCodingAgent, "", containerGate},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			r, _, a, task := regressionFixture(t)
			r.dataGateURL = containerGate
			r.hostDataGateURL = tc.host
			wl := r.bundle.Workloads["worker"]
			wl.Spec.Class = tc.class
			if tc.class == domain.ClassCodingAgent {
				wl.Spec.Coding = &domain.CodingConfig{Harness: "claude"}
			}
			r.bundle.Workloads["worker"] = wl
			r.adapters[wl.Spec.Class] = a
			if err := launchForTest(ctx, r, task, wl, a); err != nil {
				t.Fatal(err)
			}
			if a.lastSpec.DataGateURL != tc.want {
				t.Fatalf("data gate URL = %q, want %q", a.lastSpec.DataGateURL, tc.want)
			}
		})
	}
}
