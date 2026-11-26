package model

import "testing"

func TestParseGeminiJSONCountsThinkingTokensAsOutput(t *testing.T) {
	cases := []struct {
		name       string
		body       string
		wantIn     int64
		wantOut    int64
		wantParsed bool
	}{
		{"thoughts added to candidates", `{"usageMetadata":{"promptTokenCount":100,"candidatesTokenCount":10,"thoughtsTokenCount":2000}}`, 100, 2010, true},
		{"no thoughts", `{"usageMetadata":{"promptTokenCount":100,"candidatesTokenCount":10}}`, 100, 10, true},
		{"only thoughts", `{"usageMetadata":{"thoughtsTokenCount":7}}`, 0, 7, true},
		{"negative thoughts ignored", `{"usageMetadata":{"promptTokenCount":5,"candidatesTokenCount":3,"thoughtsTokenCount":-9}}`, 5, 3, true},
		{"nothing reported", `{"usageMetadata":{}}`, 0, 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := parseGeminiJSON([]byte(tc.body))
			if ok != tc.wantParsed || got.InputTokens != tc.wantIn || got.OutputTokens != tc.wantOut {
				t.Fatalf("parseGeminiJSON = in %d out %d parsed %v, want in %d out %d parsed %v",
					got.InputTokens, got.OutputTokens, ok, tc.wantIn, tc.wantOut, tc.wantParsed)
			}
		})
	}
}
