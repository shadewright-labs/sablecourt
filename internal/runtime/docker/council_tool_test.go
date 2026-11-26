package docker

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/api/types/volume"
	"github.com/shadewright-labs/sablecourt/internal/harnessimages"
	rt "github.com/shadewright-labs/sablecourt/internal/runtime"
)

const fixtureToolBody = "#!/bin/sh\necho \"tool ran: $*\"\n"

func digest(s string) string { d := sha256.Sum256([]byte(s)); return hex.EncodeToString(d[:]) }

func fixtureTool() *CouncilTool {
	return &CouncilTool{Volume: "sablecourt-harness-claude-tool", Binary: "claude", Version: "2.1.332", SHA256: digest(fixtureToolBody), InstallCommand: "sablectl harness install claude"}
}

func toolVolume(harness, name string) volume.Volume {
	return volume.Volume{Name: name, Driver: "local", Labels: CouncilToolVolumeLabels(harness)}
}

// Volume labels cannot change after creation, and only Sablecourt creates the
// tool volume, so its label set must be exactly the two tool labels.
func TestValidateCouncilToolVolumeLabels(t *testing.T) {
	for _, tc := range []struct {
		name   string
		labels map[string]string
		ok     bool
	}{
		{name: "exact tool labels", labels: CouncilToolVolumeLabels("claude"), ok: true},
		{name: "extra label", labels: map[string]string{"app.kubernetes.io/managed-by": "sablecourt", "sablecourt.io/harness-tool": "claude", "extra": "x"}},
		{name: "login label as well", labels: map[string]string{"app.kubernetes.io/managed-by": "sablecourt", "sablecourt.io/harness-tool": "claude", "sablecourt.io/harness": "claude"}},
		{name: "empty login label as well", labels: map[string]string{"app.kubernetes.io/managed-by": "sablecourt", "sablecourt.io/harness-tool": "claude", "sablecourt.io/harness": ""}},
		{name: "another harness", labels: CouncilToolVolumeLabels("agy")},
		{name: "one label missing", labels: map[string]string{"sablecourt.io/harness-tool": "claude"}},
		{name: "no labels"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v := volume.Volume{Name: "sablecourt-harness-claude-tool", Driver: "local", Labels: tc.labels}
			if err := validateCouncilToolVolume(v, "claude", v.Name); (err == nil) != tc.ok {
				t.Fatalf("err = %v, want ok %v", err, tc.ok)
			}
		})
	}
}

func TestToolDirMatchesImage(t *testing.T) {
	if councilToolDir != harnessimages.ToolDir {
		t.Fatal("runtime mount point differs from the image definition")
	}
	for _, role := range []string{"claude", "agy"} {
		d, _ := harnessimages.Lookup(role)
		tool := CouncilTool{Volume: d.Tool.Volume, Binary: d.Tool.Binary, Version: d.Tool.Version, SHA256: d.Tool.SHA256, InstallCommand: harnessimages.InstallCommand(role)}
		a := NewAdapter(nil, "")
		a.CouncilHarnesses = map[string]CouncilHarness{role: {Image: d.Image, LoginVolume: d.Volume, Tool: &tool}}
		if err := a.validateCouncilTool(role, a.CouncilHarnesses[role]); err != nil {
			t.Fatalf("%s pin rejected by the runtime: %v", role, err)
		}
	}
}

