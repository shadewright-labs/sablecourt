package mcpgate

import (
	"encoding/json"
	"errors"
	"testing"
)

func TestDecodeGenericRefusesAmbiguousObjects(t *testing.T) {
	cases := []struct {
		raw    string
		wantOK bool
	}{
		{`{"a":1,"b":{"c":[1,{"d":2}]}}`, true},
		{`{"n":12345678901234567890}`, true},
		{`{"a":1,"a":2}`, false},
		{`{"a":1,"A":2}`, false},
		{`{"a":1,"a":null}`, false},
		{`{"b":{"c":1,"c":2}}`, false},
		{`{"b":[{"c":1,"C":2}]}`, false},
		{`{"a":1} {"a":2}`, false},
		// Keys equal under the simple case folding of strings.EqualFold.
		{`{"σ":1,"ς":2}`, false},
		{`{"k":1,"K":2}`, false},
		{`{"σ":1,"τ":2}`, true},
		// A string the decoder would change is refused; a literal U+FFFD and a
		// paired surrogate escape are kept.
		{`{"t":"\ud800"}`, false},
		{`{"t":"\udc00x"}`, false},
		{`{"t":"\ud800\u0041"}`, false},
		{"{\"t\":\"\xff\"}", false},
		{"{\"\xff\":1}", false},
		{`{"t":"�"}`, true},
		{`{"t":"\ufffd"}`, true},
		{`{"t":"\ud83d\ude00"}`, true},
		{`{"t":"\\ud800"}`, true},
		{`[1,2]`, false},
		{`null`, false},
		{``, false},
	}
	for _, tc := range cases {
		v, err := decodeGeneric(json.RawMessage(tc.raw))
		if tc.wantOK != (err == nil) {
			t.Errorf("decodeGeneric(%s) = %v, %v; want ok=%v", tc.raw, v, err, tc.wantOK)
		}
		if err != nil && !errors.Is(err, errInvalidPayload) {
			t.Errorf("decodeGeneric(%s): error %v is not errInvalidPayload", tc.raw, err)
		}
	}
	v, err := decodeGeneric(json.RawMessage(`{"n":12345678901234567890}`))
	if err != nil {
		t.Fatal(err)
	}
	if out, _ := json.Marshal(v); string(out) != `{"n":12345678901234567890}` {
		t.Fatalf("number not kept as written: %s", out)
	}
}

func TestDecodeTypedRefusesStringsTheDecoderWouldChange(t *testing.T) {
	type in struct {
		Title string `json:"title"`
	}
	for _, raw := range []string{`{"title":"\ud800"}`, `{"title":"a\udfffb"}`, "{\"title\":\"\xff\"}"} {
		if v, err := decodeTyped[in](json.RawMessage(raw)); !errors.Is(err, errInvalidPayload) {
			t.Errorf("decodeTyped(%q) = %+v, %v; want errInvalidPayload", raw, v, err)
		}
	}
	for raw, want := range map[string]string{`{"title":"\ufffd"}`: "\uFFFD", `{"title":"\ud83d\ude00"}`: "\U0001F600", `{"title":"\\ud800"}`: `\ud800`} {
		v, err := decodeTyped[in](json.RawMessage(raw))
		if err != nil || v.Title != want {
			t.Errorf("decodeTyped(%q) = %+v, %v; want %q", raw, v, err, want)
		}
	}
}

func TestValidateRefName(t *testing.T) {
	good := []string{"main", "release/1.0", "sablecourt/fix-1", "a_b", "v1.2.3"}
	bad := []string{"", "HEAD", "head", "-x", ".x", "x.", "x.lock", "a/.b", "a//b", "a/", "/a", "a..b", "a b", "a\tb", "a~1", "a^", "a:b", "a?", "a*", "a[", `a\b`, "a@{1}", "ä"}
	for _, r := range good {
		if err := validateRefName(r); err != nil {
			t.Errorf("validateRefName(%q) = %v, want ok", r, err)
		}
	}
	for _, r := range bad {
		if err := validateRefName(r); err == nil {
			t.Errorf("validateRefName(%q) accepted", r)
		}
	}
}
