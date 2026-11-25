package redact_test

import (
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