// The scripts run in the real shell against a private directory that stands
// in for the read-only tool volume.
func TestToolRuntimeCheck(t *testing.T) {
	if _, err := exec.LookPath("sha256sum"); err != nil {
		t.Skip("sha256sum unavailable")
	}
	for _, tc := range []struct {
		name    string
		setup   func(t *testing.T, dir string)
		fault   int
		message string
	}{
		{name: "version match", fault: 0},
		{name: "missing", setup: func(t *testing.T, dir string) { must(t, os.Remove(filepath.Join(dir, "claude"))) }, fault: 3, message: "council tool claude is not installed: run sablectl harness install claude"},
		{name: "symlink to a matching file", setup: func(t *testing.T, dir string) {
			must(t, os.Rename(filepath.Join(dir, "claude"), filepath.Join(dir, "real")))
			must(t, os.Symlink(filepath.Join(dir, "real"), filepath.Join(dir, "claude")))
		}, fault: 3, message: "is not installed"},
		{name: "not executable", setup: func(t *testing.T, dir string) { must(t, os.Chmod(filepath.Join(dir, "claude"), 0644)) }, fault: 3, message: "is not installed"},
		{name: "wrong version", setup: func(t *testing.T, dir string) { writeFile(t, filepath.Join(dir, "VERSION"), "2.1.300\n", 0644) }, fault: 4,
			message: "council tool claude is not the pinned version 2.1.332: run sablectl harness install claude"},
		{name: "version file missing", setup: func(t *testing.T, dir string) { must(t, os.Remove(filepath.Join(dir, "VERSION"))) }, fault: 4, message: "is not the pinned version"},
		// The marker must be exactly the pinned version and one newline.
		{name: "version without newline", setup: func(t *testing.T, dir string) { writeFile(t, filepath.Join(dir, "VERSION"), "2.1.332", 0644) }, fault: 4, message: "is not the pinned version"},
		{name: "version with trailing text after 64 bytes", setup: func(t *testing.T, dir string) {
			writeFile(t, filepath.Join(dir, "VERSION"), "2.1.332\n"+strings.Repeat("x", 80), 0644)
		}, fault: 4, message: "is not the pinned version"},
		{name: "version with a second line", setup: func(t *testing.T, dir string) { writeFile(t, filepath.Join(dir, "VERSION"), "2.1.332\nextra\n", 0644) }, fault: 4, message: "is not the pinned version"},
		{name: "version with two newlines", setup: func(t *testing.T, dir string) { writeFile(t, filepath.Join(dir, "VERSION"), "2.1.332\n\n", 0644) }, fault: 4, message: "is not the pinned version"},
		{name: "version is a directory", setup: func(t *testing.T, dir string) {
			must(t, os.Remove(filepath.Join(dir, "VERSION")))
			must(t, os.Mkdir(filepath.Join(dir, "VERSION"), 0755))
		}, fault: 4, message: "is not the pinned version"},
		{name: "version is a symlink to a matching file", setup: func(t *testing.T, dir string) {
			must(t, os.Rename(filepath.Join(dir, "VERSION"), filepath.Join(dir, "realversion")))
			must(t, os.Symlink(filepath.Join(dir, "realversion"), filepath.Join(dir, "VERSION")))
		}, fault: 4, message: "is not the pinned version"},
		{name: "checksum mismatch", setup: func(t *testing.T, dir string) {
			writeFile(t, filepath.Join(dir, "claude"), fixtureToolBody+"# changed\n", 0755)
		}, fault: 5,
			message: "council tool claude does not match its pinned SHA-256: run sablectl harness install claude"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			writeFile(t, filepath.Join(dir, "claude"), fixtureToolBody, 0755)
			writeFile(t, filepath.Join(dir, "VERSION"), "2.1.332\n", 0644)
			if tc.setup != nil {
				tc.setup(t, dir)
			}
			tool := fixtureTool()
			guard := []string{filepath.Join(dir, "claude"), tool.Version, tool.SHA256, tool.InstallCommand}

			// Turn: the bootstrap reads stdin, then checks, then writes the lease.
			scratch := t.TempDir()
			cmd := exec.Command("bash", append([]string{"--noprofile", "--norc", "-c", toolBootstrap, "bootstrap"}, append(guard, filepath.Join(dir, "claude"), "-p")...)...)
			cmd.Env = []string{"TMPDIR=" + scratch, "PATH=" + os.Getenv("PATH")}
			cmd.Stdin = strings.NewReader("fixture-lease\x00the question\x00")
			var stdout, stderr bytes.Buffer
			cmd.Stdout, cmd.Stderr = &stdout, &stderr
			err := cmd.Run()
			_, leaseErr := os.Stat(filepath.Join(scratch, "task.lease"))
			if tc.fault == 0 {
				if err != nil || stdout.String() != "tool ran: -p the question\n" || leaseErr != nil {
					t.Fatalf("turn: err=%v out=%q stderr=%q", err, stdout.String(), stderr.String())
				}
			} else {
				var exit *exec.ExitError
				if !errors.As(err, &exit) || exit.ExitCode() != 125 || !strings.Contains(stderr.String(), tc.message) || stdout.Len() != 0 {
					t.Fatalf("turn: err=%v out=%q stderr=%q", err, stdout.String(), stderr.String())
				}
				if !os.IsNotExist(leaseErr) {
					t.Fatal("a refused turn wrote the lease file")
				}
			}

			// Login: the same check before the tool's own login flow.
			login := exec.Command("bash", append([]string{"--noprofile", "--norc", "-c", toolGuardLogin, "guard"}, append(guard, filepath.Join(dir, "claude"), "auth", "login")...)...)
			login.Env = []string{"PATH=" + os.Getenv("PATH")}
			out, err := login.CombinedOutput()
			if (tc.fault == 0) != (err == nil) || (tc.fault != 0 && !strings.Contains(string(out), tc.message)) {
				t.Fatalf("login: err=%v out=%q", err, out)
			}

			// Preflight: exit status per fault, then the login file check.
			home := t.TempDir()
			cred := filepath.Join(home, "cred.json")
			probe := exec.Command("bash", "--noprofile", "--norc", "-c", toolProbeScript, "probe", filepath.Join(dir, "claude"), tool.Version, tool.SHA256, cred)
			probe.Env = []string{"PATH=" + os.Getenv("PATH")}
			code := exitCode(probe.Run())
			want := tc.fault
			if want == 0 {
				want = loginProbeAbsent
			}
			if code != want {
				t.Fatalf("probe exit %d, want %d", code, want)
			}
			if tc.fault == 0 {
				writeFile(t, cred, "{}", 0600)
				probe := exec.Command("bash", "--noprofile", "--norc", "-c", toolProbeScript, "probe", filepath.Join(dir, "claude"), tool.Version, tool.SHA256, cred)
				if code := exitCode(probe.Run()); code != 0 {
					t.Fatalf("probe with login exit %d", code)
				}
			}
		})
	}
}

