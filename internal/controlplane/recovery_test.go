package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/shadewright-labs/sablecourt/internal/config"
	"github.com/shadewright-labs/sablecourt/internal/domain"
	"github.com/shadewright-labs/sablecourt/internal/gate/model"
	"github.com/shadewright-labs/sablecourt/internal/policy"
	scruntime "github.com/shadewright-labs/sablecourt/internal/runtime"
	"github.com/shadewright-labs/sablecourt/internal/store"
	"github.com/shadewright-labs/sablecourt/internal/store/sqlite"
)

type stubProcess struct {
	cmd      *exec.Cmd
	start    time.Time
	pid      int
	stopFile string
	done     chan struct{}
}

func startStubProcess(t *testing.T, dir string) *stubProcess {
	t.Helper()
	stopFile := filepath.Join(dir, "done")
	cmd := exec.Command("/bin/sh", "-c", `while [ ! -f "$1" ]; do sleep 0.02; done`, "stub", stopFile)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start stub process: %v", err)
	}
	start, err := scruntime.ProcessStartTime(cmd.Process.Pid)
	if err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		t.Fatal(err)
	}
	p := &stubProcess{start: start,
		cmd:      cmd,
		pid:      cmd.Process.Pid,
		stopFile: stopFile,
		done:     make(chan struct{}),
	}
	go func() {
		_ = cmd.Wait()
		close(p.done)
	}()
	t.Cleanup(func() {
		_ = os.WriteFile(stopFile, []byte("stop"), 0o600)
		select {
		case <-p.done:
		case <-time.After(2 * time.Second):
			_ = cmd.Process.Kill()
			<-p.done
		}
	})
	return p
}

func (p *stubProcess) finish(t *testing.T) {
	t.Helper()
	if err := os.WriteFile(p.stopFile, []byte("done"), 0o600); err != nil {
		t.Fatalf("signal stub process exit: %v", err)
	}
	select {
	case <-p.done:
	case <-time.After(3 * time.Second):
		t.Fatal("stub process did not exit")
	}
}

func (p *stubProcess) alive() bool {
	select {
	case <-p.done:
		return false
	default:
		return true
	}
}

type restartStubAdapter struct {
	t         *testing.T
	rootDir   string
	mu        sync.Mutex
	procs     map[string]*stubProcess
	lastSpec  scruntime.LaunchSpec
	stopCalls int
	now       func() time.Time
}

func newRestartStubAdapter(t *testing.T, now func() time.Time) *restartStubAdapter {
	t.Helper()
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	return &restartStubAdapter{
		t:       t,
		rootDir: t.TempDir(),
		procs:   make(map[string]*stubProcess),
		now:     now,
	}
}

func (a *restartStubAdapter) Start(_ context.Context, spec scruntime.LaunchSpec) (scruntime.Handle, error) {
	dir := filepath.Join(a.rootDir, spec.TaskID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return scruntime.Handle{}, err
	}
	proc := startStubProcess(a.t, dir)
	a.mu.Lock()
	a.lastSpec = spec
	a.procs[spec.TaskID] = proc
	a.mu.Unlock()
	return scruntime.Handle{
		AdapterName:      "coding:stub",
		ExternalID:       spec.TaskID,
		WorkDir:          dir,
		Branch:           "sablecourt/" + spec.TaskID,
		PID:              proc.pid,
		ProcessStartTime: proc.start,
	}, nil
}

func (a *restartStubAdapter) Observe(_ context.Context, handle scruntime.Handle) (scruntime.Observation, error) {
	a.mu.Lock()
	proc := a.procs[handle.ExternalID]
	now := a.now()
	a.mu.Unlock()
	if proc == nil {
		return scruntime.Observation{State: scruntime.StateUnknown, ObservedAt: now}, errors.New("unknown stub process")
	}
	if proc.alive() {
		return scruntime.Observation{State: scruntime.StateRunning, ObservedAt: now}, nil
	}
	return scruntime.Observation{State: scruntime.StateCompleted, ObservedAt: now}, nil
}

func (a *restartStubAdapter) Suspend(_ context.Context, _ scruntime.Handle) (scruntime.CheckpointRef, error) {
	return scruntime.CheckpointRef{}, errors.New("unsupported")
}

func (a *restartStubAdapter) Resume(_ context.Context, _ scruntime.Handle, _ scruntime.CheckpointRef) error {
	return errors.New("unsupported")
}

func (a *restartStubAdapter) Stop(_ context.Context, handle scruntime.Handle) error {
	a.mu.Lock()
	a.stopCalls++
	proc := a.procs[handle.ExternalID]
	a.mu.Unlock()
	if proc != nil && proc.alive() {
		_ = os.WriteFile(proc.stopFile, []byte("stop"), 0o600)
		select {
		case <-proc.done:
		case <-time.After(2 * time.Second):
			_ = proc.cmd.Process.Kill()
			<-proc.done
		}
	}
	return nil
}

