package mcpgate

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf16"
	"unicode/utf8"
)

// errInvalidPayload marks input the gate refuses before policy effects run: a
// payload that does not decode to exactly one typed value, or a value that
// fails the action's own checks. On an allowed request it counts on the
// circuit breaker, as the executor's own checks did before.
var errInvalidPayload = errors.New("invalid action payload")

func invalidPayload(format string, args ...any) error {
	return fmt.Errorf("%w: %s", errInvalidPayload, fmt.Sprintf(format, args...))
}

// jsonFieldNames lists the JSON names of the exported fields of struct type t.
func jsonFieldNames(t reflect.Type) []string {
	var names []string
	for i := range t.NumField() {
		f := t.Field(i)
		if !f.IsExported() {
			continue
		}
		name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		if name == "-" {
			continue
		}
		if name == "" {
			name = f.Name
		}
		names = append(names, name)
	}
	return names
}

// decodeTyped reads raw as one JSON object into a T with none of the leniency
// of encoding/json: each key must be exactly one of T's JSON field names
// (no case-insensitive matching), appear once, and carry a non-null value, and
// nothing may follow the object. Only then is the object decoded, so the value
// returned is the one meaning every reader of raw would agree on.
func decodeTyped[T any](raw json.RawMessage) (T, error) {
	var out T
	if err := checkStringsKept(raw); err != nil {
		return out, err
	}
	allowed := map[string]bool{}
	for _, n := range jsonFieldNames(reflect.TypeOf(out)) {
		allowed[n] = true
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	tok, err := dec.Token()
	if err != nil {
		return out, invalidPayload("payload must be a JSON object")
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return out, invalidPayload("payload must be a JSON object")
	}
	seen := map[string]bool{}
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return out, invalidPayload("malformed object")
		}
		key, _ := tok.(string)
		if seen[key] {
			return out, invalidPayload("duplicate key %q", key)
		}
		seen[key] = true
		if !allowed[key] {
			for name := range allowed {
				if strings.EqualFold(name, key) {
					return out, invalidPayload("key %q differs only in case from field %q", key, name)
				}
			}
			return out, invalidPayload("unknown field %q", key)
		}
		var value json.RawMessage
		if err := dec.Decode(&value); err != nil {
			return out, invalidPayload("malformed value for %q", key)
		}
		if bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return out, invalidPayload("null value for %q", key)
		}
	}
	if _, err := dec.Token(); err != nil {
		return out, invalidPayload("malformed object")
	}
	if _, err := dec.Token(); err != io.EOF {
		return out, invalidPayload("data after the payload object")
	}
	strict := json.NewDecoder(bytes.NewReader(raw))
	strict.DisallowUnknownFields()
	if err := strict.Decode(&out); err != nil {
		return out, invalidPayload("%v", err)
	}
	return out, nil
}

// decodeGeneric reads raw as one JSON object with no duplicate keys and no
// two keys in one object that differ only in case, at any depth. Numbers stay
// as written. It serves actions registered without a typed input.
func decodeGeneric(raw json.RawMessage) (map[string]any, error) {
	if err := checkStringsKept(raw); err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	v, err := readStrictValue(dec)
	if err != nil {
		return nil, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, invalidPayload("data after the payload object")
	}
	obj, ok := v.(map[string]any)
	if !ok {
		return nil, invalidPayload("payload must be a JSON object")
	}
	return obj, nil
}

func readStrictValue(dec *json.Decoder) (any, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, invalidPayload("malformed JSON")
	}
	switch t := tok.(type) {
	case json.Delim:
		switch t {
		case '{':
			obj := map[string]any{}
			folded := map[string]string{}
			for dec.More() {
				kt, err := dec.Token()
				if err != nil {
					return nil, invalidPayload("malformed object")
				}
				key, _ := kt.(string)
				fk := foldKey(key)
				if prev, ok := folded[fk]; ok {
					if prev == key {
						return nil, invalidPayload("duplicate key %q", key)
					}
					return nil, invalidPayload("keys %q and %q differ only in case", prev, key)
				}
				folded[fk] = key
				val, err := readStrictValue(dec)
				if err != nil {
					return nil, err
				}
				obj[key] = val
			}
			if _, err := dec.Token(); err != nil {
				return nil, invalidPayload("malformed object")
			}
			return obj, nil
		case '[':
			arr := []any{}
			for dec.More() {
				val, err := readStrictValue(dec)
				if err != nil {
					return nil, err
				}
				arr = append(arr, val)
			}
			if _, err := dec.Token(); err != nil {
				return nil, invalidPayload("malformed array")
			}
			return arr, nil
		}
		return nil, invalidPayload("malformed JSON")
	default:
		return t, nil
	}
}

// checkStringsKept refuses JSON text that encoding/json would change while
// decoding: bytes that are not UTF-8, and a \u escape of an unpaired UTF-16
// surrogate. Both decode to U+FFFD, so the decoded value, its digest and what
// the action runs with would differ from what was sent, and would equal a
// payload that sent U+FFFD itself. A literal U+FFFD stays valid.
func checkStringsKept(raw []byte) error {
	if !utf8.Valid(raw) {
		return invalidPayload("payload is not valid UTF-8")
	}
	inString := false
	for i := 0; i < len(raw); i++ {
		c := raw[i]
		if !inString {
			if c == '"' {
				inString = true
			}
			continue
		}
		switch c {
		case '"':
			inString = false
		case '\\':
			if i+1 >= len(raw) {
				return nil // malformed; the decoder refuses it
			}
			if raw[i+1] != 'u' {
				i++
				continue
			}
			r, ok := hexRune(raw, i+2)
			if !ok {
				return nil // malformed; the decoder refuses it
			}
			i += 5
			switch {
			case utf16.IsSurrogate(r) && r < 0xdc00:
				if i+6 >= len(raw) || raw[i+1] != '\\' || raw[i+2] != 'u' {
					return invalidPayload("unpaired surrogate escape in a string")
				}
				low, ok := hexRune(raw, i+3)
				if !ok || utf16.DecodeRune(r, low) == unicode.ReplacementChar {
					return invalidPayload("unpaired surrogate escape in a string")
				}
				i += 6
			case utf16.IsSurrogate(r):
				return invalidPayload("unpaired surrogate escape in a string")
			}
		}
	}
	return nil
}

// hexRune reads the four hex digits at raw[at:at+4].
func hexRune(raw []byte, at int) (rune, bool) {
	if at+4 > len(raw) {
		return 0, false
	}
	n, err := strconv.ParseUint(string(raw[at:at+4]), 16, 32)
	if err != nil {
		return 0, false
	}
	return rune(n), true
}

// foldKey maps s to one key shared by every string strings.EqualFold treats
// as equal to it: each rune becomes the smallest rune of its simple case
// folding orbit (so "Σ", "σ" and "ς" share a key, as do "K", "k" and the
// Kelvin sign).
func foldKey(s string) string {
	var b strings.Builder
	for _, r := range s {
		least := r
		for f := unicode.SimpleFold(r); f != r; f = unicode.SimpleFold(f) {
			if f < least {
				least = f
			}
		}
		b.WriteRune(least)
	}
	return b.String()
}
