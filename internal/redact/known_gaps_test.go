package redact

import (
	"strings"
	"testing"
)

// TestKnownGaps pins today's behaviour for the named gaps of the redactor.
// Each row asserts the current, imperfect result: either the secret survives
// (leak) or a non-secret is redacted (over-redaction). When a gap is fixed,
// its row starts failing: move the row to the corpus (TestCorpusMustHide or
// the keep tests) with the corrected expectation instead of deleting it.
func TestKnownGaps(t *testing.T) {
	secret := joinParts("Zq9x", "K2mV", "7pLw")
	awsTail := joinParts("AKIA", "FIXTURE", "012345678")
	tests := []struct {
		name  string
		input string
		// leaks lists substrings that must still be present in the output.
		leaks []string
		// gone lists substrings that today are removed although they are not secrets.
		gone []string
	}{
		{name: "key synonym secret key base", input: "SECRET_KEY_BASE=" + secret, leaks: []string{secret}},
		{name: "key synonym encryption key", input: "ENCRYPTION_KEY=" + secret, leaks: []string{secret}},
		{name: "key synonym master key", input: "master_key: " + secret, leaks: []string{secret}},
		{name: "no-break space separator", input: "password:\u00a0" + secret, leaks: []string{secret}},
		{name: "value starts with paren", input: "password=(" + secret + ")", leaks: []string{secret}},
		{name: "value starts with ampersand", input: "password=&" + secret, leaks: []string{secret}},
		{name: "value starts with angle bracket", input: "password=<" + secret, leaks: []string{secret}},
		{name: "double escaped json", input: `{"m":"{\\\"password\\\":\\\"` + secret + `\\\"}"}`, leaks: []string{secret}},
		{name: "folded cookie header", input: "Cookie:\r\n sid=" + secret, leaks: []string{secret}},
		{name: "logfmt authorization bearer", input: "authorization=Bearer " + secret, leaks: []string{secret}},
		{name: "logfmt quoted cookie", input: `cookie="sid=` + secret + `"`, leaks: []string{secret}},
		{name: "scheme-less authorization", input: "Authorization: " + secret, leaks: []string{secret}},
		{name: "curl proxy-user", input: "curl --proxy-user u:" + secret + " https://example.test/", leaks: []string{secret}},
		{name: "curl cookie flag", input: "curl -b sid=" + secret + " https://example.test/", leaks: []string{secret}},
		{name: "curl long cookie flag", input: "curl --cookie sid=" + secret + " https://example.test/", leaks: []string{secret}},
		{name: "unquoted passphrase keeps later words", input: "passphrase: correct horse battery staple", leaks: []string{"horse battery staple"}},
		{name: "short unquoted value kept", input: "password=abc12", leaks: []string{"abc12"}},
		{name: "marker splits secret", input: "password=" + secret + "-" + awsTail + "xyz", leaks: []string{"xyz"}},
		{name: "github token split by crlf after twenty characters", input: joinParts("gh", "p_", "0123456789abcdefghij", "\r\n", "klmnopqrst"), leaks: []string{"klmnopqrst"}},
		{name: "github token split by crlf after prefix", input: joinParts("gh", "p_", "\r\n", "0123456789abcdefghijkl"), leaks: []string{"0123456789abcdefghijkl"}},
		{name: "unterminated quote short value", input: `password="abc12`, leaks: []string{"abc12"}},
		{name: "multiline quoted value stops at colon line", input: "password=\"abc\ndef:ghi\"", leaks: []string{"ghi"}},
		{name: "hyphen tokens suffix over-redacts", input: "max-tokens=123456", gone: []string{"123456"}},
		{name: "env reference in password value", input: "password: ${DB_PASSWORD}", gone: []string{"DB_PASSWORD"}},
		{name: "env reference in token value", input: "token=$API_TOKEN", gone: []string{"API_TOKEN"}},
		{name: "env reference in bearer header", input: "Authorization: Bearer $API_TOKEN", gone: []string{"API_TOKEN"}},
		{name: "secret name reference", input: "existingSecret: postgres-secret", gone: []string{"postgres-secret"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := String(tc.input)
			for _, s := range tc.leaks {
				if !strings.Contains(got, s) {
					t.Errorf("gap closed? %q no longer survives in %q: move this row to the corpus", s, got)
				}
			}
			for _, s := range tc.gone {
				if strings.Contains(got, s) {
					t.Errorf("gap closed? %q is no longer redacted in %q: move this row to the corpus", s, got)
				}
			}
		})
	}
}

// TestSecretKeyNameShape pins how a secret key name is matched: the whole key
// must end in a listed word (any characters may come before it, except that
// pwd and pass need a "_" or "-" before them), and a key that merely starts
// with a listed word is not matched.
func TestSecretKeyNameShape(t *testing.T) {
	secret := joinParts("Zq9x", "K2mV", "7pLw")
	tests := []struct {
		name     string
		key      string
		redacted bool
	}{
		{"exact name", "password", true},
		{"suffix after underscore", "db_password", true},
		{"suffix without separator", "dbpassword", true},
		{"suffix token", "auth_token", true},
		{"plural suffix", "db_passwords", true},
		{"pwd after separator", "db_pwd", true},
		{"pass after separator", "db-pass", true},
		{"exact pass", "pass", true},
		{"pass without separator", "bypass", false},
		{"pwd without separator", "dbpwd", false},
		{"prefixed by password", "password_v2", false},
		{"prefixed by secret key", "secret_key_base", false},
		{"prefixed by token", "token_url", false},
		{"plural tokens only exact", "auth_tokens", false},
		{"exact tokens", "tokens", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := String(tc.key + "=" + secret)
			if hidden := !strings.Contains(got, secret); hidden != tc.redacted {
				t.Fatalf("%s: redacted = %v, want %v (output %q)", tc.key, hidden, tc.redacted, got)
			}
		})
	}
}
