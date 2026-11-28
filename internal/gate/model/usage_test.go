package model

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/shadewright-labs/armory/modelusage"
	"github.com/shadewright-labs/sablecourt/internal/domain"
)

// TestModelGateAccountsAnthropicCacheTokensAndOpenAIShapes verifies that recorded-shape
// Anthropic JSON/SSE responses with prompt caching and OpenAI Chat/Responses JSON/SSE
// responses (with cached_tokens details) are accounted accurately in budget totals,
// cost, and audit detail.
func TestModelGateAccountsAnthropicCacheTokensAndOpenAIShapes(t *testing.T) {
	tests := []struct {
		name           string
		route          string
		reqBody        string
		headers        func(tok string) map[string]string
		contentType    string
		respBody       string
		wantIn         int64
		wantCacheWrite int64
		wantCacheRead  int64
		wantTotalIn    int64
		wantOut        int64
		wantCost       int64
	}{
		{
			name:        "anthropic json with cache creation and cache read",
			route:       "/anthropic/v1/messages",
			reqBody:     `{"model":"claude-sonnet-4-5","max_tokens":200}`,
			headers:     func(tok string) map[string]string { return map[string]string{"x-api-key": tok} },
			contentType: "application/json",
			// 50 * $3.00 + 240 * $3.75 + 1800 * $0.30 + 120 * $15.00 = 150 + 900 + 540 + 1800 = 3390 microUSD
			respBody: `{
				"id": "msg_01fixture",
				"type": "message",
				"role": "assistant",
				"model": "claude-sonnet-4-5",
				"content": [{"type": "text", "text": "ok"}],
				"stop_reason": "end_turn",
				"usage": {
					"input_tokens": 50,
					"cache_creation_input_tokens": 240,
					"cache_read_input_tokens": 1800,
					"output_tokens": 120
				}
			}`,
			wantIn:         50,
			wantCacheWrite: 240,
			wantCacheRead:  1800,
			wantTotalIn:    2090,
			wantOut:        120,
			wantCost:       3390,
		},
		{
			name:        "anthropic sse with cache creation and cache read",
			route:       "/anthropic/v1/messages",
			reqBody:     `{"model":"claude-sonnet-4-5","max_tokens":200,"stream":true}`,
			headers:     func(tok string) map[string]string { return map[string]string{"x-api-key": tok} },
			contentType: "text/event-stream",
			// 40 * $3.00 + 400 * $3.75 + 1000 * $0.30 + 90 * $15.00 = 120 + 1500 + 300 + 1350 = 3270 microUSD
			respBody: "" +
				"event: message_start\n" +
				`data: {"type":"message_start","message":{"id":"msg_02fixture","type":"message","role":"assistant","model":"claude-sonnet-4-5","usage":{"input_tokens":40,"cache_creation_input_tokens":400,"cache_read_input_tokens":1000,"output_tokens":1}}}` + "\n\n" +
				"event: content_block_delta\n" +
				`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}` + "\n\n" +
				"event: message_delta\n" +
				`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":90}}` + "\n\n" +
				"event: message_stop\n" +
				`data: {"type":"message_stop"}` + "\n\n",
			wantIn:         40,
			wantCacheWrite: 400,
			wantCacheRead:  1000,
			wantTotalIn:    1440,
			wantOut:        90,
			wantCost:       3270,
		},
		{
			name:        "openai chat json with cached_tokens detail",
			route:       "/openai/v1/chat/completions",
			reqBody:     `{"model":"gpt-5","max_tokens":300}`,
			headers:     func(tok string) map[string]string { return map[string]string{"Authorization": "Bearer " + tok} },
			contentType: "application/json",
			// prompt_tokens=1200 includes cached_tokens=1000 -> uncached=200
			// 200 * $2.50 + 1000 * $1.25 + 300 * $10.00 = 500 + 1250 + 3000 = 4750 microUSD
			respBody: `{
				"id": "chatcmpl-fixture1",
				"object": "chat.completion",
				"model": "gpt-5",
				"choices": [{"index": 0, "message": {"role": "assistant", "content": "ok"}, "finish_reason": "stop"}],
				"usage": {
					"prompt_tokens": 1200,
					"completion_tokens": 300,
					"total_tokens": 1500,
					"prompt_tokens_details": {"cached_tokens": 1000}
				}
			}`,
			wantIn:         200,
			wantCacheWrite: 0,
			wantCacheRead:  1000,
			wantTotalIn:    1200,
			wantOut:        300,
			wantCost:       4750,
		},
		{
			name:        "openai chat sse with cached_tokens detail",
			route:       "/openai/v1/chat/completions",
			reqBody:     `{"model":"gpt-5","max_tokens":300,"stream":true}`,
			headers:     func(tok string) map[string]string { return map[string]string{"Authorization": "Bearer " + tok} },
			contentType: "text/event-stream",
			// prompt_tokens=600, cached_tokens=400 -> uncached=200
			// 200 * $2.50 + 400 * $1.25 + 50 * $10.00 = 500 + 500 + 500 = 1500 microUSD
			respBody: "" +
				`data: {"id":"chatcmpl-fixture2","object":"chat.completion.chunk","model":"gpt-5","choices":[{"index":0,"delta":{"content":"hi"}}]}` + "\n\n" +
				`data: {"id":"chatcmpl-fixture2","object":"chat.completion.chunk","model":"gpt-5","choices":[],"usage":{"prompt_tokens":600,"completion_tokens":50,"total_tokens":650,"prompt_tokens_details":{"cached_tokens":400}}}` + "\n\n" +
				"data: [DONE]\n\n",
			wantIn:         200,
			wantCacheWrite: 0,
			wantCacheRead:  400,
			wantTotalIn:    600,
			wantOut:        50,
			wantCost:       1500,
		},
		{
			name:        "openai responses json with cached_tokens and cache_write_tokens",
			route:       "/openai/v1/responses",
			reqBody:     `{"model":"gpt-5","max_output_tokens":100}`,
			headers:     func(tok string) map[string]string { return map[string]string{"Authorization": "Bearer " + tok} },
			contentType: "application/json",
			// input_tokens=1000 includes cached=600 and cache_write=200 -> uncached=200
			// openai has no separate cache_write rate, so cache_write is priced at input rate ($2.50):
			// 200 * $2.50 + 200 * $2.50 + 600 * $1.25 + 80 * $10.00 = 500 + 500 + 750 + 800 = 2550 microUSD
			respBody: `{
				"id": "resp_fixture1",
				"object": "response",
				"status": "completed",
				"model": "gpt-5",
				"usage": {
					"input_tokens": 1000,
					"output_tokens": 80,
					"total_tokens": 1080,
					"input_tokens_details": {
						"cached_tokens": 600,
						"cache_write_tokens": 200
					}
				}
			}`,
			wantIn:         200,
			wantCacheWrite: 200,
			wantCacheRead:  600,
			wantTotalIn:    1000,
			wantOut:        80,
			wantCost:       2550,
		},
		{
			name:        "openai responses sse with response.completed usage",
			route:       "/openai/v1/responses",
			reqBody:     `{"model":"gpt-5","max_output_tokens":100,"stream":true}`,
			headers:     func(tok string) map[string]string { return map[string]string{"Authorization": "Bearer " + tok} },
			contentType: "text/event-stream",
			// input_tokens=800 includes cached=500 -> uncached=300
			// 300 * $2.50 + 500 * $1.25 + 60 * $10.00 = 750 + 625 + 600 = 1975 microUSD
			respBody: "" +
				"event: response.output_text.delta\n" +
				`data: {"type":"response.output_text.delta","delta":"ok"}` + "\n\n" +
				"event: response.completed\n" +
				`data: {"type":"response.completed","response":{"id":"resp_fixture2","status":"completed","model":"gpt-5","usage":{"input_tokens":800,"output_tokens":60,"input_tokens_details":{"cached_tokens":500}}}}` + "\n\n",
			wantIn:         300,
			wantCacheWrite: 0,
			wantCacheRead:  500,
			wantTotalIn:    800,
			wantOut:        60,
			wantCost:       1975,
		},
	}

	for i, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			upstream := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", tc.contentType)
				_, _ = w.Write([]byte(tc.respBody))
			})
			gate, st := setupGateTest(t, map[string]UpstreamConfig{
				"openai":    {BaseURL: "https://openai.test", SecretName: "openai_api_key", RequireSecret: true},
				"anthropic": {BaseURL: "https://anthropic.test", SecretName: "anthropic_api_key", RequireSecret: true, AnthropicVer: "2023-06-01"},
				"local":     {BaseURL: "http://127.0.0.1:11434", RequireSecret: false},
			}, handlerTransport(upstream))
			b := gate.bundle.Budgets["agent-budget"]
			b.Spec.PerTask.MaxInputTokens = 10000
			gate.bundle.Budgets["agent-budget"] = b

			taskID := fmt.Sprintf("task-usage-%d", i)
			if err := st.CreateTask(ctx, domain.Task{
				ID: taskID, IdempotencyKey: taskID, WorkloadName: "ai-agent", State: domain.TaskStateWorking,
			}); err != nil {
				t.Fatalf("CreateTask: %v", err)
			}
			tok, err := MintTaskToken(ctx, st, taskID, "ai-agent", time.Hour)
			if err != nil {
				t.Fatalf("MintTaskToken: %v", err)
			}

			rec := doGateRequest(gate.Handler(), http.MethodPost, tc.route, tc.reqBody, tc.headers(tok))
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d body = %s, want 200", rec.Code, rec.Body.String())
			}

			totals, err := st.TaskBudgetTotals(ctx, taskID)
			if err != nil {
				t.Fatalf("TaskBudgetTotals: %v", err)
			}
			if totals.InputTokens != tc.wantIn || totals.CacheWriteTokens != tc.wantCacheWrite || totals.CacheReadTokens != tc.wantCacheRead || totals.OutputTokens != tc.wantOut {
				t.Errorf("totals = %+v, want in=%d cw=%d cr=%d out=%d", totals, tc.wantIn, tc.wantCacheWrite, tc.wantCacheRead, tc.wantOut)
			}
			if totals.TotalInputTokens() != tc.wantTotalIn {
				t.Errorf("TotalInputTokens = %d, want %d", totals.TotalInputTokens(), tc.wantTotalIn)
			}
			if totals.CostMicroUSD != tc.wantCost {
				t.Errorf("CostMicroUSD = %d, want %d", totals.CostMicroUSD, tc.wantCost)
			}

			audits, err := st.ListAudit(ctx, 1)
			if err != nil || len(audits) != 1 || audits[0].Outcome != "allowed" {
				t.Fatalf("audit = %+v, err=%v", audits, err)
			}
			var detail struct {
				InputTokens      int64 `json:"input_tokens"`
				CacheWriteTokens int64 `json:"cache_write_tokens"`
				CacheReadTokens  int64 `json:"cache_read_tokens"`
				OutputTokens     int64 `json:"output_tokens"`
				CostMicroUSD     int64 `json:"cost_microusd"`
			}
			if err := json.Unmarshal([]byte(audits[0].DetailJSON), &detail); err != nil {
				t.Fatalf("unmarshal audit detail %q: %v", audits[0].DetailJSON, err)
			}
			if detail.InputTokens != tc.wantIn || detail.CacheWriteTokens != tc.wantCacheWrite ||
				detail.CacheReadTokens != tc.wantCacheRead || detail.OutputTokens != tc.wantOut ||
				detail.CostMicroUSD != tc.wantCost {
				t.Errorf("audit detail = %+v, want in=%d cw=%d cr=%d out=%d cost=%d",
					detail, tc.wantIn, tc.wantCacheWrite, tc.wantCacheRead, tc.wantOut, tc.wantCost)
			}
		})
	}
}

