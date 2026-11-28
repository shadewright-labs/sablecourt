package store_test

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"testing"
)

var migrationName = regexp.MustCompile(`^(\d{4})_[a-z0-9_]+\.sql$`)

// Numbers that already repeat or are skipped on main. Both drivers apply
// files in file-name order, so these keep working; a new repeat or gap is
// not listed and fails the test.
var (
	legacyRepeats = map[string]map[int]bool{
		"sqlite":   {15: true},
		"postgres": {11: true},
	}
	legacyGaps = map[string]map[int]bool{
		"postgres": {2: true, 3: true},
	}
)

func migrationNumbers(t *testing.T, dir string) []int {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names) // the order the drivers apply them in
	var nums []int
	for _, name := range names {
		m := migrationName.FindStringSubmatch(name)
		if m == nil {
			t.Errorf("%s: migration name must match NNNN_name.sql", filepath.Join(dir, name))
			continue
		}
		n, _ := strconv.Atoi(m[1])
		nums = append(nums, n)
	}
	return nums
}

func TestMigrationNumbersAreUniqueAndInOrder(t *testing.T) {
	for _, driver := range []string{"sqlite", "postgres"} {
		t.Run(driver, func(t *testing.T) {
			nums := migrationNumbers(t, filepath.Join(driver, "migrations"))
			if len(nums) == 0 {
				t.Fatal("no migrations found")
			}
			seen := map[int]int{}
			for _, n := range nums {
				seen[n]++
			}
			for i, n := range nums {
				switch {
				case i == 0:
					if n != 1 {
						t.Errorf("first migration is %04d, want 0001", n)
					}
				case n < nums[i-1]:
					t.Errorf("migration %04d comes after %04d: out of order", n, nums[i-1])
				case n == nums[i-1]:
					if !legacyRepeats[driver][n] || seen[n] != 2 {
						t.Errorf("migration number %04d is used %d times", n, seen[n])
					}
				case n > nums[i-1]+1:
					for g := nums[i-1] + 1; g < n; g++ {
						if !legacyGaps[driver][g] {
							t.Errorf("migration number %04d is skipped", g)
						}
					}
				}
			}
		})
	}
}
