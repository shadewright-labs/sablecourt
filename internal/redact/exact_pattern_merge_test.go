package redact

import (
	"regexp"
	"strings"
	"testing"
)

var markerRE = regexp.MustCompile(`\[REDACTED:[a-z_]+\]`)

// The exact pass and the pattern pass find their spans on the same input and
// the spans are merged before anything is replaced, so a held value inside,
// overlapping or next to a pattern-shaped credential cannot leave a fragment
// of either. Each case is pre + region + post, where region is what must go:
// with the markers removed, the output must be pre + post. The cred field is
// what the pattern rules alone (the coverage on origin/main) remove; that
// coverage is checked first.
func TestExactAndPatternSpansMerge(t *testing.T) {
	j := func(parts ...string) string { return strings.Join(parts, "") }
	aws := j("AK", "IA", "Q7RZ", "W3X5", "N8M2", "K4P6")
	aws2 := j("AS", "IA", "B2C3", "D4E5", "F6G7", "H8J9")
	gh := j("gh", "p_", strings.Repeat("Zq9X", 9))
	ant := j("sk-", "ant-", "Rk7mPq2xLw9vTz4n", "Yb6c")
	bearerTok := j("tok.", "abc123XYZ", "_tok-987")
	kvVal := j("s3cret", "Value-", "long77")

	for _, tc := range []struct {
		name, pre, region, post, cred string
		held                          []string
	}{
		{"held inside aws", "key ", aws, " end", aws, []string{aws[6:18]}},
		{"held overlaps aws start", "key ", "zq-" + aws, " end", aws, []string{"zq-" + aws[:6]}},
		{"held overlaps aws end", "key ", aws + "-tail9", " end", aws, []string{aws[14:] + "-tail9"}},
		{"held adjacent before aws", "key ", "zq-held9-" + aws, " end", aws, []string{"zq-held9-"}},
		{"held adjacent after aws", "key ", aws + "-held9-zq", " end", aws, []string{"-held9-zq"}},
		{"held contains aws", "key ", "x:" + aws + ":y", " end", aws, []string{"x:" + aws + ":y"}},
		{"held bridges two keys", "keys ", aws + " " + aws2, " end", aws2, []string{aws[10:] + " " + aws2[:6]}},
		{"held inside github token", "t=", gh, " end", gh, []string{gh[8:24]}},
		{"held overlaps anthropic key", "use ", ant + "!zz9", " now", ant, []string{ant[12:] + "!zz9"}},
		{"held inside bearer token", "Authorization: Bearer ", bearerTok, "\nnext", bearerTok, []string{bearerTok[3:15]}},
		{"held overlaps bearer prefix", "Authorization: ", "Bearer " + bearerTok, "\nnext", bearerTok, []string{"Bearer " + bearerTok[:4]}},
		{"held inside kv value", "password=", kvVal, " rest", kvVal, []string{kvVal[2:12]}},
		{"held inside quoted kv", `{"api_key": "`, kvVal, `"}`, kvVal, []string{kvVal[4:14]}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			text := tc.pre + tc.region + tc.post
			if got := redactPatterns(text).Text; strings.Contains(got, tc.cred) {
				t.Fatalf("pattern rules alone keep the credential: %q", got)
			}
			r := NewRegistry()
			r.Register(tc.held...)
			res := r.RedactText(text)
			if got, want := markerRE.ReplaceAllString(res.Text, ""), tc.pre+tc.post; got != want {
				t.Fatalf("RedactText(%q) = %q; without markers %q, want %q", text, res.Text, got, want)
			}
			for _, h := range tc.held {
				if strings.Contains(res.Text, h) {
					t.Fatalf("held value left in %q", res.Text)
				}
			}
			if !strings.Contains(strings.Join(res.Categories, ","), KnownSecretCategory) {
				t.Fatalf("categories %v lack %s", res.Categories, KnownSecretCategory)
			}
		})
	}
}

// patternSpans must reproduce the sequential pattern pass: replacing its spans
// in the input gives redactPatterns, with the same hits and categories.
func TestPatternSpansMatchSequentialRules(t *testing.T) {
	j := func(parts ...string) string { return strings.Join(parts, "") }
	pieces := []string{
		j("AK", "IA", "Q7RZW3X5N8M2K4P6"), j("sk-", "ant-", "Rk7mPq2xLw9vTz4nYb6c"),
		j("sk-", "proj-", "Ab12Cd34Ef56Gh78Ij90"), j("gh", "p_", strings.Repeat("Zq9X", 6)),
		j("AI", "za", "SyA1b2C3d4E5f6G7h8I9j0"), j("sc_", "task_", "0123456789abcdef"),
		"Authorization: Bearer ", "password=", `"api_key": "`, "secret: ", `"`, "'", " ", "\n", ",",
		"-----BEGIN RSA PRIVATE KEY-----", "-----END RSA PRIVATE KEY-----", "plain", "x9y8z7w6",
	}
	rnd := uint32(7)
	next := func() int { rnd = rnd*1664525 + 1013904223; return int(rnd >> 8) }
	for n := range 3000 {
		var b strings.Builder
		for range 1 + n%9 {
			b.WriteString(pieces[next()%len(pieces)])
		}
		in := b.String()
		spans, found := patternSpans(in)
		got, _ := replaceMarkedSpans(in, spans)
		want := redactPatterns(in)
		if got != want.Text || len(found) != want.Hits {
			t.Fatalf("input %q: spans give %q with %d hits, sequential rules give %q with %d", in, got, len(found), want.Text, want.Hits)
		}
	}
}