func (a *restartStubAdapter) CollectArtifacts(_ context.Context, _ scruntime.Handle) ([]scruntime.Artifact, error) {
	return []scruntime.Artifact{{Name: "output", ContentType: "text/plain", Text: "completed after restart"}}, nil
}

func (a *restartStubAdapter) HandleAlive(ctx context.Context, handle scruntime.Handle) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if handle.PID <= 0 || handle.ProcessStartTime.IsZero() || handle.WorkDir == "" {
		return false, nil
	}
	return scruntime.ProcessAliveWithStartTime(handle.PID, handle.ProcessStartTime)
}

func recoveryFixture(t *testing.T, adapter scruntime.Adapter, optsMut func(*Options)) (*Reconciler, *sqlite.SQLiteStore, *config.Bundle, *policy.Engine) {
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
	wl := domain.Workload{
		Metadata: domain.ObjectMeta{Name: "coder", Zone: domain.ZoneRuntime},
		Spec: domain.WorkloadSpec{
			Class: domain.ClassCodingAgent,
			Coding: &domain.CodingConfig{
				Harness: "claude",
				Sandbox: "seatbelt",
			},
			Grants: domain.GrantSet{
				ModelGate: domain.ModelGateGrant{Allowed: true},
			},
		},
	}
	if err := st.UpsertWorkload(ctx, domain.WorkloadRecord{
		Name:          wl.Metadata.Name,
		Zone:          wl.Metadata.Zone,
		Class:         wl.Spec.Class,
		AgentCardJSON: "{}",
	}); err != nil {
		t.Fatal(err)
	}
	bundle := &config.Bundle{Workloads: map[string]domain.Workload{wl.Metadata.Name: wl}}
	pol := policy.NewEngine(nil, nil)
	opts := Options{
		Store:    st,
		Bundle:   bundle,
		Policy:   pol,
		Adapters: map[domain.WorkloadClass]scruntime.Adapter{wl.Spec.Class: adapter},
	}
	if optsMut != nil {
		optsMut(&opts)
	}
	return NewReconciler(opts), st, bundle, pol
}

func lastEventReason(t *testing.T, st store.Store, taskID string) string {
	t.Helper()
	events, err := st.ListTaskEvents(context.Background(), taskID)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) == 0 {
		return ""
	}
	return events[len(events)-1].Reason
}

func TestRestartRecoveryLiveProcess(t *testing.T) {
	ctx := context.Background()
	adapter := newRestartStubAdapter(t, nil)
	r1, st, bundle, pol := recoveryFixture(t, adapter, nil)

	task, err := r1.SubmitTask(ctx, "coder", "live-restart", `{}`)
	if err != nil {
		t.Fatal(err)
	}
	reconcile(t, r1)
	requireState(t, st, task.ID, domain.TaskStateWorking)

	// Simulate daemon restart: new reconciler instance with empty in-memory handle map.
	r2 := NewReconciler(Options{
		Store:    st,
		Bundle:   bundle,
		Policy:   pol,
		Adapters: map[domain.WorkloadClass]scruntime.Adapter{domain.ClassCodingAgent: adapter},
	})
	reconcile(t, r2)
	requireState(t, st, task.ID, domain.TaskStateWorking)

	// Finish the live stub process and verify the restarted reconciler completes the task.
	adapter.procs[task.ID].finish(t)
	reconcile(t, r2)
	requireState(t, st, task.ID, domain.TaskStateCompleted)
}

func TestRestartRecoveryProcessGoneFailsLostOnRestart(t *testing.T) {
	for _, tc := range []struct {
		name      string
		killFirst bool
	}{
		{name: "process_exited_before_restart", killFirst: true},
		{name: "no_handle_recorded", killFirst: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			adapter := newRestartStubAdapter(t, nil)
			r1, st, bundle, pol := recoveryFixture(t, adapter, nil)

			var task domain.Task
			var tokenHash string
			var err error
			if tc.killFirst {
				task, err = r1.SubmitTask(ctx, "coder", "gone-"+tc.name, `{}`)
				if err != nil {
					t.Fatal(err)
				}
				reconcile(t, r1)
				tokenHash = model.HashToken(adapter.lastSpec.TaskToken)
				adapter.procs[task.ID].finish(t)
			} else {
				task, err = r1.SubmitTask(ctx, "coder", "ghost-"+tc.name, `{}`)
				if err != nil {
					t.Fatal(err)
				}
				if err := st.UpdateTaskState(ctx, task.ID, domain.TaskStateSubmitted, domain.TaskStateWorking, "dispatch_claimed", `{}`); err != nil {
					t.Fatal(err)
				}
				tok, err := model.MintTaskToken(ctx, st, task.ID, task.WorkloadName, time.Hour)
				if err != nil {
					t.Fatal(err)
				}
				tokenHash = model.HashToken(tok)
			}

			r2 := NewReconciler(Options{
				Store:    st,
				Bundle:   bundle,
				Policy:   pol,
				Adapters: map[domain.WorkloadClass]scruntime.Adapter{domain.ClassCodingAgent: adapter},
			})
			reconcile(t, r2)

			requireState(t, st, task.ID, domain.TaskStateFailed)
			if got := lastEventReason(t, st, task.ID); got != "lost_on_restart" {
				t.Fatalf("event reason = %q, want lost_on_restart", got)
			}
			if _, err := st.GetActiveLeaseByTokenHash(ctx, tokenHash, time.Now().UTC()); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("lease survived lost_on_restart: %v", err)
			}
		})
	}
}

