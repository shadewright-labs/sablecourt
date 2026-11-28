package redact

import (
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// Third review: scan skipped "\n" and "\r" as escaped line breaks, so a held
// value starting with n or r right after a backslash lost its first byte and
// was missed. Every byte before the value is tried, in plain text, in a JSON
// string and in a stream split at every offset, and the output is checked with
// an independent substring test.
func TestRedactExactRawByteView(t *testing.T) {
	reg := NewRegistry()
	var held []string
	for _, first := range []string{"n", "r", "\\", "u", "x"} {
		v := synthSecret(first, "aw_view_", "value_0123")
		held = append(held, v)
		reg.Register(v)
	}
	before := []string{"", "\\", "\\\\", "\\\\\\", "\r", "\n", "\\r", "\\n", "\"", " ", "a"}
	for _, h := range held {
		for _, b := range before {
			for _, doc := range []string{
				"x" + b + h + "y",
				`{"p":"` + b + h + `"}`,
				b + h,
			} {
				name := fmt.Sprintf("%q", doc)
				t.Run(name, func(t *testing.T) {
					if got := reg.RedactExact(doc); strings.Contains(got, h) {
						t.Fatalf("RedactExact(%q) = %q keeps the held value", doc, got)
					}
					if !reg.ContainsHeld(doc) {
						t.Fatalf("ContainsHeld(%q) = false", doc)
					}
					if !reg.HeldInJSON(doc) {
						t.Fatalf("HeldInJSON(%q) = false", doc)
					}
					for cut := 0; cut <= len(doc); cut++ {
						s := reg.NewStream()
						out := string(s.Write([]byte(doc[:cut]), false)) + string(s.Write([]byte(doc[cut:]), true))
						if strings.Contains(out, h) {
							t.Fatalf("stream split at %d emitted %q", cut, out)
						}
					}
				})
			}
		}
	}
}

// The values that replace a match never disclose one: a registered value that
// overlaps the marker turns the marker off, and a held value rebuilt by a
// replacement (exact or pattern) refuses the whole text.
func TestMarkerNeverDiscloses(t *testing.T) {
	for _, tc := range []struct {
		name, held string
		docs       []string
	}{
		{"value inside the marker", "REDACTED:known", []string{"a REDACTED:known b", KnownSecretMarker}},
		{"marker ends where the value starts", "secret]tail_value", []string{"x secret]tail_value y", "known_secret]tail_value"}},
		{"value ends where the marker starts", "head_value_[REDACT", []string{"head_value_[REDACT", "a head_value_[REDACT b"}},
		{"pattern marker rebuilds the value", "bearer_token]]tail", []string{"Authorization: Bearer abcdefghij]tail"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reg := NewRegistry()
			reg.Register(tc.held)
			other := synthSecret("other", "_held_", "value_0456")
			reg.Register(other)
			for _, doc := range append(tc.docs, "pre "+other+" post") {
				if got := reg.RedactString(doc); strings.Contains(got, tc.held) {
					t.Fatalf("RedactString(%q) = %q shows the held value", doc, got)
				}
				if got := reg.RedactExact(doc); strings.Contains(got, tc.held) {
					t.Fatalf("RedactExact(%q) = %q shows the held value", doc, got)
				}
				for cut := 0; cut <= len(doc); cut++ {
					s := reg.NewStream()
					out := string(s.Write([]byte(doc[:cut]), false)) + string(s.Write([]byte(doc[cut:]), true))
					if strings.Contains(out, tc.held) || strings.Contains(out, other) {
						t.Fatalf("stream split at %d emitted %q", cut, out)
					}
				}
			}
			if got := reg.RefusedJSON(); strings.Contains(got, tc.held) || !json.Valid([]byte(got)) {
				t.Fatalf("RefusedJSON = %q", got)
			}
		})
	}
}

// A declared JSON record is decoded once and refused whole on any match, with
// none of its payload in the refusal. A record holding no value comes back
// byte for byte.
func TestRedactJSONRecord(t *testing.T) {
	reg := NewRegistry()
	held := synthSecret("json", "_held_", "value_0077")
	bin := []byte("bin\x00\x01_held_bytes_0042")
	reg.Register(held)
	reg.RegisterBytes(bin)
	esc := "\\" + "u0073" // "s" as a JSON unicode escape
	refused := `"` + KnownSecretMarker + `"`
	for _, tc := range []struct {
		name, in string
		refuse   bool
	}{
		{"no held value keeps every byte", `{"b":"` + esc + ` <x>", "a":1.50}`, false},
		{"duplicate names without a held value keep every byte", `{"p":"a","p":"b"}`, false},
		{"string value", `{"p":"x ` + held + `"}`, true},
		{"object name", `{"` + held + `":1}`, true},
		{"escaped held value", `{"p":"` + strings.Replace(held, "s", esc, 1) + `"}`, true},
		{"shadowed duplicate name", `{"p":"` + strings.Replace(held, "s", esc, 1) + `","p":"ok"}`, true},
		{"nested array", `{"o":[1,{"k":"` + held + `"}]}`, true},
		{"held value ends the string", `{"p":"` + held + `","q":"ok"}`, true},
		{"JSON inside a string", `{"inner":"{\"k\":\"` + held + `\"}"}`, true},
		{"trailing value", `{"p":1} {"q":"` + strings.Replace(held, "s", esc, 1) + `"}`, true},
		{"not JSON, raw bytes", `not json ` + held, true},
		{"binary value as base64", `{"b":"` + b64(bin) + `"}`, true},
		{"binary value as hex", `{"b":"` + hexLower(bin) + `"}`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := reg.RedactJSONRecord(tc.in)
			if tc.refuse {
				if ok || got != refused {
					t.Fatalf("RedactJSONRecord(%q) = %q, %v, want it refused whole", tc.in, got, ok)
				}
				return
			}
			if !ok || got != tc.in {
				t.Fatalf("RedactJSONRecord(%q) = %q, %v, want it unchanged", tc.in, got, ok)
			}
		})
	}
}

