package redact

import (
	"math/rand/v2"
	"runtime"
	"strings"
	"testing"
)

// TestStringWithholdsUnfinishedKey checks the unfinished key rule of String:
// a private key BEGIN line that no END line closes is withheld through the
// end of the text. Each want is built from the same parts as the input.
func TestStringWithholdsUnfinishedKey(t *testing.T) {
	t.Parallel()
	const m = "[REDACTED:private_key]"
	begin := func(label string) string { return joinParts("-----BEGIN ", label, "-----") }
	end := func(label string) string { return joinParts("-----END ", label, "-----") }
	b, e := begin("PRIVATE KEY"), end("PRIVATE KEY")
	ghp := joinParts("gh", "p_", strings.Repeat("A1b2", 9))
	for name, tc := range map[string]struct{ in, want string }{
		"no footer":                  {in: "keep\n" + b + "\nbodyAAA\nbodyBBB", want: "keep\n" + m},
		"no footer rsa crlf":         {in: "keep\r\n" + begin("RSA PRIVATE KEY") + "\r\nbodyAAA\r\n", want: "keep\r\n" + m},
		"header only":                {in: "keep " + b, want: "keep " + m},
		"footer cut":                 {in: b + "\nbodyAAA\n" + e[:12], want: m},
		"footer cut before label":    {in: b + "\nbodyAAA\n-----END ", want: m},
		"two unfinished keys":        {in: "x\n" + b + "\nbody1\n" + b + "\nbody2\n", want: "x\n" + m},
		"whole key then unfinished":  {in: b + "\nbody1\n" + e + "\nmid\n" + b + "\nbody2", want: m + "\nmid\n" + m},
		"unfinished then whole":      {in: b + "\nbody1\n" + b + "\nbody2\n" + e + "\nafter", want: m + "\nafter"},
		"begin and end share dashes": {in: joinParts("-----BEGIN PRIVATE KEY", "-----END PRIVATE KEY-----") + "\nbody", want: m},
		"end and next begin share dashes": {
			in:   b + "\nbody1\n" + joinParts("-----END PRIVATE KEY", "-----BEGIN PRIVATE KEY-----") + "\nbody2",
			want: m,
		},
		"token before the key stays its own": {in: "tok " + ghp + " then " + b + "\nbody", want: "tok [REDACTED:github_token] then " + m},
		"public key is not a private key":    {in: begin("PUBLIC KEY") + "\nbody", want: begin("PUBLIC KEY") + "\nbody"},
		"lower case header is not matched":   {in: "-----begin private key-----\nbody", want: "-----begin private key-----\nbody"},
		"whole key unchanged":                {in: "a\n" + b + "\nbody\n" + e + "\nz", want: "a\n" + m + "\nz"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			res := Text(tc.in)
			if res.Text != tc.want {
				t.Fatalf("Text(%q) = %q, want %q", tc.in, res.Text, tc.want)
			}
			if tc.in != tc.want && (res.Hits == 0 || !ContainsSensitive(tc.in)) {
				t.Fatalf("Text(%q) hid a key but reported no hit (%+v)", tc.in, res)
			}
			if again := String(res.Text); again != res.Text {
				t.Fatalf("not idempotent: %q then %q", res.Text, again)
			}
			// Every row that withholds an unfinished key ends in the marker,
			// and no other row does.
			if got, want := UnfinishedKey(tc.in), strings.HasSuffix(tc.want, m); got != want {
				t.Fatalf("UnfinishedKey(%q) = %v, want %v", tc.in, got, want)
			}
			if got := redactPatterns(tc.in).Text; got != tc.want {
				t.Fatalf("redactPatterns(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestHead checks the order Head owns: redact the whole, cut on a rune, and
// redact again until stable.
func TestHead(t *testing.T) {
	t.Parallel()
	b, e := joinParts("-----BEGIN ", "PRIVATE KEY-----"), joinParts("-----END ", "PRIVATE KEY-----")
	ghp := joinParts("gh", "p_", strings.Repeat("G", 36))
	for name, tc := range map[string]struct {
		text  string
		limit int
		clean func(string) string
		want  string
		cut   bool
	}{
		"fits":                        {text: "ok " + ghp, limit: 100, want: "ok [REDACTED:github_token]"},
		"empty":                       {text: "", limit: 4, want: ""},
		"cut":                         {text: "abcdef", limit: 3, want: "abc", cut: true},
		"key END past the cut":        {text: "a\n" + b + "\nbodyZZZ\n" + e, limit: 12, want: "a\n[REDACTED:", cut: true},
		"unfinished key past the cut": {text: "a\n" + b + "\nbodyZZZ", limit: 40, want: "a\n[REDACTED:private_key]"},
		"multi-byte at the cut":       {text: "ééé", limit: 3, want: "é", cut: true},
		"never settles":               {text: "a", limit: 10, clean: func(s string) string { return s + "x" }, want: "", cut: true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got, cut := Head(tc.text, tc.limit, tc.clean)
			if got != tc.want || cut != tc.cut {
				t.Fatalf("Head = %q, %v; want %q, %v", got, cut, tc.want, tc.cut)
			}
			if len(got) > tc.limit || strings.Contains(got, "ZZZ") {
				t.Fatalf("Head = %q: over the limit or shows a key body", got)
			}
		})
	}
}

// unfinishedShapes are inputs that make the unfinished key scan do the most
// work per byte: many BEGIN markers, markers chained on shared dashes,
// near-miss headers, and long labels.
var unfinishedShapes = []struct {
	name  string
	build func(int) string
}{
	{"many unfinished begin lines", func(n int) string {
		return strings.Repeat(joinParts("-----BEGIN ", "PRIVATE KEY-----\nbody\n"), max(1, n/33))
	}},
	{"begin lines nested in one whole key", func(n int) string {
		return joinParts("-----BEGIN ", "PRIVATE KEY-----\n") +
			strings.Repeat(joinParts("-----BEGIN ", "EC PRIVATE KEY-----\n"), max(1, n/29)) +
			joinParts("-----END ", "PRIVATE KEY-----")
	}},
	{"begin markers chained on shared dashes", func(n int) string {
		return strings.Repeat(joinParts("-----BEGIN ", "PRIVATE KEY"), max(1, n/22)) + "-----"
	}},
	{"whole keys then one unfinished", func(n int) string {
		key := joinParts("-----BEGIN ", "PRIVATE KEY-----x-----END ", "PRIVATE KEY-----\n")
		return strings.Repeat(key, max(1, n/len(key))) + joinParts("-----BEGIN ", "PRIVATE KEY-----")
	}},
	{"near-miss headers", func(n int) string {
		return strings.Repeat("-----BEGIN AAAA ", max(1, n/16))
	}},
	{"long label with no end", func(n int) string {
		return "-----BEGIN " + strings.Repeat("A", n) + " PRIVATE KEY-----\nbody"
	}},
}

// TestUnfinishedKeyLinearGrowth times the unfinished key scan on each shape
// at two sizes with the bound of TestRedactorLinearGrowth. The matches of the
// private key pattern are found before the timing: that pattern is covered
// by TestRedactorLinearGrowth, and its step from the bit-state backtracker
// to the NFA between 4 KiB and 32 KiB would hide the scan.
func TestUnfinishedKeyLinearGrowth(t *testing.T) {
	if testing.Short() {
		t.Skip("timing ratios are not measured under -short")
	}
	smallSize, runs := 1<<12, 5
	if raceEnabled {
		smallSize, runs = 1<<10, 2
	}
	for _, tc := range unfinishedShapes {
		t.Run(tc.name, func(t *testing.T) {
			matches := map[string][][]int{}
			for _, n := range []int{smallSize, growthSizeFactor * smallSize} {
				in := tc.build(n)
				matches[in] = rules[0].re.FindAllStringSubmatchIndex(in, -1)
			}
			scan := func(s string) Result {
				return Result{Hits: len(withUnfinishedKey(s, matches[s]))}
			}
			if msg := growthViolation(scan, tc.build, smallSize, runs); msg != "" {
				t.Fatal(msg)
			}
		})
	}
}

// TestUnfinishedKeyBoundedMemory checks that the scan allocates nothing of
// its own and that the bytes Text allocates grow no faster than its input.
func TestUnfinishedKeyBoundedMemory(t *testing.T) {
	for _, tc := range unfinishedShapes {
		t.Run(tc.name, func(t *testing.T) {
			in := tc.build(1 << 12)
			matches := rules[0].re.FindAllStringSubmatchIndex(in, -1)
			if allocs := testing.AllocsPerRun(5, func() { unfinishedKeyStart(in, matches) }); allocs != 0 {
				t.Fatalf("unfinishedKeyStart allocated %.0f times, want 0", allocs)
			}
			if allocs := testing.AllocsPerRun(5, func() { withUnfinishedKey(in, matches) }); allocs > 3 {
				t.Fatalf("withUnfinishedKey allocated %.0f times, want at most 3", allocs)
			}
			small, large := allocBytes(tc.build(1<<12)), allocBytes(tc.build(growthSizeFactor<<12))
			if ratio := float64(large) / float64(max(small, 1)); ratio > growthMaxRatio {
				t.Fatalf("allocated bytes grew %.1fx for %dx input (small=%d large=%d)", ratio, growthSizeFactor, small, large)
			}
		})
	}
}

// allocBytes returns the bytes one Text call on in allocates.
func allocBytes(in string) uint64 {
	Text(in)
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	Text(in)
	runtime.ReadMemStats(&after)
	return after.TotalAlloc - before.TotalAlloc
}

// TestNextKeyBeginMatchesPattern checks the hand scanner against the
// privateKeyBegin pattern on generated text: random joins of marker parts,
// dashes, labels, and plain bytes, and every start offset of each.
func TestNextKeyBeginMatchesPattern(t *testing.T) {
	t.Parallel()
	parts := []string{"-", "--", "-----", "BEGIN ", "BEGIN", "END ", " ", "PRIVATE KEY", "PRIVATE", " KEY", "RSA ", "EC ", "A", "9", "x", "\n", "é", "PUBLIC KEY"}
	rng := rand.New(rand.NewPCG(262, 1))
	for range 3000 {
		var b strings.Builder
		for range rng.IntN(24) {
			b.WriteString(parts[rng.IntN(len(parts))])
		}
		s := b.String()
		for from := 0; from <= len(s); from++ {
			want := []int{-1, -1}
			if loc := privateKeyBegin.FindStringIndex(s[from:]); loc != nil {
				want = []int{from + loc[0], from + loc[1]}
			}
			if gs, ge := nextKeyBegin(s, from); gs != want[0] || ge != want[1] {
				t.Fatalf("nextKeyBegin(%q, %d) = %d, %d; want %v", s, from, gs, ge, want)
			}
		}
	}
}