func TestToolInstallScript(t *testing.T) {
	if _, err := exec.LookPath("sha256sum"); err != nil {
		t.Skip("sha256sum unavailable")
	}
	for _, tc := range []struct {
		name, input string
		wantOK      bool
	}{
		{name: "pinned bytes", input: fixtureToolBody, wantOK: true},
		{name: "other bytes", input: fixtureToolBody + "x"},
		{name: "truncated", input: fixtureToolBody[:5]},
		{name: "empty", input: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			writeFile(t, filepath.Join(dir, "claude"), "old tool", 0555)
			writeFile(t, filepath.Join(dir, "VERSION"), "2.1.300\n", 0644)
			script := strings.Replace(toolInstallScript, "dir="+councilToolDir, "dir="+shQuote(dir), 1)
			cmd := exec.Command("bash", "--noprofile", "--norc", "-c", script, "install", "claude", "2.1.332", digest(fixtureToolBody))
			cmd.Stdin = strings.NewReader(tc.input)
			out, err := cmd.CombinedOutput()
			bin, _ := os.ReadFile(filepath.Join(dir, "claude"))
			ver, _ := os.ReadFile(filepath.Join(dir, "VERSION"))
			if tc.wantOK {
				info, serr := os.Stat(filepath.Join(dir, "claude"))
				if err != nil || string(bin) != fixtureToolBody || string(ver) != "2.1.332\n" || serr != nil || info.Mode().Perm() != 0555 {
					t.Fatalf("err=%v out=%q bin=%q ver=%q", err, out, bin, ver)
				}
			} else if exitCode(err) != 3 || string(bin) != "old tool" || string(ver) != "2.1.300\n" || !strings.Contains(string(out), "do not match the pinned SHA-256") {
				t.Fatalf("err=%v out=%q bin=%q ver=%q", err, out, bin, ver)
			}
			if entries, _ := os.ReadDir(dir); len(entries) != 2 {
				t.Fatalf("partial files left: %v", entries)
			}
		})
	}
}