// Documented limits (issue 189): these spellings are outside the listed set,
// so the matcher passes them unchanged. A reader who joins the fragments or
// decodes them again can rebuild the value; the tests pin the limit so a change
// to it is deliberate.
func TestRedactExactDocumentedLimits(t *testing.T) {
	reg := NewRegistry()
	held := synthSecret("limit", "_held_", "value_0031")
	reg.Register(held)
	half := len(held) / 2
	for _, tc := range []struct{ name, in string }{
		{"limit: fragments split across keys are not matched", `{"` + held[:half] + `":"` + held[half:] + `"}`},
		{"limit: fragments split across values are not matched", `["` + held[:half] + `","` + held[half:] + `"]`},
		{"limit: percent encoding is not decoded", strings.Replace(held, "_", "%5F", 1)},
		{"limit: JSON nested in a string is decoded once only", `{"inner":` + jsonString(`{"k":"`+strings.Replace(held, "l", "\\u006c", 1)+`"}`) + `}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := reg.RedactExact(tc.in); got != tc.in {
				t.Fatalf("RedactExact(%q) = %q; the documented limit changed, update the ADR-0015 amendment", tc.in, got)
			}
			if _, ok := reg.RedactJSONRecord(tc.in); !ok {
				t.Fatalf("RedactJSONRecord(%q) refused; the documented limit changed", tc.in)
			}
		})
	}
}

func jsonString(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		panic(err)
	}
	return string(b)
}

func b64(b []byte) string { return base64.StdEncoding.EncodeToString(b) }

func hexLower(b []byte) string { return hex.EncodeToString(b) }

// Fix round, finding 1: the stream emitted text before a pending match and
// replaced the match later, so the emitted text and the marker (or the text
// after a removed match) rebuilt a held value that no scan saw whole. Every
// split is tried, and the pending-cap path with a CR/LF flood.
func TestStreamReplacementNeverJoinsEmittedText(t *testing.T) {
	other := synthSecret("other", "_held_", "value_0456")
	flood := strings.Repeat("\n", maxStreamPending+16)
	for _, tc := range []struct {
		name, held string
		docs       []string
	}{
		{"emitted text ends where the marker starts", "head_value_[REDACT", []string{
			"head_value_" + other,
			"x head_value_" + other + " y",
		}},
		{"emitted text and the text after a removed match", "REDACTED:known", []string{
			"REDACTED:" + other + "known",
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reg := NewRegistry()
			reg.Register(tc.held, other)
			for _, doc := range tc.docs {
				if got := reg.RedactExact(doc); strings.Contains(got, tc.held) {
					t.Fatalf("RedactExact(%q) = %q shows the held value", doc, got)
				}
				if got := reg.RedactString(doc); strings.Contains(got, tc.held) {
					t.Fatalf("RedactString(%q) = %q shows the held value", doc, got)
				}
				for cut := 0; cut <= len(doc); cut++ {
					s := reg.NewStream()
					out := string(s.Write([]byte(doc[:cut]), false)) + string(s.Write([]byte(doc[cut:]), true))
					if strings.Contains(out, tc.held) || strings.Contains(out, other) {
						t.Fatalf("stream split at %d of %q emitted %q", cut, doc, out)
					}
				}
				// Pending cap: an open prefix followed by a CR/LF flood.
				half := len(other) / 2
				s := reg.NewStream()
				pre := strings.TrimSuffix(doc, other[half:])
				if pre == doc {
					continue
				}
				out := string(s.Write([]byte(pre+flood), false)) + string(s.Write([]byte(other[half:]), true))
				out = strings.ReplaceAll(out, "\n", "")
				if strings.Contains(out, tc.held) {
					t.Fatalf("pending cap path emitted the held value")
				}
			}
		})
	}
}

// Fix round, finding 2: every stored form had CR, LF and their escapes
// stripped, so the literal bytes of a value holding them were never matched,
// and a value that stripping shortened below MinSecretLength had no form.
func TestRedactExactValueWithLineBreaks(t *testing.T) {
	for _, tc := range []struct{ name, held, doc string }{
		{"LF after a backslash", "n1234\n5678", "\\n1234\n5678"},
		{"escaped LF shortens below the minimum", "1234\\n56", "x 1234\\n56 y"},
		{"LFs shorten below the minimum", "a\nb\nc\nd\ne", "x a\nb\nc\nd\ne y"},
		{"CRLF inside", "key_\r\n_value", "pre key_\r\n_value post"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reg := NewRegistry()
			reg.Register(tc.held)
			if got := reg.RedactExact(tc.doc); strings.Contains(got, tc.held) {
				t.Fatalf("RedactExact(%q) = %q keeps the held value", tc.doc, got)
			}
			if !reg.ContainsHeld(tc.doc) {
				t.Fatalf("ContainsHeld(%q) = false", tc.doc)
			}
			for cut := 0; cut <= len(tc.doc); cut++ {
				s := reg.NewStream()
				out := string(s.Write([]byte(tc.doc[:cut]), false)) + string(s.Write([]byte(tc.doc[cut:]), true))
				if strings.Contains(out, tc.held) {
					t.Fatalf("stream split at %d emitted %q", cut, out)
				}
			}
		})
	}
}

// Fix round, finding 3: with the marker turned off a match leaves no
// "[REDACTED:" text, and ContainsSensitive reported a held value as absent.
func TestContainsSensitiveWithEmptyMarker(t *testing.T) {
	reg := NewRegistry()
	reg.Register("REDACTED:known")
	if reg.Marker() != "" {
		t.Fatalf("marker should be off")
	}
	if !reg.ContainsSensitive("a REDACTED:known b") {
		t.Fatalf("ContainsSensitive missed a held value")
	}
	// Fix round 2, finding 2: a pattern marker that forms a held value with
	// the text after it makes RedactText refuse the whole text, which leaves
	// no "[REDACTED:" text either.
	reg.Register("bearer_token]]tail_val")
	doc := "Authorization: Bearer abcdefghij]tail_val"
	if reg.ContainsHeld(doc) {
		t.Fatalf("the input itself should show no held value")
	}
	if res := reg.RedactText(doc); res.Hits == 0 || res.Text != "" {
		t.Fatalf("RedactText(%q) = %+v, want the refused empty text", doc, res)
	}
	if !reg.ContainsSensitive(doc) {
		t.Fatalf("ContainsSensitive missed the pattern hit that rebuilt a held value")
	}
	if reg.ContainsSensitive("plain text") {
		t.Fatalf("ContainsSensitive flagged clean text")
	}
}
