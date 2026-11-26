package controlplane

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/shadewright-labs/sablecourt/internal/config"
	"github.com/shadewright-labs/sablecourt/internal/domain"
	"github.com/shadewright-labs/sablecourt/internal/policy"
	scruntime "github.com/shadewright-labs/sablecourt/internal/runtime"
	"github.com/shadewright-labs/sablecourt/internal/runtime/controller"
	"github.com/shadewright-labs/sablecourt/internal/store/sqlite"
)

type fakeAdapter struct {
	nextState    scruntime.ObservedState
	nextExitCode int
	nextMessage  string
	nextLogClose bool
	lastSpec     scruntime.LaunchSpec
}

func (f *fakeAdapter) Start(_ context.Context, spec scruntime.LaunchSpec) (scruntime.Handle, error) {
	f.lastSpec = spec
	return scruntime.Handle{AdapterName: "fake", ExternalID: spec.TaskID}, nil
}
func (f *fakeAdapter) Observe(_ context.Context, _ scruntime.Handle) (scruntime.Observation, error) {
	return scruntime.Observation{
		State:          f.nextState,
		ExitCode:       f.nextExitCode,
		Message:        f.nextMessage,
		LogCloseFailed: f.nextLogClose,
		ObservedAt:     time.Now().UTC(),
	}, nil
}
func (f *fakeAdapter) Suspend(_ context.Context, _ scruntime.Handle) (scruntime.CheckpointRef, error) {
	return scruntime.CheckpointRef{}, nil
}
func (f *fakeAdapter) Resume(_ context.Context, _ scruntime.Handle, _ scruntime.CheckpointRef) error {
	return nil
}
func (f *fakeAdapter) Stop(_ context.Context, _ scruntime.Handle) error { return nil }
func (f *fakeAdapter) CollectArtifacts(_ context.Context, _ scruntime.Handle) ([]scruntime.Artifact, error) {
	return nil, nil
}

