package sqlite

import (
	"os"
	"path/filepath"
	"testing"
)

// TestOpenRefusesQuestionMark covers the database row of the decision of
// 2026-11-27 on issue 181: the driver reads anything after "?" as options, so
// settings/linked.db?x=1 would open settings/linked.db (and follow it as a
// symlink) instead of the file the daemon resolved and created.
func TestOpenRefusesQuestionMark(t *testing.T) {
	dir := t.TempDir()
	checkout := filepath.Join(dir, "checkout")
	if err := os.Mkdir(checkout, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(checkout, "sablecourt.db"), filepath.Join(dir, "linked.db")); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{
		filepath.Join(dir, "linked.db?x=1"),
		filepath.Join(dir, "a?b", "sablecourt.db"),
		filepath.Join(dir, "sablecourt.db?mode=memory"),
	} {
		if st, err := Open(path); err == nil {
			_ = st.Close()
			t.Errorf("Open(%q) accepted a path with '?'", path)
		}
	}
	if _, err := os.Lstat(filepath.Join(checkout, "sablecourt.db")); err == nil {
		t.Fatal("a database was created inside the checkout")
	}
}

// TestMainFileReportsOpenedFile checks that MainFile names the file the
// connection opened, so the daemon can compare its identity.
func TestMainFileReportsOpenedFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sablecourt.db")
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	file, err := st.MainFile(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	want, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.Stat(file)
	if err != nil || !os.SameFile(got, want) {
		t.Fatalf("MainFile = %q, %v, want the file at %q", file, err, path)
	}
}
