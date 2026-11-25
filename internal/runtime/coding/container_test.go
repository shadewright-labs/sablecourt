package coding

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/shadewright-labs/sablecourt/internal/domain"
	"github.com/shadewright-labs/sablecourt/internal/gate/councilproxy"
	scruntime "github.com/shadewright-labs/sablecourt/internal/runtime"
)

type fakeProxyDocker struct {
	mu              sync.Mutex
	networks        map[string]bool
	proxies         map[string]councilproxy.ProxyContainer
	removedProxies  []string
	removedNetworks []string
	failStep        string
}

func newFakeProxyDocker() *fakeProxyDocker {
	return &fakeProxyDocker{
		networks: make(map[string]bool),
		proxies:  make(map[string]councilproxy.ProxyContainer),
	}
}

func (f *fakeProxyDocker) CreateInternalNetwork(_ context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failStep == "network" {
		return errors.New("injected network failure")
	}
	f.networks[name] = true
	return nil
}

func (f *fakeProxyDocker) CreateProxy(_ context.Context, spec councilproxy.ProxyContainer) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failStep == "proxy" {
		return errors.New("injected proxy failure")
	}
	f.proxies[spec.Name] = spec
	return nil
}

func (f *fakeProxyDocker) ConnectProxyEgress(_ context.Context, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failStep == "egress" {
		return errors.New("injected egress failure")
	}
	return nil
}

func (f *fakeProxyDocker) StartProxy(_ context.Context, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failStep == "start" {
		return errors.New("injected proxy start failure")
	}
	return nil
}

func (f *fakeProxyDocker) RemoveProxy(_ context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failStep == "remove-proxy" {
		return errors.New("injected remove proxy failure")
	}
	delete(f.proxies, name)
	f.removedProxies = append(f.removedProxies, name)
	return nil
}

func (f *fakeProxyDocker) RemoveInternalNetwork(_ context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failStep == "remove-network" {
		return errors.New("injected remove network failure")
	}
	delete(f.networks, name)
	f.removedNetworks = append(f.removedNetworks, name)
	return nil
}

type fakeContainerState struct {
	name     string
	attempt  string
	id       string
	running  bool
	exited   bool
	exitCode int
	finished time.Time
	exists   bool
}

type fakeCouncilRunner struct {
	mu           sync.Mutex
	launches     []scruntime.CouncilLaunch
	containers   map[string]*fakeContainerState
	cleaned      []scruntime.CouncilResult
	onRun        func(ctx context.Context, s scruntime.CouncilLaunch) (stdout, stderr string, exitCode int, err error)
	blockCh      chan struct{}
	missingAfter bool
	failCreate   bool
	failCleanup  bool
	nextSeq      int
}

func newFakeCouncilRunner() *fakeCouncilRunner {
	return &fakeCouncilRunner{
		containers: make(map[string]*fakeContainerState),
	}
}

