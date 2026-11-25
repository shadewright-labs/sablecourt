package harnessimages

import (
	"archive/tar"
	"context"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"testing"
)

type dockerCall struct {
	args  []string
	files map[string]string
}

type fakeDocker struct {
	calls    []dockerCall
	failAt   int
	cancelAt int
	cancel   context.CancelFunc
}

var errDocker = errors.New("engine failed")

type fakeCompiler struct{ empty, fail bool }

func (c fakeCompiler) Compile(context.Context) ([]byte, error) {
	if c.fail {
		return nil, errDocker
	}
	if c.empty {
		return nil, nil
	}
	return []byte("proxy executable"), nil
}

func (d *fakeDocker) Run(_ context.Context, args []string, input io.Reader) error {
	call := dockerCall{args: append([]string(nil), args...), files: make(map[string]string)}
	tr := tar.NewReader(input)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		body, err := io.ReadAll(tr)
		if err != nil {
			return err
		}
		call.files[h.Name] = string(body)
	}
	d.calls = append(d.calls, call)
	if d.cancelAt == len(d.calls) {
		d.cancel()
	}
	if d.failAt == len(d.calls) {
		return errDocker
	}
	return nil
}

func TestCatalog(t *testing.T) {
	for _, tc := range []struct {
		role, version string
		login         []string
	}{
		{"claude", "2.1.332", []string{"claude", "auth", "login"}},
		{"codex", "0.172.0", []string{"codex", "login", "--device-auth"}},
		{"agy", "1.5.16", []string{"agy"}},
	} {
		t.Run(tc.role, func(t *testing.T) {
			d, err := Lookup(tc.role)
			if err != nil {
				t.Fatal(err)
			}
			if d.Role != tc.role || d.Version != tc.version || !reflect.DeepEqual(d.LoginArgv, tc.login) ||
				d.Volume != "sablecourt-harness-"+tc.role || !strings.HasPrefix(d.Image, "sablecourt-harness-"+tc.role+":"+tc.version+"-") {
				t.Fatalf("unexpected definition: %+v", d)
			}
			d.LoginArgv[0] = "changed"
			again, _ := Lookup(tc.role)
			if again.LoginArgv[0] != tc.role {
				t.Fatal("catalog was mutated")
			}
		})
	}
}

func TestCatalogRejectsUnknownWithoutEcho(t *testing.T) {
	for _, role := range []string{"", "unknown", "Claude", "claude\nsecret=fixture", strings.Repeat("x", 100)} {
		t.Run(fmt.Sprintf("length_%d", len(role)), func(t *testing.T) {
			_, err := Lookup(role)
			if err == nil || err.Error() != "unknown harness role (choose claude, codex, or agy)" {
				t.Fatalf("unexpected refusal: %v", err)
			}
		})
	}
}

func TestVersionCheckIdentity(t *testing.T) {
	body, err := definitions.ReadFile("Dockerfile")
	if err != nil {
		t.Fatal(err)
	}
	for _, role := range []string{"claude", "codex", "agy"} {
		t.Run(role, func(t *testing.T) {
			_, stage, found := strings.Cut(string(body), "FROM base AS "+role+"\n")
			if !found {
				t.Fatal("missing image stage")
			}
			stage, _, _ = strings.Cut(stage, "\nFROM ")
			user := strings.Index(stage, "USER "+User)
			version := strings.Index(stage, "--version)")
			if user < 0 || version < 0 || user > version {
				t.Fatal("version probe must use the final non-root identity to avoid root-owned CLI state")
			}
		})
	}
}

func TestBuild(t *testing.T) {
	for _, tc := range []struct {
		name, role                                            string
		failAt, calls                                         int
		cancelAt                                              int
		cancel, wantError                                     bool
		nilDocker, nilCompiler, emptyCompiler, failedCompiler bool
	}{
		{name: "claude", role: "claude", calls: 2},
		{name: "codex", role: "codex", calls: 2},
		{name: "agy", role: "agy", calls: 2},
		{name: "unknown", role: "unknown", wantError: true},
		{name: "empty", wantError: true},
		{name: "case", role: "Claude", wantError: true},
		{name: "whitespace", role: " claude", wantError: true},
		{name: "proxy failure", role: "claude", failAt: 1, calls: 1, wantError: true},
		{name: "role failure", role: "agy", failAt: 2, calls: 2, wantError: true},
		{name: "cancelled", role: "codex", cancel: true, wantError: true},
		{name: "cancel between images", role: "claude", cancelAt: 1, calls: 1, wantError: true},
		{name: "cancel during last image", role: "agy", cancelAt: 2, calls: 2, wantError: true},
		{name: "missing engine", role: "claude", nilDocker: true, wantError: true},
		{name: "missing compiler", role: "claude", nilCompiler: true, wantError: true},
		{name: "empty executable", role: "claude", emptyCompiler: true, wantError: true},
		{name: "compiler failure", role: "claude", failedCompiler: true, wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			docker := &fakeDocker{failAt: tc.failAt}
			ctx, cancel := context.WithCancel(context.Background())
			docker.cancelAt, docker.cancel = tc.cancelAt, cancel
			defer cancel()
			if tc.cancel {
				cancel()
			}
			var engine Docker = docker
			var compiler ProxyCompiler = fakeCompiler{empty: tc.emptyCompiler, fail: tc.failedCompiler}
			if tc.nilDocker {
				engine = nil
			}
			if tc.nilCompiler {
				compiler = nil
			}
			err := Build(ctx, engine, compiler, tc.role)
			if (err != nil) != tc.wantError || len(docker.calls) != tc.calls {
				t.Fatalf("error %v, calls %d, want error %v, calls %d", err, len(docker.calls), tc.wantError, tc.calls)
			}
			if (tc.failAt != 0 || tc.failedCompiler) && !errors.Is(err, errDocker) {
				t.Fatal("lost engine error")
			}
			for i, call := range docker.calls {
				target, tag := "proxy", ProxyImage()
				if i == 1 {
					target = tc.role
					d, _ := Lookup(tc.role)
					tag = d.Image
				}
				want := []string{"build", "--platform", Platform, "--pull=false", "--rm", "--force-rm", "--target", target, "--tag", tag, "--file", "Dockerfile", "-"}
				if !reflect.DeepEqual(call.args, want) {
					t.Fatalf("args %v, want %v", call.args, want)
				}
				if len(call.files) != 2 || call.files["Dockerfile"] == "" || call.files["council-proxy"] != "proxy executable" {
					t.Fatal("build context must contain only the embedded Dockerfile and compiled proxy")
				}
				for name := range call.files {
					if strings.Contains(name, "..") || strings.HasPrefix(name, "/") {
						t.Fatalf("unsafe context file %q", name)
					}
				}
			}
			if tc.failAt != 0 {
				retry := &fakeDocker{}
				if err := Build(context.Background(), retry, fakeCompiler{}, tc.role); err != nil || len(retry.calls) != 2 {
					t.Fatalf("retry: %v", err)
				}
			}
		})
	}
}
