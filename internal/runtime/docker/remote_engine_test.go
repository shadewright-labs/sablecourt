package docker

import (
	"context"
	"errors"
	"fmt"
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
	"time"

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
	})
	// A client that does not report its host cannot be checked, so it is
	// refused like a remote one; nil reports nothing either.
	for name, cli := range map[string]EngineClient{"no DaemonHost": &fakeEngine{}, "nil": nil} {
		t.Run("NewAdapter "+name, func(t *testing.T) {
			t.Setenv("DOCKER_HOST", "")
			a, err := NewAdapter(cli, "")
			if a != nil || !errors.Is(err, errNoDaemonHost) {
				t.Fatalf("NewAdapter() = %v, %v, want %v", a, err, errNoDaemonHost)
			}
		})
	}
}

// TestZeroValueAdapterRefusesEngineCalls checks that an Adapter no
// constructor built, with the harness configuration a real one would have,
// fails every engine call with an error instead of panicking or running.
func TestZeroValueAdapterRefusesEngineCalls(t *testing.T) {
	local, _ := probeFixture("claude")
	cfg := local.CouncilHarnesses["claude"]
	cfg.Tool = fixtureTool()
	handle := rt.Handle{AdapterName: "docker", ExternalID: "ctr-123"}
	launch := rt.CouncilLaunch{}
	launch.Limits.WallTime = time.Minute
	record := func(context.Context, rt.CouncilResult) error { return nil }
	for name, call := range map[string]func(a *Adapter) error{
		"Ping":                  func(a *Adapter) error { return a.Ping(t.Context()) },
		"EnsureInternalNetwork": func(a *Adapter) error { return a.EnsureInternalNetwork(t.Context()) },
		"EnsureImage":           func(a *Adapter) error { return a.EnsureImage(t.Context(), "fixture") },
		"ResolveLocalImage": func(a *Adapter) error {
			_, err := a.ResolveLocalImage(t.Context(), "fixture")
			return err
		},
		"Start": func(a *Adapter) error {
			spec := rt.LaunchSpec{TaskID: "task"}
			spec.Workload.Spec.Runtime.Image = "fixture"
			_, err := a.Start(t.Context(), spec)
			return err
		},
		"Observe": func(a *Adapter) error {
			_, err := a.Observe(t.Context(), handle)
			return err
		},
		"Resume": func(a *Adapter) error { return a.Resume(t.Context(), handle, rt.CheckpointRef{}) },
		"Stop":   func(a *Adapter) error { return a.Stop(t.Context(), handle) },
		"HandleAlive": func(a *Adapter) error {
			_, err := a.HandleAlive(t.Context(), handle)
			return err
		},
		"HandleExited": func(a *Adapter) error {
			_, err := a.HandleExited(t.Context(), handle)
			return err
		},
		"CheckCouncilHarness":      func(a *Adapter) error { return a.CheckCouncilHarness(t.Context(), "claude") },
		"EnsureCouncilLoginVolume": func(a *Adapter) error { return a.EnsureCouncilLoginVolume(t.Context(), "claude") },
		"InstallCouncilTool": func(a *Adapter) error {
			return a.InstallCouncilTool(t.Context(), "claude", strings.NewReader(fixtureToolBody))
		},
		"RunCouncil": func(a *Adapter) error {
			_, err := a.RunCouncil(t.Context(), launch, record)
			return err
		},
		"InspectCouncil": func(a *Adapter) error {
			_, err := a.InspectCouncil(t.Context(), rt.CouncilIdentity{ContainerID: "ctr-123"})
			return err
		},
		"CleanupCouncil": func(a *Adapter) error { return a.CleanupCouncil(t.Context(), rt.CouncilResult{ContainerID: "ctr-123"}) },
		"ProvisionCouncilProxy": func(a *Adapter) error {
			_, err := a.ProvisionCouncilProxy(t.Context(), "sha256:"+strings.Repeat("a", 64), councilproxy.Config{Harness: "claude", DaemonListen: net.JoinHostPort(net.IPv6loopback.String(), "7432")})
			return err
		},
		"ConnectProxyEgress":    func(a *Adapter) error { return a.ConnectProxyEgress(t.Context(), "proxy") },
		"StartProxy":            func(a *Adapter) error { return a.StartProxy(t.Context(), "proxy") },
		"RemoveProxy":           func(a *Adapter) error { return a.RemoveProxy(t.Context(), "proxy") },
		"RemoveInternalNetwork": func(a *Adapter) error { return a.RemoveInternalNetwork(t.Context(), "sablecourt-net") },
	} {
		t.Run(name, func(t *testing.T) {
			a := &Adapter{CouncilHarnesses: map[string]CouncilHarness{"claude": cfg}, NetworkName: DefaultNetworkName}
			err := func() (err error) {
				defer func() {
					if r := recover(); r != nil {
						t.Fatalf("panicked: %v", r)
					}
				}()
				return call(a)
			}()
			if !errors.Is(err, errNoEngine) {
				t.Fatalf("err = %v, want %v", err, errNoEngine)
			}
		})
	}
	if (&Adapter{}).DaemonHost() != "" || (&Adapter{}).Close() != nil {
		t.Fatal("a zero-value Adapter reports an engine host or fails to close")
	}
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

const dockerPackage = "github.com/shadewright-labs/sablecourt/internal/runtime/docker"

// TestOnlyTheConstructorsMakeAnEngineClient keeps the remote engine refusal
// where it cannot be skipped. Across every non-test Go file of the module,
// only NewFromEnv may create a Moby client, and only NewFromEnv and
// NewAdapter may build a Docker Adapter value; both refuse a remote engine.
// With no client or adapter on a remote engine, no engine call can reach one.
func TestOnlyTheConstructorsMakeAnEngineClient(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", "..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("module root not found: %v", err)
	}
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
		for _, finding := range engineGuardFindings(fset, f, inDocker) {
			t.Error(finding)
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

// TestEngineGuardFindings runs the guard on small sources, so a way around
// it shows up here rather than in a later review.
func TestEngineGuardFindings(t *testing.T) {
	const head = "package p\n\nimport (\n"
	for _, tc := range []struct {
		name     string
		src      string
		inDocker bool
		want     int
	}{
		{"client.New", head + "\"github.com/moby/moby/client\"\n)\n\nvar _, _ = client.New()\n", false, 1},
		{"aliased client", head + "mc \"github.com/moby/moby/client\"\n)\n\nvar _, _ = mc.NewClientWithOpts()\n", false, 1},
		{"legacy client", head + "dc \"github.com/docker/docker/client\"\n)\n\nfunc f() { dc.NewClientWithOpts() }\n", false, 1},
		{"dot client", head + ". \"github.com/moby/moby/client\"\n)\n\nvar _, _ = New()\n", false, 1},
		{"dot client in docker", head + ". \"github.com/moby/moby/client\"\n)\n\nfunc NewFromEnv() { New() }\n", true, 1},
		{"client in NewFromEnv", head + "\"github.com/moby/moby/client\"\n)\n\nfunc NewFromEnv() { client.New() }\n", true, 0},
		{"client in other func", head + "\"github.com/moby/moby/client\"\n)\n\nfunc other() { client.New() }\n", true, 1},
		{"client options are fine", head + "\"github.com/moby/moby/client\"\n)\n\nvar _ = client.ContainerStartOptions{}\n", false, 0},
		{"literal", head + "\"" + dockerPackage + "\"\n)\n\nvar _ = &docker.Adapter{}\n", false, 1},
		{"aliased literal", head + "d \"" + dockerPackage + "\"\n)\n\nvar _ = d.Adapter{}\n", false, 1},
		{"dot docker", head + ". \"" + dockerPackage + "\"\n)\n\nvar _ = Adapter{}\n", false, 1},
		{"new", head + "\"" + dockerPackage + "\"\n)\n\nvar _ = new(docker.Adapter)\n", false, 1},
		{"var", head + "\"" + dockerPackage + "\"\n)\n\nvar a docker.Adapter\n", false, 1},
		{"array", head + "\"" + dockerPackage + "\"\n)\n\nvar a [1]docker.Adapter\n", false, 1},
		{"field", head + "\"" + dockerPackage + "\"\n)\n\ntype s struct{ docker.Adapter }\n", false, 1},
		{"alias", head + "\"" + dockerPackage + "\"\n)\n\ntype A = docker.Adapter\n", false, 1},
		{"pointer is fine", head + "\"" + dockerPackage + "\"\n)\n\nvar a *docker.Adapter\n\nfunc f(b *docker.Adapter) []*docker.Adapter { return nil }\n", false, 0},
		{"constructor call is fine", head + "\"" + dockerPackage + "\"\n)\n\nvar _, _ = docker.NewFromEnv()\n", false, 0},
		{"literal in NewAdapter", "package docker\n\nfunc NewAdapter() *Adapter { return &Adapter{} }\n", true, 0},
		{"literal in a method", "package docker\n\nfunc (a *Adapter) clone() *Adapter { return &Adapter{} }\n", true, 1},
		{"var in docker", "package docker\n\nfunc f() { var a Adapter; _ = a }\n", true, 1},
		{"other Adapter type", "package coding\n\nvar _ = &Adapter{}\n", false, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fset := token.NewFileSet()
			f, err := parser.ParseFile(fset, "src.go", tc.src, 0)
			if err != nil {
				t.Fatal(err)
			}
			if got := engineGuardFindings(fset, f, tc.inDocker); len(got) != tc.want {
				t.Fatalf("findings = %q, want %d", got, tc.want)
			}
		})
	}
}

// engineGuardFindings lists the ways one file makes a Moby client or a
// Docker Adapter value outside the constructors. A dot import of either
// package is refused outright: it hides which package a name comes from.
func engineGuardFindings(fset *token.FileSet, f *ast.File, inDocker bool) []string {
	var out []string
	report := func(n ast.Node, format string, args ...any) {
		out = append(out, fset.Position(n.Pos()).String()+": "+fmt.Sprintf(format, args...))
	}
	mobyNames, dockerNames := map[string]bool{}, map[string]bool{}
	for _, imp := range f.Imports {
		p, _ := strconv.Unquote(imp.Path.Value)
		if !mobyClientPackages[p] && p != dockerPackage {
			continue
		}
		name := filepath.Base(p)
		if imp.Name != nil {
			name = imp.Name.Name
		}
		switch {
		case name == ".":
			report(imp, "dot import of %s hides engine client and adapter construction from this guard", p)
		case mobyClientPackages[p]:
			mobyNames[name] = true
		default:
			dockerNames[name] = true
		}
	}
	isAdapter := func(e ast.Expr) bool {
		switch e := e.(type) {
		case *ast.Ident:
			return inDocker && e.Name == "Adapter"
		case *ast.SelectorExpr:
			x, ok := e.X.(*ast.Ident)
			return ok && dockerNames[x.Name] && e.Sel.Name == "Adapter"
		}
		return false
	}
	// holdsAdapter reports a type that holds an Adapter value, not a pointer.
	var holdsAdapter func(e ast.Expr) bool
	holdsAdapter = func(e ast.Expr) bool {
		switch e := e.(type) {
		case *ast.ParenExpr:
			return holdsAdapter(e.X)
		case *ast.ArrayType:
			return holdsAdapter(e.Elt)
		case *ast.MapType:
			return holdsAdapter(e.Value)
		case *ast.ChanType:
			return holdsAdapter(e.Value)
		}
		return isAdapter(e)
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
					report(n, "%s.%s creates a Docker engine client outside docker.NewFromEnv", x.Name, n.Sel.Name)
				}
			case *ast.CompositeLit:
				if n.Type != nil && holdsAdapter(n.Type) && owner != "NewFromEnv" && owner != "NewAdapter" {
					report(n, "builds a docker.Adapter outside docker.NewFromEnv and docker.NewAdapter")
				}
			case *ast.CallExpr:
				if id, ok := n.Fun.(*ast.Ident); ok && id.Name == "new" && len(n.Args) == 1 && holdsAdapter(n.Args[0]) {
					report(n, "new(docker.Adapter) builds an adapter outside the constructors")
				}
			case *ast.ValueSpec:
				if n.Type != nil && holdsAdapter(n.Type) {
					report(n, "declares a docker.Adapter value outside the constructors")
				}
			case *ast.TypeSpec:
				if holdsAdapter(n.Type) {
					report(n, "names the docker.Adapter type, which lets it be built outside the constructors")
				}
			case *ast.StructType:
				for _, field := range n.Fields.List {
					if holdsAdapter(field.Type) {
						report(field, "holds a docker.Adapter value outside the constructors")
					}
				}
			}
			return true
		})
	}
	return out
}
