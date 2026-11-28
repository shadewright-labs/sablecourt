package model

import (
	"context"
	"errors"
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

func TestModelGateChildBudgetCeiling(t *testing.T) {
	const chat = "/openai/v1/chat/completions"
	type testCase struct {
		name       string
		ceiling    string
		usage      domain.BudgetTotals
		route      string
		dailyCost  int64
		want       []int
		reason     string
		root       bool
		lookupFail bool
	}
	tests := []testCase{
		{
			name: "input exhausted", ceiling: `{"maxInputTokens":10}`,
			usage: domain.BudgetTotals{InputTokens: 10},
			want:  []int{429}, reason: "child_input_tokens",
		},
		{
			name: "output exhausted on responses route", ceiling: `{"maxOutputTokens":10}`,
			usage: domain.BudgetTotals{OutputTokens: 11},
			route: "/openai/v1/responses", want: []int{429}, reason: "child_output_tokens",
		},
		{
			name: "cost exhausted", ceiling: `{"maxCostUSD":0.5}`,
			usage: domain.BudgetTotals{CostMicroUSD: 500000},
			want:  []int{429}, reason: "child_cost_usd",
		},
		{
			name:    "within child and workload caps then child exhausted",
			ceiling: `{"maxInputTokens":10,"maxOutputTokens":8,"maxCostUSD":0.5}`,
			usage:   domain.BudgetTotals{InputTokens: 9, OutputTokens: 3, CostMicroUSD: 499000},
			want:    []int{200, 429}, reason: "child_input_tokens",
		},
		{
			name: "zero cap", ceiling: `{"maxOutputTokens":0}`,
			want: []int{429}, reason: "child_output_tokens",
		},
		{
			name: "sub-micro cost cap", ceiling: `{"maxCostUSD":0.0000001}`,
			want: []int{429}, reason: "child_cost_usd",
		},
		{
			name: "child cannot raise workload task cap", ceiling: `{"maxInputTokens":2000}`,
			usage: domain.BudgetTotals{InputTokens: 1000},
			want:  []int{429}, reason: "per_task_input_tokens",
		},
		{
			name: "child cannot raise workload daily cap", ceiling: `{"maxCostUSD":10}`,
			dailyCost: 5000000, want: []int{429}, reason: "daily_cost_usd",
		},
		{
			name: "absent value", want: []int{403}, reason: "invalid_child_budget_ceiling",
		},
		{
			name: "empty object", ceiling: `{}`,
			want: []int{403}, reason: "invalid_child_budget_ceiling",
		},
		{
			name: "empty child ceiling with exhausted workload", ceiling: `{}`,
			usage: domain.BudgetTotals{OutputTokens: 500},
			want:  []int{429}, reason: "per_task_output_tokens",
		},
		{
			name: "root without ceiling still uses workload", root: true,
			usage: domain.BudgetTotals{InputTokens: 999},
			want:  []int{200, 429}, reason: "per_task_input_tokens",
		},
		{
			name: "task lookup failure", ceiling: `{"maxInputTokens":10}`, lookupFail: true,
			want: []int{500}, reason: "budget check failed",
		},
	}
	for _, malformed := range []struct {
		name string
		json string
	}{
		{"syntax", `{"maxInputTokens":`},
		{"wrong object type", `[]`},
		{"wrong value type", `{"maxInputTokens":"10"}`},
		{"null object", `null`},
		{"null limit", `{"maxInputTokens":null,"maxOutputTokens":10}`},
		{"unknown field", `{"maxInputTokens":10,"maxOutputToken":10}`},
		{"duplicate field", `{"maxInputTokens":10,"maxInputTokens":100}`},
		{"negative limit", `{"maxCostUSD":-1}`},
		{"fractional tokens", `{"maxInputTokens":1.5}`},
		{"overflowing tokens", `{"maxOutputTokens":9223372036854775808}`},
		{"overflowing cost", `{"maxCostUSD":1e20}`},
		{"trailing value", `{"maxOutputTokens":10}{}`},
		{"oversized JSON", `{"maxOutputTokens":10}` + strings.Repeat(" ", 4096)},
	} {
		tests = append(tests, testCase{
			name: malformed.name, ceiling: malformed.json,
			want: []int{403}, reason: "invalid_child_budget_ceiling",
		})
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := t.Context()
			upstreamCalls := 0
			upstream := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				upstreamCalls++
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
			})
			gate, st := setupGateTest(t, nil, handlerTransport(upstream))
			parent := domain.Task{
				ID: "parent", IdempotencyKey: "parent", WorkloadName: "ai-agent",
				State: domain.TaskStateWorking,
			}
			if err := st.CreateTask(ctx, parent); err != nil {
				t.Fatal(err)
			}
			task := domain.Task{
				ID: "request-task", IdempotencyKey: "request-task", WorkloadName: parent.WorkloadName,
				State: domain.TaskStateWorking, BudgetCeilingJSON: tt.ceiling,
			}
			if !tt.root {
				task.ParentTaskID = parent.ID
			}
			if err := st.CreateTask(ctx, task); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := st.RevokeTaskLeases(context.Background(), task.ID); err != nil {
					t.Error(err)
				}
			})
			token, err := MintTaskToken(ctx, st, task.ID, task.WorkloadName, time.Hour)
			if err != nil {
				t.Fatal(err)
			}
			if tt.usage != (domain.BudgetTotals{}) {
				if err := st.RecordBudgetUsage(ctx, domain.BudgetEntry{
					TaskID: task.ID, WorkloadName: task.WorkloadName,
					InputTokens: tt.usage.InputTokens, OutputTokens: tt.usage.OutputTokens,
					CostMicroUSD: tt.usage.CostMicroUSD, CreatedAt: gate.now(),
				}); err != nil {
					t.Fatal(err)
				}
			}
			if tt.dailyCost != 0 {
				if err := st.RecordBudgetUsage(ctx, domain.BudgetEntry{
					TaskID: parent.ID, WorkloadName: parent.WorkloadName,
					CostMicroUSD: tt.dailyCost, CreatedAt: gate.now(),
				}); err != nil {
					t.Fatal(err)
				}
			}
			if tt.lookupFail {
				gate.store = failingBudgetTaskStore{Store: st}
			}

			route := tt.route
			if route == "" {
				route = chat
			}
			wantCalls := 0
			for _, want := range tt.want {
				req := httptest.NewRequest(http.MethodPost, route, strings.NewReader(`{"model":"test-model"}`))
				req.Header.Set("Authorization", "Bearer "+token)
				rec := httptest.NewRecorder()
				gate.Handler().ServeHTTP(rec, req)
				if rec.Code != want {
					t.Errorf("status = %d, want %d, body = %s", rec.Code, want, rec.Body)
				}
				if want == http.StatusOK {
					wantCalls++
				}
				if want >= 400 && !strings.Contains(rec.Body.String(), tt.reason) {
					t.Errorf("response = %s, want reason %q", rec.Body, tt.reason)
				}
			}
			if upstreamCalls != wantCalls {
				t.Errorf("upstream calls = %d, want %d", upstreamCalls, wantCalls)
			}
		})
	}
}

