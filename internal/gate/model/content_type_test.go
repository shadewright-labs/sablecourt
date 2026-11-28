package model

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/shadewright-labs/sablecourt/internal/domain"
	"github.com/shadewright-labs/sablecourt/internal/store"
)

// settleRecorder records each settlement: a nil entry is a release.
type settleRecorder struct {
	store.Store
	mu      sync.Mutex
	settled []*domain.BudgetEntry
}

func (s *settleRecorder) SettleBudgetReservation(ctx context.Context, id string, actual *domain.BudgetEntry) error {
	s.mu.Lock()
	s.settled = append(s.settled, actual)
	s.mu.Unlock()
	return s.Store.SettleBudgetReservation(ctx, id, actual)
}

// A relayed body reaches the workload with a Content-Type the gate picks from
// its allow-list and with nosniff. Any other upstream type, on any status and
// on the fallback path, is refused with a fixed 502 that never carries the
// upstream body; a refused 2xx settles the reserve, a refused non-2xx releases.
func TestModelGateRelaysOnlyAllowedContentTypes(t *testing.T) {
	const marker = "<script>alert(1)</script>"
	for _, tc := range []struct {
		name     string
		status   int
		types    []string // upstream Content-Type values; nil sends none
		fallback bool     // the primary answers 503 and local answers instead
		wantCode int
		wantType string // relayed Content-Type; "" means refused
		charged  bool   // a refusal settles the reserve instead of releasing it
	}{
		{"json", 200, []string{"application/json"}, false, 200, "application/json", false},
		{"json utf-8 charset", 200, []string{"application/json; charset=UTF-8"}, false, 200, "application/json; charset=utf-8", false},
		{"json quoted charset", 200, []string{`Application/JSON;charset="utf-8"`}, false, 200, "application/json; charset=utf-8", false},
		{"sse", 200, []string{"text/event-stream"}, false, 200, "text/event-stream", false},
		{"sse utf-8 charset", 200, []string{"text/event-stream;charset=utf-8"}, false, 200, "text/event-stream; charset=utf-8", false},
		{"json error relayed", 400, []string{"application/json"}, false, 400, "application/json", false},
		{"json on fallback", 200, []string{"application/json"}, true, 200, "application/json", false},
		{"html", 200, []string{"text/html; charset=utf-8"}, false, 502, "", true},
		{"html error", 500, []string{"text/html"}, false, 502, "", false},
		{"html on fallback", 200, []string{"text/html"}, true, 502, "", true},
		{"plain text", 200, []string{"text/plain"}, false, 502, "", true},
		{"missing", 200, nil, false, 502, "", true},
		{"two values", 200, []string{"application/json", "text/html"}, false, 502, "", true},
		{"json with utf-7 charset", 200, []string{"application/json; charset=utf-7"}, false, 502, "", true},
		{"json with extra parameter", 200, []string{"application/json; profile=x"}, false, 502, "", true},
		{"json suffix type", 200, []string{"application/problem+json"}, false, 502, "", true},
		{"ndjson", 200, []string{"application/x-ndjson"}, false, 502, "", true},
		{"unparsable", 200, []string{"application/json; charset"}, false, 502, "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := `{"model":"m","note":"` + marker + `","usage":{"prompt_tokens":1,"completion_tokens":1}}`
			gate, tok := responseGate(t, ctKeys, func(r *http.Request) (*http.Response, error) {
				if tc.fallback && r.URL.Host == "api.openai.com" {
					return &http.Response{StatusCode: http.StatusServiceUnavailable, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{}`))}, nil
				}
				h := http.Header{"X-Content-Type-Options": {"off"}}
				for _, v := range tc.types {
					h.Add("Content-Type", v)
				}
				return &http.Response{StatusCode: tc.status, Header: h, Body: io.NopCloser(strings.NewReader(body))}, nil
			})
			rec := &settleRecorder{Store: gate.store}
			gate.store = rec
			req := httptest.NewRequest(http.MethodPost, "/openai/v1/chat/completions", strings.NewReader(`{"model":"gpt-4o","max_tokens":5}`))
			req.Header.Set("Authorization", "Bearer "+tok)
			w := httptest.NewRecorder()
			gate.Handler().ServeHTTP(w, req)

			if w.Code != tc.wantCode {
				t.Fatalf("status %d %s, want %d", w.Code, w.Body.String(), tc.wantCode)
			}
			if got := w.Header().Values("X-Content-Type-Options"); len(got) != 1 || got[0] != "nosniff" {
				t.Fatalf("X-Content-Type-Options = %q, want nosniff", got)
			}
			gotTypes := w.Header().Values("Content-Type")
			if tc.wantType == "" {
				if w.Body.String() != `{"error":"upstream response content type not supported"}`+"\n" {
					t.Fatalf("refusal body = %q", w.Body.String())
				}
				if len(gotTypes) != 1 || gotTypes[0] != "application/json" {
					t.Fatalf("refusal Content-Type = %q", gotTypes)
				}
				if len(rec.settled) != 1 || (rec.settled[0] != nil) != tc.charged {
					t.Fatalf("settlements = %v, want charged=%v", rec.settled, tc.charged)
				}
				return
			}
			if len(gotTypes) != 1 || gotTypes[0] != tc.wantType {
				t.Fatalf("Content-Type = %q, want %q", gotTypes, tc.wantType)
			}
			if !strings.Contains(w.Body.String(), `"model":"m"`) {
				t.Fatalf("relayed body = %q", w.Body.String())
			}
		})
	}
}

// A streamed SSE reply keeps arriving in order through the allow-list, and
// refusals the gate writes itself carry nosniff too.
func TestModelGateSSEStreamAndRefusalsCarryNosniff(t *testing.T) {
	stream := "data: {\"model\":\"gpt-4o\",\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\ndata: [DONE]\n\n"
	gate, tok := responseGate(t, ctKeys, func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(&trickleReader{s: stream, n: 7})}, nil
	})
	req := httptest.NewRequest(http.MethodPost, "/openai/v1/chat/completions", strings.NewReader(`{"model":"gpt-4o","stream":true}`))
	req.Header.Set("Authorization", "Bearer "+tok)
	w := httptest.NewRecorder()
	gate.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK || w.Body.String() != stream {
		t.Fatalf("stream = %d %q", w.Code, w.Body.String())
	}
	if w.Header().Get("Content-Type") != "text/event-stream" || w.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("stream headers = %v", w.Header())
	}

	for _, path := range []string{"/openai/v1/unknown", "/openai/v1/chat/completions"} {
		w := httptest.NewRecorder()
		gate.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{}`)))
		if w.Code < 400 || w.Header().Get("X-Content-Type-Options") != "nosniff" || w.Header().Get("Content-Type") != "application/json" {
			t.Fatalf("%s refusal: %d %v", path, w.Code, w.Header())
		}
	}
}

var ctKeys = map[string]string{"openai_api_key": "sk-" + "ctype_" + strings.Repeat("c3", 10)}
