package docker

import (
	"context"
	"crypto/rand"
	"errors"
	"os/exec"
	"path"
	"testing"
	"time"

	"github.com/moby/moby/client"
	"github.com/shadewright-labs/sablecourt/internal/harnessimages"
)

// Real engine: missing login detection and a started probe for each volume.
// It uses scratch volumes, never the volumes of a real login.
func TestCheckCouncilHarnessDocker(t *testing.T) {
	a, err := NewFromEnv()
	if err != nil {
		t.Skipf("Docker client unavailable: %v", err)
	}
	ping, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if a.Ping(ping) != nil {
		t.Skip("Docker unavailable")
	}
	cli, err := exec.LookPath("docker")
	if err != nil {
		t.Skip("docker CLI unavailable")
	}
	for _, role := range []string{"claude", "codex", "agy"} {
		t.Run(role, func(t *testing.T) {
			def, err := harnessimages.Lookup(role)
			if err != nil {
				t.Fatal(err)
			}
			vol := "sablecourt-test-" + rand.Text()[:12] + "-" + role
			a.CouncilHarnesses = map[string]CouncilHarness{role: {Image: def.Image, LoginVolume: vol, LoginFiles: def.LoginFiles}}
			ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
			defer cancel()
			t.Cleanup(func() {
				_ = exec.Command(cli, "volume", "rm", "-f", vol).Run()
			})
			wantFault := func(want HarnessFault) {
				t.Helper()
				var he *HarnessError
				if err := a.CheckCouncilHarness(ctx, role); !errors.As(err, &he) || he.Fault != want {
					t.Fatalf("got %v, want fault %s", err, want)
				}
			}
			if _, err := a.Client.ImageInspect(ctx, def.Image); err != nil {
				wantFault(FaultImageMissing)
				t.Skipf("image %s is not built locally; run %s", def.Image, harnessimages.BuildCommand(role))
			}
			wantFault(FaultVolumeMissing)
			if err := a.EnsureCouncilLoginVolume(ctx, role); err != nil {
				t.Fatal(err)
			}
			wantFault(FaultLoginMissing)
			// An empty credential file is not a login.
			seed := func(content string) {
				t.Helper()
				file := def.LoginFiles[0]
				script := "mkdir -p " + path.Dir(file) + " && printf '%s' '" + content + "' > " + file
				out, err := exec.CommandContext(ctx, cli, "run", "--rm", "--network", "none", "--user", "10001:10001",
					"-v", vol+":/home/harness", "--entrypoint", "/bin/bash", def.Image, "-c", script).CombinedOutput()
				if err != nil {
					t.Fatalf("seed volume: %v: %s", err, out)
				}
			}
			seed("")
			wantFault(FaultLoginMissing)
			seed("fixture")
			if err := a.CheckCouncilHarness(ctx, role); err != nil {
				t.Fatal(err)
			}
			list, err := a.Client.(interface {
				ContainerList(context.Context, client.ContainerListOptions) (client.ContainerListResult, error)
			}).ContainerList(ctx, client.ContainerListOptions{All: true, Filters: make(client.Filters).Add("label", "sablecourt.io/login-probe")})
			if err != nil || len(list.Items) != 0 {
				t.Fatalf("probe containers left behind: %v %d", err, len(list.Items))
			}
		})
	}
}
