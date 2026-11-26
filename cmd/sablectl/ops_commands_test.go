package main

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// fakeBin writes an executable shell script named name into dir. The script
// appends its arguments to dir/<name>.args, prints out and exits with code.
func fakeBin(t *testing.T, dir, name, out string, code int) string {
	t.Helper()
	argsFile := filepath.Join(dir, name+".args")
	script := "#!/bin/sh\necho \"$@\" >> '" + argsFile + "'\nprintf '%s' '" + out + "'\nexit " + string(rune('0'+code)) + "\n"
	if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return argsFile
}

func readArgs(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return string(raw)
}

func TestTailscaleCommands(t *testing.T) {
	t.Run("expose serves only the local daemon port", func(t *testing.T) {
		bin := t.TempDir()
		args := fakeBin(t, bin, "tailscale", "https://node.tailnet.invalid -> 127.0.0.1:9000", 0)
		t.Setenv("PATH", bin)
		_, ts := buildOpsCommands()
		var out bytes.Buffer
		ts.SetOut(&out)
		ts.SetArgs([]string{"expose", "--port", "9000"})
		if err := ts.Execute(); err != nil {
			t.Fatalf("expose: %v", err)
		}
		got := readArgs(t, args)
		if got != "serve --bg --https=443 http://127.0.0.1:9000\nserve status\n" {
			t.Fatalf("tailscale calls = %q", got)
		}
		if !strings.Contains(out.String(), "exposed http://127.0.0.1:9000") || !strings.Contains(out.String(), "node.tailnet.invalid") {
			t.Fatalf("output = %q", out.String())
		}
	})
	t.Run("expose failure is reported", func(t *testing.T) {
		bin := t.TempDir()
		fakeBin(t, bin, "tailscale", "not logged in", 1)
		t.Setenv("PATH", bin)
		_, ts := buildOpsCommands()
		ts.SetOut(&bytes.Buffer{})
		ts.SetErr(&bytes.Buffer{})
		ts.SetArgs([]string{"expose"})
		err := ts.Execute()
		if err == nil || !strings.Contains(err.Error(), "tailscale serve failed: not logged in") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("status prints output", func(t *testing.T) {
		bin := t.TempDir()
		fakeBin(t, bin, "tailscale", "  no serve config  ", 0)
		t.Setenv("PATH", bin)
		_, ts := buildOpsCommands()
		var out bytes.Buffer
		ts.SetOut(&out)
		ts.SetArgs([]string{"status"})
		if err := ts.Execute(); err != nil || out.String() != "no serve config\n" {
			t.Fatalf("status: out=%q err=%v", out.String(), err)
		}
	})
	t.Run("status failure is reported", func(t *testing.T) {
		bin := t.TempDir()
		fakeBin(t, bin, "tailscale", "daemon down", 2)
		t.Setenv("PATH", bin)
		_, ts := buildOpsCommands()
		ts.SetOut(&bytes.Buffer{})
		ts.SetErr(&bytes.Buffer{})
		ts.SetArgs([]string{"status"})
		if err := ts.Execute(); err == nil || !strings.Contains(err.Error(), "tailscale serve status: daemon down") {
			t.Fatalf("err = %v", err)
		}
	})
	for _, sub := range []string{"expose", "status"} {
		t.Run(sub+" without tailscale", func(t *testing.T) {
			t.Setenv("PATH", t.TempDir())
			_, ts := buildOpsCommands()
			ts.SetOut(&bytes.Buffer{})
			ts.SetErr(&bytes.Buffer{})
			ts.SetArgs([]string{sub})
			if err := ts.Execute(); err == nil || !strings.Contains(err.Error(), "tailscale CLI not found on PATH") {
				t.Fatalf("err = %v", err)
			}
		})
	}
}

func TestServiceInstallWritesUnitAndReportsLoaderFailure(t *testing.T) {
	var unitRel, loader, wantFail string
	switch runtime.GOOS {
	case "darwin":
		unitRel, loader, wantFail = filepath.Join("Library", "LaunchAgents", "io.shadewright.sablecourtd.plist"), "launchctl", "launchctl load failed: refused"
	case "linux":
		unitRel, loader, wantFail = filepath.Join(".config", "systemd", "user", "sablecourtd.service"), "systemctl", "systemctl enable failed: refused"
	default:
		t.Skip("service installation is platform specific")
	}
	home, bin := t.TempDir(), t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("PATH", bin)
	fakeBin(t, bin, "sablecourtd", "", 0)
	loaderArgs := fakeBin(t, bin, loader, "refused", 1)

	service, _ := buildOpsCommands()
	service.SetOut(&bytes.Buffer{})
	service.SetErr(&bytes.Buffer{})
	service.SetArgs([]string{"install", "--listen", "127.0.0.1:7999"})
	err := service.Execute()
	if err == nil || !strings.Contains(err.Error(), wantFail) {
		t.Fatalf("err = %v, want %q", err, wantFail)
	}
	if readArgs(t, loaderArgs) == "" {
		t.Fatal("service loader was not called")
	}
	unit, err := os.ReadFile(filepath.Join(home, unitRel))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		filepath.Join(bin, "sablecourtd"),
		"--listen=127.0.0.1:7999",
		"--db=" + filepath.Join(home, ".local", "share", "sablecourt", "sablecourt.db"),
		"--manifests=" + filepath.Join(home, ".config", "sablecourt", "manifests"),
	} {
		if !strings.Contains(string(unit), want) {
			t.Errorf("unit does not contain %q:\n%s", want, unit)
		}
	}
	info, _ := os.Stat(filepath.Join(home, unitRel))
	if info.Mode().Perm() != 0o600 {
		t.Errorf("unit mode = %o, want 0600", info.Mode().Perm())
	}
}

func TestServiceInstallWithoutDaemonBinary(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("PATH", t.TempDir())
	service, _ := buildOpsCommands()
	service.SetOut(&bytes.Buffer{})
	service.SetErr(&bytes.Buffer{})
	service.SetArgs([]string{"install"})
	if err := service.Execute(); err == nil || !strings.Contains(err.Error(), "sablecourtd not found on PATH") {
		t.Fatalf("err = %v", err)
	}
}
