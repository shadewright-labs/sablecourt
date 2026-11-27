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
)

// knownGapDocs is the numbered list of known gaps in
// docs/how-it-works/tools-and-actions.md. Every row of TestKnownGaps and
// TestCorpusKnownGaps names one entry by key, and every entry is pinned by at
// least one row, so the page lists exactly the gaps the tests pin.
var knownGapDocs = []struct{ key, text string }{
	{"key-synonyms", "Key synonyms are not redacted: `SECRET_KEY_BASE=...`, `ENCRYPTION_KEY=...` and `master_key: ...` keep their value."},
	{"no-break-space", "A no-break space (U+00A0) between key and value is not a separator, so `password:` followed by a no-break space and a value keeps the value."},
	{"ampersand-value", "An unquoted value stops at `&`, which reads as the start of the next query parameter: `password=&value` keeps the value, and so does `password=abc&value`, because `abc` is shorter than six characters."},
	{"double-escaped-json", "Double-escaped JSON, a JSON document embedded in a JSON string so that its quotes are escaped twice, keeps its secret fields."},
	{"folded-cookie", "A `Cookie` header folded onto a continuation line (`Cookie:` then a line break) keeps its value."},
	{"logfmt-headers", "Logfmt pairs with `=` instead of `:` are not header matches: `authorization=Bearer ...` and `cookie=\"sid=...\"` keep their value."},
	{"scheme-less-authorization", "An `Authorization` header with no scheme word (`Authorization: value`) keeps its value."},
	{"curl-flags", "`curl --proxy-user user:value` and the cookie flags `-b` and `--cookie` keep their value."},
	{"unquoted-first-word", "An unquoted value is redacted only up to its first whitespace, and only when it is six or more characters, so later words of a passphrase and short values stay."},
	{"marker-splits-value", "When a secret value contains a token that is redacted first, the text after the marker stays: `password=<value>-<aws key>xyz` ends in `[REDACTED:aws_access_key]xyz`."},
	{"github-token-line-break", "A GitHub token split by a line break can leak: with 20 characters after the prefix before a CRLF, the tail after the break stays; with only the prefix before the break, the characters after it stay."},
	{"quoted-value-cut", "A quoted value that is cut short or spans lines can leak: an unterminated `password=\"abc12` keeps `abc12`, and a multiline double-quoted value stops at a continuation line that holds `:` or `=` (`password=\"abc`, a new line, `def:ghi\"` keeps `ghi`)."},
	{"line-break-separator", "A line break between key and separator (`password`, a new line, `= value`) or after the separator keeps the value when the key does not start a YAML line: `password=`, `x secret :` or `{\"password\":`, then the value on the next line. A YAML value on the line after a `password:` line is redacted."},
	{"status-word-value", "After a `:` separator, a lowercase word of up to 15 letters that ends like a status word (`-ed`, `-ing`, `-tion`, `-sion`, `-ment`, `-ness`, `-able`, `-ible`) is read as log prose and kept: `password: dragonslaying` keeps its value."},
	{"call-shaped-value", "An unquoted value shaped like a function call is kept: in `f(password=Xy9(value)` the value reads as a call to `Xy9`."},
	{"path-shaped-value", "An unquoted value shaped like a file path (starting with `/`, `./`, `../` or `~/`, with short path segments) is kept: `password=/x9/value` keeps it."},
	{"token-split-by-space", "A provider token split by a space leaks the part after the space: only the characters before the space match the token shape."},
	{"url-password-at-and-slash", "A URL password that holds both `@` and `/` leaks its tail: in `postgres://app:p@ss/word@example.com` the text from the first `@` on reads as a host and a path."},
	{"unlabelled-hex", "An unlabelled opaque hex string (`deploy <40 hex characters>`) is kept, because it cannot be told apart from a Git SHA, a UUID or image data."},
	{"flow-items-at-opener-indent", "In a `{` or `[` collection that spans lines, a line indented no deeper than the line that opened it ends the collection unless it starts with `}`, `]` or `,`, so an item on such a line keeps its value. This keeps an unclosed collection from hiding the rest of the output."},
	{"second-pass-noise", "Redaction is not always idempotent when invalid UTF-8 or NUL bytes sit next to quotes: redacting the output again can redact more of it (`seCret:'[REDACTED:secret]'`, an invalid byte and `'` lose the text after the marker). No secret text comes back."},
	{"tokens-suffix", "A key name that ends in `-tokens` is treated like `tokens`, so `max-tokens=123456` loses its value."},
	{"references-over-redacted", "Variable references and secret names are over-redacted: `password: ${DB_PASSWORD}`, `token=$API_TOKEN`, `Authorization: Bearer $API_TOKEN` and `existingSecret: postgres-secret` lose their text."},
	{"go-assignment-over-redacted", "Go short assignments from an expression are over-redacted in diffs and source: `password := cfg.Password` loses `cfg.Password`."},
}

func renderKnownGapList() string {
	var b strings.Builder
	for i, g := range knownGapDocs {
		fmt.Fprintf(&b, "%d. %s\n", i+1, g.text)
	}
	return strings.TrimRight(b.String(), "\n")
}

func TestKnownGapsDoc(t *testing.T) {
	docKeys := map[string]bool{}
	for _, g := range knownGapDocs {
		if docKeys[g.key] {
			t.Fatalf("duplicate known gap key %q", g.key)
		}
		docKeys[g.key] = true
	}
	pinned := map[string]bool{}
	for _, r := range knownGapRows() {
		pinned[r.gap] = true
	}
	for _, r := range corpusKnownGapRows() {
		pinned[r.gap] = true
	}
	for k := range pinned {
		if !docKeys[k] {
			t.Errorf("pinned gap %q has no entry in knownGapDocs", k)
		}
	}
	for k := range docKeys {
		if !pinned[k] {
			t.Errorf("knownGapDocs entry %q is not pinned by any row of TestKnownGaps or TestCorpusKnownGaps", k)
		}
	}

	docPath := filepath.Join("..", "..", "docs", "how-it-works", "tools-and-actions.md")
	page, err := os.ReadFile(docPath)
	if err != nil {
		t.Fatalf("read %s: %v", docPath, err)
	}
	wantPage, ok := replaceMarkedBlock(page, redactGapsStartMarker, redactGapsEndMarker, renderKnownGapList())
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
		t.Fatalf("%s redact-gaps list is out of date; run UPDATE_DOCS=1 go test ./internal/redact -run 'TestKnownGapsDoc|TestRedactionExamplesDoc'", docPath)
	}
}
