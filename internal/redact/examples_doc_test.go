package redact

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	redactExamplesStartMarker = "<!-- redact-examples:start -->"
	redactExamplesEndMarker   = "<!-- redact-examples:end -->"
)

type docExample struct {
	label        string
	input        string
	want         string
	displayInput string
	displayWant  string
}

func redactionDocExamples() ([]docExample, []string) {
	secret := joinParts("Zq9x", "K2mV", "7pLw")
	bearerSecret := joinParts("tok_", "fixture_", "01234567", "89abcdef")
	antTok := joinParts("sk-", "ant-", "api03-", "fixture0123", "456789abcdef")
	pemBody := joinParts("MIIE", "fixtureBody", "Line1")
	pem := pemBlock("RSA PRIVATE KEY", pemBody, "\n", true)
	jwt := joinParts(
		"eyJ", "hbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.",
		"eyJ", "zdWIiOiJmaXh0dXJlIn0.",
		"c2lnbmF0dXJlZml4dHVyZTEyMw",
	)
	ghpCRLF := joinParts("gh", "p_", "0123456789abcdefghij", "\r\n", "klmnopqrst")

	rawSecrets := []string{
		secret,
		bearerSecret,
		antTok,
		pemBody,
		jwt,
		"0123456789abcdefghij",
		"klmnopqrst",
	}

	rows := []docExample{
		{
			label:        "PEM private key block",
			input:        pem,
			want:         "[REDACTED:private_key]",
			displayInput: "-----BEGIN RSA ...-----\\n...\\n-----END ...",
		},
		{
			label:        "Prefixed token",
			input:        "key " + antTok,
			want:         "key [REDACTED:anthropic_key]",
			displayInput: "key sk-ant-...",
		},
		{
			label:        "URL userinfo password",
			input:        "https://alice:" + secret + "@example.com/p",
			want:         "https://[REDACTED:url_credential]@example.com/p",
			displayInput: "https://alice:...@example.com/p",
		},
		{
			label:        "Authorization header",
			input:        "Authorization: Bearer " + bearerSecret,
			want:         "Authorization: Bearer [REDACTED:bearer_token]",
			displayInput: "Authorization: Bearer ...",
		},
		{
			label:        "Cookie header",
			input:        "Cookie: sid=" + secret,
			want:         "Cookie: [REDACTED:cookie]",
			displayInput: "Cookie: sid=...",
		},
		{
			label:        "JWT",
			input:        "jwt " + jwt,
			want:         "jwt [REDACTED:jwt]",
			displayInput: "jwt eyJ... .eyJ... .sig...",
		},
		{
			label:        "Key=value secret",
			input:        "db_password=" + secret,
			want:         "db_password=[REDACTED:secret]",
			displayInput: "db_password=...",
		},
		{
			label: "Non-secret kept",
			input: "region: europe-west4",
			want:  "region: europe-west4",
		},
		{
			label: "Status prose kept",
			input: "Authorization: skipping verification",
			want:  "Authorization: skipping verification",
		},
		{
			label:        "Known gap: key synonym leaks",
			input:        "SECRET_KEY_BASE=" + secret,
			want:         "SECRET_KEY_BASE=" + secret,
			displayInput: "SECRET_KEY_BASE=<value>",
			displayWant:  "SECRET_KEY_BASE=<value>",
		},
		{
			label:        "Value in parentheses",
			input:        "password=(" + secret + ")",
			want:         "password=[REDACTED:secret]",
			displayInput: "password=(...)",
		},
		{
			label:        "Known gap: logfmt auth header leaks",
			input:        "authorization=Bearer " + secret,
			want:         "authorization=Bearer " + secret,
			displayInput: "authorization=Bearer <value>",
			displayWant:  "authorization=Bearer <value>",
		},
		{
			label:        "Known gap: scheme-less Authorization leaks",
			input:        "Authorization: " + secret,
			want:         "Authorization: " + secret,
			displayInput: "Authorization: <value>",
			displayWant:  "Authorization: <value>",
		},
		{
			label: "Known gap: short unquoted value leaks",
			input: "password=abc12",
			want:  "password=abc12",
		},
		{
			label: "Known gap: unquoted passphrase keeps later words",
			input: "passphrase: correct horse battery staple",
			want:  "passphrase: [REDACTED:secret] horse battery staple",
		},
		{
			label:        "Known gap: CRLF-split GitHub token leaks tail",
			input:        ghpCRLF,
			want:         "[REDACTED:github_token]\r\nklmnopqrst",
			displayInput: "ghp_...\\r\\n<tail>",
			displayWant:  "[REDACTED:github_token]\\r\\n<tail>",
		},
		{
			label: "Known gap: env var reference over-redacted",
			input: "password: ${DB_PASSWORD}",
			want:  "password: [REDACTED:secret]",
		},
		{
			label: "Known gap: -tokens suffix over-redacted",
			input: "max-tokens=123456",
			want:  "max-tokens=[REDACTED:secret]",
		},
	}

	return rows, rawSecrets
}

