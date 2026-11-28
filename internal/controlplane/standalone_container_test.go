package controlplane

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/shadewright-labs/sablecourt/internal/config"
	"github.com/shadewright-labs/sablecourt/internal/domain"
	"github.com/shadewright-labs/sablecourt/internal/policy"
	scruntime "github.com/shadewright-labs/sablecourt/internal/runtime"
	"github.com/shadewright-labs/sablecourt/internal/runtime/coding"
	"github.com/shadewright-labs/sablecourt/internal/store/sqlite"
)

// TestStandaloneCodingTaskRunsInHarnessContainer checks that the reconciler
// sends a standalone coding task (no council assignment) to the same harness
// container runner as a council seat, with no council deadline and the
// container data gate, and that the task completes with its commit.
func TestStandaloneCodingTaskRunsInHarnessContainer(t *testing.T) {
	run := runStandaloneContainerTask(t, `{"prompt":"implement the change"}`)
	if run.state != domain.TaskStateCompleted || len(run.launches) != 1 {
		t.Fatalf("state = %s, container launches = %d, want completed and 1", run.state, len(run.launches))
	}
	l, proxy := run.launches[0], run.proxy
	if l.Checkout.Kind != scruntime.CouncilTask || !strings.Contains(l.Prompt, "implement the change") || l.TaskToken == "" ||
		!strings.HasSuffix(l.DataGateURL, "/gate/mcp") || strings.Contains(l.DataGateURL, "127.0.0.1") {
		t.Fatalf("standalone launch = %+v", l)
	}
	if len(proxy.proxies) != 0 || len(proxy.networks) != 0 {
		t.Fatal("standalone task left its proxy or network behind")
	}
}

// TestStandaloneCodingTaskIgnoresCallerCheckoutSelector checks that the
// checkout kind comes from the task type, never from submission input: an
// ordinary task that names a council checkout mode (any case) still runs in
// its own task worktree, never in the primary repository or a persistent
// council checkout.
func TestStandaloneCodingTaskIgnoresCallerCheckoutSelector(t *testing.T) {
	t.Parallel()
	// A selector that names a room which is no council leaves an input the
	// adapter refuses (a council field without a reservation): fail closed,
	// with no launch at all.
	for name, tc := range map[string]struct {
		input   string
		refused bool
	}{
		"live":       {input: `{"prompt":"inspect repo","checkoutMode":"live"}`},
		"live-mixed": {input: `{"prompt":"inspect repo","CheckoutMode":"live","checkoutmode":"live"}`},
		"source-id":  {input: `{"prompt":"inspect repo","checkoutMode":"live","checkoutSourceId":"1:2"}`},
		"ref":        {input: `{"prompt":"inspect repo","checkoutMode":"ref","checkoutRef":"HEAD","conclaveId":"fake-room"}`, refused: true},
		"writer":     {input: `{"prompt":"edit","checkoutMode":"writer","conclaveId":"fake-room","role":"coder","checkoutWriter":"coder"}`, refused: true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			run := runStandaloneContainerTask(t, tc.input)
			if _, err := os.Stat(filepath.Join(run.worktrees, ".councils")); !os.IsNotExist(err) {
				t.Fatalf("ordinary task created a persistent council checkout: %v", err)
			}
			if tc.refused {
				if run.state != domain.TaskStateFailed || len(run.launches) != 0 {
					t.Fatalf("state = %s, launches = %d, want a refusal with no launch", run.state, len(run.launches))
				}
				return
			}
			if run.state != domain.TaskStateCompleted || len(run.launches) != 1 {
				t.Fatalf("state = %s, launches = %d, want completed and 1", run.state, len(run.launches))
			}
			l := run.launches[0]
			if l.Checkout.Kind != scruntime.CouncilTask {
				t.Fatalf("checkout kind = %q, want %q", l.Checkout.Kind, scruntime.CouncilTask)
			}
			if filepath.Dir(l.Checkout.Path) != run.worktrees {
				t.Fatalf("checkout path = %q, want a task worktree under %q", l.Checkout.Path, run.worktrees)
			}
		})
	}
}

type standaloneRun struct {
	launches  []scruntime.CouncilLaunch
	state     domain.TaskState
	proxy     *cpFakeProxyDocker
	worktrees string
}

