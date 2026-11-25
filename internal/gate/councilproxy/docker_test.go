package councilproxy

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeDocker struct {
	fail, cleanupFail string
	socketReachable   bool
	netInfo           *InternalNetworkInfo
	calls             []string
	network, proxy    string
	netSpec           InternalNetworkSpec
	spec              ProxyContainer
}

func (f *fakeDocker) step(step string) error {
	f.calls = append(f.calls, step)
	if step == f.fail || step == f.cleanupFail {
		return errors.New("injected " + step)
	}
	return nil
}

func (f *fakeDocker) ProbeGateSocket(_ context.Context, spec GateSocketProbe) error {
	if err := f.step("probe"); err != nil {
		return err
	}
	if !f.socketReachable {
		return errors.New("container cannot reach host unix socket")
	}
	var token string
	for i := 0; i+1 < len(spec.Args); i++ {
		if spec.Args[i] == "-probe-token" {
			token = spec.Args[i+1]
		}
	}
	conn, err := net.DialTimeout("unix", spec.HostSocketPath, time.Second)
	if err != nil {
		return err
	}
	defer conn.Close()
	_, err = io.WriteString(conn, token+"\n")
	return err
}

func (f *fakeDocker) CreateInternalNetwork(_ context.Context, spec InternalNetworkSpec) (InternalNetworkInfo, error) {
	f.network = spec.Name
	f.netSpec = spec
	if err := f.step("network"); err != nil {
		return InternalNetworkInfo{}, err
	}
	if f.netInfo != nil {
		return *f.netInfo, nil
	}
	return InternalNetworkInfo{
		EngineVersion: "29.9.3",
		Driver:        "bridge",
		Internal:      true,
		Options: map[string]string{
			BridgeGatewayModeIPv4: GatewayModeIsolated,
			BridgeGatewayModeIPv6: GatewayModeIsolated,
		},
		Subnets: []string{netip.PrefixFrom(netip.AddrFrom4([4]byte{172, 20, 0, 0}), 16).String()},
	}, nil
}

func (f *fakeDocker) CreateProxy(_ context.Context, spec ProxyContainer) error {
	f.proxy = spec.Name
	f.spec = spec
	if spec.InternalNetwork != f.network || spec.ImageID != imageID || spec.User != "10001:10001" ||
		!spec.ReadOnlyRootFS || !spec.DropAllCapabilities || !spec.NoNewPrivileges {
		return errors.New("proxy created without the required restrictions")
	}
	return f.step("container")
}

func (f *fakeDocker) ConnectProxyEgress(_ context.Context, _ string) error {
	return f.step("egress")
}

func (f *fakeDocker) StartProxy(_ context.Context, _ string) error {
	return f.step("start")
}

func (f *fakeDocker) RemoveProxy(ctx context.Context, _ string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := f.step("remove container"); err != nil {
		return err
	}
	f.proxy = ""
	return nil
}

func (f *fakeDocker) RemoveInternalNetwork(ctx context.Context, _ string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := f.step("remove network"); err != nil {
		return err
	}
	if f.proxy != "" {
		return errors.New("network still has an attached proxy")
	}
	f.network = ""
	return nil
}

var imageID = "sha256:" + strings.Repeat("a", 64)
var leaseDigest = fmt.Sprintf("%x", sha256.Sum256([]byte("sc_task_fixture")))

func shortSocketRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "sc-g-*")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