func TestReconcilerLifecycleAndHubSpokeDelegation(t *testing.T) {
	ctx := context.Background()
	st, err := sqlite.Open(filepath.Join(t.TempDir(), "sablecourt.db"))
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	bundle := &config.Bundle{
		Policies: map[string]domain.Policy{
			"default": {
				Metadata: domain.ObjectMeta{Name: "default"},
				Spec: domain.PolicySpec{
					DefaultEffect: domain.EffectDeny,
					Rules: []domain.PolicyRule{
						{Workload: "triage-agent", Capability: "delegate:go-coder", Effect: domain.EffectAllow},
					},
				},
			},
		},
		Workloads: map[string]domain.Workload{
			"triage-agent": {
				Metadata: domain.ObjectMeta{Name: "triage-agent", Zone: domain.ZoneRuntime},
				Spec: domain.WorkloadSpec{
					Class: domain.ClassLongLivedAgent,
					Grants: domain.GrantSet{
						ModelGate:  domain.ModelGateGrant{Allowed: true, BudgetRef: "b1"},
						DelegateTo: []string{"go-coder"},
					},
				},
			},
			"go-coder": {
				Metadata: domain.ObjectMeta{Name: "go-coder", Zone: domain.ZoneRuntime},
				Spec: domain.WorkloadSpec{
					Class:  domain.ClassCodingAgent,
					Grants: domain.GrantSet{ModelGate: domain.ModelGateGrant{Allowed: true, BudgetRef: "b1"}},
				},
			},
			"health-ctrl": {
				Metadata: domain.ObjectMeta{Name: "health-ctrl", Zone: domain.ZoneRuntime},
				Spec: domain.WorkloadSpec{
					Class:  domain.ClassDeterministicController,
					Grants: domain.GrantSet{ModelGate: domain.ModelGateGrant{Allowed: false}},
				},
			},
		},
	}

	for _, w := range bundle.Workloads {
		_ = st.UpsertWorkload(ctx, domain.WorkloadRecord{
			Name: w.Metadata.Name, Zone: w.Metadata.Zone, Class: w.Spec.Class,
			ManifestDigest: "m1", AgentCardJSON: "{}",
		})
	}

	longAdapter := &fakeAdapter{nextState: scruntime.StateRunning}
	codeAdapter := &fakeAdapter{nextState: scruntime.StateCompleted}
	ctrlInner := &fakeAdapter{nextState: scruntime.StateCompleted}
	ctrlAdapter := controller.NewAdapter(ctrlInner)

	eng := policy.NewEngine([]domain.Policy{bundle.Policies["default"]}, nil)
	rec := NewReconciler(Options{
		Store:        st,
		Bundle:       bundle,
		Policy:       eng,
		ModelGateURL: "http://127.0.0.1:7432/gate/model",
		DataGateURL:  "http://127.0.0.1:7432/gate/mcp",
		Adapters: map[domain.WorkloadClass]scruntime.Adapter{
			domain.ClassLongLivedAgent:          longAdapter,
			domain.ClassCodingAgent:             codeAdapter,
			domain.ClassDeterministicController: ctrlAdapter,
		},
	})

	parent, err := rec.SubmitTask(ctx, "triage-agent", "idem-p", `{"issue":7}`)
	if err != nil {
		t.Fatalf("SubmitTask parent: %v", err)
	}
	ctrlTask, err := rec.SubmitTask(ctx, "health-ctrl", "idem-c", `{}`)
	if err != nil {
		t.Fatalf("SubmitTask controller: %v", err)
	}

	// Pass 1: dispatch submitted tasks.
	reconcile(t, rec)
	if ctrlInner.lastSpec.ModelGateToken != "" || ctrlInner.lastSpec.ModelGateURL != "" {
		t.Fatalf("deterministic controller received Model Gate credentials: %+v", ctrlInner.lastSpec)
	}
	if longAdapter.lastSpec.ModelGateToken == "" {
		t.Fatal("long-lived agent did not receive Model Gate token")
	}

	// Hub-and-spoke child delegation: allowed for go-coder, denied for health-ctrl.
	child, err := rec.ProposeChildTask(ctx, parent.ID, "go-coder", "idem-child", `{"fix":7}`, `{"maxCostUSD":0.50}`)
	if err != nil {
		t.Fatalf("ProposeChildTask allowed: %v", err)
	}
	if child.ParentTaskID != parent.ID {
		t.Fatalf("expected parent %q, got %q", parent.ID, child.ParentTaskID)
	}
	if _, err := rec.ProposeChildTask(ctx, parent.ID, "health-ctrl", "idem-denied", `{}`, `{}`); err == nil {
		t.Fatal("expected delegation to ungranted target to fail")
	}

	// Pass 2: observe working controller -> completed.
	reconcile(t, rec)
	gotCtrl, _ := st.GetTask(ctx, ctrlTask.ID)
	if gotCtrl.State != domain.TaskStateCompleted {
		t.Fatalf("expected controller completed, got %q", gotCtrl.State)
	}

	// Suspend and resume the long-lived parent task with checkpoint persistence.
	if err := rec.SuspendTask(ctx, parent.ID, "pause for review"); err != nil {
		t.Fatalf("SuspendTask: %v", err)
	}
	gotParent, _ := st.GetTask(ctx, parent.ID)
	if gotParent.State != domain.TaskStateSuspended {
		t.Fatalf("expected parent suspended, got %q", gotParent.State)
	}
	if _, err := st.LatestCheckpoint(ctx, parent.ID); err != nil {
		t.Fatalf("expected saved checkpoint, got %v", err)
	}
	if err := rec.ResumeTask(ctx, parent.ID); err != nil {
		t.Fatalf("ResumeTask: %v", err)
	}
	gotParent, _ = st.GetTask(ctx, parent.ID)
	if gotParent.State != domain.TaskStateWorking {
		t.Fatalf("expected parent working after resume, got %q", gotParent.State)
	}
}

