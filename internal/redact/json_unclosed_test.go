package redact

import (
	"fmt"
	"strings"
	"testing"
)

// The unclosed-string rows are generated: a JSON string left open, then a
// JSON value that holds a synthetic secret under a key the JSON rule knows.
// The open string makes the next quote close it, so every quote after it is
// read with the wrong parity. The answer is known by construction: the secret
// was placed in the input and must not come back. The rows do not ask what
// survives around the secret: when a string is left open the redactor may
// hide more than the value (fail closed), never less.

// unclosedSecret is the synthetic secret placed in every row. It has no
// quote, backslash or space, so any part of it in the output is a leak.
var unclosedSecret = joinParts("Uc7q", "Wm2x", "Rk9v")

// unclosedSecretKeys are key names the JSON rule matches, so a row tests the
// open string, not the key list.
var unclosedSecretKeys = []string{"password", "api_key", "client_secret", "access_token", "operator-token"}

// unclosedOpeners place the open string in each spot a JSON string can sit:
// the value of a secret key, of a plain key, an array item, a key, and a bare
// top-level string.
var unclosedOpeners = []struct{ name, text string }{
	{"secret value", `{"passwd":"`},
	{"plain value", `{"note":"`},
	{"array item", `{"items":["`},
	{"key", `{"`},
	{"top level", `"`},
	{"log line", `msg="request failed" body={"password":"`},
}

// unclosedBodies are the text inside the open string. The last two end in a
// backslash, which escapes the next quote, so the open string swallows the
// opening quote of the next key as well.
var unclosedBodies = []struct{ name, text string }{
	{"empty", ``},
	{"words", `unclosed value`},
	{"escaped quote", `say \"hi\" then`},
	{"escaped backslash", `C:\\temp\\`},
	{"escapes", `line\nnext\ttab \u00e9`},
	{"structure", `a, "b": {c: [d]`},
	{"raw newline", "first\nsecond"},
	{"trailing backslash", `abc\`},
	{"trailing backslash after escapes", `x\\\`},
}

// unclosedSeps join the open string to the next value.
var unclosedSeps = []string{``, `, `, `,`, ",\n  ", "\n", ` `}

// unclosedValues hold the secret as a JSON value after the open string, in
// each spot the JSON grammar allows: a member, a member after a plain one, a
// nested object, an array of objects, spaced colons, a member with more after
// it, and a second open string before it.
func unclosedValues(key, secret string) []struct{ name, text string } {
	return []struct{ name, text string }{
		{"member", `"` + key + `":"` + secret + `"}`},
		{"member after plain member", `"a":1, "` + key + `":"` + secret + `"}`},
		{"nested object", `"cfg":{"` + key + `":"` + secret + `"}}`},
		{"array of objects", `"list":[{"` + key + `":"` + secret + `"}]}`},
		{"spaced colon", `"` + key + `" : "` + secret + `" }`},
		{"pretty member", "\"" + key + "\": \"" + secret + "\"\n}"},
		{"member then more", `"` + key + `":"` + secret + `","user":"alice"}`},
		{"after second open string", `"b":"also open, "` + key + `":"` + secret + `"}`},
		{"value with spaces", `"` + key + `":"` + secret + ` and more words"}`},
		{"value with escapes", `"` + key + `":"` + secret + `\"x\\y"}`},
	}
}

