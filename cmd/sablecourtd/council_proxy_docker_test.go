package main

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/shadewright-labs/sablecourt/internal/domain"
	"github.com/shadewright-labs/sablecourt/internal/gate/councilproxy"
	"github.com/shadewright-labs/sablecourt/internal/gate/model"
)

func TestCouncilProxyDocker(t *testing.T) {
	d := localProxyDocker(t)
	st := daemonTestStore(t)
	now := time.Now().UTC()
	ctx := t.Context()
	if err := st.UpsertWorkload(ctx, domain.WorkloadRecord{Name: "worker", Zone: domain.ZoneRuntime}); err != nil {
		t.Fatal(err)
	}
	newLease := func(id string, issued, expires time.Time) string {
		t.Helper()
		if err := st.CreateTask(ctx, domain.Task{
			ID: id, IdempotencyKey: id, WorkloadName: "worker", State: domain.TaskStateWorking,
			InputJSON: "{}", CreatedAt: issued, UpdatedAt: issued,
		}); err != nil {
			t.Fatal(err)
		}
		value, err := model.MintTaskTokenUntil(ctx, st, id, "worker", issued, expires)
		if err != nil {
			t.Fatal(err)
		}
		return value
	}
	valid := newLease("assigned", now, now.Add(time.Hour))
	wrong := newLease("other", now, now.Add(time.Hour))
	expired := newLease("expired", now.Add(-time.Hour), now.Add(-time.Minute))
	if err := st.AppendTaskMessage(ctx, domain.TaskMessage{
		TaskID: "assigned", Role: "agent", Content: "assigned response", CreatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}

	sockRoot := dockerTestSocketRoot(t)
	ln := proxyTestListener(t)
	daemonAddr := ln.Addr().String()
	daemon := daemonConfig{listenAddr: daemonAddr}
	mux := newHandler(st, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)
	h, err := daemon.hostHandler(mux, false)
	if err != nil {
		t.Fatal(err)
	}
	gateHandler, err := daemon.councilGateHandler(mux)
	if err != nil {
		t.Fatal(err)
	}
	daemonSrv, daemonDone := serveProxyTestClosable(t, ln, h)

	other := proxyTestListener(t)
	serveProxyTest(t, other, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	wildcard := proxyTestListenerAt(t, ":0")
	serveProxyTest(t, wildcard, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	_, gatePort, _ := net.SplitHostPort(daemonAddr)
	_, otherPort, _ := net.SplitHostPort(other.Addr().String())
	_, wildcardPort, _ := net.SplitHostPort(wildcard.Addr().String())
	hostURL := "http://" + net.JoinHostPort(councilproxy.DockerHostGateway, otherPort)
	wildcardHostURL := "http://" + net.JoinHostPort(councilproxy.DockerHostGateway, wildcardPort)
	image := d.build(t)

	start := func(token string) (*councilproxy.Session, councilproxy.WorkloadEndpoint) {
		t.Helper()
		s, err := councilproxy.Provision(ctx, d, image, councilproxy.Config{
			Harness: "claude", DaemonListen: daemonAddr, LeaseSHA256: model.HashToken(token),
			SocketRoot: sockRoot, GateHandler: gateHandler,
		})
		if s != nil {
			t.Cleanup(func() {
				d.keepProxyOnRemove.Store(false)
				if err := s.Close(context.Background()); err != nil {
					t.Error(err)
				}
			})
		}
		if err != nil {
			t.Fatal(err)
		}
		return s, s.Workload()
	}
	validSession, endpoint := start(valid)
	_, expiredEndpoint := start(expired)
	expiredProxyURL, err := url.Parse(expiredEndpoint.ProxyURL)
	if err != nil {
		t.Fatal(err)
	}
	proxyURL, err := url.Parse(endpoint.ProxyURL)
	if err != nil {
		t.Fatal(err)
	}

	ev := validSession.Evidence()
	t.Logf("probed council gate mode: %s", ev.GateMode)
	if ev.Network != endpoint.Network || ev.Proxy != proxyURL.Hostname() || ev.ProxyImageID != image ||
		ev.GateMode != endpoint.GateMode || (ev.GateMode != councilproxy.GateModeUnixSocket && ev.GateMode != councilproxy.GateModeHostGateway) {
		t.Fatalf("unexpected launch evidence: %+v endpoint=%+v", ev, endpoint)
	}
	mounts, extraHosts := d.inspectContainerIsolation(t, proxyURL.Hostname())
	switch ev.GateMode {
	case councilproxy.GateModeUnixSocket:
		if !slices.Contains(mounts, councilproxy.ContainerGateSocket) || len(mounts) != 1 || len(extraHosts) != 0 {
			t.Fatalf("unix-socket proxy container mounts=%v extraHosts=%v", mounts, extraHosts)
		}
	case councilproxy.GateModeHostGateway:
		if len(mounts) != 0 || !slices.Equal(extraHosts, []string{councilproxy.DockerHostGateway + ":host-gateway"}) {
			t.Fatalf("host-gateway proxy container mounts=%v extraHosts=%v", mounts, extraHosts)
		}
		if got := d.must(t, "", "exec", proxyURL.Hostname(), "/probe", hostURL, ""); !strings.HasPrefix(got, "204\n") {
			t.Fatalf("egress control returned %q", got)
		}
	}

	netInfo := d.inspectNetwork(t, endpoint.Network)
	if netInfo.Options[councilproxy.BridgeGatewayModeIPv4] != councilproxy.GatewayModeIsolated ||
		netInfo.Options[councilproxy.BridgeGatewayModeIPv6] != councilproxy.GatewayModeIsolated ||
		len(netInfo.Gateways) != 0 || len(netInfo.Subnets) == 0 {
		t.Fatalf("internal network is not isolated: info=%+v", netInfo)
	}
	internalGatewayIP := subnetDefaultGateway(t, netInfo.Subnets[0])
	internalGatewayWildcardURL := "http://" + net.JoinHostPort(internalGatewayIP, wildcardPort)

	peer := proxyURL.Hostname() + "-peer"
	t.Cleanup(func() {
		if err := d.removeLabelled(context.Background(), "container", peer); err != nil {
			t.Error(err)
		}
	})
	d.must(t, "", "run", "--detach", "--pull=never", "--name", peer, "--network", "bridge",
		"--add-host", councilproxy.DockerHostGateway+":host-gateway",
		"--label", councilproxy.ResourceLabel+"="+peer, "--user", "10001:10001", "--read-only",
		"--cap-drop", "ALL", "--security-opt", "no-new-privileges", "--entrypoint", "/probe", image, "serve")
	peerIP := strings.TrimSpace(d.must(t, "", "inspect", "--format", "{{.NetworkSettings.Networks.bridge.IPAddress}}", peer))
	if net.ParseIP(peerIP) == nil {
		t.Fatal("bridge peer has no IP address")
	}
	peerURL := "http://" + net.JoinHostPort(peerIP, "8080")
	if err := d.waitStatus(ctx, proxyURL.Hostname(), peerURL, "204\n"); err != nil {
		t.Fatalf("bridge peer control: %v", err)
	}
	// Control: the wildcard host listener is reachable from a normal bridge container via host-gateway.
	if got := d.must(t, "", "exec", peer, "/probe", wildcardHostURL, ""); !strings.HasPrefix(got, "204\n") {
		t.Fatalf("wildcard egress control from bridge peer returned %q", got)
	}

	// runHarnessProbe uses the caller's t so a Docker failure ends only that subtest.
	// With dial set the probe opens a TCP connection and reports whether it connected.
	runHarnessProbe := func(t *testing.T, token, target, proxy string, ep councilproxy.WorkloadEndpoint, dial bool) string {
		t.Helper()
		args := []string{"run", "--rm", "-i", "--pull=never",
			"--user", "10001:10001", "--read-only", "--cap-drop", "ALL", "--security-opt", "no-new-privileges",
			"--network", ep.Network, "--add-host", councilproxy.DockerHostGateway + ":host-gateway",
			"--entrypoint", "/probe", image, target, proxy}
		if dial {
			args = append(args, "dial")
		}
		return d.must(t, token, args...)
	}

	for _, tc := range []struct {
		name, token, resource, forward, want string
		endpoint                             councilproxy.WorkloadEndpoint
		dial                                 bool
	}{
		{"valid lease", valid, "/gate/task/messages", "", "200\n", endpoint, false},
		{"missing lease", "", "/gate/task/messages", "", "401\n", endpoint, false},
		{"wrong task's active lease", wrong, "/gate/task/messages", "", "401\n", endpoint, false},
		{"expired lease", expired, "/gate/task/messages", "", "401\n", expiredEndpoint, false},
		{"operator route", valid, "/api/v1/tasks", "", "403\n", endpoint, false},
		{"arbitrary host port through proxy", "", hostURL, "proxy", "403\n", endpoint, false},
		{"wildcard host port through proxy", "", wildcardHostURL, "proxy", "403\n", endpoint, false},
		{"direct egress to a bridge peer", "", peerURL, "", "unreachable\n", endpoint, true},
		{"direct egress to gate", valid, "http://" + net.JoinHostPort(councilproxy.DockerHostGateway, gatePort) + "/gate/task/messages", "", "unreachable\n", endpoint, true},
		{"direct egress to arbitrary host port", "", hostURL, "", "unreachable\n", endpoint, true},
		{"direct egress to wildcard host port", "", wildcardHostURL, "", "unreachable\n", endpoint, true},
		{"direct internal gateway to wildcard host port", "", internalGatewayWildcardURL, "", "unreachable\n", endpoint, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			target := tc.resource
			if strings.HasPrefix(target, "/") {
				target = tc.endpoint.ProxyURL + target
			}
			proxy := ""
			if tc.forward != "" {
				proxy = tc.endpoint.ProxyURL
			}
			got := runHarnessProbe(t, tc.token, target, proxy, tc.endpoint, tc.dial)
			if !strings.HasPrefix(got, tc.want) || (tc.want == "200\n" && !strings.Contains(got, "assigned response")) {
				t.Fatalf("probe returned %q, want prefix %q", got, tc.want)
			}
		})
	}
	inspectRaw := d.must(t, "", "inspect", proxyURL.Hostname())
	workloadEnv := strings.Join(append(endpoint.Env(), expiredEndpoint.Env()...), "\n")
	// scanForLeaks checks both proxy containers, the valid-lease one and the expired-lease one.
	scanForLeaks := func(t *testing.T) {
		t.Helper()
		for _, proxyHost := range []string{proxyURL.Hostname(), expiredProxyURL.Hostname()} {
			for label, data := range map[string]string{
				"docker inspect": d.must(t, "", "inspect", proxyHost),
				"docker logs":    d.must(t, "", "logs", proxyHost),
			} {
				for _, secretMaterial := range []string{valid, wrong, expired, "PRIVATE KEY", "-gate-secret", "-gate-launch-id", "op_"} {
					if strings.Contains(data, secretMaterial) {
						t.Fatalf("secret material %q appeared in %s of %s", secretMaterial, label, proxyHost)
					}
				}
			}
		}
		for _, secretMaterial := range []string{valid, wrong, expired, "PRIVATE KEY", "-gate-secret", "-gate-launch-id", "op_"} {
			if strings.Contains(workloadEnv, secretMaterial) {
				t.Fatalf("secret material %q appeared in workload env", secretMaterial)
			}
		}
	}
	scanForLeaks(t)
	proxyArgs, proxyEnv := decodeInspectArgsAndEnv(t, inspectRaw)
	allowedProxyFlags := map[string]bool{
		"-harness":          true,
		"-daemon-listen":    true,
		"-lease-sha256":     true,
		"-listen-name":      true,
		"-gate-socket":      true,
		"-gate-tls-port":    true,
		"-gate-spki-sha256": true,
	}
	var gateTLSPort string
	for i := 0; i < len(proxyArgs); i += 2 {
		flagName := proxyArgs[i]
		if !allowedProxyFlags[flagName] || i+1 >= len(proxyArgs) {
			t.Fatalf("unexpected or secret-carrying flag %q in proxy inspect args: %v", flagName, proxyArgs)
		}
		if flagName == "-gate-tls-port" {
			gateTLSPort = proxyArgs[i+1]
		}
	}
	for _, envEntry := range proxyEnv {
		name, _, _ := strings.Cut(envEntry, "=")
		upper := strings.ToUpper(name)
		if strings.Contains(upper, "SECRET") || strings.Contains(upper, "TOKEN") || strings.Contains(upper, "KEY") || strings.Contains(upper, "LEASE") {
			t.Fatalf("unexpected secret-like env var %q in proxy container inspect", name)
		}
	}

	// Regression for finding 1: daemon side goes away while proxy container survives,
	// and an unrelated listener takes the old upstream address and port.
	t.Run("substituted upstream after daemon loss receives zero credentials", func(t *testing.T) {
		d.keepProxyOnRemove.Store(true)
		_ = validSession.Close(ctx)
		_ = daemonSrv.Close()
		<-daemonDone

		replaceAddr := daemonAddr
		if ev.GateMode == councilproxy.GateModeHostGateway {
			if gateTLSPort == "" {
				t.Fatal("missing -gate-tls-port in host-gateway proxy args")
			}
			replaceAddr = net.JoinHostPort(net.IPv4(127, 0, 0, 1).String(), gateTLSPort)
		}

		var leakedCredentials atomic.Int32

		// 1. Plain TCP listener taking over the port receives zero credentials.
		plainRaw, err := net.Listen("tcp4", replaceAddr)
		if err != nil {
			t.Fatalf("bind plain substituted listener at %s: %v", replaceAddr, err)
		}
		plainLn := &acceptCountingListener{Listener: plainRaw}
		plainDone := make(chan struct{})
		go func() {
			defer close(plainDone)
			for {
				c, err := plainLn.Accept()
				if err != nil {
					return
				}
				_ = c.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
				buf, _ := io.ReadAll(io.LimitReader(c, 8192))
				if bytes.Contains(buf, []byte(valid)) || bytes.Contains(buf, []byte("Authorization")) {
					leakedCredentials.Add(1)
				}
				_ = c.Close()
			}
		}()
		gotPlain := runHarnessProbe(t, valid, endpoint.ProxyURL+"/gate/task/messages", "", endpoint, false)
		_ = plainLn.Close()
		<-plainDone
		if !strings.HasPrefix(gotPlain, "502\n") {
			t.Fatalf("surviving proxy against plain substituted listener returned %q, want 502", gotPlain)
		}

		// 2. Unverified TLS 1.3 listener with a fresh P-256 certificate taking over the port receives zero credentials.
		subCert := newTestSelfSignedP256Cert(t)
		subRaw, err := net.Listen("tcp4", replaceAddr)
		if err != nil {
			t.Fatalf("bind TLS substituted listener at %s: %v", replaceAddr, err)
		}
		subLn := &acceptCountingListener{Listener: subRaw}
		subSrv := &http.Server{
			ReadHeaderTimeout: time.Second,
			ErrorLog:          log.New(io.Discard, "", 0),
			Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(io.LimitReader(r.Body, 4096))
				if r.Header.Get("Authorization") != "" || strings.Contains(string(body), valid) {
					leakedCredentials.Add(1)
				}
				w.WriteHeader(http.StatusNoContent)
			}),
		}
		subDone := make(chan struct{})
		go func() {
			defer close(subDone)
			_ = subSrv.Serve(tls.NewListener(subLn, &tls.Config{
				Certificates: []tls.Certificate{subCert},
				MinVersion:   tls.VersionTLS13,
				MaxVersion:   tls.VersionTLS13,
			}))
		}()
		t.Cleanup(func() {
			_ = subSrv.Close()
			<-subDone
		})

		gotTLS := runHarnessProbe(t, valid, endpoint.ProxyURL+"/gate/task/messages", "", endpoint, false)
		if !strings.HasPrefix(gotTLS, "502\n") {
			t.Fatalf("surviving proxy against substituted TLS listener returned %q, want 502", gotTLS)
		}
		if leakedCredentials.Load() != 0 {
			t.Fatalf("substituted listeners received %d credential-bearing requests, want 0", leakedCredentials.Load())
		}
		// In host-gateway mode the proxy must actually have dialed each replacement and refused it;
		// in unix-socket mode it must never have fallen back to TCP.
		for name, accepts := range map[string]int32{"plain": plainLn.accepts.Load(), "TLS": subLn.accepts.Load()} {
			switch ev.GateMode {
			case councilproxy.GateModeHostGateway:
				if accepts == 0 {
					t.Fatalf("proxy never reached the %s replacement listener, so its rejection was not exercised", name)
				}
			case councilproxy.GateModeUnixSocket:
				if accepts != 0 {
					t.Fatalf("proxy fell back to TCP: %s replacement listener accepted %d connections", name, accepts)
				}
			}
		}
		// The failed upstream attempts themselves must not have logged a lease either.
		scanForLeaks(t)
	})
}

