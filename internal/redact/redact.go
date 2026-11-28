package redact

import (
	"regexp"
	"sort"
	"strings"
)

type rule struct {
	category string
	re       *regexp.Regexp
	replace  string
	// pre and post name the submatch groups the replacement keeps: the
	// replaced span starts at the end of group pre (the match start when 0)
	// and ends at the start of group post (the match end when 0). marker is
	// what replace puts in that span.
	pre, post int
	marker    string
}

// Go's regexp package implements RE2, guaranteeing linear O(n) scan time with no backtracking.
const sensitiveField = `[a-z0-9_-]*(?:api[_-]?key|secret|password|passwd|access[_-]?token|operator[_-]?token)`

var rules = []rule{
	{
		category: "private_key",
		re:       regexp.MustCompile(`(?s)-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----.*?-----END [A-Z0-9 ]*PRIVATE KEY-----`),
		replace:  "[REDACTED:private_key]",
	},
	{
		category: "sablecourt_token",
		re:       regexp.MustCompile(`\bsc_task_[0-9a-fA-F]{16,}\b|\bop_[0-9a-fA-F]{64}\b`),
		replace:  "[REDACTED:sablecourt_token]",
	},
	{
		category: "anthropic_key",
		re:       regexp.MustCompile(`\bsk-ant-[A-Za-z0-9_-]{16,}\b`),
		replace:  "[REDACTED:anthropic_key]",
	},
	{
		category: "openai_key",
		re:       regexp.MustCompile(`\bsk-(?:proj-)?[A-Za-z0-9_-]{20,}\b`),
		replace:  "[REDACTED:openai_key]",
	},
	{
		category: "github_token",
		re:       regexp.MustCompile(`\b(?:ghp|gho|ghu|ghs|ghr)_[A-Za-z0-9]{20,}\b|\bgithub_pat_[A-Za-z0-9_]{20,}\b`),
		replace:  "[REDACTED:github_token]",
	},
	{
		category: "google_api_key",
		re:       regexp.MustCompile(`\bAIza[0-9A-Za-z_-]{20,}\b`),
		replace:  "[REDACTED:google_api_key]",
	},
	{
		category: "aws_access_key",
		re:       regexp.MustCompile(`\b(?:AKIA|ASIA)[0-9A-Z]{16}\b`),
		replace:  "[REDACTED:aws_access_key]",
	},
	{
		category: "bearer_header",
		re:       regexp.MustCompile(`(?i)(authorization\s*:\s*bearer\s+)[A-Za-z0-9._~+/=-]{8,}`),
		replace:  "${1}[REDACTED:bearer_token]",
		pre:      1,
		marker:   "[REDACTED:bearer_token]",
	},
	{
		category: "generic_secret_kv",
		re:       regexp.MustCompile(`(?i)("` + sensitiveField + `"\s*:\s*")(?:[^"\\]|\\.)*(")`),
		replace:  "${1}[REDACTED:secret]${2}",
		pre:      1,
		post:     2,
		marker:   "[REDACTED:secret]",
	},
	// A JSON secret value whose string does not end where JSON allows (its
	// closing quote is followed by something other than a comma, a closing
	// bracket, a line break or the end, or nothing closes it on the line) was
	// read with the wrong quote parity: a string left open before it, or a
	// backslash at its end, moved the quotes, so the next key and its value
	// are not where the rule above looks (issue 238). The key needs no opening
	// quote, since an open string may have taken it. The rest of the line is
	// hidden with the value, which fails closed, and a closing quote ends the
	// marker, so a second pass reads the same string and changes nothing. The
	// value and the tail stay within one line, so each match looks at one line
	// only and the scan stays linear.
	{
		category: "generic_secret_kv",
		re:       regexp.MustCompile(`(?im)(` + sensitiveField + `"\s*:\s*")(?:[^"\\\n]|\\.)*(?:"[ \t]*[^\s,}\]]|\\?$)[^\n]*`),
		replace:  `${1}[REDACTED:secret]"`,
		pre:      1,
		marker:   `[REDACTED:secret]"`,
	},
	{
		category: "generic_secret_kv",
		re:       regexp.MustCompile(`(?i)\b(` + sensitiveField + `)\b(\s*[:=]\s*["']?)([^\s"',;]{6,})`),
		replace:  "${1}${2}[REDACTED:secret]",
		pre:      2,
		marker:   "[REDACTED:secret]",
	},
}

type Result struct {
	Text       string
	Hits       int
	Categories []string
}

// Text redacts registered daemon secrets by exact match and applies the
// pattern rules, merging the matches of both (see Registry.RedactText).
func Text(input string) Result {
	return defaultRegistry.RedactText(input)
}

// redactPatterns runs the pattern rules alone, each on the output of the one
// before. It is the reference patternSpans reproduces (tests compare them).
func redactPatterns(input string) Result {
	if input == "" {
		return Result{}
	}
	out := input
	totalHits := 0
	seen := make(map[string]struct{})

	for _, r := range rules {
		matches := r.re.FindAllStringIndex(out, -1)
		if len(matches) == 0 {
			continue
		}
		totalHits += len(matches)
		seen[r.category] = struct{}{}
		out = r.re.ReplaceAllString(out, r.replace)
	}

	cats := make([]string, 0, len(seen))
	for c := range seen {
		cats = append(cats, c)
	}
	sort.Strings(cats)

	return Result{
		Text:       out,
		Hits:       totalHits,
		Categories: cats,
	}
}

