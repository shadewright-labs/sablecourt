package redact

import (
	"fmt"
	"strings"
	"testing"
)

// The grammar rows are generated from small grammars. Each row's answer is
// known by construction: it is either hidden, a leak of a named gap class, or
// (for the keep grammars) kept or over-redacted. The class is decided from the
// parts the row was built from, never from the redactor's output.

type grammarRow struct {
	name string
	// input is the generated text.
	input string
	// needles are the synthetic secret strings.
	needles []string
	// keep lists strings that must survive in a hidden row.
	keep []string
	// class is empty when main hides the row, otherwise the gap class that
	// explains why it leaks (or, for a keep row, "over-redaction").
	class string
}

// mainKeys are the key names main's redactor knows: it matches a key that ends
// in one of these words.
var mainKeys = map[string]bool{
	"password": true, "passwd": true, "secret": true, "api_key": true, "api-key": true,
}

func grammarHideRows() []grammarRow {
	var rows []grammarRow

	// Key and value grammar.
	keys := []string{
		"password", "passwd", "passphrase", "pwd", "pass",
		"secret", "secret_key", "SECRET_KEY", "secretKey",
		"api_key", "api-key", "private_key", "access_key",
		"signing_key", "credentials", "token", "auth_token",
	}
	keyQuotes := []string{"", `"`, `'`}
	seps := []string{"=", ": ", " := ", " => ", ": !!str "}
	valShapes := []struct {
		quote string
		val   string
		need  []string
		// plain marks a value main redacts whole when it is unquoted;
		// jsonQuoted marks the one quoted form its JSON rule covers.
		plain bool
	}{
		{"", "Zx9kLm2nPq", []string{"Zx9kLm2nPq"}, true},
		{"", "Xy9(kLm2nPq", []string{"Xy9", "kLm2nPq"}, true},
		{"", "Zq9xK2)mV7pLw", []string{"Zq9xK2", "mV7pLw"}, true},
		{"", "Zq9xK2}mV7pLw", []string{"Zq9xK2", "mV7pLw"}, true},
		{"", "Zq9xK2]mV7pLw", []string{"Zq9xK2", "mV7pLw"}, true},
		{"", "ab,cdefgh99", []string{"cdefgh99"}, false},
		{"", "ab;cdefgh99", []string{"cdefgh99"}, false},
		{`"`, "secret with spaces 99", []string{"with spaces 99"}, false},
		{`'`, `o\'brienSecret99`, []string{"brienSecret99"}, false},
	}
	for _, k := range keys {
		for _, kq := range keyQuotes {
			for _, sep := range seps {
				for _, vs := range valShapes {
					input := kq + k + kq + sep + vs.quote + vs.val + vs.quote
					row := grammarRow{
						name:    fmt.Sprintf("kv %s", input),
						input:   input,
						needles: vs.need,
						keep:    []string{k},
					}
					switch {
					case !mainKeys[k] && (k == "token" || k == "auth_token"):
						row.class = "token key names"
					case !mainKeys[k]:
						row.class = "other: key names"
					case kq == `"` && sep == ": " && vs.quote == `"`:
						// The JSON rule: "key": "value with spaces".
					case kq != "" || (sep != "=" && sep != ": ") || !vs.plain:
						row.class = "other: value shapes"
					}
					rows = append(rows, row)
				}
			}
		}
	}

	// Authorization header grammar.
	headers := []string{"Authorization", "Proxy-Authorization", "authorization"}
	schemes := []struct{ name, val, need string }{
		{"Bearer", "tokFixture01234567", "tokFixture01234567"},
		{"Basic", joinParts("dXNlcjp", "wYXNzd29yZA=="), "wYXNzd29yZA=="},
		{"Token", joinParts("tokFixture", "01234567"), joinParts("tokFixture", "01234567")},
		{"ApiKey", joinParts("keyFixture", "01234567"), joinParts("keyFixture", "01234567")},
		{"Digest", `username="u", response="0123456789abcdef"`, "0123456789abcdef"},
	}
	wrappers := []struct {
		wrap func(h, s, v string) string
		keep []string
		// direct marks the two layouts where the header is followed
		// directly by its colon.
		direct bool
	}{
		{func(h, s, v string) string { return h + ": " + s + " " + v }, nil, true},
		{func(h, s, v string) string { return `{"` + h + `":"` + s + " " + v + `"}` }, []string{`"}`}, false},
		{func(h, s, v string) string { return `{"` + h + `":["` + s + " " + v + `"]}` }, []string{`"]}`}, false},
		{func(h, s, v string) string { return "map[" + h + ":[" + s + " " + v + "]]" }, []string{"]]"}, false},
		{func(h, s, v string) string { return "curl -H '" + h + ": " + s + " " + v + "' https://x.invalid" }, []string{"' https://x.invalid"}, true},
	}
	for _, h := range headers {
		for _, sc := range schemes {
			for _, w := range wrappers {
				input := w.wrap(h, sc.name, sc.val)
				if sc.name == "Digest" && (strings.Contains(input, `":"`) || strings.Contains(input, `":["`)) {
					// Raw quotes inside a JSON string are invalid JSON.
					continue
				}
				row := grammarRow{name: "auth " + input, input: input, needles: []string{sc.need}, keep: w.keep}
				switch {
				case sc.name == "Basic":
					row.class = "Basic auth"
				case sc.name != "Bearer" || !w.direct:
					row.class = "other: Authorization forms"
				}
				rows = append(rows, row)
			}
		}
	}

	// Cookie headers: main has no cookie rule.
	cookieHeaders := []string{"Cookie", "Set-Cookie", "cookie"}
	cookieLayouts := []struct {
		build func(h string) string
		need  []string
	}{
		{func(h string) string { return h + ": sid=secCookie11; Path=/; HttpOnly" }, []string{"secCookie11"}},
		{func(h string) string { return "curl -H '" + h + ": sid=secCookie11' https://x.invalid" }, []string{"secCookie11"}},
		{func(h string) string { return `curl -H "` + h + `: sid=secCookie11" https://x.invalid` }, []string{"secCookie11"}},
		{func(h string) string { return "map[" + h + ":[sid=secCookie11; Path=/ refresh=secCookie22; Secure]]" }, []string{"secCookie11", "secCookie22"}},
		{func(h string) string { return `{"` + h + `":["sid=secCookie11; Path=/","refresh=secCookie22"]}` }, []string{"secCookie11", "secCookie22"}},
		{func(h string) string {
			return "{\n  \"" + h + "\": [\n    \"sid=secCookie11; Path=/\",\n    \"refresh=secCookie22\"\n  ]\n}"
		}, []string{"secCookie11", "secCookie22"}},
	}
	for _, h := range cookieHeaders {
		for _, l := range cookieLayouts {
			in := l.build(h)
			rows = append(rows, grammarRow{name: "cookie " + in, input: in, needles: l.need, class: "cookies"})
		}
	}

	// URL userinfo: main has no URL rule.
	for _, sch := range []string{"postgres", "redis", "https", "amqp"} {
		for _, sp := range []string{",", ";", ")", "]", "}", "@", "/", "?", "#", "'"} {
			for _, host := range []string{"db.invalid:5432", "[2001:db8::1]:5432"} {
				in := sch + "://app:pwA9" + sp + "pwB8@" + host + "/db"
				rows = append(rows, grammarRow{name: "url " + in, input: in, needles: []string{"pwA9", "pwB8"}, class: "URL userinfo"})
			}
		}
	}

	// Nested YAML inside an escaped JSON string.
	for _, nl := range []string{`\n`, `\r\n`, `\u000a`, `\u000A`} {
		for _, p := range []string{"secret", "password", "token", "api_key"} {
			in := `{"text":"` + p + `:` + nl + `  value: escYamlSec99` + nl + `status: ok` + nl + `"}`
			rows = append(rows, grammarRow{name: "escaped yaml " + in, input: in, needles: []string{"escYamlSec99"}, class: "nested YAML"})
		}
	}

	// Truncated PEM blocks: main needs the END line.
	body := "MIIEvQIBADANBgkqhkiG9w0BAQEFAASCBKcw"
	layouts := []func(hdr string) string{
		func(hdr string) string { return hdr + "\n" + body },
		func(hdr string) string { return hdr + " " + body },
		func(hdr string) string { return hdr + body },
		func(hdr string) string { return `{"k":"` + hdr + ` ` + body + `"}` },
		func(hdr string) string { return `{"k":"` + hdr + `\n` + body + `"}` },
		func(hdr string) string { return `{"k":"` + hdr + `\u000a` + body + `"}` },
		func(hdr string) string { return `{"m":"{\"k\":\"` + hdr + `\\n` + body + `\"}"}` },
	}
	for _, lbl := range []string{"PRIVATE KEY", "RSA PRIVATE KEY", "EC PRIVATE KEY", "OPENSSH PRIVATE KEY"} {
		hdr := joinParts("-----BEGIN ", lbl, "-----")
		for _, l := range layouts {
			in := l(hdr)
			rows = append(rows, grammarRow{name: "pem " + in, input: in, needles: []string{body}, class: "truncated PEM"})
		}
	}
	return rows
}