type acceptCountingListener struct {
	net.Listener
	accepts atomic.Int32
}

func (l *acceptCountingListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err == nil {
		l.accepts.Add(1)
	}
	return c, err
}

func decodeInspectArgsAndEnv(t *testing.T, raw string) (args []string, env []string) {
	t.Helper()
	var inspected []struct {
		Args   []string `json:"Args"`
		Config struct {
			Cmd []string `json:"Cmd"`
			Env []string `json:"Env"`
		} `json:"Config"`
	}
	if err := json.Unmarshal([]byte(raw), &inspected); err != nil || len(inspected) != 1 {
		t.Fatalf("decode proxy inspect args/env: %v", err)
	}
	if !slices.Equal(inspected[0].Args, inspected[0].Config.Cmd) {
		t.Fatalf("inspect Args %v != Config.Cmd %v", inspected[0].Args, inspected[0].Config.Cmd)
	}
	return inspected[0].Args, inspected[0].Config.Env
}

func newTestSelfSignedP256Cert(t *testing.T) tls.Certificate {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		NotBefore:             time.Now().Add(-time.Minute),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		DNSNames:              []string{councilproxy.Authority},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &priv.PublicKey, priv)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: priv}
}

func dockerTestSocketRoot(t *testing.T) string {
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

func proxyTestListener(t *testing.T) net.Listener {
	t.Helper()
	return proxyTestListenerAt(t, net.JoinHostPort(net.IPv4(127, 0, 0, 1).String(), "0"))
}

func proxyTestListenerAt(t *testing.T, addr string) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	return ln
}

