package operatortoken

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLockStateDirIsExclusive(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")

	first, err := LockStateDir(dir)
	if err != nil {
		t.Fatalf("first lock: %v", err)
	}
	info, err := os.Stat(dir)
	if err != nil || info.Mode() != os.ModeDir|0o700 {
		t.Fatalf("state directory = %v, %v; want a 0700 directory created by the lock", info, err)
	}
	if _, err := LockStateDir(dir); err == nil || !strings.Contains(err.Error(), "already locked") {
		t.Fatalf("second lock = %v, want already locked", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	again, err := LockStateDir(dir)
	if err != nil {
		t.Fatalf("lock after release: %v", err)
	}
	_ = again.Close()
}

func TestLockStateDirRepairsModeAndRefusesUnsafeFiles(t *testing.T) {
	t.Run("loose mode is tightened", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "state")
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		lockPath := filepath.Join(dir, LockFilename)
		if err := os.WriteFile(lockPath, nil, 0o644); err != nil {
			t.Fatal(err)
		}
		l, err := LockStateDir(dir)
		if err != nil {
			t.Fatalf("LockStateDir: %v", err)
		}
		defer l.Close() //nolint:errcheck // test cleanup
		info, err := os.Stat(lockPath)
		if err != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("lock file mode = %v, %v; want 0600", info.Mode(), err)
		}
	})

	t.Run("directory in place of the lock file", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "state")
		if err := os.MkdirAll(filepath.Join(dir, LockFilename), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if l, err := LockStateDir(dir); err == nil {
			_ = l.Close()
			t.Fatal("LockStateDir accepted a directory as its lock file")
		}
	})

	t.Run("symlinked lock file", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "state")
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		outside := filepath.Join(t.TempDir(), "elsewhere")
		if err := os.WriteFile(outside, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, filepath.Join(dir, LockFilename)); err != nil {
			t.Fatal(err)
		}
		if l, err := LockStateDir(dir); err == nil {
			_ = l.Close()
			t.Fatal("LockStateDir followed a symlink out of the state directory")
		}
	})

	t.Run("relative path", func(t *testing.T) {
		if _, err := LockStateDir("relative/state"); err == nil || !strings.Contains(err.Error(), "absolute") {
			t.Fatalf("LockStateDir = %v, want absolute path refusal", err)
		}
	})

	t.Run("loose state directory", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "state")
		if err := os.Mkdir(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if _, err := LockStateDir(dir); err == nil || !strings.Contains(err.Error(), "mode 0700") {
			t.Fatalf("LockStateDir = %v, want mode refusal", err)
		}
	})

	t.Run("world writable parent", func(t *testing.T) {
		parent := filepath.Join(t.TempDir(), "shared")
		if err := os.Mkdir(parent, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(parent, 0o777); err != nil {
			t.Fatal(err)
		}
		if _, err := LockStateDir(filepath.Join(parent, "state")); err == nil || !strings.Contains(err.Error(), "unsafe ownership or permissions") {
			t.Fatalf("LockStateDir = %v, want unsafe parent refusal", err)
		}
	})
}

func TestStateDir(t *testing.T) {
	cases := []struct {
		name    string
		env     string
		home    string
		want    string
		wantErr string
	}{
		{"absolute override is cleaned", "/srv/sc/../state/", "", "/srv/state", ""},
		{"relative override refused", "state", "", "", "absolute"},
		{"default under home", "", "/home/op", "/home/op/.local/share/sablecourt", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("SABLECOURT_STATE_DIR", tc.env)
			t.Setenv("HOME", tc.home)
			got, err := StateDir()
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("StateDir = %q, %v; want %q", got, err, tc.wantErr)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("StateDir = %q, %v; want %q", got, err, tc.want)
			}
		})
	}
}

func TestReadTokenContent(t *testing.T) {
	good := "op_" + strings.Repeat("0f", 32)
	cases := []struct {
		name    string
		content string
		want    string
		wantErr bool
	}{
		{"with newline", good + "\n", good, false},
		{"without newline", good, good, false},
		{"not hex", "op_" + strings.Repeat("zz", 32) + "\n", "", true},
		{"one byte short", good[:len(good)-1], "", true},
		{"two newlines", good + "\n\n", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "state")
			if err := os.Mkdir(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, Filename), []byte(tc.content), 0o600); err != nil {
				t.Fatal(err)
			}
			got, err := Read(dir)
			if tc.wantErr {
				if err == nil || got != "" {
					t.Fatalf("Read = %q, %v; want a refusal", got, err)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("Read = %q, %v; want %q", got, err, tc.want)
			}
		})
	}

	t.Run("missing token", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "state")
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if _, err := Read(dir); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("Read = %v, want ErrNotExist", err)
		}
	})

	t.Run("missing directory is not created by Read", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "absent")
		if _, err := Read(dir); err == nil {
			t.Fatal("Read of a missing directory succeeded")
		}
		if _, err := os.Lstat(dir); !os.IsNotExist(err) {
			t.Fatal("Read created the state directory")
		}
	})
}
