package store

import (
	"bytes"
	"encoding/json"
	"strings"

	"github.com/shadewright-labs/sablecourt/internal/redact"
)

// sensitiveFieldMarker replaces the whole string value of a JSON field whose
// name the redaction rules treat as sensitive, such as "password".
const sensitiveFieldMarker = "[REDACTED:secret]"

// RedactTaskInput returns task input, or the content of a task message, as
// it may be stored and later delivered to a harness (issue 261). Both stores
// call it before they write a task row or a task message, so every reader and
// every launch path reads the redacted copy.
//
// First the typed-boundary rule of the ADR-0015 amendment: input that shows
// a daemon-held value (redact.Registry.HeldInJSON) is refused whole and
// becomes redact.Registry.RefusedJSON, with none of its payload. Then the
// pattern rules, which are best effort, replace other secret shapes:
//
// Text that is one JSON value is redacted string by string after decoding,
// so \u escapes cannot hide a secret from the rules and the result stays
// valid JSON. Every string token is checked, duplicate object names
// included. Input with nothing to replace is returned byte for byte, so
// idempotent replay and prompt digests do not change. Input that changed is
// encoded again from the decoded value: a duplicate name keeps only its last
// value, as the Go decoder every reader uses would. Text that is not one JSON
// value is redacted as plain text. If the encoding fails, the result is the
// refusal marker as a JSON string, never the input.
func RedactTaskInput(text string) string {
	if text == "" {
		return text
	}
	if refused, ok := redact.DefaultRegistry().RedactJSONRecord(text); !ok {
		return refused
	}
	if !json.Valid([]byte(text)) {
		return redact.String(text)
	}
	if !jsonNeedsRedaction(text) {
		return text
	}
	dec := json.NewDecoder(strings.NewReader(text))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return redact.DefaultRegistry().RefusedJSON()
	}
	var out bytes.Buffer
	enc := json.NewEncoder(&out)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(redactJSONValue(v)); err != nil {
		return redact.DefaultRegistry().RefusedJSON()
	}
	return strings.TrimSuffix(out.String(), "\n")
}

// jsonNeedsRedaction reports whether any string token of a valid JSON text,
// object names and duplicates included, holds something the rules replace,
// or names a sensitive field.
func jsonNeedsRedaction(text string) bool {
	dec := json.NewDecoder(strings.NewReader(text))
	dec.UseNumber()
	for {
		tok, err := dec.Token()
		if err != nil {
			// The text is valid JSON, so the only error is the end.
			return false
		}
		s, ok := tok.(string)
		if !ok {
			continue
		}
		if redact.String(s) != s || sensitiveJSONField(s) {
			return true
		}
	}
}

func redactJSONValue(v any) any {
	switch t := v.(type) {
	case string:
		return redact.String(t)
	case []any:
		for i := range t {
			t[i] = redactJSONValue(t[i])
		}
		return t
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			if _, ok := val.(string); ok && sensitiveJSONField(k) {
				val = sensitiveFieldMarker
			}
			out[redact.String(k)] = redactJSONValue(val)
		}
		return out
	default:
		return v
	}
}

// sensitiveJSONField reports whether the pattern rule for a sensitive JSON
// field matches this name. Redacting names and values apart would miss such
// a field, so its string value is replaced whole.
func sensitiveJSONField(key string) bool {
	frag, err := json.Marshal(map[string]string{key: "placeholder"})
	return err == nil && redact.String(string(frag)) != string(frag)
}
