package model

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
)

// auditReasons returns the detail of every audit row, newest first.
func auditReasons(t *testing.T, gate *Gate) string {
	t.Helper()
	recs, err := gate.store.ListAudit(context.Background(), 50)
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	for _, r := range recs {
		b.WriteString(r.Outcome + " " + r.DetailJSON + "\n")
	}
	return b.String()
}

// Issue 265: a request body past the read limit was cut and the cut body sent
// upstream; a body the gate cannot decode could not be checked for secrets.
// Both are refused before any upstream call.
func TestModelGateRefusesUncheckableRequestBodies(t *testing.T) {
	for _, tc := range []struct {
		name   string
		body   string
		status int
		reason string
	}{
		{"over the limit", `{"model":"m","pad":"` + strings.Repeat("a", maxRequestBodyBytes) + `"}`, http.StatusRequestEntityTooLarge, "request_body_too_large"},
		{"not JSON", "model=m&password=" + strings.Repeat("p", 12), http.StatusBadRequest, "request_body_not_json"},
		{"trailing text", `{"model":"m"} ` + "gh" + "p_" + strings.Repeat("x", 36), http.StatusBadRequest, "request_body_not_json"},
		{"two values", `{"model":"m"}{"model":"n"}`, http.StatusBadRequest, "request_body_not_json"},
		{"cut JSON", `{"model":"m","messages":[`, http.StatusBadRequest, "request_body_not_json"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			gate, _, tok := bodyGate(t, func(r *http.Request) (*http.Response, error) {
				calls.Add(1)
				return jsonOK(http.StatusOK), nil
			})
			rec := callGate(gate, tok, "/openai/v1/chat/completions", tc.body)
			if rec.Code != tc.status {
				t.Fatalf("status %d, want %d: %s", rec.Code, tc.status, rec.Body.String())
			}
			if calls.Load() != 0 {
				t.Fatalf("upstream called %d times", calls.Load())
			}
			if got := auditReasons(t, gate); !strings.Contains(got, `"reason":"`+tc.reason+`"`) {
				t.Fatalf("audit rows %q lack %s", got, tc.reason)
			}
		})
	}
	t.Run("empty body is forwarded", func(t *testing.T) {
		var calls atomic.Int32
		gate, _, tok := bodyGate(t, func(r *http.Request) (*http.Response, error) {
			calls.Add(1)
			return jsonOK(http.StatusOK), nil
		})
		if rec := callGate(gate, tok, "/openai/v1/chat/completions", ""); rec.Code != http.StatusOK || calls.Load() != 1 {
			t.Fatalf("status %d, calls %d", rec.Code, calls.Load())
		}
	})
}

// setVar lowers a package bound for one test.
func setVar(t *testing.T, p *int, v int) {
	t.Helper()
	old := *p
	*p = v
	t.Cleanup(func() { *p = old })
}

func streamGate(t *testing.T, body string, chunk int) (*Gate, string) {
	t.Helper()
	gate, _, tok := bodyGate(t, func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"text/event-stream"}},
			Body: io.NopCloser(&trickleReader{s: body, n: chunk})}, nil
	})
	return gate, tok
}

