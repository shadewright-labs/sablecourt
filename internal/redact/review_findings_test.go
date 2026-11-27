package redact

import (
	"strings"
	"testing"
	"time"
)

// TestReviewFindings holds one row per finding of the security review of the
// rewritten pattern rules: secrets the previous rules redacted that the
// rewrite let through, and evidence an unclosed collection swallowed.
func TestReviewFindings(t *testing.T) {
	secret := joinParts("Zq9x", "K2mV", "7pLw")
	runClaimRows(t, []claimRow{
		// A quote before the key that does not close it: the value follows.
		{name: "quote before key with double quoted value", input: `"api_key="` + secret + `"`, hidden: []string{secret}},
		{name: "quote before key with single quoted value", input: `'password='` + secret + `'`, hidden: []string{secret}},
		{name: "grep pattern with quoted key kept", input: `grep "password:" config.yml`, kept: []string{`grep "password:" config.yml`}},
		{name: "grep pattern with single quoted key kept", input: `grep -r 'token=' src/`, kept: []string{`grep -r 'token=' src/`}},
		{name: "json empty value then next key kept", input: "{\"password\":\n  null,\n  \"user\": \"alice\"}", kept: []string{`"user": "alice"`}},
		// Values that start with '(' or '<'.
		{name: "value in parentheses", input: "password=(" + secret + ")", hidden: []string{secret}},
		{name: "value after angle bracket", input: "password=<" + secret, hidden: []string{secret}},
		{name: "value in angle brackets", input: "token: <" + secret + ">", hidden: []string{secret}},
		{name: "angle placeholder kept", input: "password=<redacted>", kept: []string{"<redacted>"}},
		{name: "upper case angle placeholder kept", input: "api_key=<YOUR_API_KEY>", kept: []string{"<YOUR_API_KEY>"}},
		{name: "parenthesized placeholder kept", input: "password=(none)", kept: []string{"(none)"}},
		{name: "parenthesized call kept", input: `password=(os.getenv("DB_PASS"))`, kept: []string{"os.getenv"}},
		// A flow collection that is never closed.
		{
			name:  "unclosed flow mapping keeps a stack trace",
			input: "panic: tokens: {\ngoroutine 1 [running]:\nmain.main()\n\t/src/main.go:12 +0x1d\nexit status 2\n",
			kept:  []string{"goroutine 1 [running]:", "main.main()", "/src/main.go:12 +0x1d", "exit status 2"},
		},
		{
			name:   "unclosed flow sequence hides its own line only",
			input:  "secret: [" + secret + ", \nerror: build failed at step 3\n",
			hidden: []string{secret},
			kept:   []string{"error: build failed at step 3"},
		},
		{
			name:   "multi-line flow sequence hidden",
			input:  "secrets: [\n  " + secret + ",\n  other\n]\nstatus: ok\n",
			hidden: []string{secret},
			kept:   []string{"status: ok"},
		},
		{
			name:   "pretty printed json mapping hidden",
			input:  "{\n  \"secret\": {\n    \"value\": \"" + secret + "\"\n  },\n  \"status\": \"ok\"\n}\n",
			hidden: []string{secret},
			kept:   []string{`"status": "ok"`},
		},
	})
}

// TestReviewManyParensLinear checks the call check runs once per value: a long
// name followed by many '(' took quadratic time (about 17 seconds for
// 512 KiB). The counted steps cover the scanner; the clock covers code that
// does not count its steps, with a bound far above the linear run time.
func TestReviewManyParensLinear(t *testing.T) {
	n := 1 << 18
	in := "password=" + strings.Repeat("a", n) + strings.Repeat("(", n)
	start := time.Now()
	_, st := textWithStats(in)
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("redacting %d bytes took %v", len(in), d)
	}
	if st.scannerSteps > 40*len(in) {
		t.Fatalf("scannerSteps %d for len %d: call check is not linear", st.scannerSteps, len(in))
	}
}