func TestParseUsageOutcomesAndFailClosedSettlement(t *testing.T) {
	tests := []struct {
		name           string
		provider       string
		upstreamPath   string
		contentType    string
		body           []byte
		wantOutcome    usageOutcome
		wantErrTarget  error
		wantPartialErr bool
		wantTrailErr   bool
		wantIn         int64
		wantCacheWrite int64
		wantCacheRead  int64
		wantOut        int64
		wantCost       int64
	}{
		{
			name:          "no usage reported json",
			provider:      "anthropic",
			upstreamPath:  "/v1/messages",
			contentType:   "application/json",
			body:          []byte(`{"id":"msg_1","type":"message","content":[]}`),
			wantOutcome:   usageOutcomeNone,
			wantErrTarget: modelusage.ErrNoUsage,
			// Falls back to reserve priced at Anthropic 1h reserve rate (100 * 6.00 + 50 * 15 = 1350)
			wantIn:   100,
			wantOut:  50,
			wantCost: 1350,
		},
		{
			name:          "no usage reported sse",
			provider:      "openai",
			upstreamPath:  "/v1/chat/completions",
			contentType:   "text/event-stream",
			body:          []byte("data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\ndata: [DONE]\n\n"),
			wantOutcome:   usageOutcomeNone,
			wantErrTarget: modelusage.ErrNoUsage,
			// 100 * 2.50 + 50 * 10.00 = 750
			wantIn:   100,
			wantOut:  50,
			wantCost: 750,
		},
		{
			name:          "malformed json body",
			provider:      "anthropic",
			upstreamPath:  "/v1/messages",
			contentType:   "application/json",
			body:          []byte(`{"usage":{"input_tokens":10,"output_tokens":5}`),
			wantOutcome:   usageOutcomeNone,
			wantErrTarget: modelusage.ErrMalformed,
			wantIn:        100,
			wantOut:       50,
			wantCost:      1350,
		},
		{
			name:          "duplicate json key is malformed",
			provider:      "openai",
			upstreamPath:  "/v1/chat/completions",
			contentType:   "application/json",
			body:          []byte(`{"usage":{"prompt_tokens":10,"prompt_tokens":999,"completion_tokens":5}}`),
			wantOutcome:   usageOutcomeNone,
			wantErrTarget: modelusage.ErrMalformed,
			wantIn:        100,
			wantOut:       50,
			wantCost:      750,
		},
		{
			name:          "trailing data in json body is malformed",
			provider:      "anthropic",
			upstreamPath:  "/v1/messages",
			contentType:   "application/json",
			body:          []byte(`{"usage":{"input_tokens":10,"output_tokens":5}}{"extra":1}`),
			wantOutcome:   usageOutcomeNone,
			wantErrTarget: modelusage.ErrMalformed,
			wantIn:        100,
			wantOut:       50,
			wantCost:      1350,
		},
		{
			name:          "out of range cached tokens exceeding prompt tokens",
			provider:      "openai",
			upstreamPath:  "/v1/chat/completions",
			contentType:   "application/json",
			body:          []byte(`{"usage":{"prompt_tokens":10,"completion_tokens":5,"prompt_tokens_details":{"cached_tokens":20}}}`),
			wantOutcome:   usageOutcomeNone,
			wantErrTarget: modelusage.ErrOutOfRange,
			wantIn:        100,
			wantOut:       50,
			wantCost:      750,
		},
		{
			// Regression guard: armory rejects chat total_tokens below the sum of parts.
			name:          "out of range chat total_tokens below parts",
			provider:      "openai",
			upstreamPath:  "/v1/chat/completions",
			contentType:   "application/json",
			body:          []byte(`{"id":"chatcmpl-test0003","object":"chat.completion","model":"gpt-example","choices":[{"index":0,"message":{"role":"assistant","content":"The pod is out of memory."},"finish_reason":"stop"}],"usage":{"prompt_tokens":19,"completion_tokens":10,"total_tokens":20}}`),
			wantOutcome:   usageOutcomeNone,
			wantErrTarget: modelusage.ErrOutOfRange,
			wantIn:        100,
			wantOut:       50,
			wantCost:      750,
		},
		{
			// Regression guard: armory rejects chat SSE total_tokens below the sum of parts.
			name:          "out of range chat stream total_tokens below parts",
			provider:      "openai",
			upstreamPath:  "/v1/chat/completions",
			contentType:   "text/event-stream",
			body:          []byte("data: {\"id\":\"chatcmpl-test0003\",\"object\":\"chat.completion.chunk\",\"model\":\"gpt-example\",\"choices\":[],\"usage\":{\"prompt_tokens\":19,\"completion_tokens\":10,\"total_tokens\":20}}\n\ndata: [DONE]\n\n"),
			wantOutcome:   usageOutcomeNone,
			wantErrTarget: modelusage.ErrOutOfRange,
			wantIn:        100,
			wantOut:       50,
			wantCost:      750,
		},
		{
			// Regression guard: armory fails closed on non-terminal Responses status in_progress.
			name:          "non-terminal responses status in_progress fails closed as truncated",
			provider:      "openai",
			upstreamPath:  "/v1/responses",
			contentType:   "application/json",
			body:          []byte(`{"id":"resp_test0002","object":"response","status":"in_progress","error":null,"incomplete_details":null,"model":"gpt-example","output":[],"usage":{"input_tokens":75,"output_tokens":12,"total_tokens":87}}`),
			wantOutcome:   usageOutcomeNone,
			wantErrTarget: modelusage.ErrTruncated,
			wantIn:        100,
			wantOut:       50,
			wantCost:      750,
		},
		{
			// Regression guard: armory fails closed on Responses SSE with non-terminal status in_progress.
			name:          "non-terminal responses stream status in_progress fails closed as truncated",
			provider:      "openai",
			upstreamPath:  "/v1/responses",
			contentType:   "text/event-stream",
			body:          []byte("event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_test0002\",\"object\":\"response\",\"status\":\"in_progress\",\"error\":null,\"incomplete_details\":null,\"model\":\"gpt-example\",\"output\":[],\"usage\":{\"input_tokens\":75,\"output_tokens\":12,\"total_tokens\":87}}}\n\n"),
			wantOutcome:   usageOutcomeNone,
			wantErrTarget: modelusage.ErrTruncated,
			wantIn:        100,
			wantOut:       50,
			wantCost:      750,
		},
		{
			name:          "oversized json body returns LimitError",
			provider:      "anthropic",
			upstreamPath:  "/v1/messages",
			contentType:   "application/json",
			body:          []byte(`{"pad":"` + strings.Repeat("a", maxCapturedResponseBytes) + `","usage":{"input_tokens":10,"output_tokens":5}}`),
			wantOutcome:   usageOutcomeNone,
			wantErrTarget: modelusage.ErrTooLarge,
			wantIn:        100,
			wantOut:       50,
			wantCost:      1350,
		},
		{
			name:          "oversized sse line returns LineTooLong",
			provider:      "openai",
			upstreamPath:  "/v1/chat/completions",
			contentType:   "text/event-stream",
			body:          []byte("data: " + strings.Repeat("x", maxUsageLineBytes+1) + "\n\n"),
			wantOutcome:   usageOutcomeNone,
			wantErrTarget: modelusage.ErrLineTooLong,
			wantIn:        100,
			wantOut:       50,
			wantCost:      750,
		},
		{
			name:           "partial anthropic stream truncated below reserve charges reserve floor",
			provider:       "anthropic",
			upstreamPath:   "/v1/messages",
			contentType:    "text/event-stream",
			body:           []byte("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":20,\"output_tokens\":1}}}\n\n"),
			wantOutcome:    usageOutcomePartial,
			wantErrTarget:  modelusage.ErrTruncated,
			wantPartialErr: true,
			// Partial (20 in, 1 out) < reserve (100 in, 50 out) -> floored at reserve tokens & reserve cost (1350)
			wantIn:   100,
			wantOut:  50,
			wantCost: 1350,
		},
		{
			name:         "partial anthropic stream above reserve bills without cache read discount",
			provider:     "anthropic",
			upstreamPath: "/v1/messages",
			contentType:  "text/event-stream",
			body: []byte("" +
				"event: message_start\n" +
				`data: {"type":"message_start","message":{"usage":{"input_tokens":50,"cache_creation_input_tokens":200,"cache_read_input_tokens":1000,"output_tokens":1}}}` + "\n\n" +
				"event: message_delta\n" +
				`data: {"type":"message_delta","usage":{"output_tokens":80}}` + "\n\n"),
			wantOutcome:    usageOutcomePartial,
			wantErrTarget:  modelusage.ErrTruncated,
			wantPartialErr: true,
			// Total input = 1250 (> reserve 100); cacheWrite=200 kept at $3.75, remaining 1050 billed at $3.00 (no cache-read discount)
			// 1050 * 3.00 + 200 * 3.75 + 80 * 15.00 = 3150 + 750 + 1200 = 5100
			wantIn:         1050,
			wantCacheWrite: 200,
			wantCacheRead:  0,
			wantOut:        80,
			wantCost:       5100,
		},
		{
			name:         "partial inconsistent stream cannot drop below reserve",
			provider:     "openai",
			upstreamPath: "/v1/chat/completions",
			contentType:  "text/event-stream",
			body: []byte("" +
				`data: {"usage":{"prompt_tokens":300,"completion_tokens":40}}` + "\n\n" +
				`data: {"usage":{"prompt_tokens":10,"completion_tokens":5}}` + "\n\n" +
				"data: [DONE]\n\n"),
			wantOutcome:    usageOutcomePartial,
			wantErrTarget:  modelusage.ErrInconsistent,
			wantPartialErr: true,
			// Inconsistent drop to (10, 5) is floored by reserve (100, 50) -> 750 microUSD
			wantIn:   100,
			wantOut:  50,
			wantCost: 750,
		},
		{
			name:         "trailing data after final sse event settles exact final usage",
			provider:     "openai",
			upstreamPath: "/v1/chat/completions",
			contentType:  "text/event-stream",
			body: []byte("" +
				`data: {"usage":{"prompt_tokens":400,"completion_tokens":20,"prompt_tokens_details":{"cached_tokens":300}}}` + "\n\n" +
				"data: [DONE]\n\n" +
				`data: {"usage":{"prompt_tokens":1,"completion_tokens":1}}` + "\n\n"),
			wantOutcome:   usageOutcomeTrailing,
			wantErrTarget: modelusage.ErrMalformed,
			wantTrailErr:  true,
			// Exact final usage before [DONE]: uncached=100, cacheRead=300, out=20
			// 100 * 2.50 + 300 * 1.25 + 20 * 10.00 = 250 + 375 + 200 = 825 microUSD
			wantIn:        100,
			wantCacheRead: 300,
			wantOut:       20,
			wantCost:      825,
		},
		{
			name:         "all zero valid usage settles reserved estimate",
			provider:     "anthropic",
			upstreamPath: "/v1/messages",
			contentType:  "application/json",
			body:         []byte(`{"usage":{"input_tokens":0,"output_tokens":0}}`),
			wantOutcome:  usageOutcomeFinal,
			wantIn:       100,
			wantOut:      50,
			wantCost:     1350,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			parsed := parseUsage(tc.provider, tc.upstreamPath, tc.body, tc.contentType)
			if parsed.Outcome != tc.wantOutcome {
				t.Fatalf("outcome = %v, want %v (err=%v)", parsed.Outcome, tc.wantOutcome, parsed.Err)
			}
			if tc.wantErrTarget != nil && !errors.Is(parsed.Err, tc.wantErrTarget) {
				t.Fatalf("err = %v, want errors.Is(%v)", parsed.Err, tc.wantErrTarget)
			}
			if tc.wantPartialErr {
				if _, ok := errors.AsType[*modelusage.PartialError](parsed.Err); !ok {
					t.Fatalf("err = %T (%v), want *modelusage.PartialError", parsed.Err, parsed.Err)
				}
			}
			if tc.wantTrailErr {
				if _, ok := errors.AsType[*modelusage.TrailingError](parsed.Err); !ok {
					t.Fatalf("err = %T (%v), want *modelusage.TrailingError", parsed.Err, parsed.Err)
				}
			}
			reserve := domain.BudgetTotals{
				InputTokens:  100,
				OutputTokens: 50,
				CostMicroUSD: estimateReserveMicroUSD(tc.provider, "model-x", 100, 50, nil),
			}
			settled, cost := settleUsageForEntry(tc.provider, "model-x", parsed, reserve, nil)
			if settled.InputTokens != tc.wantIn || settled.CacheWriteTokens != tc.wantCacheWrite ||
				settled.CacheReadTokens != tc.wantCacheRead || settled.OutputTokens != tc.wantOut {
				t.Errorf("settled = %+v, want in=%d cw=%d cr=%d out=%d",
					settled, tc.wantIn, tc.wantCacheWrite, tc.wantCacheRead, tc.wantOut)
			}
			if cost != tc.wantCost {
				t.Errorf("cost = %d, want %d", cost, tc.wantCost)
			}
		})
	}
}

