package sqlite

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/shadewright-labs/sablecourt/internal/domain"
)

func TestCourtMemoryCRUDAndSearch(t *testing.T) {
	ctx := context.Background()
	st, err := Open(filepath.Join(t.TempDir(), "mem.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	if err := st.PutMemory(ctx, domain.MemoryEntry{
		Namespace: "sablecourt",
		Key:       "testing",
		Value:     "Always run go test -race ./...",
		Author:    "operator",
		UpdatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("PutMemory: %v", err)
	}

	got, err := st.GetMemory(ctx, "sablecourt", "testing")
	if err != nil || got.Value != "Always run go test -race ./..." {
		t.Fatalf("GetMemory mismatch: %+v err=%v", got, err)
	}

	hits, err := st.SearchMemory(ctx, "sablecourt", "race", 10)
	if err != nil || len(hits) != 1 {
		t.Fatalf("SearchMemory expected 1 hit, got %d err=%v", len(hits), err)
	}

	if err := st.DeleteMemory(ctx, "sablecourt", "testing"); err != nil {
		t.Fatalf("DeleteMemory: %v", err)
	}
	if _, err := st.GetMemory(ctx, "sablecourt", "testing"); err == nil {
		t.Fatal("expected error after DeleteMemory")
	}
}