// unclosedRows builds the combinations. Rows whose secret sits in the open
// string itself (the opener is a secret key's value) are built too: the
// secret is the body, and nothing closes it.
func unclosedRows() []grammarRow {
	var rows []grammarRow
	add := func(op, body, sep, key, v struct{ name, text string }) {
		rows = append(rows, grammarRow{
			name:    fmt.Sprintf("%s/%s/%q/%s/%s", op.name, body.name, sep.text, key.text, v.name),
			input:   op.text + body.text + sep.text + v.text,
			needles: []string{unclosedSecret},
		})
	}
	named := func(s string) struct{ name, text string } { return struct{ name, text string }{s, s} }
	// Every opener, body, separator and value with one key, then every key
	// with every value after one opener, body and separator: the key name
	// does not change where the parser is.
	for _, op := range unclosedOpeners {
		for _, body := range unclosedBodies {
			for _, sep := range unclosedSeps {
				for _, v := range unclosedValues(unclosedSecretKeys[1], unclosedSecret) {
					add(op, body, named(sep), named(unclosedSecretKeys[1]), v)
				}
			}
		}
	}
	for _, key := range unclosedSecretKeys {
		for _, body := range unclosedBodies {
			for _, v := range unclosedValues(key, unclosedSecret) {
				add(unclosedOpeners[0], body, named(", "), named(key), v)
			}
		}
	}
	// The secret inside the open value of a secret key, followed by plain
	// JSON or by nothing.
	for _, key := range unclosedSecretKeys {
		for _, tail := range []string{``, `}`, `, "user":"alice"}`, ",\n  \"user\": \"alice\"\n}", `\`, `\", "user":"alice"}`} {
			in := `{"` + key + `":"` + unclosedSecret + tail
			rows = append(rows, grammarRow{
				name:    fmt.Sprintf("open secret value/%s/%q", key, tail),
				input:   in,
				needles: []string{unclosedSecret},
			})
		}
	}
	return rows
}

func TestUnclosedJSONStringMustHide(t *testing.T) {
	rows := unclosedRows()
	leaked := 0
	for _, row := range rows {
		res := Text(row.input)
		bad := ""
		for _, n := range row.needles {
			if strings.Contains(res.Text, n) {
				bad = n
			}
		}
		if bad != "" {
			leaked++
			if leaked <= 20 {
				t.Errorf("%s: leaked %q\ninput:  %q\noutput: %q", row.name, bad, row.input, res.Text)
			}
			continue
		}
		if twice := String(res.Text); twice != res.Text {
			t.Errorf("%s: not idempotent\ninput: %q\nonce:  %q\ntwice: %q", row.name, row.input, res.Text, twice)
		}
	}
	if leaked > 0 {
		t.Errorf("%d of %d generated rows leaked the secret", leaked, len(rows))
	}
}

// TestClosedJSONKeepsNeighbours is the same grammar with every string closed:
// well-formed JSON. Each secret value is hidden and every plain value next to
// it survives, so hiding more after an open string does not spill into
// well-formed input.
func TestClosedJSONKeepsNeighbours(t *testing.T) {
	plain := []string{"alice", "request failed", `say \"hi\" then`, `C:\\temp\\`, "line\\nnext"}
	for _, key := range unclosedSecretKeys {
		for _, p := range plain {
			inputs := []string{
				`{"note":"` + p + `","` + key + `":"` + unclosedSecret + `","user":"` + p + `"}`,
				"{\n  \"note\": \"" + p + "\",\n  \"" + key + "\": \"" + unclosedSecret + "\",\n  \"user\": \"" + p + "\"\n}",
				`{"items":["` + p + `",{"` + key + `":"` + unclosedSecret + `"}],"user":"` + p + `"}`,
				`{"` + key + `":"` + unclosedSecret + `"} {"user":"` + p + `"}`,
				`{"` + key + `":"` + unclosedSecret + `"}` + "\n" + `{"user":"` + p + `"}`,
				`{"cfg":{"` + key + `":"` + unclosedSecret + `"},"user":"` + p + `"}`,
				`[{"` + key + `":"` + unclosedSecret + `"},"` + p + `"]`,
			}
			for _, in := range inputs {
				got := String(in)
				if strings.Contains(got, unclosedSecret) {
					t.Errorf("leaked secret: input %q output %q", in, got)
				}
				want := strings.ReplaceAll(in, unclosedSecret, "[REDACTED:secret]")
				if got != want {
					t.Errorf("well-formed JSON changed beyond the secret value:\ninput: %q\ngot:   %q\nwant:  %q", in, got, want)
				}
			}
		}
	}
}