func renderExamplesTable(rows []docExample) string {
	var b strings.Builder
	b.WriteString("| Case | Input | Output |\n")
	b.WriteString("| --- | --- | --- |\n")
	for _, r := range rows {
		in := r.displayInput
		if in == "" {
			in = r.input
		}
		out := r.displayWant
		if out == "" {
			out = r.want
		}
		b.WriteString("| ")
		b.WriteString(escapeTableText(r.label))
		b.WriteString(" | ")
		b.WriteString(formatCodeCell(in))
		b.WriteString(" | ")
		b.WriteString(formatCodeCell(out))
		b.WriteString(" |\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

func escapeTableText(s string) string {
	return strings.ReplaceAll(s, "|", `\|`)
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
	if i < 0 || j < 0 || j < i {
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

func TestRedactionExamplesDoc(t *testing.T) {
	rows, rawSecrets := redactionDocExamples()
	for _, row := range rows {
		t.Run(row.label, func(t *testing.T) {
			if got := String(row.input); got != row.want {
				t.Fatalf("String(%q) = %q, want %q", row.input, got, row.want)
			}
		})
	}

	rendered := renderExamplesTable(rows)
	for _, s := range rawSecrets {
		if strings.Contains(rendered, s) {
			t.Fatalf("rendered examples table contains synthetic secret payload %q", s)
		}
	}

	docPath := filepath.Join("..", "..", "docs", "how-it-works", "tools-and-actions.md")
	page, err := os.ReadFile(docPath)
	if err != nil {
		t.Fatalf("read %s: %v", docPath, err)
	}
	wantPage, ok := replaceMarkedBlock(page, redactExamplesStartMarker, redactExamplesEndMarker, rendered)
	if !ok {
		t.Fatalf("%s: missing or malformed markers %q ... %q", docPath, redactExamplesStartMarker, redactExamplesEndMarker)
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
		t.Fatalf("%s redact-examples block is out of date; run UPDATE_DOCS=1 go test ./internal/redact -run TestRedactionExamplesDoc", docPath)
	}
}

func TestReplaceMarkedBlock(t *testing.T) {
	orig := []byte("before\n" + redactExamplesStartMarker + "\nold\n" + redactExamplesEndMarker + "\nafter\n")
	got, ok := replaceMarkedBlock(orig, redactExamplesStartMarker, redactExamplesEndMarker, "new content")
	if !ok {
		t.Fatal("replaceMarkedBlock returned ok=false")
	}
	want := "before\n" + redactExamplesStartMarker + "\n\nnew content\n\n" + redactExamplesEndMarker + "\nafter\n"
	if string(got) != want {
		t.Fatalf("got %q, want %q", string(got), want)
	}
	if _, ok := replaceMarkedBlock([]byte("no markers"), redactExamplesStartMarker, redactExamplesEndMarker, "x"); ok {
		t.Fatal("expected missing markers to fail")
	}
}