type failingBudgetTaskStore struct {
	store.Store
}

func (failingBudgetTaskStore) GetTask(context.Context, string) (domain.Task, error) {
	return domain.Task{}, errors.New("task lookup failed")
}

func TestModelGateChildBudgetConcurrentReservation(t *testing.T) {
	type raceCase struct {
		name         string
		ceiling      string
		route        string
		body         string
		upstreamResp string
		workers      int
		wantOK       int32
		wantReason   string
	}
	cases := []raceCase{
		{
			name:         "input_ceiling_race",
			ceiling:      `{"maxInputTokens":3}`,
			route:        "/openai/v1/chat/completions",
			body:         `{"model":"gpt-5"}`,
			upstreamResp: `{"model":"gpt-5","usage":{"prompt_tokens":1,"completion_tokens":1}}`,
			workers:      12,
			wantOK:       3,
			wantReason:   "child_input_tokens",
		},
		{
			name:         "output_ceiling_race",
			ceiling:      `{"maxOutputTokens":10}`,
			route:        "/openai/v1/chat/completions",
			body:         `{"model":"gpt-5","max_tokens":3}`,
			upstreamResp: `{"model":"gpt-5","usage":{"prompt_tokens":1,"completion_tokens":2}}`,
			workers:      12,
			wantOK:       3,
			wantReason:   "child_output_tokens",
		},
		{
			name:         "cost_ceiling_race",
			ceiling:      `{"maxCostUSD":0.000030}`,
			route:        "/openai/v1/chat/completions",
			body:         `{"model":"gpt-5"}`,
			upstreamResp: `{"model":"gpt-5","usage":{"prompt_tokens":1,"completion_tokens":1}}`,
			workers:      10,
			wantOK:       2,
			wantReason:   "child_cost_usd",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			admissionDone := make(chan struct{}, 2*tc.workers)
			finishUpstream := make(chan struct{})
			var upstreamCalls atomic.Int32
			upstream := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				upstreamCalls.Add(1)
				admissionDone <- struct{}{}
				<-finishUpstream
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(tc.upstreamResp))
			})
			gate, st := setupGateTest(t, nil, handlerTransport(upstream))
			parent := domain.Task{
				ID: "parent-" + tc.name, IdempotencyKey: "parent-" + tc.name,
				WorkloadName: "ai-agent", State: domain.TaskStateWorking,
			}
			if err := st.CreateTask(ctx, parent); err != nil {
				t.Fatal(err)
			}
			child := domain.Task{
				ID: "child-" + tc.name, ParentTaskID: parent.ID,
				IdempotencyKey: "child-" + tc.name, WorkloadName: parent.WorkloadName,
				State: domain.TaskStateWorking, BudgetCeilingJSON: tc.ceiling,
			}
			if err := st.CreateTask(ctx, child); err != nil {
				t.Fatal(err)
			}
			token, err := MintTaskToken(ctx, st, child.ID, child.WorkloadName, time.Hour)
			if err != nil {
				t.Fatal(err)
			}

			start := make(chan struct{})
			var okCount, deniedCount, otherCount atomic.Int32
			var wg sync.WaitGroup
			for range tc.workers {
				wg.Add(1)
				go func() {
					defer wg.Done()
					<-start
					req := httptest.NewRequest(http.MethodPost, tc.route, strings.NewReader(tc.body))
					req.Header.Set("Authorization", "Bearer "+token)
					rec := httptest.NewRecorder()
					gate.Handler().ServeHTTP(rec, req)
					admissionDone <- struct{}{}
					switch rec.Code {
					case http.StatusOK:
						okCount.Add(1)
					case http.StatusTooManyRequests:
						if strings.Contains(rec.Body.String(), tc.wantReason) {
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
			// Each request reaches upstream or returns a response before any reservation can settle.
			for range tc.workers {
				<-admissionDone
			}
			close(finishUpstream)
			wg.Wait()

			if got := okCount.Load(); got != tc.wantOK {
				t.Fatalf("200 OK count = %d, want %d (denied=%d, upstreamCalls=%d)", got, tc.wantOK, deniedCount.Load(), upstreamCalls.Load())
			}
			if got := deniedCount.Load(); got != int32(tc.workers)-tc.wantOK {
				t.Fatalf("429 count = %d, want %d", got, int32(tc.workers)-tc.wantOK)
			}
			if otherCount.Load() != 0 {
				t.Fatalf("unexpected non-200/429 responses: %d", otherCount.Load())
			}
			if got := upstreamCalls.Load(); got != tc.wantOK {
				t.Fatalf("upstream calls = %d, want %d", got, tc.wantOK)
			}
		})
	}
}

