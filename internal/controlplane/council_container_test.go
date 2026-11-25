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
	"github.com/shadewright-labs/sablecourt/internal/gate/councilproxy"
	"github.com/shadewright-labs/sablecourt/internal/gate/model"
	"github.com/shadewright-labs/sablecourt/internal/policy"
	scruntime "github.com/shadewright-labs/sablecourt/internal/runtime"
	"github.com/shadewright-labs/sablecourt/internal/runtime/coding"
	"github.com/shadewright-labs/sablecourt/internal/store"
	"github.com/shadewright-labs/sablecourt/internal/store/sqlite"
)

type cpFakeProxyDocker struct {
	mu              sync.Mutex
	proxies         map[string]councilproxy.ProxyContainer
	networks        map[string]bool
	removedProxies  []string
	removedNetworks []string
}

func newCPFakeProxyDocker() *cpFakeProxyDocker {
	return &cpFakeProxyDocker{
		proxies:  make(map[string]councilproxy.ProxyContainer),
		networks: make(map[string]bool),
	}
}

func (f *cpFakeProxyDocker) CreateInternalNetwork(_ context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.networks[name] = true
	return nil
}

func (f *cpFakeProxyDocker) CreateProxy(_ context.Context, spec councilproxy.ProxyContainer) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.proxies[spec.Name] = spec
	return nil
}

func (f *cpFakeProxyDocker) ConnectProxyEgress(_ context.Context, _ string) error { return nil }
func (f *cpFakeProxyDocker) StartProxy(_ context.Context, _ string) error         { return nil }

func (f *cpFakeProxyDocker) RemoveProxy(_ context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.proxies, name)
	f.removedProxies = append(f.removedProxies, name)
	return nil
}

func (f *cpFakeProxyDocker) RemoveInternalNetwork(_ context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.networks, name)
	f.removedNetworks = append(f.removedNetworks, name)
	return nil
}

type cpFakeContainer struct {
	name     string
	attempt  string
	id       string
	running  bool
	exited   bool
	exitCode int
	finished time.Time
	exists   bool
}

type cpFakeCouncilRunner struct {
	mu         sync.Mutex
	starts     int
	launches   []scruntime.CouncilLaunch
	containers map[string]*cpFakeContainer
	releaseCh  chan struct{}
	onRun      func(ctx context.Context, s scruntime.CouncilLaunch) (stdout, stderr string, exitCode int, err error)
}

func newCPFakeCouncilRunner() *cpFakeCouncilRunner {
	return &cpFakeCouncilRunner{
		containers: make(map[string]*cpFakeContainer),
	}
}

