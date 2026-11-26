package model

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/shadewright-labs/sablecourt/internal/domain"
	"github.com/shadewright-labs/sablecourt/internal/secret"
)

func TestModelGateRefusals(t *testing.T) {
	ctx := context.Background()
	upstreamCalls := 0
	upstream := handlerTransport(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		upstreamCalls++
		w.WriteHeader(http.StatusOK)
	}))
	gate, st := setupGateTest(t, nil, upstream)
	bundle := gate.currentBundle()

	add := func(name string, w domain.Workload) string {
		w.Metadata.Name = name
		bundle.Workloads[name] = w
		_ = st.UpsertWorkload(ctx, domain.WorkloadRecord{Name: name, Zone: domain.ZoneRuntime, Class: w.Spec.Class, ManifestDigest: "t", AgentCardJSON: "{}"})
		_ = st.CreateTask(ctx, domain.Task{ID: "task-" + name, IdempotencyKey: "k-" + name, WorkloadName: name, State: domain.TaskStateWorking})
		tok, err := MintTaskToken(ctx, st, "task-"+name, name, time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		return tok
	}
	agent := add("ai-agent", bundle.Workloads["ai-agent"])
	noGrant := add("no-grant", domain.Workload{Spec: domain.WorkloadSpec{Class: domain.ClassLongLivedAgent}})
	noBudget := add("no-budget", domain.Workload{Spec: domain.WorkloadSpec{Class: domain.ClassLongLivedAgent, Grants: domain.GrantSet{ModelGate: domain.ModelGateGrant{Allowed: true, BudgetRef: "missing"}}}})
	anyProvider := add("any-provider", domain.Workload{Spec: domain.WorkloadSpec{Class: domain.ClassLongLivedAgent, Grants: domain.GrantSet{ModelGate: domain.ModelGateGrant{Allowed: true, BudgetRef: "agent-budget"}}}})
	ghost := add("ghost", domain.Workload{})
	delete(bundle.Workloads, "ghost")

	body := `{"model":"gpt-4o-mini","messages":[{"role":"user","content":"hi"}],"max_tokens":10}`
	cases := []struct {
		name    string
		path    string
		headers map[string]string
		code    int
		msg     string
		reason  string
	}{
		{"unknown route", "/mistral/v1/chat", map[string]string{"Authorization": "Bearer " + agent}, http.StatusNotFound, "unknown model gate route", ""},
		{"no token", "/openai/v1/chat/completions", nil, http.StatusUnauthorized, "valid task token required", "missing_task_token"},
		{"provider key instead of task token", "/openai/v1/chat/completions", map[string]string{"Authorization": "Bearer sk-" + strings.Repeat("x", 20)}, http.StatusUnauthorized, "valid task token required", "missing_task_token"},
		{"unknown task token", "/openai/v1/chat/completions", map[string]string{"x-api-key": TokenPrefix + "nope"}, http.StatusUnauthorized, "invalid, expired, or revoked", "invalid_or_expired_lease"},
		{"undeclared workload", "/openai/v1/chat/completions", map[string]string{"x-goog-api-key": ghost}, http.StatusForbidden, "workload not declared", "unknown_workload"},
		{"no model grant", "/openai/v1/chat/completions", map[string]string{"Authorization": "Bearer " + noGrant}, http.StatusForbidden, "no Model Gate grant", "no_model_gate_grant"},
		{"provider not granted", "/local/v1/chat/completions", map[string]string{"Authorization": "Bearer " + agent}, http.StatusForbidden, "provider not granted", "provider_not_allowed"},
		{"budget not declared", "/openai/v1/chat/completions", map[string]string{"Authorization": "Bearer " + noBudget}, http.StatusForbidden, "budget not found", "missing_budget"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := doGateRequest(gate.Handler(), http.MethodPost, tc.path, body, tc.headers)
			if rec.Code != tc.code || !strings.Contains(rec.Body.String(), tc.msg) {
				t.Fatalf("got %d %s, want %d containing %q", rec.Code, rec.Body.String(), tc.code, tc.msg)
			}
			if tc.reason != "" {
				recs, _ := st.ListAudit(ctx, 1)
				if len(recs) != 1 || recs[0].Outcome != "denied" || !strings.Contains(recs[0].DetailJSON, tc.reason) {
					t.Fatalf("audit = %+v, want denied %s", recs, tc.reason)
				}
			}
		})
	}
	if upstreamCalls != 0 {
		t.Fatalf("refused requests reached the upstream %d times", upstreamCalls)
	}

	// A workload with no provider list may use any routed provider; the local one
	// needs no credential.
	rec := doGateRequest(gate.Handler(), http.MethodPost, "/local/v1/chat/completions", body, map[string]string{"Authorization": "Bearer " + anyProvider})
	if rec.Code != http.StatusOK || upstreamCalls != 1 {
		t.Fatalf("local provider: %d %s (calls %d)", rec.Code, rec.Body.String(), upstreamCalls)
	}

	gate.SetBundle(nil)
	rec = doGateRequest(gate.Handler(), http.MethodPost, "/openai/v1/chat/completions", body, map[string]string{"Authorization": "Bearer " + agent})
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("no bundle: %d %s", rec.Code, rec.Body.String())
	}
}

