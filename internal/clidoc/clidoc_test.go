package clidoc

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

const page = "intro\n<!-- BEGIN GENERATED a: do not edit, run `make docs` -->\n\nold a\n\n<!-- END GENERATED a -->\nmid\n<!-- BEGIN GENERATED b: do not edit, run `make docs` -->\n\nold b\n\n<!-- END GENERATED b -->\n"

func TestReplaceRefusesBrokenMarkers(t *testing.T) {
	for _, tc := range []struct {
		name, page string
	}{
		{"missing", "no markers"},
		{"reversed", "<!-- END GENERATED a -->\n<!-- BEGIN GENERATED a: do not edit, run `make docs` -->\n"},
		{"twice", page + page},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := replace([]byte(tc.page), "a", "x"); !errors.Is(err, errMarkers) {
				t.Fatalf("err = %v, want errMarkers", err)
			}
		})
	}
}

func TestCheckNeverWrites(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cli.md")
	if err := os.WriteFile(path, []byte(page), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Check(path, "a", "new a"); err == nil {
		t.Fatal("stale block passed")
	}
	if err := Check(path, "a", "old a"); err != nil {
		t.Fatalf("current block failed: %v", err)
	}
	got, _ := os.ReadFile(path)
	if string(got) != page {
		t.Fatal("Check changed the page")
	}
}

func TestWriteBlocks(t *testing.T) {
	for _, tc := range []struct {
		name    string
		blocks  map[string]string
		wantErr bool
	}{
		{"all blocks", map[string]string{"a": "new a", "b": "new b"}, false},
		{"block file missing", map[string]string{"a": "new a"}, true},
		{"block not on page", map[string]string{"a": "new a", "b": "new b", "c": "new c"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "cli.md")
			if err := os.WriteFile(path, []byte(page), 0o600); err != nil {
				t.Fatal(err)
			}
			dir := t.TempDir()
			for name, content := range tc.blocks {
				if err := SaveBlock(dir, name, content); err != nil {
					t.Fatal(err)
				}
			}
			err := WriteBlocks(path, dir)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if tc.wantErr {
				if got, _ := os.ReadFile(path); string(got) != page {
					t.Fatal("a failed write changed the page")
				}
				return
			}
			for name, content := range tc.blocks {
				if err := Check(path, name, content); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}
