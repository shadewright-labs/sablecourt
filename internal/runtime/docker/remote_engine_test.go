package docker

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"

	"github.com/shadewright-labs/sablecourt/internal/gate/councilproxy"
	rt "github.com/shadewright-labs/sablecourt/internal/runtime"
)

// The fakes below report a daemon host the way the Moby client does, so the
// adapter sees a client that is already pointed at that engine.
type hostedProbe struct {
	*probeEngine
	host string
}

func (h hostedProbe) DaemonHost() string { return h.host }

// hostedProxy also counts every call that would change the engine, since a
// failed launch removes the network it made.
type hostedProxy struct {
	*readinessEngine
	host  string
	calls int
}

func (h *hostedProxy) DaemonHost() string { return h.host }
func (h *hostedProxy) NetworkCreate(ctx context.Context, name string, o client.NetworkCreateOptions) (client.NetworkCreateResult, error) {
	h.calls++
	return h.readinessEngine.NetworkCreate(ctx, name, o)
}
func (h *hostedProxy) NetworkConnect(ctx context.Context, id string, o client.NetworkConnectOptions) (client.NetworkConnectResult, error) {
	h.calls++
	return h.readinessEngine.NetworkConnect(ctx, id, o)
}
func (h *hostedProxy) ContainerCreate(ctx context.Context, o client.ContainerCreateOptions) (client.ContainerCreateResult, error) {
	h.calls++
	return h.readinessEngine.ContainerCreate(ctx, o)
}
func (h *hostedProxy) ContainerStart(ctx context.Context, id string, o client.ContainerStartOptions) (client.ContainerStartResult, error) {
	h.calls++
	return h.readinessEngine.ContainerStart(ctx, id, o)
}
func (h *hostedProxy) ExecCreate(ctx context.Context, id string, o client.ExecCreateOptions) (client.ExecCreateResult, error) {
	h.calls++
	return h.readinessEngine.ExecCreate(ctx, id, o)
}

type hostedFake struct {
	*fakeEngine
	host string
}

func (h hostedFake) DaemonHost() string { return h.host }

var remoteHosts = []string{"tcp://engine.invalid:2376", "ssh://engine.invalid"}

// TestLocalEngineHost is the table behind every constructor: only a local
// Unix socket or a Windows named pipe is a local engine.
func TestLocalEngineHost(t *testing.T) {
	for host, local := range map[string]bool{
		"":                                  true,
		"unix:///var/run/docker.sock":       true,
		"npipe:////./pipe/docker_engine":    true,
		"tcp://engine.invalid:2376":         false,
		"tcp://127.0.0.1:2375":              false,
		"ssh://engine.invalid":              false,
		"http://engine.invalid":             false,
		"https://engine.invalid":            false,
		"fd://":                             false,
		"/var/run/docker.sock":              false,
		"unix":                              false,
		"UNIX:///var/run/docker.sock":       false,
		"engine.invalid:2376":               false,
		"://":                               false,
		"unix://":                           false,
		"npipe://":                          false,
		"tcp://engine.invalid:2376/unix://": false,
	} {
		if err := checkLocalEngine(host); (err == nil) != local {
			t.Errorf("checkLocalEngine(%q) = %v, want local=%t", host, err, local)
		}
	}
}

