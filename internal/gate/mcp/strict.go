package mcpgate

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
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
				if prev, ok := folded[strings.ToLower(key)]; ok {
					if prev == key {
						return nil, invalidPayload("duplicate key %q", key)
					}
					return nil, invalidPayload("keys %q and %q differ only in case", prev, key)
				}
				folded[strings.ToLower(key)] = key
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
