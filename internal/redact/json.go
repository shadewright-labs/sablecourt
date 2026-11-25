package redact

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"regexp"
	"strings"
)

var sensitiveKey = regexp.MustCompile(`(?i)^` + sensitiveField + `$`)

// JSON redacts a JSON document value by value, so the result is still valid
// JSON: each string is passed through Text, and a string or number held under
// a secret-named key is replaced whole. Input that is not a single JSON value
// is redacted as plain text. Input without a hit is returned unchanged.
func JSON(input string) string {
	dec := json.NewDecoder(strings.NewReader(input))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return String(input)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return String(input)
	}
	out, changed := redactValue(v, false)
	if !changed {
		return input
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(out); err != nil {
		// Every value came from the decoder, so this is not expected; withhold
		// rather than return the unredacted input.
		return `"[REDACTED:unencodable]"`
	}
	return strings.TrimSuffix(buf.String(), "\n")
}

// secretKey reports whether a JSON key names a credential. An Authorization
// field holds a credential whatever its scheme.
func secretKey(k string) bool {
	return sensitiveKey.MatchString(k) || strings.EqualFold(k, "authorization")
}

func redactValue(v any, underSecret bool) (any, bool) {
	switch val := v.(type) {
	case string:
		if underSecret && val != "" {
			return "[REDACTED:secret]", true
		}
		clean := String(val)
		return clean, clean != val
	case json.Number:
		if underSecret {
			return "[REDACTED:secret]", true
		}
	case []any:
		changed := false
		for i, item := range val {
			var c bool
			val[i], c = redactValue(item, underSecret)
			changed = changed || c
		}
		return val, changed
	case map[string]any:
		out := make(map[string]any, len(val))
		changed := false
		for k, item := range val {
			clean, c := redactValue(item, secretKey(k))
			key := String(k)
			out[key] = clean
			changed = changed || c || key != k
		}
		return out, changed
	}
	return v, false
}