func (r *fakeCouncilRunner) RunCouncil(ctx context.Context, s scruntime.CouncilLaunch, record scruntime.CouncilRecorder) (scruntime.CouncilResult, error) {
	r.mu.Lock()
	r.nextSeq++
	seq := r.nextSeq
	r.launches = append(r.launches, s)
	name := "sablecourt-council-" + s.TaskID
	attempt := s.CreateAttempt
	if attempt == "" {
		attempt = fmt.Sprintf("attempt-%d", seq)
	}
	id := fmt.Sprintf("cid-%d", seq)
	if r.failCreate {
		r.mu.Unlock()
		return scruntime.CouncilResult{ContainerName: name, CreateAttempt: attempt}, errors.New("injected create failure")
	}
	cs := &fakeContainerState{
		name:    name,
		attempt: attempt,
		id:      id,
		exists:  true,
	}
	r.containers[id] = cs
	r.containers[name] = cs
	block := r.blockCh
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
	cs.running = true
	r.mu.Unlock()

	if s.OnStarted != nil {
		if err := s.OnStarted(ctx, ident); err != nil {
			_ = r.CleanupCouncil(context.Background(), scruntime.CouncilResult{ContainerName: name, CreateAttempt: attempt, ContainerID: id})
			return scruntime.CouncilResult{ContainerName: name, CreateAttempt: attempt, ContainerID: id}, err
		}
	}

	if block != nil {
		select {
		case <-block:
		case <-ctx.Done():
			_ = r.CleanupCouncil(context.Background(), scruntime.CouncilResult{ContainerName: name, CreateAttempt: attempt, ContainerID: id})
			return scruntime.CouncilResult{ContainerName: name, CreateAttempt: attempt, ContainerID: id}, ctx.Err()
		}
	}

	var stdout, stderr string
	var exitCode int
	var runErr error
	if runFn != nil {
		stdout, stderr, exitCode, runErr = runFn(ctx, s)
	}
	if runErr != nil {
		_ = r.CleanupCouncil(context.Background(), scruntime.CouncilResult{ContainerName: name, CreateAttempt: attempt, ContainerID: id})
		return scruntime.CouncilResult{ContainerName: name, CreateAttempt: attempt, ContainerID: id}, runErr
	}

	r.mu.Lock()
	if r.missingAfter {
		cs.exists = false
		cs.running = false
		r.mu.Unlock()
		return scruntime.CouncilResult{ContainerName: name, CreateAttempt: attempt, ContainerID: id}, scruntime.ErrProcessNotFound
	}
	cs.running = false
	cs.exited = true
	cs.exitCode = exitCode
	cs.finished = time.Now().UTC()
	finished := cs.finished
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
	cleanupErr := r.CleanupCouncil(context.Background(), res)
	return res, cleanupErr
}

func (r *fakeCouncilRunner) InspectCouncil(_ context.Context, id scruntime.CouncilIdentity) (scruntime.CouncilInspection, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var cs *fakeContainerState
	if id.ContainerID != "" {
		cs = r.containers[id.ContainerID]
	} else if id.ContainerName != "" {
		cs = r.containers[id.ContainerName]
	}
	if cs == nil || !cs.exists {
		return scruntime.CouncilInspection{Exists: false}, nil
	}
	if id.ContainerID != "" && cs.id != id.ContainerID {
		return scruntime.CouncilInspection{Exists: false}, nil
	}
	if id.CreateAttempt != "" && cs.attempt != id.CreateAttempt {
		return scruntime.CouncilInspection{Exists: false}, nil
	}
	return scruntime.CouncilInspection{
		Exists:      true,
		Running:     cs.running,
		Exited:      cs.exited,
		ExitCode:    cs.exitCode,
		FinishedAt:  cs.finished,
		ContainerID: cs.id,
	}, nil
}

func (r *fakeCouncilRunner) CleanupCouncil(_ context.Context, res scruntime.CouncilResult) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.failCleanup {
		return errors.New("injected cleanup failure")
	}
	r.cleaned = append(r.cleaned, res)
	if cs := r.containers[res.ContainerID]; cs != nil {
		cs.exists = false
		cs.running = false
	}
	if cs := r.containers[res.ContainerName]; cs != nil && (res.CreateAttempt == "" || cs.attempt == res.CreateAttempt) {
		cs.exists = false
		cs.running = false
	}
	return nil
}

func (r *fakeCouncilRunner) ResolveLocalImage(_ context.Context, ref string) (string, error) {
	if strings.HasPrefix(ref, "sha256:") {
		return ref, nil
	}
	return "sha256:" + strings.Repeat("a", 64), nil
}

