package docker

import (
	"strings"
	"testing"

	"github.com/shadewright-labs/sablecourt/internal/redact"
)

// TestCommandOutputRedactionGrowthSetsOverflowAndRedactsCut: a stream under
// the limit whose redaction grows past it is cut, flagged as overflowed, and
// the cut line, where a word character hid a token, is dropped.
func TestCommandOutputRedactionGrowthSetsOverflowAndRedactsCut(t *testing.T) {
	t.Parallel()
	github := "gh" + "p_" + strings.Repeat("G", 36)
	// Each 16-byte line grows to 27 bytes once redacted. The raw stream,
	// 4059 bytes, fits the limit; the redacted one, 6809 bytes, does not,
	// and its cut falls right after the "x" that hides the token.
	const limit = 4108
	line := "pass" + "word=123456\n"
	input := strings.Repeat(line, 100) + "x" + github + "\n" + strings.Repeat("a", 16) + "\n" + strings.Repeat(line, 150)
	out := councilOutput{keepTail: true, limit: limit, lookback: 1024}
	if _, err := out.Write([]byte(input)); err != nil {
		t.Fatal(err)
	}
	got := out.visible("", "")
	if !out.overflow {
		t.Fatalf("out.overflow = false after visible truncated redacted output to %d bytes", len(got))
	}
	if strings.Contains(got, github) {
		t.Fatalf("visible exposed the token at the start of the cut tail: %.80q", got)
	}
}

// TestCommandOutputOverflowHidesUnfinishedPrivateKey: a key with no END line
// whose BEGIN line falls before the cut is still hidden.
func TestCommandOutputOverflowHidesUnfinishedPrivateKey(t *testing.T) {
	t.Parallel()
	header := "-----BEGIN PRIVATE KEY-----\n"
	body := "keybody-secret-line-1\nkeybody-secret-line-2\n"
	const limit = 4096
	padLen := limit + 10 - len(header) - len(body) - len("\nend\n")
	input := header + body + strings.Repeat("a", padLen) + "\nend\n"
	out := councilOutput{keepTail: true, limit: limit, lookback: 1024}
	if _, err := out.Write([]byte(input)); err != nil {
		t.Fatal(err)
	}
	if got := out.visible("", ""); strings.Contains(got, "keybody-secret") {
		t.Fatalf("unfinished private key body leaked after overflow: %.80q", got)
	}
}

// TestCommandOutputOverflowHidesMultilineSecrets: a secret whose key or
// header ends on the line the cut falls in, with its value on a later line,
// is redacted with its context before the cut.
func TestCommandOutputOverflowHidesMultilineSecrets(t *testing.T) {
	t.Parallel()
	secret := "SUPERSECRETVALUE12345"
	for name, prefix := range map[string]string{
		"bearer header across newline": "Authorization: Bearer\n" + secret + "\n",
		"json kv across newline":       "\"pass" + "word\":\n\"" + secret + "\"\n",
		"json multiline string value":  "\"pass" + "word\": \"first\n" + secret + "\"\n",
		"plain kv across newline":      "pass" + "word:\n" + secret + "\n",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			const limit = 4096
			padLen := limit + 4 - len(prefix) - len("\nend\n")
			input := prefix + strings.Repeat("f", padLen) + "\nend\n"
			if strings.Contains(redact.String(input), secret) {
				t.Fatalf("precondition failed: uncut input was not redacted")
			}
			out := councilOutput{keepTail: true, limit: limit, lookback: 1024}
			if _, err := out.Write([]byte(input)); err != nil {
				t.Fatal(err)
			}
			if got := out.visible("", ""); strings.Contains(got, secret) {
				t.Fatalf("multi-line secret leaked after overflow: %.80q", got)
			}
		})
	}
}
