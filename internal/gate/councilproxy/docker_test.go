package councilproxy

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
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
		return ErrGateSocketUnsupported
	}
	var token string
	for i := 0; i+1 < len(spec.Args); i++ {
		if spec.Args[i] == "-probe-token" {
			token = spec.Args[i+1]
		}
	}
	return ProbeGateSocketClient(spec.HostSocketPath, token)
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

func (f *fakeDocker) RemoveProxy(ctx context.Context, name string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := f.step("remove container"); err != nil {
		return err
	}
	// An already absent resource is a no-op, but a live one is only removed by its own name.
	if f.proxy != "" && name != f.proxy {
		return fmt.Errorf("remove container %q, want %q", name, f.proxy)
	}
	f.proxy = ""
	return nil
}

func (f *fakeDocker) RemoveInternalNetwork(ctx context.Context, name string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := f.step("remove network"); err != nil {
		return err
	}
	if f.network != "" && name != f.network {
		return fmt.Errorf("remove network %q, want %q", name, f.network)
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
	const token = "sc_task_fixture"
	daemonAddr := net.JoinHostPort(net.IPv4(127, 0, 0, 1).String(), "7432")
	gateAuth, _ := GateAuthority(daemonAddr)
	for _, tc := range []struct {
		name, fail, cleanupFail string
		want                    []string
	}{
		{"success", "", "", []string{"probe", "network", "container", "egress", "start"}},
		{"probe failure", "probe", "", []string{"probe"}},
		{"network creation may commit", "network", "", []string{"probe", "network", "remove network"}},
		{"proxy creation may commit", "container", "", []string{"probe", "network", "container", "remove container", "remove network"}},
		{"connect failure", "egress", "", []string{"probe", "network", "container", "egress", "remove container", "remove network"}},
		{"start failure", "start", "", []string{"probe", "network", "container", "egress", "start", "remove container", "remove network"}},
		{"proxy cleanup needs retry", "start", "remove container", []string{"probe", "network", "container", "egress", "start", "remove container"}},
		{"network cleanup needs retry", "start", "remove network", []string{"probe", "network", "container", "egress", "start", "remove container", "remove network"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sockRoot := shortSocketRoot(t)
			engine := &fakeDocker{fail: tc.fail, cleanupFail: tc.cleanupFail, socketReachable: false}
			cfg := Config{
				Harness: "codex", DaemonListen: daemonAddr, LeaseSHA256: leaseDigest,
				SocketRoot: sockRoot, GateHandler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.Host != gateAuth || r.Header.Get("Authorization") != "Bearer "+token {
						http.Error(w, "bad gate request", http.StatusBadRequest)
						return
					}
					w.WriteHeader(http.StatusNoContent)
				}),
			}
			session, err := Provision(t.Context(), engine, imageID, cfg)
			if (err != nil) != (tc.fail != "") || !reflect.DeepEqual(engine.calls, tc.want) {
				t.Fatalf("provision error %v, calls %v, want %v", err, engine.calls, tc.want)
			}
			entries, readErr := os.ReadDir(sockRoot)
			if readErr != nil || len(entries) != 0 {
				t.Fatalf("probe or host-gateway launch left socket entries: err=%v entries=%v", readErr, entries)
			}
			var tlsPort, spkiHex string
			for i := 0; i+1 < len(engine.spec.Args); i += 2 {
				switch engine.spec.Args[i] {
				case "-gate-tls-port":
					tlsPort = engine.spec.Args[i+1]
				case "-gate-spki-sha256":
					spkiHex = engine.spec.Args[i+1]
				}
			}
			if tc.fail == "" {
				if engine.spec.HostGateway != DockerHostGateway || engine.spec.HostGatewayTarget != defaultHostGatewayTarget ||
					engine.spec.GateSocketHostPath != "" || engine.spec.GateSocketContainerPath != "" ||
					!validPort(tlsPort) || !validHexBytes(spkiHex, sha256.Size, sha256.Size) {
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
				// Verify pinned TLS 1.3 connection succeeds while session is open.
				tlsAddr := net.JoinHostPort(net.IPv4(127, 0, 0, 1).String(), tlsPort)
				pin := decodeSPKIPin(t, spkiHex)
				tr := newTLSGateTransport(pin, func(ctx context.Context, network, _ string) (net.Conn, error) {
					return (&net.Dialer{Timeout: time.Second}).DialContext(ctx, network, tlsAddr)
				})
				defer tr.CloseIdleConnections()
				req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://"+net.JoinHostPort(DockerHostGateway, tlsPort)+"/gate/task/messages", nil)
				if err != nil {
					t.Fatal(err)
				}
				req.Host = gateAuth
				req.Header.Set("Authorization", "Bearer "+token)
				res, err := tr.RoundTrip(req)
				if err != nil {
					t.Fatalf("active session pinned TLS call failed: %v", err)
				}
				_ = res.Body.Close()
				if res.StatusCode != http.StatusNoContent {
					t.Fatalf("active session TLS gate status=%d, want 204", res.StatusCode)
				}
			} else if session != nil && (session.Workload() != (WorkloadEndpoint{}) || session.Evidence() != (LaunchEvidence{})) {
				t.Fatal("failed proxy was advertised to a workload or left evidence")
			}
			if (session != nil) != (tc.fail == "" || tc.cleanupFail != "") {
				t.Fatal("unexpected cleanup handle")
			}
			if session != nil {
				engine.cleanupFail = ""
				ctx, cancel := context.WithCancel(t.Context())
				cancel()
				if err := session.Close(ctx); err != nil {
					t.Fatalf("cleanup retry: %v", err)
				}
				if tlsPort != "" {
					tlsAddr := net.JoinHostPort(net.IPv4(127, 0, 0, 1).String(), tlsPort)
					if conn, dialErr := net.DialTimeout("tcp4", tlsAddr, 200*time.Millisecond); dialErr == nil {
						_ = conn.Close()
						t.Fatalf("per-launch TLS listener at %s remained open after Close", tlsAddr)
					}
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
	daemonAddr := loopbackAddress()
	gateAuthority, err := GateAuthority(daemonAddr)
	if err != nil {
		t.Fatal(err)
	}
	gateHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
	})

	sockRoot := shortSocketRoot(t)
	engine := &fakeDocker{socketReachable: true}
	cfg := Config{
		Harness: "claude", DaemonListen: daemonAddr, LeaseSHA256: leaseDigest,
		SocketRoot: sockRoot, GateHandler: gateHandler,
	}
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
	// The mount source is symlink-free, so compare against the resolved root.
	resolvedRoot, err := filepath.EvalSymlinks(sockRoot)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(launchDir) != resolvedRoot {
		t.Fatalf("socket dir %q not directly under %q", launchDir, resolvedRoot)
	}
	dirInfo, err := os.Lstat(launchDir)
	if err != nil {
		t.Fatalf("stat launch dir: %v", err)
	}
	if dirInfo.Mode() != os.ModeDir|0o700 {
		t.Fatalf("launch dir mode=%v, want 0700 dir", dirInfo.Mode())
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
	staleDir := filepath.Join(sockRoot, "g-0123456789abcdef")
	if err := os.Mkdir(staleDir, 0o700); err != nil {
		t.Fatal(err)
	}
	uln, err := net.Listen("unix", filepath.Join(staleDir, "gate.sock"))
	if err != nil {
		t.Fatal(err)
	}
	_ = uln.Close()
	// Re-create a stale file in another directory to simulate ungraceful daemon exit.
	staleDir2 := filepath.Join(sockRoot, "p-fedcba9876543210")
	if err := os.Mkdir(staleDir2, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(staleDir2, "leftover"), []byte("stale"), 0o600); err != nil {
		t.Fatal(err)
	}

	// Entries that are not launch directories must survive the sweep.
	keep := map[string]bool{}
	for _, name := range []string{"state.db", "g-short", "g-0123456789abcdeg", "x-0123456789abcdef", "g-0123456789abcdef0"} {
		if err := os.WriteFile(filepath.Join(sockRoot, name), []byte("keep"), 0o600); err != nil {
			t.Fatal(err)
		}
		keep[name] = true
	}
	keepDir := "g-0123456789abcdee.d"
	if err := os.Mkdir(filepath.Join(sockRoot, keepDir), 0o700); err != nil {
		t.Fatal(err)
	}
	keep[keepDir] = true
	// A regular file named like a launch directory is not one.
	if err := os.WriteFile(filepath.Join(sockRoot, "g-aaaaaaaaaaaaaaaa"), []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	keep["g-aaaaaaaaaaaaaaaa"] = true

	if err := SweepStaleSockets(sockRoot); err != nil {
		t.Fatalf("SweepStaleSockets failed: %v", err)
	}
	entries, err := os.ReadDir(sockRoot)
	if err != nil || len(entries) != len(keep) {
		t.Fatalf("sweep left unexpected entries, err=%v entries=%v", err, entries)
	}
	for _, entry := range entries {
		if !keep[entry.Name()] {
			t.Fatalf("sweep kept stale launch entry %q", entry.Name())
		}
	}
	for name := range keep {
		if _, err := os.Lstat(filepath.Join(sockRoot, name)); err != nil {
			t.Fatalf("sweep removed non-launch entry %q: %v", name, err)
		}
	}
	for _, name := range []string{"state.db", "g-short", "g-0123456789abcdeg", "x-0123456789abcdef", "g-0123456789abcdef0", keepDir, "g-aaaaaaaaaaaaaaaa"} {
		if err := os.RemoveAll(filepath.Join(sockRoot, name)); err != nil {
			t.Fatal(err)
		}
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

func TestSocketCleanupRetainsVerifiedDirectoryHandle(t *testing.T) {
	base := shortSocketRoot(t)
	sockRoot := filepath.Join(base, "gates")
	if err := os.Mkdir(sockRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	sockPath, _, cleanup, err := createPrivateUnixSocket(sockRoot, "g-")
	if err != nil {
		t.Fatal(err)
	}
	subName := filepath.Base(filepath.Dir(sockPath))

	victimRoot := filepath.Join(base, "victim")
	victimSub := filepath.Join(victimRoot, subName)
	if err := os.MkdirAll(victimSub, 0o700); err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(victimSub, "keep.txt")
	if err := os.WriteFile(sentinel, []byte("do-not-delete"), 0o600); err != nil {
		t.Fatal(err)
	}

	movedRoot := filepath.Join(base, "moved-gates")
	if err := os.Rename(sockRoot, movedRoot); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victimRoot, sockRoot); err != nil {
		t.Fatal(err)
	}

	cleanErr := cleanup()
	if _, err := os.Lstat(sentinel); err != nil {
		t.Fatalf("cleanup followed replaced parent symlink and deleted victim file: %v", err)
	}
	if cleanErr != nil {
		t.Fatalf("cleanup via retained handle failed: %v", cleanErr)
	}
	if _, err := os.Lstat(filepath.Join(movedRoot, subName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("original launch socket directory was not removed from verified root: %v", err)
	}
}

func TestSessionCloseRetriesFailedSocketCleanup(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("directory permissions do not block removal for root")
	}
	sockRoot := shortSocketRoot(t)
	engine := &fakeDocker{socketReachable: true}
	session, err := Provision(t.Context(), engine, imageID, Config{
		Harness: "codex", DaemonListen: loopbackAddress(), LeaseSHA256: leaseDigest,
		SocketRoot: sockRoot, GateHandler: http.NotFoundHandler(),
	})
	if err != nil {
		t.Fatal(err)
	}
	launchDir := filepath.Dir(engine.spec.GateSocketHostPath)
	// A read-only launch directory stops the socket from being unlinked.
	if err := os.Chmod(launchDir, 0o500); err != nil {
		t.Fatal(err)
	}
	restore := func() { _ = os.Chmod(launchDir, 0o700) }
	t.Cleanup(restore)

	if err := session.Close(t.Context()); err == nil {
		t.Fatal("Close returned nil although the launch directory could not be removed")
	}
	if session.unixGate == nil {
		t.Fatal("Close dropped the Unix gate handle before cleanup succeeded")
	}
	if engine.proxy != "" || engine.network != "" {
		t.Fatalf("Docker resources left behind: proxy=%q network=%q", engine.proxy, engine.network)
	}
	if err := session.Close(t.Context()); err == nil {
		t.Fatal("second Close returned nil although the launch directory is still there")
	}
	restore()
	if err := session.Close(t.Context()); err != nil {
		t.Fatalf("retry Close failed: %v", err)
	}
	if _, err := os.Lstat(launchDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("launch directory still exists after the retry: %v", err)
	}
	if session.unixGate != nil {
		t.Fatal("Unix gate handle retained after successful cleanup")
	}
	before := len(engine.calls)
	if err := session.Close(t.Context()); err != nil || len(engine.calls) != before {
		t.Fatalf("repeated Close after success: err=%v calls=%v", err, engine.calls[before:])
	}
}

func TestTLSGateReissuesCertificateWithPinnedKey(t *testing.T) {
	tg, err := startTLSGate(Config{
		Harness: "codex", DaemonListen: loopbackAddress(), LeaseSHA256: leaseDigest,
		GateHandler: http.NotFoundHandler(),
	}, DefaultGateLimits())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tg.close() })
	spki := func(c *tls.Certificate) string {
		sum := sha256.Sum256(c.Leaf.RawSubjectPublicKeyInfo)
		return hex.EncodeToString(sum[:])
	}
	first := tg.cert.Load()
	start := first.Leaf.NotAfter.Add(-launchCertLifetime)
	for _, tc := range []struct {
		name    string
		at      time.Time
		renewed bool
	}{
		{"fresh certificate is reused", start.Add(time.Minute), false},
		{"half way through the lifetime", start.Add(launchCertLifetime / 3), false},
		{"renewal window opens", start.Add(launchCertLifetime/2 + time.Minute), true},
		{"well past the first expiry", start.Add(3 * launchCertLifetime), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := tg.cert.Load()
			got, err := tg.currentCertificate(tc.at)
			if err != nil {
				t.Fatal(err)
			}
			if (got != before) != tc.renewed {
				t.Fatalf("renewed=%v, want %v", got != before, tc.renewed)
			}
			if tc.at.After(got.Leaf.NotAfter) || tc.at.Before(got.Leaf.NotBefore) {
				t.Fatalf("certificate not valid at %v: %v to %v", tc.at, got.Leaf.NotBefore, got.Leaf.NotAfter)
			}
			if spki(got) != tg.spkiSHA256 {
				t.Fatal("reissued certificate does not match the pin given to the proxy")
			}
		})
	}
	if err := tg.close(); err != nil {
		t.Fatal(err)
	}
	if c, err := tg.currentCertificate(start.Add(4 * launchCertLifetime)); err == nil || c != nil {
		t.Fatalf("closed gate reissued a certificate: %v %v", c, err)
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
		{"engine 27.0 before isolated gateway support", InternalNetworkInfo{
			EngineVersion: "27.0.3", Driver: "bridge", Internal: true,
			Options: isolatedOpts, Subnets: []string{isolatedSubnet},
		}},
		{"engine 27.1 before isolated gateway support", InternalNetworkInfo{
			EngineVersion: "27.1.0", Driver: "bridge", Internal: true,
			Options: isolatedOpts, Subnets: []string{isolatedSubnet},
		}},
		{"engine 27.5 before isolated gateway support", InternalNetworkInfo{
			EngineVersion: "27.5.1", Driver: "bridge", Internal: true,
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

	t.Run("engine 28.0 with isolated options succeeds", func(t *testing.T) {
		info := InternalNetworkInfo{
			EngineVersion: "28.0.0", Driver: "bridge", Internal: true,
			Options: isolatedOpts, Subnets: []string{isolatedSubnet},
		}
		engine := &fakeDocker{netInfo: &info}
		cfg := Config{Harness: "codex", DaemonListen: loopbackAddress(), SocketRoot: shortSocketRoot(t)}
		s, err := Provision(t.Context(), engine, imageID, cfg)
		if err != nil || s == nil {
			t.Fatalf("expected 28.0.0 isolated network to succeed, got session=%v err=%v", s, err)
		}
		if err := s.Close(t.Context()); err != nil {
			t.Fatal(err)
		}
	})
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
		{"pre-populated gate socket", imageID, Config{Harness: "codex", DaemonListen: loopbackAddress(), LeaseSHA256: leaseDigest, GateSocket: ContainerGateSocket, GateHandler: http.NotFoundHandler()}},
		{"pre-populated gate spki", imageID, Config{Harness: "codex", DaemonListen: loopbackAddress(), LeaseSHA256: leaseDigest, GateSPKISHA256: strings.Repeat("a", 64), GateHandler: http.NotFoundHandler()}},
		{"missing gate handler with lease", imageID, Config{Harness: "codex", DaemonListen: loopbackAddress(), LeaseSHA256: leaseDigest}},
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

func startFakePipeGate(t *testing.T, limits GateLimits) *fakePipeListener {
	t.Helper()
	rawLn := newFakePipeListener()
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
	return rawLn
}

func openStalledPipes(t *testing.T, rawLn *fakePipeListener, n int) []net.Conn {
	t.Helper()
	stalled := make([]net.Conn, 0, n)
	t.Cleanup(func() {
		for _, c := range stalled {
			_ = c.Close()
		}
	})
	for i := range n {
		c, ok := rawLn.tryOffer(time.Second)
		if !ok {
			t.Fatalf("failed to admit connection %d below ceiling", i)
		}
		stalled = append(stalled, c)
		if _, err := io.WriteString(c, "GET /gate/task/messages HTTP/1.1\r\nHost: gate\r\n"); err != nil {
			t.Fatal(err)
		}
	}
	return stalled
}

func requireFreshConnectionServed(t *testing.T, rawLn *fakePipeListener) {
	t.Helper()
	good, ok := rawLn.tryOffer(time.Second)
	if !ok {
		t.Fatal("listener did not recover capacity")
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
}

func TestGateListenerLimitsWithFakeListener(t *testing.T) {
	const ceiling = 3
	t.Run("ceiling holds while incomplete headers stay open", func(t *testing.T) {
		// The header timeout is far longer than the test, so the held connections cannot expire mid-check.
		rawLn := startFakePipeGate(t, GateLimits{
			MaxConns: ceiling, ReadHeaderTimeout: time.Minute, ReadTimeout: time.Minute,
			WriteTimeout: time.Second, IdleTimeout: time.Second, MaxHeaderBytes: 4096, MaxBodyBytes: 1024,
		})
		stalled := openStalledPipes(t, rawLn, ceiling)
		if extra, ok := rawLn.tryOffer(40 * time.Millisecond); ok {
			_ = extra.Close()
			t.Fatal("listener admitted connection beyond simultaneous incomplete-header ceiling")
		}
		// Releasing one held connection frees exactly one slot.
		_ = stalled[0].Close()
		requireFreshConnectionServed(t, rawLn)
	})

	t.Run("header timeout releases stalled connections", func(t *testing.T) {
		rawLn := startFakePipeGate(t, GateLimits{
			MaxConns: ceiling, ReadHeaderTimeout: 120 * time.Millisecond, ReadTimeout: time.Second,
			WriteTimeout: time.Second, IdleTimeout: time.Second, MaxHeaderBytes: 4096, MaxBodyBytes: 1024,
		})
		stalled := openStalledPipes(t, rawLn, ceiling)
		// Drain stalled connections until ReadHeaderTimeout makes the server close them.
		var wg sync.WaitGroup
		for _, c := range stalled {
			wg.Go(func() {
				_, _ = io.Copy(io.Discard, c)
				_ = c.Close()
			})
		}
		wg.Wait()
		requireFreshConnectionServed(t, rawLn)
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

func TestWrapGateHandlerSlowBodiesAndPromptRefusal(t *testing.T) {
	ln, err := net.Listen("tcp4", net.JoinHostPort(net.IPv4(127, 0, 0, 1).String(), "0"))
	if err != nil {
		t.Fatal(err)
	}
	const slots = MaxGateConns // 32
	limits := GateLimits{
		MaxConns:          slots,
		ReadHeaderTimeout: time.Second,
		ReadTimeout:       250 * time.Millisecond,
		WriteTimeout:      time.Second,
		IdleTimeout:       time.Second,
		MaxHeaderBytes:    4096,
		MaxBodyBytes:      1024,
	}
	// Mirror sablecourtd: server sets ReadHeaderTimeout only; WrapGateHandler must enforce body deadlines and 32-slot admission.
	srv := &http.Server{
		ReadHeaderTimeout: time.Second,
		Handler: WrapGateHandler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNoContent)
		}), limits),
	}
	srvDone := make(chan struct{})
	go func() {
		defer close(srvDone)
		_ = srv.Serve(LimitListener(ln, 256))
	}()
	t.Cleanup(func() {
		_ = srv.Close()
		<-srvDone
	})

	addr := ln.Addr().String()
	slowConns := make([]net.Conn, 0, slots)
	slowReaders := make([]*bufio.Reader, 0, slots)
	t.Cleanup(func() {
		for _, c := range slowConns {
			_ = c.Close()
		}
	})

	// Hold all 32 slots in WrapGateHandler with incomplete bodies.
	// Expect: 100-continue fires only when WrapGateHandler starts reading r.Body after acquiring its slot.
	for i := range slots {
		c, err := net.DialTimeout("tcp4", addr, time.Second)
		if err != nil {
			t.Fatalf("dial slow body %d: %v", i, err)
		}
		slowConns = append(slowConns, c)
		if _, err := io.WriteString(c, "POST /gate/mcp HTTP/1.1\r\nHost: gate\r\nContent-Length: 64\r\nExpect: 100-continue\r\n\r\n"); err != nil {
			t.Fatalf("write headers %d: %v", i, err)
		}
		br := bufio.NewReader(c)
		slowReaders = append(slowReaders, br)
		if err := c.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
			t.Fatal(err)
		}
		line, err := br.ReadString('\n')
		if err != nil || !strings.Contains(line, "100 Continue") {
			t.Fatalf("slot %d did not enter body read: line=%q err=%v", i, line, err)
		}
		// Consume blank line terminating 100 Continue response, then send 1 byte and stall.
		if _, err := br.ReadString('\n'); err != nil {
			t.Fatalf("slot %d 100-continue crlf: %v", i, err)
		}
		if _, err := io.WriteString(c, "{"); err != nil {
			t.Fatalf("slot %d partial body write: %v", i, err)
		}
	}

	// The 33rd request must be refused promptly (well before the 250ms body ReadTimeout).
	fastClient := &http.Client{Timeout: 100 * time.Millisecond}
	defer fastClient.CloseIdleConnections()
	res33, err := fastClient.Get("http://" + addr + "/gate/task/messages")
	if err != nil {
		t.Fatalf("33rd request blocked instead of prompt refusal: %v", err)
	}
	_ = res33.Body.Close()
	if res33.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("33rd request status = %d, want %d", res33.StatusCode, http.StatusServiceUnavailable)
	}

	// Wait for the body ReadTimeout to abort all 32 stalled bodies and free their slots.
	deadline := time.Now().Add(2 * time.Second)
	for i, c := range slowConns {
		if err := c.SetReadDeadline(deadline); err != nil {
			t.Fatal(err)
		}
		res, err := http.ReadResponse(slowReaders[i], nil)
		if err != nil {
			t.Fatalf("slot %d: server did not end the stalled upload before the client gave up: %v", i, err)
		}
		_ = res.Body.Close()
		if res.StatusCode != http.StatusBadRequest {
			t.Fatalf("slot %d: stalled upload status=%d, want %d", i, res.StatusCode, http.StatusBadRequest)
		}
		_ = c.Close()
	}

	// Verify all 32 slots are freed after the deadline by running 32 requests.
	okClient := &http.Client{Timeout: time.Second}
	defer okClient.CloseIdleConnections()
	var wg sync.WaitGroup
	errs := make(chan error, slots)
	for range slots {
		wg.Go(func() {
			res, err := okClient.Post("http://"+addr+"/gate/mcp", "application/json", strings.NewReader(`{}`))
			if err != nil {
				errs <- err
				return
			}
			_ = res.Body.Close()
			if res.StatusCode != http.StatusNoContent {
				errs <- fmt.Errorf("status = %d, want 204", res.StatusCode)
			}
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("post-deadline slot recovery failed: %v", err)
	}
}

type probeOverrideDocker struct {
	fakeDocker
	probe func(context.Context, GateSocketProbe) error
}

func (p *probeOverrideDocker) ProbeGateSocket(ctx context.Context, spec GateSocketProbe) error {
	p.calls = append(p.calls, "probe")
	return p.probe(ctx, spec)
}

func TestProbeUnixSocketModeFailsClosedOnDockerOrExchangeError(t *testing.T) {
	for _, tc := range []struct {
		name  string
		probe func(context.Context, GateSocketProbe) error
	}{
		{
			name: "docker runtime error",
			probe: func(_ context.Context, _ GateSocketProbe) error {
				return errors.New("docker daemon unavailable")
			},
		},
		{
			name: "zero exit without connecting",
			probe: func(_ context.Context, _ GateSocketProbe) error {
				return nil
			},
		},
		{
			name: "wrong probe token",
			probe: func(_ context.Context, spec GateSocketProbe) error {
				conn, err := net.DialTimeout("unix", spec.HostSocketPath, time.Second)
				if err != nil {
					return err
				}
				defer conn.Close()
				_, _ = io.WriteString(conn, "wrong-token\n")
				return nil
			},
		},
		{
			name: "eof before token",
			probe: func(_ context.Context, spec GateSocketProbe) error {
				conn, err := net.DialTimeout("unix", spec.HostSocketPath, time.Second)
				if err != nil {
					return err
				}
				return conn.Close()
			},
		},
		{
			name: "unsupported claimed after host connection accepted",
			probe: func(_ context.Context, spec GateSocketProbe) error {
				conn, err := net.DialTimeout("unix", spec.HostSocketPath, time.Second)
				if err != nil {
					return err
				}
				_ = conn.Close()
				return ErrGateSocketUnsupported
			},
		},
		{
			name: "unsupported claimed after completed token exchange",
			probe: func(_ context.Context, spec GateSocketProbe) error {
				var token string
				for i := 0; i+1 < len(spec.Args); i++ {
					if spec.Args[i] == "-probe-token" {
						token = spec.Args[i+1]
					}
				}
				if err := ProbeGateSocketClient(spec.HostSocketPath, token); err != nil {
					return err
				}
				return ErrGateSocketUnsupported
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sockRoot := shortSocketRoot(t)
			engine := &probeOverrideDocker{probe: tc.probe}
			cfg := Config{
				Harness: "codex", DaemonListen: loopbackAddress(), LeaseSHA256: leaseDigest,
				SocketRoot: sockRoot, GateHandler: http.NotFoundHandler(),
			}
			s, err := Provision(t.Context(), engine, imageID, cfg)
			if s != nil {
				_ = s.Close(t.Context())
			}
			if err == nil || !reflect.DeepEqual(engine.calls, []string{"probe"}) {
				t.Fatalf("probe failure %q fell back instead of failing closed: session=%v err=%v calls=%v", tc.name, s, err, engine.calls)
			}
			entries, readErr := os.ReadDir(sockRoot)
			if readErr != nil || len(entries) != 0 {
				t.Fatalf("probe failure left socket entries: err=%v entries=%v", readErr, entries)
			}
		})
	}
}

func TestUnixSocketModeDispatchesInProcessWithoutLoopbackDial(t *testing.T) {
	const token = "sc_task_fixture"
	ln, err := net.Listen("tcp4", net.JoinHostPort(net.IPv4(127, 0, 0, 1).String(), "0"))
	if err != nil {
		t.Fatal(err)
	}
	daemonAddr := ln.Addr().String()
	gateAuthority, err := GateAuthority(daemonAddr)
	if err != nil {
		_ = ln.Close()
		t.Fatal(err)
	}

	gateHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Host != gateAuthority || r.Header.Get("Cookie") != "" || r.Header.Get("Authorization") != "Bearer "+token {
			http.Error(w, "unexpected gate request", http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "in-process-ok")
	})

	sockRoot := shortSocketRoot(t)
	engine := &fakeDocker{socketReachable: true}
	session, err := Provision(t.Context(), engine, imageID, Config{
		Harness:      "claude",
		DaemonListen: daemonAddr,
		LeaseSHA256:  leaseDigest,
		SocketRoot:   sockRoot,
		GateHandler:  gateHandler,
	})
	if err != nil {
		_ = ln.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = session.Close(t.Context()) })

	// Close the daemon loopback port and replace it with an unauthenticated listener.
	_ = ln.Close()
	subLn, err := net.Listen("tcp4", daemonAddr)
	if err != nil {
		t.Fatalf("bind substituted loopback listener: %v", err)
	}
	var tcpHits, tcpLeaks int32
	var tcpMu sync.Mutex
	subSrv := &http.Server{
		ReadHeaderTimeout: time.Second,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			tcpMu.Lock()
			tcpHits++
			if r.Header.Get("Authorization") != "" {
				tcpLeaks++
			}
			tcpMu.Unlock()
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, "substituted-tcp")
		}),
	}
	subDone := make(chan struct{})
	go func() {
		defer close(subDone)
		_ = subSrv.Serve(subLn)
	}()
	t.Cleanup(func() {
		_ = subSrv.Close()
		<-subDone
	})

	hostSock := engine.spec.GateSocketHostPath
	unixClient := &http.Client{
		Timeout: 2 * time.Second,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{Timeout: time.Second}).DialContext(ctx, "unix", hostSock)
			},
		},
	}
	t.Cleanup(unixClient.CloseIdleConnections)

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://unix/gate/task/messages", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = gateAuthority
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Cookie", "operator=strip-me")
	res, err := unixClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(res.Body)
	_ = res.Body.Close()

	tcpMu.Lock()
	hits, leaks := tcpHits, tcpLeaks
	tcpMu.Unlock()
	if hits != 0 || leaks != 0 {
		t.Fatalf("unix socket gate dialed loopback port: hits=%d leaks=%d body=%q", hits, leaks, string(body))
	}
	if res.StatusCode != http.StatusOK || string(body) != "in-process-ok" {
		t.Fatalf("status=%d body=%q, want 200 in-process-ok", res.StatusCode, string(body))
	}
}