type failingSettlementStore struct {
	store.Store
}

func (s failingSettlementStore) SettleBudgetReservation(ctx context.Context, id string, actual *domain.BudgetEntry) error {
	if actual != nil {
		return errors.New("settlement failed")
	}
	return s.Store.SettleBudgetReservation(ctx, id, nil)
}

func TestModelGateChildBudgetFailedSettlement(t *testing.T) {
	ctx := t.Context()
	upstream := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"model":"gpt-5","usage":{"prompt_tokens":1,"completion_tokens":2}}`))
	})
	gate, st := setupGateTest(t, nil, handlerTransport(upstream))
	gate.store = failingSettlementStore{Store: st}
	parent := domain.Task{
		ID: "parent-settle", IdempotencyKey: "parent-settle",
		WorkloadName: "ai-agent", State: domain.TaskStateWorking,
	}
	if err := st.CreateTask(ctx, parent); err != nil {
		t.Fatal(err)
	}
	child := domain.Task{
		ID: "child-settle", ParentTaskID: parent.ID, IdempotencyKey: "child-settle",
		WorkloadName: parent.WorkloadName, State: domain.TaskStateWorking,
		BudgetCeilingJSON: `{"maxOutputTokens":5}`,
	}
	if err := st.CreateTask(ctx, child); err != nil {
		t.Fatal(err)
	}
	token, err := MintTaskToken(ctx, st, child.ID, child.WorkloadName, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	h := gate.Handler()
	headers := map[string]string{"Authorization": "Bearer " + token}
	const route = "/openai/v1/chat/completions"
	const body = `{"model":"gpt-5","max_tokens":3}`
	if rec := doGateRequest(h, http.MethodPost, route, body, headers); rec.Code != http.StatusOK {
		t.Fatalf("first response = %d %s, want 200", rec.Code, rec.Body.String())
	}
	if rec := doGateRequest(h, http.MethodPost, route, body, headers); rec.Code != http.StatusTooManyRequests || !strings.Contains(rec.Body.String(), "child_output_tokens") {
		t.Fatalf("response after failed settlement = %d %s, want 429 child_output_tokens", rec.Code, rec.Body.String())
	}
}

func TestModelGateChildBudgetRequestEstimateAndRelease(t *testing.T) {
	ctx := t.Context()
	var failNext atomic.Bool
	failNext.Store(true)
	upstream := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if failNext.Load() {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":"boom"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"model":"gpt-5","usage":{"prompt_tokens":1,"completion_tokens":2}}`))
	})
	gate, st := setupGateTest(t, nil, handlerTransport(upstream))
	parent := domain.Task{
		ID: "parent-est", IdempotencyKey: "parent-est",
		WorkloadName: "ai-agent", State: domain.TaskStateWorking,
	}
	if err := st.CreateTask(ctx, parent); err != nil {
		t.Fatal(err)
	}
	child := domain.Task{
		ID: "child-est", ParentTaskID: parent.ID,
		IdempotencyKey: "child-est", WorkloadName: parent.WorkloadName,
		State:             domain.TaskStateWorking,
		BudgetCeilingJSON: `{"maxInputTokens":10,"maxOutputTokens":5,"maxCostUSD":0.001}`,
	}
	if err := st.CreateTask(ctx, child); err != nil {
		t.Fatal(err)
	}
	token, err := MintTaskToken(ctx, st, child.ID, child.WorkloadName, time.Hour)
	if err != nil {
		t.Fatal(err)
	}

	// 1. Oversized prompt exceeding child maxInputTokens (10) is refused before upstream.
	bigPrompt := strings.Repeat("word ", 40) // 200 bytes -> ~50 tokens > 10
	rec := doGateRequest(gate.Handler(), http.MethodPost, "/openai/v1/chat/completions",
		`{"model":"gpt-5","messages":[{"role":"user","content":"`+bigPrompt+`"}]}`,
		map[string]string{"Authorization": "Bearer " + token})
	if rec.Code != http.StatusTooManyRequests || !strings.Contains(rec.Body.String(), "child_input_tokens") {
		t.Fatalf("oversized input = %d %s, want 429 child_input_tokens", rec.Code, rec.Body.String())
	}

	// 2. Request asking for max_tokens > child maxOutputTokens (5) is refused before upstream.
	rec = doGateRequest(gate.Handler(), http.MethodPost, "/openai/v1/chat/completions",
		`{"model":"gpt-5","max_tokens":10}`,
		map[string]string{"Authorization": "Bearer " + token})
	if rec.Code != http.StatusTooManyRequests || !strings.Contains(rec.Body.String(), "child_output_tokens") {
		t.Fatalf("oversized max_tokens = %d %s, want 429 child_output_tokens", rec.Code, rec.Body.String())
	}

	// 3. Upstream 500 error releases the reservation so the next request succeeds.
	rec = doGateRequest(gate.Handler(), http.MethodPost, "/openai/v1/chat/completions",
		`{"model":"gpt-5","max_tokens":5}`,
		map[string]string{"Authorization": "Bearer " + token})
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500 from upstream, got %d %s", rec.Code, rec.Body.String())
	}
	failNext.Store(false)
	rec = doGateRequest(gate.Handler(), http.MethodPost, "/openai/v1/chat/completions",
		`{"model":"gpt-5","max_tokens":5}`,
		map[string]string{"Authorization": "Bearer " + token})
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 after failed request released reservation, got %d %s", rec.Code, rec.Body.String())
	}

	// 4. Settled actual output tokens = 2 (not reserved 5), so another max_tokens=3 request fits (2+3 <= 5).
	rec = doGateRequest(gate.Handler(), http.MethodPost, "/openai/v1/chat/completions",
		`{"model":"gpt-5","max_tokens":3}`,
		map[string]string{"Authorization": "Bearer " + token})
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 for max_tokens=3 after settling 2 output tokens, got %d %s", rec.Code, rec.Body.String())
	}
}