func TestPricingCacheRatesAndFallbackToInputRate(t *testing.T) {
	tests := []struct {
		name     string
		provider string
		model    string
		usage    Usage
		custom   map[string]PricePerMillionTokens
		want     int64
	}{
		{
			name:     "anthropic explicit cache rates",
			provider: "anthropic",
			usage:    Usage{InputTokens: 100, CacheWriteTokens: 200, CacheReadTokens: 1000, OutputTokens: 50},
			// 100*3 + 200*3.75 + 1000*0.30 + 50*15 = 300 + 750 + 300 + 750 = 2100
			want: 2100,
		},
		{
			name:     "openai cache write falls back to input rate",
			provider: "openai",
			usage:    Usage{InputTokens: 100, CacheWriteTokens: 200, CacheReadTokens: 400, OutputTokens: 10},
			// 100*2.5 + 200*2.5 + 400*1.25 + 10*10 = 250 + 500 + 500 + 100 = 1350
			want: 1350,
		},
		{
			name:     "gemini cache tokens fall back to input rate never zero",
			provider: "gemini",
			usage:    Usage{InputTokens: 0, CacheWriteTokens: 400, CacheReadTokens: 400, OutputTokens: 0},
			// 800 * 1.25 = 1000
			want: 1000,
		},
		{
			name:     "unknown provider cache tokens fall back to conservative input rate",
			provider: "other",
			usage:    Usage{CacheWriteTokens: 100, CacheReadTokens: 100},
			// 200 * 5.00 = 1000
			want: 1000,
		},
		{
			name:     "custom model without cache rates falls back to custom input rate",
			provider: "anthropic",
			model:    "custom-model",
			usage:    Usage{InputTokens: 10, CacheWriteTokens: 20, CacheReadTokens: 30, OutputTokens: 5},
			custom: map[string]PricePerMillionTokens{
				"custom-model": {InputUSD: 4.00, OutputUSD: 8.00},
			},
			// (10 + 20 + 30) * 4 + 5 * 8 = 240 + 40 = 280
			want: 280,
		},
		{
			name:     "custom model with explicit cache rates uses them",
			provider: "anthropic",
			model:    "custom-model",
			usage:    Usage{InputTokens: 10, CacheWriteTokens: 20, CacheReadTokens: 30, OutputTokens: 5},
			custom: map[string]PricePerMillionTokens{
				"custom-model": {InputUSD: 4.00, OutputUSD: 8.00, CacheWriteUSD: 5.00, CacheReadUSD: 0.40},
			},
			// 10*4 + 20*5 + 30*0.4 + 5*8 = 40 + 100 + 12 + 40 = 192
			want: 192,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := EstimateUsageMicroUSD(tc.provider, tc.model, tc.usage, tc.custom); got != tc.want {
				t.Fatalf("EstimateUsageMicroUSD = %d, want %d", got, tc.want)
			}
		})
	}

	// Pre-call reservation must never be smaller than uncached input pricing, and
	// for Anthropic must cover the higher cache write rate.
	body := []byte(`{"model":"claude-sonnet-4-5","messages":[{"role":"user","content":"` + strings.Repeat("a", 400) + `"}],"max_tokens":10}`)
	res := estimateRequestReserve("anthropic", "claude-sonnet-4-5", body, nil)
	uncachedOnly := EstimateMicroUSD("anthropic", "claude-sonnet-4-5", res.InputTokens, res.OutputTokens, nil)
	if res.CostMicroUSD <= uncachedOnly {
		t.Fatalf("anthropic reserve cost = %d, want > uncached-only cost %d", res.CostMicroUSD, uncachedOnly)
	}
}

func TestModelGateEnforcesInputBudgetIncludingCacheTokens(t *testing.T) {
	ctx := context.Background()
	upstream := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// Small uncached input (50), large cache write (250) + cache read (800) = 1100 total input tokens.
		_, _ = w.Write([]byte(`{"model":"claude-sonnet-4-5","usage":{"input_tokens":50,"cache_creation_input_tokens":250,"cache_read_input_tokens":800,"output_tokens":10}}`))
	})
	gate, st := setupGateTest(t, map[string]UpstreamConfig{
		"anthropic": {BaseURL: "https://anthropic.test", SecretName: "anthropic_api_key", RequireSecret: true, AnthropicVer: "2023-06-01"},
	}, handlerTransport(upstream))
	// agent-budget has MaxInputTokens = 1000.

	if err := st.CreateTask(ctx, domain.Task{
		ID: "task-cache-cap", IdempotencyKey: "task-cache-cap", WorkloadName: "ai-agent", State: domain.TaskStateWorking,
	}); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	tok, err := MintTaskToken(ctx, st, "task-cache-cap", "ai-agent", time.Hour)
	if err != nil {
		t.Fatalf("MintTaskToken: %v", err)
	}

	rec := doGateRequest(gate.Handler(), http.MethodPost, "/anthropic/v1/messages", `{"model":"claude-sonnet-4-5","max_tokens":10}`, map[string]string{"x-api-key": tok})
	if rec.Code != http.StatusOK {
		t.Fatalf("first call status = %d body = %s, want 200", rec.Code, rec.Body.String())
	}

	// Second call must be refused with per_task_input_tokens because 50 + 250 + 800 = 1100 >= 1000.
	rec = doGateRequest(gate.Handler(), http.MethodPost, "/anthropic/v1/messages", `{"model":"claude-sonnet-4-5","max_tokens":10}`, map[string]string{"x-api-key": tok})
	if rec.Code != http.StatusTooManyRequests || !strings.Contains(rec.Body.String(), "per_task_input_tokens") {
		t.Fatalf("second call = %d %s, want 429 per_task_input_tokens", rec.Code, rec.Body.String())
	}
}

// pipeListener is an in-memory net.Listener backed by net.Pipe so tests can
// exercise real net/http.Server and net/http.Transport compression handling
// without opening loopback sockets.
type pipeListener struct {
	ch     chan net.Conn
	closed chan struct{}
}

func newPipeListener() *pipeListener {
	return &pipeListener{
		ch:     make(chan net.Conn, 16),
		closed: make(chan struct{}),
	}
}

func (l *pipeListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.ch:
		return c, nil
	case <-l.closed:
		return nil, net.ErrClosed
	}
}

func (l *pipeListener) Close() error {
	select {
	case <-l.closed:
	default:
		close(l.closed)
	}
	return nil
}

func (l *pipeListener) Addr() net.Addr { return pipeAddr{} }

func (l *pipeListener) DialContext(ctx context.Context, _, _ string) (net.Conn, error) {
	clientConn, serverConn := net.Pipe()
	select {
	case l.ch <- serverConn:
		return clientConn, nil
	case <-l.closed:
		_ = clientConn.Close()
		_ = serverConn.Close()
		return nil, net.ErrClosed
	case <-ctx.Done():
		_ = clientConn.Close()
		_ = serverConn.Close()
		return nil, ctx.Err()
	}
}

type pipeAddr struct{}

func (pipeAddr) Network() string { return "pipe" }
func (pipeAddr) String() string  { return "pipe" }

func newPipeHTTPClient(t *testing.T, h http.Handler) *http.Client {
	t.Helper()
	ln := newPipeListener()
	srv := &http.Server{Handler: h, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		_ = srv.Serve(ln)
	}()
	t.Cleanup(func() {
		_ = srv.Close()
		_ = ln.Close()
	})
	tr := &http.Transport{
		DialContext: ln.DialContext,
	}
	t.Cleanup(tr.CloseIdleConnections)
	return &http.Client{Transport: tr}
}

func TestModelGateDecompressesGzipUpstreamReplies(t *testing.T) {
	tests := []struct {
		name           string
		route          string
		reqBody        string
		acceptEncoding string
		headers        func(tok string) map[string]string
		contentType    string
		respBody       string
		wantIn         int64
		wantCacheWrite int64
		wantCacheRead  int64
		wantOut        int64
		wantCost       int64
	}{
		{
			name:           "openai chat json gzip",
			route:          "/openai/v1/chat/completions",
			reqBody:        `{"model":"gpt-5"}`,
			acceptEncoding: "gzip",
			headers:        func(tok string) map[string]string { return map[string]string{"Authorization": "Bearer " + tok} },
			contentType:    "application/json",
			// uncached=200 ($2.50) + cacheRead=300 ($1.25) + out=40 ($10.00) = 500 + 375 + 400 = 1275
			respBody:      `{"model":"gpt-5","usage":{"prompt_tokens":500,"completion_tokens":40,"prompt_tokens_details":{"cached_tokens":300}}}`,
			wantIn:        200,
			wantCacheRead: 300,
			wantOut:       40,
			wantCost:      1275,
		},
		{
			name:           "openai chat sse gzip",
			route:          "/openai/v1/chat/completions",
			reqBody:        `{"model":"gpt-5","stream":true}`,
			acceptEncoding: "gzip",
			headers:        func(tok string) map[string]string { return map[string]string{"Authorization": "Bearer " + tok} },
			contentType:    "text/event-stream",
			// uncached=120 ($2.50) + cacheRead=80 ($1.25) + out=30 ($10.00) = 300 + 100 + 300 = 700
			respBody: "" +
				`data: {"model":"gpt-5","choices":[{"delta":{"content":"ok"}}]}` + "\n\n" +
				`data: {"model":"gpt-5","choices":[],"usage":{"prompt_tokens":200,"completion_tokens":30,"prompt_tokens_details":{"cached_tokens":80}}}` + "\n\n" +
				"data: [DONE]\n\n",
			wantIn:        120,
			wantCacheRead: 80,
			wantOut:       30,
			wantCost:      700,
		},
		{
			name:           "openai responses json gzip",
			route:          "/openai/v1/responses",
			reqBody:        `{"model":"gpt-5"}`,
			acceptEncoding: "br, gzip",
			headers:        func(tok string) map[string]string { return map[string]string{"Authorization": "Bearer " + tok} },
			contentType:    "application/json",
			// uncached=150 ($2.50) + cacheRead=100 ($1.25) + out=25 ($10.00) = 375 + 125 + 250 = 750
			respBody:      `{"id":"resp_gz1","status":"completed","model":"gpt-5","usage":{"input_tokens":250,"output_tokens":25,"input_tokens_details":{"cached_tokens":100}}}`,
			wantIn:        150,
			wantCacheRead: 100,
			wantOut:       25,
			wantCost:      750,
		},
		{
			name:           "openai responses sse gzip",
			route:          "/openai/v1/responses",
			reqBody:        `{"model":"gpt-5","stream":true}`,
			acceptEncoding: "gzip",
			headers:        func(tok string) map[string]string { return map[string]string{"Authorization": "Bearer " + tok} },
			contentType:    "text/event-stream",
			// uncached=100 ($2.50) + cacheRead=200 ($1.25) + out=50 ($10.00) = 250 + 250 + 500 = 1000
			respBody: "" +
				"event: response.output_text.delta\n" +
				`data: {"type":"response.output_text.delta","delta":"hi"}` + "\n\n" +
				"event: response.completed\n" +
				`data: {"type":"response.completed","response":{"id":"resp_gz2","status":"completed","model":"gpt-5","usage":{"input_tokens":300,"output_tokens":50,"input_tokens_details":{"cached_tokens":200}}}}` + "\n\n",
			wantIn:        100,
			wantCacheRead: 200,
			wantOut:       50,
			wantCost:      1000,
		},
		{
			name:           "anthropic json gzip",
			route:          "/anthropic/v1/messages",
			reqBody:        `{"model":"claude-sonnet-4-5"}`,
			acceptEncoding: "gzip",
			headers:        func(tok string) map[string]string { return map[string]string{"x-api-key": tok} },
			contentType:    "application/json",
			// uncached=60 ($3.00) + cacheWrite=100 ($3.75) + cacheRead=200 ($0.30) + out=40 ($15.00) = 180 + 375 + 60 + 600 = 1215
			respBody:       `{"id":"msg_gz1","type":"message","model":"claude-sonnet-4-5","usage":{"input_tokens":60,"cache_creation_input_tokens":100,"cache_read_input_tokens":200,"output_tokens":40}}`,
			wantIn:         60,
			wantCacheWrite: 100,
			wantCacheRead:  200,
			wantOut:        40,
			wantCost:       1215,
		},
		{
			name:           "anthropic sse gzip",
			route:          "/anthropic/v1/messages",
			reqBody:        `{"model":"claude-sonnet-4-5","stream":true}`,
			acceptEncoding: "gzip",
			headers:        func(tok string) map[string]string { return map[string]string{"x-api-key": tok} },
			contentType:    "text/event-stream",
			// uncached=40 ($3.00) + cacheWrite=80 ($3.75) + cacheRead=100 ($0.30) + out=20 ($15.00) = 120 + 300 + 30 + 300 = 750
			respBody: "" +
				"event: message_start\n" +
				`data: {"type":"message_start","message":{"id":"msg_gz2","model":"claude-sonnet-4-5","usage":{"input_tokens":40,"cache_creation_input_tokens":80,"cache_read_input_tokens":100,"output_tokens":1}}}` + "\n\n" +
				"event: message_delta\n" +
				`data: {"type":"message_delta","usage":{"output_tokens":20}}` + "\n\n" +
				"event: message_stop\n" +
				`data: {"type":"message_stop"}` + "\n\n",
			wantIn:         40,
			wantCacheWrite: 80,
			wantCacheRead:  100,
			wantOut:        20,
			wantCost:       750,
		},
		{
			name:           "gemini json gzip",
			route:          "/gemini/v1beta/models/gemini-2.5-pro:generateContent",
			reqBody:        `{}`,
			acceptEncoding: "gzip",
			headers:        func(tok string) map[string]string { return map[string]string{"x-goog-api-key": tok} },
			contentType:    "application/json",
			// 160 * $1.25 + 40 * $10.00 = 200 + 400 = 600
			respBody: `{"modelVersion":"gemini-2.5-pro","usageMetadata":{"promptTokenCount":160,"candidatesTokenCount":40}}`,
			wantIn:   160,
			wantOut:  40,
			wantCost: 600,
		},
		{
			name:           "gemini sse gzip",
			route:          "/gemini/v1beta/models/gemini-2.5-pro:streamGenerateContent",
			reqBody:        `{}`,
			acceptEncoding: "gzip",
			headers:        func(tok string) map[string]string { return map[string]string{"x-goog-api-key": tok} },
			contentType:    "text/event-stream",
			// 200 * $1.25 + 60 * $10.00 = 250 + 600 = 850
			respBody: "" +
				`data: {"modelVersion":"gemini-2.5-pro","usageMetadata":{"promptTokenCount":200,"candidatesTokenCount":10}}` + "\n\n" +
				`data: {"modelVersion":"gemini-2.5-pro","usageMetadata":{"promptTokenCount":200,"candidatesTokenCount":60}}` + "\n\n",
			wantIn:   200,
			wantOut:  60,
			wantCost: 850,
		},
	}

	for i, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			var upstreamAcceptEncoding string
			upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				upstreamAcceptEncoding = r.Header.Get("Accept-Encoding")
				w.Header().Set("Content-Type", tc.contentType)
				w.Header().Set("Content-Encoding", "gzip")
				var gzBuf bytes.Buffer
				gz := gzip.NewWriter(&gzBuf)
				_, _ = gz.Write([]byte(tc.respBody))
				_ = gz.Close()
				_, _ = w.Write(gzBuf.Bytes())
			})
			client := newPipeHTTPClient(t, upstream)
			gate, st := setupGateTest(t, map[string]UpstreamConfig{
				"openai":    {BaseURL: "http://127.0.0.1:1", SecretName: "openai_api_key", RequireSecret: true},
				"anthropic": {BaseURL: "http://127.0.0.2:1", SecretName: "anthropic_api_key", RequireSecret: true, AnthropicVer: "2023-06-01"},
				"gemini":    {BaseURL: "http://127.0.0.3:1", SecretName: "gemini_api_key", RequireSecret: true},
			}, client)
			b := gate.bundle.Budgets["agent-budget"]
			b.Spec.PerTask.MaxInputTokens = 10000
			gate.bundle.Budgets["agent-budget"] = b

			taskID := fmt.Sprintf("task-gzip-%d", i)
			if err := st.CreateTask(ctx, domain.Task{
				ID: taskID, IdempotencyKey: taskID, WorkloadName: "ai-agent", State: domain.TaskStateWorking,
			}); err != nil {
				t.Fatalf("CreateTask: %v", err)
			}
			tok, err := MintTaskToken(ctx, st, taskID, "ai-agent", time.Hour)
			if err != nil {
				t.Fatalf("MintTaskToken: %v", err)
			}

			hdrs := tc.headers(tok)
			hdrs["Accept-Encoding"] = tc.acceptEncoding
			rec := doGateRequest(gate.Handler(), http.MethodPost, tc.route, tc.reqBody, hdrs)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d body = %q, want 200", rec.Code, rec.Body.String())
			}
			if strings.Contains(strings.ToLower(upstreamAcceptEncoding), "br") {
				t.Errorf("upstream Accept-Encoding = %q, must not forward caller br encoding", upstreamAcceptEncoding)
			}
			if rec.Header().Get("Content-Encoding") != "" {
				t.Errorf("downstream Content-Encoding = %q, want empty for decompressed body", rec.Header().Get("Content-Encoding"))
			}
			if rec.Body.String() != tc.respBody {
				t.Errorf("downstream body = %q, want decompressed %q", rec.Body.String(), tc.respBody)
			}

			totals, err := st.TaskBudgetTotals(ctx, taskID)
			if err != nil {
				t.Fatalf("TaskBudgetTotals: %v", err)
			}
			if totals.InputTokens != tc.wantIn || totals.CacheWriteTokens != tc.wantCacheWrite ||
				totals.CacheReadTokens != tc.wantCacheRead || totals.OutputTokens != tc.wantOut {
				t.Errorf("totals = %+v, want in=%d cw=%d cr=%d out=%d",
					totals, tc.wantIn, tc.wantCacheWrite, tc.wantCacheRead, tc.wantOut)
			}
			if totals.CostMicroUSD != tc.wantCost {
				t.Errorf("CostMicroUSD = %d, want %d", totals.CostMicroUSD, tc.wantCost)
			}
		})
	}
}