func TestStaleWorkingTaskFailsLostOnRestart(t *testing.T) {
	for _, tc := range []struct {
		name             string
		advance          time.Duration
		clearHeartbeat   bool
		wantState        domain.TaskState
		wantReason       string
		wantLeaseRevoked bool
	}{
		{
			name:             "fresh_within_window",
			advance:          30 * time.Second,
			wantState:        domain.TaskStateWorking,
			wantLeaseRevoked: false,
		},
		{
			name:             "heartbeat_past_staleness_window",
			advance:          3 * time.Minute,
			wantState:        domain.TaskStateFailed,
			wantReason:       "lost_on_restart",
			wantLeaseRevoked: true,
		},
		{
			name:             "no_heartbeat_past_staleness_window",
			advance:          3 * time.Minute,
			clearHeartbeat:   true,
			wantState:        domain.TaskStateFailed,
			wantReason:       "lost_on_restart",
			wantLeaseRevoked: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			clock := time.Now().UTC().Truncate(time.Second)
			nowFn := func() time.Time { return clock }
			adapter := newRestartStubAdapter(t, nowFn)
			r, st, _, _ := recoveryFixture(t, adapter, func(o *Options) {
				o.StalenessWindow = 2 * time.Minute
				o.Now = nowFn
			})

			task, err := r.SubmitTask(ctx, "coder", "stale-"+tc.name, `{}`)
			if err != nil {
				t.Fatal(err)
			}
			var tokenHash string
			if tc.clearHeartbeat {
				if err := st.UpdateTaskState(ctx, task.ID, domain.TaskStateSubmitted, domain.TaskStateWorking, "dispatch_claimed", `{}`); err != nil {
					t.Fatal(err)
				}
				tok, err := model.MintTaskToken(ctx, st, task.ID, task.WorkloadName, time.Hour)
				if err != nil {
					t.Fatal(err)
				}
				tokenHash = model.HashToken(tok)
			} else {
				reconcile(t, r)
				tokenHash = model.HashToken(adapter.lastSpec.TaskToken)
			}

			clock = clock.Add(tc.advance)
			reconcile(t, r)

			requireState(t, st, task.ID, tc.wantState)
			if tc.wantReason != "" {
				if got := lastEventReason(t, st, task.ID); got != tc.wantReason {
					t.Fatalf("event reason = %q, want %q", got, tc.wantReason)
				}
			}
			_, leaseErr := st.GetActiveLeaseByTokenHash(ctx, tokenHash, clock)
			if tc.wantLeaseRevoked && !errors.Is(leaseErr, store.ErrNotFound) {
				t.Fatalf("expected lease revoked, got %v", leaseErr)
			}
			if !tc.wantLeaseRevoked && leaseErr != nil {
				t.Fatalf("expected active lease, got %v", leaseErr)
			}
		})
	}
}

func TestGhostsDoNotCountAgainstMaxConcurrent(t *testing.T) {
	for _, tc := range []struct {
		name      string
		staleOnly bool
	}{
		{name: "lost_on_restart_ghosts", staleOnly: false},
		{name: "stale_working_ghosts", staleOnly: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			clock := time.Now().UTC().Truncate(time.Second)
			nowFn := func() time.Time { return clock }
			adapter := newRestartStubAdapter(t, nowFn)
			r1, st, bundle, pol := recoveryFixture(t, adapter, func(o *Options) {
				o.MaxConcurrent = 2
				o.StalenessWindow = 2 * time.Minute
				o.Now = nowFn
			})

			// Launch 2 tasks while both remain alive so both stay working and fill maxConcurrent.
			ghostIDs := make([]string, 0, 2)
			for i := range 2 {
				clock = clock.Add(time.Second)
				ghost, err := r1.SubmitTask(ctx, "coder", "ghost-"+tc.name+"-"+string(rune('a'+i)), `{}`)
				if err != nil {
					t.Fatal(err)
				}
				ghostIDs = append(ghostIDs, ghost.ID)
			}
			reconcile(t, r1)
			for _, id := range ghostIDs {
				requireState(t, st, id, domain.TaskStateWorking)
			}

			// Submit a newer task after the ghosts. ListReconcileTasks orders by created_at DESC,
			// so this submitted task is visited before older ghosts unless ghosts are cleared first.
			clock = clock.Add(time.Second)
			fresh, err := r1.SubmitTask(ctx, "coder", "fresh-"+tc.name, `{}`)
			if err != nil {
				t.Fatal(err)
			}

			target := r1
			if tc.staleOnly {
				clock = clock.Add(3 * time.Minute)
			} else {
				for _, id := range ghostIDs {
					adapter.procs[id].finish(t)
				}
				target = NewReconciler(Options{
					Store:           st,
					Bundle:          bundle,
					Policy:          pol,
					Adapters:        map[domain.WorkloadClass]scruntime.Adapter{domain.ClassCodingAgent: adapter},
					MaxConcurrent:   2,
					StalenessWindow: 2 * time.Minute,
					Now:             nowFn,
				})
			}

			reconcile(t, target)

			for _, id := range ghostIDs {
				requireState(t, st, id, domain.TaskStateFailed)
				if got := lastEventReason(t, st, id); got != "lost_on_restart" {
					t.Fatalf("ghost %s reason = %q, want lost_on_restart", id, got)
				}
			}
			requireState(t, st, fresh.ID, domain.TaskStateWorking)
		})
	}
}