// A destination that exists but is not a regular file must fail the install:
// mv would move the checked file inside a directory and report success.
func TestToolInstallScriptDestinationNotRegular(t *testing.T) {
	if _, err := exec.LookPath("sha256sum"); err != nil {
		t.Skip("sha256sum unavailable")
	}
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, dir string)
	}{
		{name: "binary is a directory", setup: func(t *testing.T, dir string) { must(t, os.Mkdir(filepath.Join(dir, "claude"), 0755)) }},
		{name: "version is a directory", setup: func(t *testing.T, dir string) { must(t, os.Mkdir(filepath.Join(dir, "VERSION"), 0755)) }},
		{name: "binary is a symlink to a directory", setup: func(t *testing.T, dir string) {
			must(t, os.Mkdir(filepath.Join(dir, "elsewhere"), 0755))
			must(t, os.Symlink(filepath.Join(dir, "elsewhere"), filepath.Join(dir, "claude")))
		}},
		{name: "version is a symlink to a file", setup: func(t *testing.T, dir string) {
			writeFile(t, filepath.Join(dir, "other"), "2.1.332\n", 0644)
			must(t, os.Symlink(filepath.Join(dir, "other"), filepath.Join(dir, "VERSION")))
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			tc.setup(t, dir)
			script := strings.Replace(toolInstallScript, "dir="+councilToolDir, "dir="+shQuote(dir), 1)
			cmd := exec.Command("bash", "--noprofile", "--norc", "-c", script, "install", "claude", "2.1.332", digest(fixtureToolBody))
			cmd.Stdin = strings.NewReader(fixtureToolBody)
			out, err := cmd.CombinedOutput()
			if err == nil {
				t.Fatalf("install succeeded with a destination that is not a regular file: %q", out)
			}
			if !strings.Contains(string(out), "not a regular file") {
				t.Fatalf("err=%v out=%q", err, out)
			}
			entries, _ := os.ReadDir(dir)
			for _, e := range entries {
				if strings.HasPrefix(e.Name(), ".tool") || strings.HasPrefix(e.Name(), ".version") {
					t.Fatalf("staging file left: %s", e.Name())
				}
			}
		})
	}
}

