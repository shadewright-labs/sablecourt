package redact_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/shadewright-labs/sablecourt/internal/redact"
	"github.com/shadewright-labs/sablecourt/internal/redact/redacttest"
)

func TestStringRedactsGeneratedCorpus(t *testing.T) {
	for _, tc := range redacttest.Corpus() {
		t.Run(tc.Name, func(t *testing.T) {
			out := redact.String(tc.Text)
			if frag, ok := redacttest.Leak(out, tc.Secrets); ok {
				t.Fatalf("secret fragment %q survived redaction: %q", frag, out)
			}
		})
	}
}

func TestStringWithholdsUnterminatedPrivateKey(t *testing.T) {
	for _, prefix := range []int{0, 1, 1500} {
		text, body := redacttest.UnterminatedKey(prefix)
		res := redact.Text(text)
		if frag, ok := redacttest.Leak(res.Text, []string{body}); ok {
			t.Fatalf("prefix %d: key fragment %q survived: %d bytes out", prefix, frag, len(res.Text))
		}
		if !strings.Contains(res.Text, "[REDACTED:private_key]") || res.Hits == 0 {
			t.Fatalf("prefix %d: no private key marker or hit: hits=%d", prefix, res.Hits)
		}
		if !strings.HasPrefix(res.Text, strings.Repeat("p", prefix)) {
			t.Fatalf("prefix %d: text before the key was not kept", prefix)
		}
	}
}

func TestJSONRedactsGeneratedCorpusAndStaysValid(t *testing.T) {
	for _, tc := range redacttest.Corpus() {
		t.Run(tc.Name, func(t *testing.T) {
			for name, input := range map[string]string{
				"raw":     tc.Text,
				"wrapped": mustJSON(t, map[string]string{"prompt": tc.Text}),
			} {
				out := redact.JSON(input)
				if frag, ok := redacttest.Leak(out, tc.Secrets); ok {
					t.Fatalf("%s: secret fragment %q survived: %q", name, frag, out)
				}
				if json.Valid([]byte(input)) && !json.Valid([]byte(out)) {
					t.Fatalf("%s: valid JSON became invalid: %q", name, out)
				}
			}
		})
	}
	text, body := redacttest.UnterminatedKey(10)
	in := mustJSON(t, map[string]any{"prompt": text, "n": 1})
	out := redact.JSON(in)
	if _, ok := redacttest.Leak(out, []string{body}); ok || !json.Valid([]byte(out)) {
		t.Fatalf("unterminated key in JSON: valid=%t out=%q", json.Valid([]byte(out)), out)
	}
	clean := `{"b":1, "a":"<ok>"}`
	if got := redact.JSON(clean); got != clean {
		t.Fatalf("clean JSON changed: %q", got)
	}
	if got := redact.JSON(`{"api_key_count":123456,"password":12345678}`); got != `{"api_key_count":123456,"password":"[REDACTED:secret]"}` {
		t.Fatalf("numeric secret field: %q", got)
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}
