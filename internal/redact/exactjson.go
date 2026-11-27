package redact

import (
	"encoding/json/jsontext"
	"strings"
)

// RedactExact replaces daemon-held values found in input by literal match and
// leaves every other byte unchanged. Use it where pattern rules would rewrite
// structured text.
//
// What it matches is fixed: the registered bytes and the spellings Register and
// RegisterBytes list (quoted, JSON-escaped, and base64 and hex for binary
// values), in the raw bytes of input and in input with CR, LF and their escaped
// forms skipped. It decodes nothing else: percent encoding, mixed-case hex,
// nested base64, Unicode normalisation, number renderings and fragments split
// across fields or records are outside it.
//
// The result is scanned again, so a marker that forms a held value with its
// neighbours is replaced too; if that does not settle, the whole input becomes
// the marker. The marker is empty when a registered value occurs inside it, so
// it never discloses a value.
func (r *Registry) RedactExact(input string) string {
	out, _ := r.redactExact(input)
	return out
}

// ContainsHeld reports whether input shows a daemon-held value by the literal
// match of RedactExact.
func (r *Registry) ContainsHeld(input string) bool {
	if r == nil || input == "" {
		return false
	}
	m := r.matcher.Load()
	return m != nil && m.hit(input)
}

// HeldInJSON reports whether a declared JSON record shows a daemon-held value:
// in its raw bytes, or in any string or object name after decoding the record
// once. Every member is read, so a duplicate name cannot hide a value. A record
// that does not decode is judged on its raw bytes alone. Strings are decoded
// once only: JSON nested inside a string is matched on its decoded text, not
// decoded again.
func (r *Registry) HeldInJSON(record string) bool {
	if r == nil || record == "" {
		return false
	}
	m := r.matcher.Load()
	if m == nil {
		return false
	}
	if m.hit(record) {
		return true
	}
	dec := jsontext.NewDecoder(strings.NewReader(record), jsontext.AllowDuplicateNames(true))
	for {
		tok, err := dec.ReadToken()
		if err != nil {
			return false
		}
		if tok.Kind() == '"' && m.hit(tok.String()) {
			return true
		}
	}
}

// RedactJSONRecord applies the typed-boundary rule to a declared JSON record:
// a record that shows no held value (see HeldInJSON) comes back byte for byte,
// and one that shows any is refused as a whole. The refusal is RefusedJSON, so
// it still decodes and carries none of the record.
func (r *Registry) RedactJSONRecord(record string) (string, bool) {
	if !r.HeldInJSON(record) {
		return record, true
	}
	return r.RefusedJSON(), false
}

// RefusedJSON is the value that replaces a refused JSON record: the marker as a
// JSON string, or null when that string would show a registered value.
func (r *Registry) RefusedJSON() string {
	if m := r.marker(); m != "" && !r.ContainsHeld(`"`+m+`"`) {
		return `"` + m + `"`
	}
	return "null"
}

// Marker returns the text that replaces a held value: KnownSecretMarker, or
// the empty string when a registered value occurs inside it.
func (r *Registry) Marker() string {
	return r.marker()
}

func (r *Registry) marker() string {
	if r == nil {
		return KnownSecretMarker
	}
	if m := r.matcher.Load(); m != nil {
		return m.marker
	}
	return KnownSecretMarker
}

func (r *Registry) redactExact(input string) (string, int) {
	if r == nil || input == "" {
		return input, 0
	}
	m := r.matcher.Load()
	if m == nil {
		return input, 0
	}
	return m.redactString(input)
}

// markerFor returns KnownSecretMarker unless a pattern occurs inside it; then
// the replacement is empty. A pattern that only overlaps the marker's ends is
// caught by scanning the replaced text again (redactString, Stream.Write).
func markerFor(patterns map[string]struct{}) string {
	for p := range patterns {
		if strings.Contains(KnownSecretMarker, p) {
			return ""
		}
	}
	return KnownSecretMarker
}
