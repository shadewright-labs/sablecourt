package redact

import (
	"math"
	"strings"
	"testing"
	"time"
)

// TestRedactorLinearGrowth feeds inputs shaped like each class of the corpus at
// two sizes, 8x apart, and checks that the time Text takes grows no faster than
// a linear bound. For 8x input, O(n) time grows by about 8x, O(n^1.5) by 22.6x
// and O(n^2) by 64x. Small and large runs alternate and each size takes the
// fastest of several runs, which damps scheduler and GC noise; the bound of 16
// leaves 2x above linear and still rejects O(n^1.5). CI runs only with -race,
// which slows Text by about 20x, so under -race the test uses smaller inputs,
// fewer runs and every raceCaseStep-th case, with the same ratio and bound.
func TestRedactorLinearGrowth(t *testing.T) {
	if testing.Short() {
		t.Skip("timing ratios are not measured under -short")
	}
	smallSize, runs, step := 1<<12, 5, 1
	if raceEnabled {
		smallSize, runs, step = 1<<10, 3, raceCaseStep
	}
	const (
		sizeFactor = 8
		maxRatio   = 16.0
	)
	largeSize := sizeFactor * smallSize

	cases := []struct {
		name  string
		build func(int) string
	}{
		{
			name: "nested yaml mapping",
			build: func(n int) string {
				return "secret:\n" + strings.Repeat("  key: value012345\n", max(1, n/19))
			},
		},
		{
			name: "nested yaml unclosed indent near miss",
			build: func(n int) string {
				return "secret:\n" + strings.Repeat(" ", n) + "value"
			},
		},
		{
			name: "nested yaml block scalar with hash lines",
			build: func(n int) string {
				return "private_key: |\n" + strings.Repeat("  #line0123456789\n", max(1, n/18))
			},
		},
		{
			name: "json escaped newline nested yaml",
			build: func(n int) string {
				return `{"text":"secret:\n` + strings.Repeat(`  key: val012345\n`, max(1, n/18)) + `"}`
			},
		},
		{
			name: "flow collection nested json objects",
			build: func(n int) string {
				return `{"secret": {` + strings.Repeat(`"k":"val012345",`, max(1, n/16)) + `"last":"endval99"}}`
			},
		},
		{
			name: "flow collection yaml sequence",
			build: func(n int) string {
				return "secret: [" + strings.Repeat("item0123, ", max(1, n/10)) + "end01234]"
			},
		},
		{
			name: "url credential near miss without userinfo at sign",
			build: func(n int) string {
				return "https://" + strings.Repeat("a", n/2) + ":" + strings.Repeat("b", n/2) + "/x@y"
			},
		},
		{
			name: "url credential repeated schemes",
			build: func(n int) string {
				return strings.Repeat("https://host.invalid:8443/@p ", max(1, n/29))
			},
		},
		{
			name: "url credential repeated at signs and escaped slashes",
			build: func(n int) string {
				return strings.Repeat(`https:\/\/a:p@w@example.com\/x `, max(1, n/29))
			},
		},
		{
			name: "basic auth header long value",
			build: func(n int) string {
				return "Proxy-Authorization: Basic " + strings.Repeat("A", n) + "!"
			},
		},
		{
			name: "bearer header long value",
			build: func(n int) string {
				return "Proxy-Authorization: Bearer " + strings.Repeat("A", n) + "!"
			},
		},
		{
			name: "digest auth header long params",
			build: func(n int) string {
				return `Authorization: Digest ` + strings.Repeat(`k="v", `, max(1, n/7))
			},
		},
		{
			name: "cookie header many pairs",
			build: func(n int) string {
				return "Cookie: " + strings.Repeat("k=v12345; ", max(1, n/10))
			},
		},
		{
			name: "jwt missing third segment",
			build: func(n int) string {
				return "eyJ" + strings.Repeat("a", n/2) + "." + strings.Repeat("b", n/2)
			},
		},
		{
			name: "jwt repeated dotted prefixes",
			build: func(n int) string {
				return strings.Repeat("eyJaaaaaaa.", max(1, n/11))
			},
		},
		{
			name: "jwt repeated valid headers without payload",
			build: func(n int) string {
				return strings.Repeat("eyJhbGciOiJIUzI1NiJ9.short ", max(1, n/27))
			},
		},
		{
			name: "pem truncated body",
			build: func(n int) string {
				return joinParts("-----BEGIN ", "RSA PRIVATE KEY-----\n") + strings.Repeat("MIIEfixture\n", max(1, n/12))
			},
		},
		{
			name: "pem repeated nested headers with final footer",
			build: func(n int) string {
				return joinParts("-----BEGIN ", "RSA PRIVATE KEY-----\n") +
					strings.Repeat(joinParts("-----BEGIN ", "EC PRIVATE KEY-----\n"), max(1, n/29)) +
					joinParts("-----END ", "RSA PRIVATE KEY-----")
			},
		},
		{
			name: "pem repeated unclosed grep lines",
			build: func(n int) string {
				return strings.Repeat("grep '"+joinParts("-----BEGIN ", "PRIVATE KEY-----")+"' x\n", max(1, n/38))
			},
		},
		{
			name: "noise split provider token invalid utf8",
			build: func(n int) string {
				return "ghp_" + strings.Repeat("a\xff", max(1, n/2))
			},
		},
		{
			name: "noise split provider token zwsp and ansi csi",
			build: func(n int) string {
				return "ghp_" + strings.Repeat("a\u200b\x1b[0m", max(1, n/8))
			},
		},
		{
			name: "noise malformed ansi escape storm",
			build: func(n int) string {
				return strings.Repeat("\x1b[", max(1, n/2))
			},
		},
		{
			name: "inline quoted kv unclosed double quote",
			build: func(n int) string {
				return `password="` + strings.Repeat("a", n)
			},
		},
		{
			name: "inline quoted kv unclosed single quote",
			build: func(n int) string {
				return `password='` + strings.Repeat("a", n)
			},
		},
		{
			name: "inline multiline double quoted kv",
			build: func(n int) string {
				return `password="` + strings.Repeat("abcdefgh\n", max(1, n/9)) + `end"`
			},
		},
		{
			name: "xml secret tags repeated",
			build: func(n int) string {
				return strings.Repeat("<password>val01234</password>", max(1, n/29))
			},
		},
		{
			name: "cli flags and curl user repeated",
			build: func(n int) string {
				return strings.Repeat("curl -u a:pass1234 --password secret1234; ", max(1, n/42))
			},
		},
		{
			name: "stripe and slack tokens repeated",
			build: func(n int) string {
				tok := joinParts("sk_", "live_", "0123456789abcdefghij") + " " + joinParts("xox", "b-", "0123456789-abcdef") + "\n"
				return strings.Repeat(tok, max(1, n/len(tok)))
			},
		},
		{
			name: "N1 unquoted kv contiguous repeated keys",
			build: func(n int) string {
				return strings.Repeat("password=", max(1, n/9))
			},
		},
		{
			name: "N1 cookie header repeated on one line",
			build: func(n int) string {
				return strings.Repeat("Cookie:a=b ", max(1, n/11))
			},
		},
		{
			name: "N1 cookie header repeated without equals on line",
			build: func(n int) string {
				return strings.Repeat("Cookie: foo ", max(1, n/12)) + "\nCookie: a=b"
			},
		},
		{
			name: "N1 digest auth header repeated on one line",
			build: func(n int) string {
				return strings.Repeat("Authorization: Digest k=v ", max(1, n/26))
			},
		},
		{
			name: "N1 url credential repeated schemes with trailing at",
			build: func(n int) string {
				return strings.Repeat("a://", max(1, n/4)) + "@"
			},
		},
		{
			name: "N1 url credential repeated escaped schemes with trailing at",
			build: func(n int) string {
				return strings.Repeat(`a:\/\/`, max(1, n/6)) + "@"
			},
		},
		{
			name: "N1 url credential repeated user-pass-slash with trailing at",
			build: func(n int) string {
				return strings.Repeat("a://u:p/", max(1, n/8)) + "@"
			},
		},
		{
			name: "provider tokens sablecourt anthropic openai google aws",
			build: func(n int) string {
				tok := joinParts("sc_task_", "0123456789abcdef0123") + " " +
					joinParts("sk-ant-", "0123456789abcdef0123") + " " +
					joinParts("sk-proj-", "0123456789abcdef0123") + " " +
					joinParts("AIza", "0123456789abcdefghij0123") + " " +
					joinParts("AKIA", "0123456789ABCDEF") + "\n"
				return strings.Repeat(tok, max(1, n/len(tok)))
			},
		},
		{
			name: "jwt repeated empty seg2 and short seg2",
			build: func(n int) string {
				return strings.Repeat("eyJhbGciOiJIUzI1NiJ9..eyJhbGciOiJIUzI1NiJ9.a.", max(1, n/45))
			},
		},
		{
			name: "pem truncated inline base64 body",
			build: func(n int) string {
				return joinParts("-----BEGIN ", "PRIVATE KEY----- ") + strings.Repeat("MIIEfixture0", max(1, n/12))
			},
		},
		{
			name: "N1 pem repeated headers on one line",
			build: func(n int) string {
				return strings.Repeat(joinParts("-----BEGIN ", "PRIVATE KEY----- "), max(1, n/28))
			},
		},
		{
			name: "N1 kv repeated open-paren call near misses",
			build: func(n int) string {
				return strings.Repeat("password=fn(", max(1, n/12))
			},
		},
		{
			name: "kv long identifier then many open parens",
			build: func(n int) string {
				return "password=" + strings.Repeat("a", n/2) + strings.Repeat("(", n/2)
			},
		},
		{
			name: "N1 kv repeated unclosed double quotes",
			build: func(n int) string {
				return strings.Repeat("password=\"abcdef\n", max(1, n/17))
			},
		},
		{
			name: "N1 json set-cookie array many entries",
			build: func(n int) string {
				return `{"Set-Cookie":[` + strings.Repeat(`"a=b; Path=/",`, max(1, n/14)) + `"z=1"]}`
			},
		},
		{
			name: "N1 url credential repeated single-quoted schemes with trailing at",
			build: func(n int) string {
				return strings.Repeat("'a://", max(1, n/5)) + "@"
			},
		},
	}

	for i, tc := range cases {
		if i%step != 0 {
			continue
		}
		t.Run(tc.name, func(t *testing.T) {
			smallInput := tc.build(smallSize)
			largeInput := tc.build(largeSize)

			smallTime, largeTime := fastestRuns(smallInput, largeInput, runs)

			ratio := float64(largeTime) / float64(smallTime)
			if ratio > maxRatio {
				t.Fatalf("time ratio %.2f exceeded linear bound %.1f (small=%v large=%v smallLen=%d largeLen=%d)",
					ratio, maxRatio, smallTime, largeTime, len(smallInput), len(largeInput))
			}
		})
	}
}