// String is a convenience wrapper returning only the redacted string.
func String(input string) string {
	return defaultRegistry.RedactString(input)
}

// ContainsSensitive reports whether input matches any registered secret or redaction rule.
func ContainsSensitive(input string) bool {
	return defaultRegistry.ContainsSensitive(input)
}

// markedSpan is a byte span of the input and the marker that replaces it.
// exact marks a span of a registered value.
type markedSpan struct {
	start, end int
	marker     string
	exact      bool
}

// patternMatch is the input span one pattern match covered and its rule.
type patternMatch struct {
	start, end int
	category   string
}

// patternSpans returns, in input coordinates, the spans the pattern rules
// replace and every match they made. The rules run in order, each on the
// output of the one before, as redactPatterns does; each output byte keeps the
// input span it came from, so a match that touches an earlier marker covers
// that marker's whole input span. Replacing the returned spans in input gives
// redactPatterns(input).Text.
func patternSpans(input string) ([]markedSpan, []patternMatch) {
	var (
		spans   []markedSpan
		found   []patternMatch
		cur     = input
		from    []int // input offset each byte of cur starts at; nil while cur is input
		through []int // input offset each byte of cur ends at
	)
	for _, r := range rules {
		matches := r.re.FindAllStringSubmatchIndex(cur, -1)
		if len(matches) == 0 {
			continue
		}
		marker := r.marker
		if marker == "" {
			marker = r.replace
		}
		// toInput maps [s, e) of cur to input. An empty span (the value of
		// `"api_key": ""`) maps to the point before byte s of cur.
		toInput := func(s, e int) (int, int) {
			if from == nil {
				return s, e
			}
			is := len(input)
			if s < len(from) {
				is = from[s]
			}
			if e <= s {
				return is, is
			}
			return is, through[e-1]
		}
		var added []markedSpan
		for _, m := range matches {
			if m[1] > m[0] {
				ms, me := toInput(m[0], m[1])
				found = append(found, patternMatch{start: ms, end: me, category: r.category})
			}
			start, end := m[0], m[1]
			if r.pre > 0 && m[2*r.pre+1] >= 0 {
				start = m[2*r.pre+1]
			}
			if r.post > 0 && m[2*r.post] >= 0 {
				end = m[2*r.post]
			}
			if end < start {
				continue
			}
			is, ie := toInput(start, end)
			added = append(added, markedSpan{start: is, end: ie, marker: marker})
		}
		if len(added) == 0 {
			continue
		}
		spans = overlaySpans(spans, added)
		cur, from, through = applySpans(input, spans)
	}
	return spans, found
}

// overlaySpans adds the spans of one rule to the earlier ones. A new span
// covers whole earlier spans (see patternSpans), which it replaces.
func overlaySpans(old, added []markedSpan) []markedSpan {
	out := make([]markedSpan, 0, len(old)+len(added))
	j := 0
	for _, sp := range old {
		// Both lists are sorted and disjoint, so one pass finds the only added
		// span that can cover sp.
		for j < len(added) && added[j].end < sp.end {
			j++
		}
		if j < len(added) && added[j].start <= sp.start && sp.end <= added[j].end {
			continue
		}
		out = append(out, sp)
	}
	out = append(out, added...)
	sort.SliceStable(out, func(i, j int) bool { return out[i].start < out[j].start })
	return out
}

// applySpans replaces sorted, disjoint spans of input with their markers and
// maps each output byte to the input span it came from.
func applySpans(input string, spans []markedSpan) (string, []int, []int) {
	var b strings.Builder
	from := make([]int, 0, len(input))
	through := make([]int, 0, len(input))
	pos := 0
	for _, sp := range spans {
		for i := pos; i < sp.start; i++ {
			from, through = append(from, i), append(through, i+1)
		}
		b.WriteString(input[pos:sp.start])
		for range len(sp.marker) {
			from, through = append(from, sp.start), append(through, sp.end)
		}
		b.WriteString(sp.marker)
		pos = sp.end
	}
	for i := pos; i < len(input); i++ {
		from, through = append(from, i), append(through, i+1)
	}
	b.WriteString(input[pos:])
	return b.String(), from, through
}

// mergeMarkedSpans merges overlapping spans. A merged span that holds part of
// a registered value takes the exact marker; otherwise it keeps the marker of
// the pattern span it came from. Spans that only touch stay apart.
func mergeMarkedSpans(spans []markedSpan) []markedSpan {
	sort.SliceStable(spans, func(i, j int) bool { return spans[i].start < spans[j].start })
	merged := spans[:0:0]
	for _, sp := range spans {
		if n := len(merged); n > 0 && sp.start < merged[n-1].end {
			last := &merged[n-1]
			last.end = max(last.end, sp.end)
			if sp.exact && !last.exact {
				last.marker, last.exact = sp.marker, true
			}
			continue
		}
		merged = append(merged, sp)
	}
	return merged
}

// replaceMarkedSpans replaces sorted, disjoint spans of input with their
// markers and returns the output ranges of the pattern markers.
func replaceMarkedSpans(input string, spans []markedSpan) (string, []byteSpan) {
	var (
		b       strings.Builder
		pattern []byteSpan
	)
	b.Grow(len(input))
	pos := 0
	for _, sp := range spans {
		b.WriteString(input[pos:sp.start])
		if !sp.exact {
			pattern = append(pattern, byteSpan{start: b.Len(), end: b.Len() + len(sp.marker)})
		}
		b.WriteString(sp.marker)
		pos = sp.end
	}
	b.WriteString(input[pos:])
	return b.String(), pattern
}