// Two installs for one role can overlap. Run A pauses after its own checksum
// check while run B is still streaming a truncated input. A must not rename
// B's unchecked bytes into place and report success.
func TestToolInstallScriptConcurrent(t *testing.T) {
	if _, err := exec.LookPath("sha256sum"); err != nil {
		t.Skip("sha256sum unavailable")
	}
	realChmod, err := exec.LookPath("chmod")
	if err != nil {
		t.Skip("chmod unavailable")
	}
	dir, shim := t.TempDir(), t.TempDir()
	reached, release := filepath.Join(shim, "reached"), filepath.Join(shim, "release")
	// A chmod that stops run A between its checksum check and its renames.
	writeFile(t, filepath.Join(shim, "chmod"), "#!/bin/sh\n: > "+shQuote(reached)+"\nwhile ! test -e "+shQuote(release)+"; do sleep 0.01; done\nexec "+shQuote(realChmod)+" \"$@\"\n", 0755)
	script := strings.Replace(toolInstallScript, "dir="+councilToolDir, "dir="+shQuote(dir), 1)
	install := func(path string) *exec.Cmd {
		cmd := exec.Command("bash", "--noprofile", "--norc", "-c", script, "install", "claude", "2.1.332", digest(fixtureToolBody))
		cmd.Env = []string{"PATH=" + path}
		return cmd
	}
	waitFor := func(cond func() bool) {
		t.Helper()
		for range 500 {
			if cond() {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatal("timed out")
	}

	a := install(shim + ":" + os.Getenv("PATH"))
	a.Stdin = strings.NewReader(fixtureToolBody)
	var aOut bytes.Buffer
	a.Stdout, a.Stderr = &aOut, &aOut
	must(t, a.Start())
	waitFor(func() bool { _, err := os.Stat(reached); return err == nil })

	b := install(os.Getenv("PATH"))
	bIn, err := b.StdinPipe()
	must(t, err)
	must(t, b.Start())
	_, err = io.WriteString(bIn, fixtureToolBody[:5])
	must(t, err)
	// B has opened its staging file and written the first bytes.
	waitFor(func() bool {
		entries, _ := os.ReadDir(dir)
		for _, e := range entries {
			if info, err := e.Info(); err == nil && strings.HasPrefix(e.Name(), ".tool") && info.Size() == 5 {
				return true
			}
		}
		return false
	})

	writeFile(t, release, "", 0644)
	aErr := a.Wait()
	must(t, bIn.Close())
	bErr := b.Wait()

	bin, _ := os.ReadFile(filepath.Join(dir, "claude"))
	if aErr == nil && string(bin) != fixtureToolBody {
		t.Fatalf("install A reported success but the volume holds unchecked bytes %q (A: %s)", bin, aOut.String())
	}
	if aErr != nil || exitCode(bErr) != 3 {
		t.Fatalf("A: %v %q, B: %v", aErr, aOut.String(), bErr)
	}
	ver, _ := os.ReadFile(filepath.Join(dir, "VERSION"))
	if string(ver) != "2.1.332\n" {
		t.Fatalf("VERSION %q", ver)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 2 {
		t.Fatalf("staging files left: %v", entries)
	}
}

func TestCouncilLaunchWithTool(t *testing.T) {
	for _, tc := range []struct {
		name    string
		login   bool
		setup   func(a *Adapter, f *councilEngine)
		wantErr string
	}{
		{name: "turn"},
		{name: "login", login: true},
		{name: "tool volume missing labels", setup: func(_ *Adapter, f *councilEngine) { f.tool.Labels = nil }, wantErr: "tool volume does not belong"},
		{name: "tool volume of another harness", setup: func(_ *Adapter, f *councilEngine) { f.tool.Labels = CouncilToolVolumeLabels("agy") }, wantErr: "tool volume does not belong"},
		{name: "tool volume with login labels too", setup: func(_ *Adapter, f *councilEngine) { f.tool.Labels["sablecourt.io/harness"] = "claude" }, wantErr: "tool volume labels"},
		{name: "tool volume with an extra label", setup: func(_ *Adapter, f *councilEngine) { f.tool.Labels["com.docker.compose.project"] = "x" }, wantErr: "tool volume labels"},
		{name: "tool volume is the login volume", setup: func(a *Adapter, _ *councilEngine) {
			c := a.CouncilHarnesses["claude"]
			c.Tool.Volume = c.LoginVolume
		}, wantErr: "separate from every login volume"},
		{name: "bad digest", setup: func(a *Adapter, _ *councilEngine) { a.CouncilHarnesses["claude"].Tool.SHA256 = "abc" }, wantErr: "invalid council tool configuration"},
		{name: "hint with shell syntax", setup: func(a *Adapter, _ *councilEngine) { a.CouncilHarnesses["claude"].Tool.InstallCommand = "run $(id)" }, wantErr: "invalid council tool configuration"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, f, s := fixtureRun(t)
			cfg := a.CouncilHarnesses["claude"]
			cfg.Tool = fixtureTool()
			a.CouncilHarnesses["claude"] = cfg
			tv := toolVolume("claude", cfg.Tool.Volume)
			f.tool = &tv
			if tc.setup != nil {
				tc.setup(a, f)
			}
			var err error
			if tc.login {
				s.Checkout, s.Prompt, s.TaskToken, s.Model = rt.CouncilCheckout{}, "", "", ""
				_, err = a.RunCouncilLogin(t.Context(), s, func(context.Context, io.Reader, io.Writer) error { return nil }, discardResult)
			} else {
				_, err = a.RunCouncil(t.Context(), s, discardResult)
			}
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) || slices.Contains(f.calls, "start") {
					t.Fatalf("err = %v calls = %v", err, f.calls)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			h, c := f.createdOpts.HostConfig, f.createdOpts.Config
			want := mount.Mount{Type: mount.TypeVolume, Source: cfg.Tool.Volume, Target: councilToolDir, ReadOnly: true, VolumeOptions: &mount.VolumeOptions{NoCopy: true}}
			var found int
			for _, m := range h.Mounts {
				if m.Source == cfg.Tool.Volume {
					found++
					if m.Type != want.Type || m.Target != want.Target || !m.ReadOnly || m.VolumeOptions == nil || !m.VolumeOptions.NoCopy {
						t.Fatalf("tool mount = %+v", m)
					}
				}
			}
			if found != 1 {
				t.Fatalf("mounts = %+v", h.Mounts)
			}
			script := toolBootstrap
			if tc.login {
				script = toolGuardLogin
			}
			if len(c.Cmd) < 10 || c.Cmd[4] != script || !slices.Equal(c.Cmd[6:10], cfg.Tool.guardArgs()) || c.Cmd[10] != councilToolDir+"/claude" {
				t.Fatalf("cmd = %q", c.Cmd)
			}
			if slices.Contains(c.Cmd, "/usr/local/bin/claude") {
				t.Fatal("command still runs a tool from the image")
			}
			if !slices.Contains(c.Env, "DISABLE_UPDATES=1") {
				t.Fatalf("claude may update itself: env = %v", c.Env)
			}
		})
	}
}

// Claude Code must never replace itself: DISABLE_UPDATES=1 blocks background
// and manual updates. Only the claude seat gets it, in turns and at login.
func TestCouncilDisablesClaudeSelfUpdate(t *testing.T) {
	for _, harness := range []string{"claude", "codex", "agy"} {
		for _, login := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/login=%v", harness, login), func(t *testing.T) {
				a, f, s := fixtureRun(t)
				cfg := a.CouncilHarnesses["claude"]
				delete(a.CouncilHarnesses, "claude")
				f.vol.Name = "sablecourt-login-" + harness
				cfg.LoginVolume = f.vol.Name
				f.vol.Labels = CouncilLoginVolumeLabels(harness)
				a.CouncilHarnesses[harness] = cfg
				s.Harness = harness
				var err error
				if login {
					s.Checkout, s.Prompt, s.TaskToken, s.Model = rt.CouncilCheckout{}, "", "", ""
					_, err = a.RunCouncilLogin(t.Context(), s, func(context.Context, io.Reader, io.Writer) error { return nil }, discardResult)
				} else {
					_, err = a.RunCouncil(t.Context(), s, discardResult)
				}
				if err != nil {
					t.Fatal(err)
				}
				if got := slices.Contains(f.createdOpts.Config.Env, "DISABLE_UPDATES=1"); got != (harness == "claude") {
					t.Fatalf("DISABLE_UPDATES=1 set = %v for %s: env = %v", got, harness, f.createdOpts.Config.Env)
				}
			})
		}
	}
}