// fastestRuns calls Text on small and large in turn, after one warm-up call of
// each, and returns the shortest wall time seen for each.
// raceCaseStep thins the cases under -race: every raceCaseStep-th one runs.
const raceCaseStep = 3

// growthText is the function under test; the proof that the test catches
// quadratic growth replaces it.
var growthText = Text

func fastestRuns(small, large string, runs int) (time.Duration, time.Duration) {
	growthText(small)
	growthText(large)
	bestSmall := time.Duration(math.MaxInt64)
	bestLarge := time.Duration(math.MaxInt64)
	for range runs {
		start := time.Now()
		growthText(small)
		bestSmall = min(bestSmall, time.Since(start))
		start = time.Now()
		growthText(large)
		bestLarge = min(bestLarge, time.Since(start))
	}
	return max(bestSmall, time.Microsecond), max(bestLarge, time.Microsecond)
}

func TestGrowthBoundRejectsSuperlinear(t *testing.T) {
	const (
		smallN   = 1 << 10
		largeN   = 8 * smallN
		maxRatio = 16.0
	)
	for _, tc := range []struct {
		name       string
		work       func(int) float64
		wantLinear bool
	}{
		{"O(n)", func(n int) float64 { return float64(10 * n) }, true},
		{"O(n^1.5)", func(n int) float64 { return math.Pow(float64(n), 1.5) }, false},
		{"O(n^2)", func(n int) float64 { return float64(n) * float64(n) }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ratio := tc.work(largeN) / tc.work(smallN)
			if got := ratio <= maxRatio; got != tc.wantLinear {
				t.Fatalf("ratio %.2f classified linear=%t, want %t", ratio, got, tc.wantLinear)
			}
		})
	}
}
