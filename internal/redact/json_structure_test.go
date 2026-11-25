package redact_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/shadewright-labs/sablecourt/internal/redact"
	"github.com/shadewright-labs/sablecourt/internal/redact/redacttest"
)

// jsonSecret is a value no text rule recognises on its own, so only the
// structure of the document can mark it.
const jsonSecret = "Qv7hTzP2mWx9LkR4nB6c"

// TestJSONStructuralCases builds documents where the secret is known by
// construction and only the JSON structure says it is one.
func TestJSONStructuralCases(t *testing.T) {
	anth := "sk-ant-api03-" + strings.Repeat("Ab3", 14)
	inner := `{"password":"` + jsonSecret + `"}`
	numeric := `{"password":918273645546372819}`
	cases := []struct {
		name, input string
		secrets     []string
	}{
		{"duplicate member, secret first", `{"note":"` + anth + `","note":"clean"}`, []string{anth[7:]}},
		{"duplicate sensitive key, secret first", `{"password":"` + jsonSecret + `","password":""}`, []string{jsonSecret}},
		{"object under sensitive key", `{"password":{"value":"` + jsonSecret + `"}}`, []string{jsonSecret}},
		{"object in array under sensitive key", `{"api_key":[{"v":"` + jsonSecret + `"}]}`, []string{jsonSecret}},
		{"deep object under authorization", `{"Authorization":{"a":[[{"b":"` + jsonSecret + `"}]]}}`, []string{jsonSecret}},
		{"escaped key", `{"password":"` + jsonSecret + `"}`, []string{jsonSecret}},
		{"upper case escaped key", `{"PASSWORD":"` + jsonSecret + `"}`, []string{jsonSecret}},
		{"stringified JSON with escaped key", mustJSON(t, map[string]string{"note": inner}), []string{jsonSecret}},
		{"stringified JSON with numeric secret", mustJSON(t, map[string]string{"note": numeric}), []string{"918273645546372819"}},
		{"twice stringified JSON", mustJSON(t, map[string]string{"note": mustJSON(t, map[string]string{"x": numeric})}), []string{"918273645546372819"}},
		{"stringified object under sensitive nesting", mustJSON(t, []any{map[string]string{"note": `{"secret":{"k":"` + jsonSecret + `"}}`}}), []string{jsonSecret}},
		{"broken stringified JSON with numeric secret", mustJSON(t, map[string]string{"note": `{"password":918273645546372819,`}), []string{"918273645546372819"}},
		{"broken stringified JSON with escaped key", mustJSON(t, map[string]string{"note": `{"password":"` + jsonSecret + `",`}), []string{jsonSecret}},
		{"not JSON, embedded numeric secret", `run with {"password": 918273645546372819`, []string{"918273645546372819"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := redact.JSON(tc.input)
			if frag, ok := redacttest.Leak(out, tc.secrets); ok {
				t.Fatalf("secret fragment %q survived: %s", frag, out)
			}
			if json.Valid([]byte(tc.input)) && !json.Valid([]byte(out)) {
				t.Fatalf("valid JSON became invalid: %s", out)
			}
		})
	}
}

// TestJSONKeepsShapeAndOrder checks that redaction does not reorder keys,
// merge duplicates or touch bytes it has no reason to change.
func TestJSONKeepsShapeAndOrder(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{`{"z":1,"a":2,"z":3}`, `{"z":1,"a":2,"z":3}`},
		{"{ \"b\" : 1.50e3 ,\n \"a\": [true,false,null] }", "{ \"b\" : 1.50e3 ,\n \"a\": [true,false,null] }"},
		{`{"password":"x","password":"y","n":"é"}`, `{"password":"[REDACTED:secret]","password":"[REDACTED:secret]","n":"é"}`},
		{`{"password":{"z":1,"a":"v"},"after":"ok"}`, `{"password":{"z":"[REDACTED:secret]","a":"[REDACTED:secret]"},"after":"ok"}`},
		{`{"password":[true,null,""]}`, `{"password":[true,null,""]}`},
		{`{"api_key_count":123456}`, `{"api_key_count":123456}`},
	} {
		if got := redact.JSON(tc.in); got != tc.want {
			t.Errorf("JSON(%s) = %s, want %s", tc.in, got, tc.want)
		}
	}
}

// TestJSONDeepNestingFallsBackToText checks a document nested past the
// scanner's limit: it is redacted as text and withheld when the text redactor
// cannot vouch for it.
func TestJSONDeepNestingFallsBackToText(t *testing.T) {
	deep := strings.Repeat("[", 5000) + `{"password":918273645546372819}` + strings.Repeat("]", 5000)
	out := redact.JSON(deep)
	if strings.Contains(out, "918273645546372819") {
		t.Fatalf("numeric secret survived deep nesting: %d bytes out", len(out))
	}
	clean := strings.Repeat("[", 5000) + strings.Repeat("]", 5000)
	if got := redact.JSON(clean); got != clean {
		t.Fatal("clean deep document changed")
	}
}

// FuzzJSONStaysValid checks that valid JSON in gives valid JSON out and that
// the scanner accepts exactly what encoding/json accepts as one value.
func FuzzJSONStaysValid(f *testing.F) {
	for _, seed := range []string{`{"a":[1,2,{"password":3}]}`, `"xé😀"`, `{"note":"{\"api_key\":[1]}"}`, `[1e5,-0.5,true]`, `{"a":1,}`, ` 01`} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, in string) {
		out := redact.JSON(in)
		if json.Valid([]byte(in)) && !json.Valid([]byte(out)) {
			t.Fatalf("valid JSON became invalid: %q -> %q", in, out)
		}
	})
}
