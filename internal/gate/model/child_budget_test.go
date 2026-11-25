package model

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
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
