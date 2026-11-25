package coding

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
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
	failProvision   bool
	fault           string
}

func newFakeProxyDocker() *fakeProxyDocker {
	return &fakeProxyDocker{
		networks: make(map[string]bool),
		proxies:  make(map[string]councilproxy.ProxyContainer),
	}
}

type fixtureProxySession struct {
	d    *fakeProxyDocker
	name string
}

func (s *fixtureProxySession) Workload() councilproxy.WorkloadEndpoint {
	if s.d.fault == "missing endpoint" {
		return councilproxy.WorkloadEndpoint{}
	}
	u := "http://" + s.name + ":" + councilproxy.Port
	mode := councilproxy.GateModeUnixSocket
	if s.d.fault == "no gate" {
		mode = councilproxy.GateModeNone
	}
	return councilproxy.WorkloadEndpoint{Network: s.name + "-net", ProxyURL: u, DataGateURL: u + "/gate/mcp", GateMode: mode}
}
func (s *fixtureProxySession) Close(ctx context.Context) error {
	if err := s.d.RemoveProxy(ctx, s.name); err != nil {
		return err
	}
	return s.d.RemoveInternalNetwork(ctx, s.name+"-net")
}
func (f *fakeProxyDocker) ProvisionCouncilProxy(ctx context.Context, image string, cfg councilproxy.Config) (councilproxy.LaunchSession, error) {
	name := "sablecourt-council-" + cfg.LeaseSHA256[:16]
	if err := cfg.RecordResources(name, name+"-net"); err != nil {
		return nil, err
	}
	f.mu.Lock()
	f.networks[name+"-net"] = true
	f.proxies[name] = councilproxy.ProxyContainer{Name: name, ImageID: image, Args: []string{"-lease-sha256", cfg.LeaseSHA256}}
	f.mu.Unlock()
	if f.fault == "missing session" {
		return nil, nil
	}
	s := &fixtureProxySession{d: f, name: name}
	if f.failProvision {
		if err := s.Close(ctx); err != nil {
			return s, err
		}
		return nil, errors.New("provision failed")
	}
	return s, nil
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
			adapter.DaemonListen = net.JoinHostPort(net.IPv6loopback.String(), "7432")

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

			taskBase, err := adapter.taskBase(spec.TaskID)
			if err != nil {
				t.Fatal(err)
			}
			<-adapter.containerProc(taskBase).done
			obs, err := adapter.Observe(t.Context(), handle)
			if err != nil {
				t.Fatal(err)
			}
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
			proxy.mu.Lock()
			removedProxies := append([]string(nil), proxy.removedProxies...)
			removedNetworks := append([]string(nil), proxy.removedNetworks...)
			proxy.mu.Unlock()
			if len(removedProxies) == 0 || len(removedNetworks) == 0 {
				t.Fatalf("proxy or network not cleaned up: proxies=%v nets=%v", removedProxies, removedNetworks)
			}
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
	adapter.DaemonListen = net.JoinHostPort(net.IPv6loopback.String(), "7432")

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

func TestFailedProvisionNeverStartsHarness(t *testing.T) {
	for _, cleanupFails := range []bool{false, true} {
		t.Run(fmt.Sprint(cleanupFails), func(t *testing.T) {
			repo := initTempGitRepo(t)
			a := NewAdapter(filepath.Join(t.TempDir(), "worktrees"))
			proxy := newFakeProxyDocker()
			proxy.failProvision = true
			if cleanupFails {
				proxy.failStep = "remove-proxy"
			}
			runner := newFakeCouncilRunner()
			a.CouncilProxy = proxy
			a.CouncilRunner = runner
			a.CouncilProxyImage = "sha256:" + strings.Repeat("a", 64)
			a.DaemonListen = net.JoinHostPort(net.IPv6loopback.String(), "7432")
			spec := scruntime.LaunchSpec{TaskID: "failed-provision", Workload: domain.Workload{Metadata: domain.ObjectMeta{Name: "claude"}, Spec: domain.WorkloadSpec{Coding: &domain.CodingConfig{Harness: "claude", RepoPath: repo}}}, InputJSON: `{"checkoutMode":"live","role":"claude","conclaveId":"ccl_test","context_packet":"Review."}`, TaskToken: "sc_task_fixture", CouncilContainer: true}
			h, err := a.Start(t.Context(), spec)
			if err == nil || len(runner.launches) != 0 {
				t.Fatalf("started after failed provision: %v", err)
			}
			if cleanupFails {
				if h.ProxyName == "" {
					t.Fatal("lost partial start handle")
				}
				proxy.failStep = ""
				if err := a.Stop(t.Context(), h); err != nil {
					t.Fatal(err)
				}
			}
			if len(proxy.proxies) != 0 || len(proxy.networks) != 0 {
				t.Fatal("failed provision leaked resources")
			}
		})
	}
}

func TestStopCannotCancelAnotherAttempt(t *testing.T) {
	a := NewAdapter(t.TempDir())
	taskBase, err := a.taskBase("turn")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	p := &containerProcess{identity: containerIdentity{ContainerName: "turn", CreateAttempt: "second"}, cancel: cancel, done: make(chan struct{})}
	close(p.done)
	a.containers.Store(taskBase, p)
	err = a.Stop(t.Context(), scruntime.Handle{ExternalID: "turn", ContainerName: "turn", CreateAttempt: "first"})
	if err == nil || ctx.Err() != nil {
		t.Fatal("stale stop cancelled a different attempt")
	}
}

func TestRecoveredStopBeforeProxyJournal(t *testing.T) {
	a := NewAdapter(filepath.Join(t.TempDir(), "worktrees"))
	a.CouncilRunner = newFakeCouncilRunner()
	repo := initTempGitRepo(t)
	task := "before-proxy"
	_, base, _, _, err := a.prepareLiveCheckout(t.Context(), repo, task, "")
	if err != nil {
		t.Fatal(err)
	}
	h := scruntime.Handle{ExternalID: task, AdapterName: "coding:claude", ContainerName: "sablecourt-council-" + task, CreateAttempt: "attempt"}
	if err = a.Stop(t.Context(), h); err != nil {
		t.Fatal(err)
	}
	state, err := readWorktreeState(base + stateSuffix)
	if err != nil || state.FinishedAt.IsZero() {
		t.Fatal("restart did not release the checkout")
	}
}

func TestMissingProxyEvidenceRefusesLaunch(t *testing.T) {
	for _, fault := range []string{"missing session", "missing endpoint", "no gate"} {
		t.Run(fault, func(t *testing.T) {
			repo := initTempGitRepo(t)
			a := NewAdapter(filepath.Join(t.TempDir(), "worktrees"))
			proxy := newFakeProxyDocker()
			proxy.fault = fault
			runner := newFakeCouncilRunner()
			a.CouncilRunner = runner
			a.CouncilProxy = proxy
			a.CouncilProxyImage = "sha256:" + strings.Repeat("a", 64)
			a.DaemonListen = net.JoinHostPort(net.IPv6loopback.String(), "7432")
			spec := scruntime.LaunchSpec{TaskID: "missing-evidence", TaskToken: "sc_task_fixture", CouncilContainer: true,
				Workload:  domain.Workload{Metadata: domain.ObjectMeta{Name: "claude"}, Spec: domain.WorkloadSpec{Coding: &domain.CodingConfig{RepoPath: repo, Harness: "claude"}}},
				InputJSON: `{"conclaveId":"ccl_test","role":"claude","context_packet":"Review.","checkoutMode":"live"}`}
			h, err := a.Start(t.Context(), spec)
			if err == nil || len(runner.launches) != 0 {
				t.Fatal("harness started without proxy evidence")
			}
			if isContainerHandle(h) {
				if err := a.Stop(t.Context(), h); err != nil {
					t.Fatal(err)
				}
			}
			if len(proxy.networks) != 0 || len(proxy.proxies) != 0 {
				t.Fatal("proxy resources survived failed launch")
			}
		})
	}
}

func TestCleanupRetriesLiveSession(t *testing.T) {
	for _, fault := range []string{"remove-proxy", "remove-network"} {
		t.Run(fault, func(t *testing.T) {
			proxy := newFakeProxyDocker()
			proxy.failStep = fault
			session := &fixtureProxySession{d: proxy, name: "proxy"}
			proxy.proxies["proxy"] = councilproxy.ProxyContainer{Name: "proxy"}
			proxy.networks["proxy-net"] = true
			a := NewAdapter(t.TempDir())
			a.CouncilProxy = proxy
			a.CouncilRunner = newFakeCouncilRunner()
			id := &containerIdentity{ContainerName: "turn", CreateAttempt: "attempt", ProxyName: "proxy", NetworkName: "proxy-net"}
			if err := a.cleanupContainerResources(t.Context(), id, session); err == nil {
				t.Fatal("lost cleanup failure")
			}
			if len(proxy.networks) != 1 {
				t.Fatal("network removed ahead of the proxy")
			}
			proxy.failStep = ""
			if err := a.cleanupContainerResources(t.Context(), id, session); err != nil {
				t.Fatal(err)
			}
			if len(proxy.networks) != 0 || len(proxy.proxies) != 0 {
				t.Fatal("retry left Docker resources")
			}
		})
	}
}

func TestCouncilHandleRequiresMatchingEvidence(t *testing.T) {
	c := &containerIdentity{ContainerName: "turn", CreateAttempt: "first", ContainerID: "cid"}
	for _, tc := range []struct {
		name   string
		state  *containerIdentity
		handle scruntime.Handle
		match  bool
	}{
		{"immutable ID", c, scruntime.Handle{ContainerID: "cid"}, true},
		{"pending start", c, scruntime.Handle{ContainerName: "turn", CreateAttempt: "first"}, true},
		{"name only", c, scruntime.Handle{ContainerName: "turn"}, false},
		{"wrong attempt", c, scruntime.Handle{ContainerID: "cid", CreateAttempt: "second"}, false},
		{"unrecorded ID", &containerIdentity{ContainerName: "turn", CreateAttempt: "first"}, scruntime.Handle{ContainerID: "foreign"}, false},
		{"mismatched ID", c, scruntime.Handle{ContainerName: "turn", CreateAttempt: "first", ContainerID: "foreign"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if matchesContainerHandle(tc.state, tc.handle) != tc.match {
				t.Fatal("wrong identity decision")
			}
		})
	}
}
