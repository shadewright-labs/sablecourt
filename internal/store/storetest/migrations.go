package storetest

import (
	"io/fs"
	"os"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"testing"
)

// migrationName is the only accepted migration file name. The fixed-width
// number makes file-name order, the order the drivers apply migrations in,
// the same as number order.
var migrationName = regexp.MustCompile(`^(\d{4})_[a-z0-9_]+\.sql$`)

// MigrationRules lists the numbering exceptions a backend already ships.
type MigrationRules struct {
	// Repeats maps a number used more than once to the exact file names
	// that share it. Any other file at that number fails.
	Repeats map[int][]string
	// Gaps lists numbers that have no file. Adding a file at one fails.
	Gaps []int
}

// CheckMigrations checks the migrations a backend loads. loaded is the
// backend's embedded file system, read at "migrations" as the backend reads
// it; diskDir is the source directory the embed directive compiles from.
//
// Every loaded entry must be a regular file named NNNN_name.sql, every
// entry of diskDir must be loaded (so a directory or a misnamed file the
// backend would skip fails), numbers start at 0001 and only the repeats and
// gaps in rules are allowed, pinned to their exact files.
func CheckMigrations(t *testing.T, loaded fs.FS, diskDir string, rules MigrationRules) {
	t.Helper()
	entries, err := fs.ReadDir(loaded, "migrations")
	if err != nil {
		t.Fatalf("read embedded migrations: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("no migrations embedded")
	}

	byNumber := map[int][]string{}
	loadedNames := map[string]bool{}
	maxNum := 0
	for _, e := range entries {
		name := e.Name()
		loadedNames[name] = true
		if !e.Type().IsRegular() {
			t.Errorf("migrations/%s: is not a regular file", name)
			continue
		}
		m := migrationName.FindStringSubmatch(name)
		if m == nil {
			t.Errorf("migrations/%s: name must match NNNN_name.sql", name)
			continue
		}
		n, _ := strconv.Atoi(m[1])
		byNumber[n] = append(byNumber[n], name)
		maxNum = max(maxNum, n)
	}

	disk, err := os.ReadDir(diskDir)
	if err != nil {
		t.Fatalf("read %s: %v", diskDir, err)
	}
	for _, e := range disk {
		if !e.Type().IsRegular() {
			t.Errorf("%s/%s: is not a regular file", diskDir, e.Name())
			continue
		}
		if !loadedNames[e.Name()] {
			t.Errorf("%s/%s: is not loaded by the backend; name it NNNN_name.sql", diskDir, e.Name())
		}
	}

	for n := 1; n <= maxNum; n++ {
		got := byNumber[n]
		sort.Strings(got)
		want := slices.Clone(rules.Repeats[n])
		sort.Strings(want)
		isGap := slices.Contains(rules.Gaps, n)
		switch {
		case isGap && len(got) > 0:
			t.Errorf("migration number %04d must stay unused, found %v", n, got)
		case isGap:
		case len(got) == 0:
			t.Errorf("migration number %04d is skipped", n)
		case want != nil && !slices.Equal(got, want):
			t.Errorf("migration number %04d: files %v, want exactly %v", n, got, want)
		case want == nil && len(got) > 1:
			t.Errorf("migration number %04d is used %d times: %v", n, len(got), got)
		}
	}
	for n, want := range rules.Repeats {
		if n > maxNum {
			t.Errorf("migration number %04d: files [], want exactly %v", n, want)
		}
	}
	for _, n := range rules.Gaps {
		if n > maxNum {
			t.Errorf("gap %04d is past the last migration %04d", n, maxNum)
		}
	}
}