func serveProxyTest(t *testing.T, ln net.Listener, h http.Handler) {
	t.Helper()
	_, _ = serveProxyTestClosable(t, ln, h)
}

func serveProxyTestClosable(t *testing.T, ln net.Listener, h http.Handler) (*http.Server, <-chan struct{}) {
	t.Helper()
	server := &http.Server{Handler: h, ReadHeaderTimeout: 5 * time.Second}
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := server.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			t.Error(err)
		}
	}()
	t.Cleanup(func() {
		_ = server.Close()
		<-done
	})
	return server, done
}

type proxyDockerCLI struct {
	executable, socket, configDir string
	keepProxyOnRemove             *atomic.Bool
}

var _ councilproxy.Docker = proxyDockerCLI{}

func localProxyDocker(t *testing.T) proxyDockerCLI {
	t.Helper()
	executable, err := exec.LookPath("docker")
	if err != nil {
		t.Skip("Docker unavailable: CLI not installed")
	}
	socket := os.Getenv("DOCKER_HOST")
	if socket == "" || os.Getenv("DOCKER_CONTEXT") != "" {
		ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
		defer cancel()
		out, err := exec.CommandContext(ctx, executable, "context", "inspect", "--format", "{{.Endpoints.docker.Host}}").Output()
		if err != nil {
			t.Skip("Docker unavailable: local context could not be resolved")
		}
		socket = strings.TrimSpace(string(out))
	}
	path, local := strings.CutPrefix(socket, "unix://")
	if !local || !filepath.IsAbs(path) {
		t.Skip("Docker unavailable: this test requires a local Unix socket")
	}
	conn, err := net.DialTimeout("unix", path, 2*time.Second)
	if err != nil {
		t.Skip("Docker unavailable: local Unix socket cannot be reached")
	}
	_ = conn.Close()
	d := proxyDockerCLI{
		executable:        executable,
		socket:            socket,
		configDir:         t.TempDir(),
		keepProxyOnRemove: new(atomic.Bool),
	}
	versions, err := d.run(t.Context(), "", "version", "--format", "{{.Client.Version}} {{.Server.Version}}")
	if err != nil {
		t.Skip("Docker unavailable: local engine did not answer")
	}
	// Isolated bridge gateways and "network connect --gw-priority" both arrived in Docker 28.
	for i, version := range strings.Fields(versions) {
		if major, ok := dockerMajorVersion(version); !ok || major < 28 {
			t.Skipf("Docker unavailable: %s version %q lacks isolated gateway and gw-priority support (needs 28 or newer)", []string{"CLI", "engine"}[min(i, 1)], version)
		}
	}
	return d
}

