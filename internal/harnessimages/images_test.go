package harnessimages

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
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
		if errors.Is(err, io.EOF) {
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
		{"agy", "1.5.17", []string{"agy"}},
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

func dockerStage(t *testing.T, name string) string {
	t.Helper()
	body, err := definitions.ReadFile("Dockerfile")
	if err != nil {
		t.Fatal(err)
	}
	for _, chunk := range strings.Split(string(body), "\nFROM ")[1:] {
		header, rest, _ := strings.Cut(chunk, "\n")
		if strings.HasSuffix(header, " AS "+name) {
			return header + "\n" + rest
		}
	}
	t.Fatalf("missing image stage %s", name)
	return ""
}

func TestVersionCheckIdentity(t *testing.T) {
	stage := dockerStage(t, "codex")
	user := strings.Index(stage, "USER "+User)
	version := strings.Index(stage, "--version)")
	if user < 0 || version < 0 || user > version {
		t.Fatal("version probe must use the final non-root identity to avoid root-owned CLI state")
	}
}

// The Dockerfile and images.go pin the same Codex CLI: the version in the
// catalog, the release asset and its SHA-256 for the one pinned architecture.
// Bumping the version in one place without the other, or without a checksum
// recorded for that version, fails here; a wrong checksum fails the build.
func TestCodexPinMatchesDockerfile(t *testing.T) {
	d, err := Lookup("codex")
	if err != nil {
		t.Fatal(err)
	}
	b := d.Bundled
	if b == nil || b.Version != d.Version || b.Platform != Platform || !strings.HasPrefix(Platform, "linux/") || len(b.SHA256) != 64 || b.Target == "" {
		t.Fatalf("codex bundled pin %+v does not match version %s and platform %s", b, d.Version, Platform)
	}
	arch := strings.TrimPrefix(Platform, "linux/")
	stage := dockerStage(t, "codex")
	for _, want := range []string{
		"ARG TARGETARCH",
		"releases/download/rust-v" + d.Version + "/codex-${codex_target}.tar.gz",
		arch + ") codex_target=" + b.Target + " codex_sha256=" + b.SHA256 + " ;;",
		"*) echo \"no Codex CLI is pinned for ${TARGETARCH}\" >&2; exit 1 ;;",
		"OpenAI Codex CLI " + d.Version + ", unmodified.",
		"test \"$(codex --version)\" = 'codex-cli " + d.Version + "'",
	} {
		if !strings.Contains(stage, want) {
			t.Fatalf("codex stage lacks %q", want)
		}
	}
	download, check, extract := strings.Index(stage, "curl "), strings.Index(stage, "sha256sum --check --strict"), strings.Index(stage, "tar -xzf")
	if download < 0 || check < download || extract < check {
		t.Fatal("the codex stage must check the SHA-256 after the download and before extracting")
	}
}

// The published images for the proprietary tools must carry no vendor
// software: no download, no copy of a binary, only the base and a README.
func TestToolLessStages(t *testing.T) {
	for _, name := range []string{"tool-less", "claude", "agy"} {
		t.Run(name, func(t *testing.T) {
			stage := dockerStage(t, name)
			for _, banned := range []string{"curl", "wget", "ADD ", "install.sh", "googleapis", "claude.ai", "/usr/local/bin"} {
				if strings.Contains(stage, banned) {
					t.Fatalf("stage %s contains %q", name, banned)
				}
			}
			if name == "tool-less" {
				if !strings.HasPrefix(stage, "base AS ") || !strings.Contains(stage, ToolDir) || !strings.Contains(stage, "USER "+User) {
					t.Fatal("tool-less stage must derive from base, own the tool mount point and run as the harness user")
				}
				return
			}
			if !strings.HasPrefix(stage, "tool-less AS ") || !strings.Contains(stage, "licenses/image/"+name+".txt /licenses/README") ||
				!strings.Contains(stage, "Contains no vendor software; the tool is installed at login.") {
				t.Fatalf("stage %s must derive from tool-less and say it carries no vendor software", name)
			}
		})
	}
}

func TestImageLicences(t *testing.T) {
	for _, tc := range []struct{ stage, want string }{
		{"codex", "COPY licenses/codex/LICENSE licenses/codex/NOTICE /licenses/codex/"},
		{"codex", "Contains the OpenAI Codex CLI 0.172.0, unmodified."},
		{"codex", "rust-v0.172.0"},
		{"proxy", "COPY licenses/go/LICENSE /licenses/go/LICENSE"},
		{"proxy", "COPY licenses/sablecourt/LICENSE /licenses/sablecourt/LICENSE"},
		{"proxy", "/licenses/README"},
	} {
		if !strings.Contains(dockerStage(t, tc.stage), tc.want) {
			t.Fatalf("stage %s lacks %q", tc.stage, tc.want)
		}
	}
	for _, name := range []string{"licenses/codex/LICENSE", "licenses/codex/NOTICE", "licenses/go/LICENSE", "licenses/sablecourt/LICENSE",
		"licenses/image/claude.txt", "licenses/image/codex.txt", "licenses/image/agy.txt", "licenses/image/proxy.txt"} {
		if body, err := definitions.ReadFile(name); err != nil || len(body) == 0 {
			t.Fatalf("%s missing from the embedded definitions", name)
		}
	}
	notice, _ := definitions.ReadFile("licenses/codex/NOTICE")
	if !strings.Contains(string(notice), "OpenAI Codex") {
		t.Fatal("codex NOTICE is not the upstream file")
	}
	ours, _ := definitions.ReadFile("licenses/sablecourt/LICENSE")
	root, err := os.ReadFile(filepath.Join("..", "..", "LICENSE"))
	if err != nil || !bytes.Equal(ours, root) {
		t.Fatal("licenses/sablecourt/LICENSE must equal the repository LICENSE")
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
				if call.files["Dockerfile"] == "" || call.files["council-proxy"] != "proxy executable" || call.files["licenses/codex/NOTICE"] == "" {
					t.Fatal("build context must contain the embedded Dockerfile, licence files and compiled proxy")
				}
				for name := range call.files {
					if name != "Dockerfile" && name != "council-proxy" && !strings.HasPrefix(name, "licenses/") {
						t.Fatalf("unexpected build context file %q", name)
					}
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
