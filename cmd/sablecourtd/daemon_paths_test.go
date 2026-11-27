package main

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/shadewright-labs/sablecourt/internal/runtime/hostpath"
	"github.com/shadewright-labs/sablecourt/internal/store/sqlite"
)

// TestDaemonPrivatePathsHoldResolvedFiles checks that the protected list holds
// the path the daemon opens, not the configured spelling. The path spellings
// themselves are covered by TestDaemonFilePathSpellings in the docker package.
func TestDaemonPrivatePathsHoldResolvedFiles(t *testing.T) {
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	settings := filepath.Join(base, "settings")
	checkout := filepath.Join(base, "checkout")
	for _, dir := range []string{settings, filepath.Join(checkout, "sub")} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	// settings/link points into the checkout, so settings/link/../x opens checkout/x.
	if err := os.Symlink(filepath.Join(checkout, "sub"), filepath.Join(settings, "link")); err != nil {
		t.Fatal(err)
	}
	sep := string(filepath.Separator)
	db, err := hostpath.ResolveFile(settings + sep + "link" + sep + ".." + sep + "sablecourt.db")
	if err != nil {
		t.Fatal(err)
	}
	paths := daemonPrivatePaths(filepath.Join(base, "state"), db, "")
	for _, want := range []string{filepath.Join(checkout, "sablecourt.db"), checkout, filepath.Join(checkout, "worktrees")} {
		if !slices.Contains(paths, want) {
			t.Fatalf("protected paths %v miss %s", paths, want)
		}
	}
}

// TestDaemonStartRefusesDaemonFilesUnderWorktreeRoot covers the startup and
// database rows of the decision of 2026-11-27 on issue 181: the daemon does
// not start when its database, secret file or state directory lies under the
// worktree root or shares identity with it, or when --db holds a "?".
func TestDaemonStartRefusesDaemonFilesUnderWorktreeRoot(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, dbDir string) (cfg daemonConfig)
		want  string
	}{
		{name: "question mark in the database path", want: "must not contain '?'", setup: func(t *testing.T, dbDir string) daemonConfig {
			return daemonConfig{dbPath: filepath.Join(dbDir, "sablecourt.db?mode=rwc")}
		}},
		{name: "secret file under the worktree root", want: "under the worktree root", setup: func(t *testing.T, dbDir string) daemonConfig {
			mustMkdirAll(t, filepath.Join(dbDir, "worktrees"))
			return daemonConfig{dbPath: filepath.Join(dbDir, "sablecourt.db"), secretFile: filepath.Join(dbDir, "worktrees", "secrets.json")}
		}},
		{name: "state directory under the worktree root", want: "under the worktree root", setup: func(t *testing.T, dbDir string) daemonConfig {
			t.Setenv("SABLECOURT_STATE_DIR", filepath.Join(dbDir, "worktrees", "state"))
			return daemonConfig{dbPath: filepath.Join(dbDir, "sablecourt.db")}
		}},
		{name: "worktree root is the database directory", want: "under the worktree root", setup: func(t *testing.T, dbDir string) daemonConfig {
			if err := os.Symlink(dbDir, filepath.Join(dbDir, "worktrees")); err != nil {
				t.Fatal(err)
			}
			return daemonConfig{dbPath: filepath.Join(dbDir, "sablecourt.db")}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			t.Setenv("SABLECOURT_STATE_DIR", filepath.Join(t.TempDir(), "state"))
			dbDir, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			cfg := tc.setup(t, dbDir)
			cfg.runtimeBackend = "k8s"
			cfg.listenAddr = "127.0.0.1:0"
			err = runDaemon(t.Context(), cfg)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("runDaemon = %v, want an error containing %q", err, tc.want)
			}
			// A refused start creates neither the secret file nor the database.
			for _, path := range []string{cfg.secretFile, filepath.Join(dbDir, "sablecourt.db")} {
				if path == "" {
					continue
				}
				if _, err := os.Lstat(path); !os.IsNotExist(err) {
					t.Fatalf("refused start left %s behind: %v", filepath.Base(path), err)
				}
			}
		})
	}
}

// TestSameDatabaseFile checks the identity test run after sqlite.Open: the
// file SQLite reports for the main database must be the file CreateFile made.
func TestSameDatabaseFile(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "sablecourt.db")
	created, err := hostpath.CreateFile(db, nil)
	if err != nil {
		t.Fatal(err)
	}
	other, err := hostpath.CreateFile(filepath.Join(dir, "other.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	st, err := sqlite.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := sameDatabaseFile(t.Context(), st, created); err != nil {
		t.Fatalf("same file refused: %v", err)
	}
	if err := sameDatabaseFile(t.Context(), st, other); err == nil {
		t.Fatal("a different file accepted as the database")
	}
}

func mustMkdirAll(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
}
