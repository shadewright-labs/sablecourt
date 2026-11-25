package councilproxy

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

type fakeDocker struct {
	fail, cleanupFail string
	calls             []string
	network, proxy    string
}

func (f *fakeDocker) step(step string) error {
	f.calls = append(f.calls, step)
	if step == f.fail || step == f.cleanupFail {
		return errors.New("injected " + step)
	}
	return nil
}

func (f *fakeDocker) CreateInternalNetwork(_ context.Context, name string) error {
	f.network = name
	return f.step("network")
}

func (f *fakeDocker) CreateProxy(_ context.Context, spec ProxyContainer) error {
	f.proxy = spec.Name
	if spec.InternalNetwork != f.network || spec.ImageID != imageID || spec.User != "10001:10001" ||
		!spec.ReadOnlyRootFS || !spec.DropAllCapabilities || !spec.NoNewPrivileges ||
		spec.HostGateway != DockerHostGateway || !reflect.DeepEqual(spec.Args, []string{
		"-harness", "codex", "-daemon-listen", loopbackAddress(), "-lease-sha256", leaseDigest,
		"-listen-name", spec.Name,
	}) {
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
		{"success", "", "", []string{"network", "container", "egress", "start"}},
		{"network creation may commit", "network", "", []string{"network", "remove network"}},
		{"proxy creation may commit", "container", "", []string{"network", "container", "remove container", "remove network"}},
		{"connect failure", "egress", "", []string{"network", "container", "egress", "remove container", "remove network"}},
		{"start failure", "start", "", []string{"network", "container", "egress", "start", "remove container", "remove network"}},
		{"proxy cleanup needs retry", "start", "remove container", []string{"network", "container", "egress", "start", "remove container"}},
		{"network cleanup needs retry", "start", "remove network", []string{"network", "container", "egress", "start", "remove container", "remove network"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			engine := &fakeDocker{fail: tc.fail, cleanupFail: tc.cleanupFail}
			cfg := Config{Harness: "codex", DaemonListen: loopbackAddress(), LeaseSHA256: leaseDigest}
			session, err := Provision(t.Context(), engine, imageID, cfg)
			if (err != nil) != (tc.fail != "") || !reflect.DeepEqual(engine.calls, tc.want) {
				t.Fatalf("provision error %v, calls %v, want %v", err, engine.calls, tc.want)
			}
			if tc.fail == "" {
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
		name, image, harness string
	}{
		{"remote image tag", "registry/repo:tag", "codex"},
		{"invalid image id", "sha256:abc", "codex"},
		{"invalid harness", imageID, "other"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			engine := new(fakeDocker)
			session, err := Provision(t.Context(), engine, tc.image, Config{Harness: tc.harness, DaemonListen: loopbackAddress()})
			if session != nil || err == nil || len(engine.calls) != 0 {
				t.Fatalf("invalid config reached Docker: %v, %v", err, engine.calls)
			}
		})
	}
}
