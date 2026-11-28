package model

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
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
		{name: "short message", body: `{"messages":[{"role":"user","content":"hi"}]}`, wantIn: 4, wantOut: 1},
		{
			name:   "tools definitions count as input",
			body:   `{"messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"f","description":"` + longText + `"}}]}`,
			wantIn: 113, wantOut: 1,
		},
		{
			name:   "legacy functions count as input",
			body:   `{"messages":[],"functions":[{"name":"f","description":"` + longText + `"}]}`,
			wantIn: 104, wantOut: 1,
		},
		{
			name:   "responses instructions count as input",
			body:   `{"instructions":"` + longText + `","input":"hi"}`,
			wantIn: 100, wantOut: 1,
		},
		{
			name:   "gemini snake case system_instruction counts as input",
			body:   `{"system_instruction":{"parts":[{"text":"` + longText + `"}]}}`,
			wantIn: 102, wantOut: 1,
		},
		{
			name:   "response_format schema counts as input",
			body:   `{"messages":[],"response_format":{"type":"json_schema","json_schema":{"description":"` + longText + `"}}}`,
			wantIn: 109, wantOut: 1,
		},
		{
			name:   "responses text format schema counts as input",
			body:   `{"input":"hi","text":{"format":{"type":"json_schema","description":"` + longText + `"}}}`,
			wantIn: 108, wantOut: 1,
		},
		{name: "n scales output", body: `{"max_tokens":3,"n":2}`, wantIn: 1, wantOut: 6},
		{name: "n of one", body: `{"max_tokens":3,"n":1}`, wantIn: 1, wantOut: 3},
		{name: "n without limit scales the floor", body: `{"n":4}`, wantIn: 1, wantOut: 4},
		{name: "zero n ignored", body: `{"max_tokens":3,"n":0}`, wantIn: 1, wantOut: 3},
		{name: "negative n ignored", body: `{"max_tokens":3,"n":-2}`, wantIn: 1, wantOut: 3},
		{name: "non numeric n ignored", body: `{"max_tokens":3,"n":"2"}`, wantIn: 1, wantOut: 3},
		{name: "gemini candidate count scales output", body: `{"generationConfig":{"maxOutputTokens":3,"candidateCount":2}}`, wantIn: 1, wantOut: 6},
		{name: "gemini candidate count of one", body: `{"generationConfig":{"maxOutputTokens":3,"candidateCount":1}}`, wantIn: 1, wantOut: 3},
		{name: "gemini candidate count saturates", body: `{"generationConfig":{"maxOutputTokens":9e18,"candidateCount":8}}`, wantIn: 1, wantOut: maxReservedTokens},
		{name: "gemini snake case config", body: `{"generation_config":{"max_output_tokens":3,"candidate_count":2}}`, wantIn: 1, wantOut: 6},
		{name: "gemini string limits", body: `{"generationConfig":{"maxOutputTokens":"3","candidateCount":"2"}}`, wantIn: 1, wantOut: 6},
		{name: "gemini larger of both configs", body: `{"generationConfig":{"maxOutputTokens":3},"generation_config":{"max_output_tokens":5}}`, wantIn: 1, wantOut: 5},
		{
			name:   "tool property named name still counts",
			body:   `{"tools":[{"parameters":{"properties":{"name":{"type":"string","description":"` + longText + `"}}}}]}`,
			wantIn: 111, wantOut: 1,
		},
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
			w.Header().Set("Content-Type", "application/json")
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