func grammarKeepRows() []grammarRow {
	var rows []grammarRow

	// Status prose after a colon.
	for _, k := range []string{"credentials", "password", "token", "pass", "secret", "api_key"} {
		for _, ph := range []string{
			"loaded from file", "changed", "refreshed", "passed", "rotated",
			"verified", "missing", "required", "disabled", "expired",
			"configured via env", "updating cache", "authentication failed",
		} {
			row := grammarRow{name: "prose " + k + ": " + ph, input: k + ": " + ph}
			if mainKeys[k] {
				row.class = "over-redaction"
			}
			rows = append(rows, row)
		}
	}

	// Prose after an Authorization header.
	for _, h := range []string{"Authorization", "Proxy-Authorization"} {
		for _, ph := range []string{
			"skipping verification",
			"checking credentials",
			"required for admin endpoints",
			"Bearer authentication failed",
			"Bearer missing token",
			"Basic auth is not supported",
		} {
			row := grammarRow{name: "auth prose " + h + ": " + ph, input: h + ": " + ph}
			if ph == "Bearer authentication failed" {
				// The word after Bearer is eight characters or more.
				row.class = "over-redaction"
			}
			rows = append(rows, row)
		}
	}

	// Configuration metadata under a secret-looking parent.
	pairs := [][2]string{
		{"name", "regcred"}, {"region", "eu"}, {"profile", "default"},
		{"secretName", "db-creds"}, {"defaultMode", "0400"}, {"type", "string"},
		{"minLength", "8"}, {"required", "true"},
	}
	for _, p := range []string{"secret", "credentials", "imagePullSecrets", "token"} {
		for _, kv := range pairs {
			block := grammarRow{name: "meta block " + p + "/" + kv[0], input: p + ":\n  " + kv[0] + ": " + kv[1] + "\n"}
			list := grammarRow{name: "meta list " + p + "/" + kv[0], input: p + ":\n- " + kv[0] + ": " + kv[1] + "\n"}
			flow := grammarRow{name: "meta flow " + p + "/" + kv[0], input: p + ": {" + kv[0] + ": " + kv[1] + "}"}
			if mainKeys[p] {
				// The first word after the key is read as the value when it
				// is six characters or more: "key:" in a block, "{key:" in a
				// flow mapping.
				if len(kv[0])+1 >= 6 {
					block.class = "over-redaction"
				}
				if len(kv[0])+2 >= 6 {
					flow.class = "over-redaction"
				}
			}
			rows = append(rows, block, list, flow)
		}
	}

	// Code calls and filesystem paths.
	for _, k := range []string{"token", "password", "api_key", "secret"} {
		for _, c := range []string{`os.Getenv("TOKEN")`, "getPassword()", "cfg.Get(key)"} {
			row := grammarRow{name: "call " + k + " = " + c, input: k + " = " + c}
			if mainKeys[k] {
				row.class = "over-redaction"
			}
			rows = append(rows, row)
		}
	}
	for _, k := range []string{"PWD", "private_key", "credentials"} {
		for _, p := range []string{"/opt/runner/work/repo", "/etc/ssl/private/app.key", "/var/run/secrets/token"} {
			rows = append(rows,
				grammarRow{name: "path " + k + "=" + p, input: k + "=" + p},
				grammarRow{name: "path " + k + ": " + p, input: k + ": " + p})
		}
	}

	// IPv6 URLs without userinfo.
	for _, sch := range []string{"https", "http"} {
		for _, host := range []string{"[2001:db8::1]", "[2001:db8::1]:8443"} {
			for _, path := range []string{"/u/bob@example.com", "/@scope/pkg"} {
				in := sch + "://" + host + path
				rows = append(rows, grammarRow{name: "ipv6 " + in, input: in})
			}
		}
	}
	return rows
}