// TestConstructorsRefuseRemoteEngine checks that no Docker adapter exists on a
// remote engine: NewFromEnv refuses a remote DOCKER_HOST, and NewAdapter
// refuses both a remote DOCKER_HOST and an injected client whose DaemonHost is
// remote.
func TestConstructorsRefuseRemoteEngine(t *testing.T) {
	for _, host := range remoteHosts {
		t.Run("NewFromEnv/"+host, func(t *testing.T) {
			t.Setenv("DOCKER_HOST", host)
			a, err := NewFromEnv()
			if a != nil || err == nil || !strings.Contains(err.Error(), "remote engine") {
				t.Fatalf("NewFromEnv() = %v, %v, want the remote engine refusal", a, err)
			}
		})
		t.Run("NewAdapter DOCKER_HOST/"+host, func(t *testing.T) {
			t.Setenv("DOCKER_HOST", host)
			a, err := NewAdapter(&fakeEngine{}, "")
			if a != nil || err == nil || !strings.Contains(err.Error(), "remote engine") {
				t.Fatalf("NewAdapter() = %v, %v, want the remote engine refusal", a, err)
			}
		})
		t.Run("NewAdapter DaemonHost/"+host, func(t *testing.T) {
			t.Setenv("DOCKER_HOST", "")
			a, err := NewAdapter(hostedFake{fakeEngine: &fakeEngine{}, host: host}, "")
			if a != nil || err == nil || !strings.Contains(err.Error(), "remote engine") {
				t.Fatalf("NewAdapter() = %v, %v, want the remote engine refusal", a, err)
			}
		})
	}
	t.Run("local engines", func(t *testing.T) {
		t.Setenv("DOCKER_HOST", "unix:///nonexistent/docker.sock")
		if a, err := NewFromEnv(); err != nil || a == nil {
			t.Fatalf("NewFromEnv() on a Unix socket = %v, %v", a, err)
		}
		for _, host := range []string{"unix:///nonexistent/docker.sock", "npipe:////./pipe/docker_engine"} {
			if a, err := NewAdapter(hostedFake{fakeEngine: &fakeEngine{}, host: host}, ""); err != nil || a == nil {
				t.Fatalf("NewAdapter() on %s = %v, %v", host, a, err)
			}
		}
		if a, err := NewAdapter(nil, ""); err != nil || a == nil {
			t.Fatalf("NewAdapter(nil) = %v, %v", a, err)
		}
	})
}

// TestRemoteEngineReachesNothing covers the calls the second review found
// still reaching a remote engine after the container create check: the tool
// and login volumes, the proxy and workload networks, Resume and StartProxy.
// With the refusal in the constructor, none of them has an adapter to run on,
// so the engine records no create, connect, start or exec.
func TestRemoteEngineReachesNothing(t *testing.T) {
	for _, host := range remoteHosts {
		for _, tc := range []struct {
			name string
			run  func(t *testing.T) (changes int, err error)
		}{
			{name: "tool install volume", run: func(t *testing.T) (int, error) {
				local, p := probeFixture("claude")
				a, err := NewAdapter(hostedProbe{probeEngine: p, host: host}, "")
				if err == nil {
					cfg := local.CouncilHarnesses["claude"]
					cfg.Tool = fixtureTool()
					a.CouncilHarnesses = map[string]CouncilHarness{"claude": cfg}
					err = a.InstallCouncilTool(t.Context(), "claude", strings.NewReader(fixtureToolBody))
				}
				return len(p.volCreate) + len(p.created), err
			}},
			{name: "login volume", run: func(t *testing.T) (int, error) {
				local, p := probeFixture("claude")
				clear(p.volumes)
				a, err := NewAdapter(hostedProbe{probeEngine: p, host: host}, "")
				if err == nil {
					a.CouncilHarnesses = local.CouncilHarnesses
					err = a.EnsureCouncilLoginVolume(t.Context(), "claude")
				}
				return len(p.volCreate) + len(p.created), err
			}},
			{name: "login probe", run: func(t *testing.T) (int, error) {
				local, p := probeFixture("claude")
				a, err := NewAdapter(hostedProbe{probeEngine: p, host: host}, "")
				if err == nil {
					a.CouncilHarnesses = local.CouncilHarnesses
					err = a.CheckCouncilHarness(t.Context(), "claude")
				}
				return len(p.volCreate) + len(p.created), err
			}},
			{name: "proxy network", run: func(t *testing.T) (int, error) {
				h := &hostedProxy{readinessEngine: &readinessEngine{fakeProxyEngine: newFakeProxyEngine()}, host: host}
				a, err := NewAdapter(h, "")
				if err == nil {
					_, err = a.ProvisionCouncilProxy(t.Context(), h.imageID, councilproxy.Config{Harness: "claude", DaemonListen: net.JoinHostPort(net.IPv6loopback.String(), "7432")})
				}
				return h.calls, err
			}},
			{name: "workload network", run: func(t *testing.T) (int, error) {
				f := &fakeEngine{}
				a, err := NewAdapter(hostedFake{fakeEngine: f, host: host}, "")
				if err == nil {
					spec := rt.LaunchSpec{TaskID: "task"}
					spec.Workload.Spec.Runtime.Image = "fixture"
					_, err = a.Start(t.Context(), spec)
				}
				return fakeChanges(f), err
			}},
			{name: "Resume", run: func(t *testing.T) (int, error) {
				f := &fakeEngine{}
				a, err := NewAdapter(hostedFake{fakeEngine: f, host: host}, "")
				if err == nil {
					err = a.Resume(t.Context(), rt.Handle{ExternalID: "ctr-123"}, rt.CheckpointRef{})
				}
				return fakeChanges(f), err
			}},
			{name: "StartProxy", run: func(t *testing.T) (int, error) {
				h := &hostedProxy{readinessEngine: &readinessEngine{fakeProxyEngine: newFakeProxyEngine()}, host: host}
				h.containers["proxy"] = container.InspectResponse{ID: "cid-proxy", State: &container.State{Status: "created"}}
				a, err := NewAdapter(h, "")
				if err == nil {
					err = a.StartProxy(t.Context(), "proxy")
				}
				return h.calls, err
			}},
		} {
			t.Run(host+"/"+tc.name, func(t *testing.T) {
				t.Setenv("DOCKER_HOST", "")
				changes, err := tc.run(t)
				if changes != 0 {
					t.Fatalf("made %d changes on a remote engine (err = %v)", changes, err)
				}
				if err == nil || !strings.Contains(err.Error(), "remote engine") {
					t.Fatalf("err = %v, want the remote engine refusal", err)
				}
			})
		}
	}
}