func TestAnthropic1hCacheWritePricingAndReserve(t *testing.T) {
	// Fixture shape from armory/modelusage/testdata/anthropic-reply-cache.json:
	// input=50, cache_creation_input_tokens=248 (5m=148, 1h=100), cache_read=1800, output=503.
	// Expected with default Anthropic rates ($3 in, $3.75 5m write, $6 1h write, $0.30 read, $15 out):
	// 50*3 + 148*3.75 + 100*6 + 1800*0.30 + 503*15 = 150 + 555 + 600 + 540 + 7545 = 9390 microUSD.
	body := []byte(`{
		"id": "msg_1h",
		"type": "message",
		"model": "claude-sonnet-4-5",
		"usage": {
			"input_tokens": 50,
			"cache_creation_input_tokens": 248,
			"cache_read_input_tokens": 1800,
			"cache_creation": {
				"ephemeral_5m_input_tokens": 148,
				"ephemeral_1h_input_tokens": 100
			},
			"output_tokens": 503
		}
	}`)
	parsed := parseUsage("anthropic", "/v1/messages", body, "application/json")
	if parsed.Outcome != usageOutcomeFinal {
		t.Fatalf("outcome = %v, err = %v, want final", parsed.Outcome, parsed.Err)
	}
	reserve := domain.BudgetTotals{InputTokens: 100, OutputTokens: 10, CostMicroUSD: 750}
	settled, cost := settleUsageForEntry("anthropic", "claude-sonnet-4-5", parsed, reserve, nil)
	if settled.InputTokens != 50 || settled.CacheWriteTokens != 248 || settled.CacheReadTokens != 1800 || settled.OutputTokens != 503 {
		t.Fatalf("settled = %+v, want in=50 cw=248 cr=1800 out=503", settled)
	}
	if cost != 9390 {
		t.Errorf("1h cache write cost = %d, want 9390", cost)
	}

	// Reserve for Anthropic must price input at the 1-hour cache-write rate (2x input = $6/1M).
	// 100 tokens * $6.00 + 10 tokens * $15.00 = 600 + 150 = 750 microUSD.
	gotReserve := estimateReserveMicroUSD("anthropic", "claude-sonnet-4-5", 100, 10, nil)
	if gotReserve != 750 {
		t.Errorf("anthropic reserve cost = %d, want 750", gotReserve)
	}

	// Custom model with explicit CacheWrite1hUSD uses the configured rate for both
	// settlement and reservation.
	customExplicit := map[string]PricePerMillionTokens{
		"claude-custom": {InputUSD: 3.00, OutputUSD: 15.00, CacheWriteUSD: 3.75, CacheWrite1hUSD: 9.00, CacheReadUSD: 0.30},
	}
	// 50*3 + 148*3.75 + 100*9 + 1800*0.30 + 503*15 = 150 + 555 + 900 + 540 + 7545 = 9690
	if _, got := settleUsageForEntry("anthropic", "claude-custom", parsed, reserve, customExplicit); got != 9690 {
		t.Errorf("custom explicit 1h cost = %d, want 9690", got)
	}
	if got := estimateReserveMicroUSD("anthropic", "claude-custom", 100, 10, customExplicit); got != 1050 {
		t.Errorf("custom explicit 1h reserve = %d, want 1050", got)
	}

	// Custom model without CacheWrite1hUSD defaults the 1-hour rate to twice InputUSD.
	customDefault := map[string]PricePerMillionTokens{
		"claude-no1h": {InputUSD: 4.00, OutputUSD: 8.00, CacheWriteUSD: 5.00, CacheReadUSD: 0.40},
	}
	// 50*4 + 148*5 + 100*8 + 1800*0.40 + 503*8 = 200 + 740 + 800 + 720 + 4024 = 6484
	if _, got := settleUsageForEntry("anthropic", "claude-no1h", parsed, reserve, customDefault); got != 6484 {
		t.Errorf("custom default 2x 1h cost = %d, want 6484", got)
	}
	// Reserve on Anthropic uses 2*InputUSD = $8/1M -> 100*8 + 10*8 = 880.
	if got := estimateReserveMicroUSD("anthropic", "claude-no1h", 100, 10, customDefault); got != 880 {
		t.Errorf("custom default 2x 1h reserve = %d, want 880", got)
	}

	// If ephemeral_1h_input_tokens exceeds cache_creation_input_tokens, cap at total cache writes.
	over1hBody := []byte(`{
		"usage": {
			"input_tokens": 10,
			"cache_creation_input_tokens": 50,
			"cache_creation": {"ephemeral_1h_input_tokens": 120},
			"output_tokens": 5
		}
	}`)
	overParsed := parseUsage("anthropic", "/v1/messages", over1hBody, "application/json")
	if overParsed.Usage.CacheWriteTokens != 50 || overParsed.Usage.CacheWrite1hTokens != 50 {
		t.Fatalf("over1h parsed = %+v, want cw=50 cw1h=50", overParsed.Usage)
	}
	// 10*3 + 50*6 + 5*15 = 30 + 300 + 75 = 405
	if _, got := settleUsageForEntry("anthropic", "claude-sonnet-4-5", overParsed, reserve, nil); got != 405 {
		t.Errorf("capped 1h cost = %d, want 405", got)
	}

	// Partial Anthropic stream with 1-hour cache writes preserves the 1h surcharge.
	partial1h := []byte("" +
		"event: message_start\n" +
		`data: {"type":"message_start","message":{"usage":{"input_tokens":20,"cache_creation_input_tokens":100,"cache_creation":{"ephemeral_1h_input_tokens":60},"cache_read_input_tokens":100,"output_tokens":1}}}` + "\n\n" +
		"event: message_delta\n" +
		`data: {"type":"message_delta","usage":{"output_tokens":60}}` + "\n\n")
	partialParsed := parseUsage("anthropic", "/v1/messages", partial1h, "text/event-stream")
	if partialParsed.Outcome != usageOutcomePartial {
		t.Fatalf("partial 1h outcome = %v, want partial", partialParsed.Outcome)
	}
	// Total input=220 (> reserve 100); cw=100 (40 at $3.75 + 60 at $6.00 = 150 + 360 = 510);
	// remaining 120 at $3.00 = 360 (no cache-read discount); out=60 at $15 = 900. Total = 1770.
	if _, got := settleUsageForEntry("anthropic", "claude-sonnet-4-5", partialParsed, reserve, nil); got != 1770 {
		t.Errorf("partial 1h cost = %d, want 1770", got)
	}
}

