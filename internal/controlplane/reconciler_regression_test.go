package controlplane

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/shadewright-labs/sablecourt/internal/config"
	"github.com/shadewright-labs/sablecourt/internal/domain"
	"github.com/shadewright-labs/sablecourt/internal/policy"
	scruntime "github.com/shadewright-labs/sablecourt/internal/runtime"
	"github.com/shadewright-labs/sablecourt/internal/store"
	"github.com/shadewright-labs/sablecourt/internal/store/sqlite"
)

type controlledAdapter struct {
	fakeAdapter
	mu         sync.Mutex
	state      scruntime.ObservedState
	observeErr error
	stopErr    error
	started    chan struct{}
	release    chan struct{}
	live       bool
	starts     int
	stops      int
}

func (a *controlledAdapter) Start(_ context.Context, spec scruntime.LaunchSpec) (scruntime.Handle, error) {
	a.mu.Lock()
	a.starts++
	a.live = true
	a.mu.Unlock()
	if a.started != nil {
		a.started <- struct{}{}
		<-a.release
	}
	return scruntime.Handle{AdapterName: "controlled", ExternalID: spec.TaskID}, nil
}
func (a *controlledAdapter) Observe(context.Context, scruntime.Handle) (scruntime.Observation, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return scruntime.Observation{State: a.state, ObservedAt: time.Now().UTC()}, a.observeErr
}
func (a *controlledAdapter) Stop(context.Context, scruntime.Handle) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.stopErr != nil {
		return a.stopErr
	}
	a.live = false
	a.stops++
	return nil
}

