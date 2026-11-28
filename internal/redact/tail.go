package redact

import (
	"regexp"
	"strings"
)

// tailPasses bounds the redact and cut passes of Tail. Redaction settles in
// one or two passes; text that has not settled by then is dropped.
const tailPasses = 8

var (
	privateKeyBegin = regexp.MustCompile(`-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----`)
	privateKeyEnd   = regexp.MustCompile(`-----END [A-Z0-9 ]*PRIVATE KEY-----`)
)

// Tail returns the end of text, redacted and at most limit bytes long, and
// reports whether anything was cut. Every capped output uses it, so the
// order is fixed in one place:
//
//  1. Redact the whole text, so a secret that spans a later cut is replaced
//     whole before anything is cut. Every redaction pass, whatever clean
//     is, also replaces everything from the first private key BEGIN line
//     that has no END line after it through the end with the private key
//     marker, so a later cut cannot drop that BEGIN line and show the key
//     body.
//  2. Cut the redacted text to its last limit bytes.
//  3. After any cut, drop the first line of what is left (a cut can fall
//     inside a secret, whose rest no rule would match) and everything
//     through a private key END line that comes before any BEGIN line.
//  4. Redact the cut text again until it stops changing (a cut can remove a
//     word character that hid a token from its rule), and go back to step 2
//     when that grows it past limit.
//
// text[:from] is context only: it is redacted together with the rest, so a
// secret that starts in it and ends after from is replaced whole, but no
// byte of it is returned. A from above 0 counts as a cut. clean is the
// redaction pass, String when nil; it must be deterministic.
//
// The result is empty, with cut set, when no whole line is left or the
// passes do not settle.
func Tail(text string, from, limit int, clean func(string) string) (string, bool) {
	if clean == nil {
		clean = String
	}
	pass := clean
	clean = func(s string) string { return hideUnfinishedKey(pass(hideUnfinishedKey(s))) }
	from = min(max(from, 0), len(text))
	limit = max(limit, 0)
	out := clean(text)
	cut := from > 0
	drop := cut
	if cut {
		// Keep only the part of the redacted whole that the redacted kept
		// part ends with: text past the last place where they differ comes
		// from text[from:] and was redacted with its context.
		out = out[len(out)-commonSuffix(out, clean(text[from:])):]
	}
	for range tailPasses {
		if len(out) > limit {
			out, cut, drop = out[len(out)-limit:], true, true
		}
		if drop {
			drop = false
			if out = dropPartialStart(out); out == "" {
				return "", true
			}
		}
		next := clean(out)
		if next == out {
			return out, cut
		}
		out = next
	}
	return "", true
}

// unfinishedKeyMarker replaces a private key that has no END line. It is
// the marker of the private key rule, so a finished and an unfinished key
// look the same once redacted.
const unfinishedKeyMarker = "[REDACTED:private_key]"

// hideUnfinishedKey replaces everything from the first private key BEGIN
// line that has no END line after it through the end of s with the private
// key marker. No rule matches such a key, so without this its body would be
// shown as soon as a cut dropped the BEGIN line. The diagnostics after it
// are lost with it.
func hideUnfinishedKey(s string) string {
	rest := 0
	if ends := privateKeyEnd.FindAllStringIndex(s, -1); len(ends) > 0 {
		rest = ends[len(ends)-1][1]
	}
	start := privateKeyBegin.FindStringIndex(s[rest:])
	if start == nil {
		return s
	}
	return s[:rest+start[0]] + unfinishedKeyMarker
}

// dropPartialStart drops the first line of s and then everything through
// the last private key END line before the first BEGIN line, since a tail
// that holds such an END line starts inside a key. It returns "" when no
// whole line is left. A newline byte never occurs inside a multi-byte
// UTF-8 sequence, so the result starts on a rune when s is valid UTF-8.
func dropPartialStart(s string) string {
	nl := strings.IndexByte(s, '\n')
	if nl < 0 {
		return ""
	}
	s = s[nl+1:]
	head := s
	if start := privateKeyBegin.FindStringIndex(s); start != nil {
		head = s[:start[0]]
	}
	if ends := privateKeyEnd.FindAllStringIndex(head, -1); len(ends) > 0 {
		s = s[ends[len(ends)-1][1]:]
	}
	return s
}

// commonSuffix returns the length of the longest common suffix of a and b.
func commonSuffix(a, b string) int {
	n := 0
	for n < len(a) && n < len(b) && a[len(a)-1-n] == b[len(b)-1-n] {
		n++
	}
	return n
}