func dockerMajorVersion(version string) (int, bool) {
	digits, _, _ := strings.Cut(strings.TrimSpace(version), ".")
	major, err := strconv.Atoi(digits)
	return major, err == nil
}

func TestDockerMajorVersion(t *testing.T) {
	for _, tc := range []struct {
		in    string
		major int
		ok    bool
	}{
		{"27.5.1", 27, true},
		{"28.0.0", 28, true},
		{"29.9.3-ce", 29, true},
		{"", 0, false},
		{"unknown", 0, false},
	} {
		if major, ok := dockerMajorVersion(tc.in); major != tc.major || ok != tc.ok {
			t.Errorf("dockerMajorVersion(%q) = %d, %v, want %d, %v", tc.in, major, ok, tc.major, tc.ok)
		}
	}
}

func (d proxyDockerCLI) run(ctx context.Context, input string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, d.executable, append([]string{"--config", d.configDir, "--host", d.socket}, args...)...)
	cmd.Env = append(os.Environ(), "DOCKER_CONTEXT=")
	cmd.Stdin = strings.NewReader(input)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("docker %s: %w: %s", args[0], err, out)
	}
	return string(out), nil
}

func (d proxyDockerCLI) must(t *testing.T, input string, args ...string) string {
	t.Helper()
	out, err := d.run(t.Context(), input, args...)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func (d proxyDockerCLI) build(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	source := filepath.Join("..", "..", "internal", "gate", "councilproxy")
	probeFile := filepath.Join(dir, "probe.go")
	if err := os.WriteFile(probeFile, []byte(dockerProbeSource), 0o600); err != nil {
		t.Fatal(err)
	}
	arch := strings.TrimSpace(d.must(t, "", "version", "--format", "{{.Server.Arch}}"))
	for _, binary := range []struct{ input, output string }{
		{filepath.Join(source, "proxy_main.go"), "council-proxy"},
		{probeFile, "probe"},
	} {
		cmd := exec.CommandContext(t.Context(), "go", "build", "-o", filepath.Join(dir, binary.output), binary.input)
		cmd.Env = append(os.Environ(), "GOOS=linux", "GOARCH="+arch, "CGO_ENABLED=0", "GOPROXY=off")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("build %s: %v: %s", binary.output, err, out)
		}
	}
	dockerfile, err := os.ReadFile(filepath.Join(source, "Dockerfile"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), append(dockerfile, "\nCOPY probe /probe\n"...), 0o600); err != nil {
		t.Fatal(err)
	}
	idfile := filepath.Join(dir, "image-id")
	d.must(t, "", "build", "--network", "none", "--pull=false", "--iidfile", idfile, dir)
	data, err := os.ReadFile(idfile)
	if err != nil {
		t.Fatal(err)
	}
	image := strings.TrimSpace(string(data))
	t.Cleanup(func() {
		if _, err := d.run(context.Background(), "", "image", "rm", image); err != nil {
			t.Error(err)
		}
	})
	return image
}

