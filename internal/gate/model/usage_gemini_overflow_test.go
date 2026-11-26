package model

import (
	"errors"
	"testing"

	"github.com/shadewright-labs/armory/modelusage"
)

// A count above int64 fails the decode; it must be classified like a count that decodes and is out of range.
func TestParseGeminiCountAboveInt64IsOutOfRange(t *testing.T) {
	cases := []struct {
		name string
		body string
		want error
	}{
		{"one above int64", `{"usageMetadata":{"promptTokenCount":9223372036854775808,"candidatesTokenCount":1}}`, modelusage.ErrOutOfRange},
		{"far above int64", `{"usageMetadata":{"promptTokenCount":1,"thoughtsTokenCount":99999999999999999999999}}`, modelusage.ErrOutOfRange},
		{"below the lowest int64", `{"usageMetadata":{"promptTokenCount":-9223372036854775809}}`, modelusage.ErrOutOfRange},
		{"largest int64", `{"usageMetadata":{"promptTokenCount":9223372036854775807,"candidatesTokenCount":1}}`, modelusage.ErrOutOfRange},
		{"a fraction", `{"usageMetadata":{"promptTokenCount":1.5}}`, modelusage.ErrMalformed},
		{"an exponent", `{"usageMetadata":{"promptTokenCount":1e30}}`, modelusage.ErrMalformed},
		{"a string", `{"usageMetadata":{"promptTokenCount":"12"}}`, modelusage.ErrMalformed},
		{"cut off", `{"usageMetadata":{"promptTokenCount":12`, modelusage.ErrMalformed},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			usage, ok, err := parseGeminiJSONChecked([]byte(c.body))
			if ok || !errors.Is(err, c.want) {
				t.Fatalf("usage=%+v ok=%v err=%v, want not ok and %v", usage, ok, err, c.want)
			}
		})
	}
}

// In a stream, earlier usage is dropped for a later count above int64, as it is for one that decodes out of range.
func TestParseGeminiSSECountAboveInt64DropsEarlierUsage(t *testing.T) {
	first := `data: {"usageMetadata":{"promptTokenCount":5000,"candidatesTokenCount":4000}}` + "\n\n"
	for _, last := range []string{"9223372036854775808", "9223372036854775807"} {
		body := first + `data: {"usageMetadata":{"promptTokenCount":` + last + `,"candidatesTokenCount":1}}` + "\n\n"
		got := parseGeminiUsage([]byte(body), true)
		if got.Outcome != usageOutcomeNone || !errors.Is(got.Err, modelusage.ErrOutOfRange) {
			t.Errorf("last count %s: outcome=%v err=%v usage=%+v, want no usage and ErrOutOfRange", last, got.Outcome, got.Err, got.Usage)
		}
	}
}
