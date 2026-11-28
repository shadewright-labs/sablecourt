package sqlite

import (
	"testing"

	"github.com/shadewright-labs/sablecourt/internal/store/storetest"
)

// TestMigrationNumbers reads migrationFS, the files Open applies. The one
// repeat predates this test and is pinned to its two files.
func TestMigrationNumbers(t *testing.T) {
	storetest.CheckMigrations(t, migrationFS, "migrations", storetest.MigrationRules{
		Repeats: map[int][]string{
			15: {"0015_budget_cache_tokens.sql", "0015_expire_legacy_approvals.sql"},
		},
	})
}