func TestCheckCouncilHarnessTool(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(*Adapter, *probeEngine)
		fault HarnessFault
		probe bool
	}{
		{name: "tool and login present", probe: true},
		{name: "tool volume missing", setup: func(_ *Adapter, p *probeEngine) { delete(p.volumes, "sablecourt-harness-claude-tool") }, fault: FaultToolMissing},
		{name: "tool volume unlabelled", setup: func(_ *Adapter, p *probeEngine) {
			p.volumes["sablecourt-harness-claude-tool"] = volume.Volume{Name: "sablecourt-harness-claude-tool", Driver: "local"}
		}, fault: FaultToolInvalid},
		{name: "tool not installed", setup: func(_ *Adapter, p *probeEngine) { p.exit = 3 }, fault: FaultToolMissing, probe: true},
		{name: "tool wrong version", setup: func(_ *Adapter, p *probeEngine) { p.exit = 4 }, fault: FaultToolVersion, probe: true},
		{name: "tool checksum mismatch", setup: func(_ *Adapter, p *probeEngine) { p.exit = 5 }, fault: FaultToolChecksum, probe: true},
		{name: "login missing after the tool check", setup: func(_ *Adapter, p *probeEngine) { p.exit = 1 }, fault: FaultLoginMissing, probe: true},
		{name: "invalid tool config", setup: func(a *Adapter, _ *probeEngine) { a.CouncilHarnesses["claude"].Tool.Version = "latest" }, fault: FaultBackend},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, p := probeFixture("claude")
			cfg := a.CouncilHarnesses["claude"]
			cfg.Tool = fixtureTool()
			a.CouncilHarnesses["claude"] = cfg
			p.volumes[cfg.Tool.Volume] = toolVolume("claude", cfg.Tool.Volume)
			if tc.setup != nil {
				tc.setup(a, p)
			}
			err := a.CheckCouncilHarness(t.Context(), "claude")
			var he *HarnessError
			if tc.fault == "" && err != nil || tc.fault != "" && (!errors.As(err, &he) || he.Fault != tc.fault) {
				t.Fatalf("err = %v, want %q", err, tc.fault)
			}
			if tc.probe != (len(p.created) == 1) {
				t.Fatalf("probes = %d", len(p.created))
			}
			if !tc.probe {
				return
			}
			o := p.created[0]
			if o.HostConfig.NetworkMode != "none" || len(o.HostConfig.Mounts) != 2 || !o.HostConfig.Mounts[0].ReadOnly || !o.HostConfig.Mounts[1].ReadOnly ||
				o.HostConfig.Mounts[1].Source != cfg.Tool.Volume || o.Config.Cmd[1] != toolProbeScript {
				t.Fatalf("probe = %+v %+v", o.HostConfig.Mounts, o.Config.Cmd)
			}
		})
	}
}