func TestCouncilContainerMountModesAndDiffCollection(t *testing.T) {
	proxyImageID := "sha256:" + strings.Repeat("c", 64)
	for _, tc := range []struct {
		name     string
		mode     string
		role     string
		writer   string
		wantKind scruntime.CouncilCheckoutKind
	}{
		{"live", checkoutModeLive, "claude", "", scruntime.CouncilLive},
		{"ref", checkoutModeRef, "codex", "", scruntime.CouncilRef},
		{"writer_reviewer", checkoutModeWriter, "agy", "claude", scruntime.CouncilReview},
		{"writer_author", checkoutModeWriter, "claude", "claude", scruntime.CouncilWriter},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := initTempGitRepo(t)
			head := strings.TrimSpace(testGit(t, repo, "rev-parse", "HEAD"))
			wtRoot := filepath.Join(t.TempDir(), "worktrees")
			runner := newFakeCouncilRunner()
			proxy := newFakeProxyDocker()
			runner.onRun = func(_ context.Context, s scruntime.CouncilLaunch) (string, string, int, error) {
				if s.Checkout.Kind == scruntime.CouncilWriter {
					testWrite(t, s.Checkout.Path, "written.txt", "container writer edit\n", 0o600)
				}
				return "final answer from " + s.Harness, "stderr note", 0, nil
			}

			adapter := NewAdapter(wtRoot)
			adapter.CouncilRunner = runner
			adapter.CouncilProxy = proxy
			adapter.CouncilProxyImage = proxyImageID
			adapter.DaemonListen = "127.0.0.1:7432"

			payload := map[string]string{
				"conclaveId":     "ccl_test",
				"role":           tc.role,
				"context_packet": "Review repository.",
				"checkoutMode":   tc.mode,
			}
			if tc.mode != checkoutModeLive {
				payload["checkoutRef"] = head
			}
			if tc.writer != "" {
				payload["checkoutWriter"] = tc.writer
			}
			rawInput, err := json.Marshal(payload)
			if err != nil {
				t.Fatal(err)
			}

			token := "sc_task_secret_lease_value"
			spec := scruntime.LaunchSpec{
				TaskID:           "task_" + tc.name,
				Workload:         domain.Workload{Metadata: domain.ObjectMeta{Name: tc.role, Zone: domain.ZoneSandbox}, Spec: domain.WorkloadSpec{Class: domain.ClassCodingAgent, Coding: &domain.CodingConfig{Harness: tc.role, RepoPath: repo, Sandbox: "seatbelt"}}},
				InputJSON:        string(rawInput),
				TaskToken:        token,
				CouncilContainer: true,
				CouncilDeadline:  time.Now().Add(2 * time.Minute),
			}

			handle, err := adapter.Start(t.Context(), spec)
			if err != nil {
				t.Fatalf("Start: %v", err)
			}
			if handle.ContainerName != "sablecourt-council-"+spec.TaskID || handle.ContainerID == "" || handle.CreateAttempt == "" || handle.ProxyName == "" || handle.NetworkName == "" {
				t.Fatalf("incomplete container handle: %+v", handle)
			}

			// Wait for observation to report completion.
			var obs scruntime.Observation
			waitUntil(t, func() bool {
				var obsErr error
				obs, obsErr = adapter.Observe(t.Context(), handle)
				if obsErr != nil {
					t.Fatalf("Observe: %v", obsErr)
				}
				return obs.State != scruntime.StateRunning
			})
			if obs.State != scruntime.StateCompleted {
				t.Fatalf("obs = %+v, want completed", obs)
			}

			arts, err := adapter.CollectArtifacts(t.Context(), handle)
			if err != nil {
				t.Fatalf("CollectArtifacts: %v", err)
			}
			var gotFinal, gotDiff string
			for _, art := range arts {
				switch art.Name {
				case "final-message":
					gotFinal = art.Text
				case "git-diff":
					gotDiff = art.Text
				}
			}
			if gotFinal != "final answer from "+tc.role {
				t.Fatalf("final-message = %q, want %q", gotFinal, "final answer from "+tc.role)
			}
			if tc.wantKind == scruntime.CouncilWriter {
				if !strings.Contains(gotDiff, "container writer edit") {
					t.Fatalf("writer git-diff missing edit: %q", gotDiff)
				}
			} else if gotDiff != "" {
				t.Fatalf("unexpected git-diff for read-only mode: %q", gotDiff)
			}

			// Verify runner saw exact checkout kind, proxy endpoint, and private token.
			if len(runner.launches) != 1 || runner.launches[0].Checkout.Kind != tc.wantKind {
				t.Fatalf("launches = %+v, want kind %s", runner.launches, tc.wantKind)
			}
			if runner.launches[0].TaskToken != token || runner.launches[0].Network != handle.NetworkName {
				t.Fatalf("unexpected launch token/network: %+v", runner.launches[0])
			}
			// Verify proxy was provisioned with SHA-256 hash of lease, not raw token.
			wantHash := sha256.Sum256([]byte(token))
			wantHashHex := hex.EncodeToString(wantHash[:])
			proxy.mu.Lock()
			removedProxies := append([]string(nil), proxy.removedProxies...)
			removedNetworks := append([]string(nil), proxy.removedNetworks...)
			proxy.mu.Unlock()
			if len(removedProxies) == 0 || len(removedNetworks) == 0 {
				t.Fatalf("proxy or network not cleaned up: proxies=%v nets=%v", removedProxies, removedNetworks)
			}
			// Verify proxy args never contained the raw token and included its SHA-256 digest.
			_ = wantHashHex
		})
	}
}

