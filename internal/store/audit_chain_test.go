package store_test

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/shadewright-labs/sablecourt/internal/domain"
	"github.com/shadewright-labs/sablecourt/internal/store"
	"github.com/shadewright-labs/sablecourt/internal/store/postgres"
	"github.com/shadewright-labs/sablecourt/internal/store/sqlite"
)

type auditBackend struct {
	st        store.Store
	db        *sql.DB // raw handle used to tamper with rows
	migration string  // file name of the audit chain version 2 migration
}

// openAuditBackend opens a fresh, migrated store and a raw handle to its tables.
func openAuditBackend(t *testing.T, backend string) auditBackend {
	t.Helper()
	var b auditBackend
	var err error
	switch backend {
	case "sqlite":
		path := filepath.Join(t.TempDir(), "audit.db")
		if b.st, err = sqlite.Open(path); err == nil {
			b.db, err = sql.Open("sqlite", path)
		}
		b.migration = "0016_audit_chain_v2.sql"
	case "postgres":
		dsn := os.Getenv("SABLECOURT_TEST_POSTGRES_DSN")
		if dsn == "" {
			t.Skip("SABLECOURT_TEST_POSTGRES_DSN is unset")
		}
		key := isolatedPostgresDSN(t, dsn)
		if b.st, err = postgres.Open(key); err == nil {
			b.db, err = sql.Open("pgx", key)
		}
		b.migration = "0012_audit_chain_v2.sql"
	}
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = b.st.Close(); _ = b.db.Close() })
	if err := b.st.Migrate(t.Context()); err != nil {
		t.Fatal(err)
	}
	return b
}

