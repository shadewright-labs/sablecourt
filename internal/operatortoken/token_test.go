package operatortoken

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestLoadOrCreate(t *testing.T) {
	for _, writers := range []int{1, 8} {
		t.Run(map[int]string{1: "restart", 8: "concurrent startup"}[writers], func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "state")
			if err := os.Mkdir(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, Filename+".pending"), []byte("interrupted write"), 0o600); err != nil {
				t.Fatal(err)
			}
			var wg sync.WaitGroup
			tokens := make([]string, writers)
			for i := range tokens {
				wg.Go(func() {
					token, err := LoadOrCreate(dir)
					if err != nil {
						t.Error(err)
					}
					tokens[i] = token
				})
			}
			wg.Wait()
			got, err := Read(dir)
			if err != nil || len(got) != 67 || !strings.HasPrefix(got, "op_") {
				t.Fatalf("read generated credential: %v", err)
			}
			for _, token := range tokens {
				if token != got {
					t.Fatal("startup generated different credentials")
				}
			}
			if token, err := LoadOrCreate(dir); err != nil || token != got {
				t.Fatalf("restart did not reuse credential: %v", err)
			}
			info, err := os.Stat(filepath.Join(dir, Filename))
			if err != nil || info.Mode().Perm() != 0o600 {
				t.Fatalf("credential file must have mode 0600: %v", err)
			}
			if _, err := os.Lstat(filepath.Join(dir, Filename+".pending")); !os.IsNotExist(err) {
				t.Fatal("staging file survived a successful retry")
			}
		})
	}
}

func TestUnsafeTokenStateIsRefused(t *testing.T) {
	for _, name := range []string{"directory mode", "file mode", "directory symlink", "token symlink", "nonregular token", "empty", "null", "wrong class", "oversized"} {
		t.Run(name, func(t *testing.T) {
			base := t.TempDir()
			dir := filepath.Join(base, "state")
			mode := os.FileMode(0o700)
			if name == "directory mode" {
				mode = 0o755
			}
			if err := os.Mkdir(dir, mode); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, Filename)
			content := "op_" + strings.Repeat("a", 64) + "\n"
			write := true
			fileMode := os.FileMode(0o600)
			switch name {
			case "file mode":
				fileMode = 0o644
			case "directory symlink":
				link := filepath.Join(base, "linked")
				if err := os.Symlink(dir, link); err != nil {
					t.Fatal(err)
				}
			case "token symlink":
				target := filepath.Join(dir, "target")
				if err := os.WriteFile(target, []byte(content), 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(filepath.Base(target), path); err != nil {
					t.Fatal(err)
				}
				write = false
			case "nonregular token":
				if err := os.Mkdir(path, 0o700); err != nil {
					t.Fatal(err)
				}
				write = false
			case "empty":
				content = ""
			case "null":
				content = "null"
			case "wrong class":
				content = "task_" + strings.Repeat("a", 64)
			case "oversized":
				content += strings.Repeat("a", 8192)
			}
			if write {
				if err := os.WriteFile(path, []byte(content), fileMode); err != nil {
					t.Fatal(err)
				}
			}
			if name == "directory symlink" {
				dir = filepath.Join(base, "linked")
			}
			for _, open := range []func(string) (string, error){Read, LoadOrCreate} {
				if token, err := open(dir); err == nil || os.IsNotExist(err) || token != "" {
					t.Fatal("unsafe state must return a refusal")
				}
			}
		})
	}
}