func TestCancelTaskAfterRestartStopsLiveProcess(t *testing.T) {
	ctx := context.Background()
	adapter := newRestartStubAdapter(t, nil)
	r1, st, bundle, pol := recoveryFixture(t, adapter, nil)

	task, err := r1.SubmitTask(ctx, "coder", "cancel-restart", `{}`)
	if err != nil {
		t.Fatal(err)
	}
	reconcile(t, r1)
	proc := adapter.procs[task.ID]

	r2 := NewReconciler(Options{
		Store:    st,
		Bundle:   bundle,
		Policy:   pol,
		Adapters: map[domain.WorkloadClass]scruntime.Adapter{domain.ClassCodingAgent: adapter},
	})
	if err := r2.CancelTask(ctx, task.ID, "operator_cancelled"); err != nil {
		t.Fatalf("CancelTask after restart: %v", err)
	}
	requireState(t, st, task.ID, domain.TaskStateCancelled)
	if adapter.stopCalls == 0 || proc.alive() {
		t.Fatalf("CancelTask after restart did not stop live process (stopCalls=%d alive=%t)", adapter.stopCalls, proc.alive())
	}
}

func TestRestartRecoveryReusedPIDNotAdopted(t *testing.T) {
	ctx := context.Background()
	adapter := newRestartStubAdapter(t, nil)
	r1, st, bundle, pol := recoveryFixture(t, adapter, nil)

	task, err := r1.SubmitTask(ctx, "coder", "reused-pid", `{}`)
	if err != nil {
		t.Fatal(err)
	}
	reconcile(t, r1)
	tokenHash := model.HashToken(adapter.lastSpec.TaskToken)

	// End the original task process and start an unrelated live process.
	adapter.procs[task.ID].finish(t)
	unrelated := startStubProcess(t, t.TempDir())
	unrelatedStart, err := scruntime.ProcessStartTime(unrelated.pid)
	if err != nil {
		t.Fatalf("ProcessStartTime(unrelated): %v", err)
	}

	// Point the persisted handle at the unrelated live PID with a different start time,
	// simulating OS PID reuse after the original worker exited.
	reusedRecord, err := json.Marshal(map[string]any{
		"task_id":            task.ID,
		"workload_class":     string(domain.ClassCodingAgent),
		"adapter_name":       "coding:stub",
		"external_id":        task.ID,
		"work_dir":           filepath.Join(adapter.rootDir, task.ID),
		"branch":             "sablecourt/" + task.ID,
		"pid":                unrelated.pid,
		"process_start_time": unrelatedStart.Add(-5 * time.Second).UTC().Format(time.RFC3339Nano),
		"log_path":           filepath.Join(adapter.rootDir, task.ID) + ".log",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.PutMemory(ctx, domain.MemoryEntry{
		Namespace: "system:runtime-handles",
		Key:       task.ID,
		Value:     string(reusedRecord),
		Author:    "controlplane",
		UpdatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}

	r2 := NewReconciler(Options{
		Store:    st,
		Bundle:   bundle,
		Policy:   pol,
		Adapters: map[domain.WorkloadClass]scruntime.Adapter{domain.ClassCodingAgent: adapter},
	})
	reconcile(t, r2)

	requireState(t, st, task.ID, domain.TaskStateFailed)
	if got := lastEventReason(t, st, task.ID); got != "lost_on_restart" {
		t.Fatalf("event reason = %q, want lost_on_restart", got)
	}
	if _, err := st.GetActiveLeaseByTokenHash(ctx, tokenHash, time.Now().UTC()); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("lease survived reused PID rejection: %v", err)
	}
	if adapter.stopCalls != 0 {
		t.Fatalf("Stop was called %d times on a reused PID", adapter.stopCalls)
	}
	if !unrelated.alive() {
		t.Fatal("unrelated process sharing the reused PID was killed")
	}
}

func TestRestartRecoveryMalformedHandlesFailClosed(t *testing.T) {
	live := startStubProcess(t, t.TempDir())
	liveStart, err := scruntime.ProcessStartTime(live.pid)
	if err != nil {
		t.Fatal(err)
	}
	validFields := func(taskID string, mutate func(map[string]any)) string {
		m := map[string]any{
			"task_id":            taskID,
			"workload_class":     string(domain.ClassCodingAgent),
			"adapter_name":       "coding:stub",
			"external_id":        taskID,
			"work_dir":           "/tmp/wt",
			"pid":                live.pid,
			"process_start_time": liveStart.UTC().Format(time.RFC3339Nano),
		}
		if mutate != nil {
			mutate(m)
		}
		b, _ := json.Marshal(m)
		return string(b)
	}

	for _, tc := range []struct {
		name   string
		author string
		value  func(taskID string) string
	}{
		{name: "empty", author: "controlplane", value: func(string) string { return "" }},
		{name: "null", author: "controlplane", value: func(string) string { return "null" }},
		{name: "array", author: "controlplane", value: func(string) string { return "[]" }},
		{name: "wrong_type_pid", author: "controlplane", value: func(id string) string {
			return validFields(id, func(m map[string]any) { m["pid"] = "not-a-number" })
		}},
		{name: "zero_pid", author: "controlplane", value: func(id string) string {
			return validFields(id, func(m map[string]any) { m["pid"] = 0 })
		}},
		{name: "missing_start_time", author: "controlplane", value: func(id string) string {
			return validFields(id, func(m map[string]any) { m["process_start_time"] = "" })
		}},
		{name: "wrong_author", author: "untrusted-agent", value: func(id string) string { return validFields(id, nil) }},
		{name: "cross_task_external_id", author: "controlplane", value: func(id string) string { return validFields(id, func(m map[string]any) { m["external_id"] = "other" }) }},
		{name: "mismatched_task_id", author: "controlplane", value: func(id string) string {
			return validFields(id, func(m map[string]any) { m["task_id"] = "task_other" })
		}},
		{name: "oversized", author: "controlplane", value: func(id string) string {
			return validFields(id, func(m map[string]any) { m["work_dir"] = strings.Repeat("a", 32*1024) })
		}},
		{name: "unknown_field", author: "controlplane", value: func(id string) string {
			return validFields(id, func(m map[string]any) { m["extra"] = "unexpected" })
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			adapter := newRestartStubAdapter(t, nil)
			r, st, _, _ := recoveryFixture(t, adapter, nil)

			task, err := r.SubmitTask(ctx, "coder", "malformed-"+tc.name, `{}`)
			if err != nil {
				t.Fatal(err)
			}
			if err := st.UpdateTaskState(ctx, task.ID, domain.TaskStateSubmitted, domain.TaskStateWorking, "dispatch_claimed", `{}`); err != nil {
				t.Fatal(err)
			}
			tok, err := model.MintTaskToken(ctx, st, task.ID, task.WorkloadName, time.Hour)
			if err != nil {
				t.Fatal(err)
			}
			if err := st.PutMemory(ctx, domain.MemoryEntry{
				Namespace: "system:runtime-handles",
				Key:       task.ID,
				Value:     tc.value(task.ID),
				Author:    tc.author,
				UpdatedAt: time.Now().UTC(),
			}); err != nil {
				t.Fatal(err)
			}

			reconcile(t, r)

			requireState(t, st, task.ID, domain.TaskStateFailed)
			if got := lastEventReason(t, st, task.ID); got != "lost_on_restart" {
				t.Fatalf("event reason = %q, want lost_on_restart", got)
			}
			if _, err := st.GetActiveLeaseByTokenHash(ctx, model.HashToken(tok), time.Now().UTC()); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("lease survived malformed handle: %v", err)
			}
			if adapter.stopCalls != 0 {
				t.Fatalf("Stop was called %d times on malformed handle", adapter.stopCalls)
			}
		})
	}
}

type failingHandlePutStore struct {
	store.Store
	failHandles bool
}

func (s failingHandlePutStore) PutMemory(ctx context.Context, entry domain.MemoryEntry) error {
	if s.failHandles && entry.Namespace == runtimeHandlesNamespace {
		return errors.New("handle store unavailable")
	}
	return s.Store.PutMemory(ctx, entry)
}

func TestHandlePersistFailureCleansUp(t *testing.T) {
	ctx := context.Background()
	adapter := newRestartStubAdapter(t, nil)
	r, st, _, _ := recoveryFixture(t, adapter, nil)
	r.store = failingHandlePutStore{Store: st, failHandles: true}

	task, err := r.SubmitTask(ctx, "coder", "persist-fail", `{}`)
	if err != nil {
		t.Fatal(err)
	}
	err = errors.Join(r.ReconcileOnce(ctx), waitForLaunches(t, r))
	if err == nil {
		t.Fatal("expected handle persistence error")
	}
	requireState(t, st, task.ID, domain.TaskStateFailed)
	if adapter.stopCalls != 1 {
		t.Fatalf("stopCalls = %d, want 1", adapter.stopCalls)
	}
	if adapter.procs[task.ID].alive() {
		t.Fatal("process remained alive after handle persistence failure")
	}
	if _, err := st.GetActiveLeaseByTokenHash(ctx, model.HashToken(adapter.lastSpec.TaskToken), time.Now().UTC()); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("lease survived handle persistence failure: %v", err)
	}
}

