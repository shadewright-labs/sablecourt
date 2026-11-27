package redact

import (
	"strings"
	"testing"
)

// The corpus: rows whose answer is known by construction. A must-hide row
// holds a synthetic secret that must not survive; a must-keep row holds
// ordinary text that must come back unchanged. Rows that main's redactor gets
// wrong are not here: they live in the known-gap table (known_gaps_test.go),
// named by gap class.

func joinParts(parts ...string) string {
	return strings.Join(parts, "")
}

func pemBlock(label, body, sep string, closed bool) string {
	header := joinParts("-----BEGIN ", label, "-----")
	if !closed {
		return header + sep + body
	}
	footer := joinParts("-----END ", label, "-----")
	return header + sep + body + sep + footer
}

func TestCorpusMustHide(t *testing.T) {
	cases := []struct {
		name           string
		input          string
		mustNotContain []string
		mustContain    []string
		wantCategory   string
		wantMarker     string
	}{
		{
			name:           "H6 password value on next line",
			input:          "password:\nhunter2hunter2\n",
			mustNotContain: []string{"hunter2hunter2"},
			mustContain:    []string{"password:\n"},
		},
		{
			name:           "H6 unterminated double quote",
			input:          `password="hunter2hunter2`,
			mustNotContain: []string{"hunter2hunter2"},
			mustContain:    []string{`password="`},
		},
		{
			name:           "H6 unterminated single quote",
			input:          "password: 'hunter2hunter2",
			mustNotContain: []string{"hunter2hunter2"},
			mustContain:    []string{"password: '"},
		},
		{
			name:           "M1 adversarial unclosed quote followed by second secret",
			input:          "password=\"unclosedSecret\napi_key=\"secondSecretVal\"",
			mustNotContain: []string{"unclosedSecret", "secondSecretVal"},
			mustContain:    []string{`password="`, `api_key="`},
		},
		{
			name:           "proxy-authorization bearer",
			input:          "Proxy-Authorization: Bearer " + bearerVal,
			mustNotContain: []string{bearerVal},
			mustContain:    []string{"Proxy-Authorization: Bearer "},
			wantCategory:   "bearer_header",
			wantMarker:     "[REDACTED:bearer_token]",
		},
		{
			name:           "authorization bearer mixed case and tab",
			input:          "AUTHORIZATION:\tBearer\t" + bearerVal + "\r\n",
			mustNotContain: []string{bearerVal},
			mustContain:    []string{"AUTHORIZATION:\tBearer\t"},
		},
		{
			name:           "H6 Authorization folded before scheme",
			input:          "Authorization:\r\n Bearer abcdefghijkl",
			mustNotContain: []string{"abcdefghijkl"},
			mustContain:    []string{"Authorization:\r\n Bearer [REDACTED:bearer_token]"},
		},
		{
			name:           "H6 Authorization folded after scheme",
			input:          "Authorization: Bearer\r\n abcdefghijkl",
			mustNotContain: []string{"abcdefghijkl"},
			mustContain:    []string{"Authorization: Bearer\r\n [REDACTED:bearer_token]"},
		},
		{
			name:           "pem rsa complete",
			input:          "before\n" + pemBlock("RSA PRIVATE KEY", pemBody, "\n", true) + "\nafter",
			mustNotContain: []string{"fixtureBodyLine1", "fixtureBodyLine2"},
			mustContain:    []string{"before\n", "\nafter"},
			wantCategory:   "private_key",
			wantMarker:     "[REDACTED:private_key]",
		},
		{
			name:           "pem ec complete",
			input:          "before\n" + pemBlock("EC PRIVATE KEY", pemBody, "\n", true) + "\nafter",
			mustNotContain: []string{"fixtureBodyLine1"},
			mustContain:    []string{"before\n", "\nafter"},
		},
		{
			name:           "pem openssh complete",
			input:          "before\n" + pemBlock("OPENSSH PRIVATE KEY", pemBody, "\n", true) + "\nafter",
			mustNotContain: []string{"fixtureBodyLine1"},
			mustContain:    []string{"before\n", "\nafter"},
		},
		{
			name:           "pem pkcs8 complete with CRLF",
			input:          "before\r\n" + pemBlock("PRIVATE KEY", pemBodyCRLF, "\r\n", true) + "\r\nafter",
			mustNotContain: []string{"fixtureBodyLine1"},
			mustContain:    []string{"before\r\n", "\r\nafter"},
		},
		{
			name:           "provider ghp token",
			input:          "val=" + ghp,
			mustNotContain: []string{"ghp_", "0123456789", "abcdefghij", "klmnopqrst"},
			wantCategory:   "github_token",
			wantMarker:     "[REDACTED:github_token]",
		},
		{
			name:           "provider gho token",
			input:          "val=" + gho,
			mustNotContain: []string{"0123456789", "klmnopqrst"},
		},
		{
			name:           "provider ghu token",
			input:          "val=" + ghu,
			mustNotContain: []string{"0123456789", "klmnopqrst"},
		},
		{
			name:           "provider ghs token",
			input:          "val=" + ghs,
			mustNotContain: []string{"0123456789", "klmnopqrst"},
		},
		{
			name:           "provider ghr token",
			input:          "val=" + ghr,
			mustNotContain: []string{"0123456789", "klmnopqrst"},
		},
		{
			name:           "provider github_pat token",
			input:          "val=" + ghPat,
			mustNotContain: []string{"0123456789", "klmnop"},
		},
		{
			name:           "provider sc_task token",
			input:          "val=" + scTask,
			mustNotContain: []string{"sc_task_", "0123456789abcdef", "fedcba9876543210"},
			wantCategory:   "sablecourt_token",
			wantMarker:     "[REDACTED:sablecourt_token]",
		},
		{
			name:           "provider op token",
			input:          "val=" + opTok,
			mustNotContain: []string{"0123456789abcdef"},
			wantCategory:   "sablecourt_token",
		},
		{
			name:           "provider anthropic key",
			input:          "val=" + antKey,
			mustNotContain: []string{"sk-ant-", "fixture0123456789", "abcdef"},
			wantCategory:   "anthropic_key",
			wantMarker:     "[REDACTED:anthropic_key]",
		},
		{
			name:           "provider openai project key",
			input:          "val=" + oaiKey,
			mustNotContain: []string{"fixture0123456789", "abcdef"},
			wantCategory:   "openai_key",
			wantMarker:     "[REDACTED:openai_key]",
		},
		{
			name:           "provider openai legacy key",
			input:          "val=" + oaiLegacy,
			mustNotContain: []string{"fixture0123456789", "abcdef"},
			wantCategory:   "openai_key",
		},
		{
			name:           "provider google api key",
			input:          "val=" + googKey,
			mustNotContain: []string{"SyFixture0123456789", "abcd"},
			wantCategory:   "google_api_key",
		},
		{
			name:           "provider aws AKIA key",
			input:          "val=" + awsAKIA,
			mustNotContain: []string{"AKIA", "FIXTURE012345678"},
			wantCategory:   "aws_access_key",
			wantMarker:     "[REDACTED:aws_access_key]",
		},
		{
			name:           "provider aws ASIA key",
			input:          "val=" + awsASIA,
			mustNotContain: []string{"ASIA", "FIXTURE012345678"},
			wantCategory:   "aws_access_key",
		},
		{
			name: "two complete pem blocks preserve text between",
			input: pemBlock("RSA PRIVATE KEY", pemBody, "\n", true) +
				"\nkeep-middle\n" +
				pemBlock("EC PRIVATE KEY", "ecFixtureBody", "\n", true) +
				"\nkeep-end",
			mustNotContain: []string{"fixtureBodyLine1", "ecFixtureBody"},
			mustContain:    []string{"\nkeep-middle\n", "\nkeep-end"},
		},
		{
			name:           "N4 unquoted value containing open paren",
			input:          "DB_PASSWORD=Xy9(kLm2nPq",
			mustNotContain: []string{"Xy9", "kLm2nPq"},
			mustContain:    []string{"DB_PASSWORD=[REDACTED:secret]"},
		},
		{
			name:           "N4 unquoted value containing close paren mid-token",
			input:          "password=Zq9xK2)mV7pLw",
			mustNotContain: []string{"Zq9xK2", "mV7pLw"},
			mustContain:    []string{"password=[REDACTED:secret]"},
		},
		{
			name:           "N4 unquoted value containing close brace mid-token",
			input:          "password=Zq9xK2}mV7pLw",
			mustNotContain: []string{"Zq9xK2", "mV7pLw"},
			mustContain:    []string{"password=[REDACTED:secret]"},
		},
		{
			name:           "N4 unquoted value containing close bracket mid-token",
			input:          "password=Zq9xK2]mV7pLw",
			mustNotContain: []string{"Zq9xK2", "mV7pLw"},
			mustContain:    []string{"password=[REDACTED:secret]"},
		},
		// Rows that the first rewrite of the redactor listed as gaps and that
		// main's redactor hides.
		{
			name:           "no-break space separator",
			input:          "password:\u00a0" + gapSecret,
			mustNotContain: []string{gapSecret},
		},
		{
			name:           "value starts with ampersand",
			input:          "password=&" + gapSecret,
			mustNotContain: []string{gapSecret},
		},
		{
			name:           "value split by ampersand",
			input:          "password=abc&" + gapSecret,
			mustNotContain: []string{gapSecret},
		},
		{
			name:           "token marker inside a password value",
			input:          "password=" + gapSecret + "-" + awsTail + "xyz",
			mustNotContain: []string{gapSecret, "xyz"},
		},
		{
			name:           "line break between key and separator",
			input:          "password\n= " + gapSecret,
			mustNotContain: []string{gapSecret},
		},
		{
			name:           "value on the next line after equals",
			input:          "password=\n" + gapSecret,
			mustNotContain: []string{gapSecret},
		},
		{
			name:           "value on the next line after a colon mid-line",
			input:          "x secret :\n" + gapSecret,
			mustNotContain: []string{gapSecret},
		},
		{
			name:           "json value on the next line",
			input:          "{\"password\":\n\"" + gapSecret + "\"}",
			mustNotContain: []string{gapSecret},
		},
		{
			name:           "lowercase word with a status suffix after colon",
			input:          "password: dragonslaying",
			mustNotContain: []string{"dragonslaying"},
		},
		{
			name:           "value shaped like a call",
			input:          "f(password=Xy9(" + gapSecret + ")",
			mustNotContain: []string{gapSecret},
		},
		{
			name:           "value shaped like a path",
			input:          "password=/x9/" + gapSecret,
			mustNotContain: []string{gapSecret},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := Text(tc.input)
			got := res.Text
			if res.Hits == 0 {
				t.Errorf("expected Hits > 0 for %q, got 0", tc.input)
			}
			for _, bad := range tc.mustNotContain {
				if strings.Contains(got, bad) {
					t.Errorf("leaked %q in redacted output %q", bad, got)
				}
			}
			for _, keep := range tc.mustContain {
				if !strings.Contains(got, keep) {
					t.Errorf("expected %q to survive in redacted output %q", keep, got)
				}
			}
			if tc.wantMarker != "" && !strings.Contains(got, tc.wantMarker) {
				t.Errorf("expected marker %q in redacted output %q", tc.wantMarker, got)
			}
			if tc.wantCategory != "" {
				found := false
				for _, c := range res.Categories {
					if c == tc.wantCategory {
						found = true
						break
					}
				}
				if !found {
					t.Errorf("expected category %q in %v", tc.wantCategory, res.Categories)
				}
			}
			if twice := String(got); twice != got {
				t.Errorf("String not idempotent:\nonce:  %q\ntwice: %q", got, twice)
			}
		})
	}
}