func TestDockerProxyHostGatewayLifecycle(t *testing.T) {
	for _, tc := range []struct {
		name, fail, cleanupFail string
		want                    []string
	}{
		{"success", "", "", []string{"probe", "network", "container", "egress", "start"}},
		{"network creation may commit", "network", "", []string{"probe", "network", "remove network"}},
		{"proxy creation may commit", "container", "", []string{"probe", "network", "container", "remove container", "remove network"}},
		{"connect failure", "egress", "", []string{"probe", "network", "container", "egress", "remove container", "remove network"}},
		{"start failure", "start", "", []string{"probe", "network", "container", "egress", "start", "remove container", "remove network"}},
		{"proxy cleanup needs retry", "start", "remove container", []string{"probe", "network", "container", "egress", "start", "remove container"}},
		{"network cleanup needs retry", "start", "remove network", []string{"probe", "network", "container", "egress", "start", "remove container", "remove network"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reg := NewRegistry()
			sockRoot := shortSocketRoot(t)
			engine := &fakeDocker{fail: tc.fail, cleanupFail: tc.cleanupFail, socketReachable: false}
			cfg := Config{
				Harness: "codex", DaemonListen: loopbackAddress(), LeaseSHA256: leaseDigest,
				SocketRoot: sockRoot, Registry: reg,
			}
			session, err := Provision(t.Context(), engine, imageID, cfg)
			if (err != nil) != (tc.fail != "") || !reflect.DeepEqual(engine.calls, tc.want) {
				t.Fatalf("provision error %v, calls %v, want %v", err, engine.calls, tc.want)
			}
			entries, readErr := os.ReadDir(sockRoot)
			if readErr != nil || len(entries) != 0 {
				t.Fatalf("probe or host-gateway launch left socket entries: err=%v entries=%v", readErr, entries)
			}
			if tc.fail == "" {
				if engine.spec.HostGateway != DockerHostGateway || engine.spec.HostGatewayTarget != defaultHostGatewayTarget ||
					engine.spec.GateSocketHostPath != "" || engine.spec.GateSocketContainerPath != "" {
					t.Fatalf("unexpected host-gateway container spec: %+v", engine.spec)
				}
				ev := session.Evidence()
				if ev.GateMode != GateModeHostGateway || ev.Network != engine.network || ev.Proxy != engine.proxy || ev.ProxyImageID != imageID {
					t.Fatalf("unexpected launch evidence: %+v", ev)
				}
				endpoint := session.Workload()
				if endpoint.GateMode != GateModeHostGateway || endpoint.Network != engine.network ||
					endpoint.ProxyURL != "http://"+engine.proxy+":"+Port || endpoint.DataGateURL != endpoint.ProxyURL+"/gate/mcp" {
					t.Fatalf("unexpected workload endpoint: %+v", endpoint)
				}
				// Verify preflight succeeds while session is open and fails once closed.
				gateAuth, _ := GateAuthority(loopbackAddress())
				preReq := httptest.NewRequest(http.MethodHead, PreflightPath, nil)
				preReq.Header.Set(LaunchHeader, engine.proxy)
				preReq.Header.Set(ChallengeHeader, strings.Repeat("b", 32))
				preRec := httptest.NewRecorder()
				reg.HandlePreflight(preRec, preReq, gateAuth)
				if preRec.Code != http.StatusNoContent || preRec.Header().Get(ProofHeader) == "" {
					t.Fatalf("active session preflight status=%d proof=%q", preRec.Code, preRec.Header().Get(ProofHeader))
				}
			} else if session != nil && (session.Workload() != (WorkloadEndpoint{}) || session.Evidence() != (LaunchEvidence{})) {
				t.Fatal("failed proxy was advertised to a workload or left evidence")
			}
			if (session != nil) != (tc.fail == "" || tc.cleanupFail != "") {
				t.Fatal("unexpected cleanup handle")
			}
			if session != nil {
				proxyName := engine.proxy
				engine.cleanupFail = ""
				ctx, cancel := context.WithCancel(t.Context())
				cancel()
				if err := session.Close(ctx); err != nil {
					t.Fatalf("cleanup retry: %v", err)
				}
				// Preflight must fail closed after Close.
				gateAuth, _ := GateAuthority(loopbackAddress())
				preReq := httptest.NewRequest(http.MethodHead, PreflightPath, nil)
				preReq.Header.Set(LaunchHeader, proxyName)
				preReq.Header.Set(ChallengeHeader, strings.Repeat("b", 32))
				preRec := httptest.NewRecorder()
				reg.HandlePreflight(preRec, preReq, gateAuth)
				if preRec.Code != http.StatusForbidden || preRec.Header().Get(ProofHeader) != "" {
					t.Fatalf("closed session preflight status=%d proof=%q", preRec.Code, preRec.Header().Get(ProofHeader))
				}
				before := len(engine.calls)
				if err := session.Close(ctx); err != nil || len(engine.calls) != before {
					t.Fatal("repeated cleanup issued Docker operations")
				}
			}
			if engine.proxy != "" || engine.network != "" {
				t.Fatalf("left behind proxy=%q network=%q", engine.proxy, engine.network)
			}
		})
	}
}

