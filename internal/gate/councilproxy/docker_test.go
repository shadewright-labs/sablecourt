package councilproxy

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"reflect"
	"strings"
	"testing"
	"time"
)

type fakeDocker struct {
	fail, cleanupFail string
	gateway           string
	calls             []string
	network, proxy    string
	spec              ProxyContainer
}

func (f *fakeDocker) step(step string) error {
	f.calls = append(f.calls, step)
	if step == f.fail || step == f.cleanupFail {
		return errors.New("injected " + step)
	}
	return nil
}

func (f *fakeDocker) EgressGateway(_ context.Context) (string, error) {
	if err := f.step("gateway"); err != nil {
		return "", err
	}
	return f.gateway, nil
}

func (f *fakeDocker) CreateInternalNetwork(_ context.Context, name string) error {
	f.network = name
	return f.step("network")
}

func (f *fakeDocker) CreateProxy(_ context.Context, spec ProxyContainer) error {
	f.proxy = spec.Name
	f.spec = spec
	if spec.InternalNetwork != f.network || spec.ImageID != imageID || spec.User != "10001:10001" ||
		!spec.ReadOnlyRootFS || !spec.DropAllCapabilities || !spec.NoNewPrivileges ||
		spec.HostGateway != DockerHostGateway || spec.HostGatewayTarget == "" {
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

func TestDockerProxyLifecycle(t *testing.T) {
	for _, tc := range []struct {
		name, fail, cleanupFail string
		want                    []string
	}{
		{"success", "", "", []string{"gateway", "network", "container", "egress", "start"}},
		{"gateway failure", "gateway", "", []string{"gateway"}},
		{"network creation may commit", "network", "", []string{"gateway", "network", "remove network"}},
		{"proxy creation may commit", "container", "", []string{"gateway", "network", "container", "remove container", "remove network"}},
		{"connect failure", "egress", "", []string{"gateway", "network", "container", "egress", "remove container", "remove network"}},
		{"start failure", "start", "", []string{"gateway", "network", "container", "egress", "start", "remove container", "remove network"}},
		{"proxy cleanup needs retry", "start", "remove container", []string{"gateway", "network", "container", "egress", "start", "remove container"}},
		{"network cleanup needs retry", "start", "remove network", []string{"gateway", "network", "container", "egress", "start", "remove container", "remove network"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			engine := &fakeDocker{fail: tc.fail, cleanupFail: tc.cleanupFail}
			cfg := Config{Harness: "codex", DaemonListen: loopbackAddress(), LeaseSHA256: leaseDigest}
			session, err := Provision(t.Context(), engine, imageID, cfg)
			if (err != nil) != (tc.fail != "") || !reflect.DeepEqual(engine.calls, tc.want) {
				t.Fatalf("provision error %v, calls %v, want %v", err, engine.calls, tc.want)
			}
			if tc.fail == "" {
				wantArgs := []string{
					"-harness", "codex", "-daemon-listen", loopbackAddress(), "-lease-sha256", leaseDigest,
					"-listen-name", engine.proxy,
				}
				if engine.spec.HostGatewayTarget != defaultHostGatewayTarget || !reflect.DeepEqual(engine.spec.Args, wantArgs) {
					t.Fatalf("unexpected proxy spec target=%q args=%v", engine.spec.HostGatewayTarget, engine.spec.Args)
				}
				endpoint := session.Workload()
				if endpoint.Network != engine.network || endpoint.ProxyURL != "http://"+engine.proxy+":"+Port ||
					endpoint.DataGateURL != endpoint.ProxyURL+"/gate/mcp" ||
					!reflect.DeepEqual(endpoint.Env(), []string{
						"HTTP_PROXY=" + endpoint.ProxyURL, "HTTPS_PROXY=" + endpoint.ProxyURL,
						"http_proxy=" + endpoint.ProxyURL, "https_proxy=" + endpoint.ProxyURL,
						"NO_PROXY=" + engine.proxy, "no_proxy=" + engine.proxy,
						"SABLECOURT_DATA_GATE_URL=" + endpoint.DataGateURL,
					}) {
					t.Fatal("unexpected workload network or environment")
				}
			} else if session != nil && session.Workload() != (WorkloadEndpoint{}) {
				t.Fatal("failed proxy was advertised to a workload")
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

func TestDockerProxyRefusesBeforeCreate(t *testing.T) {
	for _, tc := range []struct {
		name, image, harness, gateway string
		wantCalls                     []string
	}{
		{"remote image tag", "registry/repo:tag", "codex", "", nil},
		{"invalid image id", "sha256:abc", "codex", "", nil},
		{"invalid harness", imageID, "other", "", nil},
		{"malformed gateway", imageID, "codex", "not-an-ip", []string{"gateway"}},
		{"loopback gateway", imageID, "codex", net.IPv4(127, 0, 0, 1).String(), []string{"gateway"}},
		{"unspecified gateway", imageID, "codex", net.IPv4zero.String(), []string{"gateway"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			engine := &fakeDocker{gateway: tc.gateway}
			session, err := Provision(t.Context(), engine, tc.image, Config{Harness: tc.harness, DaemonListen: loopbackAddress()})
			if session != nil || err == nil || !reflect.DeepEqual(engine.calls, tc.wantCalls) {
				t.Fatalf("invalid config reached Docker: %v, calls=%v want=%v", err, engine.calls, tc.wantCalls)
			}
		})
	}
}

func TestDockerProxyLocalGatewayBridge(t *testing.T) {
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
		_, _ = w.Write([]byte("bridged:" + r.Host))
	}))
	t.Cleanup(daemon.Close)
	daemonAddr := daemon.Listener.Addr().String()
	gateAuthority, err := GateAuthority(daemonAddr)
	if err != nil {
		t.Fatal(err)
	}
	gatewayIP := netip.AddrFrom4([4]byte{198, 51, 100, 1})

	origLocal := isLocalHostIP
	origListen := listenBridge
	t.Cleanup(func() {
		isLocalHostIP = origLocal
		listenBridge = origListen
	})
	isLocalHostIP = func(addr netip.Addr) bool { return addr == gatewayIP }
	var boundAddr string
	listenCalls := 0
	listenBridge = func(network, address string) (net.Listener, error) {
		listenCalls++
		if network != "tcp" || address != net.JoinHostPort(gatewayIP.String(), "0") {
			return nil, fmt.Errorf("unexpected bridge bind %s %s", network, address)
		}
		ln, err := net.Listen("tcp4", net.JoinHostPort(net.IPv4(127, 0, 0, 1).String(), "0"))
		if err == nil {
			boundAddr = ln.Addr().String()
		}
		return ln, err
	}

	engine := &fakeDocker{gateway: gatewayIP.String()}
	cfg := Config{Harness: "claude", DaemonListen: daemonAddr, LeaseSHA256: leaseDigest}
	session, err := Provision(t.Context(), engine, imageID, cfg)
	if err != nil {
		t.Fatal(err)
	}
	_, bridgePort, err := net.SplitHostPort(boundAddr)
	if err != nil {
		t.Fatal(err)
	}
	wantArgs := []string{
		"-harness", "claude", "-daemon-listen", daemonAddr, "-lease-sha256", leaseDigest,
		"-listen-name", engine.proxy, "-gate-port", bridgePort,
	}
	if engine.spec.HostGatewayTarget != gatewayIP.String() || !reflect.DeepEqual(engine.spec.Args, wantArgs) {
		t.Fatalf("unexpected local bridge spec target=%q args=%v", engine.spec.HostGatewayTarget, engine.spec.Args)
	}

	client := &http.Client{
		Timeout:   2 * time.Second,
		Transport: &http.Transport{},
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	t.Cleanup(client.CloseIdleConnections)

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
			req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+boundAddr+tc.path, nil)
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
			res, err := client.Do(req)
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
	if conn, err := net.DialTimeout("tcp", boundAddr, 200*time.Millisecond); err == nil {
		_ = conn.Close()
		t.Fatal("gate bridge listener stayed open after session close")
	}

	// Login-only sessions need no gate bridge listener.
	beforeLogin := listenCalls
	loginEngine := &fakeDocker{gateway: gatewayIP.String()}
	loginSession, err := Provision(t.Context(), loginEngine, imageID, Config{Harness: "claude", DaemonListen: daemonAddr})
	if err != nil {
		t.Fatal(err)
	}
	if listenCalls != beforeLogin || loginEngine.spec.HostGatewayTarget != gatewayIP.String() {
		t.Fatalf("login session unexpectedly opened bridge: calls=%d target=%q", listenCalls-beforeLogin, loginEngine.spec.HostGatewayTarget)
	}
	if err := loginSession.Close(t.Context()); err != nil {
		t.Fatal(err)
	}

	// Rollback on start failure must close the bridge listener.
	failEngine := &fakeDocker{gateway: gatewayIP.String(), fail: "start"}
	if s, err := Provision(t.Context(), failEngine, imageID, cfg); s != nil || err == nil {
		t.Fatalf("expected rollback failure, got session=%v err=%v", s, err)
	}
	if conn, err := net.DialTimeout("tcp", boundAddr, 200*time.Millisecond); err == nil {
		_ = conn.Close()
		t.Fatal("gate bridge listener stayed open after rollback")
	}
}
