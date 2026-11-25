package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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

	ln := proxyTestListener(t)
	daemon := daemonConfig{listenAddr: ln.Addr().String()}
	h, err := daemon.hostHandler(newHandler(st, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil), false)
	if err != nil {
		t.Fatal(err)
	}
	serveProxyTest(t, ln, h)
	other := proxyTestListener(t)
	serveProxyTest(t, other, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	_, gatePort, _ := net.SplitHostPort(ln.Addr().String())
	_, otherPort, _ := net.SplitHostPort(other.Addr().String())
	hostURL := "http://" + net.JoinHostPort(councilproxy.DockerHostGateway, otherPort)
	image := d.build(t)

	start := func(token string) councilproxy.WorkloadEndpoint {
		t.Helper()
		s, err := councilproxy.Provision(ctx, d, image, councilproxy.Config{
			Harness: "claude", DaemonListen: ln.Addr().String(), LeaseSHA256: model.HashToken(token),
		})
		if s != nil {
			t.Cleanup(func() {
				if err := s.Close(context.Background()); err != nil {
					t.Error(err)
				}
			})
		}
		if err != nil {
			t.Fatal(err)
		}
		return s.Workload()
	}
	endpoint := start(valid)
	expiredEndpoint := start(expired)
	proxyURL, err := url.Parse(endpoint.ProxyURL)
	if err != nil {
		t.Fatal(err)
	}
	// This control establishes that the other loopback listener is live from the egress side.
	if got := d.must(t, "", "exec", proxyURL.Hostname(), "/probe", hostURL, ""); !strings.HasPrefix(got, "204\n") {
		t.Fatalf("egress control returned %q", got)
	}
	peer := proxyURL.Hostname() + "-peer"
	t.Cleanup(func() {
		if err := d.removeLabelled(context.Background(), "container", peer); err != nil {
			t.Error(err)
		}
	})
	d.must(t, "", "run", "--detach", "--pull=never", "--name", peer, "--network", "bridge",
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

	for _, tc := range []struct {
		name, token, resource, forward, want string
		endpoint                             councilproxy.WorkloadEndpoint
	}{
		{"valid lease", valid, "/gate/task/messages", "", "200\n", endpoint},
		{"missing lease", "", "/gate/task/messages", "", "401\n", endpoint},
		{"wrong task's active lease", wrong, "/gate/task/messages", "", "401\n", endpoint},
		{"expired lease", expired, "/gate/task/messages", "", "401\n", expiredEndpoint},
		{"operator route", valid, "/api/v1/tasks", "", "403\n", endpoint},
		{"arbitrary host port through proxy", "", hostURL, "proxy", "403\n", endpoint},
		{"direct egress to a bridge peer", "", peerURL, "", "unreachable\n", endpoint},
		{"direct egress to gate", valid, "http://" + net.JoinHostPort(councilproxy.DockerHostGateway, gatePort) + "/gate/task/messages", "", "unreachable\n", endpoint},
		{"direct egress to arbitrary host port", "", hostURL, "", "unreachable\n", endpoint},
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
			got := d.must(t, tc.token, "run", "--rm", "-i", "--pull=never",
				"--user", "10001:10001", "--read-only", "--cap-drop", "ALL", "--security-opt", "no-new-privileges",
				"--network", tc.endpoint.Network, "--add-host", councilproxy.DockerHostGateway+":host-gateway",
				"--entrypoint", "/probe", image, target, proxy)
			if !strings.HasPrefix(got, tc.want) || (tc.want == "200\n" && !strings.Contains(got, "assigned response")) {
				t.Fatalf("probe returned %q, want prefix %q", got, tc.want)
			}
		})
	}
	for _, args := range [][]string{{"inspect", proxyURL.Hostname()}, {"logs", proxyURL.Hostname()}} {
		data := d.must(t, "", args...)
		if strings.Contains(data, valid) || strings.Contains(data, wrong) || strings.Contains(data, expired) {
			t.Fatalf("raw task lease appeared in docker %s", args[0])
		}
	}
}

func proxyTestListener(t *testing.T) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp4", net.JoinHostPort(net.IPv4(127, 0, 0, 1).String(), "0"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	return ln
}

func serveProxyTest(t *testing.T, ln net.Listener, h http.Handler) {
	t.Helper()
	server := &http.Server{Handler: h, ReadHeaderTimeout: 5 * time.Second}
	done := make(chan error, 1)
	go func() { done <- server.Serve(ln) }()
	t.Cleanup(func() {
		_ = server.Close()
		if err := <-done; err != nil && !errors.Is(err, http.ErrServerClosed) {
			t.Error(err)
		}
	})
}

type proxyDockerCLI struct {
	executable, socket, configDir string
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
	d := proxyDockerCLI{executable: executable, socket: socket, configDir: t.TempDir()}
	if _, err := d.run(t.Context(), "", "version", "--format", "{{.Server.Version}}"); err != nil {
		t.Skip("Docker unavailable: local engine did not answer")
	}
	return d
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

func (d proxyDockerCLI) CreateInternalNetwork(ctx context.Context, name string) error {
	_, err := d.run(ctx, "", "network", "create", "--driver", "bridge", "--internal", "--label", councilproxy.ResourceLabel+"="+name, name)
	return err
}

func (d proxyDockerCLI) CreateProxy(ctx context.Context, spec councilproxy.ProxyContainer) error {
	args := []string{"create", "--pull=never", "--name", spec.Name, "--network", spec.InternalNetwork,
		"--network-alias", spec.Name, "--label", councilproxy.ResourceLabel + "=" + spec.Name,
		"--user", spec.User, "--add-host", spec.HostGateway + ":host-gateway"}
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
		fmt.Println("unreachable")
		return
	}
	defer res.Body.Close()
	fmt.Println(res.StatusCode)
	_, _ = io.Copy(os.Stdout, io.LimitReader(res.Body, 8192))
}
`
