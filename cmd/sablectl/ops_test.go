package main

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestServiceDirectoryPermissions(t *testing.T) {
	var dir string
	switch runtime.GOOS {
	case "darwin":
		dir = filepath.Join("Library", "LaunchAgents")
	case "linux":
		dir = filepath.Join(".config", "systemd", "user")
	default:
		t.Skip("service installation is platform specific")
	}
	home, bin := t.TempDir(), t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("PATH", bin)
	for _, name := range []string{"sablecourtd", "launchctl", "systemctl"} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	service, _ := buildOpsCommands()
	service.SetArgs([]string{"install"})
	if err := service.Execute(); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(home, dir))
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o700 {
		t.Fatalf("permissions = %o, want 0700", got)
	}
}
