package redact_test

import (
	"fmt"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/shadewright-labs/sablecourt/internal/redact"
	"github.com/shadewright-labs/sablecourt/internal/redact/redacttest"
)

// TestTextTimeGrowsLinearly feeds inputs built to defeat a backtracking or
// rescanning redactor and checks that four times the input costs well under
// sixteen times the time.
func TestTextTimeGrowsLinearly(t *testing.T) {
	var corpus strings.Builder
	for _, tc := range redacttest.Corpus() {
		corpus.WriteString(tc.Text)
	}
	for _, tc := range []struct{ name, unit string }{
		{"unterminated key headers", "-----BEGIN RSA PRIVATE KEY-----\nAAAA\n"},
		{"key headers with stray footers", "-----BEGIN EC PRIVATE KEY-----\n-----END RSA PRIVATE KEY-----\n"},
		{"open JSON secret fields", `{"password":"\\` + `"`},
		{"assignment labels without values", "password= api_key: secret="},
		{"bearer headers without tokens", "Authorization: Bearer \n"},
		{"token prefixes", "sk- ghp_ AIza sc_task_ op_ "},
		{"corpus", corpus.String()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ratio := growthRatio(func(n int) {
				redact.Text(strings.Repeat(tc.unit, n/len(tc.unit)+1))
			}, 32*1024, 128*1024)
			if ratio > 10 {
				t.Fatalf("4x input took %.1fx the time", ratio)
			}
		})
	}
}

func growthRatio(run func(int), small, large int) float64 {
	measure := func(size int) time.Duration {
		samples := make([]time.Duration, 7)
		for i := range samples {
			start := time.Now()
			run(size)
			samples[i] = time.Since(start)
		}
		sort.Slice(samples, func(i, j int) bool { return samples[i] < samples[j] })
		return samples[len(samples)/2]
	}
	s, l := measure(small), measure(large)
	if s <= 0 {
		s = time.Nanosecond
	}
	return float64(l) / float64(s)
}

// TestJSONTimeGrowsLinearly runs the JSON redactor on shapes that would cost
// more than linear time in a scanner that sorts keys, rescans nested strings
// or backtracks over escapes, at 256 KiB and 1 MiB (64 KiB and 256 KiB under
// the race detector).
func TestJSONTimeGrowsLinearly(t *testing.T) {
	wide := func(n int) string {
		var b strings.Builder
		b.WriteByte('{')
		for i := 0; b.Len() < n; i++ {
			if i > 0 {
				b.WriteByte(',')
			}
			fmt.Fprintf(&b, `"k%d":"v%d","password":%d`, n-i, i, i)
		}
		b.WriteByte('}')
		return b.String()
	}
	deep := func(n int) string {
		unit := `{"a":[`
		k := n / (len(unit) + 2)
		return strings.Repeat(unit, k) + strings.Repeat("]}", k)
	}
	deepLimited := func(n int) string {
		// Nesting just under the scanner's limit, repeated.
		unit := strings.Repeat("[", 500) + `{"secret":{"x":1}}` + strings.Repeat("]", 500)
		return "[" + strings.TrimSuffix(strings.Repeat(unit+",", n/len(unit)+1), ",") + "]"
	}
	escapes := func(n int) string {
		return `{"note":"` + strings.Repeat(`\\\"`, n/4) + `"}`
	}
	quotes := func(n int) string {
		return strings.Repeat(`"\`, n/2)
	}
	nested := func(n int) string {
		inner := `{"x":"{\"y\":\"{\\\"password\\\":1}\"}"}`
		return "[" + strings.TrimSuffix(strings.Repeat(inner+",", n/len(inner)+1), ",") + "]"
	}
	for _, tc := range []struct {
		name string
		gen  func(int) string
	}{
		{"wide object", wide},
		{"deep nesting past the limit", deep},
		{"deep nesting under the limit", deepLimited},
		{"escaped quotes and backslashes in a string", escapes},
		{"bare quotes and backslashes", quotes},
		{"stringified JSON", nested},
	} {
		t.Run(tc.name, func(t *testing.T) {
			size := 256 * 1024
			if raceEnabled {
				// Same 4x step, smaller sizes: the race detector slows every
				// byte down about tenfold.
				size = 64 * 1024
			}
			small, large := tc.gen(size), tc.gen(4*size)
			ratio := growthRatio(func(n int) {
				if n == len(small) {
					redact.JSON(small)
				} else {
					redact.JSON(large)
				}
			}, len(small), len(large))
			if ratio > 10 {
				t.Fatalf("4x input took %.1fx the time", ratio)
			}
		})
	}
}

// TestJSONMemoryIsOutputBound checks that the JSON redactor allocates about
// the size of its output and not a decoded copy of the document: a wide
// object of short members and a deep one, 1 MiB each.
func TestJSONMemoryIsOutputBound(t *testing.T) {
	if raceEnabled {
		t.Skip("allocation counts are not meaningful under the race detector")
	}
	var wide strings.Builder
	wide.WriteByte('{')
	for i := 0; wide.Len() < 1<<20; i++ {
		fmt.Fprintf(&wide, `"k%d":[%d,"v"],`, i, i)
	}
	wide.WriteString(`"password":1}`)
	deep := strings.Repeat(`{"a":[`, 200) + `{"password":1}` + strings.Repeat(`]}`, 200)
	deep = "[" + strings.TrimSuffix(strings.Repeat(deep+",", (1<<20)/len(deep)), ",") + "]"
	for name, doc := range map[string]string{"wide": wide.String(), "deep": deep} {
		var before, after runtime.MemStats
		runtime.GC()
		runtime.ReadMemStats(&before)
		out := redact.JSON(doc)
		runtime.ReadMemStats(&after)
		allocated := after.TotalAlloc - before.TotalAlloc
		t.Logf("%s: allocated %d bytes for %d bytes of output", name, allocated, len(out))
		if limit := uint64(4 * len(out)); allocated > limit {
			t.Errorf("%s: allocated %d bytes for %d bytes of output (limit %d)", name, allocated, len(out), limit)
		}
	}
}