// Issue 265: the response loop read the upstream body without a bound. Past
// the relay limit it stops, sends nothing held, and audits why.
func TestModelGateResponseBounds(t *testing.T) {
	ghp := "gh" + "p_" + strings.Repeat("Q7", 18)

	t.Run("relay limit", func(t *testing.T) {
		setVar(t, &relayResponseLimit, 4096)
		// The token starts just before the limit, so a body cut at the
		// limit would show its first bytes.
		line := "data: {\"t\":\"keepme\"}\n\n"
		body := strings.Repeat(line, 4090/len(line)) + "data: {\"t\":\"" + ghp + "\"}\n\n" + strings.Repeat(line, 400) + "data: after_the_limit\n\n"
		gate, tok := streamGate(t, body, 64)
		rec := callGate(gate, tok, "/openai/v1/chat/completions", `{"model":"m","stream":true}`)
		out := rec.Body.String()
		if len(out) > 4096 || strings.Contains(out, "after_the_limit") || strings.Contains(out, "gh"+"p_") {
			t.Fatalf("relay passed its limit (%d bytes): %q", len(out), out[max(0, len(out)-200):])
		}
		if !strings.Contains(out, "keepme") {
			t.Fatalf("nothing relayed before the limit")
		}
		if got := auditReasons(t, gate); !strings.Contains(got, "upstream_response_too_large") {
			t.Fatalf("audit rows lack the stop: %q", got)
		}
	})

	t.Run("held line limit", func(t *testing.T) {
		setVar(t, &pendingResponseLimit, 1024)
		body := `{"model":"m","text":"` + strings.Repeat("k", 1500) + ghp + strings.Repeat("k", 3000) + `"}`
		gate, _, tok := bodyGate(t, func(r *http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}},
				Body: io.NopCloser(&trickleReader{s: body, n: 256})}, nil
		})
		rec := callGate(gate, tok, "/openai/v1/chat/completions", `{"model":"m"}`)
		if out := rec.Body.String(); strings.Contains(out, "gh"+"p_") || len(out) > 0 {
			t.Fatalf("held bytes were sent: %d bytes", len(out))
		}
		if got := auditReasons(t, gate); !strings.Contains(got, "upstream_response_line_too_long") {
			t.Fatalf("audit rows lack the stop: %q", got)
		}
	})

	pemBody := strings.Repeat("Mk9", 20)
	begin, end := "-----BEGIN "+"PRIVATE KEY-----", "-----END "+"PRIVATE KEY-----"
	for _, tc := range []struct {
		name string
		body string
	}{
		{"key across lines", "data: {\"t\":\"keepme\"}\n\ndata: " + begin + "\ndata: " + pemBody + "\ndata: " + end + "\n\ndata: {\"t\":\"keepme\"}\n\n"},
		{"key never finished", "data: {\"t\":\"keepme\"}\n\ndata: " + begin + "\ndata: " + pemBody + "\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, chunk := range []int{1, 9, 4096} {
				gate, tok := streamGate(t, tc.body, chunk)
				rec := callGate(gate, tok, "/openai/v1/chat/completions", `{"model":"m","stream":true}`)
				out := rec.Body.String()
				if strings.Contains(out, pemBody[:12]) {
					t.Fatalf("chunk %d: key body relayed: %q", chunk, out)
				}
				if !strings.Contains(out, "keepme") {
					t.Fatalf("chunk %d: clean text lost: %q", chunk, out)
				}
			}
		})
	}
}

// cutReader returns its text, then a read error, as a connection reset does.
type cutReader struct{ s string }

func (r *cutReader) Read(p []byte) (int, error) {
	if r.s == "" {
		return 0, io.ErrUnexpectedEOF
	}
	n := copy(p, r.s)
	r.s = r.s[n:]
	return n, nil
}

// A body that breaks off mid-line could end in the first bytes of a secret,
// too few for a rule to match. Nothing held is sent, and the stop is audited.
func TestModelGateDropsHeldBytesOnReadError(t *testing.T) {
	cut := "gh" + "p_" + "Abc12" // shorter than any token rule
	gate, _, tok := bodyGate(t, func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"text/event-stream"}},
			Body: io.NopCloser(&cutReader{s: "data: {\"t\":\"keepme\"}\n\ndata: {\"t\":\"" + cut})}, nil
	})
	rec := callGate(gate, tok, "/openai/v1/chat/completions", `{"model":"m","stream":true}`)
	if out := rec.Body.String(); strings.Contains(out, cut) || !strings.Contains(out, "keepme") {
		t.Fatalf("relayed %q", out)
	}
	if got := auditReasons(t, gate); !strings.Contains(got, "upstream_response_read_failed") {
		t.Fatalf("audit rows lack the stop: %q", got)
	}
}
