package main

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/shadewright-labs/sablecourt/internal/runtime/hostpath"
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
	for _, want := range []string{filepath.Join(checkout, "sablecourt.db"), filepath.Join(checkout, "worktrees")} {
		if !slices.Contains(paths, want) {
			t.Fatalf("protected paths %v miss %s", paths, want)
		}
	}
}

// TestResolveExistingPathKeepsSymlinkBeforeDotDot checks the council repo
// overlap guard's resolver: it must not clean a path before resolving it, so
// in link/../name the ".." applies to the link's target even when name does
// not exist yet, and a "." or ".." past the existing prefix is refused.
func TestResolveExistingPathKeepsSymlinkBeforeDotDot(t *testing.T) {
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
	if err := os.Symlink(filepath.Join(checkout, "sub"), filepath.Join(settings, "link")); err != nil {
		t.Fatal(err)
	}
	sep := string(filepath.Separator)
	for _, tc := range []struct {
		name, raw, want string
	}{
		{name: "plain missing tail", raw: filepath.Join(settings, "a", "b.json"), want: filepath.Join(settings, "a", "b.json")},
		{name: "symlink then dot-dot, missing file", raw: settings + sep + "link" + sep + ".." + sep + "new.db", want: filepath.Join(checkout, "new.db")},
		{name: "symlink then dot-dot, missing directory", raw: settings + sep + "link" + sep + ".." + sep + "x" + sep + "new.db", want: filepath.Join(checkout, "x", "new.db")},
		{name: "dot-dot past the existing prefix", raw: settings + sep + "missing" + sep + ".." + sep + "new.db", want: ""},
		{name: "relative", raw: "settings" + sep + "new.db", want: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolveExistingPath(tc.raw)
			if tc.want == "" {
				if err == nil {
					t.Fatalf("resolveExistingPath(%q) = %q, want an error", tc.raw, got)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("resolveExistingPath(%q) = %q, %v, want %q", tc.raw, got, err, tc.want)
			}
		})
	}
}