func TestUnsafeTokenStateAncestorsRefused(t *testing.T) {
	validToken := "op_" + strings.Repeat("a", 64) + "\n"
	for _, tc := range []struct {
		name      string
		setup     func(t *testing.T, base string) string
		wantAllow bool
	}{
		{
			name: "symlinked parent",
			setup: func(t *testing.T, base string) string {
				realParent := filepath.Join(base, "real")
				state := filepath.Join(realParent, "state")
				if err := os.MkdirAll(state, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(state, Filename), []byte(validToken), 0o600); err != nil {
					t.Fatal(err)
				}
				sym := filepath.Join(base, "sym")
				if err := os.Symlink(realParent, sym); err != nil {
					t.Fatal(err)
				}
				return filepath.Join(sym, "state")
			},
		},
		{
			name: "trailing slash symlink",
			setup: func(t *testing.T, base string) string {
				state := filepath.Join(base, "state")
				if err := os.Mkdir(state, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(state, Filename), []byte(validToken), 0o600); err != nil {
					t.Fatal(err)
				}
				link := filepath.Join(base, "linked")
				if err := os.Symlink(state, link); err != nil {
					t.Fatal(err)
				}
				return link + string(os.PathSeparator)
			},
		},
		{
			name: "world-writable non-sticky parent",
			setup: func(t *testing.T, base string) string {
				parent := filepath.Join(base, "open")
				state := filepath.Join(parent, "state")
				if err := os.MkdirAll(state, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(parent, 0o777); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(state, Filename), []byte(validToken), 0o600); err != nil {
					t.Fatal(err)
				}
				return state
			},
		},
		{
			name: "group-writable non-sticky parent",
			setup: func(t *testing.T, base string) string {
				parent := filepath.Join(base, "group")
				state := filepath.Join(parent, "state")
				if err := os.MkdirAll(state, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(parent, 0o770); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(state, Filename), []byte(validToken), 0o600); err != nil {
					t.Fatal(err)
				}
				return state
			},
		},
		{
			name: "sticky writable parent",
			setup: func(t *testing.T, base string) string {
				parent := filepath.Join(base, "sticky")
				state := filepath.Join(parent, "state")
				if err := os.MkdirAll(state, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(parent, os.ModeSticky|0o777); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(state, Filename), []byte(validToken), 0o600); err != nil {
					t.Fatal(err)
				}
				return state
			},
			wantAllow: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := tc.setup(t, t.TempDir())
			for _, open := range []func(string) (string, error){Read, LoadOrCreate} {
				token, err := open(dir)
				if tc.wantAllow {
					if err != nil || token != strings.TrimSpace(validToken) {
						t.Fatalf("expected valid sticky parent to succeed, got token=%q err=%v", token, err)
					}
					continue
				}
				if err == nil || os.IsNotExist(err) || token != "" {
					t.Fatalf("unsafe ancestor path %q must be refused, got token=%q err=%v", dir, token, err)
				}
			}
		})
	}

	t.Run("LoadOrCreate does not create leaf under unsafe parent", func(t *testing.T) {
		base := t.TempDir()
		openParent := filepath.Join(base, "open")
		if err := os.Mkdir(openParent, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(openParent, 0o777); err != nil {
			t.Fatal(err)
		}
		target := filepath.Join(openParent, "newstate")
		if token, err := LoadOrCreate(target); err == nil || token != "" {
			t.Fatalf("LoadOrCreate accepted unsafe parent: token=%q err=%v", token, err)
		}
		if _, err := os.Lstat(target); !os.IsNotExist(err) {
			t.Fatalf("LoadOrCreate created directory inside unsafe parent: %v", err)
		}
	})
}

func TestStateDirCreationIgnoresRacedSymlink(t *testing.T) {
	for _, name := range []string{"intermediate", "leaf"} {
		t.Run(name, func(t *testing.T) {
			base := t.TempDir()
			shared := filepath.Join(base, "shared")
			outside := filepath.Join(base, "outside")
			for _, d := range []string{shared, outside} {
				if err := os.Mkdir(d, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.Chmod(shared, 0o777|os.ModeSticky); err != nil {
				t.Fatal(err)
			}
			// The path was checked while missing; another user now plants a link in the shared parent.
			rel := filepath.Join("a", "state")
			link := filepath.Join(shared, "a")
			if name == "leaf" {
				rel = "state"
				link = filepath.Join(shared, "state")
			}
			if err := os.Symlink(outside, link); err != nil {
				t.Fatal(err)
			}
			err := mkdirBelow(shared, rel)
			if name == "intermediate" && err == nil {
				t.Fatal("creation followed a symlink out of the verified ancestor")
			}
			entries, readErr := os.ReadDir(outside)
			if readErr != nil || len(entries) != 0 {
				t.Fatalf("symlink target was modified: %v %v", entries, readErr)
			}
		})
	}
}

func TestStateDirCreationUnderStickyParent(t *testing.T) {
	shared := filepath.Join(t.TempDir(), "shared")
	if err := os.Mkdir(shared, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(shared, 0o777|os.ModeSticky); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(shared, "a", "state")
	lock, err := LockStateDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	lock.Close()
	if _, err := LoadOrCreate(filepath.Join(shared, "b", "c", "state")); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(dir)
	if err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("state directory mode: %v %v", info, err)
	}
}
