package redact

import (
	"strings"
	"testing"
)

// Additional redaction coverage for prefixed token variants, key-value field
// syntaxes and non-secret forms. Inputs are built by concatenation so that no
// key-shaped literal sits in the file.

type claimRow struct {
	name  string
	input string
	// hidden lists substrings that must not survive.
	hidden []string
	// kept lists substrings that must survive.
	kept []string
}

func runClaimRows(t *testing.T, rows []claimRow) {
	t.Helper()
	for _, tc := range rows {
		t.Run(tc.name, func(t *testing.T) {
			got := String(tc.input)
			for _, s := range tc.hidden {
				if strings.Contains(got, s) {
					t.Errorf("%q survives in %q", s, got)
				}
			}
			for _, s := range tc.kept {
				if !strings.Contains(got, s) {
					t.Errorf("%q was removed from %q", s, got)
				}
			}
		})
	}
}

// TestDocPrefixedTokens checks each supported token prefix with a valid body.
func TestDocPrefixedTokens(t *testing.T) {
	tail := "0123456789abcdefghijklmnop"
	prefixes := []string{
		joinParts("sc_", "task_"), joinParts("op", "_"), joinParts("sk-", "ant-"), joinParts("sk-", "proj-"), joinParts("sk", "-"),
		joinParts("sk_", "live_"), joinParts("rk_", "live_"),
		joinParts("xox", "b-"), joinParts("xox", "a-"), joinParts("xox", "p-"), joinParts("xox", "r-"), joinParts("xox", "s-"),
		joinParts("gh", "p_"), joinParts("gh", "o_"), joinParts("gh", "u_"), joinParts("gh", "s_"), joinParts("gh", "r_"),
		joinParts("github_", "pat_"), joinParts("AI", "za"), joinParts("AK", "IA"), joinParts("AS", "IA"),
	}
	rows := make([]claimRow, 0, len(prefixes))
	for _, p := range prefixes {
		body := tail
		switch p {
		case joinParts("sc_", "task_"):
			body = "0123456789abcdeffedcba9876543210"
		case joinParts("op", "_"):
			body = strings.Repeat("0123456789abcdef", 4)
		case joinParts("AK", "IA"), joinParts("AS", "IA"):
			body = "FIXTURE012345678"
		}
		rows = append(rows, claimRow{name: p, input: "x " + p + body + " y", hidden: []string{body}, kept: []string{"x ", " y"}})
	}
	runClaimRows(t, rows)
}

// TestDocKeyValueForms checks secret key names and assignment syntaxes.
func TestDocKeyValueForms(t *testing.T) {
	secret := joinParts("Zq9x", "K2mV", "7pLw")
	var rows []claimRow
	for _, key := range []string{"api_key", "secret", "secret_key", "secret_access_key", "private_key", "access_key", "signing_key", "password", "passwd", "passphrase", "pwd", "pass", "credentials", "token", "tokens", "db_password", "auth_token", "DB_PASSWORD", "Api_Key"} {
		rows = append(rows, claimRow{name: "equals " + key, input: key + "=" + secret, hidden: []string{secret}})
	}
	rows = append(rows,
		claimRow{name: "colon", input: "password: " + secret, hidden: []string{secret}},
		claimRow{name: "colon equals", input: "password := " + secret, hidden: []string{secret}},
		claimRow{name: "hash rocket", input: `"password" => "` + secret + `"`, hidden: []string{secret}},
		claimRow{name: "json string", input: `{"password": "` + secret + `"}`, hidden: []string{secret}},
		claimRow{name: "json number", input: `{"password": 1234567}`, hidden: []string{"1234567"}},
		claimRow{name: "single quoted", input: "password='" + secret + "'", hidden: []string{secret}},
		claimRow{name: "double quoted", input: `password="` + secret + `"`, hidden: []string{secret}},
		claimRow{name: "xml element", input: "<password>" + secret + "</password>", hidden: []string{secret}},
		claimRow{name: "cli flag", input: "tool --password " + secret, hidden: []string{secret}},
		claimRow{name: "curl user", input: joinParts("curl ", "-u ", "alice:", secret, " https://host.invalid/"), hidden: []string{secret}},
		claimRow{name: "yaml nested", input: "token:\n  value: " + secret + "\nstatus: ok", hidden: []string{secret}, kept: []string{"status: ok"}},
	)
	runClaimRows(t, rows)
}

// TestDocKeyValueKept checks non-secret fields and status text stay intact.
func TestDocKeyValueKept(t *testing.T) {
	runClaimRows(t, []claimRow{
		{name: "name field", input: "name: my-service-name", kept: []string{"my-service-name"}},
		{name: "region field", input: "region: europe-west4", kept: []string{"europe-west4"}},
		{name: "credentials prose", input: "credentials: loaded from file", kept: []string{"loaded from file"}},
		{name: "password prose", input: "password: changed", kept: []string{"changed"}},
		{name: "getenv call", input: "password = os.Getenv(\"DB_PASS\")", kept: []string{"os.Getenv"}},
		{name: "password prefix key", input: "password_v2=" + joinParts("Zq9x", "K2mV", "7pLw"), kept: []string{joinParts("Zq9x", "K2mV", "7pLw")}},
	})
}
