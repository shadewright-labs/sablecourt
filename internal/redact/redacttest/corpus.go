// Package redacttest generates redaction test inputs whose secrets are known
// by construction, so a test can assert that no part of a secret survives.
package redacttest

import (
	"strings"
)

// Case is one generated input and the secret bodies it carries.
type Case struct {
	Name    string
	Text    string
	Secrets []string
}

// Window is the length of the secret fragment a test treats as a leak.
const Window = 10

// Leak returns the first fragment of a secret that is still present in out.
func Leak(out string, secrets []string) (string, bool) {
	for _, secret := range secrets {
		if len(secret) <= Window {
			if strings.Contains(out, secret) {
				return secret, true
			}
			continue
		}
		for i := 0; i+Window <= len(secret); i++ {
			if strings.Contains(out, secret[i:i+Window]) {
				return secret[i : i+Window], true
			}
		}
	}
	return "", false
}

const (
	alnum = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"
	upper = "ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	hex   = "0123456789abcdef"
)

// body returns a deterministic string of n characters from alphabet. The
// seed spreads the bodies so no two forms share a fragment.
func body(seed, n int, alphabet string) string {
	var b strings.Builder
	x := uint32(seed*2654435761 + 1)
	for range n {
		x ^= x << 13
		x ^= x >> 17
		x ^= x << 5
		b.WriteByte(alphabet[int(x%uint32(len(alphabet)))])
	}
	return b.String()
}

type form struct {
	name  string
	token string // the whole credential as it appears on a line
	body  string // the part that must never be visible
	field string // a JSON or env field name that does not itself mark a secret
}

func forms() []form {
	anth := body(1, 40, alnum)
	oai := body(2, 40, alnum)
	ghp := body(3, 36, alnum)
	pat := body(4, 40, alnum)
	goog := body(5, 35, alnum)
	aws := body(6, 16, upper)
	task := body(7, 32, hex)
	op := body(8, 64, hex)
	return []form{
		{"anthropic", "sk-ant-api03-" + anth, anth, "note"},
		{"openai", "sk-proj-" + oai, oai, "note"},
		{"github", "ghp_" + ghp, ghp, "note"},
		{"github_pat", "github_pat_" + pat, pat, "note"},
		{"google", "AIza" + goog, goog, "note"},
		{"aws", "AKIA" + aws, aws, "note"},
		{"task_token", "sc_task_" + task, task, "note"},
		{"operator_token", "op_" + op, op, "note"},
	}
}

// Corpus returns every supported secret form in plain text, JSON and
// env-style lines.
func Corpus() []Case {
	var cases []Case
	for _, f := range forms() {
		cases = append(cases,
			Case{f.name + "/plain", "before " + f.token + " after\n", []string{f.body}},
			Case{f.name + "/json", `{"` + f.field + `":"` + f.token + `","n":1}` + "\n", []string{f.body}},
			Case{f.name + "/env", "SOME_VALUE=" + f.token + "\n", []string{f.body}},
		)
	}

	pw := body(9, 24, alnum)
	spaced := body(10, 12, alnum) + " " + body(11, 12, alnum)
	cases = append(cases,
		Case{"password/plain", "the password: " + pw + " is set\n", []string{pw}},
		Case{"password/json", `{"password":"` + pw + `"}` + "\n", []string{pw}},
		Case{"password/json_spaced", `{"user":"x","password": "` + spaced + `"}` + "\n", []string{spaced}},
		Case{"password/json_short", `{"password":"hunter2hunter2"}` + "\n", []string{"hunter2hunter2"}},
		Case{"password/env", "DB_PASSWORD=" + pw + "\n", []string{pw}},
		Case{"api_key/json", `{"api_key": "` + pw + `"}` + "\n", []string{pw}},
		Case{"api_key/env", "export OPENAI_API_KEY=" + pw + "\n", []string{pw}},
	)

	bearer := body(12, 40, alnum)
	cases = append(cases,
		Case{"bearer/plain", "Authorization: Bearer " + bearer + "\n", []string{bearer}},
		Case{"bearer/json", `{"Authorization":"Bearer ` + bearer + `"}` + "\n", []string{bearer}},
		Case{"bearer/header_line", "curl -H 'authorization: bearer " + bearer + "' x\n", []string{bearer}},
	)

	pem := body(13, 64, alnum)
	block := "-----BEGIN RSA PRIVATE KEY-----\n" + pem + "\n-----END RSA PRIVATE KEY-----"
	escaped := strings.ReplaceAll(block, "\n", `\n`)
	cases = append(cases,
		Case{"private_key/plain", "key follows\n" + block + "\ndone\n", []string{pem}},
		Case{"private_key/json", `{"key":"` + escaped + `"}` + "\n", []string{pem}},
		Case{"private_key/env", `SSH_KEY="` + escaped + `"` + "\n", []string{pem}},
	)
	return cases
}

// UnterminatedKey returns text whose private key block has no END marker, as
// left by a cut, and the key body.
func UnterminatedKey(prefix int) (string, string) {
	pem := body(14, 1600, alnum)
	return strings.Repeat("p", prefix) + "\n-----BEGIN OPENSSH PRIVATE KEY-----\n" + pem, pem
}