func (b auditBackend) exec(t *testing.T, stmts ...string) {
	t.Helper()
	for _, s := range stmts {
		if _, err := b.db.ExecContext(t.Context(), s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
}

func (b auditBackend) count(t *testing.T) int {
	t.Helper()
	var n int
	if err := b.db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM audit_log`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// appendFour writes rows 1 to 4. Row 2 carries a '|' in its outcome so a test
// can move it into the action.
func appendFour(t *testing.T, st store.Store) domain.AuditChainHead {
	t.Helper()
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := range 4 {
		action, outcome := "action:git.open_pr", "allow"
		if i == 1 {
			action, outcome = "a", "b|c"
		}
		if err := st.AppendAudit(t.Context(), domain.AuditRecord{
			Category: "policy", WorkloadName: "worker", Action: action, Outcome: outcome,
			DetailJSON: fmt.Sprintf(`{"step":%d}`, i), CreatedAt: base.Add(time.Duration(i) * time.Second),
		}); err != nil {
			t.Fatalf("AppendAudit %d: %v", i, err)
		}
	}
	head, err := st.VerifyAuditChain(t.Context(), nil)
	if err != nil || head.Rows != 4 || head.Seq != 4 || head.LegacyRows != 0 {
		t.Fatalf("clean chain: head=%+v err=%v", head, err)
	}
	return head
}

func TestAuditChainTamperStores(t *testing.T) {
	cases := []struct {
		name    string
		tamper  []string
		anchor  bool // verify against the head recorded before tampering
		wantErr string
	}{
		{"edit", []string{`UPDATE audit_log SET outcome = 'deny' WHERE id = 2`}, false, "tampered"},
		{"edit id", []string{`UPDATE audit_log SET id = 10 WHERE id = 4`}, false, "tampered"},
		{"move pipe between fields", []string{`UPDATE audit_log SET action = 'a|b', outcome = 'c' WHERE id = 2`}, false, "tampered"},
		{"delete", []string{`DELETE FROM audit_log WHERE id = 2`}, false, "sequence"},
		{"reorder", []string{
			`UPDATE audit_log SET id = 100 WHERE id = 2`,
			`UPDATE audit_log SET id = 2 WHERE id = 3`,
			`UPDATE audit_log SET id = 3 WHERE id = 100`,
		}, false, "sequence"},
		{"edit sequence", []string{`UPDATE audit_log SET seq = 9 WHERE id = 4`}, false, "sequence"},
		{"blank one hash", []string{`UPDATE audit_log SET row_hash = '' WHERE id = 2`}, false, "no stored hash"},
		{"blank every hash", []string{`UPDATE audit_log SET row_hash = '', prev_hash = ''`}, false, "no stored hash"},
		{"downgrade hash version", []string{`UPDATE audit_log SET hash_version = 1 WHERE id = 4`}, false, "version 1 after"},
		{"truncate tail with anchor", []string{`DELETE FROM audit_log WHERE id = 4`}, true, "before anchor"},
		{"truncate two rows with anchor", []string{`DELETE FROM audit_log WHERE id >= 3`}, true, "before anchor"},
	}
	for _, backend := range []string{"sqlite", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			for _, tc := range cases {
				t.Run(tc.name, func(t *testing.T) {
					b := openAuditBackend(t, backend)
					head := appendFour(t, b.st)
					b.exec(t, tc.tamper...)
					var anchor *domain.AuditAnchor
					if tc.anchor {
						anchor = &domain.AuditAnchor{Seq: head.Seq, Hash: head.Hash}
					}
					_, err := b.st.VerifyAuditChain(t.Context(), anchor)
					if !errors.Is(err, domain.ErrAuditChainBroken) || !strings.Contains(err.Error(), tc.wantErr) {
						t.Fatalf("verify after %s = %v, want %q", tc.name, err, tc.wantErr)
					}
				})
			}
			t.Run("truncate tail without anchor is not seen", func(t *testing.T) {
				b := openAuditBackend(t, backend)
				appendFour(t, b.st)
				b.exec(t, `DELETE FROM audit_log WHERE id = 4`)
				if head, err := b.st.VerifyAuditChain(t.Context(), nil); err != nil || head.Seq != 3 {
					t.Fatalf("head=%+v err=%v", head, err)
				}
			})
			t.Run("anchor at the head passes and new rows extend it", func(t *testing.T) {
				b := openAuditBackend(t, backend)
				head := appendFour(t, b.st)
				if err := b.st.AppendAudit(t.Context(), domain.AuditRecord{Category: "policy", Action: "a", Outcome: "o", CreatedAt: time.Now()}); err != nil {
					t.Fatal(err)
				}
				got, err := b.st.VerifyAuditChain(t.Context(), &domain.AuditAnchor{Seq: head.Seq, Hash: head.Hash})
				if err != nil || got.Seq != 5 || got.Rows != 5 {
					t.Fatalf("head=%+v err=%v", got, err)
				}
			})
			t.Run("append refuses a head row without a hash", func(t *testing.T) {
				b := openAuditBackend(t, backend)
				appendFour(t, b.st)
				b.exec(t, `UPDATE audit_log SET row_hash = '' WHERE id = 4`)
				err := b.st.AppendAudit(t.Context(), domain.AuditRecord{Category: "policy", Action: "a", Outcome: "o", CreatedAt: time.Now()})
				if !errors.Is(err, domain.ErrAuditChainBroken) {
					t.Fatalf("append on a blank head = %v", err)
				}
				if n := b.count(t); n != 4 {
					t.Fatalf("refused append left %d rows", n)
				}
			})
		})
	}
}

// TestAuditChainMigrationStores rebuilds the schema as it was before the version
// 2 migration, writes rows the old way, and runs the migration again.
func TestAuditChainMigrationStores(t *testing.T) {
	type legacyRow struct {
		outcome string
		blank   bool // written before 0005_audit_chain.sql: no hash at all
	}
	cases := []struct {
		name       string
		rows       []legacyRow
		wantErr    string
		wantLegacy int
	}{
		{"hashed rows keep their version 1 hash", []legacyRow{{outcome: "allow"}, {outcome: "deny"}}, "", 2},
		{"row without hash fails verification", []legacyRow{{blank: true}, {outcome: "allow"}}, "no stored hash", 0},
		{"no rows", nil, "", 0},
	}
	for _, backend := range []string{"sqlite", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			for _, tc := range cases {
				t.Run(tc.name, func(t *testing.T) {
					b := openAuditBackend(t, backend)
					ph := func(n int) string {
						if backend == "postgres" {
							return fmt.Sprintf("$%d", n)
						}
						return "?"
					}
					b.exec(t,
						`DROP INDEX idx_audit_log_seq`,
						`ALTER TABLE audit_log DROP COLUMN seq`,
						`ALTER TABLE audit_log DROP COLUMN hash_version`,
						`DELETE FROM schema_migrations WHERE version = '`+b.migration+`'`,
					)
					prev := ""
					for i, r := range tc.rows {
						rec := domain.AuditRecord{Category: "policy", Action: "act", Outcome: r.outcome, DetailJSON: "{}"}
						created := fmt.Sprintf("2026-01-01T00:00:%02dZ", i)
						prevHash, rowHash := "", ""
						if !r.blank {
							prevHash = prev
							if prevHash == "" {
								prevHash = domain.AuditGenesis
							}
							rowHash = domain.ComputeAuditRowHashV1(prevHash, rec, created)
							prev = rowHash
						}
						q := fmt.Sprintf(`INSERT INTO audit_log (category, task_id, workload_name, action, outcome, detail_json, prev_hash, row_hash, created_at)
							VALUES (%s, '', '', %s, %s, '{}', %s, %s, %s)`, ph(1), ph(2), ph(3), ph(4), ph(5), ph(6))
						if _, err := b.db.ExecContext(t.Context(), q, rec.Category, rec.Action, rec.Outcome, prevHash, rowHash, created); err != nil {
							t.Fatal(err)
						}
					}
					if err := b.st.Migrate(t.Context()); err != nil {
						t.Fatalf("migrate: %v", err)
					}
					head, err := b.st.VerifyAuditChain(t.Context(), nil)
					if tc.wantErr != "" {
						if !errors.Is(err, domain.ErrAuditChainBroken) || !strings.Contains(err.Error(), tc.wantErr) {
							t.Fatalf("verify = %v, want %q", err, tc.wantErr)
						}
						return
					}
					if err != nil || head.LegacyRows != tc.wantLegacy || head.Rows != len(tc.rows) {
						t.Fatalf("after migration head=%+v err=%v", head, err)
					}
					// New rows are version 2 and chain onto the last older row.
					if err := b.st.AppendAudit(t.Context(), domain.AuditRecord{Category: "policy", Action: "a", Outcome: "o", CreatedAt: time.Now()}); err != nil {
						t.Fatal(err)
					}
					head, err = b.st.VerifyAuditChain(t.Context(), nil)
					if err != nil || head.Rows != len(tc.rows)+1 || head.Seq != int64(len(tc.rows)+1) || head.LegacyRows != tc.wantLegacy {
						t.Fatalf("after append head=%+v err=%v", head, err)
					}
					recs, err := b.st.ListAudit(t.Context(), 1)
					if err != nil || len(recs) != 1 || recs[0].HashVersion != domain.AuditHashV2 || recs[0].PrevHash != prevOr(prev) {
						t.Fatalf("newest row = %+v err=%v", recs, err)
					}
				})
			}
		})
	}
}

func prevOr(prev string) string {
	if prev == "" {
		return domain.AuditGenesis
	}
	return prev
}