type stopFailOnceAdapter struct {
	*restartStubAdapter
	failStop bool
}

func (a *stopFailOnceAdapter) Stop(ctx context.Context, handle scruntime.Handle) error {
	a.mu.Lock()
	shouldFail := a.failStop
	a.mu.Unlock()
	if shouldFail {
		return errors.New("temporary stop failure")
	}
	return a.restartStubAdapter.Stop(ctx, handle)
}

func TestCancelAfterRestartRetriesFailedStopAndSkipsReusedPID(t *testing.T) {
	t.Run("retries_failed_stop_on_persisted_handle", func(t *testing.T) {
		ctx := context.Background()
		base := newRestartStubAdapter(t, nil)
		adapter := &stopFailOnceAdapter{restartStubAdapter: base}
		r1, st, bundle, pol := recoveryFixture(t, adapter, nil)

		task, err := r1.SubmitTask(ctx, "coder", "cancel-retry", `{}`)
		if err != nil {
			t.Fatal(err)
		}
		reconcile(t, r1)
		proc := base.procs[task.ID]

		adapter.failStop = true
		r2 := NewReconciler(Options{
			Store:    st,
			Bundle:   bundle,
			Policy:   pol,
			Adapters: map[domain.WorkloadClass]scruntime.Adapter{domain.ClassCodingAgent: adapter},
		})
		if err := r2.CancelTask(ctx, task.ID, "operator_cancelled"); err == nil {
			t.Fatal("expected stop failure from CancelTask")
		}
		if _, err := r2.RetryTaskWithHandoff(ctx, task.ID, false, ""); !errors.Is(err, store.ErrConflict) {
			t.Fatalf("retry before cleanup = %v, want ErrConflict", err)
		}

		// Clearing the stop error allows the next ReconcileOnce pass to finish stopping the process.
		adapter.failStop = false
		reconcile(t, r2)
		if proc.alive() {
			t.Fatal("live process was not stopped on reconcile retry")
		}
		if _, err := r2.RetryTaskWithHandoff(ctx, task.ID, false, ""); err != nil {
			t.Fatalf("retry after cleanup: %v", err)
		}
	})

	t.Run("does_not_kill_reused_pid", func(t *testing.T) {
		ctx := context.Background()
		adapter := newRestartStubAdapter(t, nil)
		r1, st, bundle, pol := recoveryFixture(t, adapter, nil)

		task, err := r1.SubmitTask(ctx, "coder", "cancel-reused", `{}`)
		if err != nil {
			t.Fatal(err)
		}
		reconcile(t, r1)
		adapter.procs[task.ID].finish(t)

		unrelated := startStubProcess(t, t.TempDir())
		unrelatedStart, err := scruntime.ProcessStartTime(unrelated.pid)
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := json.Marshal(persistedHandle{
			TaskID:           task.ID,
			WorkloadClass:    domain.ClassCodingAgent,
			AdapterName:      "coding:stub",
			ExternalID:       task.ID,
			WorkDir:          filepath.Join(adapter.rootDir, task.ID),
			Branch:           "sablecourt/" + task.ID,
			PID:              unrelated.pid,
			ProcessStartTime: unrelatedStart.Add(-3 * time.Second).UTC().Format(time.RFC3339Nano),
			LogPath:          filepath.Join(adapter.rootDir, task.ID) + ".log",
		})
		if err := st.PutMemory(ctx, domain.MemoryEntry{
			Namespace: runtimeHandlesNamespace,
			Key:       task.ID,
			Value:     string(raw),
			Author:    "controlplane",
			UpdatedAt: time.Now().UTC(),
		}); err != nil {
			t.Fatal(err)
		}

		r2 := NewReconciler(Options{
			Store:    st,
			Bundle:   bundle,
			Policy:   pol,
			Adapters: map[domain.WorkloadClass]scruntime.Adapter{domain.ClassCodingAgent: adapter},
		})
		if err := r2.CancelTask(ctx, task.ID, "operator_cancelled"); err != nil {
			t.Fatalf("CancelTask on reused PID: %v", err)
		}
		requireState(t, st, task.ID, domain.TaskStateCancelled)
		if adapter.stopCalls != 0 {
			t.Fatalf("Stop called %d times on reused PID", adapter.stopCalls)
		}
		if !unrelated.alive() {
			t.Fatal("unrelated process sharing reused PID was killed")
		}
	})
}