func TestInstallCouncilTool(t *testing.T) {
	for _, tc := range []struct {
		name    string
		setup   func(*Adapter, *probeEngine)
		exit    int
		wantErr string
		creates int
	}{
		{name: "creates the labelled volume and installs", creates: 1},
		{name: "reuses a labelled volume", setup: func(_ *Adapter, p *probeEngine) {
			p.volumes["sablecourt-harness-claude-tool"] = toolVolume("claude", "sablecourt-harness-claude-tool")
		}},
		{name: "refuses an unlabelled volume", setup: func(_ *Adapter, p *probeEngine) {
			p.volumes["sablecourt-harness-claude-tool"] = volume.Volume{Name: "sablecourt-harness-claude-tool", Driver: "local"}
		}, wantErr: "tool volume does not belong"},
		{name: "refuses the login volume", setup: func(_ *Adapter, p *probeEngine) {
			p.volumes["sablecourt-harness-claude-tool"] = volume.Volume{Name: "sablecourt-harness-claude-tool", Driver: "local", Labels: CouncilLoginVolumeLabels("claude")}
		}, wantErr: "tool volume does not belong"},
		{name: "in-container checksum fails", exit: 3, wantErr: "tool install exited with status 3", creates: 1},
		{name: "no tool configured", setup: func(a *Adapter, _ *probeEngine) {
			c := a.CouncilHarnesses["claude"]
			c.Tool = nil
			a.CouncilHarnesses["claude"] = c
		}, wantErr: "no tool to install"},
		{name: "image missing", setup: func(_ *Adapter, p *probeEngine) { p.imageErr = missing{errors.New("no image")} }, wantErr: "resolve harness image"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, p := probeFixture("claude")
			cfg := a.CouncilHarnesses["claude"]
			cfg.Tool = fixtureTool()
			a.CouncilHarnesses["claude"] = cfg
			p.exit = tc.exit
			if tc.setup != nil {
				tc.setup(a, p)
			}
			err := a.InstallCouncilTool(t.Context(), "claude", strings.NewReader(fixtureToolBody))
			if len(p.volCreate) != tc.creates {
				t.Fatalf("volume creates = %d", len(p.volCreate))
			}
			if tc.creates == 1 && p.volCreate[0].Labels["sablecourt.io/harness-tool"] != "claude" {
				t.Fatalf("created %+v", p.volCreate[0])
			}
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v", err)
				}
				if len(p.created) > 0 && p.removed != 1 {
					t.Fatal("install container not removed")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if p.channel.body.String() != fixtureToolBody || !p.channel.eof || p.removed != 1 {
				t.Fatalf("stdin=%q eof=%v removed=%d", p.channel.body.String(), p.channel.eof, p.removed)
			}
			o := p.created[0]
			h, c := o.HostConfig, o.Config
			if string(h.NetworkMode) != "none" || !h.ReadonlyRootfs || h.Privileged || !slices.Equal(h.CapDrop, []string{"ALL"}) ||
				!slices.Contains(h.SecurityOpt, "no-new-privileges:true") || c.User != "10001:10001" || c.Image != p.imageID || len(h.Binds) != 0 {
				t.Fatalf("install container is not isolated: %+v %+v", h, c)
			}
			want := mount.Mount{Type: mount.TypeVolume, Source: "sablecourt-harness-claude-tool", Target: councilToolDir}
			if len(h.Mounts) != 1 || h.Mounts[0] != want {
				t.Fatalf("mounts = %+v", h.Mounts)
			}
			if !slices.Equal(c.Env, councilBaseEnv) || !slices.Equal(c.Cmd, []string{"-c", toolInstallScript, "install", "claude", "2.1.332", digest(fixtureToolBody)}) {
				t.Fatalf("env=%v cmd=%q", c.Env, c.Cmd)
			}
		})
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func writeFile(t *testing.T, path, body string, mode os.FileMode) {
	t.Helper()
	_ = os.Remove(path)
	must(t, os.WriteFile(path, []byte(body), mode))
	must(t, os.Chmod(path, mode))
}

func exitCode(err error) int {
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return exit.ExitCode()
	}
	if err != nil {
		return -1
	}
	return 0
}

// shQuote returns s as one single-quoted shell word, so a temporary path with spaces stays one argument.
func shQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