func TestDockerProxyUnixSocketLifecycleAndRouteBoundary(t *testing.T) {
	const token = "sc_task_fixture"
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.RawQuery == "redirect" {
			w.Header().Set("Location", "/login")
			w.WriteHeader(http.StatusTemporaryRedirect)
			return
		}
		if r.Header.Get("Cookie") != "" || r.Header.Get("Authorization") != "Bearer "+token {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("unix:" + r.Host))
	}))
	t.Cleanup(daemon.Close)
	daemonAddr := daemon.Listener.Addr().String()
	gateAuthority, err := GateAuthority(daemonAddr)
	if err != nil {
		t.Fatal(err)
	}

	sockRoot := shortSocketRoot(t)
	engine := &fakeDocker{socketReachable: true}
	cfg := Config{Harness: "claude", DaemonListen: daemonAddr, LeaseSHA256: leaseDigest, SocketRoot: sockRoot}
	session, err := Provision(t.Context(), engine, imageID, cfg)
	if err != nil {
		t.Fatal(err)
	}

	if engine.spec.HostGateway != "" || engine.spec.HostGatewayTarget != "" ||
		engine.spec.GateSocketContainerPath != ContainerGateSocket || engine.spec.GateSocketHostPath == "" {
		t.Fatalf("unexpected unix-socket proxy spec: %+v", engine.spec)
	}
	wantArgs := []string{
		"-harness", "claude", "-daemon-listen", daemonAddr, "-lease-sha256", leaseDigest,
		"-listen-name", engine.proxy, "-gate-socket", ContainerGateSocket,
	}
	if !reflect.DeepEqual(engine.spec.Args, wantArgs) {
		t.Fatalf("unexpected proxy args %v, want %v", engine.spec.Args, wantArgs)
	}
	if ev := session.Evidence(); ev.GateMode != GateModeUnixSocket {
		t.Fatalf("evidence GateMode=%q, want %q", ev.GateMode, GateModeUnixSocket)
	}

	// Verify per-launch directory is 0700 and inside sockRoot.
	launchDir := filepath.Dir(engine.spec.GateSocketHostPath)
	if filepath.Dir(launchDir) != sockRoot {
		t.Fatalf("socket dir %q not directly under %q", launchDir, sockRoot)
	}
	dirInfo, err := os.Lstat(launchDir)
	if err != nil || dirInfo.Mode() != os.ModeDir|0o700 {
		t.Fatalf("launch dir stat err=%v mode=%v, want 0700 dir", err, dirInfo.Mode())
	}

	hostSock := engine.spec.GateSocketHostPath
	unixClient := &http.Client{
		Timeout: 2 * time.Second,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{Timeout: time.Second}).DialContext(ctx, "unix", hostSock)
			},
		},
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	t.Cleanup(unixClient.CloseIdleConnections)

	for _, tc := range []struct {
		name, host, path, bearer, upgrade string
		want                              int
	}{
		{"valid gate call", gateAuthority, "/gate/task/messages", token, "", http.StatusOK},
		{"wrong host authority", daemonAddr, "/gate/task/messages", token, "", http.StatusForbidden},
		{"operator route", gateAuthority, "/api/v1/tasks", token, "", http.StatusForbidden},
		{"upgrade refused", gateAuthority, "/gate/task/messages", token, "websocket", http.StatusForbidden},
		{"missing lease", gateAuthority, "/gate/task/messages", "", "", http.StatusUnauthorized},
		{"wrong lease", gateAuthority, "/gate/task/messages", "sc_task_other", "", http.StatusUnauthorized},
		{"daemon redirect refused", gateAuthority, "/gate/task/messages?redirect", token, "", http.StatusBadGateway},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://unix"+tc.path, nil)
			if err != nil {
				t.Fatal(err)
			}
			req.Host = tc.host
			req.Header.Set("Cookie", "operator=fixture")
			if tc.bearer != "" {
				req.Header.Set("Authorization", "Bearer "+tc.bearer)
			}
			if tc.upgrade != "" {
				req.Header.Set("Upgrade", tc.upgrade)
			}
			res, err := unixClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer res.Body.Close()
			if res.StatusCode != tc.want || res.Header.Get("Location") != "" {
				t.Fatalf("status %d Location %q, want %d", res.StatusCode, res.Header.Get("Location"), tc.want)
			}
		})
	}

	if err := session.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(hostSock); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("socket still exists after Close: %v", err)
	}
	if _, err := os.Lstat(launchDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("launch directory still exists after Close: %v", err)
	}

	// Every failure path must remove both the socket and its directory, even if Docker cleanup fails.
	for _, step := range []string{"network", "container", "egress", "start"} {
		failEngine := &fakeDocker{socketReachable: true, fail: step, cleanupFail: "remove container"}
		s, err := Provision(t.Context(), failEngine, imageID, cfg)
		if err == nil {
			t.Fatalf("step %s unexpectedly succeeded", step)
		}
		entries, readErr := os.ReadDir(sockRoot)
		if readErr != nil || len(entries) != 0 {
			t.Fatalf("step %s left socket directory entries: err=%v entries=%v", step, readErr, entries)
		}
		if s != nil {
			failEngine.cleanupFail = ""
			_ = s.Close(t.Context())
		}
	}
}