func (r *cpFakeCouncilRunner) RunCouncil(ctx context.Context, s scruntime.CouncilLaunch, record scruntime.CouncilRecorder) (scruntime.CouncilResult, error) {
	r.mu.Lock()
	r.starts++
	seq := r.starts
	r.launches = append(r.launches, s)
	name := "sablecourt-council-" + s.TaskID
	attempt := s.CreateAttempt
	if attempt == "" {
		attempt = "attempt"
	}
	id := "cid-" + s.TaskID + "-" + string(rune('0'+seq))
	c := &cpFakeContainer{name: name, attempt: attempt, id: id, exists: true}
	r.containers[id] = c
	r.containers[name] = c
	rel := r.releaseCh
	runFn := r.onRun
	r.mu.Unlock()

	ident := scruntime.CouncilIdentity{ContainerName: name, CreateAttempt: attempt, ContainerID: id}
	if s.OnCreated != nil {
		if err := s.OnCreated(ctx, ident); err != nil {
			_ = r.CleanupCouncil(context.Background(), scruntime.CouncilResult{ContainerName: name, CreateAttempt: attempt, ContainerID: id})
			return scruntime.CouncilResult{ContainerName: name, CreateAttempt: attempt, ContainerID: id}, err
		}
	}
	r.mu.Lock()
	c.running = true
	r.mu.Unlock()
	if s.OnStarted != nil {
		if err := s.OnStarted(ctx, ident); err != nil {
			_ = r.CleanupCouncil(context.Background(), scruntime.CouncilResult{ContainerName: name, CreateAttempt: attempt, ContainerID: id})
			return scruntime.CouncilResult{ContainerName: name, CreateAttempt: attempt, ContainerID: id}, err
		}
	}
	if rel != nil {
		select {
		case <-rel:
		case <-ctx.Done():
			_ = r.CleanupCouncil(context.Background(), scruntime.CouncilResult{ContainerName: name, CreateAttempt: attempt, ContainerID: id})
			return scruntime.CouncilResult{ContainerName: name, CreateAttempt: attempt, ContainerID: id}, ctx.Err()
		}
	}
	var stdout, stderr string
	var exitCode int
	var err error
	if runFn != nil {
		stdout, stderr, exitCode, err = runFn(ctx, s)
	}
	if err != nil {
		_ = r.CleanupCouncil(context.Background(), scruntime.CouncilResult{ContainerName: name, CreateAttempt: attempt, ContainerID: id})
		return scruntime.CouncilResult{ContainerName: name, CreateAttempt: attempt, ContainerID: id}, err
	}
	r.mu.Lock()
	c.running = false
	c.exited = true
	c.exitCode = exitCode
	c.finished = time.Now().UTC()
	finished := c.finished
	r.mu.Unlock()
	res := scruntime.CouncilResult{
		ContainerName: name,
		CreateAttempt: attempt,
		ContainerID:   id,
		FinishedAt:    finished,
		ExitCode:      exitCode,
		Stdout:        stdout,
		Stderr:        stderr,
	}
	if err := record(ctx, res); err != nil {
		_ = r.CleanupCouncil(context.Background(), res)
		return res, err
	}
	return res, r.CleanupCouncil(context.Background(), res)
}

func (r *cpFakeCouncilRunner) InspectCouncil(_ context.Context, id scruntime.CouncilIdentity) (scruntime.CouncilInspection, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var c *cpFakeContainer
	if id.ContainerID != "" {
		c = r.containers[id.ContainerID]
	} else {
		c = r.containers[id.ContainerName]
	}
	if c == nil || !c.exists {
		return scruntime.CouncilInspection{Exists: false}, nil
	}
	if id.ContainerID != "" && c.id != id.ContainerID {
		return scruntime.CouncilInspection{Exists: false}, nil
	}
	if id.CreateAttempt != "" && c.attempt != id.CreateAttempt {
		return scruntime.CouncilInspection{Exists: false}, nil
	}
	return scruntime.CouncilInspection{
		Exists:      true,
		Running:     c.running,
		Exited:      c.exited,
		ExitCode:    c.exitCode,
		FinishedAt:  c.finished,
		ContainerID: c.id,
	}, nil
}

func (r *cpFakeCouncilRunner) CleanupCouncil(_ context.Context, res scruntime.CouncilResult) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if c := r.containers[res.ContainerID]; c != nil {
		c.exists = false
		c.running = false
	}
	if c := r.containers[res.ContainerName]; c != nil && (res.CreateAttempt == "" || c.attempt == res.CreateAttempt) {
		c.exists = false
		c.running = false
	}
	return nil
}

func (r *cpFakeCouncilRunner) ResolveLocalImage(_ context.Context, ref string) (string, error) {
	if strings.HasPrefix(ref, "sha256:") {
		return ref, nil
	}
	return "sha256:" + strings.Repeat("b", 64), nil
}

