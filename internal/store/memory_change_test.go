package store_test

import (
	"errors"
	"testing"
	"time"

	"github.com/shadewright-labs/sablecourt/internal/domain"
	"github.com/shadewright-labs/sablecourt/internal/store"
)

// ChangeMemory commits the audit row and the memory writes together: a row
// the chain refuses leaves memory untouched, and a delete of a missing entry
// leaves no row (issue 228).
func TestChangeMemoryAuditRowInSameTransaction(t *testing.T) {
	row := func(action, outcome string) domain.AuditRecord {
		return domain.AuditRecord{Category: "memory", WorkloadName: "operator", Action: action, Outcome: outcome,
			DetailJSON: `{"namespace":"notes"}`, CreatedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	}
	cases := []struct {
		name      string
		change    store.MemoryChange
		breakHash bool
		wantErr   error
		wantRows  int // audit rows added
		wantK     string
		wantOld   bool
	}{
		{"put and delete commit with their row",
			store.MemoryChange{Audit: row("memory.put", "stored"), Put: []domain.MemoryEntry{{Namespace: "notes", Key: "k", Value: "new"}}, Delete: []store.MemoryKey{{Namespace: "notes", Key: "old"}}},
			false, nil, 1, "new", false},
		{"put refused when the row cannot be written",
			store.MemoryChange{Audit: row("memory.put", "stored"), Put: []domain.MemoryEntry{{Namespace: "notes", Key: "k", Value: "new"}}},
			true, store.ErrAuditWrite, 0, "seed", true},
		{"delete refused when the row cannot be written",
			store.MemoryChange{Audit: row("memory.delete", "deleted"), Delete: []store.MemoryKey{{Namespace: "notes", Key: "old"}}},
			true, store.ErrAuditWrite, 0, "seed", true},
		{"delete of a missing entry writes no row and changes nothing",
			store.MemoryChange{Audit: row("memory.delete", "deleted"), Put: []domain.MemoryEntry{{Namespace: "notes", Key: "k", Value: "new"}}, Delete: []store.MemoryKey{{Namespace: "notes", Key: "absent"}}},
			false, store.ErrNotFound, 0, "seed", true},
		{"empty change is refused",
			store.MemoryChange{Audit: row("memory.put", "stored")},
			false, store.ErrEmptyMemoryChange, 0, "seed", true},
	}
	for _, backend := range []string{"sqlite", "postgres"} {
		for _, tc := range cases {
			t.Run(backend+"/"+tc.name, func(t *testing.T) {
				b := openAuditBackend(t, backend)
				ctx := t.Context()
				for _, e := range []domain.MemoryEntry{{Namespace: "notes", Key: "k", Value: "seed"}, {Namespace: "notes", Key: "old", Value: "seed"}} {
					if err := b.st.PutMemory(ctx, e); err != nil {
						t.Fatal(err)
					}
				}
				if err := b.st.AppendAudit(ctx, row("seed", "ok")); err != nil {
					t.Fatal(err)
				}
				if tc.breakHash {
					b.exec(t, `UPDATE audit_log SET row_hash = ''`)
				}
				before := b.count(t)
				err := b.st.ChangeMemory(ctx, tc.change)
				if !errors.Is(err, tc.wantErr) || (tc.wantErr == nil) != (err == nil) {
					t.Fatalf("ChangeMemory = %v, want %v", err, tc.wantErr)
				}
				if tc.breakHash && !errors.Is(err, domain.ErrAuditChainBroken) {
					t.Fatalf("refusal %v does not name the broken chain", err)
				}
				if got := b.count(t) - before; got != tc.wantRows {
					t.Fatalf("audit rows added = %d, want %d", got, tc.wantRows)
				}
				if e, err := b.st.GetMemory(ctx, "notes", "k"); err != nil || e.Value != tc.wantK {
					t.Fatalf("notes/k = %+v %v, want value %q", e, err, tc.wantK)
				}
				_, err = b.st.GetMemory(ctx, "notes", "old")
				if tc.wantOld != (err == nil) {
					t.Fatalf("notes/old present=%v, want %v (%v)", err == nil, tc.wantOld, err)
				}
			})
		}
	}
}