func (d proxyDockerCLI) ProbeGateSocket(ctx context.Context, spec councilproxy.GateSocketProbe) error {
	args := []string{
		"run", "--rm", "--pull=never", "--network", "none",
		"--label", councilproxy.ResourceLabel + "=" + spec.Name,
		"--user", spec.User,
		"--mount", "type=bind,src=" + spec.HostSocketPath + ",dst=" + spec.ContainerSocketPath,
	}
	if spec.ReadOnlyRootFS {
		args = append(args, "--read-only")
	}
	if spec.DropAllCapabilities {
		args = append(args, "--cap-drop", "ALL")
	}
	if spec.NoNewPrivileges {
		args = append(args, "--security-opt", "no-new-privileges")
	}
	args = append(append(args, spec.ImageID), spec.Args...)
	_, err := d.run(ctx, "", args...)
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		if exitErr.ExitCode() == councilproxy.ProbeSocketUnsupportedExitCode ||
			councilproxy.IsUnshareableSocketBindError(spec.HostSocketPath, exitErr.ExitCode(), err.Error()) {
			return councilproxy.ErrGateSocketUnsupported
		}
	}
	return err
}

func (d proxyDockerCLI) CreateInternalNetwork(ctx context.Context, spec councilproxy.InternalNetworkSpec) (councilproxy.InternalNetworkInfo, error) {
	args := []string{"network", "create", "--driver", "bridge", "--internal", "--label", councilproxy.ResourceLabel + "=" + spec.Name}
	keys := make([]string, 0, len(spec.Options))
	for k := range spec.Options {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		args = append(args, "--opt", k+"="+spec.Options[k])
	}
	args = append(args, spec.Name)
	if _, err := d.run(ctx, "", args...); err != nil {
		return councilproxy.InternalNetworkInfo{}, err
	}
	return d.networkState(ctx, spec.Name)
}