func fakeChanges(f *fakeEngine) int {
	n := 0
	if f.createdNetwork.Driver != "" {
		n++
	}
	if f.createdOpts.Config != nil {
		n++
	}
	if f.startedID != "" {
		n++
	}
	return n
}

// mobyClientPackages are the import paths of the Docker engine client.
var mobyClientPackages = map[string]bool{
	"github.com/moby/moby/client":     true,
	"github.com/docker/docker/client": true,
}

// TestOnlyTheConstructorsMakeAnEngineClient keeps the remote engine refusal
// where it cannot be skipped. In non-test code of the module, only NewFromEnv
// may create a Moby client, and only NewFromEnv and NewAdapter may build a
// Docker Adapter value; both refuse a remote engine. With no client or adapter
// on a remote engine, no engine call can reach one.
func TestOnlyTheConstructorsMakeAnEngineClient(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", "..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("module root not found: %v", err)
	}
	const self = "github.com/shadewright-labs/sablecourt/internal/runtime/docker"
	fset := token.NewFileSet()
	checked := 0
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if name := d.Name(); path != root && (strings.HasPrefix(name, ".") || name == "testdata" || name == "node_modules") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return err
		}
		checked++
		inDocker := filepath.Dir(path) == filepath.Join(root, "internal", "runtime", "docker")
		mobyNames, dockerNames := map[string]bool{}, map[string]bool{}
		for _, imp := range f.Imports {
			p, _ := strconv.Unquote(imp.Path.Value)
			name := filepath.Base(p)
			if imp.Name != nil {
				name = imp.Name.Name
			}
			switch {
			case mobyClientPackages[p]:
				mobyNames[name] = true
			case p == self:
				dockerNames[name] = true
			}
		}
		for _, decl := range f.Decls {
			fn, _ := decl.(*ast.FuncDecl)
			owner := ""
			if fn != nil && fn.Recv == nil && inDocker {
				owner = fn.Name.Name
			}
			ast.Inspect(decl, func(n ast.Node) bool {
				switch n := n.(type) {
				case *ast.SelectorExpr:
					if x, ok := n.X.(*ast.Ident); ok && mobyNames[x.Name] && strings.HasPrefix(n.Sel.Name, "New") && owner != "NewFromEnv" {
						t.Errorf("%s: %s.%s creates a Docker engine client outside docker.NewFromEnv", fset.Position(n.Pos()), x.Name, n.Sel.Name)
					}
				case *ast.CompositeLit:
					if isDockerAdapterType(n.Type, inDocker, dockerNames) && owner != "NewFromEnv" && owner != "NewAdapter" {
						t.Errorf("%s: builds a docker.Adapter outside docker.NewFromEnv and docker.NewAdapter", fset.Position(n.Pos()))
					}
				}
				return true
			})
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if checked < 50 {
		t.Fatalf("checked only %d files under %s", checked, root)
	}
}

func isDockerAdapterType(e ast.Expr, inDocker bool, dockerNames map[string]bool) bool {
	switch e := e.(type) {
	case *ast.Ident:
		return inDocker && e.Name == "Adapter"
	case *ast.SelectorExpr:
		x, ok := e.X.(*ast.Ident)
		return ok && dockerNames[x.Name] && e.Sel.Name == "Adapter"
	}
	return false
}
