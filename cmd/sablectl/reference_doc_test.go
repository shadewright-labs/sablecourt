package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/shadewright-labs/sablecourt/internal/clidoc"
)

// TestCLIReferenceIsCurrent fails when the sablectl block of the CLI
// reference differs from the command tree. It only reads the page.
func TestCLIReferenceIsCurrent(t *testing.T) {
	page := filepath.Join("..", "..", "docs", "reference", "cli.md")
	if err := clidoc.Check(page, "sablectl", clidoc.Render(newRootCmd())); err != nil {
		t.Fatal(err)
	}
}

// TestRenderCLIBlock saves the rendered sablectl block into the directory
// named by SABLECOURT_CLI_BLOCK_DIR, for make docs to write into the page.
// Without the variable it is skipped.
func TestRenderCLIBlock(t *testing.T) {
	dir := os.Getenv("SABLECOURT_CLI_BLOCK_DIR")
	if dir == "" {
		t.Skip("SABLECOURT_CLI_BLOCK_DIR is not set")
	}
	if err := clidoc.SaveBlock(dir, "sablectl", clidoc.Render(newRootCmd())); err != nil {
		t.Fatal(err)
	}
}