func initCPTestRepo(t *testing.T) (string, string) {
	t.Helper()
	dir := t.TempDir()
	run := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v (%s)", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	run("init")
	run("config", "user.email", "sablecourt-test")
	run("config", "user.name", "Sablecourt Test")
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("# repo\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	run("add", "README.md")
	run("commit", "-m", "init")
	canon, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	return canon, run("rev-parse", "HEAD")
}

func newCouncilContainerFixture(t *testing.T, writerRole string, duration time.Duration) (*Reconciler, *sqlite.SQLiteStore, *config.Bundle, *policy.Engine, *coding.Adapter, *cpFakeCouncilRunner, *cpFakeProxyDocker, domain.Task, string) {
	t.Helper()
	ctx := t.Context()
	repo, commit := initCPTestRepo(t)
	st, err := sqlite.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	wl := domain.Workload{
		Metadata: domain.ObjectMeta{Name: "claude", Zone: domain.ZoneSandbox},
		Spec: domain.WorkloadSpec{
			Class:  domain.ClassCodingAgent,
			Coding: &domain.CodingConfig{Harness: "claude", RepoPath: repo, Sandbox: "seatbelt"},
		},
	}
	if err := st.UpsertWorkload(ctx, domain.WorkloadRecord{Name: "claude", Zone: domain.ZoneSandbox, Class: domain.ClassCodingAgent}); err != nil {
		t.Fatal(err)
	}
	runner := newCPFakeCouncilRunner()
	proxy := newCPFakeProxyDocker()
	wtRoot := filepath.Join(t.TempDir(), "worktrees")
	adapter := coding.NewAdapter(wtRoot)
	adapter.CouncilRunner = runner
	adapter.CouncilProxy = proxy
	adapter.CouncilProxyImage = "sha256:" + strings.Repeat("d", 64)
	adapter.DaemonListen = "127.0.0.1:7432"

	bundle := &config.Bundle{Workloads: map[string]domain.Workload{"claude": wl}}
	pol := policy.NewEngine(nil, nil)
	r := NewReconciler(Options{
		Store:    st,
		Bundle:   bundle,
		Policy:   pol,
		Adapters: map[domain.WorkloadClass]scruntime.Adapter{domain.ClassCodingAgent: adapter},
	})
	checkout := store.CouncilCheckout{Source: repo, Commit: commit, WriterRole: writerRole}
	spec := store.CouncilSpec{
		Question: "Evaluate patch.",
		Checkout: checkout,
		Duration: &duration,
		Members:  []store.CouncilMember{{Vassal: domain.ConclaveVassal{Role: "claude", Workload: "claude"}, Zone: domain.ZoneSandbox}},
	}
	if err := st.CreateCouncil(ctx, domain.ConclaveSession{ID: "ccl_1", State: domain.ConclaveStateActive, CreatedAt: time.Now().UTC()}, spec); err != nil {
		t.Fatal(err)
	}
	assign := store.ConclaveAssignment{ConclaveID: "ccl_1", Round: 1, Role: "claude", Attempt: 1}
	mode := "ref"
	if writerRole != "" {
		mode = "writer"
	}
	input, err := json.Marshal(struct {
		store.ConclaveAssignment
		Packet string `json:"context_packet"`
		Mode   string `json:"checkoutMode"`
		Ref    string `json:"checkoutRef,omitempty"`
		Writer string `json:"checkoutWriter,omitempty"`
	}{assign, "Evaluate patch.", mode, commit, writerRole})
	if err != nil {
		t.Fatal(err)
	}
	task, err := store.SubmitConclaveTurn(ctx, "ccl_1", assign.TurnKey(), store.ConclaveLimits{}, func(ctx context.Context) (domain.Task, error) {
		return r.SubmitTask(ctx, "claude", assign.TurnKey(), string(input))
	})
	if err != nil {
		t.Fatal(err)
	}
	return r, st, bundle, pol, adapter, runner, proxy, task, repo
}

func TestCouncilContainerRestartWithoutDuplicateStarts(t *testing.T) {
	r1, st, bundle, pol, adapter1, runner, proxy, task, _ := newCouncilContainerFixture(t, "", 2*time.Minute)
	runner.releaseCh = make(chan struct{})
	defer close(runner.releaseCh)

	reconcile(t, r1)
	requireState(t, st, task.ID, domain.TaskStateWorking)
	if runner.starts != 1 {
		t.Fatalf("starts = %d, want 1", runner.starts)
	}

	// Simulate daemon restart: fresh adapter & reconciler sharing worktree root + Docker engine.
	adapter2 := coding.NewAdapter(adapter1.WorktreeRoot)
	adapter2.CouncilRunner = runner
	adapter2.CouncilProxy = proxy
	adapter2.CouncilProxyImage = adapter1.CouncilProxyImage
	adapter2.DaemonListen = adapter1.DaemonListen

	r2 := NewReconciler(Options{
		Store:    st,
		Bundle:   bundle,
		Policy:   pol,
		Adapters: map[domain.WorkloadClass]scruntime.Adapter{domain.ClassCodingAgent: adapter2},
	})
	reconcile(t, r2)
	requireState(t, st, task.ID, domain.TaskStateWorking)
	if runner.starts != 1 {
		t.Fatalf("duplicate start after restart: starts = %d, want 1", runner.starts)
	}
}

func TestCouncilContainerMissingFailureRevokesLeaseAndCleansWriter(t *testing.T) {
	ctx := t.Context()
	r1, st, bundle, pol, adapter1, runner, proxy, task, _ := newCouncilContainerFixture(t, "claude", 2*time.Minute)
	runner.releaseCh = make(chan struct{})
	defer close(runner.releaseCh)

	reconcile(t, r1)
	requireState(t, st, task.ID, domain.TaskStateWorking)
	tokenHash := model.HashToken(runner.launches[0].TaskToken)
	if _, err := st.GetActiveLeaseByTokenHash(ctx, tokenHash, time.Now().UTC()); err != nil {
		t.Fatalf("expected active lease: %v", err)
	}

	// Simulate container disappearing externally while daemon is down.
	runner.mu.Lock()
	for _, c := range runner.containers {
		c.exists = false
		c.running = false
	}
	runner.mu.Unlock()

	adapter2 := coding.NewAdapter(adapter1.WorktreeRoot)
	adapter2.CouncilRunner = runner
	adapter2.CouncilProxy = proxy
	adapter2.CouncilProxyImage = adapter1.CouncilProxyImage
	adapter2.DaemonListen = adapter1.DaemonListen

	r2 := NewReconciler(Options{
		Store:    st,
		Bundle:   bundle,
		Policy:   pol,
		Adapters: map[domain.WorkloadClass]scruntime.Adapter{domain.ClassCodingAgent: adapter2},
	})
	reconcile(t, r2)

	requireState(t, st, task.ID, domain.TaskStateFailed)
	if _, err := st.GetActiveLeaseByTokenHash(ctx, tokenHash, time.Now().UTC()); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("lease survived missing container: %v", err)
	}
	proxy.mu.Lock()
	remProxies, remNets := len(proxy.proxies), len(proxy.networks)
	proxy.mu.Unlock()
	if remProxies != 0 || remNets != 0 {
		t.Fatalf("proxy or network leaked after restart recovery: proxies=%d nets=%d", remProxies, remNets)
	}
	// Writer lock and active task must be released so CleanupCouncil succeeds.
	if err := r2.CleanupCouncil(ctx, "ccl_1"); err != nil {
		t.Fatalf("CleanupCouncil after missing writer container: %v", err)
	}
}