func TestReconcilerRecordsFailureReasonOnRuntimeExit(t *testing.T) {
	for _, tc := range []struct {
		name           string
		obsState       scruntime.ObservedState
		exitCode       int
		message        string
		wantTaskState  domain.TaskState
		wantExactFacts string
		wantReasonSub  string
		forbidSub      string
	}{
		{
			name:          "failed_exit_records_reason",
			obsState:      scruntime.StateFailed,
			exitCode:      -1,
			message:       "close coding stdout log: stdout disk full",
			wantTaskState: domain.TaskStateFailed,
			wantReasonSub: "close coding stdout log: stdout disk full",
		},
		{
			name:          "failed_exit_redacts_secret_in_reason",
			obsState:      scruntime.StateFailed,
			exitCode:      -1,
			message:       "close coding stderr log: password=fixture-value",
			wantTaskState: domain.TaskStateFailed,
			wantReasonSub: "[REDACTED:secret]",
			forbidSub:     "fixture-value",
		},
		{
			name:          "failed_exit_redacts_dsn_and_unclosed_pem",
			obsState:      scruntime.StateFailed,
			exitCode:      1,
			message:       "connect postgres://app:fixture-dsn-pass" + "@" + "db.invalid/sc -----BEGIN " + "PRIVATE KEY-----\nMIIEfixturepembody",
			wantTaskState: domain.TaskStateFailed,
			wantReasonSub: "[REDACTED:userinfo]",
			forbidSub:     "fixture-dsn-pass",
		},
		{
			name:          "failed_exit_truncates_multibyte_utf8_at_1024",
			obsState:      scruntime.StateFailed,
			exitCode:      1,
			message:       strings.Repeat("a", 1022) + "\U0001F680" + "trailing",
			wantTaskState: domain.TaskStateFailed,
			wantReasonSub: strings.Repeat("a", 100),
		},
		{
			name:          "failed_exit_normalizes_invalid_utf8_before_1024_limit",
			obsState:      scruntime.StateFailed,
			exitCode:      1,
			message:       strings.Repeat("a\xff", 400),
			wantTaskState: domain.TaskStateFailed,
			wantReasonSub: "a\uFFFDa",
		},
		{
			name:          "failed_exit_normalizes_invalid_utf8_at_1024_boundary",
			obsState:      scruntime.StateFailed,
			exitCode:      1,
			message:       strings.Repeat("a", 1022) + "\xff\xff",
			wantTaskState: domain.TaskStateFailed,
			wantReasonSub: strings.Repeat("a", 100),
		},
		{
			name:          "failed_exit_redacts_secret_straddling_1024",
			obsState:      scruntime.StateFailed,
			exitCode:      1,
			message:       strings.Repeat("b", 1015) + "sk-" + "ant-fixturesecret1234567890abcdef" + "tail",
			wantTaskState: domain.TaskStateFailed,
			wantReasonSub: strings.Repeat("b", 100),
			forbidSub:     "fixturesecret",
		},
		{
			name:           "completed_exit_keeps_strict_fact_json",
			obsState:       scruntime.StateCompleted,
			exitCode:       0,
			message:        "nonfatal stderr line",
			wantTaskState:  domain.TaskStateCompleted,
			wantExactFacts: `{"exit_code":0}`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			st, err := sqlite.Open(filepath.Join(t.TempDir(), "sablecourt.db"))
			if err != nil {
				t.Fatalf("open sqlite: %v", err)
			}
			t.Cleanup(func() { _ = st.Close() })
			if err := st.Migrate(ctx); err != nil {
				t.Fatalf("migrate: %v", err)
			}

			bundle := &config.Bundle{
				Workloads: map[string]domain.Workload{
					"go-coder": {
						Metadata: domain.ObjectMeta{Name: "go-coder", Zone: domain.ZoneRuntime},
						Spec:     domain.WorkloadSpec{Class: domain.ClassCodingAgent},
					},
				},
			}
			_ = st.UpsertWorkload(ctx, domain.WorkloadRecord{
				Name: "go-coder", Zone: domain.ZoneRuntime, Class: domain.ClassCodingAgent,
				ManifestDigest: "m1", AgentCardJSON: "{}",
			})

			adapter := &fakeAdapter{
				nextState:    tc.obsState,
				nextExitCode: tc.exitCode,
				nextMessage:  tc.message,
			}
			rec := NewReconciler(Options{
				Store:  st,
				Bundle: bundle,
				Policy: policy.NewEngine(nil, nil),
				Adapters: map[domain.WorkloadClass]scruntime.Adapter{
					domain.ClassCodingAgent: adapter,
				},
			})

			task, err := rec.SubmitTask(ctx, "go-coder", "idem-"+tc.name, `{}`)
			if err != nil {
				t.Fatalf("SubmitTask: %v", err)
			}
			reconcile(t, rec)
			reconcile(t, rec)

			got, err := st.GetTask(ctx, task.ID)
			if err != nil {
				t.Fatalf("GetTask: %v", err)
			}
			if got.State != tc.wantTaskState {
				t.Fatalf("task state = %q, want %q", got.State, tc.wantTaskState)
			}

			events, err := st.ListTaskEvents(ctx, task.ID)
			if err != nil {
				t.Fatalf("ListTaskEvents: %v", err)
			}
			checked := 0
			for _, ev := range events {
				if ev.Reason != "runtime_exit_observed" && ev.Reason != "runtime_failed" && ev.Reason != "runtime_completed" {
					continue
				}
				checked++
				if tc.wantExactFacts != "" {
					if ev.FactJSON != tc.wantExactFacts {
						t.Fatalf("event %q FactJSON = %q, want %q", ev.Reason, ev.FactJSON, tc.wantExactFacts)
					}
					continue
				}
				var fact struct {
					ExitCode int    `json:"exit_code"`
					Reason   string `json:"reason"`
				}
				if err := json.Unmarshal([]byte(ev.FactJSON), &fact); err != nil {
					t.Fatalf("unmarshal event %q FactJSON %q: %v", ev.Reason, ev.FactJSON, err)
				}
				if fact.ExitCode != tc.exitCode {
					t.Fatalf("event %q exit_code = %d, want %d", ev.Reason, fact.ExitCode, tc.exitCode)
				}
				if !strings.Contains(fact.Reason, tc.wantReasonSub) {
					t.Fatalf("event %q reason = %q (raw %s), want substring %q", ev.Reason, fact.Reason, ev.FactJSON, tc.wantReasonSub)
				}
				if len(fact.Reason) > maxRuntimeExitReasonBytes {
					t.Fatalf("event %q reason length = %d, want <= %d", ev.Reason, len(fact.Reason), maxRuntimeExitReasonBytes)
				}
				if !utf8.ValidString(fact.Reason) {
					t.Fatalf("event %q reason is not valid UTF-8: %q", ev.Reason, fact.Reason)
				}
				if tc.forbidSub != "" && strings.Contains(ev.FactJSON, tc.forbidSub) {
					t.Fatalf("event %q FactJSON leaked %q: %s", ev.Reason, tc.forbidSub, ev.FactJSON)
				}
			}
			if checked < 2 {
				t.Fatalf("expected at least 2 terminal exit events, checked %d (%+v)", checked, events)
			}
		})
	}
}

