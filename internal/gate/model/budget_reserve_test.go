package model

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/shadewright-labs/sablecourt/internal/domain"
	"github.com/shadewright-labs/sablecourt/internal/store"
)

func TestEstimateRequestReserveBillableFields(t *testing.T) {
	longText := strings.Repeat("w", 400)
	tests := []struct {
		name    string
		body    string
		wantIn  int64
		wantOut int64
	}{
		{name: "short message", body: `{"messages":[{"role":"user","content":"hi"}]}`, wantIn: 1, wantOut: 1},
		{
			name:   "tools definitions count as input",
			body:   `{"messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"f","description":"` + longText + `"}}]}`,
			wantIn: 100, wantOut: 1,
		},
		{
			name:   "legacy functions count as input",
			body:   `{"messages":[],"functions":[{"name":"f","description":"` + longText + `"}]}`,
			wantIn: 100, wantOut: 1,
		},
		{name: "n scales output", body: `{"max_tokens":3,"n":2}`, wantIn: 1, wantOut: 6},
		{name: "n of one", body: `{"max_tokens":3,"n":1}`, wantIn: 1, wantOut: 3},
		{name: "n without limit scales the floor", body: `{"n":4}`, wantIn: 1, wantOut: 4},
		{name: "zero n ignored", body: `{"max_tokens":3,"n":0}`, wantIn: 1, wantOut: 3},
		{name: "negative n ignored", body: `{"max_tokens":3,"n":-2}`, wantIn: 1, wantOut: 3},
		{name: "non numeric n ignored", body: `{"max_tokens":3,"n":"2"}`, wantIn: 1, wantOut: 3},
		{name: "n product saturates", body: `{"max_tokens":9e18,"n":128}`, wantIn: 1, wantOut: maxReservedTokens},
		{name: "huge limit is clamped", body: `{"max_tokens":1e300}`, wantIn: 1, wantOut: maxReservedTokens},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := estimateRequestReserve("openai", "gpt-5", []byte(tt.body), nil)
			if got.InputTokens != tt.wantIn || got.OutputTokens != tt.wantOut {
				t.Fatalf("reserve = %+v, want input %d output %d", got, tt.wantIn, tt.wantOut)
			}
			if got.CostMicroUSD < 1 {
				t.Fatalf("cost reserve = %d, want at least 1", got.CostMicroUSD)
			}
		})
	}
}

type childGateSetup struct {
	gate  *Gate
	st    store.Store
	token string
}

func newChildGate(t *testing.T, ceiling string, upstream http.Handler) childGateSetup {
	t.Helper()
	ctx := t.Context()
	gate, st := setupGateTest(t, nil, handlerTransport(upstream))
	parent := domain.Task{ID: "parent", IdempotencyKey: "parent", WorkloadName: "ai-agent", State: domain.TaskStateWorking}
	if err := st.CreateTask(ctx, parent); err != nil {
		t.Fatal(err)
	}
	child := domain.Task{
		ID: "child", ParentTaskID: parent.ID, IdempotencyKey: "child", WorkloadName: parent.WorkloadName,
		State: domain.TaskStateWorking, BudgetCeilingJSON: ceiling,
	}
	if err := st.CreateTask(ctx, child); err != nil {
		t.Fatal(err)
	}
	token, err := MintTaskToken(ctx, st, child.ID, child.WorkloadName, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	return childGateSetup{gate: gate, st: st, token: token}
}

func (c childGateSetup) post(body string) (int, string) {
	rec := doGateRequest(c.gate.Handler(), http.MethodPost, "/openai/v1/chat/completions", body,
		map[string]string{"Authorization": "Bearer " + c.token})
	return rec.Code, rec.Body.String()
}

func jsonUpstream(body string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	})
}

func TestModelGateReservesToolsAndChoices(t *testing.T) {
	tests := []struct {
		name       string
		ceiling    string
		body       string
		wantReason string
	}{
		{
			name:       "large tools array exceeds the input ceiling",
			ceiling:    `{"maxInputTokens":10}`,
			body:       `{"model":"gpt-5","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"f","description":"` + strings.Repeat("w", 400) + `"}}]}`,
			wantReason: "child_input_tokens",
		},
		{
			name:       "multiple choices exceed the output ceiling",
			ceiling:    `{"maxOutputTokens":5}`,
			body:       `{"model":"gpt-5","max_tokens":3,"n":2}`,
			wantReason: "child_output_tokens",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var calls atomic.Int32
			c := newChildGate(t, tt.ceiling, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				calls.Add(1)
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
			}))
			code, body := c.post(tt.body)
			if code != http.StatusTooManyRequests || !strings.Contains(body, tt.wantReason) {
				t.Fatalf("response = %d %s, want 429 %s", code, body, tt.wantReason)
			}
			if calls.Load() != 0 {
				t.Fatalf("upstream calls = %d, want 0", calls.Load())
			}
		})
	}
}

type reserveRecorderStore struct {
	store.Store
	got []time.Time
}

func (s *reserveRecorderStore) ReserveBudget(ctx context.Context, req store.BudgetReservationRequest) (store.BudgetReservationResult, error) {
	s.got = append(s.got, req.Now)
	return s.Store.ReserveBudget(ctx, req)
}

