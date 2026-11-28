package model

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"regexp"

	"github.com/shadewright-labs/sablecourt/internal/redact"
)

var (
	errRequestBodyTooLarge = errors.New("request body too large")
	errRequestBodyNotJSON  = errors.New("request body must be one JSON value")
)

const (
	// maxRelayedResponseBytes is how much of one upstream response body the
	// gate reads and relays. Past it the relay stops and nothing more is sent.
	maxRelayedResponseBytes = 64 << 20
	// maxResponsePendingBytes is how much of a response the gate holds while
	// it waits for the end of a line (or of a private key block) before it
	// applies the pattern rules. Past it the relay stops and the held bytes
	// are dropped.
	maxResponsePendingBytes = maxCapturedResponseBytes
)

// The bounds in force; tests lower them.
var (
	relayResponseLimit   = maxRelayedResponseBytes
	pendingResponseLimit = maxResponsePendingBytes
)

// readRequestBody reads at most maxRequestBodyBytes. A longer body is refused
// instead of being cut, so the upstream never gets a truncated request.
func readRequestBody(r io.Reader) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(r, maxRequestBodyBytes+1))
	if err != nil {
		return nil, err
	}
	if len(body) > maxRequestBodyBytes {
		return nil, errRequestBodyTooLarge
	}
	return body, nil
}

// redactRequestBody returns the body the gate sends upstream. Every route the
// gate serves takes a JSON body, so the body is decoded and each string,
// member name included, goes through redact.String: daemon-held values by
// exact match and the pattern rules. Decoding first means a secret spelled
// with JSON escapes is seen as the provider would decode it. A string member
// whose name the key-value rule treats as sensitive has its whole value
// replaced, as that rule does on raw text. The result is encoded again, so
// what goes upstream is exactly what was checked; member order and spacing
// may change, numbers are kept as written. An empty body is passed as is, and
// a body that is not one JSON value is refused, because it cannot be checked.
func redactRequestBody(body []byte) ([]byte, error) {
	if len(bytes.TrimSpace(body)) == 0 {
		return body, nil
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, errRequestBodyNotJSON
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errRequestBodyNotJSON
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(redactJSONValue(v)); err != nil {
		return nil, errRequestBodyNotJSON
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}

func redactJSONValue(v any) any {
	switch x := v.(type) {
	case string:
		return redact.String(x)
	case []any:
		for i := range x {
			x[i] = redactJSONValue(x[i])
		}
		return x
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, val := range x {
			name := redact.String(k)
			if _, isString := val.(string); isString {
				if marker, ok := sensitiveMember(name); ok {
					out[name] = marker
					continue
				}
			}
			out[name] = redactJSONValue(val)
		}
		return out
	default:
		// json.Number, bool and nil carry no text a rule matches.
		return v
	}
}

// sensitiveMember reports whether the pattern rules hide the string value of
// a JSON member with this name, and returns what they put in its place. It
// asks redact.String about a probe member, so the decision stays with the
// rules in internal/redact.
func sensitiveMember(name string) (string, bool) {
	key, err := json.Marshal(name)
	if err != nil {
		return "", false
	}
	probe := `{` + string(key) + `:"x"}`
	out := redact.String(probe)
	if out == probe {
		return "", false
	}
	var m map[string]string
	if err := json.Unmarshal([]byte(out), &m); err == nil && len(m) == 1 {
		for _, marker := range m {
			if marker != "x" {
				return marker, true
			}
		}
	}
	return "[REDACTED:secret]", true
}

var (
	responseKeyStart = regexp.MustCompile(`-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----`)
	responseKeyEnd   = regexp.MustCompile(`-----END [A-Z0-9 ]*PRIVATE KEY-----`)
)

// unfinishedKeyAt returns the offset of a private key header with no footer
// after it, or -1.
func unfinishedKeyAt(text string) int {
	starts := responseKeyStart.FindAllStringIndex(text, -1)
	if len(starts) == 0 {
		return -1
	}
	last := starts[len(starts)-1]
	if responseKeyEnd.MatchString(text[last[1]:]) {
		return -1
	}
	return last[0]
}

// responseRedactor filters an upstream response body for the workload. Each
// chunk first passes the exact matcher stream, which may cut anywhere it is
// safe for held values. The pattern rules need whole tokens, so the output of
// that stream is held until a line ends and the complete lines go through
// redact.String. Relayed bodies are JSON or server-sent events: a JSON string
// never holds a raw line break, so no token the rules match crosses a line.
// A private key header with no footer yet keeps its lines held until the
// footer comes. More than maxPending held bytes stop the relay: the held
// bytes are dropped and overflow is set.
type responseRedactor struct {
	known      *redact.Stream
	pending    []byte
	maxPending int
	overflow   bool
	stopped    bool
}

func newResponseRedactor() *responseRedactor {
	return &responseRedactor{known: redact.NewStream(), maxPending: pendingResponseLimit}
}

// Write returns the bytes safe to send for chunk. Pass eof=true once, at the
// end of the body, to flush what is held.
func (r *responseRedactor) Write(chunk []byte, eof bool) []byte {
	if r.overflow || r.stopped {
		return nil
	}
	r.pending = append(r.pending, r.known.Write(chunk, eof)...)
	if eof {
		text := redact.String(string(r.pending))
		r.pending = nil
		if at := unfinishedKeyAt(text); at >= 0 {
			text = text[:at] + "[REDACTED:private_key]"
		}
		return []byte(text)
	}
	var out []byte
	if nl := bytes.LastIndexByte(r.pending, '\n'); nl >= 0 {
		block := redact.String(string(r.pending[:nl+1]))
		if unfinishedKeyAt(block) < 0 {
			out = []byte(block)
			r.pending = r.pending[:copy(r.pending, r.pending[nl+1:])]
		}
	}
	if len(r.pending) > r.maxPending {
		r.overflow = true
		r.pending = nil
	}
	return out
}

// Stop drops what is held without sending it.
func (r *responseRedactor) Stop() {
	r.pending = nil
	r.stopped = true
}
