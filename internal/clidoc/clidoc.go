// Package clidoc renders a cobra command tree as Markdown for the CLI
// reference and keeps generated blocks of a page in sync with the code.
package clidoc

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// Render returns the Markdown reference of root and every visible command
// below it. Hidden commands, hidden or deprecated flags and the help flag
// that every command has are left out.
// Cobra's default help and completion commands are added first, so the
// output matches what a user can run. Cobra adds the completion command to
// a root without subcommands only when it is called, so Render asks for it
// by name: "sablecourtd completion bash" works and is listed.
func Render(root *cobra.Command) string {
	root.InitDefaultHelpCmd()
	root.InitDefaultCompletionCmd("completion")

	var b strings.Builder
	cmds := visible(root)
	for i, c := range cmds {
		if i > 0 {
			b.WriteString("\n")
		}
		writeCommand(&b, c)
	}
	return b.String()
}

func visible(root *cobra.Command) []*cobra.Command {
	var out []*cobra.Command
	var walk func(*cobra.Command)
	walk = func(c *cobra.Command) {
		if c.Hidden || c.Deprecated != "" {
			return
		}
		out = append(out, c)
		children := append([]*cobra.Command(nil), c.Commands()...)
		sort.Slice(children, func(i, j int) bool { return children[i].Name() < children[j].Name() })
		for _, child := range children {
			walk(child)
		}
	}
	walk(root)
	return out
}

func writeCommand(b *strings.Builder, c *cobra.Command) {
	usage := c.CommandPath()
	if _, args, ok := strings.Cut(c.Use, " "); ok {
		usage += " " + args
	}
	fmt.Fprintf(b, "### `%s`\n\n", usage)
	if short := strings.TrimSpace(c.Short); short != "" {
		fmt.Fprintf(b, "%s\n\n", cell(short))
	}
	if len(c.Aliases) > 0 {
		fmt.Fprintf(b, "Aliases: `%s`.\n\n", strings.Join(c.Aliases, "`, `"))
	}
	if !c.Runnable() {
		b.WriteString("Groups the commands below; it does nothing on its own.\n\n")
	}
	flags := collectFlags(c)
	if len(flags) == 0 {
		b.WriteString("No flags of its own.\n")
		return
	}
	b.WriteString("| Flag | Type | Default | Description |\n| --- | --- | --- | --- |\n")
	for _, f := range flags {
		name := "`--" + f.Name + "`"
		if f.Shorthand != "" {
			name = "`-" + f.Shorthand + "`, " + name
		}
		fmt.Fprintf(b, "| %s | %s | %s | %s |\n", name, f.Value.Type(), defaultText(f), cell(f.Usage))
	}
}

// collectFlags returns the flags defined on c itself (local and persistent),
// without the inherited ones, which are listed on the command that defines
// them.
func collectFlags(c *cobra.Command) []*pflag.Flag {
	var out []*pflag.Flag
	seen := map[string]bool{}
	add := func(f *pflag.Flag) {
		if f.Hidden || f.Deprecated != "" || f.Name == "help" || seen[f.Name] {
			return
		}
		seen[f.Name] = true
		out = append(out, f)
	}
	c.LocalNonPersistentFlags().VisitAll(add)
	c.PersistentFlags().VisitAll(add)
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func defaultText(f *pflag.Flag) string {
	d := f.DefValue
	switch f.Value.Type() {
	case "bool":
		if d == "false" {
			return "`false`"
		}
	case "stringSlice", "stringArray":
		if d == "[]" {
			return "none"
		}
	}
	if d == "" {
		return "empty"
	}
	return "`" + d + "`"
}

func cell(s string) string {
	s = strings.ReplaceAll(s, "|", `\|`)
	return strings.Join(strings.Fields(s), " ")
}

// markers delimit a generated block inside a hand-written page.
func markers(name string) (begin, end string) {
	return "<!-- BEGIN GENERATED " + name + ": do not edit, run `make docs` -->",
		"<!-- END GENERATED " + name + " -->"
}

// errMarkers reports a page without a complete pair of markers.
var errMarkers = errors.New("generated block markers missing or out of order")

// replace returns page with the block called name replaced by content.
func replace(page []byte, name, content string) ([]byte, error) {
	begin, end := markers(name)
	i := bytes.Index(page, []byte(begin))
	j := bytes.Index(page, []byte(end))
	if i < 0 || j < 0 || j < i || bytes.Count(page, []byte(begin)) != 1 || bytes.Count(page, []byte(end)) != 1 {
		return nil, fmt.Errorf("%s: %w", name, errMarkers)
	}
	var out bytes.Buffer
	out.Write(page[:i+len(begin)])
	out.WriteString("\n\n")
	out.WriteString(strings.TrimSpace(content))
	out.WriteString("\n\n")
	out.Write(page[j:])
	return out.Bytes(), nil
}

// Check returns an error when the block called name in the file at path
// differs from content. It never writes.
func Check(path, name, content string) error {
	page, err := os.ReadFile(path) // #nosec G304 -- a repository path chosen by the test
	if err != nil {
		return err
	}
	want, err := replace(page, name, content)
	if err != nil {
		return err
	}
	if !bytes.Equal(page, want) {
		return fmt.Errorf("%s: generated block %q is stale; run `make docs` and commit the result", path, name)
	}
	return nil
}

// blockExt is the extension of a rendered block file in the directory that
// WriteBlocks reads.
const blockExt = ".md"

// SaveBlock writes content as the block called name into dir, as
// <name>.md. Each command package saves its own file, so packages that run
// in parallel never write the same file.
func SaveBlock(dir, name, content string) error {
	return os.WriteFile(filepath.Join(dir, name+blockExt), []byte(content), 0o600)
}

// WriteBlocks replaces every generated block of the page at path with the
// file of the same name in dir and writes the page once. It is the only
// code that writes the page. Every block of the page needs a file, and every
// file needs a block.
func WriteBlocks(path, dir string) error {
	page, err := os.ReadFile(path) // #nosec G304 -- a repository path given by make docs
	if err != nil {
		return err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	out := page
	n := 0
	for _, e := range entries {
		name, ok := strings.CutSuffix(e.Name(), blockExt)
		if !ok || e.IsDir() {
			continue
		}
		content, err := os.ReadFile(filepath.Join(dir, e.Name())) // #nosec G304 -- a file in the block directory
		if err != nil {
			return err
		}
		if out, err = replace(out, name, string(content)); err != nil {
			return err
		}
		n++
	}
	if blocks := bytes.Count(page, []byte("<!-- BEGIN GENERATED ")); n != blocks {
		return fmt.Errorf("%s has %d generated blocks, %s holds %d block files", path, blocks, dir, n)
	}
	if bytes.Equal(page, out) {
		return nil
	}
	return os.WriteFile(path, out, 0o600) // #nosec G703 -- the page path that make docs passes, a repository file
}