func (d proxyDockerCLI) inspectNetwork(t *testing.T, name string) councilproxy.InternalNetworkInfo {
	t.Helper()
	info, err := d.networkState(t.Context(), name)
	if err != nil {
		t.Fatal(err)
	}
	return info
}

func (d proxyDockerCLI) networkState(ctx context.Context, name string) (councilproxy.InternalNetworkInfo, error) {
	ver, err := d.run(ctx, "", "version", "--format", "{{.Server.Version}}")
	if err != nil {
		return councilproxy.InternalNetworkInfo{}, err
	}
	raw, err := d.run(ctx, "", "network", "inspect", name)
	if err != nil {
		return councilproxy.InternalNetworkInfo{}, err
	}
	var inspected []struct {
		Driver   string            `json:"Driver"`
		Internal bool              `json:"Internal"`
		Options  map[string]string `json:"Options"`
		IPAM     struct {
			Config []struct {
				Subnet  string `json:"Subnet"`
				Gateway string `json:"Gateway"`
			} `json:"Config"`
		} `json:"IPAM"`
	}
	if err := json.Unmarshal([]byte(raw), &inspected); err != nil || len(inspected) != 1 {
		return councilproxy.InternalNetworkInfo{}, fmt.Errorf("decode network inspect: %w", err)
	}
	info := councilproxy.InternalNetworkInfo{
		EngineVersion: strings.TrimSpace(ver),
		Driver:        inspected[0].Driver,
		Internal:      inspected[0].Internal,
		Options:       inspected[0].Options,
	}
	for _, cfg := range inspected[0].IPAM.Config {
		if s := strings.TrimSpace(cfg.Subnet); s != "" {
			info.Subnets = append(info.Subnets, s)
		}
		if g := strings.TrimSpace(cfg.Gateway); g != "" {
			info.Gateways = append(info.Gateways, g)
		}
	}
	return info, nil
}

