package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/shadewright-labs/sablecourt/internal/clidoc"
)

// TestCLIReferenceIsCurrent fails when the sablecourtd block of the CLI
// reference differs from the daemon's flags. It only reads the page.
func TestCLIReferenceIsCurrent(t *testing.T) {
	page := filepath.Join("..", "..", "docs", "reference", "cli.md")
	if err := clidoc.Check(page, "sablecourtd", clidoc.Render(newRootCmd())); err != nil {
		t.Fatal(err)
	}
}

// TestRenderCLIBlock saves the rendered sablecourtd block into the directory
// named by SABLECOURT_CLI_BLOCK_DIR, for make docs to write into the page.
// Without the variable it is skipped.
func TestRenderCLIBlock(t *testing.T) {
	dir := os.Getenv("SABLECOURT_CLI_BLOCK_DIR")
	if dir == "" {
		t.Skip("SABLECOURT_CLI_BLOCK_DIR is not set")
	}
	if err := clidoc.SaveBlock(dir, "sablecourtd", clidoc.Render(newRootCmd())); err != nil {
		t.Fatal(err)
	}
}