func TestHostGatewayArgsContainNoSecret(t *testing.T) {
	sockRoot := shortSocketRoot(t)
	engine := &fakeDocker{socketReachable: false}
	session, err := Provision(t.Context(), engine, imageID, Config{
		Harness:      "codex",
		DaemonListen: loopbackAddress(),
		LeaseSHA256:  leaseDigest,
		SocketRoot:   sockRoot,
		GateHandler:  http.NotFoundHandler(),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = session.Close(t.Context()) })

	allowedFlags := map[string]bool{
		"-harness":          true,
		"-daemon-listen":    true,
		"-lease-sha256":     true,
		"-listen-name":      true,
		"-gate-tls-port":    true,
		"-gate-spki-sha256": true,
	}
	const rawLease = "sc_task_fixture"
	for i := 0; i < len(engine.spec.Args); i += 2 {
		flagName := engine.spec.Args[i]
		if !allowedFlags[flagName] || i+1 >= len(engine.spec.Args) {
			t.Fatalf("unexpected or secret-carrying flag %q in proxy argv: %v", flagName, engine.spec.Args)
		}
		value := engine.spec.Args[i+1]
		if flagName == "-lease-sha256" && value != leaseDigest {
			t.Fatalf("-lease-sha256 does not carry the fixture digest")
		}
		if strings.Contains(value, rawLease) {
			t.Fatalf("raw lease appears in the value of proxy flag %q", flagName)
		}
	}
	for _, entry := range session.Workload().Env() {
		name, val, _ := strings.Cut(entry, "=")
		if strings.Contains(entry, rawLease) {
			t.Fatalf("raw lease appears in workload env entry %q", name)
		}
		upper := strings.ToUpper(name)
		if strings.Contains(upper, "SECRET") || strings.Contains(upper, "TOKEN") || strings.Contains(upper, "KEY") || strings.Contains(val, "PRIVATE KEY") {
			t.Fatalf("unexpected secret-like workload env entry %q", name)
		}
	}
	if err := session.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	if session.tlsGate != nil {
		t.Fatal("tlsGate retained on session after Close")
	}
}

