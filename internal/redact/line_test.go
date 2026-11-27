package redact

import (
	"strings"
	"testing"
)

func TestLineRedactor(t *testing.T) {
	cases := []struct {
		name           string
		lines          []string
		mustNotContain []string
		mustContain    []string
	}{
		{
			name:           "H2 bare token nested yaml line by line",
			lines:          []string{"token:\n", "  value: hunter2hunter2\n", "status: ok\n"},
			mustNotContain: []string{"hunter2hunter2"},
			mustContain:    []string{"token:\n", "  value: [REDACTED:secret]\n", "status: ok\n"},
		},
		{
			name:           "H2 nested secret depth 2 line by line",
			lines:          []string{"secret:\n", "  prod:\n", "    value: deepStreamSecret99\n", "  staging:\n", "    value: stageStreamSecret88\n", "next: keep\n"},
			mustNotContain: []string{"deepStreamSecret99", "stageStreamSecret88"},
			mustContain:    []string{"secret:\n", "  prod:\n", "    value: [REDACTED:secret]\n", "next: keep\n"},
		},
		{
			name:           "yaml block scalar with trailing hash line",
			lines:          []string{"private_key: |\n", "  abcdefgh\n", "  #tailsecretmaterial\n", "next: keep\n"},
			mustNotContain: []string{"abcdefgh", "tailsecretmaterial"},
			mustContain:    []string{"private_key: |\n", "next: keep\n"},
		},
		{
			name: "multi-line pem private key block",
			lines: []string{
				"before\n",
				joinParts("-----BEGIN ", "RSA PRIVATE KEY-----\n"),
				"MIIEfixtureBodyLine1\n",
				"fixtureBodyLine2\n",
				joinParts("-----END ", "RSA PRIVATE KEY-----\n"),
				"after\n",
			},
			mustNotContain: []string{"MIIEfixtureBodyLine1", "fixtureBodyLine2"},
			mustContain:    []string{"before\n", "[REDACTED:private_key]", "after\n"},
		},
		{
			name: "inline pem header in grep command does not eat following lines",
			lines: []string{
				"grep '" + joinParts("-----BEGIN ", "PRIVATE KEY-----") + "' *.pem\n",
				"file1.pem\n",
			},
			mustNotContain: []string{"BEGIN PRIVATE KEY"},
			mustContain:    []string{"grep '[REDACTED:private_key]' *.pem\n", "file1.pem\n"},
		},
		{
			name: "standalone truncated pem fails closed across following lines",
			lines: []string{
				joinParts("-----BEGIN ", "PRIVATE KEY-----\n"),
				"private bytes\n",
			},
			mustNotContain: []string{"private bytes"},
			mustContain:    []string{"[REDACTED:private_key]"},
		},
		{
			name:           "folded Authorization header before scheme",
			lines:          []string{"Authorization:\r\n", " Bearer abcdefghijkl\r\n", "Host: api.invalid\r\n"},
			mustNotContain: []string{"abcdefghijkl"},
			mustContain:    []string{"Authorization:\r\n", " Bearer [REDACTED:bearer_token]\r\n", "Host: api.invalid\r\n"},
		},
		{
			name:           "folded Authorization header after scheme",
			lines:          []string{"Authorization: Bearer\r\n", " abcdefghijkl\r\n", "Host: api.invalid\r\n"},
			mustNotContain: []string{"abcdefghijkl"},
			mustContain:    []string{"Authorization: Bearer\r\n", " [REDACTED:bearer_token]\r\n", "Host: api.invalid\r\n"},
		},
		{
			name:           "same-indent yaml list under password",
			lines:          []string{"password:\n", "- hunter2hunter2\n", "- secondSecret99\n", "status: ok\n"},
			mustNotContain: []string{"hunter2hunter2", "secondSecret99"},
			mustContain:    []string{"password:\n", "- [REDACTED:secret]\n", "status: ok\n"},
		},
		{
			name:           "metadata keys under secret block kept",
			lines:          []string{"secret:\n", "  secretName: db-creds\n", "  defaultMode: 0400\n"},
			mustNotContain: []string{"[REDACTED:"},
			mustContain:    []string{"secretName: db-creds\n", "defaultMode: 0400\n"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			lr := NewLineRedactor()
			var b strings.Builder
			for _, line := range tc.lines {
				b.WriteString(lr.String(line))
			}
			got := b.String()
			for _, bad := range tc.mustNotContain {
				if strings.Contains(got, bad) {
					t.Errorf("leaked %q in output:\n%s", bad, got)
				}
			}
			for _, keep := range tc.mustContain {
				if !strings.Contains(got, keep) {
					t.Errorf("expected %q to survive in output:\n%s", keep, got)
				}
			}
			// Idempotency across a second LineRedactor pass.
			lr2 := NewLineRedactor()
			var b2 strings.Builder
			for _, line := range strings.SplitAfter(got, "\n") {
				if line == "" {
					continue
				}
				b2.WriteString(lr2.String(line))
			}
			if twice := b2.String(); twice != got {
				t.Errorf("LineRedactor not idempotent:\nonce:  %q\ntwice: %q", got, twice)
			}
		})
	}
}
