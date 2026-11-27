package main

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// TestResolveDaemonFile checks that the daemon resolves its database and
// secret file paths to the file actually opened, so the checkout guard
// compares the same path. In link/../name the link is followed before "..",
// which a lexical clean (filepath.Abs) gets wrong.
func TestResolveDaemonFile(t *testing.T) {
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
	if err := os.WriteFile(filepath.Join(checkout, "existing.db"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(checkout, "existing.db"), filepath.Join(settings, "linked.db")); err != nil {
		t.Fatal(err)
	}
	sep := string(filepath.Separator)
	t.Chdir(settings)
	for _, tc := range []struct {
		name, raw, want string
		wantErr         bool
	}{
		{name: "empty", raw: "", want: ""},
		{name: "new file in a plain directory", raw: filepath.Join(settings, "sablecourt.db"), want: filepath.Join(settings, "sablecourt.db")},
		{name: "relative new file", raw: "sablecourt.db", want: filepath.Join(settings, "sablecourt.db")},
		{name: "symlink then dot-dot, new file", raw: settings + sep + "link" + sep + ".." + sep + "sablecourt.db", want: filepath.Join(checkout, "sablecourt.db")},
		{name: "symlink then dot-dot, existing file", raw: settings + sep + "link" + sep + ".." + sep + "existing.db", want: filepath.Join(checkout, "existing.db")},
		{name: "relative symlink then dot-dot", raw: "link" + sep + ".." + sep + "secrets.json", want: filepath.Join(checkout, "secrets.json")},
		{name: "symlinked file", raw: filepath.Join(settings, "linked.db"), want: filepath.Join(checkout, "existing.db")},
		{name: "missing parent", raw: filepath.Join(base, "missing", "sablecourt.db"), wantErr: true},
		{name: "names a directory entry, not a file", raw: filepath.Join(base, "missing") + sep + "..", wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolveDaemonFile(tc.raw)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("resolveDaemonFile(%q) = %q, want an error", tc.raw, got)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("resolveDaemonFile(%q) = %q, %v, want %q", tc.raw, got, err, tc.want)
			}
		})
	}

	// The protected list holds the resolved spelling, which is inside the checkout.
	db, err := resolveDaemonFile(settings + sep + "link" + sep + ".." + sep + "sablecourt.db")
	if err != nil {
		t.Fatal(err)
	}
	if paths := daemonPrivatePaths(filepath.Join(base, "state"), db, ""); !slices.Contains(paths, filepath.Join(checkout, "sablecourt.db")) {
		t.Fatalf("protected paths %v miss the database the daemon opens", paths)
	}
}
