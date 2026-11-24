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