func TestReconcilerSkipsAutoRemediateOnLogCloseFailure(t *testing.T) {
	for _, tc := range []struct {
		name         string
		exitCode     int
		logClose     bool
		message      string
		wantTasks    int
		wantLogClose bool
	}{
		{
			name:         "exit_0_log_close_failure_does_not_retry",
			exitCode:     scruntime.ExitCodeLogClose,
			logClose:     true,
			message:      "close coding stdout log: stdout disk full",
			wantTasks:    1,
			wantLogClose: true,
		},
		{
			name:         "nonzero_exit_with_log_close_failure_does_not_retry",
			exitCode:     17,
			logClose:     true,
			message:      "close coding stdout log: stdout disk full\nharness failed",
			wantTasks:    1,
			wantLogClose: true,
		},
		{
			name:         "ordinary_failure_still_auto_remediates",
			exitCode:     1,
			logClose:     false,
			message:      "tests failed",
			wantTasks:    2,
			wantLogClose: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			st, err := sqlite.Open(filepath.Join(t.TempDir(), "sablecourt.db"))
			if err != nil {
				t.Fatalf("open sqlite: %v", err)
			}
			t.Cleanup(func() { _ = st.Close() })
			if err := st.Migrate(ctx); err != nil {
				t.Fatalf("migrate: %v", err)
			}

			bundle := &config.Bundle{
				Workloads: map[string]domain.Workload{
					"go-coder": {
						Metadata: domain.ObjectMeta{Name: "go-coder", Zone: domain.ZoneRuntime},
						Spec:     domain.WorkloadSpec{Class: domain.ClassCodingAgent},
					},
				},
			}
			_ = st.UpsertWorkload(ctx, domain.WorkloadRecord{
				Name: "go-coder", Zone: domain.ZoneRuntime, Class: domain.ClassCodingAgent,
				ManifestDigest: "m1", AgentCardJSON: "{}",
			})

			adapter := &fakeAdapter{
				nextState:    scruntime.StateFailed,
				nextExitCode: tc.exitCode,
				nextMessage:  tc.message,
				nextLogClose: tc.logClose,
			}
			rec := NewReconciler(Options{
				Store:  st,
				Bundle: bundle,
				Policy: policy.NewEngine(nil, nil),
				Adapters: map[domain.WorkloadClass]scruntime.Adapter{
					domain.ClassCodingAgent: adapter,
				},
			})

			task, err := rec.SubmitTask(ctx, "go-coder", "idem-"+tc.name, `{"prompt":"edit","auto_remediate":true,"max_retries":2}`)
			if err != nil {
				t.Fatalf("SubmitTask: %v", err)
			}
			reconcile(t, rec)
			reconcile(t, rec)

			got, err := st.GetTask(ctx, task.ID)
			if err != nil {
				t.Fatalf("GetTask: %v", err)
			}
			if got.State != domain.TaskStateFailed {
				t.Fatalf("task state = %q, want failed", got.State)
			}
			tasks, err := st.ListTasks(ctx)
			if err != nil {
				t.Fatalf("ListTasks: %v", err)
			}
			if len(tasks) != tc.wantTasks {
				t.Fatalf("task count = %d, want %d (%+v)", len(tasks), tc.wantTasks, tasks)
			}
			events, err := st.ListTaskEvents(ctx, task.ID)
			if err != nil {
				t.Fatalf("ListTaskEvents: %v", err)
			}
			if len(events) == 0 || events[len(events)-1].Reason != "runtime_failed" {
				t.Fatalf("expected final runtime_failed event, got %+v", events)
			}
			var fact struct {
				ExitCode int    `json:"exit_code"`
				Reason   string `json:"reason"`
				LogClose bool   `json:"log_close"`
			}
			if err := json.Unmarshal([]byte(events[len(events)-1].FactJSON), &fact); err != nil {
				t.Fatalf("unmarshal final FactJSON %q: %v", events[len(events)-1].FactJSON, err)
			}
			if fact.ExitCode != tc.exitCode || fact.LogClose != tc.wantLogClose {
				t.Fatalf("final fact = %+v, want exit_code=%d log_close=%v", fact, tc.exitCode, tc.wantLogClose)
			}
		})
	}
}
