package store

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// handWrittenClaim matches text that says what a request did or did not
// change around a failed audit write. Only AuditFailureText writes such text,
// from the effects recorded on the request (issue 179, decision of
// 2026-11-28).
var handWrittenClaim = regexp.MustCompile(`(?i)nothing (was |has been )?(changed|stored|created|spent|written|run)|did not run\b|could not be written|(was|were|is) not (changed|created|stored|run)\b|, but (an |its )?audit`)

// No production code outside audit.go writes a claim about what changed by
// hand: every such answer comes from AuditFailureText.
func TestNoHandWrittenAuditFailureClaims(t *testing.T) {
	own, err := filepath.Abs("audit.go")
	if err != nil {
		t.Fatal(err)
	}
	checked := 0
	for _, root := range []string{"../../cmd", "../../internal"} {
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			if abs, _ := filepath.Abs(path); abs == own {
				return nil
			}
			f, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
			if err != nil {
				return err
			}
			checked++
			ast.Inspect(f, func(n ast.Node) bool {
				lit, ok := n.(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					return true
				}
				text, err := strconv.Unquote(lit.Value)
				if err != nil {
					text = lit.Value
				}
				if handWrittenClaim.MatchString(text) {
					t.Errorf("%s: hand-written audit failure claim %q; answer with store.AuditFailureText", path, text)
				}
				return true
			})
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if checked < 50 {
		t.Fatalf("checked only %d files", checked)
	}
}

func TestHandWrittenClaimPattern(t *testing.T) {
	for _, bad := range []string{
		"promotion refused: its audit record could not be written, nothing was changed",
		"the tool ran, but its audit record could not be written",
		"action refused: its audit record could not be written, the action did not run",
		"task t1 was created, but an audit record could not be written",
		"nothing changed",
	} {
		if !handWrittenClaim.MatchString(bad) {
			t.Errorf("pattern misses %q", bad)
		}
	}
	for _, ok := range []string{"approval %s was spent", "task %s was created", "request cancelled"} {
		if handWrittenClaim.MatchString(ok) {
			t.Errorf("pattern matches effect note %q", ok)
		}
	}
}