func TestCouncilContainerDeadlineCancellationAndWriterCleanup(t *testing.T) {
	ctx := t.Context()
	r, st, _, _, _, runner, proxy, task, _ := newCouncilContainerFixture(t, "claude", 2*time.Second)
	runner.releaseCh = make(chan struct{})
	defer close(runner.releaseCh)

	reconcile(t, r)
	requireState(t, st, task.ID, domain.TaskStateWorking)
	tokenHash := model.HashToken(runner.launches[0].TaskToken)

	// Wait past the council deadline so container cancels itself.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		reconcile(t, r)
		cur, err := st.GetTask(ctx, task.ID)
		if err != nil {
			t.Fatal(err)
		}
		if cur.State == domain.TaskStateFailed {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	requireState(t, st, task.ID, domain.TaskStateFailed)
	if _, err := st.GetActiveLeaseByTokenHash(ctx, tokenHash, time.Now().UTC()); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("lease survived deadline cancellation: %v", err)
	}
	proxy.mu.Lock()
	remProxies, remNets := len(proxy.proxies), len(proxy.networks)
	proxy.mu.Unlock()
	if remProxies != 0 || remNets != 0 {
		t.Fatalf("proxy/network leaked after deadline: proxies=%d nets=%d", remProxies, remNets)
	}
	if err := r.CleanupCouncil(ctx, "ccl_1"); err != nil {
		t.Fatalf("CleanupCouncil after deadline: %v", err)
	}
}