func TestCorpusGrammarMustHide(t *testing.T) {
	hidden := 0
	for _, row := range grammarHideRows() {
		if row.class != "" {
			continue
		}
		hidden++
		res := Text(row.input)
		if res.Hits == 0 {
			t.Errorf("expected Hits > 0 for %q", row.input)
		}
		for _, n := range row.needles {
			if strings.Contains(res.Text, n) {
				t.Errorf("leaked %q in output %q (input %q)", n, res.Text, row.input)
			}
		}
		for _, k := range row.keep {
			if !strings.Contains(res.Text, k) {
				t.Errorf("expected %q to survive in output %q (input %q)", k, res.Text, row.input)
			}
		}
		if twice := String(res.Text); twice != res.Text {
			t.Errorf("not idempotent:\ninput: %q\nonce:  %q\ntwice: %q", row.input, res.Text, twice)
		}
	}
	if hidden == 0 {
		t.Fatal("grammar produced no hidden rows")
	}
}

// TestCorpusGrammarKnownGaps pins the generated rows that leak. When a row
// starts being hidden the test fails: its class predicate no longer holds, and
// the row belongs in the hidden grammar.
func TestCorpusGrammarKnownGaps(t *testing.T) {
	leaking := 0
	for _, row := range grammarHideRows() {
		if row.class == "" {
			continue
		}
		leaking++
		got := String(row.input)
		visible := false
		for _, n := range row.needles {
			if strings.Contains(got, n) {
				visible = true
			}
		}
		if !visible {
			t.Errorf("gap closed? class %q row %q no longer leaks (output %q): move it to the hidden grammar", row.class, row.input, got)
		}
	}
	if leaking == 0 {
		t.Fatal("grammar produced no leaking rows")
	}
}

func TestCorpusGrammarMustKeep(t *testing.T) {
	for _, row := range grammarKeepRows() {
		if row.class != "" {
			continue
		}
		res := Text(row.input)
		if res.Text != row.input || res.Hits != 0 {
			t.Errorf("Text(%q) altered clean input: got %q (hits=%d cats=%v)", row.input, res.Text, res.Hits, res.Categories)
		}
	}
}

// TestCorpusGrammarOverRedaction pins the generated keep rows that main alters.
func TestCorpusGrammarOverRedaction(t *testing.T) {
	over := 0
	for _, row := range grammarKeepRows() {
		if row.class == "" {
			continue
		}
		over++
		if String(row.input) == row.input {
			t.Errorf("gap closed? %q is no longer altered: move it to the kept grammar", row.input)
		}
	}
	if over == 0 {
		t.Fatal("grammar produced no over-redaction rows")
	}
}