func TestSweepStaleSockets(t *testing.T) {
	sockRoot := shortSocketRoot(t)
	staleDir := filepath.Join(sockRoot, "g-stale1234")
	if err := os.Mkdir(staleDir, 0o700); err != nil {
		t.Fatal(err)
	}
	uln, err := net.Listen("unix", filepath.Join(staleDir, "gate.sock"))
	if err != nil {
		t.Fatal(err)
	}
	_ = uln.Close()
	// Re-create a stale file in another directory to simulate ungraceful daemon exit.
	staleDir2 := filepath.Join(sockRoot, "g-stale5678")
	if err := os.Mkdir(staleDir2, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(staleDir2, "leftover"), []byte("stale"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := SweepStaleSockets(sockRoot); err != nil {
		t.Fatalf("SweepStaleSockets failed: %v", err)
	}
	entries, err := os.ReadDir(sockRoot)
	if err != nil || len(entries) != 0 {
		t.Fatalf("expected empty socket root after sweep, err=%v entries=%v", err, entries)
	}

	// Missing root directory is a clean no-op at first daemon start.
	if err := SweepStaleSockets(filepath.Join(sockRoot, "absent")); err != nil {
		t.Fatalf("SweepStaleSockets on absent dir: %v", err)
	}

	// Unsafe permissions on root directory must fail closed.
	unsafeDir := shortSocketRoot(t)
	if err := os.Chmod(unsafeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := SweepStaleSockets(unsafeDir); err == nil {
		t.Fatal("SweepStaleSockets accepted 0755 directory")
	}
}

func TestDockerProxyRefusesNonIsolatedInternalNetwork(t *testing.T) {
	isolatedSubnet := netip.PrefixFrom(netip.AddrFrom4([4]byte{172, 20, 0, 0}), 16).String()
	assignedGateway := netip.AddrFrom4([4]byte{172, 20, 0, 1}).String()
	isolatedOpts := map[string]string{BridgeGatewayModeIPv4: GatewayModeIsolated, BridgeGatewayModeIPv6: GatewayModeIsolated}
	for _, tc := range []struct {
		name string
		info InternalNetworkInfo
	}{
		{"engine older than 27.1", InternalNetworkInfo{
			EngineVersion: "27.0.3", Driver: "bridge", Internal: true,
			Options: isolatedOpts, Subnets: []string{isolatedSubnet},
		}},
		{"engine missing version", InternalNetworkInfo{
			Driver: "bridge", Internal: true,
			Options: isolatedOpts, Subnets: []string{isolatedSubnet},
		}},
		{"gateway ip still assigned", InternalNetworkInfo{
			EngineVersion: "29.9.3", Driver: "bridge", Internal: true,
			Options: isolatedOpts, Subnets: []string{isolatedSubnet},
			Gateways: []string{assignedGateway},
		}},
		{"missing ipv4 isolated option", InternalNetworkInfo{
			EngineVersion: "29.9.3", Driver: "bridge", Internal: true,
			Options: map[string]string{BridgeGatewayModeIPv6: GatewayModeIsolated}, Subnets: []string{isolatedSubnet},
		}},
		{"missing ipv6 isolated option", InternalNetworkInfo{
			EngineVersion: "29.9.3", Driver: "bridge", Internal: true,
			Options: map[string]string{BridgeGatewayModeIPv4: GatewayModeIsolated}, Subnets: []string{isolatedSubnet},
		}},
		{"not internal", InternalNetworkInfo{
			EngineVersion: "29.9.3", Driver: "bridge", Internal: false,
			Options: isolatedOpts, Subnets: []string{isolatedSubnet},
		}},
		{"wrong driver", InternalNetworkInfo{
			EngineVersion: "29.9.3", Driver: "host", Internal: true,
			Options: isolatedOpts, Subnets: []string{isolatedSubnet},
		}},
		{"missing subnet", InternalNetworkInfo{
			EngineVersion: "29.9.3", Driver: "bridge", Internal: true,
			Options: isolatedOpts,
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			info := tc.info
			engine := &fakeDocker{netInfo: &info}
			cfg := Config{Harness: "codex", DaemonListen: loopbackAddress(), SocketRoot: shortSocketRoot(t)}
			s, err := Provision(t.Context(), engine, imageID, cfg)
			if s != nil || err == nil {
				t.Fatalf("expected refusal for %s, got session=%v err=%v", tc.name, s, err)
			}
			if engine.netSpec.Options[BridgeGatewayModeIPv4] != GatewayModeIsolated ||
				engine.netSpec.Options[BridgeGatewayModeIPv6] != GatewayModeIsolated {
				t.Fatalf("CreateInternalNetwork did not request isolated gateway options: %+v", engine.netSpec.Options)
			}
			if engine.network != "" || engine.proxy != "" {
				t.Fatalf("non-isolated network was not rolled back: network=%q proxy=%q", engine.network, engine.proxy)
			}
		})
	}
}

func TestDockerProxyRefusesBeforeCreate(t *testing.T) {
	lanListen := net.JoinHostPort(netip.AddrFrom4([4]byte{192, 168, 1, 10}).String(), "7432")
	unspecListen := net.JoinHostPort(net.IPv4zero.String(), "7432")
	for _, tc := range []struct {
		name  string
		image string
		cfg   Config
	}{
		{"remote image tag", "registry/repo:tag", Config{Harness: "codex", DaemonListen: loopbackAddress()}},
		{"invalid image id", "sha256:abc", Config{Harness: "codex", DaemonListen: loopbackAddress()}},
		{"invalid harness", imageID, Config{Harness: "other", DaemonListen: loopbackAddress()}},
		{"lan daemon address", imageID, Config{Harness: "codex", DaemonListen: lanListen}},
		{"unspecified daemon address", imageID, Config{Harness: "codex", DaemonListen: unspecListen}},
		{"pre-populated gate socket", imageID, Config{Harness: "codex", DaemonListen: loopbackAddress(), LeaseSHA256: leaseDigest, GateSocket: ContainerGateSocket}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			engine := &fakeDocker{}
			session, err := Provision(t.Context(), engine, tc.image, tc.cfg)
			if session != nil || err == nil || len(engine.calls) != 0 {
				t.Fatalf("invalid config reached Docker: %v, calls=%v", err, engine.calls)
			}
		})
	}
}

type fakePipeListener struct {
	ch   chan net.Conn
	done chan struct{}
	once sync.Once
}

func newFakePipeListener() *fakePipeListener {
	return &fakePipeListener{ch: make(chan net.Conn), done: make(chan struct{})}
}

func (l *fakePipeListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.ch:
		return c, nil
	case <-l.done:
		return nil, net.ErrClosed
	}
}

func (l *fakePipeListener) Close() error {
	l.once.Do(func() { close(l.done) })
	return nil
}

func (l *fakePipeListener) Addr() net.Addr { return fakeAddr("fake-gate") }

type fakeAddr string

func (a fakeAddr) Network() string { return "fake" }
func (a fakeAddr) String() string  { return string(a) }

func (l *fakePipeListener) tryOffer(timeout time.Duration) (net.Conn, bool) {
	client, server := net.Pipe()
	select {
	case l.ch <- server:
		return client, true
	case <-time.After(timeout):
		_ = client.Close()
		_ = server.Close()
		return nil, false
	}
}

func TestGateListenerLimitsWithFakeListener(t *testing.T) {
	t.Run("admission ceiling for simultaneous incomplete headers and connections", func(t *testing.T) {
		rawLn := newFakePipeListener()
		const ceiling = 3
		limits := GateLimits{
			MaxConns:          ceiling,
			ReadHeaderTimeout: 120 * time.Millisecond,
			ReadTimeout:       time.Second,
			WriteTimeout:      time.Second,
			IdleTimeout:       time.Second,
			MaxHeaderBytes:    4096,
			MaxBodyBytes:      1024,
		}
		srv := NewGateServer(WrapGateHandler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNoContent)
		}), limits), limits)
		done := make(chan struct{})
		go func() {
			defer close(done)
			_ = srv.Serve(LimitListener(rawLn, limits.MaxConns))
		}()
		t.Cleanup(func() {
			_ = srv.Close()
			<-done
		})

		// Open ceiling connections and write incomplete HTTP headers on each.
		stalled := make([]net.Conn, 0, ceiling)
		for i := range ceiling {
			c, ok := rawLn.tryOffer(time.Second)
			if !ok {
				t.Fatalf("failed to admit connection %d below ceiling", i)
			}
			stalled = append(stalled, c)
			if _, err := io.WriteString(c, "GET /gate/task/messages HTTP/1.1\r\nHost: gate\r\n"); err != nil {
				t.Fatal(err)
			}
		}
		t.Cleanup(func() {
			for _, c := range stalled {
				_ = c.Close()
			}
		})

		// While ceiling incomplete headers are open, connection ceiling+1 must NOT be admitted.
		if extra, ok := rawLn.tryOffer(40 * time.Millisecond); ok {
			_ = extra.Close()
			t.Fatal("listener admitted connection beyond simultaneous incomplete-header ceiling")
		}

		// Drain stalled connections as ReadHeaderTimeout closes them, then verify a fresh connection succeeds.
		var wg sync.WaitGroup
		for _, c := range stalled {
			wg.Go(func() {
				_, _ = io.Copy(io.Discard, c)
				_ = c.Close()
			})
		}
		wg.Wait()

		good, ok := rawLn.tryOffer(time.Second)
		if !ok {
			t.Fatal("listener did not recover capacity after incomplete headers timed out")
		}
		defer good.Close()
		if _, err := io.WriteString(good, "GET /gate/task/messages HTTP/1.1\r\nHost: gate\r\nConnection: close\r\n\r\n"); err != nil {
			t.Fatal(err)
		}
		res, err := http.ReadResponse(bufio.NewReader(good), nil)
		if err != nil {
			t.Fatal(err)
		}
		_ = res.Body.Close()
		if res.StatusCode != http.StatusNoContent {
			t.Fatalf("status=%d, want 204", res.StatusCode)
		}
	})

	t.Run("write deadline aborts stalled client", func(t *testing.T) {
		rawLn := newFakePipeListener()
		limits := GateLimits{
			MaxConns:          2,
			ReadHeaderTimeout: time.Second,
			ReadTimeout:       time.Second,
			WriteTimeout:      80 * time.Millisecond,
			IdleTimeout:       time.Second,
			MaxHeaderBytes:    4096,
			MaxBodyBytes:      1024,
		}
		writeErrCh := make(chan error, 1)
		srv := NewGateServer(WrapGateHandler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			flusher, _ := w.(http.Flusher)
			// net.Pipe has zero kernel buffer, so writing while client does not read blocks until WriteTimeout.
			for range 8 {
				if _, err := w.Write(bytes.Repeat([]byte("x"), 4096)); err != nil {
					writeErrCh <- err
					return
				}
				if flusher != nil {
					flusher.Flush()
				}
			}
			writeErrCh <- nil
		}), limits), limits)
		done := make(chan struct{})
		go func() {
			defer close(done)
			_ = srv.Serve(LimitListener(rawLn, limits.MaxConns))
		}()
		t.Cleanup(func() {
			_ = srv.Close()
			<-done
		})

		client, ok := rawLn.tryOffer(time.Second)
		if !ok {
			t.Fatal("failed to offer client pipe")
		}
		defer client.Close()
		if _, err := io.WriteString(client, "GET /gate/task/messages HTTP/1.1\r\nHost: gate\r\n\r\n"); err != nil {
			t.Fatal(err)
		}
		// Intentionally do not read from client so server write stalls.
		select {
		case err := <-writeErrCh:
			if err == nil {
				t.Fatal("expected write deadline error on stalled client, got nil")
			}
		case <-time.After(time.Second):
			t.Fatal("handler remained blocked past write deadline")
		}
	})

	t.Run("body size limit rejects oversized content-length and chunked bodies", func(t *testing.T) {
		rawLn := newFakePipeListener()
		limits := GateLimits{
			MaxConns:          4,
			ReadHeaderTimeout: time.Second,
			ReadTimeout:       time.Second,
			WriteTimeout:      time.Second,
			IdleTimeout:       time.Second,
			MaxHeaderBytes:    4096,
			MaxBodyBytes:      64,
		}
		var handlerCalled bool
		srv := NewGateServer(WrapGateHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			data, err := io.ReadAll(r.Body)
			if err != nil {
				http.Error(w, "read error", http.StatusBadRequest)
				return
			}
			if len(data) > 64 {
				handlerCalled = true
			}
			w.WriteHeader(http.StatusNoContent)
		}), limits), limits)
		done := make(chan struct{})
		go func() {
			defer close(done)
			_ = srv.Serve(LimitListener(rawLn, limits.MaxConns))
		}()
		t.Cleanup(func() {
			_ = srv.Close()
			<-done
		})

		for _, tc := range []struct {
			name string
			raw  string
			want int
		}{
			{
				"oversized content-length",
				fmt.Sprintf("POST /gate/mcp HTTP/1.1\r\nHost: gate\r\nContent-Length: 65\r\nConnection: close\r\n\r\n%s", strings.Repeat("a", 65)),
				http.StatusRequestEntityTooLarge,
			},
			{
				"oversized chunked",
				fmt.Sprintf("POST /gate/mcp HTTP/1.1\r\nHost: gate\r\nTransfer-Encoding: chunked\r\nConnection: close\r\n\r\n41\r\n%s\r\n0\r\n\r\n", strings.Repeat("b", 65)),
				http.StatusRequestEntityTooLarge,
			},
			{
				"within body limit",
				fmt.Sprintf("POST /gate/mcp HTTP/1.1\r\nHost: gate\r\nContent-Length: 32\r\nConnection: close\r\n\r\n%s", strings.Repeat("c", 32)),
				http.StatusNoContent,
			},
		} {
			t.Run(tc.name, func(t *testing.T) {
				client, ok := rawLn.tryOffer(time.Second)
				if !ok {
					t.Fatal("failed to connect")
				}
				defer client.Close()
				errCh := make(chan error, 1)
				go func() {
					_, err := io.WriteString(client, tc.raw)
					errCh <- err
				}()
				res, err := http.ReadResponse(bufio.NewReader(client), nil)
				if err != nil {
					t.Fatal(err)
				}
				_ = res.Body.Close()
				<-errCh
				if res.StatusCode != tc.want {
					t.Fatalf("status=%d, want %d", res.StatusCode, tc.want)
				}
			})
		}
		if handlerCalled {
			t.Fatal("handler observed payload exceeding MaxBodyBytes")
		}
	})
}
