package redact

import (
	"strings"
	"testing"
)

func TestTextRedactsKnownSecrets(t *testing.T) {
	input := strings.Join([]string{
		"Authorization: Bearer eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9",
		"task=sc_task_0123456789abcdef0123456789abcdef",
		"openai=sk-proj-abcdefghijklmnopqrstuvwxyz123456",
		"anthropic=sk-ant-api03-abcdefghijklmnopqrstuvwxyz",
		"github=ghp_1234567890abcdefghijklmnopqrstuvwx",
		"gemini=AIzaSyA1234567890abcdefghijklmnop",
		"aws=AKIAIOSFODNN7EXAMPLE",
		"db_password=supersecretvalue123",
		"-----BEGIN RSA PRIVATE KEY-----\nMIIEpAIBAAKCAQEA\n-----END RSA PRIVATE KEY-----",
		"safe line: status=ok count=42",
	}, "\n")

	res := Text(input)
	if res.Hits < 9 {
		t.Fatalf("expected at least 9 redaction hits, got %d (cats=%v)", res.Hits, res.Categories)
	}

	for _, leaked := range []string{
		"eyJhbGci",
		"sc_task_0123",
		"sk-proj-abcd",
		"sk-ant-api03",
		"ghp_1234",
		"AIzaSyA1234",
		"AKIAIOSFODNN7EXAMPLE",
		"supersecretvalue123",
		"MIIEpAIBAAKCAQEA",
	} {
		if strings.Contains(res.Text, leaked) {
			t.Fatalf("leaked secret substring %q in output:\n%s", leaked, res.Text)
		}
	}

	if !strings.Contains(res.Text, "safe line: status=ok count=42") {
		t.Fatalf("safe line was unexpectedly altered:\n%s", res.Text)
	}
}

func TestTextCleanInputHasZeroHits(t *testing.T) {
	res := Text("issue #42: fix timeout in sqlite store")
	if res.Hits != 0 || res.Text != "issue #42: fix timeout in sqlite store" {
		t.Fatalf("unexpected redaction on clean text: %+v", res)
	}
}

func TestQuotedSecretFields(t *testing.T) {
	for _, tc := range []struct{ input, want string }{
		{`{"api_key":"fixture with spaces"}`, `{"api_key":"[REDACTED:secret]"}`},
		{`{"password":"a\"b"}`, `{"password":"[REDACTED:secret]"}`},
		{`{"api_key":"x","status":"ready"}`, `{"api_key":"[REDACTED:secret]","status":"ready"}`},
		{`{"api_key_count":123456}`, `{"api_key_count":123456}`},
	} {
		if got := String(tc.input); got != tc.want {
			t.Errorf("String(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}
}
