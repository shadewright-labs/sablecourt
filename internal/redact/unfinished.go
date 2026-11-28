package redact

import "strings"

// markerDashes is the run of dashes that closes a PEM marker. A BEGIN
// marker can start on the closing dashes of the marker before it, so a scan
// for the next one resumes that many bytes before the end of the last.
const markerDashes = len("-----")

// withUnfinishedKey extends the matches of the private key rule on s with
// the unfinished key: everything from the first BEGIN marker that no match
// covers whole through the end of s. Such a BEGIN has no END marker after
// it (the rule takes the leftmost BEGIN that has one), or shares its dashes
// with the only one, so withholding the rest fails closed: a cut that later
// drops the BEGIN line cannot show the key body. A match that the unfinished
// span starts inside is folded into it. matches are sorted and disjoint, as
// FindAllStringSubmatchIndex returns them, and hold no submatch groups.
//
// The scan is linear in s and allocates nothing (see nextKeyBegin); only a
// found unfinished key allocates the returned slice.
func withUnfinishedKey(s string, matches [][]int) [][]int {
	start := unfinishedKeyStart(s, matches)
	if start < 0 {
		return matches
	}
	kept := make([][]int, 0, len(matches)+1)
	for _, m := range matches {
		if m[1] <= start {
			kept = append(kept, m)
			continue
		}
		if m[0] < start {
			start = m[0]
		}
		break
	}
	return append(kept, []int{start, len(s)})
}

// unfinishedKeyStart returns the offset of the first BEGIN marker of s that
// lies outside every match, or -1.
func unfinishedKeyStart(s string, matches [][]int) int {
	pos, mi := 0, 0
	for {
		begin, end := nextKeyBegin(s, pos)
		if begin < 0 {
			return -1
		}
		for mi < len(matches) && matches[mi][1] <= begin {
			mi++
		}
		if mi == len(matches) || begin < matches[mi][0] || end > matches[mi][1] {
			return begin
		}
		pos = end - markerDashes
	}
}

const keyBeginPrefix = "-----BEGIN "

// nextKeyBegin returns the span of the first private key BEGIN marker of s
// at or after from, or -1, -1. It matches what privateKeyBegin matches
// without the regexp package: a search per call there costs time in the
// length of the rest of s, which a loop over many markers turns quadratic.
// The label is the longest run of [A-Z0-9 ] after the prefix; the pattern
// can only match where that run ends in "PRIVATE KEY" and five dashes
// follow, since a dash ends the run. A search resumes after a label, which
// holds no dash, so each byte of s is read a bounded number of times.
func nextKeyBegin(s string, from int) (int, int) {
	for from < len(s) {
		i := strings.Index(s[from:], keyBeginPrefix)
		if i < 0 {
			return -1, -1
		}
		start := from + i
		label := start + len(keyBeginPrefix)
		n := label
		for n < len(s) && (s[n] == ' ' || s[n] >= 'A' && s[n] <= 'Z' || s[n] >= '0' && s[n] <= '9') {
			n++
		}
		if strings.HasSuffix(s[label:n], "PRIVATE KEY") && strings.HasPrefix(s[n:], "-----") {
			return start, n + markerDashes
		}
		from = n
	}
	return -1, -1
}

// UnfinishedKey reports whether s holds a private key BEGIN marker that no
// END marker closes: the text String withholds from that marker through the
// end. A caller that must refuse such text instead of sending what is left
// of it checks the text with UnfinishedKey before redacting it.
func UnfinishedKey(s string) bool {
	for _, r := range rules {
		if r.unfinished {
			return unfinishedKeyStart(s, r.re.FindAllStringSubmatchIndex(s, -1)) >= 0
		}
	}
	return false
}