// TestModelGateChargesAnthropicCompactionAndAdvisorUsage verifies that Anthropic
// compaction and advisor_message iteration entries are charged in input, cache
// write (including 1-hour TTL writes), cache read, and output totals for both
// JSON replies and SSE streams, while forwarding anthropic-beta unchanged.
func TestModelGateChargesAnthropicCompactionAndAdvisorUsage(t *testing.T) {
	tests := []struct {
		name           string
		reqBody        string
		betaHeader     string
		contentType    string
		respBody       string
		wantIn         int64
		wantCacheWrite int64
		wantCacheRead  int64
		wantTotalIn    int64
		wantOut        int64
		wantCost       int64
	}{
		{
			name:        "anthropic json compaction",
			reqBody:     `{"model":"claude-example","max_tokens":1000}`,
			betaHeader:  "compact-2026-01-12",
			contentType: "application/json",
			// Shape from armory/modelusage/testdata/anthropic-reply-compaction.json:
			// uncached = 23000 + 180000 = 203000 ($3.00 = 609000)
			// cw = 200 + 400 = 600 (5m = 500 * $3.75 = 1875, 1h = 100 * $6.00 = 600)
			// cr = 1000 + 50000 = 51000 ($0.30 = 15300)
			// out = 1000 + 3500 = 4500 ($15.00 = 67500)
			// total = 694275 microUSD
			respBody: `{
				"id": "msg_test0004",
				"type": "message",
				"role": "assistant",
				"model": "claude-example",
				"content": [
					{"type": "compaction", "content": "Summary of earlier turns."},
					{"type": "text", "text": "The pod is out of memory."}
				],
				"stop_reason": "end_turn",
				"stop_sequence": null,
				"usage": {
					"input_tokens": 23000,
					"cache_creation_input_tokens": 200,
					"cache_read_input_tokens": 1000,
					"output_tokens": 1000,
					"iterations": [
						{
							"type": "compaction",
							"input_tokens": 180000,
							"output_tokens": 3500,
							"cache_creation_input_tokens": 400,
							"cache_read_input_tokens": 50000,
							"cache_creation": {
								"ephemeral_5m_input_tokens": 300,
								"ephemeral_1h_input_tokens": 100
							}
						},
						{
							"type": "message",
							"input_tokens": 23000,
							"output_tokens": 1000,
							"cache_creation_input_tokens": 200,
							"cache_read_input_tokens": 1000
						}
					]
				}
			}`,
			wantIn:         203000,
			wantCacheWrite: 600,
			wantCacheRead:  51000,
			wantTotalIn:    254600,
			wantOut:        4500,
			wantCost:       694275,
		},
		{
			name:        "anthropic sse compaction",
			reqBody:     `{"model":"claude-example","max_tokens":1000,"stream":true}`,
			betaHeader:  "compact-2026-01-12",
			contentType: "text/event-stream",
			// Shape from armory/modelusage/testdata/anthropic-stream-compaction.sse:
			// uncached = 23000 + 180000 = 203000 ($3.00 = 609000)
			// cr = 50000 ($0.30 = 15000)
			// out = 1000 + 3500 = 4500 ($15.00 = 67500)
			// total = 691500 microUSD
			respBody: "" +
				"event: message_start\n" +
				`data: {"type":"message_start","message":{"id":"msg_test0005","type":"message","role":"assistant","model":"claude-example","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":23000,"output_tokens":1,"iterations":[{"type":"compaction","input_tokens":180000,"output_tokens":3500,"cache_read_input_tokens":50000}]}}}` + "\n\n" +
				"event: content_block_start\n" +
				`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}` + "\n\n" +
				"event: content_block_delta\n" +
				`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"The pod is out of memory."}}` + "\n\n" +
				"event: content_block_stop\n" +
				`data: {"type":"content_block_stop","index":0}` + "\n\n" +
				"event: message_delta\n" +
				`data: {"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":1000,"iterations":[{"type":"compaction","input_tokens":180000,"output_tokens":3500,"cache_read_input_tokens":50000},{"type":"message","input_tokens":23000,"output_tokens":1000}]}}` + "\n\n" +
				"event: message_stop\n" +
				`data: {"type":"message_stop"}` + "\n\n",
			wantIn:         203000,
			wantCacheWrite: 0,
			wantCacheRead:  50000,
			wantTotalIn:    253000,
			wantOut:        4500,
			wantCost:       691500,
		},
		{
			name:        "anthropic json advisor",
			reqBody:     `{"model":"claude-example","max_tokens":1000}`,
			betaHeader:  "advisor-tool-2026-03-01",
			contentType: "application/json",
			// Shape from armory/modelusage/testdata/anthropic-reply-advisor.json:
			// uncached = 1760 + 823 = 2583 ($3.00 = 7749)
			// cr = 412 ($0.30 = 123.6)
			// out = 531 + 1612 = 2143 ($15.00 = 32145)
			// total = ceil(40017.6) = 40018 microUSD
			respBody: `{
				"id": "msg_test0006",
				"type": "message",
				"role": "assistant",
				"model": "claude-example",
				"content": [{"type": "text", "text": "The pod is out of memory."}],
				"stop_reason": "end_turn",
				"stop_sequence": null,
				"usage": {
					"input_tokens": 1760,
					"cache_read_input_tokens": 412,
					"output_tokens": 531,
					"iterations": [
						{
							"type": "message",
							"input_tokens": 1760,
							"cache_read_input_tokens": 412,
							"output_tokens": 531
						},
						{
							"type": "advisor_message",
							"input_tokens": 823,
							"output_tokens": 1612
						}
					]
				}
			}`,
			wantIn:         2583,
			wantCacheWrite: 0,
			wantCacheRead:  412,
			wantTotalIn:    2995,
			wantOut:        2143,
			wantCost:       40018,
		},
		{
			name:        "anthropic sse advisor",
			reqBody:     `{"model":"claude-example","max_tokens":1000,"stream":true}`,
			betaHeader:  "advisor-tool-2026-03-01",
			contentType: "text/event-stream",
			// Shape from armory/modelusage/testdata/anthropic-stream-advisor.sse:
			// same counts as anthropic-reply-advisor.json -> 40018 microUSD
			respBody: "" +
				"event: message_start\n" +
				`data: {"type":"message_start","message":{"id":"msg_test0007","type":"message","role":"assistant","model":"claude-example","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":1760,"cache_read_input_tokens":412,"output_tokens":1}}}` + "\n\n" +
				"event: content_block_start\n" +
				`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}` + "\n\n" +
				"event: content_block_delta\n" +
				`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"The pod is out of memory."}}` + "\n\n" +
				"event: content_block_stop\n" +
				`data: {"type":"content_block_stop","index":0}` + "\n\n" +
				"event: message_delta\n" +
				`data: {"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":531,"iterations":[{"type":"message","input_tokens":1760,"cache_read_input_tokens":412,"output_tokens":531},{"type":"advisor_message","input_tokens":823,"output_tokens":1612}]}}` + "\n\n" +
				"event: message_stop\n" +
				`data: {"type":"message_stop"}` + "\n\n",
			wantIn:         2583,
			wantCacheWrite: 0,
			wantCacheRead:  412,
			wantTotalIn:    2995,
			wantOut:        2143,
			wantCost:       40018,
		},
		{
			name:        "anthropic json advisor with cache creation 1h and cache read",
			reqBody:     `{"model":"claude-example","max_tokens":1000}`,
			betaHeader:  "advisor-tool-2026-03-01",
			contentType: "application/json",
			// uncached = 100 + 200 = 300 ($3.00 = 900)
			// cw = 50 + 80 = 130 (5m = 90 * $3.75 = 337.5, 1h = 40 * $6.00 = 240)
			// cr = 20 + 300 = 320 ($0.30 = 96)
			// out = 10 + 60 = 70 ($15.00 = 1050)
			// total = ceil(2623.5) = 2624 microUSD
			respBody: `{
				"id": "msg_adv_cache",
				"type": "message",
				"model": "claude-example",
				"usage": {
					"input_tokens": 100,
					"cache_creation_input_tokens": 50,
					"cache_read_input_tokens": 20,
					"output_tokens": 10,
					"iterations": [
						{
							"type": "advisor_message",
							"input_tokens": 200,
							"output_tokens": 60,
							"cache_creation_input_tokens": 80,
							"cache_read_input_tokens": 300,
							"cache_creation": {
								"ephemeral_5m_input_tokens": 40,
								"ephemeral_1h_input_tokens": 40
							}
						}
					]
				}
			}`,
			wantIn:         300,
			wantCacheWrite: 130,
			wantCacheRead:  320,
			wantTotalIn:    750,
			wantOut:        70,
			wantCost:       2624,
		},
	}

	for i, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			var gotBetaHeader string
			upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotBetaHeader = r.Header.Get("anthropic-beta")
				w.Header().Set("Content-Type", tc.contentType)
				_, _ = w.Write([]byte(tc.respBody))
			})
			gate, st := setupGateTest(t, map[string]UpstreamConfig{
				"anthropic": {BaseURL: "https://anthropic.test", SecretName: "anthropic_api_key", RequireSecret: true, AnthropicVer: "2023-06-01"},
			}, handlerTransport(upstream))
			b := gate.bundle.Budgets["agent-budget"]
			b.Spec.PerTask.MaxInputTokens = 1_000_000
			b.Spec.PerTask.MaxOutputTokens = 100_000
			b.Spec.PerTask.MaxCostUSD = 10.00
			gate.bundle.Budgets["agent-budget"] = b

			taskID := fmt.Sprintf("task-iter-%d", i)
			if err := st.CreateTask(ctx, domain.Task{
				ID: taskID, IdempotencyKey: taskID, WorkloadName: "ai-agent", State: domain.TaskStateWorking,
			}); err != nil {
				t.Fatalf("CreateTask: %v", err)
			}
			tok, err := MintTaskToken(ctx, st, taskID, "ai-agent", time.Hour)
			if err != nil {
				t.Fatalf("MintTaskToken: %v", err)
			}

			rec := doGateRequest(gate.Handler(), http.MethodPost, "/anthropic/v1/messages", tc.reqBody, map[string]string{
				"x-api-key":      tok,
				"anthropic-beta": tc.betaHeader,
			})
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d body = %s, want 200", rec.Code, rec.Body.String())
			}
			if gotBetaHeader != tc.betaHeader {
				t.Errorf("upstream anthropic-beta = %q, want %q (must not be stripped silently)", gotBetaHeader, tc.betaHeader)
			}

			totals, err := st.TaskBudgetTotals(ctx, taskID)
			if err != nil {
				t.Fatalf("TaskBudgetTotals: %v", err)
			}
			if totals.InputTokens != tc.wantIn || totals.CacheWriteTokens != tc.wantCacheWrite ||
				totals.CacheReadTokens != tc.wantCacheRead || totals.OutputTokens != tc.wantOut {
				t.Errorf("totals = %+v, want in=%d cw=%d cr=%d out=%d",
					totals, tc.wantIn, tc.wantCacheWrite, tc.wantCacheRead, tc.wantOut)
			}
			if totals.TotalInputTokens() != tc.wantTotalIn {
				t.Errorf("TotalInputTokens = %d, want %d", totals.TotalInputTokens(), tc.wantTotalIn)
			}
			if totals.CostMicroUSD != tc.wantCost {
				t.Errorf("CostMicroUSD = %d, want %d", totals.CostMicroUSD, tc.wantCost)
			}
		})
	}
}

