package docker

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/shadewright-labs/sablecourt/internal/runtime/hostpath"
)

// TestDaemonFilePathSpellings runs one set of path spellings through the
// startup resolver (hostpath.ResolveFile, used for --db and --secret-file) and
// the checkout guard (safeBindSource with the spelling as a protected path).
// Both must agree on which file the spelling names: the startup resolver
// returns the one path the daemon opens or refuses, and the guard refuses a
// checkout that holds that file, or a spelling it cannot fully resolve.
func TestDaemonFilePathSpellings(t *testing.T) {
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	settings := filepath.Join(base, "settings")
	checkout := filepath.Join(base, "checkout")
	for _, dir := range []string{settings, filepath.Join(checkout, "sub"), filepath.Join(base, "home"), filepath.Join(base, "state")} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	symlink := func(target, link string) {
		t.Helper()
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}
	}
	// settings/link points at checkout/sub, so settings/link/.. is the checkout.
	symlink(filepath.Join(checkout, "sub"), filepath.Join(settings, "link"))
	// settings/inside points at the checkout itself.
	symlink(checkout, filepath.Join(settings, "inside"))
	// settings/dangling.json points at a file in the checkout that does not exist yet.
	symlink(filepath.Join(checkout, "dangling.json"), filepath.Join(settings, "dangling.json"))
	if err := os.WriteFile(filepath.Join(checkout, "existing.db"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	// settings/linked.db points at an existing database inside the checkout.
	symlink(filepath.Join(checkout, "existing.db"), filepath.Join(settings, "linked.db"))
	t.Setenv("HOME", filepath.Join(base, "home"))
	t.Setenv("SABLECOURT_STATE_DIR", filepath.Join(base, "state"))
	t.Chdir(settings)

	sep := string(filepath.Separator)
	const refuse, accept = true, false
	for _, tc := range []struct {
		name string
		raw  string
		// want is the path the startup resolver returns; "" means it refuses.
		want string
		// guardRefuses says whether the guard refuses to mount the checkout
		// with raw as a protected path.
		guardRefuses bool
	}{
		{name: "plain new file outside", raw: filepath.Join(settings, "sablecourt.db"), want: filepath.Join(settings, "sablecourt.db"), guardRefuses: accept},
		{name: "plain existing file inside", raw: filepath.Join(checkout, "existing.db"), want: filepath.Join(checkout, "existing.db"), guardRefuses: refuse},
		{name: "plain new file inside", raw: filepath.Join(checkout, "new.db"), want: filepath.Join(checkout, "new.db"), guardRefuses: refuse},
		{name: "trailing slash", raw: filepath.Join(checkout, "new.db") + sep, want: "", guardRefuses: refuse},
		{name: "dot-dot after a symlink, new file", raw: settings + sep + "link" + sep + ".." + sep + "sablecourt.db", want: filepath.Join(checkout, "sablecourt.db"), guardRefuses: refuse},
		{name: "dot-dot after a symlink, existing file", raw: settings + sep + "link" + sep + ".." + sep + "existing.db", want: filepath.Join(checkout, "existing.db"), guardRefuses: refuse},
		{name: "dot-dot after a symlink as the final component", raw: settings + sep + "link" + sep + "..", want: "", guardRefuses: refuse},
		{name: "symlinked parent", raw: filepath.Join(settings, "inside", "secrets.json"), want: filepath.Join(checkout, "secrets.json"), guardRefuses: refuse},
		{name: "dangling symlink final component", raw: filepath.Join(settings, "dangling.json"), want: "", guardRefuses: refuse},
		{name: "symlink to the database", raw: filepath.Join(settings, "linked.db"), want: "", guardRefuses: refuse},
		{name: "relative new file", raw: "relative.db", want: filepath.Join(settings, "relative.db"), guardRefuses: refuse},
		{name: "relative dot-dot after a symlink", raw: "link" + sep + ".." + sep + "relative.db", want: filepath.Join(checkout, "relative.db"), guardRefuses: refuse},
		{name: "missing parent", raw: filepath.Join(base, "missing", "sablecourt.db"), want: "", guardRefuses: refuse},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := hostpath.ResolveFile(tc.raw)
			switch {
			case tc.want == "" && err == nil:
				t.Errorf("startup: ResolveFile(%q) = %q, want a refusal", tc.raw, got)
			case tc.want != "" && (err != nil || got != tc.want):
				t.Errorf("startup: ResolveFile(%q) = %q, %v, want %q", tc.raw, got, err, tc.want)
			}
			_, err = safeBindSource(checkout, nil, []string{tc.raw})
			if refused := err != nil; refused != tc.guardRefuses {
				t.Errorf("guard: protected path %q refused=%v, want %v (err %v)", tc.raw, refused, tc.guardRefuses, err)
			}
		})
	}
}
