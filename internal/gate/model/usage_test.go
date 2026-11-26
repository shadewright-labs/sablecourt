package model

import (
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
	reserve := domain.BudgetTotals{InputTokens: 100, OutputTokens: 50, CostMicroUSD: 1350}

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
				"openai":    {BaseURL: "http://openai.pipe", SecretName: "openai_api_key", RequireSecret: true},
				"anthropic": {BaseURL: "http://anthropic.pipe", SecretName: "anthropic_api_key", RequireSecret: true, AnthropicVer: "2023-06-01"},
				"gemini":    {BaseURL: "http://gemini.pipe", SecretName: "gemini_api_key", RequireSecret: true},
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

// TestModelGatePinsAnthropicCompactionUndercount pins the known limitation in
// github.com/shadewright-labs/armory/modelusage (shadewright-labs/armory issue 26):
// the gate forwards anthropic-beta unchanged and does not yet count
// usage.iterations entries of type compaction.
func TestModelGatePinsAnthropicCompactionUndercount(t *testing.T) {
	ctx := context.Background()
	var gotBetaHeader string
	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBetaHeader = r.Header.Get("anthropic-beta")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id": "msg_compact",
			"type": "message",
			"model": "claude-sonnet-4-5",
			"usage": {
				"input_tokens": 90,
				"output_tokens": 20,
				"iterations": [
					{"type": "message", "input_tokens": 90, "output_tokens": 20},
					{"type": "compaction", "input_tokens": 80, "output_tokens": 10}
				]
			}
		}`))
	})
	gate, st := setupGateTest(t, map[string]UpstreamConfig{
		"anthropic": {BaseURL: "https://anthropic.test", SecretName: "anthropic_api_key", RequireSecret: true, AnthropicVer: "2023-06-01"},
	}, handlerTransport(upstream))

	if err := st.CreateTask(ctx, domain.Task{
		ID: "task-compact", IdempotencyKey: "task-compact", WorkloadName: "ai-agent", State: domain.TaskStateWorking,
	}); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	tok, err := MintTaskToken(ctx, st, "task-compact", "ai-agent", time.Hour)
	if err != nil {
		t.Fatalf("MintTaskToken: %v", err)
	}

	rec := doGateRequest(gate.Handler(), http.MethodPost, "/anthropic/v1/messages", `{"model":"claude-sonnet-4-5","max_tokens":50}`, map[string]string{
		"x-api-key":      tok,
		"anthropic-beta": "compact-2026-01-12",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s, want 200", rec.Code, rec.Body.String())
	}
	if gotBetaHeader != "compact-2026-01-12" {
		t.Errorf("upstream anthropic-beta = %q, want %q (must not be stripped silently)", gotBetaHeader, "compact-2026-01-12")
	}

	// Pinned known under-count: only the top-level 90 input / 20 output tokens are
	// counted; the 80 input / 10 output compaction iteration tokens are omitted
	// until shadewright-labs/armory issue 26 lands.
	totals, err := st.TaskBudgetTotals(ctx, "task-compact")
	if err != nil {
		t.Fatalf("TaskBudgetTotals: %v", err)
	}
	if totals.InputTokens != 90 || totals.OutputTokens != 20 {
		t.Errorf("totals = %+v, want pinned top-level in=90 out=20", totals)
	}
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