func TestRecoverAtStartupRespectsMaxConcurrentAndCancelledContext(t *testing.T) {
	ctx := context.Background()
	adapter := newRestartStubAdapter(t, nil)
	r1, st, bundle, pol := recoveryFixture(t, adapter, func(o *Options) {
		o.MaxConcurrent = 1
	})

	liveTask, err := r1.SubmitTask(ctx, "coder", "live-slot", `{}`)
	if err != nil {
		t.Fatal(err)
	}
	reconcile(t, r1)
	requireState(t, st, liveTask.ID, domain.TaskStateWorking)

	// Cancelled context must not falsely mark a live task lost_on_restart.
	rCanceled := NewReconciler(Options{
		Store:         forbidHistory{st},
		Bundle:        bundle,
		Policy:        pol,
		Adapters:      map[domain.WorkloadClass]scruntime.Adapter{domain.ClassCodingAgent: adapter},
		MaxConcurrent: 1,
	})
	canceledCtx, cancel := context.WithCancel(ctx)
	cancel()
	if err := rCanceled.Recover(canceledCtx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Recover(cancelled) = %v, want context.Canceled", err)
	}
	requireState(t, st, liveTask.ID, domain.TaskStateWorking)

	// Recover on a fresh reconciler adopts the live task without scanning history,
	// and the adopted live task still consumes its maxConcurrent slot.
	queued, err := r1.SubmitTask(ctx, "coder", "queued-behind-live", `{}`)
	if err != nil {
		t.Fatal(err)
	}
	r2 := NewReconciler(Options{
		Store:         forbidHistory{st},
		Bundle:        bundle,
		Policy:        pol,
		Adapters:      map[domain.WorkloadClass]scruntime.Adapter{domain.ClassCodingAgent: adapter},
		MaxConcurrent: 1,
	})
	if err := r2.Recover(ctx); err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if r2.handles[liveTask.ID].PID != adapter.procs[liveTask.ID].pid {
		t.Fatal("startup did not adopt the live handle")
	}
	reconcile(t, r2)
	requireState(t, st, liveTask.ID, domain.TaskStateWorking)
	requireState(t, st, queued.ID, domain.TaskStateSubmitted)

	// Once the recovered live task finishes, the queued task launches.
	adapter.procs[liveTask.ID].finish(t)
	reconcile(t, r2)
	requireState(t, st, liveTask.ID, domain.TaskStateCompleted)
	requireState(t, st, queued.ID, domain.TaskStateWorking)
}