func regressionFixture(t *testing.T) (*Reconciler, *sqlite.SQLiteStore, *controlledAdapter, domain.Task) {
	t.Helper()
	ctx := context.Background()
	st, err := sqlite.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	wl := domain.Workload{Metadata: domain.ObjectMeta{Name: "worker"}, Spec: domain.WorkloadSpec{Class: domain.ClassDeterministicController, Grants: domain.GrantSet{DelegateTo: []string{"worker"}}}}
	if err := st.UpsertWorkload(ctx, domain.WorkloadRecord{Name: "worker", Class: wl.Spec.Class, AgentCardJSON: "{}"}); err != nil {
		t.Fatal(err)
	}
	a := &controlledAdapter{state: scruntime.StateRunning}
	b := &config.Bundle{Workloads: map[string]domain.Workload{"worker": wl}}
	p := policy.NewEngine([]domain.Policy{{Spec: domain.PolicySpec{Rules: []domain.PolicyRule{{Workload: "worker", Capability: "delegate:worker", Effect: domain.EffectAllow}}}}}, nil)
	r := NewReconciler(Options{Store: st, Bundle: b, Policy: p, Adapters: map[domain.WorkloadClass]scruntime.Adapter{wl.Spec.Class: a}})
	task, err := r.SubmitTask(ctx, "worker", "original", "{}")
	if err != nil {
		t.Fatal(err)
	}
	return r, st, a, task
}
func requireState(t *testing.T, st store.Store, id string, want domain.TaskState) {
	t.Helper()
	got, err := st.GetTask(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != want {
		t.Fatalf("state = %s, want %s", got.State, want)
	}
}
func reconcile(t *testing.T, r *Reconciler) {
	t.Helper()
	if err := r.ReconcileOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
}
func TestApprovalTaskProgress(t *testing.T) {
	for _, tc := range []struct {
		name     string
		decision domain.ApprovalStatus
		observed scruntime.ObservedState
		want     domain.TaskState
	}{
		{"approved", domain.ApprovalApproved, scruntime.StateRunning, domain.TaskStateWorking},
		{"denied", domain.ApprovalDenied, scruntime.StateRunning, domain.TaskStateWorking},
		{"expired", domain.ApprovalExpired, scruntime.StateRunning, domain.TaskStateWorking},
		{"exited_completed", domain.ApprovalPending, scruntime.StateCompleted, domain.TaskStateCompleted},
		{"exited_failed", domain.ApprovalPending, scruntime.StateFailed, domain.TaskStateFailed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			r, st, a, task := regressionFixture(t)
			reconcile(t, r)
			if err := st.CreateApproval(ctx, domain.Approval{ID: "approval", TaskID: task.ID, WorkloadName: "worker", Capability: "action:test", PayloadCanonicalJSON: "{}", PayloadSHA256: "digest", Status: domain.ApprovalPending, ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
				t.Fatal(err)
			}
			if err := st.UpdateTaskState(ctx, task.ID, domain.TaskStateWorking, domain.TaskStateInputRequired, "approval_required", "{}"); err != nil {
				t.Fatal(err)
			}
			reconcile(t, r)
			requireState(t, st, task.ID, domain.TaskStateInputRequired)
			if tc.decision != domain.ApprovalPending {
				if err := st.DecideApproval(ctx, "approval", "digest", tc.decision, "operator", "{}"); err != nil {
					t.Fatal(err)
				}
			}
			a.state = tc.observed
			reconcile(t, r)
			requireState(t, st, task.ID, tc.want)
			if tc.want == domain.TaskStateWorking {
				a.state = scruntime.StateCompleted
				reconcile(t, r)
				requireState(t, st, task.ID, domain.TaskStateCompleted)
			}
		})
	}
}
func TestMissingRuntimeFails(t *testing.T) {
	for _, tc := range []struct {
		name     string
		launched bool
		adapter  bool
	}{
		{"removed_submitted", false, false}, {"removed_working", true, false}, {"missing_adapter", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, st, a, task := regressionFixture(t)
			if tc.launched {
				reconcile(t, r)
			}
			reason := "workload_missing"
			if tc.adapter {
				r.adapters = map[domain.WorkloadClass]scruntime.Adapter{}
				reason = "adapter_missing"
			} else {
				r.Reload(&config.Bundle{}, r.policy)
			}
			reconcile(t, r)
			requireState(t, st, task.ID, domain.TaskStateFailed)
			events, err := st.ListTaskEvents(context.Background(), task.ID)
			if err != nil {
				t.Fatal(err)
			}
			if len(events) == 0 || events[len(events)-1].Reason != reason {
				t.Fatalf("missing reason %s: %+v", reason, events)
			}
			if a.live {
				t.Fatal("removed workload is still live")
			}
		})
	}
}
func TestUnobservableTaskFails(t *testing.T) {
	for _, tc := range []struct {
		name  string
		state scruntime.ObservedState
		err   error
	}{
		{"observe_error", scruntime.StateRunning, errors.New("observation unavailable")},
		{"unknown", scruntime.StateUnknown, nil},
		{"unrecognized", scruntime.ObservedState("invalid"), nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, st, a, task := regressionFixture(t)
			reconcile(t, r)
			before, err := st.GetTask(context.Background(), task.ID)
			if err != nil {
				t.Fatal(err)
			}
			a.state = tc.state
			a.observeErr = tc.err
			reconcile(t, r)
			requireState(t, st, task.ID, domain.TaskStateWorking)
			after, err := st.GetTask(context.Background(), task.ID)
			if err != nil {
				t.Fatal(err)
			}
			if before.LastHeartbeatAt == nil || after.LastHeartbeatAt == nil || !before.LastHeartbeatAt.Equal(*after.LastHeartbeatAt) {
				t.Error("untrusted observation refreshed heartbeat")
			}
			reconcile(t, r)
			reconcile(t, r)
			requireState(t, st, task.ID, domain.TaskStateFailed)
			if a.live {
				t.Fatal("failed observation left process live")
			}
			events, err := st.ListTaskEvents(context.Background(), task.ID)
			if err != nil {
				t.Fatal(err)
			}
			if len(events) < 4 {
				t.Fatalf("observation failures lack events: %+v", events)
			}
		})
	}
}
func TestRetryEndStatesAndIdempotency(t *testing.T) {
	for _, state := range []domain.TaskState{domain.TaskStateSubmitted, domain.TaskStateWorking, domain.TaskStateInputRequired, domain.TaskStateSuspended, domain.TaskStateFailed, domain.TaskStateCompleted, domain.TaskStateCancelled} {
		for _, handoff := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/handoff_%t", state, handoff), func(t *testing.T) {
				ctx := context.Background()
				r, st, _, task := regressionFixture(t)
				if state != domain.TaskStateSubmitted {
					if err := st.UpdateTaskState(ctx, task.ID, domain.TaskStateSubmitted, state, "fixture", "{}"); err != nil {
						t.Fatal(err)
					}
				}
				terminal := state == domain.TaskStateFailed || state == domain.TaskStateCompleted || state == domain.TaskStateCancelled
				first, err := r.RetryTaskWithHandoff(ctx, task.ID, handoff, "retry")
				if !terminal {
					if !errors.Is(err, store.ErrConflict) {
						t.Fatalf("retry accepted active state: %v", err)
					}
					return
				}
				if err != nil {
					t.Fatal(err)
				}
				second, err := r.RetryTaskWithHandoff(ctx, task.ID, handoff, "retry")
				if err != nil {
					t.Fatal(err)
				}
				if first.ID != second.ID {
					t.Fatalf("duplicate retries: %s, %s", first.ID, second.ID)
				}
				tasks, err := st.ListTasks(ctx)
				if err != nil {
					t.Fatal(err)
				}
				if len(tasks) != 2 {
					t.Fatalf("tasks = %d", len(tasks))
				}
			})
		}
	}
}
func TestDuplicateSubmission(t *testing.T) {
	for _, parallel := range []bool{false, true} {
		t.Run(fmt.Sprint(parallel), func(t *testing.T) {
			r, st, _, original := regressionFixture(t)
			call := func() error {
				got, err := r.SubmitTask(context.Background(), "worker", "original", "{}")
				if err != nil {
					return err
				}
				if got.ID != original.ID {
					return errors.New("different task returned")
				}
				return nil
			}
			if parallel {
				results := make(chan error, 8)
				for i := 0; i < 8; i++ {
					go func() { results <- call() }()
				}
				for i := 0; i < 8; i++ {
					if err := <-results; err != nil {
						t.Error(err)
					}
				}
			} else {
				if err := call(); err != nil {
					t.Fatal(err)
				}
			}
			tasks, err := st.ListTasks(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if len(tasks) != 1 {
				t.Fatalf("tasks = %d", len(tasks))
			}
		})
	}
}
func TestCancelDuringLaunch(t *testing.T) {
	for _, reload := range []bool{false, true} {
		t.Run(fmt.Sprint(reload), func(t *testing.T) {
			ctx := context.Background()
			r, st, a, task := regressionFixture(t)
			a.started = make(chan struct{}, 2)
			a.release = make(chan struct{})
			done := make(chan error, 1)
			go func() { done <- r.ReconcileOnce(ctx) }()
			select {
			case <-a.started:
			case <-time.After(5 * time.Second):
				t.Fatal("start did not begin")
			}
			if reload {
				r.Reload(&config.Bundle{}, r.policy)
			}
			cancelErr := r.CancelTask(ctx, task.ID, "cancel during launch")
			close(a.release)
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			if cancelErr != nil {
				t.Fatal(cancelErr)
			}
			requireState(t, st, task.ID, domain.TaskStateCancelled)
			if a.live {
				t.Fatal("cancel left a live process")
			}
			if a.starts != 1 || a.stops != 1 {
				t.Fatalf("starts=%d stops=%d", a.starts, a.stops)
			}
		})
	}
}
func TestLostLaunchClaim(t *testing.T) {
	for _, state := range []domain.TaskState{domain.TaskStateCancelled, domain.TaskStateWorking} {
		t.Run(string(state), func(t *testing.T) {
			ctx := context.Background()
			r, st, a, task := regressionFixture(t)
			if err := st.UpdateTaskState(ctx, task.ID, domain.TaskStateSubmitted, state, "competing_claim", "{}"); err != nil {
				t.Fatal(err)
			}
			err := r.launchTask(ctx, task, r.bundle.Workloads["worker"], a)
			if !errors.Is(err, store.ErrConflict) {
				t.Fatalf("claim error = %v", err)
			}
			if a.starts != 0 {
				t.Fatal("started adapter without claiming task")
			}
		})
	}
}
func TestDelegationMissingConfiguration(t *testing.T) {
	for _, missing := range []string{"bundle", "policy"} {
		t.Run(missing, func(t *testing.T) {
			r, st, _, task := regressionFixture(t)
			if missing == "bundle" {
				r.Reload(nil, r.policy)
			} else {
				r.Reload(r.bundle, nil)
			}
			defer func() {
				if v := recover(); v != nil {
					t.Errorf("delegation panicked: %v", v)
				}
			}()
			_, err := r.ProposeChildTask(context.Background(), task.ID, "worker", "child", "{}", "{}")
			if err == nil {
				t.Fatal("delegation accepted missing configuration")
			}
			tasks, listErr := st.ListTasks(context.Background())
			if listErr != nil {
				t.Fatal(listErr)
			}
			if len(tasks) != 1 {
				t.Fatal("child created without configuration")
			}
		})
	}
}
func TestReloadConcurrentReaders(t *testing.T) {
	for _, reader := range []string{"reconcile", "submit", "delegate"} {
		t.Run(reader, func(t *testing.T) {
			ctx := context.Background()
			r, _, _, task := regressionFixture(t)
			b, p := r.bundle, r.policy
			start := make(chan struct{})
			done := make(chan struct{})
			go func() {
				defer close(done)
				<-start
				for i := 0; i < 1000; i++ {
					r.Reload(b, p)
				}
			}()
			close(start)
			for i := 0; i < 20; i++ {
				var err error
				switch reader {
				case "reconcile":
					err = r.ReconcileOnce(ctx)
				case "submit":
					_, err = r.SubmitTask(ctx, "worker", fmt.Sprintf("submission-%d", i), "{}")
				case "delegate":
					_, err = r.ProposeChildTask(ctx, task.ID, "worker", fmt.Sprintf("child-%d", i), "{}", "{}")
				}
				if err != nil && !strings.Contains(err.Error(), "conflict") {
					t.Error(err)
				}
			}
			<-done
		})
	}
}

func TestCancelAfterReload(t *testing.T) {
	for _, bundle := range []*config.Bundle{nil, {}} {
		t.Run(fmt.Sprintf("nil_%t", bundle == nil), func(t *testing.T) {
			r, st, a, task := regressionFixture(t)
			reconcile(t, r)
			r.Reload(bundle, nil)
			if err := r.CancelTask(context.Background(), task.ID, ""); err != nil {
				t.Fatal(err)
			}
			requireState(t, st, task.ID, domain.TaskStateCancelled)
			if a.live {
				t.Fatal("cancel lost the original adapter after reload")
			}
		})
	}
}
func TestConcurrentRetries(t *testing.T) {
	for _, handoff := range []bool{false, true} {
		t.Run(fmt.Sprint(handoff), func(t *testing.T) {
			ctx := context.Background()
			r, st, _, task := regressionFixture(t)
			if err := st.UpdateTaskState(ctx, task.ID, domain.TaskStateSubmitted, domain.TaskStateFailed, "fixture", "{}"); err != nil {
				t.Fatal(err)
			}
			type result struct {
				task domain.Task
				err  error
			}
			results := make(chan result, 8)
			for i := 0; i < 8; i++ {
				go func() {
					task, err := r.RetryTaskWithHandoff(ctx, task.ID, handoff, "retry")
					results <- result{task, err}
				}()
			}
			id := ""
			for i := 0; i < 8; i++ {
				got := <-results
				if got.err != nil {
					t.Error(got.err)
					continue
				}
				if id != "" && got.task.ID != id {
					t.Error("concurrent retries created different tasks")
				}
				id = got.task.ID
			}
		})
	}
}

func TestRetryWaitsForCleanup(t *testing.T) {
	for _, stage := range []string{"launching", "stop_failed"} {
		t.Run(stage, func(t *testing.T) {
			ctx := context.Background()
			r, st, a, task := regressionFixture(t)
			var done chan error
			if stage == "launching" {
				a.started = make(chan struct{}, 1)
				a.release = make(chan struct{})
				done = make(chan error, 1)
				go func() { done <- r.ReconcileOnce(ctx) }()
				select {
				case <-a.started:
				case <-time.After(5 * time.Second):
					t.Fatal("start did not begin")
				}
			} else {
				reconcile(t, r)
				a.stopErr = errors.New("stop unavailable")
			}
			cancelErr := r.CancelTask(ctx, task.ID, "")
			if stage == "stop_failed" && cancelErr == nil {
				t.Error("stop failure was hidden")
			}
			_, retryErr := r.RetryTaskWithHandoff(ctx, task.ID, false, "")
			if done != nil {
				close(a.release)
				if err := <-done; err != nil {
					t.Fatal(err)
				}
			} else {
				a.stopErr = nil
				reconcile(t, r)
			}
			if !errors.Is(retryErr, store.ErrConflict) {
				t.Fatalf("retry before cleanup = %v", retryErr)
			}
			requireState(t, st, task.ID, domain.TaskStateCancelled)
			if a.live {
				t.Fatal("cleanup did not stop the process")
			}
			if _, err := r.RetryTaskWithHandoff(ctx, task.ID, false, ""); err != nil {
				t.Fatalf("retry after cleanup: %v", err)
			}
		})
	}
}