func runStandaloneContainerTask(t *testing.T, input string) standaloneRun {
	t.Helper()
	ctx := t.Context()
	repo, _ := initCPTestRepo(t)
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
			Class:   domain.ClassCodingAgent,
			Runtime: domain.RuntimeConfig{Type: "worktree-cli"},
			Coding:  &domain.CodingConfig{Harness: "claude", RepoPath: repo, Sandbox: "container"},
		},
	}
	if err := st.UpsertWorkload(ctx, domain.WorkloadRecord{Name: "coder", Zone: domain.ZoneRuntime, Class: domain.ClassCodingAgent}); err != nil {
		t.Fatal(err)
	}
	runner := newCPFakeCouncilRunner()
	runner.onRun = func(_ context.Context, s scruntime.CouncilLaunch) (string, string, int, error) {
		return "standalone done", "", 0, nil
	}
	proxy := newCPFakeProxyDocker()
	worktrees := filepath.Join(t.TempDir(), "worktrees")
	adapter := coding.NewAdapter(worktrees)
	adapter.CouncilRunner = runner
	adapter.CouncilProxy = proxy
	adapter.CouncilProxyImage = "sha256:" + strings.Repeat("d", 64)
	adapter.DaemonListen = net.JoinHostPort(net.IPv6loopback.String(), "7432")
	r := NewReconciler(Options{
		Store:           st,
		Bundle:          &config.Bundle{Workloads: map[string]domain.Workload{"coder": wl}},
		Policy:          policy.NewEngine(nil, nil),
		Adapters:        map[domain.WorkloadClass]scruntime.Adapter{domain.ClassCodingAgent: adapter},
		HostDataGateURL: "http://127.0.0.1:7432/gate/mcp",
	})
	task, err := r.SubmitTask(ctx, "coder", "standalone-1", input)
	if err != nil {
		t.Fatal(err)
	}
	// A refused start is reported by the launch; the terminal state is what counts.
	step := func() { _ = errors.Join(r.ReconcileOnce(ctx), waitForLaunches(t, r)) }
	step()
	deadline := time.Now().Add(5 * time.Second)
	var state domain.TaskState
	for {
		step()
		got, err := st.GetTask(ctx, task.ID)
		if err != nil {
			t.Fatal(err)
		}
		state = got.State
		if state == domain.TaskStateCompleted || state == domain.TaskStateFailed {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("task state = %s, want a terminal state", got.State)
		}
		time.Sleep(20 * time.Millisecond)
	}
	runner.mu.Lock()
	launches := append([]scruntime.CouncilLaunch(nil), runner.launches...)
	runner.mu.Unlock()
	resolved, err := filepath.EvalSymlinks(worktrees)
	if err != nil {
		resolved = worktrees
	}
	return standaloneRun{launches: launches, state: state, proxy: proxy, worktrees: resolved}
}

// TestCodingTaskRefusalRecordsMissingRuntime checks that a coding task the
// adapter refuses (no container runtime, host mode off) fails with a durable
// reason naming the missing runtime, not an empty launch failure.
func TestCodingTaskRefusalRecordsMissingRuntime(t *testing.T) {
	ctx := t.Context()
	repo, _ := initCPTestRepo(t)
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
			Class:   domain.ClassCodingAgent,
			Runtime: domain.RuntimeConfig{Type: "worktree-cli"},
			Coding:  &domain.CodingConfig{Harness: "claude", RepoPath: repo, Sandbox: "container"},
		},
	}
	if err := st.UpsertWorkload(ctx, domain.WorkloadRecord{Name: "coder", Zone: domain.ZoneRuntime, Class: domain.ClassCodingAgent}); err != nil {
		t.Fatal(err)
	}
	// The daemon's default adapter: recovery only, no runner, no host opt-in.
	adapter := coding.NewAdapter(filepath.Join(t.TempDir(), "worktrees"))
	r := NewReconciler(Options{
		Store:    st,
		Bundle:   &config.Bundle{Workloads: map[string]domain.Workload{"coder": wl}},
		Policy:   policy.NewEngine(nil, nil),
		Adapters: map[domain.WorkloadClass]scruntime.Adapter{domain.ClassCodingAgent: adapter},
	})
	task, err := r.SubmitTask(ctx, "coder", "refused-1", `{"prompt":"edit"}`)
	if err != nil {
		t.Fatal(err)
	}
	_ = errors.Join(r.ReconcileOnce(ctx), waitForLaunches(t, r))
	requireState(t, st, task.ID, domain.TaskStateFailed)
	events, err := st.ListTaskEvents(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, ev := range events {
		if ev.Reason != "launch_failed" {
			continue
		}
		if !strings.Contains(ev.FactJSON, `"reason":"coding_runtime_unavailable"`) || !strings.Contains(ev.FactJSON, "host mode is off") {
			t.Fatalf("launch_failed facts = %s, want the missing coding runtime", ev.FactJSON)
		}
		return
	}
	t.Fatalf("no launch_failed event: %+v", events)
}