func (d proxyDockerCLI) inspectContainerIsolation(t *testing.T, name string) (mounts []string, extraHosts []string) {
	t.Helper()
	raw := d.must(t, "", "inspect", name)
	var inspected []struct {
		Mounts []struct {
			Destination string `json:"Destination"`
		} `json:"Mounts"`
		HostConfig struct {
			ExtraHosts []string `json:"ExtraHosts"`
		} `json:"HostConfig"`
	}
	if err := json.Unmarshal([]byte(raw), &inspected); err != nil || len(inspected) != 1 {
		t.Fatalf("decode container inspect: %v", err)
	}
	for _, m := range inspected[0].Mounts {
		mounts = append(mounts, m.Destination)
	}
	return mounts, inspected[0].HostConfig.ExtraHosts
}

func subnetDefaultGateway(t *testing.T, subnet string) string {
	t.Helper()
	prefix, err := netip.ParsePrefix(subnet)
	if err != nil {
		t.Fatalf("parse subnet %q: %v", subnet, err)
	}
	gw := prefix.Masked().Addr().Next()
	if !gw.IsValid() || !prefix.Contains(gw) {
		t.Fatalf("subnet %q has no usable gateway candidate", subnet)
	}
	return gw.String()
}

func (d proxyDockerCLI) CreateProxy(ctx context.Context, spec councilproxy.ProxyContainer) error {
	args := []string{"create", "--pull=never", "--name", spec.Name, "--network", spec.InternalNetwork,
		"--network-alias", spec.Name, "--label", councilproxy.ResourceLabel + "=" + spec.Name,
		"--user", spec.User}
	if spec.HostGateway != "" && spec.HostGatewayTarget != "" {
		args = append(args, "--add-host", spec.HostGateway+":"+spec.HostGatewayTarget)
	}
	if spec.GateSocketHostPath != "" || spec.GateSocketContainerPath != "" {
		args = append(args, "--mount", "type=bind,src="+spec.GateSocketHostPath+",dst="+spec.GateSocketContainerPath)
	}
	if spec.ReadOnlyRootFS {
		args = append(args, "--read-only")
	}
	if spec.DropAllCapabilities {
		args = append(args, "--cap-drop", "ALL")
	}
	if spec.NoNewPrivileges {
		args = append(args, "--security-opt", "no-new-privileges")
	}
	_, err := d.run(ctx, "", append(append(args, spec.ImageID), spec.Args...)...)
	return err
}

