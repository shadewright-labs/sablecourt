package redact

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	redactGapsStartMarker = "<!-- redact-gaps:start -->"
	redactGapsEndMarker   = "<!-- redact-gaps:end -->"
	maxGapExamples        = 3
)

// renderKnownGapTable builds the table of known gaps for the docs from the
// rows of knownGapRows: one line per class with the row count and up to
// maxGapExamples examples.
func renderKnownGapTable() string {
	rows := knownGapRows()
	var b strings.Builder
	b.WriteString("| Class | Rows | Examples |\n| --- | --- | --- |\n")
	for _, class := range gapClasses {
		count := 0
		var examples []string
		for _, r := range rows {
			if r.class != class {
				continue
			}
			count++
			if r.example != "" && len(examples) < maxGapExamples {
				examples = append(examples, formatCodeCell(r.example))
			}
		}
		fmt.Fprintf(&b, "| %s | %d | %s |\n", class, count, strings.Join(examples, ", "))
	}
	return strings.TrimRight(b.String(), "\n")
}

func formatCodeCell(s string) string {
	s = strings.ReplaceAll(s, "\r", `\r`)
	s = strings.ReplaceAll(s, "\n", `\n`)
	s = strings.ReplaceAll(s, "|", `\|`)
	return "`" + s + "`"
}

func replaceMarkedBlock(page []byte, startMarker, endMarker, content string) ([]byte, bool) {
	startBytes := []byte(startMarker)
	endBytes := []byte(endMarker)
	if bytes.Count(page, startBytes) != 1 || bytes.Count(page, endBytes) != 1 {
		return nil, false
	}
	i := bytes.Index(page, startBytes)
	j := bytes.Index(page, endBytes)
	if j < i {
		return nil, false
	}
	var out bytes.Buffer
	out.Write(page[:i+len(startBytes)])
	out.WriteString("\n\n")
	out.WriteString(strings.TrimSpace(content))
	out.WriteString("\n\n")
	out.Write(page[j:])
	return out.Bytes(), true
}

func TestReplaceMarkedBlock(t *testing.T) {
	orig := []byte("a\n" + redactGapsStartMarker + "\nold\n" + redactGapsEndMarker + "\nb\n")
	got, ok := replaceMarkedBlock(orig, redactGapsStartMarker, redactGapsEndMarker, "new")
	if !ok {
		t.Fatal("replaceMarkedBlock returned ok=false")
	}
	want := "a\n" + redactGapsStartMarker + "\n\nnew\n\n" + redactGapsEndMarker + "\nb\n"
	if string(got) != want {
		t.Fatalf("got %q, want %q", got, want)
	}
	if _, ok := replaceMarkedBlock([]byte("no markers"), redactGapsStartMarker, redactGapsEndMarker, "x"); ok {
		t.Fatal("expected ok=false without markers")
	}
}

// TestKnownGapsDoc fails when the table in the Redaction section of
// docs/how-it-works/tools-and-actions.md differs from the one generated from
// the gap table. UPDATE_DOCS=1 rewrites the page.
func TestKnownGapsDoc(t *testing.T) {
	docPath := filepath.Join("..", "..", "docs", "how-it-works", "tools-and-actions.md")
	page, err := os.ReadFile(docPath)
	if err != nil {
		t.Fatalf("read %s: %v", docPath, err)
	}
	wantPage, ok := replaceMarkedBlock(page, redactGapsStartMarker, redactGapsEndMarker, renderKnownGapTable())
	if !ok {
		t.Fatalf("%s: missing or malformed markers %q ... %q", docPath, redactGapsStartMarker, redactGapsEndMarker)
	}
	if os.Getenv("UPDATE_DOCS") == "1" {
		if !bytes.Equal(page, wantPage) {
			if err := os.WriteFile(docPath, wantPage, 0o644); err != nil {
				t.Fatalf("write %s: %v", docPath, err)
			}
		}
		return
	}
	if !bytes.Equal(page, wantPage) {
		t.Fatalf("%s redact-gaps table is out of date; run UPDATE_DOCS=1 go test ./internal/redact -run TestKnownGapsDoc", docPath)
	}
}