type advancingReader struct {
	r       io.Reader
	advance func()
}

func (a *advancingReader) Read(p []byte) (int, error) {
	a.advance()
	return a.r.Read(p)
}

func TestModelGateReservationStartsAfterBodyRead(t *testing.T) {
	c := newChildGate(t, `{"maxOutputTokens":50}`, jsonUpstream(`{"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	rec := &reserveRecorderStore{Store: c.st}
	c.gate.store = rec

	clock := time.Now().UTC()
	c.gate.now = func() time.Time { return clock }
	start := clock
	const bodyDelay = 20 * time.Minute // longer than the default reservation lifetime
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "http://sablecourt.local/openai/v1/chat/completions",
		&advancingReader{
			r:       strings.NewReader(`{"model":"gpt-5","max_tokens":3}`),
			advance: func() { clock = start.Add(bodyDelay) },
		})
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	w := &discardResponse{header: http.Header{}}
	c.gate.Handler().ServeHTTP(w, req)
	if w.code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.code)
	}
	if len(rec.got) != 1 {
		t.Fatalf("reservations = %d, want 1", len(rec.got))
	}
	if rec.got[0].Before(start.Add(bodyDelay)) {
		t.Fatalf("reservation time = %v, want at or after body read end %v", rec.got[0], start.Add(bodyDelay))
	}
}

type discardResponse struct {
	header http.Header
	code   int
}

func (d *discardResponse) Header() http.Header         { return d.header }
func (d *discardResponse) Write(p []byte) (int, error) { return len(p), nil }
func (d *discardResponse) WriteHeader(code int) {
	if d.code == 0 {
		d.code = code
	}
}

// flakySettlementStore fails the first failures settlement calls, with or without usage.
type flakySettlementStore struct {
	store.Store
	failures atomic.Int32
	calls    atomic.Int32
}

func (s *flakySettlementStore) SettleBudgetReservation(ctx context.Context, id string, actual *domain.BudgetEntry) error {
	s.calls.Add(1)
	if s.failures.Add(-1) >= 0 {
		return errors.New("transient settlement failure")
	}
	return s.Store.SettleBudgetReservation(ctx, id, actual)
}

func TestModelGateRetriesFailedSettlementWithUsage(t *testing.T) {
	c := newChildGate(t, `{"maxOutputTokens":5}`, jsonUpstream(`{"model":"gpt-5","usage":{"prompt_tokens":1,"completion_tokens":2}}`))
	flaky := &flakySettlementStore{Store: c.st}
	flaky.failures.Store(1)
	c.gate.store = flaky

	if code, body := c.post(`{"model":"gpt-5","max_tokens":3}`); code != http.StatusOK {
		t.Fatalf("first response = %d %s, want 200", code, body)
	}
	totals, err := c.st.TaskBudgetTotals(t.Context(), "child")
	if err != nil {
		t.Fatal(err)
	}
	if totals.OutputTokens != 2 {
		t.Fatalf("recorded output tokens = %d, want 2 after the retried settlement", totals.OutputTokens)
	}
	// Settled usage is 2, so 3 more fits. A leftover reservation of 3 would refuse it.
	if code, body := c.post(`{"model":"gpt-5","max_tokens":3}`); code != http.StatusOK {
		t.Fatalf("second response = %d %s, want 200", code, body)
	}
}

func TestModelGateRetriesFailedRelease(t *testing.T) {
	var fail atomic.Bool
	fail.Store(true)
	c := newChildGate(t, `{"maxOutputTokens":5}`, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if fail.Load() {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	}))
	flaky := &flakySettlementStore{Store: c.st}
	flaky.failures.Store(1)
	c.gate.store = flaky

	if code, _ := c.post(`{"model":"gpt-5","max_tokens":5}`); code != http.StatusInternalServerError {
		t.Fatalf("first response = %d, want upstream 500", code)
	}
	fail.Store(false)
	if code, body := c.post(`{"model":"gpt-5","max_tokens":5}`); code != http.StatusOK {
		t.Fatalf("response after retried release = %d %s, want 200", code, body)
	}
}

func TestModelGateSettlesReservedEstimateWithoutUsage(t *testing.T) {
	upstream := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"model\":\"gpt-5\",\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\ndata: [DONE]\n\n"))
	})
	c := newChildGate(t, `{"maxOutputTokens":5}`, upstream)

	if code, body := c.post(`{"model":"gpt-5","stream":true,"max_tokens":3}`); code != http.StatusOK {
		t.Fatalf("first response = %d %s, want 200", code, body)
	}
	totals, err := c.st.TaskBudgetTotals(t.Context(), "child")
	if err != nil {
		t.Fatal(err)
	}
	if totals.OutputTokens != 3 || totals.CostMicroUSD < 1 {
		t.Fatalf("recorded usage = %+v, want the reserved 3 output tokens and a positive cost", totals)
	}
	code, body := c.post(`{"model":"gpt-5","stream":true,"max_tokens":3}`)
	if code != http.StatusTooManyRequests || !strings.Contains(body, "child_output_tokens") {
		t.Fatalf("second response = %d %s, want 429 child_output_tokens", code, body)
	}
}
