// Command write is the single writer of the CLI reference. It runs
// TestRenderCLIBlock in each command package, which saves that package's
// rendered block to its own file in a scratch directory, and then puts the
// blocks into the page with one write. make docs runs it from the
// repository root.
//
// Usage: go run ./internal/clidoc/write <page> <package>...
package main

import (
	"fmt"
	"os"
	"os/exec"

	"github.com/shadewright-labs/sablecourt/internal/clidoc"
)

func main() {
	if len(os.Args) < 3 {
		fmt.Fprintln(os.Stderr, "usage: write <page> <package>...")
		os.Exit(2)
	}
	if err := run(os.Args[1], os.Args[2:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(page string, pkgs []string) error {
	dir, err := os.MkdirTemp("", "sablecourt-cli-blocks-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	args := append([]string{"test", "-count=1", "-run", "^TestRenderCLIBlock$"}, pkgs...)
	cmd := exec.Command("go", args...) // #nosec G702 -- runs go test on the packages that make docs names
	cmd.Env = append(os.Environ(), "SABLECOURT_CLI_BLOCK_DIR="+dir)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("render blocks: %w", err)
	}
	return clidoc.WriteBlocks(page, dir)
}