type recoveryProbe struct {
	*restartStubAdapter
	probe func(context.Context, scruntime.Handle) (bool, error)
}

func (a *recoveryProbe) HandleAlive(ctx context.Context, h scruntime.Handle) (bool, error) {
	return a.probe(ctx, h)
}

func restartWorker(t *testing.T) (*Reconciler, *restartStubAdapter, domain.Task, Options) {
	t.Helper()
	a := newRestartStubAdapter(t, nil)
	r, st, b, p := recoveryFixture(t, a, nil)
	task, err := r.SubmitTask(t.Context(), "coder", "first", "{}")
	if err != nil {
		t.Fatal(err)
	}
	reconcile(t, r)
	opts := Options{Store: st, Bundle: b, Policy: p, Adapters: map[domain.WorkloadClass]scruntime.Adapter{domain.ClassCodingAgent: a}, MaxConcurrent: 1}
	return NewReconciler(opts), a, task, opts
}

func TestRecoveryProbeRetry(t *testing.T) {
	r, a, task, _ := restartWorker(t)
	broken := errors.New("probe unavailable")
	r.adapters[domain.ClassCodingAgent] = &recoveryProbe{a, func(context.Context, scruntime.Handle) (bool, error) { return false, broken }}
	if err := r.Recover(t.Context()); !errors.Is(err, broken) {
		t.Errorf("probe error = %v", err)
	}
	requireState(t, r.store, task.ID, domain.TaskStateWorking)
	if _, err := r.store.GetActiveLeaseByTokenHash(t.Context(), model.HashToken(a.lastSpec.TaskToken), time.Now()); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("lease remained active", err)
	}
	r.adapters[domain.ClassCodingAgent] = a
	if err := r.Recover(t.Context()); err != nil || r.handles[task.ID].PID == 0 {
		t.Fatal("retry failed to adopt", err)
	}
}

