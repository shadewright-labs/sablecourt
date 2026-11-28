package redact

import (
	"fmt"
	"math"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestRedactorLinearGrowth feeds inputs shaped like each class of the corpus at
// two sizes, 8x apart, and checks that the CPU time Text takes grows no faster
// than a linear bound. For 8x input, O(n) work grows by about 8x, O(n^1.5) by
// 22.6x and O(n^2) by 64x. Small and large batches alternate and each size
// takes the least of several batches; the bound of 16 leaves 2x above linear
// and still rejects O(n^1.5). Time spent waiting for a CPU is not counted (see
// growthViolation), so a loaded runner does not move the ratio. CI runs only
// with -race, which slows Text by about 20x, so under -race the test uses
// smaller inputs and fewer runs for every case, with the same ratio and bound.
func TestRedactorLinearGrowth(t *testing.T) {
	if testing.Short() {
		t.Skip("timing ratios are not measured under -short")
	}
	smallSize, runs := 1<<12, 5
	if raceEnabled {
		smallSize, runs = 1<<10, 2
	}

	cases := []struct {
		name  string
		build func(int) string
		// minSmall raises the small size for shapes whose regexp leaves the
		// bit-state backtracker, which is cheaper per byte, for the slower NFA
		// near 4 KiB. Both sizes must sit on the same side of that step.
		minSmall int
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
			name:     "pem repeated nested headers with final footer",
			minSmall: 1 << 12,
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
			// Measured exponent is 1.0 to 1.1 from 4 KiB up, but the step from 1 KiB
			// to 4 KiB runs near 1.4 (backtracker to NFA), so 1 KiB to 8 KiB
			// landed at a ratio of 16.7 under -race.
			minSmall: 1 << 12,
		},
		{
			name: "N1 json set-cookie array many entries",
			build: func(n int) string {
				return `{"Set-Cookie":[` + strings.Repeat(`"a=b; Path=/",`, max(1, n/14)) + `"z=1"]}`
			},
		},
		{
			name: "json open secret values on many lines",
			build: func(n int) string {
				return strings.Repeat(`{"password":"open value`+"\n", max(1, n/24))
			},
		},
		{
			name: "json secret values with text after the closing quote",
			build: func(n int) string {
				return strings.Repeat(`"api_key":"v"x `, max(1, n/15))
			},
		},
		{
			name: "json open string then many secret members",
			build: func(n int) string {
				return `{"note":"open\` + strings.Repeat(`"api_key":"v\`, max(1, n/13)) + `"}`
			},
		},
		{
			name: "json secret keys without the opening quote repeated",
			build: func(n int) string {
				return strings.Repeat(`password":"\`, max(1, n/12))
			},
		},
		{
			name: "N1 url credential repeated single-quoted schemes with trailing at",
			build: func(n int) string {
				return strings.Repeat("'a://", max(1, n/5)) + "@"
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if msg := growthViolation(Text, tc.build, max(smallSize, tc.minSmall), runs); msg != "" {
				t.Fatal(msg)
			}
		})
	}
}

const (
	growthSizeFactor = 8
	growthMaxRatio   = 16.0
	// growthAttempts is how many times a ratio over the bound is measured
	// again before it fails. Quadratic work exceeds the bound on every
	// attempt; a run slowed by its machine does not.
	growthAttempts = 3
	// growthRetryBelow is the ratio under which a failed attempt is measured
	// again. A ratio above it (O(n^2) gives about 64) fails at once.
	growthRetryBelow = 2 * growthMaxRatio
	// growthMinBatch is the least CPU time one batch of small calls takes.
	// Each batch repeats the call until it reaches this, so the clock's
	// resolution is small against what it measures.
	growthMinBatch = 2 * time.Millisecond
	growthMaxReps  = 256
)

// growthViolation builds the input at smallSize and growthSizeFactor times
// that, measures the CPU time fn takes on both, and returns a message if the
// ratio exceeds growthMaxRatio on every attempt, or "" if one attempt stays
// within it. The real test and the self-test below both go through it.
//
// The measure is CPU time, not wall time (issues 124 and 237): on Linux the
// CPU time of the OS thread the goroutine is locked to, which does not grow
// while the thread waits for a CPU, so other processes, other test packages
// and other goroutines on a busy runner do not change the ratio.
func growthViolation(fn func(string) Result, build func(int) string, smallSize, runs int) string {
	smallInput := build(smallSize)
	largeInput := build(growthSizeFactor * smallSize)
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	var attempts []string
	for range growthAttempts {
		smallTime, largeTime, reps := fastestRuns(fn, smallInput, largeInput, runs)
		ratio := float64(largeTime) / float64(smallTime)
		if ratio <= growthMaxRatio {
			return ""
		}
		attempts = append(attempts, fmt.Sprintf("ratio %.2f (small=%v large=%v for %d calls)", ratio, smallTime, largeTime, reps))
		if ratio > growthRetryBelow {
			break
		}
	}
	return fmt.Sprintf("%s ratio exceeded linear bound %.1f (smallLen=%d largeLen=%d): %s",
		workClockName, growthMaxRatio, len(smallInput), len(largeInput), strings.Join(attempts, "; "))
}

// fastestRuns calls fn on small and large in turn, in batches of reps calls
// each, after one warm-up call of each, and returns the least CPU time seen
// for a batch of each and reps. reps is set so a batch of small calls takes at
// least growthMinBatch.
func fastestRuns(fn func(string) Result, small, large string, runs int) (time.Duration, time.Duration, int) {
	fn(large)
	// Calibrate on a doubling count of small calls until they take at least a
	// tenth of growthMinBatch, so a clock that ticks coarsely cannot read
	// zero and set a huge count.
	n := 1
	var took time.Duration
	for {
		start := workClock()
		for range n {
			fn(small)
		}
		took = workClock() - start
		if took >= growthMinBatch/10 || n >= growthMaxReps {
			break
		}
		n *= 2
	}
	once := max(took/time.Duration(n), time.Nanosecond)
	reps := int(min(growthMaxReps, max(1, int64(growthMinBatch/once)+1)))
	batch := func(input string) time.Duration {
		start := workClock()
		for range reps {
			fn(input)
		}
		return workClock() - start
	}
	bestSmall := time.Duration(math.MaxInt64)
	bestLarge := time.Duration(math.MaxInt64)
	for range runs {
		bestSmall = min(bestSmall, batch(small))
		bestLarge = min(bestLarge, batch(large))
	}
	return max(bestSmall, time.Microsecond), max(bestLarge, time.Microsecond), reps
}

// wallClock is the fallback of workClock: time since the test binary started.
func wallClock() time.Duration { return time.Since(wallStart) }

var wallStart = time.Now()

// busySink keeps the busy goroutines' loops from being optimised away.
var busySink atomic.Int64

// TestGrowthBoundRejectsSuperlinear runs the real bound on wrappers of Text:
// one that adds linear work must pass, one that adds quadratic work must fail.
func TestGrowthBoundRejectsSuperlinear(t *testing.T) {
	if testing.Short() {
		t.Skip("timing ratios are not measured under -short")
	}
	checkGrowthWrappers(t)
}

// TestGrowthBoundUnderLoad repeats the self-test while busy goroutines, two
// per P, compete for every CPU the test may use. With wall time the busy
// goroutines take turns with the measured one and the ratio swings either
// way; thread CPU time leaves their share out. It runs on Linux only, where
// workClock is per thread.
func TestGrowthBoundUnderLoad(t *testing.T) {
	if testing.Short() {
		t.Skip("timing ratios are not measured under -short")
	}
	if workClockName != "thread CPU time" {
		t.Skipf("workClock measures %s here, which counts the busy goroutines", workClockName)
	}
	stop := make(chan struct{})
	var wg sync.WaitGroup
	for range 2 * runtime.GOMAXPROCS(0) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			n := 0
			for {
				select {
				case <-stop:
					busySink.Add(int64(n))
					return
				default:
				}
				for i := range 1 << 16 {
					n += i
				}
			}
		}()
	}
	defer func() {
		close(stop)
		wg.Wait()
	}()
	checkGrowthWrappers(t)
}

// checkGrowthWrappers asserts the growth bound accepts linear extra work and
// rejects quadratic extra work. The extra work is more calls of Text, so it
// keeps its share of the total under -race, which slows Text about 20x: the
// linear wrapper redacts one fixed prefix per step of input, the quadratic one
// a prefix that grows by a step each time.
func checkGrowthWrappers(t *testing.T) {
	t.Helper()
	smallSize, runs := 1<<10, 3
	if raceEnabled {
		smallSize, runs = 1<<9, 2
	}
	build := func(n int) string {
		return "secret:\n" + strings.Repeat("  key: value012345\n", max(1, n/19))
	}
	step := smallSize / 4
	linear := func(s string) Result {
		for i := step; i <= len(s); i += step {
			Text(s[:step])
		}
		return Text(s)
	}
	quadratic := func(s string) Result {
		for i := step; i <= len(s); i += step {
			Text(s[:i])
		}
		return Text(s)
	}
	if msg := growthViolation(linear, build, smallSize, runs); msg != "" {
		t.Errorf("linear wrapper rejected: %s", msg)
	}
	if msg := growthViolation(quadratic, build, smallSize, runs); msg == "" {
		t.Error("quadratic wrapper passed the growth bound")
	}
}