func TestModelGateReservesFallbackProviderCost(t *testing.T) {
	// With default pricing, openai charges 10 microUSD per output token (plus 2.5 per input):
	// max_tokens=10 with 1 input token estimates 103 microUSD on openai and 1 microUSD (floor) on local.
	const reqBody = `{"model":"gpt-5","max_tokens":10}`

	tests := []struct {
		name           string
		route          string
		providers      []string
		upstreams      map[string]UpstreamConfig
		pricing        map[string]PricePerMillionTokens
		ceiling        string
		wantCode       int
		wantBodySubstr string
		wantCalls      int32
	}{
		{
			name:           "local with openai fallback reserves openai cost",
			route:          "/local/v1/chat/completions",
			providers:      []string{"local", "openai"},
			ceiling:        `{"maxCostUSD":0.000100}`,
			wantCode:       http.StatusTooManyRequests,
			wantBodySubstr: "child_cost_usd",
			wantCalls:      0,
		},
		{
			name:      "local without openai grant reserves local cost",
			route:     "/local/v1/chat/completions",
			providers: []string{"local"},
			ceiling:   `{"maxCostUSD":0.000100}`,
			wantCode:  http.StatusOK,
			wantCalls: 1,
		},
		{
			name:      "local without configured openai upstream reserves local cost",
			route:     "/local/v1/chat/completions",
			providers: []string{"local", "openai"},
			upstreams: map[string]UpstreamConfig{
				"local": {BaseURL: "http://127.0.0.1:11434", SecretName: "local_model_api_key", RequireSecret: false},
			},
			ceiling:   `{"maxCostUSD":0.000100}`,
			wantCode:  http.StatusOK,
			wantCalls: 1,
		},
		{
			name:      "openai with costlier local fallback reserves local cost",
			route:     "/openai/v1/chat/completions",
			providers: []string{"openai", "local"},
			pricing: map[string]PricePerMillionTokens{
				"local": {InputUSD: 50.0, OutputUSD: 50.0},
			},
			ceiling:        `{"maxCostUSD":0.000200}`,
			wantCode:       http.StatusTooManyRequests,
			wantBodySubstr: "child_cost_usd",
			wantCalls:      0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var calls atomic.Int32
			upstream := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				calls.Add(1)
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"model":"gpt-5","usage":{"prompt_tokens":1,"completion_tokens":1}}`))
			})
			c := newChildGate(t, tt.ceiling, upstream)
			wl := c.gate.bundle.Workloads["ai-agent"]
			wl.Spec.Grants.ModelGate.Providers = tt.providers
			c.gate.bundle.Workloads["ai-agent"] = wl
			if tt.upstreams != nil {
				c.gate.upstreams = tt.upstreams
			}
			if tt.pricing != nil {
				c.gate.pricing = tt.pricing
			}

			rec := doGateRequest(c.gate.Handler(), http.MethodPost, tt.route, reqBody,
				map[string]string{"Authorization": "Bearer " + c.token})
			if rec.Code != tt.wantCode {
				t.Fatalf("status = %d body = %s, want %d", rec.Code, rec.Body.String(), tt.wantCode)
			}
			if tt.wantBodySubstr != "" && !strings.Contains(rec.Body.String(), tt.wantBodySubstr) {
				t.Fatalf("body = %s, want substring %q", rec.Body.String(), tt.wantBodySubstr)
			}
			if calls.Load() != tt.wantCalls {
				t.Fatalf("upstream calls = %d, want %d", calls.Load(), tt.wantCalls)
			}
		})
	}
}

func TestModelGateConcurrentLocalFallbackRespectsCostCeiling(t *testing.T) {
	const workers = 6
	const ceilingMicroUSD = 150
	const reqBody = `{"model":"gpt-5","max_tokens":10}` // openai reserve = 103 microUSD

	admissionDone := make(chan struct{}, 2*workers)
	finishOpenAI := make(chan struct{})
	var openaiCalls atomic.Int32

	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.Host, "127.0.0.1") {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		openaiCalls.Add(1)
		admissionDone <- struct{}{}
		<-finishOpenAI
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"model":"gpt-5","usage":{"prompt_tokens":1,"completion_tokens":10}}`))
	})

	c := newChildGate(t, `{"maxCostUSD":0.000150}`, upstream)
	wl := c.gate.bundle.Workloads["ai-agent"]
	wl.Spec.Grants.ModelGate.Providers = []string{"local", "openai"}
	c.gate.bundle.Workloads["ai-agent"] = wl

	start := make(chan struct{})
	var okCount, deniedCount, otherCount atomic.Int32
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			rec := doGateRequest(c.gate.Handler(), http.MethodPost, "/local/v1/chat/completions", reqBody,
				map[string]string{"Authorization": "Bearer " + c.token})
			admissionDone <- struct{}{}
			switch rec.Code {
			case http.StatusOK:
				okCount.Add(1)
			case http.StatusTooManyRequests:
				if strings.Contains(rec.Body.String(), "child_cost_usd") {
					deniedCount.Add(1)
				} else {
					otherCount.Add(1)
					t.Errorf("unexpected 429 body = %s", rec.Body.String())
				}
			default:
				otherCount.Add(1)
				t.Errorf("unexpected status = %d body = %s", rec.Code, rec.Body.String())
			}
		}()
	}
	close(start)
	for range workers {
		<-admissionDone
	}
	close(finishOpenAI)
	wg.Wait()

	if okCount.Load() != 1 || deniedCount.Load() != workers-1 || otherCount.Load() != 0 {
		t.Fatalf("ok=%d denied=%d other=%d, want ok=1 denied=%d other=0", okCount.Load(), deniedCount.Load(), otherCount.Load(), workers-1)
	}
	if openaiCalls.Load() != 1 {
		t.Fatalf("openai fallback calls = %d, want 1", openaiCalls.Load())
	}
	totals, err := c.st.TaskBudgetTotals(t.Context(), "child")
	if err != nil {
		t.Fatal(err)
	}
	if totals.CostMicroUSD > ceilingMicroUSD {
		t.Fatalf("settled cost = %d microUSD, exceeds ceiling %d", totals.CostMicroUSD, ceilingMicroUSD)
	}
	if totals.CostMicroUSD != 103 {
		t.Fatalf("settled cost = %d microUSD, want the openai charge of 103", totals.CostMicroUSD)
	}
}

