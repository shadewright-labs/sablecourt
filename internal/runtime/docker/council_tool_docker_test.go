package docker

import (
	"context"
	"crypto/rand"
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/shadewright-labs/sablecourt/internal/harnessimages"
)

// Real engine and real vendor download: install the pinned tool into a
// throwaway volume, check it the way a turn does, and confirm the harness user
// cannot write it. It needs no login and no account, and never touches the
// volumes of a real login.
func TestInstallCouncilToolDocker(t *testing.T) {
	if os.Getenv("SABLECOURT_TOOL_INSTALL_TEST") != "1" {
		t.Skip("set SABLECOURT_TOOL_INSTALL_TEST=1 to download the pinned vendor tools and install them into scratch volumes")
	}
	a, err := NewFromEnv()
	if err != nil {
		t.Skipf("Docker client unavailable: %v", err)
	}
	cli, err := exec.LookPath("docker")
	if err != nil {
		t.Skip("docker CLI unavailable")
	}
	for _, tc := range []struct{ role, version string }{{"claude", "2.1.332 (Claude Code)"}, {"agy", "1.5.17"}} {
		t.Run(tc.role, func(t *testing.T) {
			def, _ := harnessimages.Lookup(tc.role)
			ctx, cancel := context.WithTimeout(t.Context(), 15*time.Minute)
			defer cancel()
			if _, err := a.Client.ImageInspect(ctx, def.Image); err != nil {
				t.Skipf("image %s is not built locally; run %s", def.Image, harnessimages.BuildCommand(tc.role))
			}
			id := strings.ToLower(rand.Text()[:12])
			login, toolVol := "sablecourt-test-"+id+"-"+tc.role, "sablecourt-test-"+id+"-"+tc.role+"-tool"
			t.Cleanup(func() { _ = exec.Command(cli, "volume", "rm", "-f", login, toolVol).Run() })
			tool := &CouncilTool{Volume: toolVol, Binary: def.Tool.Binary, Version: def.Tool.Version, SHA256: def.Tool.SHA256, InstallCommand: harnessimages.InstallCommand(tc.role)}
			a.CouncilHarnesses = map[string]CouncilHarness{tc.role: {Image: def.Image, LoginVolume: login, LoginFiles: def.LoginFiles, Tool: tool}}
			if err := a.EnsureCouncilLoginVolume(ctx, tc.role); err != nil {
				t.Fatal(err)
			}
			wantFault := func(want HarnessFault) {
				t.Helper()
				var he *HarnessError
				if err := a.CheckCouncilHarness(ctx, tc.role); !errors.As(err, &he) || he.Fault != want {
					t.Fatalf("got %v, want fault %s", err, want)
				}
			}
			wantFault(FaultToolMissing)

			started := time.Now()
			verified, err := harnessimages.FetchTool(ctx, harnessimages.HTTPDownloader{}, tc.role)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = verified.Close() }()
			t.Logf("download and host-side check: %s", time.Since(started).Round(time.Millisecond))
			f, err := verified.Open()
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = f.Close() }()
			started = time.Now()
			if err := a.InstallCouncilTool(ctx, tc.role, f); err != nil {
				t.Fatal(err)
			}
			t.Logf("install container: %s", time.Since(started).Round(time.Millisecond))

			// The tool checks pass: the only remaining fault is the missing login.
			started = time.Now()
			wantFault(FaultLoginMissing)
			t.Logf("preflight with tool digest: %s", time.Since(started).Round(time.Millisecond))

			run := func(script string) (string, error) {
				out, err := exec.CommandContext(ctx, cli, "run", "--rm", "--network", "none", "--read-only", "--cap-drop", "ALL",
					"--security-opt", "no-new-privileges", "--user", "10001:10001", "--tmpfs", "/scratch:uid=10001,gid=10001,mode=0700",
					"--mount", "type=volume,src="+toolVol+",dst="+councilToolDir+",readonly,volume-nocopy",
					"--entrypoint", "/bin/bash", def.Image, "-c", script).CombinedOutput()
				return strings.TrimSpace(string(out)), err
			}
			out, err := run(councilToolDir + "/" + def.Tool.Binary + " --version")
			if err != nil || out != tc.version {
				t.Fatalf("installed tool version: %q %v", out, err)
			}
			if out, err := run("touch " + councilToolDir + "/x || cp /bin/true " + councilToolDir + "/" + def.Tool.Binary); err == nil {
				t.Fatalf("the harness user wrote the read-only tool volume: %s", out)
			}
			// The guard a turn runs before the tool, on the installed volume.
			guard := "t=" + councilToolDir + "/" + def.Tool.Binary + " v=" + def.Tool.Version + " d=" + def.Tool.SHA256 + "\n" + toolCheck + "echo $tool_fault"
			started = time.Now()
			if out, err := run(guard); err != nil || out != "0" {
				t.Fatalf("guard: %q %v", out, err)
			}
			t.Logf("turn guard: %s", time.Since(started).Round(time.Millisecond))

			// A second install replaces the tool in place and leaves no partial file.
			f2, _ := verified.Open()
			defer func() { _ = f2.Close() }()
			if err := a.InstallCouncilTool(ctx, tc.role, f2); err != nil {
				t.Fatal(err)
			}
			if out, err := run("ls -A " + councilToolDir + " | tr '\\n' ' '; stat -c '%u:%g %a' " + councilToolDir + "/" + def.Tool.Binary); err != nil ||
				out != "VERSION "+def.Tool.Binary+" 10001:10001 555" {
				t.Fatalf("volume contents: %q %v", out, err)
			}
		})
	}
}