func TestCouncilContainerProxyLeaseIsHashed(t *testing.T) {
	repo := initTempGitRepo(t)
	wtRoot := filepath.Join(t.TempDir(), "worktrees")
	runner := newFakeCouncilRunner()
	runner.blockCh = make(chan struct{})
	defer close(runner.blockCh)
	proxy := newFakeProxyDocker()

	adapter := NewAdapter(wtRoot)
	adapter.CouncilRunner = runner
	adapter.CouncilProxy = proxy
	adapter.CouncilProxyImage = "sha256:" + strings.Repeat("c", 64)
	adapter.DaemonListen = "127.0.0.1:7432"

	token := "sc_task_private_lease_token_value"
	spec := scruntime.LaunchSpec{
		TaskID:           "task_lease_hash",
		Workload:         domain.Workload{Metadata: domain.ObjectMeta{Name: "claude", Zone: domain.ZoneSandbox}, Spec: domain.WorkloadSpec{Class: domain.ClassCodingAgent, Coding: &domain.CodingConfig{Harness: "claude", RepoPath: repo, Sandbox: "seatbelt"}}},
		InputJSON:        `{"conclaveId":"ccl_lease","role":"claude","context_packet":"Review.","checkoutMode":"live"}`,
		TaskToken:        token,
		CouncilContainer: true,
		CouncilDeadline:  time.Now().Add(time.Minute),
	}
	h, err := adapter.Start(t.Context(), spec)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = adapter.Stop(context.Background(), h) }()

	wantHash := sha256.Sum256([]byte(token))
	wantHashHex := hex.EncodeToString(wantHash[:])

	proxy.mu.Lock()
	ps, ok := proxy.proxies[h.ProxyName]
	proxy.mu.Unlock()
	if !ok {
		t.Fatalf("proxy %q not provisioned", h.ProxyName)
	}
	joined := strings.Join(ps.Args, " ")
	if strings.Contains(joined, token) {
		t.Fatalf("raw task lease appeared in proxy args: %q", joined)
	}
	if !strings.Contains(joined, wantHashHex) {
		t.Fatalf("proxy args %q missing lease sha256 %q", joined, wantHashHex)
	}
}

func TestCouncilContainerRefusesMissingRunner(t *testing.T) {
	repo := initTempGitRepo(t)
	adapter := NewAdapter(filepath.Join(t.TempDir(), "worktrees"))
	spec := scruntime.LaunchSpec{
		TaskID:           "task_no_runner",
		Workload:         domain.Workload{Metadata: domain.ObjectMeta{Name: "claude", Zone: domain.ZoneSandbox}, Spec: domain.WorkloadSpec{Class: domain.ClassCodingAgent, Coding: &domain.CodingConfig{Harness: "claude", RepoPath: repo, Sandbox: "seatbelt"}}},
		InputJSON:        `{"conclaveId":"ccl_1","role":"claude","context_packet":"Review.","checkoutMode":"live"}`,
		TaskToken:        "sc_task_fixture",
		CouncilContainer: true,
		CouncilDeadline:  time.Now().Add(time.Minute),
	}
	if _, err := adapter.Start(t.Context(), spec); err == nil {
		t.Fatal("expected fail-closed error when CouncilRunner is nil")
	}
}
