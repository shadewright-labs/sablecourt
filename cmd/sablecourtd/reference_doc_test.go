package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/shadewright-labs/sablecourt/internal/clidoc"
)

// TestCLIReferenceIsCurrent fails when the sablecourtd block of the CLI
// reference differs from the daemon's flags. SABLECOURT_UPDATE_DOCS=1
// rewrites the block instead (make docs).
func TestCLIReferenceIsCurrent(t *testing.T) {
	page := filepath.Join("..", "..", "docs", "reference", "cli.md")
	update := os.Getenv("SABLECOURT_UPDATE_DOCS") == "1"
	if err := clidoc.Sync(page, "sablecourtd", clidoc.Render(newRootCmd()), update); err != nil {
		t.Fatal(err)
	}
}
