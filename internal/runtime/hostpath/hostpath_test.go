package hostpath

import (
	"os"
	"path/filepath"
	"testing"
)

// TestCreateFile checks that a daemon file is created only at the resolved
// path: a planted symlink, dangling or not, or a directory is refused, and an
// existing regular file is kept as it is.
func TestCreateFile(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target.json")
	for _, tc := range []struct {
		name    string
		setup   func(path string)
		wantErr bool
		want    string
	}{
		{name: "new file", setup: func(string) {}, want: "{}\n"},
		{name: "existing file kept", setup: func(p string) { mustWrite(t, p, "kept") }, want: "kept"},
		{name: "dangling symlink", setup: func(p string) { mustSymlink(t, target, p) }, wantErr: true},
		{name: "symlink to a file", setup: func(p string) { mustWrite(t, target, "x"); mustSymlink(t, target, p) }, wantErr: true},
		{name: "directory", setup: func(p string) { mustMkdir(t, p) }, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_ = os.Remove(target)
			path := filepath.Join(t.TempDir(), "secrets.json")
			tc.setup(path)
			created, err := CreateFile(path, []byte("{}\n"))
			if tc.wantErr {
				if err == nil {
					t.Fatal("CreateFile accepted an unsafe entry")
				}
				if _, statErr := os.Stat(target); tc.name == "dangling symlink" && statErr == nil {
					t.Fatal("CreateFile wrote through a dangling symlink")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			got, err := os.ReadFile(path)
			if err != nil || string(got) != tc.want {
				t.Fatalf("content = %q, %v, want %q", got, err, tc.want)
			}
			info, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			if !os.SameFile(created, info) {
				t.Fatal("CreateFile returned the identity of another file")
			}
			if tc.name == "new file" && info.Mode().Perm() != 0o600 {
				t.Fatalf("mode = %v, want 0600", info.Mode().Perm())
			}
		})
	}
}

func mustWrite(t *testing.T, p, content string) {
	t.Helper()
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func mustSymlink(t *testing.T, target, link string) {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
}

func mustMkdir(t *testing.T, p string) {
	t.Helper()
	if err := os.Mkdir(p, 0o700); err != nil {
		t.Fatal(err)
	}
}