func TestModelGateChargesAnthropicUnclassifiedIterations(t *testing.T) {
	reserve := domain.BudgetTotals{InputTokens: 1, OutputTokens: 1, CostMicroUSD: 21}

	tests := []struct {
		name             string
		contentType      string
		body             string
		wantOutcome      usageOutcome
		wantIn           int64
		wantCacheWrite   int64
		wantCacheWrite1h int64
		wantCacheRead    int64
		wantTotalIn      int64
		wantOut          int64
		wantCost         int64
	}{
		{
			name:        "json fallback_message 1h cache write bills at 1h cache write rate",
			contentType: "application/json",
			// Review input: fallback_message with 1,000,000 1h cache write tokens.
			// Must bill at $6.00/M (6,000,000 microUSD), not $3.00/M, while TotalInputTokens stays 1,000,000.
			body: `{
				"id": "msg_unclass_1h_cw",
				"type": "message",
				"model": "claude-example",
				"usage": {
					"input_tokens": 0,
					"output_tokens": 0,
					"iterations": [
						{
							"type": "fallback_message",
							"cache_creation_input_tokens": 1000000,
							"cache_creation": {"ephemeral_1h_input_tokens": 1000000}
						}
					]
				}
			}`,
			wantOutcome:      usageOutcomeFinal,
			wantIn:           0,
			wantCacheWrite:   1000000,
			wantCacheWrite1h: 1000000,
			wantCacheRead:    0,
			wantTotalIn:      1000000,
			wantOut:          0,
			wantCost:         6000000,
		},
		{
			name:        "json fallback_message adds all input-side counts to 1h cache write and output to output",
			contentType: "application/json",
			// top: in=100 ($3=300), cw=50 (5m: $3.75=187.5), cr=200 ($0.30=60), out=30
			// fallback_message: input=40 + cw=10 + cr=25 = 75 unclassified in (1h: $6=450); out=15 unclassified out
			// settled: in=100, cw=125 (5m=50, 1h=75), cr=200 (totalIn=425), out=45 ($15=675) -> ceil(1672.5)=1673
			body: `{
				"id": "msg_unclass_1",
				"type": "message",
				"model": "claude-example",
				"usage": {
					"input_tokens": 100,
					"cache_creation_input_tokens": 50,
					"cache_read_input_tokens": 200,
					"output_tokens": 30,
					"iterations": [
						{"type": "fallback_message", "input_tokens": 40, "cache_creation_input_tokens": 10, "cache_read_input_tokens": 25, "output_tokens": 15}
					]
				}
			}`,
			wantOutcome:      usageOutcomeFinal,
			wantIn:           100,
			wantCacheWrite:   125,
			wantCacheWrite1h: 75,
			wantCacheRead:    200,
			wantTotalIn:      425,
			wantOut:          45,
			wantCost:         1673,
		},
		{
			name:        "json unknown and missing iteration types fail closed into 1h cache write and output",
			contentType: "application/json",
			// top: in=20 ($3=60), out=5; unknown: in=30, out=10; missing type: in=15, out=4; null type: in=5, out=1
			// settled: in=20, cw=50 (1h: $6=300), totalIn=70, out=20 ($15=300) -> 660
			body: `{
				"id": "msg_unclass_2",
				"type": "message",
				"model": "claude-example",
				"usage": {
					"input_tokens": 20,
					"output_tokens": 5,
					"iterations": [
						{"type": "future_step", "input_tokens": 30, "output_tokens": 10},
						{"input_tokens": 15, "output_tokens": 4},
						{"type": null, "input_tokens": 5, "output_tokens": 1}
					]
				}
			}`,
			wantOutcome:      usageOutcomeFinal,
			wantIn:           20,
			wantCacheWrite:   50,
			wantCacheWrite1h: 50,
			wantTotalIn:      70,
			wantOut:          20,
			wantCost:         660,
		},
		{
			name:        "partial sse stream preserves unclassified iteration counts at 1h cache write rate",
			contentType: "text/event-stream",
			// message_start: in=20, cr=100, out=1, unclassified in=40 (1h cw), unclassified out=10
			// partial bills no cache-read discount: in=120 ($3=360), cw=40 (1h: $6=240), totalIn=160, out=11 ($15=165) -> 765
			body: "" +
				"event: message_start\n" +
				`data: {"type":"message_start","message":{"id":"msg_unclass_sse","model":"claude-example","usage":{"input_tokens":20,"cache_read_input_tokens":100,"output_tokens":1,"iterations":[{"type":"fallback_message","input_tokens":40,"output_tokens":10}]}}}` + "\n\n",
			wantOutcome:      usageOutcomePartial,
			wantIn:           120,
			wantCacheWrite:   40,
			wantCacheWrite1h: 40,
			wantTotalIn:      160,
			wantOut:          11,
			wantCost:         765,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			parsed := parseUsage("anthropic", "/v1/messages", []byte(tc.body), tc.contentType)
			if parsed.Outcome != tc.wantOutcome {
				t.Fatalf("outcome = %v (err=%v), want %v", parsed.Outcome, parsed.Err, tc.wantOutcome)
			}
			settled, cost := settleUsageForEntry("anthropic", "claude-example", parsed, reserve, nil)
			if settled.InputTokens != tc.wantIn || settled.CacheWriteTokens != tc.wantCacheWrite ||
				settled.CacheWrite1hTokens != tc.wantCacheWrite1h || settled.CacheReadTokens != tc.wantCacheRead ||
				settled.OutputTokens != tc.wantOut {
				t.Errorf("settled = %+v, want in=%d cw=%d cw1h=%d cr=%d out=%d",
					settled, tc.wantIn, tc.wantCacheWrite, tc.wantCacheWrite1h, tc.wantCacheRead, tc.wantOut)
			}
			if settled.TotalInputTokens() != tc.wantTotalIn {
				t.Errorf("TotalInputTokens = %d, want %d", settled.TotalInputTokens(), tc.wantTotalIn)
			}
			if cost != tc.wantCost {
				t.Errorf("cost = %d, want %d", cost, tc.wantCost)
			}
		})
	}
}

func TestModelGateChargesReportedTotalExcessAsOutput(t *testing.T) {
	reserve := domain.BudgetTotals{InputTokens: 1, OutputTokens: 1, CostMicroUSD: 13}

	tests := []struct {
		name          string
		upstreamPath  string
		contentType   string
		body          string
		wantOutcome   usageOutcome
		wantIn        int64
		wantCacheRead int64
		wantOut       int64
		wantCost      int64
	}{
		{
			name:         "chat json prompt 9 completion 2 total 20 charges 9 extra as output",
			upstreamPath: "/v1/chat/completions",
			contentType:  "application/json",
			// parts = 9 + 2 = 11; excess = 20 - 11 = 9 -> out = 11
			// 9 * $2.50 + 11 * $10.00 = 22.5 + 110 = 132.5 -> ceil 133
			body:        `{"model":"gpt-5","usage":{"prompt_tokens":9,"completion_tokens":2,"total_tokens":20}}`,
			wantOutcome: usageOutcomeFinal,
			wantIn:      9,
			wantOut:     11,
			wantCost:    133,
		},
		{
			name:         "chat json with cached prompt keeps cache split and charges excess as output",
			upstreamPath: "/v1/chat/completions",
			contentType:  "application/json",
			// uncached = 5, cacheRead = 4, parts = 11, excess = 9 -> out = 11
			// 5 * $2.50 + 4 * $1.25 + 11 * $10.00 = 12.5 + 5 + 110 = 127.5 -> ceil 128
			body:          `{"model":"gpt-5","usage":{"prompt_tokens":9,"completion_tokens":2,"total_tokens":20,"prompt_tokens_details":{"cached_tokens":4}}}`,
			wantOutcome:   usageOutcomeFinal,
			wantIn:        5,
			wantCacheRead: 4,
			wantOut:       11,
			wantCost:      128,
		},
		{
			name:         "responses json input 9 output 2 total 20 charges 9 extra as output",
			upstreamPath: "/v1/responses",
			contentType:  "application/json",
			body:         `{"id":"resp_tot","status":"completed","model":"gpt-5","usage":{"input_tokens":9,"output_tokens":2,"total_tokens":20}}`,
			wantOutcome:  usageOutcomeFinal,
			wantIn:       9,
			wantOut:      11,
			wantCost:     133,
		},
		{
			name:         "chat partial sse stream preserves reported total excess as output",
			upstreamPath: "/v1/chat/completions",
			contentType:  "text/event-stream",
			body:         `data: {"model":"gpt-5","usage":{"prompt_tokens":9,"completion_tokens":2,"total_tokens":20}}` + "\n\n",
			wantOutcome:  usageOutcomePartial,
			wantIn:       9,
			wantOut:      11,
			wantCost:     133,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			parsed := parseUsage("openai", tc.upstreamPath, []byte(tc.body), tc.contentType)
			if parsed.Outcome != tc.wantOutcome {
				t.Fatalf("outcome = %v (err=%v), want %v", parsed.Outcome, parsed.Err, tc.wantOutcome)
			}
			settled, cost := settleUsageForEntry("openai", "gpt-5", parsed, reserve, nil)
			if settled.InputTokens != tc.wantIn || settled.CacheReadTokens != tc.wantCacheRead || settled.OutputTokens != tc.wantOut {
				t.Errorf("settled = %+v, want in=%d cr=%d out=%d", settled, tc.wantIn, tc.wantCacheRead, tc.wantOut)
			}
			if settled.TotalTokens() != 20 {
				t.Errorf("TotalTokens = %d, want 20", settled.TotalTokens())
			}
			if cost != tc.wantCost {
				t.Errorf("cost = %d, want %d", cost, tc.wantCost)
			}
		})
	}
}

func TestModelUsageMappingOverflowSafetyAndCacheSeparation(t *testing.T) {
	reserve := domain.BudgetTotals{
		InputTokens:  100,
		OutputTokens: 50,
		CostMicroUSD: estimateReserveMicroUSD("anthropic", "claude-example", 100, 50, nil),
	}

	overflowCases := []struct {
		name        string
		contentType string
		body        string
	}{
		{
			name:        "json input plus unclassified input exceeds maxReservedTokens",
			contentType: "application/json",
			body: fmt.Sprintf(
				`{"usage":{"input_tokens":%d,"output_tokens":0,"iterations":[{"type":"fallback_message","input_tokens":1,"output_tokens":0}]}}`,
				maxReservedTokens,
			),
		},
		{
			name:        "json output plus unclassified output exceeds maxReservedTokens",
			contentType: "application/json",
			body: fmt.Sprintf(
				`{"usage":{"input_tokens":0,"output_tokens":%d,"iterations":[{"type":"fallback_message","input_tokens":0,"output_tokens":1}]}}`,
				maxReservedTokens,
			),
		},
		{
			name:        "json total tokens with unclassified exceeds maxReservedTokens",
			contentType: "application/json",
			body: fmt.Sprintf(
				`{"usage":{"input_tokens":%d,"output_tokens":0,"iterations":[{"type":"fallback_message","input_tokens":0,"output_tokens":1}]}}`,
				maxReservedTokens,
			),
		},
		{
			name:        "partial sse stream with unclassified exceeding maxReservedTokens fails closed",
			contentType: "text/event-stream",
			body: fmt.Sprintf(
				"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":%d,\"output_tokens\":0,\"iterations\":[{\"type\":\"fallback_message\",\"input_tokens\":1,\"output_tokens\":0}]}}}\n\n",
				maxReservedTokens,
			),
		},
	}

	for _, tc := range overflowCases {
		t.Run(tc.name, func(t *testing.T) {
			parsed := parseUsage("anthropic", "/v1/messages", []byte(tc.body), tc.contentType)
			if parsed.Outcome != usageOutcomeNone {
				t.Fatalf("outcome = %v (usage=%+v), want usageOutcomeNone", parsed.Outcome, parsed.Usage)
			}
			if !errors.Is(parsed.Err, modelusage.ErrOutOfRange) {
				t.Fatalf("err = %v, want modelusage.ErrOutOfRange", parsed.Err)
			}
			settled, cost := settleUsageForEntry("anthropic", "claude-example", parsed, reserve, nil)
			if settled.InputTokens != reserve.InputTokens || settled.OutputTokens != reserve.OutputTokens || cost != reserve.CostMicroUSD {
				t.Fatalf("settled = %+v cost=%d, want reserve %+v", settled, cost, reserve)
			}
		})
	}

	t.Run("exact maxReservedTokens with unclassified is accepted", func(t *testing.T) {
		body := fmt.Sprintf(
			`{"usage":{"input_tokens":%d,"output_tokens":5,"iterations":[{"type":"fallback_message","input_tokens":3,"output_tokens":2}]}}`,
			maxReservedTokens-10,
		)
		parsed := parseUsage("anthropic", "/v1/messages", []byte(body), "application/json")
		if parsed.Outcome != usageOutcomeFinal {
			t.Fatalf("outcome = %v err = %v, want usageOutcomeFinal", parsed.Outcome, parsed.Err)
		}
		if parsed.Usage.InputTokens != maxReservedTokens-10 || parsed.Usage.CacheWriteTokens != 3 ||
			parsed.Usage.CacheWrite1hTokens != 3 || parsed.Usage.TotalInputTokens() != maxReservedTokens-7 ||
			parsed.Usage.OutputTokens != 7 || parsed.Usage.TotalTokens() != maxReservedTokens {
			t.Fatalf("usage = %+v, want in=%d cw=3 cw1h=3 totalIn=%d out=7 total=%d",
				parsed.Usage, maxReservedTokens-10, maxReservedTokens-7, maxReservedTokens)
		}
	})

	t.Run("iteration 1h lifetime without parent cache write adds nothing", func(t *testing.T) {
		body := `{
			"usage": {
				"input_tokens": 10,
				"cache_creation_input_tokens": 100,
				"output_tokens": 5,
				"iterations": [
					{
						"type": "compaction",
						"input_tokens": 20,
						"output_tokens": 5,
						"cache_creation": {"ephemeral_1h_input_tokens": 80}
					}
				]
			}
		}`
		parsed := parseUsage("anthropic", "/v1/messages", []byte(body), "application/json")
		if parsed.Outcome != usageOutcomeFinal {
			t.Fatalf("outcome = %v err = %v, want usageOutcomeFinal", parsed.Outcome, parsed.Err)
		}
		if parsed.Usage.CacheWriteTokens != 100 || parsed.Usage.CacheWrite1hTokens != 0 {
			t.Fatalf("usage = %+v, want cw=100 cw1h=0", parsed.Usage)
		}
	})

	t.Run("partial anthropic stream keeps compaction output and 1h surcharge", func(t *testing.T) {
		body := "" +
			"event: message_start\n" +
			`data: {"type":"message_start","message":{"usage":{"input_tokens":50,"output_tokens":1,"iterations":[{"type":"compaction","input_tokens":200,"output_tokens":90,"cache_creation_input_tokens":100,"cache_creation":{"ephemeral_1h_input_tokens":40},"cache_read_input_tokens":50}]}}}` + "\n\n"
		parsed := parseUsage("anthropic", "/v1/messages", []byte(body), "text/event-stream")
		if parsed.Outcome != usageOutcomePartial {
			t.Fatalf("outcome = %v err = %v, want usageOutcomePartial", parsed.Outcome, parsed.Err)
		}
		// totalIn = 50 + 200 + 100 + 50 = 400; cw = 100 (60*3.75 + 40*6 = 225 + 240 = 465);
		// remaining 300 at $3 = 900; out = 91 at $15 = 1365 -> total = 2730
		settled, cost := settleUsageForEntry("anthropic", "claude-example", parsed, reserve, nil)
		if settled.InputTokens != 300 || settled.CacheWriteTokens != 100 || settled.CacheWrite1hTokens != 40 || settled.CacheReadTokens != 0 || settled.OutputTokens != 91 {
			t.Fatalf("settled = %+v, want in=300 cw=100 cw1h=40 cr=0 out=91", settled)
		}
		if cost != 2730 {
			t.Fatalf("cost = %d, want 2730", cost)
		}
	})

	t.Run("Usage totals saturate on int64 overflow", func(t *testing.T) {
		u := Usage{
			InputTokens:      math.MaxInt64 - 5,
			CacheWriteTokens: 10,
			CacheReadTokens:  10,
			OutputTokens:     10,
		}
		if got := u.TotalInputTokens(); got != math.MaxInt64 {
			t.Errorf("TotalInputTokens = %d, want MaxInt64", got)
		}
		if got := u.TotalTokens(); got != math.MaxInt64 {
			t.Errorf("TotalTokens = %d, want MaxInt64", got)
		}
	})

	t.Run("satAddInt64 clamps negative operands and saturates on overflow", func(t *testing.T) {
		tests := []struct {
			name string
			a, b int64
			want int64
		}{
			{name: "normal sum", a: 10, b: 20, want: 30},
			{name: "overflow saturates", a: math.MaxInt64 - 5, b: 10, want: math.MaxInt64},
			{name: "negative b clamped to zero", a: 10, b: -1, want: 10},
			{name: "min int64 b clamped to zero", a: 10, b: math.MinInt64, want: 10},
			{name: "negative a clamped to zero", a: -5, b: 10, want: 10},
			{name: "both negative clamped to zero", a: -5, b: -1, want: 0},
		}
		for _, tc := range tests {
			t.Run(tc.name, func(t *testing.T) {
				if got := satAddInt64(tc.a, tc.b); got != tc.want {
					t.Errorf("satAddInt64(%d, %d) = %d, want %d", tc.a, tc.b, got, tc.want)
				}
			})
		}
	})
}