func TestCorpusMustKeep(t *testing.T) {
	cases := []struct {
		name  string
		input string
	}{
		{
			name:  "url without userinfo https",
			input: "fetch https://docs.invalid/guide/auth?tab=1#top",
		},
		{
			name:  "url without userinfo postgres",
			input: "connect postgres://db.internal:5432/app?sslmode=require",
		},
		{
			name:  "url with at sign in path not userinfo",
			input: "GET https://registry.invalid:8443/@scope/pkg-1.2.3",
		},
		{
			name:  "M7 ssh url with plain username git",
			input: "ssh://git@example.com/org/repo.git",
		},
		{
			name:  "M7 url followed by comma and email",
			input: "see https://a.invalid,foo@example.com",
		},
		{
			name:  "word secret in prose",
			input: "This secret stays in the vault and never enters logs.",
		},
		{
			name:  "word password and token in prose",
			input: "Rotate the password and refresh the token before Friday.",
		},
		{
			name:  "H3 new key synonyms in prose",
			input: "All tests pass; run pwd, check credentials, access_key, private_key, secret_key and signing_key docs.",
		},
		{
			name:  "H3 non-secret suffix near pass",
			input: "bypass: enabled compass: magnetic pass_count=123456",
		},
		{
			name:  "H3 xml non-secret tag mentioning password",
			input: "<description>rotate password soon</description><count>123456</count>",
		},
		{
			name:  "H3 cli flag followed by another flag",
			input: "mycli --password --help",
		},
		{
			name:  "H3 docker -u uid:gid not curl",
			input: "docker run -u 1000:1000 alpine",
		},
		{
			name:  "M7 H4 Authorization Basic prose",
			input: "Authorization: Basic auth is not supported",
		},
		{
			name:  "H4 Authorization prose word after colon",
			input: "Authorization: required for admin endpoints",
		},
		{
			name:  "H4 Cookie prose without key=value",
			input: "Cookie: disabled in private browsing",
		},
		{
			name:  "M7 invalid token error message",
			input: "invalid token: signature expired",
		},
		{
			name:  "M7 code calling os.Getenv",
			input: `token = os.Getenv("TOKEN")`,
		},
		{
			name:  "M7 github actions token input metadata",
			input: "inputs:\n  token:\n    description: 'GitHub token'\n    required: true\n",
		},
		{
			name:  "M7 openapi password schema",
			input: "password:\n  type: string\n  minLength: 8\n",
		},
		{
			name:  "M7 pre-redacted plain REDACTED marker",
			input: "Token: [REDACTED]",
		},
		{
			name:  "yaml value under non-secret parent",
			input: "timeout:\n  value: 30s\n",
		},
		{
			name:  "yaml value two levels under non-secret parent",
			input: "config:\n  retries:\n    value: 5\n  mode: strict\n",
		},
		{
			name:  "base64 image data uri",
			input: "img=data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNk+M9QDwADhgGAWjR9awAAAABJRU5ErkJggg==",
		},
		{
			name:  "git commit sha",
			input: "commit a1b2c3d4e5f60718293a4b5c6d7e8f9012345678 merged",
		},
		{
			name:  "uuid v4",
			input: "request_id=550e8400-e29b-41d4-a716-446655440000",
		},
		{
			name:  "dotted identifiers not jwt",
			input: "type pkg.Service.HandleRequest version v1.2.3",
		},
		{
			name:  "eyJ hostname not jwt",
			input: "connect eyJ1234567.service.internal:443",
		},
		{
			name:  "word ending in sk- not openai key",
			input: "id=task-0123456789abcdefghij",
		},
		{
			name:  "pem public key untouched",
			input: pemBlock("PUBLIC KEY", joinParts("MFkwEwYHKoZIzj0C", "AQYIKoZIzj0DAQcD", "QgAEfixture"), "\n", true),
		},
		{
			name:  "pem certificate untouched",
			input: pemBlock("CERTIFICATE", "MIICfixtureCertBody", "\n", true),
		},
		{
			name:  "count fields untouched",
			input: `{"api_key_count":123456,"max_tokens":4096,"secret_version":2}`,
		},
		{
			name:  "clean text with invalid utf8 byte",
			input: "status=ok\xffcount=42",
		},
		{
			name:  "pre-redacted url userinfo marker stays intact",
			input: "https://[REDACTED:userinfo]@example.com/x",
		},
		{
			name:  "pre-redacted url_credential marker stays intact",
			input: "https://[REDACTED:url_credential]@example.com/x",
		},
		{
			name:  "N6 prose credentials loaded from file",
			input: "credentials: loaded from file",
		},
		{
			name:  "N6 prose token refreshed",
			input: "token: refreshed",
		},
		{
			name:  "N6 prose pass passed",
			input: "pass: passed",
		},
		{
			name:  "N6 env PWD filesystem path",
			input: "PWD=/opt/runner/work/repo",
		},
		{
			name:  "N6 ipv6 url with email in path",
			input: "https://[2001:db8::1]/u/bob@example.com",
		},
		{
			name:  "N6 k8s imagePullSecrets list item name",
			input: "imagePullSecrets:\n- name: regcred\n",
		},
		{
			name:  "N6 credentials flow mapping metadata",
			input: "credentials: {region: eu, profile: x}",
		},
		{
			name:  "N6 Authorization skipping verification prose",
			input: "Authorization: skipping verification",
		},
		{
			name:  "max-tokens setting",
			input: "max-tokens=123456",
		},
		{
			name:  "env reference as token value",
			input: "token=$API_TOKEN",
		},
		{
			name:  "env reference in bearer header",
			input: "Authorization: Bearer $API_TOKEN",
		},
		{
			name:  "go short assignment from a field",
			input: "+\tpassword := cfg.Password",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := Text(tc.input)
			if res.Text != tc.input || res.Hits != 0 {
				t.Errorf("Text(%q) altered clean input: got %q (hits=%d cats=%v)", tc.input, res.Text, res.Hits, res.Categories)
			}
		})
	}
}