func TestModelGateSettlesAtRequestModelPrice(t *testing.T) {
	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"model":"gpt-5-expensive","usage":{"prompt_tokens":1,"completion_tokens":10}}`))
	})
	c := newChildGate(t, `{"maxCostUSD":0.000150}`, upstream)
	c.gate.pricing = map[string]PricePerMillionTokens{
		"gpt-5":           {InputUSD: 1, OutputUSD: 1},
		"gpt-5-expensive": {InputUSD: 50, OutputUSD: 50},
	}
	rec := doGateRequest(c.gate.Handler(), http.MethodPost, "/openai/v1/chat/completions",
		`{"model":"gpt-5","max_tokens":10}`, map[string]string{"Authorization": "Bearer " + c.token})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
	totals, err := c.st.TaskBudgetTotals(t.Context(), "child")
	if err != nil {
		t.Fatal(err)
	}
	if totals.CostMicroUSD != 11 {
		t.Fatalf("settled cost = %d microUSD, want 11 at the request model rate", totals.CostMicroUSD)
	}
}

type settlementCtxCaptureStore struct {
	store.Store
	mu          sync.Mutex
	calls       int
	ctxErr      error
	hasDeadline bool
	deadline    time.Time
	observedAt  time.Time
}

func (s *settlementCtxCaptureStore) SettleBudgetReservation(ctx context.Context, id string, actual *domain.BudgetEntry) error {
	s.mu.Lock()
	s.calls++
	s.ctxErr = ctx.Err()
	s.deadline, s.hasDeadline = ctx.Deadline()
	s.observedAt = time.Now()
	s.mu.Unlock()
	return s.Store.SettleBudgetReservation(ctx, id, actual)
}

func TestModelGateSettlementContextIsDetachedAndBounded(t *testing.T) {
	if settleReservationTimeout != 10*time.Second {
		t.Fatalf("settleReservationTimeout = %v, want 10s", settleReservationTimeout)
	}
	const wantTimeout = settleReservationTimeout

	tests := []struct {
		name       string
		statusCode int
		respBody   string
	}{
		{
			name:       "settle with usage after caller cancel",
			statusCode: http.StatusOK,
			respBody:   `{"model":"gpt-5","usage":{"prompt_tokens":1,"completion_tokens":2}}`,
		},
		{
			name:       "release without usage after caller cancel",
			statusCode: http.StatusInternalServerError,
			respBody:   `{"error":"upstream failed"}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reqCtx, cancelReq := context.WithCancel(t.Context())
			defer cancelReq()

			upstream := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tt.statusCode)
				_, _ = w.Write([]byte(tt.respBody))
				cancelReq()
			})
			c := newChildGate(t, `{"maxOutputTokens":50}`, upstream)
			capture := &settlementCtxCaptureStore{Store: c.st}
			c.gate.store = capture

			before := time.Now()
			req := httptest.NewRequest(http.MethodPost, "http://sablecourt.local/openai/v1/chat/completions",
				strings.NewReader(`{"model":"gpt-5","max_tokens":3}`)).WithContext(reqCtx)
			req.Header.Set("Authorization", "Bearer "+c.token)
			rec := httptest.NewRecorder()
			c.gate.Handler().ServeHTTP(rec, req)

			if capture.calls != 1 {
				t.Fatalf("SettleBudgetReservation calls = %d, want 1", capture.calls)
			}
			if capture.ctxErr != nil {
				t.Fatalf("settlement ctx.Err() = %v, want nil (detached from caller cancellation)", capture.ctxErr)
			}
			if !capture.hasDeadline {
				t.Fatal("settlement context has no deadline, want bounded timeout")
			}
			if capture.deadline.Before(before.Add(wantTimeout)) || capture.deadline.After(capture.observedAt.Add(wantTimeout)) {
				t.Fatalf("settlement deadline = %v, want within [%v, %v]",
					capture.deadline, before.Add(wantTimeout), capture.observedAt.Add(wantTimeout))
			}
		})
	}
}
