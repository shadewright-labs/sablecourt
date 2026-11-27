package docker

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"

	"github.com/shadewright-labs/sablecourt/internal/gate/councilproxy"
	rt "github.com/shadewright-labs/sablecourt/internal/runtime"
)

// countingProxyEngine counts every container create the proxy fake receives.
type countingProxyEngine struct {
	*readinessEngine
	creates int
}

func (c *countingProxyEngine) ContainerCreate(ctx context.Context, o client.ContainerCreateOptions) (client.ContainerCreateResult, error) {
	c.creates++
	return c.readinessEngine.ContainerCreate(ctx, o)
}

func countCreates(calls []string) int {
	n := 0
	for _, c := range calls {
		if c == "create" {
			n++
		}
	}
	return n
}

// TestRemoteEngineCreatesNoContainer covers the remote engine row of the
// decision on issue 181: no container of any kind is created on an engine
// that is not reached over a local Unix socket, whichever entry point asks.
func TestRemoteEngineCreatesNoContainer(t *testing.T) {
	for _, host := range []string{"tcp://engine.invalid:2376", "ssh://engine.invalid"} {
		for _, tc := range []struct {
			name string
			run  func(t *testing.T) (int, error)
		}{
			{name: "login probe", run: func(t *testing.T) (int, error) {
				a, p := probeFixture("claude")
				err := a.CheckCouncilHarness(t.Context(), "claude")
				return len(p.created), err
			}},
			{name: "tool install", run: func(t *testing.T) (int, error) {
				a, p := probeFixture("claude")
				cfg := a.CouncilHarnesses["claude"]
				cfg.Tool = fixtureTool()
				a.CouncilHarnesses["claude"] = cfg
				err := a.InstallCouncilTool(t.Context(), "claude", strings.NewReader(fixtureToolBody))
				return len(p.created), err
			}},
			{name: "gate socket probe", run: func(t *testing.T) (int, error) {
				e := &socketProbeEngine{councilEngine: newCouncilEngine()}
				a := NewAdapter(e, "")
				err := a.ProbeGateSocket(t.Context(), councilproxy.GateSocketProbe{Name: "probe", ImageID: e.imageID, User: "10001:10001", HostSocketPath: "/source.sock", ContainerSocketPath: councilproxy.ContainerGateSocket})
				return countCreates(e.calls), err
			}},
			{name: "proxy container", run: func(t *testing.T) (int, error) {
				e := &countingProxyEngine{readinessEngine: &readinessEngine{fakeProxyEngine: newFakeProxyEngine()}}
				e.nets["private"] = network.Network{ID: "net-private", Name: "private", Internal: true, Driver: "bridge", Labels: map[string]string{councilproxy.ResourceLabel: "private"}}
				a := NewAdapter(e, "")
				err := a.CreateProxy(t.Context(), councilproxy.ProxyContainer{Name: "proxy", ImageID: e.imageID, InternalNetwork: "private", InternalNetworkID: "net-private"})
				return e.creates, err
			}},
			{name: "proxy provisioning", run: func(t *testing.T) (int, error) {
				e := &countingProxyEngine{readinessEngine: &readinessEngine{fakeProxyEngine: newFakeProxyEngine()}}
				a := NewAdapter(e, "")
				_, err := a.ProvisionCouncilProxy(t.Context(), e.imageID, councilproxy.Config{Harness: "claude", DaemonListen: net.JoinHostPort(net.IPv6loopback.String(), "7432")})
				return e.creates, err
			}},
			{name: "council turn", run: func(t *testing.T) (int, error) {
				host := os.Getenv("DOCKER_HOST")
				m := newMountRuleFixture(t, rt.CouncilTask) // isolateHome clears DOCKER_HOST
				t.Setenv("DOCKER_HOST", host)
				_, err := m.a.RunCouncil(t.Context(), m.s, discardResult)
				return countCreates(m.f.calls), err
			}},
			{name: "workload container", run: func(t *testing.T) (int, error) {
				f := &inspectEngine{net: network.Network{Name: DefaultNetworkName, ID: "net-id", Driver: "bridge", Internal: true}, imageID: "sha256:" + strings.Repeat("a", 64)}
				spec := rt.LaunchSpec{TaskID: "task"}
				spec.Workload.Spec.Runtime.Image = "fixture"
				_, err := NewAdapter(f, "").Start(t.Context(), spec)
				n := 0
				if f.createdOpts.Config != nil {
					n = 1
				}
				return n, err
			}},
		} {
			t.Run(host+"/"+tc.name, func(t *testing.T) {
				t.Setenv("DOCKER_HOST", host)
				creates, err := tc.run(t)
				if creates != 0 {
					t.Fatalf("created %d containers on a remote engine (err = %v)", creates, err)
				}
				if err == nil || !strings.Contains(err.Error(), "remote engine") {
					t.Fatalf("err = %v, want the remote engine refusal", err)
				}
			})
		}
	}
}

// TestContainerCreateHasOneCaller keeps the remote engine rule in one place:
// only createContainer may call the engine's ContainerCreate, so a new entry
// point cannot create a container without the check.
func TestContainerCreateHasOneCaller(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		f, err := parser.ParseFile(fset, name, src, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				sel, ok := n.(*ast.SelectorExpr)
				if ok && sel.Sel.Name == "ContainerCreate" && fn.Name.Name != "createContainer" {
					t.Errorf("%s: %s calls ContainerCreate directly; use createContainer", fset.Position(sel.Pos()), fn.Name.Name)
				}
				return true
			})
		}
	}
}