func TestModelGateUpstreamCredentialRefusals(t *testing.T) {
	ctx := context.Background()
	body := `{"model":"gpt-4o-mini","messages":[],"max_tokens":300}`
	cases := []struct {
		name      string
		upstreams map[string]UpstreamConfig
		secrets   func(*Gate)
		code      int
		msg       string
	}{
		{"provider without upstream", map[string]UpstreamConfig{"anthropic": {BaseURL: "https://a.example"}}, nil, http.StatusBadGateway, "upstream not configured"},
		{"required secret missing", map[string]UpstreamConfig{"openai": {BaseURL: "https://o.example", SecretName: "absent_key", RequireSecret: true}}, nil, http.StatusBadGateway, "credential unavailable"},
		{"required secret without a store", map[string]UpstreamConfig{"openai": {BaseURL: "https://o.example", SecretName: "openai_api_key", RequireSecret: true}}, func(g *Gate) { g.secrets = nil }, http.StatusBadGateway, "credential unavailable"},
		{"secret store failure on an optional secret", map[string]UpstreamConfig{"openai": {BaseURL: "https://o.example", SecretName: "k"}}, func(g *Gate) { g.secrets = failingSecrets{} }, http.StatusBadGateway, "credential unavailable"},
		{"base URL without scheme", map[string]UpstreamConfig{"openai": {BaseURL: "o.example"}}, nil, http.StatusBadGateway, "invalid upstream URL"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			gate, st := setupGateTest(t, tc.upstreams, handlerTransport(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				calls++
				w.WriteHeader(http.StatusOK)
			})))
			if tc.secrets != nil {
				tc.secrets(gate)
			}
			_ = st.CreateTask(ctx, domain.Task{ID: "t1", IdempotencyKey: "k1", WorkloadName: "ai-agent", State: domain.TaskStateWorking})
			tok, err := MintTaskToken(ctx, st, "t1", "ai-agent", time.Hour)
			if err != nil {
				t.Fatal(err)
			}
			rec := doGateRequest(gate.Handler(), http.MethodPost, "/openai/v1/chat/completions", body, map[string]string{"Authorization": "Bearer " + tok})
			if rec.Code != tc.code || !strings.Contains(rec.Body.String(), tc.msg) || calls != 0 {
				t.Fatalf("got %d %s (calls %d), want %d %q", rec.Code, rec.Body.String(), calls, tc.code, tc.msg)
			}
			// The reservation taken before the refusal is released: a leaked one would
			// make the same request exceed the per-task output budget the second time.
			again := doGateRequest(gate.Handler(), http.MethodPost, "/openai/v1/chat/completions", body, map[string]string{"Authorization": "Bearer " + tok})
			if again.Code != tc.code {
				t.Fatalf("repeated request got %d %s, want %d", again.Code, again.Body.String(), tc.code)
			}
			if used, err := st.TaskBudgetTotals(ctx, "t1"); err != nil || used != (domain.BudgetTotals{}) {
				t.Fatalf("refused request left budget used or reserved: %+v, %v", used, err)
			}
		})
	}
}

type failingSecrets struct{ secret.SecretStore }

func (failingSecrets) Get(context.Context, string) (string, error) {
	return "", context.DeadlineExceeded
}

func TestNewRoutesEveryProviderThroughGateway(t *testing.T) {
	g := New(Options{GatewayURL: " https://gw.example/ "})
	if len(g.upstreams) != len(DefaultUpstreams()) {
		t.Fatalf("upstreams = %v", g.upstreams)
	}
	for name, cfg := range g.upstreams {
		if cfg.BaseURL != "https://gw.example" || cfg.RequireSecret {
			t.Fatalf("%s = %+v, want the gateway without a required secret", name, cfg)
		}
	}
	if g.now().Location() != time.UTC {
		t.Fatal("default clock is not UTC")
	}
}

func TestMintTaskTokenUntilRefusesPastExpiry(t *testing.T) {
	now := time.Now()
	if _, err := MintTaskTokenUntil(context.Background(), nil, "t", "w", now, now); err == nil {
		t.Fatal("minted a token that is already expired")
	}
}