func TestMicroUSDFromFloatRoundsUpAtBoundaries(t *testing.T) {
	belowMax := math.Nextafter(float64(math.MaxInt64), 0)
	tests := []struct {
		name string
		in   float64
		want int64
	}{
		{name: "zero", in: 0, want: 0},
		{name: "negative zero", in: math.Copysign(0, -1), want: 0},
		{name: "negative fraction", in: -0.0001, want: 0},
		{name: "negative integer", in: -5, want: 0},
		{name: "negative inf", in: math.Inf(-1), want: 0},
		{name: "nan", in: math.NaN(), want: 0},
		{name: "smallest nonzero", in: math.SmallestNonzeroFloat64, want: 1},
		{name: "tiny positive", in: 1e-12, want: 1},
		{name: "anthropic single cache read token", in: 0.30, want: 1},
		{name: "just below half", in: 0.49, want: 1},
		{name: "exact half", in: 0.50, want: 1},
		{name: "exact one", in: 1.0, want: 1},
		{name: "just above one", in: 1.000001, want: 2},
		{name: "openai single cache read token", in: 1.25, want: 2},
		{name: "exact integer", in: 3390.0, want: 3390},
		{name: "largest float below MaxInt64", in: belowMax, want: int64(belowMax)},
		{name: "float MaxInt64 saturates", in: float64(math.MaxInt64), want: math.MaxInt64},
		{name: "huge float saturates", in: 1e300, want: math.MaxInt64},
		{name: "positive inf saturates", in: math.Inf(1), want: math.MaxInt64},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := microUSDFromFloat(tc.in)
			if got != tc.want {
				t.Errorf("microUSDFromFloat(%v) = %d, want %d", tc.in, got, tc.want)
			}
			if got < 0 {
				t.Errorf("microUSDFromFloat(%v) = %d, must never be negative", tc.in, got)
			}
		})
	}

	// End-to-end: 1 Anthropic cache-read token ($0.30/1M) must cost 1 microUSD, not 0.
	if got := EstimateUsageMicroUSD("anthropic", "claude-sonnet-4-5", Usage{CacheReadTokens: 1}, nil); got != 1 {
		t.Errorf("1 anthropic cache-read token cost = %d, want 1", got)
	}
}

func TestSettlementNeverBelowAdmittedReserve(t *testing.T) {
	// Admission reserved 103 microUSD (openai rate for 1 input, 10 output).
	admittedReserve := domain.BudgetTotals{
		InputTokens:  1,
		OutputTokens: 10,
		CostMicroUSD: 103,
	}

	unitCases := []struct {
		name           string
		activeProvider string
		parsed         parsedUsage
		wantIn         int64
		wantOut        int64
		wantCost       int64
	}{
		{
			name:           "unreadable usage with cheaper active provider settles admitted reserve",
			activeProvider: "local",
			parsed:         parsedUsage{Outcome: usageOutcomeNone, Err: modelusage.ErrNoUsage},
			wantIn:         1,
			wantOut:        10,
			wantCost:       103,
		},
		{
			name:           "all-zero usage with cheaper active provider settles admitted reserve",
			activeProvider: "local",
			parsed:         parsedUsage{Outcome: usageOutcomeFinal, Usage: Usage{}},
			wantIn:         1,
			wantOut:        10,
			wantCost:       103,
		},
		{
			name:           "partial usage with cheaper active provider floors at admitted reserve",
			activeProvider: "local",
			parsed:         parsedUsage{Outcome: usageOutcomePartial, Usage: Usage{InputTokens: 1, OutputTokens: 2}},
			wantIn:         1,
			wantOut:        10,
			wantCost:       103,
		},
		{
			name:           "nonzero final usage keeps active provider rates",
			activeProvider: "local",
			parsed:         parsedUsage{Outcome: usageOutcomeFinal, Usage: Usage{InputTokens: 1, OutputTokens: 10}},
			wantIn:         1,
			wantOut:        10,
			wantCost:       0,
		},
		{
			name:           "partial usage above admitted reserve charges higher partial cost",
			activeProvider: "openai",
			parsed:         parsedUsage{Outcome: usageOutcomePartial, Usage: Usage{InputTokens: 100, OutputTokens: 50}},
			wantIn:         100,
			wantOut:        50,
			wantCost:       750,
		},
	}

	for _, tc := range unitCases {
		t.Run(tc.name, func(t *testing.T) {
			settled, cost := settleUsageForEntry(tc.activeProvider, "gpt-5", tc.parsed, admittedReserve, nil)
			if settled.InputTokens != tc.wantIn || settled.OutputTokens != tc.wantOut {
				t.Errorf("settled = %+v, want in=%d out=%d", settled, tc.wantIn, tc.wantOut)
			}
			if cost != tc.wantCost {
				t.Errorf("cost = %d, want %d", cost, tc.wantCost)
			}
		})
	}

	// End-to-end through Gate.Handler(): both primary-local-with-openai-fallback and
	// primary-openai-failing-over-to-local must settle at least the admitted reserve
	// cost (103 microUSD) when usage is unreadable, all-zero, or partial.
	e2eCases := []struct {
		name        string
		route       string
		failPrimary bool
		contentType string
		respBody    string
		wantCost    int64
	}{
		{
			name:        "e2e local primary with openai fallback unreadable usage",
			route:       "/local/v1/chat/completions",
			contentType: "application/json",
			respBody:    `{"id":"local-no-usage"}`,
			wantCost:    103,
		},
		{
			name:        "e2e local primary with openai fallback all-zero usage",
			route:       "/local/v1/chat/completions",
			contentType: "application/json",
			respBody:    `{"model":"gpt-5","usage":{"prompt_tokens":0,"completion_tokens":0}}`,
			wantCost:    103,
		},
		{
			name:        "e2e local primary with openai fallback partial stream",
			route:       "/local/v1/chat/completions",
			contentType: "text/event-stream",
			respBody:    `data: {"model":"gpt-5","usage":{"prompt_tokens":1,"completion_tokens":2}}` + "\n\n",
			wantCost:    103,
		},
		{
			name:        "e2e openai fallback to local unreadable usage",
			route:       "/openai/v1/chat/completions",
			failPrimary: true,
			contentType: "application/json",
			respBody:    `{"id":"fallback-no-usage"}`,
			wantCost:    103,
		},
		{
			name:        "e2e openai fallback to local all-zero usage",
			route:       "/openai/v1/chat/completions",
			failPrimary: true,
			contentType: "application/json",
			respBody:    `{"model":"gpt-5","usage":{"prompt_tokens":0,"completion_tokens":0}}`,
			wantCost:    103,
		},
		{
			name:        "e2e openai fallback to local partial stream",
			route:       "/openai/v1/chat/completions",
			failPrimary: true,
			contentType: "text/event-stream",
			respBody:    `data: {"model":"gpt-5","usage":{"prompt_tokens":1,"completion_tokens":2}}` + "\n\n",
			wantCost:    103,
		},
		{
			name:        "e2e openai fallback to local nonzero final usage uses local rate",
			route:       "/openai/v1/chat/completions",
			failPrimary: true,
			contentType: "application/json",
			respBody:    `{"model":"gpt-5","usage":{"prompt_tokens":1,"completion_tokens":10}}`,
			wantCost:    0,
		},
	}

	for _, tc := range e2eCases {
		t.Run(tc.name, func(t *testing.T) {
			upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if tc.failPrimary && strings.Contains(r.Host, "openai.com") {
					w.WriteHeader(http.StatusServiceUnavailable)
					return
				}
				w.Header().Set("Content-Type", tc.contentType)
				_, _ = w.Write([]byte(tc.respBody))
			})
			c := newChildGate(t, `{"maxCostUSD":0.001}`, upstream)
			wl := c.gate.bundle.Workloads["ai-agent"]
			wl.Spec.Grants.ModelGate.Providers = []string{"openai", "local"}
			c.gate.bundle.Workloads["ai-agent"] = wl

			rec := doGateRequest(c.gate.Handler(), http.MethodPost, tc.route,
				`{"model":"gpt-5","max_tokens":10}`,
				map[string]string{"Authorization": "Bearer " + c.token})
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d body = %s, want 200", rec.Code, rec.Body.String())
			}
			totals, err := c.st.TaskBudgetTotals(t.Context(), "child")
			if err != nil {
				t.Fatalf("TaskBudgetTotals: %v", err)
			}
			if totals.CostMicroUSD != tc.wantCost {
				t.Fatalf("settled CostMicroUSD = %d, want %d", totals.CostMicroUSD, tc.wantCost)
			}
		})
	}
}

