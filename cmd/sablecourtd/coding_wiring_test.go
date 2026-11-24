package main

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/shadewright-labs/sablecourt/internal/attach"
)

func TestCodingAdapterWiresPersistentState(t *testing.T) {
	for _, retention := range []time.Duration{0, 72 * time.Hour} {
		t.Run(retention.String(), func(t *testing.T) {
			state := t.TempDir()
			hub := attach.NewHub()
			adapter := newCodingAdapter(hub, filepath.Join(state, "sablecourt.db"), retention)
			if adapter.WorktreeRoot != filepath.Join(state, "worktrees") || adapter.BranchRetention != retention || adapter.AttachHub != hub {
				t.Fatalf("coding adapter root = %q, retention = %s, attach hub wired = %t",
					adapter.WorktreeRoot, adapter.BranchRetention, adapter.AttachHub == hub)
			}
		})
	}
}