func TestCouncilContainerWriterFinalMessageAndDiffRelic(t *testing.T) {
	ctx := t.Context()
	r, st, _, _, _, runner, proxy, task, _ := newCouncilContainerFixture(t, "claude", 2*time.Minute)
	runner.onRun = func(_ context.Context, s scruntime.CouncilLaunch) (string, string, int, error) {
		if err := os.WriteFile(filepath.Join(s.Checkout.Path, "patch.txt"), []byte("container edit\n"), 0o600); err != nil {
			return "", "", 1, err
		}
		return "Summary: wrote patch.txt", "trace info", 0, nil
	}

	reconcile(t, r)
	// Poll until Observe collects artifacts and finishes task.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		reconcile(t, r)
		cur, err := st.GetTask(ctx, task.ID)
		if err != nil {
			t.Fatal(err)
		}
		if cur.State == domain.TaskStateCompleted {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	cur, err := st.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if cur.State != domain.TaskStateCompleted {
		t.Fatalf("task state = %s, want completed", cur.State)
	}
	if !strings.Contains(cur.ArtifactsJSON, "Summary: wrote patch.txt") || !strings.Contains(cur.ArtifactsJSON, `"final-message"`) {
		t.Fatalf("artifacts missing final-message: %s", cur.ArtifactsJSON)
	}
	snap, err := st.CouncilSnapshot(ctx, "ccl_1")
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Attempts) != 1 || len(snap.Attempts[0].Relics) != 1 || !strings.Contains(snap.Attempts[0].Relics[0].Body, "container edit") {
		t.Fatalf("expected writer diff Relic, got %+v", snap.Attempts)
	}
	// Lease must be revoked after completion.
	tokenHash := model.HashToken(runner.launches[0].TaskToken)
	if _, err := st.GetActiveLeaseByTokenHash(ctx, tokenHash, time.Now().UTC()); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("lease survived completion: %v", err)
	}
	proxy.mu.Lock()
	remProxies, remNets := len(proxy.proxies), len(proxy.networks)
	proxy.mu.Unlock()
	if remProxies != 0 || remNets != 0 {
		t.Fatalf("proxy/network leaked: proxies=%d nets=%d", remProxies, remNets)
	}
	if err := r.CleanupCouncil(ctx, "ccl_1"); err != nil {
		t.Fatalf("CleanupCouncil: %v", err)
	}
}

