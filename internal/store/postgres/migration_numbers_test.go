package postgres

import (
	"testing"

	"github.com/shadewright-labs/sablecourt/internal/store/storetest"
)

// TestMigrationNumbers reads migrationFS, the files Open applies. It needs no
// database. The repeat and the gap predate this test: the repeat is pinned
// to its two files, and Postgres has no 0002 or 0003 file.
func TestMigrationNumbers(t *testing.T) {
	storetest.CheckMigrations(t, migrationFS, "migrations", storetest.MigrationRules{
		Repeats: map[int][]string{
			11: {"0011_budget_cache_tokens.sql", "0011_expire_legacy_approvals.sql"},
		},
		Gaps: []int{2, 3},
	})
}
