package harnessimages

import (
	"context"
	"crypto/rand"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestDockerImages(t *testing.T) {
	docker := CommandDocker{}
	probe, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	err := docker.Run(probe, []string{"info", "--format", "{{.OSType}}"}, nil)
	cancel()
	if err != nil {
		t.Skipf("Docker unavailable: %v", err)
	}
	if os.Getenv("SABLECOURT_DOCKER_TEST") != "1" {
		t.Skip("Docker image builds require SABLECOURT_DOCKER_TEST=1 and access to the pinned release artifacts")
	}
	if runtime.GOOS != "darwin" || runtime.GOARCH != "arm64" {
		t.Skip("image acceptance targets an arm64 Mac")
	}
	_, file, _, _ := runtime.Caller(0)
	source := filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
	for _, role := range []string{"claude", "codex", "agy"} {
		t.Run(role, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
			defer cancel()
			if err := Build(ctx, docker, LocalProxyCompiler{Source: source}, role); err != nil {
				t.Fatal(err)
			}
			d, _ := Lookup(role)
			// codex carries its CLI; claude and agy carry no vendor software and
			// only the empty mount point for the tool volume.
			check := fmt.Sprintf("test \"$(%s --version)\" = 'codex-cli %s' && test -s /licenses/codex/LICENSE && test -s /licenses/codex/NOTICE", role, d.Version)
			if d.Tool != nil {
				check = fmt.Sprintf("! command -v %s && test -z \"$(ls -A %s)\" && test \"$(stat -c %%u %s)\" = 10001", role, ToolDir, ToolDir)
			}
			for _, image := range []struct{ tag, check string }{
				{d.Image, "test \"$(id -u)\" = 10001 && test \"$(id -g)\" = 10001 && test -s /licenses/README && " + check},
				{ProxyImage(), "test \"$(id -u)\" = 10001 && test \"$(id -g)\" = 10001 && test -x /usr/local/bin/council-proxy && /usr/local/bin/council-proxy -h"},
			} {
				var nonce [12]byte
				if _, err := rand.Read(nonce[:]); err != nil {
					t.Fatal(err)
				}
				name := fmt.Sprintf("sablecourt-image-test-%x", nonce)
				t.Cleanup(func() {
					cleanup, stop := context.WithTimeout(context.Background(), 10*time.Second)
					defer stop()
					if err := docker.Run(cleanup, []string{"rm", "-f", name}, nil); err != nil {
						t.Logf("container cleanup failed (creation may have failed): %v", err)
					}
				})
				args := []string{"run", "--pull=never", "--name", name, "--network", "none", "--read-only", "--cap-drop", "ALL", "--security-opt", "no-new-privileges", "--tmpfs", StateDir + ":uid=10001,gid=10001,mode=0700", "--tmpfs", "/scratch:uid=10001,gid=10001,mode=0700", "--entrypoint", "/bin/sh", image.tag, "-c", image.check}
				if err := docker.Run(ctx, args, nil); err != nil {
					t.Fatalf("verify %s: %v", image.tag, err)
				}
			}
		})
	}
}

func TestCommandDockerEnvironment(t *testing.T) {
	for _, tc := range []struct {
		name      string
		cancelled bool
	}{
		{name: "filtered environment"}, {name: "cancelled", cancelled: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			script := "#!/bin/sh\n[ -z \"$SABLECOURT_TASK_TOKEN$ANTHROPIC_API_KEY$OPENAI_API_KEY$HTTP_PROXY$DOCKER_AUTH_CONFIG\" ] || exit 9\n[ \"$DOCKER_CONTEXT\" = fixture ] || exit 10\ncat >/dev/null\n"
			if err := os.WriteFile(filepath.Join(dir, "docker"), []byte(script), 0700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
			t.Setenv("DOCKER_CONTEXT", "fixture")
			for _, key := range []string{"SABLECOURT_TASK_TOKEN", "ANTHROPIC_API_KEY", "OPENAI_API_KEY", "HTTP_PROXY", "DOCKER_AUTH_CONFIG"} {
				t.Setenv(key, "fixture")
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if tc.cancelled {
				cancel()
			}
			err := (CommandDocker{}).Run(ctx, []string{"info"}, strings.NewReader("build context"))
			if (err != nil) != tc.cancelled {
				t.Fatalf("error = %v", err)
			}
		})
	}
}