func TestCouncilContainerCrashWindowAroundStart(t *testing.T) {
	for _, tc := range []struct {
		name      string
		running   bool
		created   bool
		wantState domain.TaskState
	}{
		{"crashed_after_start_reattaches_without_duplicate", true, true, domain.TaskStateWorking},
		{"crashed_after_create_before_start_cleans_up", false, true, domain.TaskStateFailed},
		{"crashed_before_create_fails_and_revokes_lease", false, false, domain.TaskStateFailed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			r1, st, bundle, pol, adapter1, runner, proxy, task, _ := newCouncilContainerFixture(t, "", 2*time.Minute)

			// Simulate crash right after launchTask claimed the task, minted a lease, and persisted pre-start handle.
			if err := store.ClaimConclaveTask(ctx, st, task.ID, "ccl_1", r1.now); err != nil {
				t.Fatal(err)
			}
			tok, err := model.MintTaskToken(ctx, st, task.ID, "claude", time.Hour)
			if err != nil {
				t.Fatal(err)
			}
			tokenHash := model.HashToken(tok)
			attempt := "crash-attempt-1"
			cname := "sablecourt-council-" + task.ID
			pre := scruntime.Handle{
				AdapterName:   "coding:claude",
				ExternalID:    task.ID,
				ContainerName: cname,
				CreateAttempt: attempt,
				ProxyName:     "sablecourt-council-proxy-crash",
				NetworkName:   "sablecourt-council-proxy-crash-net",
			}
			if err := r1.saveHandle(ctx, task.ID, domain.ClassCodingAgent, pre); err != nil {
				t.Fatal(err)
			}
			proxy.mu.Lock()
			proxy.proxies[pre.ProxyName] = councilproxy.ProxyContainer{Name: pre.ProxyName}
			proxy.networks[pre.NetworkName] = true
			proxy.mu.Unlock()
			if tc.created {
				runner.mu.Lock()
				c := &cpFakeContainer{
					name:    cname,
					attempt: attempt,
					id:      "cid-crash-1",
					running: tc.running,
					exists:  true,
				}
				runner.containers[c.id] = c
				runner.containers[cname] = c
				runner.mu.Unlock()
			}

			adapter2 := coding.NewAdapter(adapter1.WorktreeRoot)
			adapter2.CouncilRunner = runner
			adapter2.CouncilProxy = proxy
			adapter2.CouncilProxyImage = adapter1.CouncilProxyImage
			adapter2.DaemonListen = adapter1.DaemonListen

			r2 := NewReconciler(Options{
				Store:    st,
				Bundle:   bundle,
				Policy:   pol,
				Adapters: map[domain.WorkloadClass]scruntime.Adapter{domain.ClassCodingAgent: adapter2},
			})
			reconcile(t, r2)

			requireState(t, st, task.ID, tc.wantState)
			if runner.starts != 0 {
				t.Fatalf("unexpected duplicate start during recovery: starts=%d", runner.starts)
			}
			if tc.wantState == domain.TaskStateFailed {
				if _, err := st.GetActiveLeaseByTokenHash(ctx, tokenHash, time.Now().UTC()); !errors.Is(err, store.ErrNotFound) {
					t.Fatalf("lease survived crash recovery failure: %v", err)
				}
				runner.mu.Lock()
				stillExists := runner.containers[cname] != nil && runner.containers[cname].exists
				runner.mu.Unlock()
				if stillExists {
					t.Fatal("unstarted container leaked after crash recovery")
				}
				proxy.mu.Lock()
				remProxies, remNets := len(proxy.proxies), len(proxy.networks)
				proxy.mu.Unlock()
				if remProxies != 0 || remNets != 0 {
					t.Fatalf("proxy or network leaked after crash recovery: proxies=%d nets=%d", remProxies, remNets)
				}
			}
		})
	}
}

func TestCouncilContainerMissingDuringObserveAndCancelRevokesLease(t *testing.T) {
	ctx := t.Context()
	r, st, _, _, _, runner, proxy, task, _ := newCouncilContainerFixture(t, "claude", 2*time.Minute)
	runner.releaseCh = make(chan struct{})
	defer close(runner.releaseCh)

	reconcile(t, r)
	requireState(t, st, task.ID, domain.TaskStateWorking)
	tokenHash := model.HashToken(runner.launches[0].TaskToken)

	// Operator cancellation must stop container, revoke lease, remove proxy/network, and release writer lock.
	if err := r.CancelTask(ctx, task.ID, "deadline"); err != nil {
		t.Fatalf("CancelTask: %v", err)
	}
	requireState(t, st, task.ID, domain.TaskStateCancelled)
	if _, err := st.GetActiveLeaseByTokenHash(ctx, tokenHash, time.Now().UTC()); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("lease survived cancel: %v", err)
	}
	proxy.mu.Lock()
	remProxies, remNets := len(proxy.proxies), len(proxy.networks)
	proxy.mu.Unlock()
	if remProxies != 0 || remNets != 0 {
		t.Fatalf("proxy/network leaked after cancel: proxies=%d nets=%d", remProxies, remNets)
	}
	if err := r.CleanupCouncil(ctx, "ccl_1"); err != nil {
		t.Fatalf("CleanupCouncil after cancel: %v", err)
	}
}