func (d proxyDockerCLI) ConnectProxyEgress(ctx context.Context, name string) error {
	_, err := d.run(ctx, "", "network", "connect", "--gw-priority", "1", "bridge", name)
	return err
}

func (d proxyDockerCLI) StartProxy(ctx context.Context, name string) error {
	if _, err := d.run(ctx, "", "start", name); err != nil {
		return err
	}
	gate := "http://" + net.JoinHostPort(name, councilproxy.Port) + "/gate/task/messages"
	return d.waitStatus(ctx, name, gate, "401\n")
}

func (d proxyDockerCLI) waitStatus(ctx context.Context, name, target, status string) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	tick := time.NewTicker(200 * time.Millisecond)
	defer tick.Stop()
	for {
		if out, err := d.run(ctx, "", "exec", name, "/probe", target, ""); err == nil && strings.HasPrefix(out, status) {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("probe did not reach status %s: %w", strings.TrimSpace(status), ctx.Err())
		case <-tick.C:
		}
	}
}

func (d proxyDockerCLI) RemoveProxy(ctx context.Context, name string) error {
	if d.keepProxyOnRemove != nil && d.keepProxyOnRemove.Load() {
		return errors.New("simulated proxy container removal failure")
	}
	return d.removeLabelled(ctx, "container", name)
}

func (d proxyDockerCLI) RemoveInternalNetwork(ctx context.Context, name string) error {
	return d.removeLabelled(ctx, "network", name)
}

func (d proxyDockerCLI) removeLabelled(ctx context.Context, kind, name string) error {
	args := []string{kind, "ls", "--filter", "label=" + councilproxy.ResourceLabel + "=" + name, "--format", "{{.ID}}"}
	if kind == "container" {
		args = append(args, "--all")
	}
	out, err := d.run(ctx, "", args...)
	if err != nil || strings.TrimSpace(out) == "" {
		return err
	}
	ids := strings.Fields(out)
	if len(ids) != 1 {
		return errors.New("multiple Docker resources matched the council proxy label")
	}
	args = []string{kind, "rm"}
	if kind == "container" {
		args = append(args, "--force")
	}
	_, err = d.run(ctx, "", append(args, ids[0])...)
	return err
}

const dockerProbeSource = `package main

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

func main() {
	if len(os.Args) == 2 && os.Args[1] == "serve" {
		server := &http.Server{Addr: ":8080", ReadHeaderTimeout: time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNoContent)
		})}
		if server.ListenAndServe() != nil {
			os.Exit(1)
		}
		return
	}
	if len(os.Args) == 4 && os.Args[3] == "dial" {
		target, err := url.Parse(os.Args[1])
		if err != nil {
			os.Exit(1)
		}
		conn, err := net.DialTimeout("tcp", target.Host, 2*time.Second)
		if err != nil {
			fmt.Printf("unreachable\n%v\n", err)
			return
		}
		conn.Close()
		fmt.Println("connected")
		return
	}
	token, err := io.ReadAll(io.LimitReader(os.Stdin, 512))
	if err != nil {
		os.Exit(1)
	}
	transport := &http.Transport{DialContext: (&net.Dialer{Timeout: 2*time.Second}).DialContext}
	if os.Args[2] != "" {
		proxy, err := url.Parse(os.Args[2])
		if err != nil {
			os.Exit(1)
		}
		transport.Proxy = http.ProxyURL(proxy)
	}
	client := &http.Client{Transport: transport, Timeout: 4*time.Second}
	req, err := http.NewRequest("GET", os.Args[1], nil)
	if err != nil {
		os.Exit(1)
	}
	if len(token) != 0 {
		req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(token)))
	}
	res, err := client.Do(req)
	if err != nil {
		fmt.Printf("unreachable\n%v\n", err)
		return
	}
	defer res.Body.Close()
	fmt.Println(res.StatusCode)
	_, _ = io.Copy(os.Stdout, io.LimitReader(res.Body, 8192))
}
`