func TestIsUnshareableSocketBindError(t *testing.T) {
	sockRoot := shortSocketRoot(t)
	sockPath, _, cleanup, err := createPrivateUnixSocket(sockRoot, "p-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cleanup() })

	resolved, err := filepath.EvalSymlinks(sockPath)
	if err != nil {
		t.Fatal(err)
	}
	regFile := filepath.Join(sockRoot, "not-a-socket")
	if err := os.WriteFile(regFile, []byte("plain"), 0o600); err != nil {
		t.Fatal(err)
	}
	missingPath := filepath.Join(sockRoot, "missing.sock")

	for _, tc := range []struct {
		name     string
		path     string
		exitCode int
		output   string
		want     bool
	}{
		{
			name:     "docker desktop socket_mnt rewrite",
			path:     sockPath,
			exitCode: 125,
			output:   "docker run: exit status 125: docker: Error response from daemon: invalid mount config for type \"bind\": bind source path does not exist: /socket_mnt" + resolved + "\n\nRun 'docker run --help' for more information\n",
			want:     true,
		},
		{
			name:     "docker desktop host_mnt rewrite",
			path:     sockPath,
			exitCode: 125,
			output:   "docker: Error response from daemon: invalid mount config for type \"bind\": bind source path does not exist: /host_mnt" + resolved + "\n",
			want:     true,
		},
		{
			name:     "unprefixed vm missing bind source",
			path:     sockPath,
			exitCode: 125,
			output:   "docker: Error response from daemon: invalid mount config for type \"bind\": bind source path does not exist: " + sockPath + "\n",
			want:     true,
		},
		{
			name:     "docker desktop mounts denied multiline",
			path:     sockPath,
			exitCode: 125,
			output:   "docker: Error response from daemon: Mounts denied:\nThe path " + filepath.Dir(resolved) + " is not shared from the host and is not known to Docker.\n",
			want:     true,
		},
		{
			name:     "mounts denied for a sibling of the probe directory fails closed",
			path:     sockPath,
			exitCode: 125,
			output:   "docker: Error response from daemon: Mounts denied:\nThe path " + filepath.Dir(resolved) + "-other is not shared from the host and is not known to Docker.\n",
			want:     false,
		},
		{
			name:     "mounts denied names the probe directory only in passing fails closed",
			path:     sockPath,
			exitCode: 125,
			output:   "docker: Error response from daemon: Mounts denied:\nThe path /somewhere/else is not shared from the host and is not known to Docker. See " + filepath.Dir(resolved) + "\n",
			want:     false,
		},
		{
			name:     "mounts denied for the exact socket path",
			path:     sockPath,
			exitCode: 125,
			output:   "docker: Error response from daemon: Mounts denied: The path " + resolved + " is not shared from the host and is not known to Docker.\n",
			want:     true,
		},
		{
			name:     "missing host socket fails closed",
			path:     missingPath,
			exitCode: 125,
			output:   "docker: Error response from daemon: invalid mount config for type \"bind\": bind source path does not exist: /socket_mnt" + missingPath + "\n",
			want:     false,
		},
		{
			name:     "regular host file fails closed",
			path:     regFile,
			exitCode: 125,
			output:   "docker: Error response from daemon: invalid mount config for type \"bind\": bind source path does not exist: /socket_mnt" + regFile + "\n",
			want:     false,
		},
		{
			name:     "wrong exit code fails closed",
			path:     sockPath,
			exitCode: 1,
			output:   "docker: Error response from daemon: invalid mount config for type \"bind\": bind source path does not exist: /socket_mnt" + resolved + "\n",
			want:     false,
		},
		{
			name:     "different bind path fails closed",
			path:     sockPath,
			exitCode: 125,
			output:   "docker: Error response from daemon: invalid mount config for type \"bind\": bind source path does not exist: /socket_mnt/tmp/unrelated/gate.sock\n",
			want:     false,
		},
		{
			name:     "daemon unreachable fails closed",
			path:     sockPath,
			exitCode: 125,
			output:   "docker: Cannot connect to the Docker daemon at unix:///var/run/docker.sock.\n",
			want:     false,
		},
		{
			name:     "missing image fails closed",
			path:     sockPath,
			exitCode: 125,
			output:   "Unable to find image 'sha256:aaa' locally\n",
			want:     false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsUnshareableSocketBindError(tc.path, tc.exitCode, tc.output); got != tc.want {
				t.Fatalf("IsUnshareableSocketBindError() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestWrapGateHandlerBusyRefusalBoundsUnreadBody(t *testing.T) {
	ln, err := net.Listen("tcp4", net.JoinHostPort(net.IPv4(127, 0, 0, 1).String(), "0"))
	if err != nil {
		t.Fatal(err)
	}
	limits := GateLimits{
		MaxConns: 1, ReadHeaderTimeout: time.Second, ReadTimeout: 200 * time.Millisecond,
		WriteTimeout: time.Second, IdleTimeout: time.Second, MaxHeaderBytes: 4096, MaxBodyBytes: 1024,
	}
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	// Mirror sablecourtd: the server sets ReadHeaderTimeout only.
	srv := &http.Server{
		ReadHeaderTimeout: time.Second,
		Handler: WrapGateHandler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			entered <- struct{}{}
			<-release
			w.WriteHeader(http.StatusNoContent)
		}), limits),
	}
	srvDone := make(chan struct{})
	go func() {
		defer close(srvDone)
		_ = srv.Serve(LimitListener(ln, 8))
	}()
	t.Cleanup(func() {
		close(release)
		_ = srv.Close()
		<-srvDone
	})

	holder, err := net.DialTimeout("tcp4", ln.Addr().String(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = holder.Close() })
	if _, err := io.WriteString(holder, "GET /gate/task/messages HTTP/1.1\r\nHost: gate\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("first request never reached the handler")
	}

	stalled, err := net.DialTimeout("tcp4", ln.Addr().String(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer stalled.Close()
	if _, err := io.WriteString(stalled, "POST /gate/mcp HTTP/1.1\r\nHost: gate\r\nContent-Length: 64\r\n\r\n{"); err != nil {
		t.Fatal(err)
	}
	if err := stalled.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	br := bufio.NewReader(stalled)
	res, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatalf("busy refusal: %v", err)
	}
	_ = res.Body.Close()
	if res.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status=%d, want %d", res.StatusCode, http.StatusServiceUnavailable)
	}
	// The server must end the connection once the read deadline passes instead of
	// waiting for the rest of the body.
	if _, err := io.Copy(io.Discard, br); err != nil {
		t.Fatalf("server kept the connection open past the read deadline: %v", err)
	}
}

func TestProvisionRequiresDurableIdentityBeforeDocker(t *testing.T) {
	d := &fakeDocker{}
	called := false
	s, err := Provision(t.Context(), d, "sha256:"+strings.Repeat("a", 64), Config{
		Harness: "claude", DaemonListen: loopbackAddress(),
		RecordResources: func(proxy, network string) error {
			called = true
			if proxy == "" || network != proxy+"-net" {
				t.Fatal("incomplete resource identity")
			}
			return errors.New("journal unavailable")
		},
	})
	if !called || s != nil || err == nil || len(d.calls) != 0 {
		t.Fatalf("calls=%v err=%v", d.calls, err)
	}
}