func TestRecoverySingleOwner(t *testing.T) {
	r, _, task, opts := restartWorker(t)
	peer := NewReconciler(opts)
	start := make(chan struct{})
	done := make(chan error, 2)
	for _, rec := range []*Reconciler{r, peer} {
		go func() { <-start; done <- rec.Recover(t.Context()) }()
	}
	close(start)
	for range 2 {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	reconcile(t, r)
	reconcile(t, peer)
	if (r.handles[task.ID].PID == 0) == (peer.handles[task.ID].PID == 0) {
		t.Fatal("expected exactly one owner")
	}
}

type unreadableHandle struct{ store.Store }

func (s unreadableHandle) GetMemory(ctx context.Context, ns, id string) (domain.MemoryEntry, error) {
	if ns == runtimeHandlesNamespace {
		return domain.MemoryEntry{}, errors.New("handle read failed")
	}
	return s.Store.GetMemory(ctx, ns, id)
}
func TestUnreadableHandleReservesSlot(t *testing.T) {
	r, _, _, _ := restartWorker(t)
	q, err := r.SubmitTask(t.Context(), "coder", "queued", "{}")
	if err != nil {
		t.Fatal(err)
	}
	r.store = unreadableHandle{r.store}
	if err := r.ReconcileOnce(t.Context()); err == nil {
		t.Error("missing read error")
	}
	if err := waitForLaunches(t, r); err != nil {
		t.Fatal(err)
	}
	requireState(t, r.store, q.ID, domain.TaskStateSubmitted)
}

func TestBlockedRecoveryReservesSlot(t *testing.T) {
	r, a, _, _ := restartWorker(t)
	q, err := r.SubmitTask(t.Context(), "coder", "queued", "{}")
	if err != nil {
		t.Fatal(err)
	}
	start, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	r.adapters[domain.ClassCodingAgent] = &recoveryProbe{a, func(ctx context.Context, h scruntime.Handle) (bool, error) {
		once.Do(func() { close(start) })
		<-release
		return a.HandleAlive(ctx, h)
	}}
	done := make(chan error, 1)
	go func() { done <- r.Recover(t.Context()) }()
	<-start
	pass := make(chan error, 1)
	go func() { pass <- r.ReconcileOnce(t.Context()) }()
	select {
	case err := <-pass:
		if err != nil {
			t.Error(err)
		}
	case <-time.After(time.Second):
		t.Error("reconciliation blocked on startup probe")
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	requireState(t, r.store, q.ID, domain.TaskStateSubmitted)
}

func TestRestartRetriesPersistedStop(t *testing.T) {
	a := &stopFailOnceAdapter{restartStubAdapter: newRestartStubAdapter(t, nil)}
	r, st, b, p := recoveryFixture(t, a, nil)
	task, err := r.SubmitTask(t.Context(), "coder", "stop-retry", "{}")
	if err != nil {
		t.Fatal(err)
	}
	reconcile(t, r)
	a.failStop = true
	if err := r.CancelTask(t.Context(), task.ID, "cancel"); err == nil {
		t.Fatal("missing stop failure")
	}
	if _, err := st.GetActiveLeaseByTokenHash(t.Context(), model.HashToken(a.lastSpec.TaskToken), time.Now()); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("lease survived cancellation", err)
	}
	a.failStop = false
	fresh := NewReconciler(Options{Store: st, Bundle: b, Policy: p, Adapters: r.adapters})
	if err := fresh.Recover(t.Context()); err != nil {
		t.Fatal(err)
	}
	if a.procs[task.ID].alive() {
		t.Fatal("restart skipped surviving worker")
	}
}

func TestRecoverRecordedExit(t *testing.T) {
	r, a, task, _ := restartWorker(t)
	a.procs[task.ID].finish(t)
	r.adapters[domain.ClassCodingAgent] = recordedStub{a}
	requireState(t, r.store, task.ID, domain.TaskStateWorking)
	if err := r.Recover(t.Context()); err != nil {
		t.Fatal(err)
	}
	if r.handles[task.ID].PID == 0 {
		t.Fatal("discarded durable exit")
	}
	reconcile(t, r)
	requireState(t, r.store, task.ID, domain.TaskStateCompleted)
}

type recordedStub struct{ *restartStubAdapter }

func (a recordedStub) HandleExited(_ context.Context, h scruntime.Handle) (bool, error) {
	p := a.procs[h.ExternalID]
	return p != nil && p.pid == h.PID && h.ProcessStartTime.Equal(p.start) && !p.alive(), nil
}

func TestMissingLaunchIdentity(t *testing.T) {
	r, _, _, _ := recoveryFixture(t, newRestartStubAdapter(t, nil), nil)
	h := scruntime.Handle{PID: os.Getpid(), AdapterName: "coding:stub", ExternalID: "missing-start"}
	if err := r.saveHandle(t.Context(), h.ExternalID, domain.ClassCodingAgent, h); err == nil {
		t.Fatal("accepted a process without launch-time identity")
	}
}
