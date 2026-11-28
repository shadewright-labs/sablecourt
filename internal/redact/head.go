package redact

import "unicode/utf8"

// Head returns the start of text, redacted and at most limit bytes long, and
// reports whether anything was cut. It is the counterpart of Tail for output
// capped at its start, with the order fixed the same way:
//
//  1. Redact the whole text, so a secret that spans the cut is replaced whole
//     before anything is cut. A private key with no END line is withheld
//     from its BEGIN line through the end (String does this), so no cut can
//     keep a BEGIN line and a part of the body.
//  2. Cut the redacted text to its first limit bytes, back to the start of
//     a rune.
//  3. Redact the cut text again until it stops changing (a cut can remove a
//     word character that hid a token from its rule), and go back to step 2
//     when that grows it past limit.
//
// clean is the redaction pass, String when nil; it must be deterministic.
// The result is empty, with cut set, when the passes do not settle.
func Head(text string, limit int, clean func(string) string) (string, bool) {
	if clean == nil {
		clean = String
	}
	limit = max(limit, 0)
	out := clean(text)
	cut := false
	for range tailPasses {
		if len(out) > limit {
			n := limit
			for n > 0 && !utf8.RuneStart(out[n]) {
				n--
			}
			out, cut = out[:n], true
		}
		next := clean(out)
		if next == out {
			return out, cut
		}
		out = next
	}
	return "", true
}
