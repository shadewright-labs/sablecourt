package redact

import (
	"regexp"
	"strings"
	"testing"
	"unicode/utf8"
)

// TestTail checks the order Tail owns: redact the whole, cut, drop the
// partial first line, redact again until stable, and report every cut.
func TestTail(t *testing.T) {
	t.Parallel()
	github := "gh" + "p_" + strings.Repeat("G", 36)
	grow := "api" + "_key=abcdef\n" // 14 bytes, 25 once redacted
	// atStart redacts a token only at the start of the text, so only a
	// second pass after the cut can find it.
	atStart := regexp.MustCompile(`\Atok[0-9]+`)
	startOnly := func(s string) string { return atStart.ReplaceAllString(s, "[REDACTED:start]") }
	for name, tc := range map[string]struct {
		text        string
		from, limit int
		clean       func(string) string
		want        string
		cut         bool
	}{
		"fits":                      {text: "ok " + github + "\n", limit: 100, want: "ok [REDACTED:github_token]\n"},
		"empty":                     {text: "", limit: 10, want: ""},
		"cut keeps the whole lines": {text: strings.Repeat("a", 30) + "\nlast\n", limit: 10, want: "last\n", cut: true},
		"no whole line left":        {text: strings.Repeat("a", 30), limit: 10, want: "", cut: true},
		"redaction growth is a cut": {text: grow + grow + "ok", limit: 32, want: "api_key=[REDACTED:secret]\nok", cut: true},
		// The token starts in the context and ends after from: it is
		// redacted whole and none of it is shown.
		"secret across from":  {text: "ctx " + github + " rest\nnext\n", from: 10, limit: 100, want: "next\n", cut: true},
		"from with no secret": {text: "ctx\nkept line\n", from: 2, limit: 100, want: "kept line\n", cut: true},
		"from past the end":   {text: "abc", from: 10, limit: 100, want: "", cut: true},
		"key END after the cut": {
			text: "-----BEGIN " + "PRIVATE KEY-----\nkeybody\n-----END " + "PRIVATE KEY-----\ndone\n",
			from: 20, limit: 100, clean: func(s string) string { return s }, want: "\ndone\n", cut: true,
		},
		"second pass after the cut": {text: "x\ntok123 left\n", from: 1, limit: 100, clean: startOnly, want: "[REDACTED:start] left\n", cut: true},
		"never settles":             {text: "a\nb\n", limit: 2, clean: func(s string) string { return s + "\n" }, want: "", cut: true},
		"multi-byte at the cut":     {text: strings.Repeat("é", 20) + "\nend\n", limit: 8, want: "end\n", cut: true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got, cut := Tail(tc.text, tc.from, tc.limit, tc.clean)
			if got != tc.want || cut != tc.cut {
				t.Fatalf("Tail = %q, %v; want %q, %v", got, cut, tc.want, tc.cut)
			}
			if len(got) > tc.limit || !utf8.ValidString(got) {
				t.Fatalf("Tail = %q: over the limit or not valid UTF-8", got)
			}
		})
	}
}