func TestParseGeminiSSEScannerErrorReturnsPartialOrNone(t *testing.T) {
	// An oversized SSE line between maxUsageLineBytes (4 MiB) and maxCapturedResponseBytes (8 MiB)
	// causes bufio.Scanner to stop with bufio.ErrTooLong.
	oversizedLine := "data: " + strings.Repeat("x", maxUsageLineBytes+16) + "\n\n"
	reserve := domain.BudgetTotals{
		InputTokens:  100,
		OutputTokens: 50,
		CostMicroUSD: estimateReserveMicroUSD("gemini", "gemini-2.5-pro", 100, 50, nil),
	}

	t.Run("usage before scanner error is partial and floors at reserve", func(t *testing.T) {
		body := []byte(
			`data: {"modelVersion":"gemini-2.5-pro","usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":2}}` + "\n\n" +
				oversizedLine,
		)
		parsed := parseUsage("gemini", "/v1beta/models/gemini-2.5-pro:streamGenerateContent", body, "text/event-stream")
		if parsed.Outcome != usageOutcomePartial {
			t.Fatalf("outcome = %v, want usageOutcomePartial (err=%v)", parsed.Outcome, parsed.Err)
		}
		if !errors.Is(parsed.Err, bufio.ErrTooLong) {
			t.Fatalf("err = %v, want bufio.ErrTooLong", parsed.Err)
		}
		settled, cost := settleUsageForEntry("gemini", "gemini-2.5-pro", parsed, reserve, nil)
		if settled.InputTokens != 100 || settled.CacheReadTokens != 0 || settled.OutputTokens != 50 {
			t.Fatalf("settled = %+v, want reserve floor in=100 cr=0 out=50", settled)
		}
		if cost != reserve.CostMicroUSD {
			t.Fatalf("cost = %d, want reserve floor %d", cost, reserve.CostMicroUSD)
		}
	})

	t.Run("cached usage above reserve before scanner error drops cache-read discount", func(t *testing.T) {
		body := []byte(
			`data: {"modelVersion":"gemini-2.5-pro","usageMetadata":{"promptTokenCount":500,"cachedContentTokenCount":400,"candidatesTokenCount":80}}` + "\n\n" +
				oversizedLine,
		)
		custom := map[string]PricePerMillionTokens{
			"gemini": {InputUSD: 1.25, CacheReadUSD: 0.30, OutputUSD: 10.00},
		}
		parsed := parseUsage("gemini", "/v1beta/models/gemini-2.5-pro:streamGenerateContent", body, "text/event-stream")
		if parsed.Outcome != usageOutcomePartial {
			t.Fatalf("outcome = %v, want usageOutcomePartial (err=%v)", parsed.Outcome, parsed.Err)
		}
		if !errors.Is(parsed.Err, bufio.ErrTooLong) {
			t.Fatalf("err = %v, want bufio.ErrTooLong", parsed.Err)
		}
		settled, cost := settleUsageForEntry("gemini", "gemini-2.5-pro", parsed, reserve, custom)
		// 500 * $1.25 + 80 * $10.00 = 625 + 800 = 1425 (no cache-read discount on partial).
		if settled.InputTokens != 500 || settled.CacheReadTokens != 0 || settled.OutputTokens != 80 {
			t.Fatalf("settled = %+v, want in=500 cr=0 out=80", settled)
		}
		if cost != 1425 {
			t.Fatalf("cost = %d, want 1425", cost)
		}
	})

	t.Run("scanner error before any usage returns none and charges reserve", func(t *testing.T) {
		body := []byte(oversizedLine)
		parsed := parseUsage("gemini", "/v1beta/models/gemini-2.5-pro:streamGenerateContent", body, "text/event-stream")
		if parsed.Outcome != usageOutcomeNone {
			t.Fatalf("outcome = %v, want usageOutcomeNone (err=%v)", parsed.Outcome, parsed.Err)
		}
		if !errors.Is(parsed.Err, bufio.ErrTooLong) {
			t.Fatalf("err = %v, want bufio.ErrTooLong", parsed.Err)
		}
		settled, cost := settleUsageForEntry("gemini", "gemini-2.5-pro", parsed, reserve, nil)
		if settled.InputTokens != 100 || settled.OutputTokens != 50 || cost != reserve.CostMicroUSD {
			t.Fatalf("settled = %+v cost=%d, want reserve in=100 out=50 cost=%d", settled, cost, reserve.CostMicroUSD)
		}
	})
}

func TestParseGeminiSplitsCachedPromptTokens(t *testing.T) {
	custom := map[string]PricePerMillionTokens{
		"gemini": {InputUSD: 1.25, CacheReadUSD: 0.30, OutputUSD: 10.00},
	}
	reserve := domain.BudgetTotals{InputTokens: 1, OutputTokens: 1, CostMicroUSD: 12}

	tests := []struct {
		name          string
		contentType   string
		body          string
		wantIn        int64
		wantCacheRead int64
		wantTotalIn   int64
		wantOut       int64
		wantCost      int64
	}{
		{
			name:          "json splits cachedContentTokenCount out of promptTokenCount",
			contentType:   "application/json",
			body:          `{"modelVersion":"gemini-2.5-pro","usageMetadata":{"promptTokenCount":200,"cachedContentTokenCount":150,"candidatesTokenCount":40,"thoughtsTokenCount":10}}`,
			wantIn:        50,
			wantCacheRead: 150,
			wantTotalIn:   200,
			wantOut:       50,
			// 50 * 1.25 + 150 * 0.30 + 50 * 10.00 = 62.5 + 45 + 500 = 607.5 -> ceil 608
			wantCost: 608,
		},
		{
			name:          "json fully cached prompt",
			contentType:   "application/json",
			body:          `{"modelVersion":"gemini-2.5-pro","usageMetadata":{"promptTokenCount":200,"cachedContentTokenCount":200,"candidatesTokenCount":10}}`,
			wantIn:        0,
			wantCacheRead: 200,
			wantTotalIn:   200,
			wantOut:       10,
			// 200 * 0.30 + 10 * 10.00 = 60 + 100 = 160
			wantCost: 160,
		},
		{
			name:          "json fully cached prompt with zero output",
			contentType:   "application/json",
			body:          `{"modelVersion":"gemini-2.5-pro","usageMetadata":{"promptTokenCount":200,"cachedContentTokenCount":200}}`,
			wantIn:        0,
			wantCacheRead: 200,
			wantTotalIn:   200,
			wantOut:       0,
			// 200 * 0.30 = 60
			wantCost: 60,
		},
		{
			name:        "sse carries split from same chunk",
			contentType: "text/event-stream",
			body: "" +
				`data: {"modelVersion":"gemini-2.5-pro","usageMetadata":{"promptTokenCount":200,"cachedContentTokenCount":150,"candidatesTokenCount":10}}` + "\n\n" +
				`data: {"modelVersion":"gemini-2.5-pro","usageMetadata":{"promptTokenCount":200,"cachedContentTokenCount":150,"candidatesTokenCount":40}}` + "\n\n",
			wantIn:        50,
			wantCacheRead: 150,
			wantTotalIn:   200,
			wantOut:       40,
			// 50 * 1.25 + 150 * 0.30 + 40 * 10.00 = 62.5 + 45 + 400 = 507.5 -> ceil 508
			wantCost: 508,
		},
		{
			name:        "sse chunk replacing uncached prompt with fully cached prompt does not double count",
			contentType: "text/event-stream",
			body: "" +
				`data: {"modelVersion":"gemini-2.5-pro","usageMetadata":{"promptTokenCount":200,"candidatesTokenCount":10}}` + "\n\n" +
				`data: {"modelVersion":"gemini-2.5-pro","usageMetadata":{"promptTokenCount":200,"cachedContentTokenCount":200,"candidatesTokenCount":40}}` + "\n\n",
			wantIn:        0,
			wantCacheRead: 200,
			wantTotalIn:   200,
			wantOut:       40,
			// 200 * 0.30 + 40 * 10.00 = 60 + 400 = 460
			wantCost: 460,
		},
		{
			name:        "sse output-only chunk preserves earlier prompt split",
			contentType: "text/event-stream",
			body: "" +
				`data: {"modelVersion":"gemini-2.5-pro","usageMetadata":{"promptTokenCount":200,"cachedContentTokenCount":150}}` + "\n\n" +
				`data: {"modelVersion":"gemini-2.5-pro","usageMetadata":{"candidatesTokenCount":40}}` + "\n\n",
			wantIn:        50,
			wantCacheRead: 150,
			wantTotalIn:   200,
			wantOut:       40,
			wantCost:      508,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			parsed := parseUsage("gemini", "/v1beta/models/gemini-2.5-pro:generateContent", []byte(tc.body), tc.contentType)
			if parsed.Outcome != usageOutcomeFinal {
				t.Fatalf("outcome = %v, err = %v, want usageOutcomeFinal", parsed.Outcome, parsed.Err)
			}
			if parsed.Usage.InputTokens != tc.wantIn || parsed.Usage.CacheReadTokens != tc.wantCacheRead || parsed.Usage.OutputTokens != tc.wantOut {
				t.Fatalf("usage = %+v, want in=%d cr=%d out=%d", parsed.Usage, tc.wantIn, tc.wantCacheRead, tc.wantOut)
			}
			if parsed.Usage.TotalInputTokens() != tc.wantTotalIn {
				t.Fatalf("TotalInputTokens = %d, want %d", parsed.Usage.TotalInputTokens(), tc.wantTotalIn)
			}
			settled, cost := settleUsageForEntry("gemini", "gemini-2.5-pro", parsed, reserve, custom)
			if settled.InputTokens != tc.wantIn || settled.CacheReadTokens != tc.wantCacheRead || settled.OutputTokens != tc.wantOut {
				t.Fatalf("settled = %+v, want in=%d cr=%d out=%d", settled, tc.wantIn, tc.wantCacheRead, tc.wantOut)
			}
			if cost != tc.wantCost {
				t.Fatalf("cost = %d, want %d", cost, tc.wantCost)
			}
		})
	}
}

func TestParseGeminiRejectsOutOfRangeAndOverflowCounts(t *testing.T) {
	reserve := domain.BudgetTotals{
		InputTokens:  100,
		OutputTokens: 50,
		CostMicroUSD: estimateReserveMicroUSD("gemini", "gemini-2.5-pro", 100, 50, nil),
	}

	outOfRangeCases := []struct {
		name        string
		contentType string
		body        string
	}{
		{
			name:        "json candidates plus thoughts overflows int64",
			contentType: "application/json",
			body:        fmt.Sprintf(`{"usageMetadata":{"promptTokenCount":100,"candidatesTokenCount":%d,"thoughtsTokenCount":1}}`, math.MaxInt64),
		},
		{
			name:        "json candidates plus thoughts exceeds maxReservedTokens",
			contentType: "application/json",
			body:        fmt.Sprintf(`{"usageMetadata":{"promptTokenCount":100,"candidatesTokenCount":%d,"thoughtsTokenCount":%d}}`, maxReservedTokens/2+1, maxReservedTokens/2),
		},
		{
			name:        "json prompt exceeds maxReservedTokens",
			contentType: "application/json",
			body:        fmt.Sprintf(`{"usageMetadata":{"promptTokenCount":%d,"candidatesTokenCount":10}}`, maxReservedTokens+1),
		},
		{
			name:        "json cached exceeds maxReservedTokens",
			contentType: "application/json",
			body:        fmt.Sprintf(`{"usageMetadata":{"promptTokenCount":100,"cachedContentTokenCount":%d}}`, maxReservedTokens+1),
		},
		{
			name:        "json cached exceeds prompt",
			contentType: "application/json",
			body:        `{"usageMetadata":{"promptTokenCount":100,"cachedContentTokenCount":150,"candidatesTokenCount":10}}`,
		},
		{
			name:        "json prompt plus output exceeds maxReservedTokens",
			contentType: "application/json",
			body:        fmt.Sprintf(`{"usageMetadata":{"promptTokenCount":%d,"candidatesTokenCount":1}}`, maxReservedTokens),
		},
		{
			name:        "sse later chunk overflows after valid chunk",
			contentType: "text/event-stream",
			body: "" +
				`data: {"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":1}}` + "\n\n" +
				fmt.Sprintf(`data: {"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":%d,"thoughtsTokenCount":10}}`, math.MaxInt64) + "\n\n",
		},
		{
			name:        "sse combined prompt and output across chunks exceeds maxReservedTokens",
			contentType: "text/event-stream",
			body: "" +
				fmt.Sprintf(`data: {"usageMetadata":{"promptTokenCount":%d}}`, maxReservedTokens) + "\n\n" +
				`data: {"usageMetadata":{"candidatesTokenCount":1}}` + "\n\n",
		},
	}

	for _, tc := range outOfRangeCases {
		t.Run(tc.name, func(t *testing.T) {
			parsed := parseUsage("gemini", "/v1beta/models/gemini-2.5-pro:generateContent", []byte(tc.body), tc.contentType)
			if parsed.Outcome != usageOutcomeNone {
				t.Fatalf("outcome = %v (usage=%+v), want usageOutcomeNone", parsed.Outcome, parsed.Usage)
			}
			if !errors.Is(parsed.Err, modelusage.ErrOutOfRange) {
				t.Fatalf("err = %v, want modelusage.ErrOutOfRange", parsed.Err)
			}
			settled, cost := settleUsageForEntry("gemini", "gemini-2.5-pro", parsed, reserve, nil)
			if settled.InputTokens != reserve.InputTokens || settled.OutputTokens != reserve.OutputTokens || cost != reserve.CostMicroUSD {
				t.Fatalf("settled = %+v cost=%d, want reserve %+v", settled, cost, reserve)
			}
		})
	}

	t.Run("exact maxReservedTokens total is accepted", func(t *testing.T) {
		body := fmt.Sprintf(
			`{"usageMetadata":{"promptTokenCount":%d,"cachedContentTokenCount":5,"candidatesTokenCount":6,"thoughtsTokenCount":4}}`,
			maxReservedTokens-10,
		)
		parsed := parseUsage("gemini", "/v1beta/models/gemini-2.5-pro:generateContent", []byte(body), "application/json")
		if parsed.Outcome != usageOutcomeFinal {
			t.Fatalf("outcome = %v err = %v, want usageOutcomeFinal", parsed.Outcome, parsed.Err)
		}
		if parsed.Usage.InputTokens != maxReservedTokens-15 || parsed.Usage.CacheReadTokens != 5 || parsed.Usage.OutputTokens != 10 {
			t.Fatalf("usage = %+v, want in=%d cr=5 out=10", parsed.Usage, maxReservedTokens-15)
		}
	})
}
