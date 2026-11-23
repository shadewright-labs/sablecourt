package model

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/shadewright-labs/sablecourt/internal/config"
	"github.com/shadewright-labs/sablecourt/internal/domain"
	"github.com/shadewright-labs/sablecourt/internal/secret"
	"github.com/shadewright-labs/sablecourt/internal/store"
)

const TokenPrefix = "sc_task_"

type UpstreamConfig struct {
	BaseURL         string
	SecretName      string
	RequireSecret   bool
	AnthropicVer    string
}

type Options struct {
	Store      store.Store
	Secrets    secret.SecretStore
	Bundle     *config.Bundle
	Upstreams  map[string]UpstreamConfig
	GatewayURL string // Optional upstream AI gateway (e.g. Linux Foundation agentgateway or Agent Router)
	Pricing    map[string]PricePerMillionTokens
	Client     *http.Client
	Now        func() time.Time
}

type Gate struct {
	mu        sync.RWMutex
	store     store.Store
	secrets   secret.SecretStore
	bundle    *config.Bundle
	upstreams map[string]UpstreamConfig
	pricing   map[string]PricePerMillionTokens
	client    *http.Client
	now       func() time.Time
}

func (g *Gate) SetBundle(b *config.Bundle) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.bundle = b
}

func (g *Gate) currentBundle() *config.Bundle {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.bundle
}

func DefaultUpstreams() map[string]UpstreamConfig {
	return map[string]UpstreamConfig{
		"openai": {
			BaseURL:       "https://api.openai.com",
			SecretName:    "openai_api_key",
			RequireSecret: true,
		},
		"anthropic": {
			BaseURL:       "https://api.anthropic.com",
			SecretName:    "anthropic_api_key",
			RequireSecret: true,
			AnthropicVer:  "2023-06-01",
		},
		"gemini": {
			BaseURL:       "https://generativelanguage.googleapis.com",
			SecretName:    "gemini_api_key",
			RequireSecret: true,
		},
		"local": {
			BaseURL:       "http://127.0.0.1:11434",
			SecretName:    "local_model_api_key",
			RequireSecret: false,
		},
	}
}

func New(opts Options) *Gate {
	upstreams := opts.Upstreams
	if len(upstreams) == 0 {
		upstreams = DefaultUpstreams()
	}
	if gw := strings.TrimRight(strings.TrimSpace(opts.GatewayURL), "/"); gw != "" {
		routed := make(map[string]UpstreamConfig, len(upstreams))
		for k, cfg := range upstreams {
			cfg.BaseURL = gw
			cfg.RequireSecret = false
			routed[k] = cfg
		}
		upstreams = routed
	}
	client := opts.Client
	if client == nil {
		client = &http.Client{Timeout: 5 * time.Minute}
	}
	nowFn := opts.Now
	if nowFn == nil {
		nowFn = func() time.Time { return time.Now().UTC() }
	}
	return &Gate{
		store:     opts.Store,
		secrets:   opts.Secrets,
		bundle:    opts.Bundle,
		upstreams: upstreams,
		pricing:   opts.Pricing,
		client:    client,
		now:       nowFn,
	}
}

// MintTaskToken generates a scoped task bearer token and stores only its SHA-256 hash.
func MintTaskToken(ctx context.Context, st store.Store, taskID, workloadName string, ttl time.Duration) (string, error) {
	if ttl <= 0 {
		ttl = time.Hour
	}
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("generate task token: %w", err)
	}
	token := TokenPrefix + hex.EncodeToString(raw[:])
	now := time.Now().UTC()
	lease := domain.Lease{
		TokenSHA256:  HashToken(token),
		TaskID:       taskID,
		WorkloadName: workloadName,
		ExpiresAt:    now.Add(ttl),
		CreatedAt:    now,
	}
	if err := st.CreateLease(ctx, lease); err != nil {
		return "", err
	}
	return token, nil
}

func HashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// Handler returns an http.Handler expecting paths prefixed with /openai/, /anthropic/, /gemini/, or /local/.
func (g *Gate) Handler() http.Handler {
	return http.HandlerFunc(g.serveHTTP)
}

func (g *Gate) serveHTTP(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	provider, upstreamPath, ok := routeProvider(r.URL.Path)
	if !ok {
		writeErr(w, http.StatusNotFound, "unknown model gate route")
		return
	}

	token := extractTaskToken(r)
	if !strings.HasPrefix(token, TokenPrefix) {
		g.audit(ctx, "", "", "model_gate:"+provider, "denied", `{"reason":"missing_task_token"}`)
		writeErr(w, http.StatusUnauthorized, "valid task token required")
		return
	}

	now := g.now()
	lease, err := g.store.GetActiveLeaseByTokenHash(ctx, HashToken(token), now)
	if err != nil {
		g.audit(ctx, "", "", "model_gate:"+provider, "denied", `{"reason":"invalid_or_expired_lease"}`)
		writeErr(w, http.StatusUnauthorized, "invalid, expired, or revoked task token")
		return
	}

	bundle := g.currentBundle()
	if bundle == nil {
		writeErr(w, http.StatusServiceUnavailable, "no workload bundle loaded")
		return
	}
	workload, exists := bundle.Workloads[lease.WorkloadName]
	if !exists {
		g.audit(ctx, lease.TaskID, lease.WorkloadName, "model_gate:"+provider, "denied", `{"reason":"unknown_workload"}`)
		writeErr(w, http.StatusForbidden, "workload not declared")
		return
	}

	if workload.Spec.Class == domain.ClassDeterministicController || !workload.Spec.Grants.ModelGate.Allowed {
		g.audit(ctx, lease.TaskID, lease.WorkloadName, "model_gate:"+provider, "denied", `{"reason":"no_model_gate_grant"}`)
		writeErr(w, http.StatusForbidden, "workload has no Model Gate grant")
		return
	}

	if len(workload.Spec.Grants.ModelGate.Providers) > 0 && !slices.Contains(workload.Spec.Grants.ModelGate.Providers, provider) {
		g.audit(ctx, lease.TaskID, lease.WorkloadName, "model_gate:"+provider, "denied", `{"reason":"provider_not_allowed"}`)
		writeErr(w, http.StatusForbidden, "provider not granted to workload")
		return
	}

	budget, ok := bundle.Budgets[workload.Spec.Grants.ModelGate.BudgetRef]
	if !ok {
		g.audit(ctx, lease.TaskID, lease.WorkloadName, "model_gate:"+provider, "denied", `{"reason":"missing_budget"}`)
		writeErr(w, http.StatusForbidden, "workload budget not found")
		return
	}

	if exceeded, reason, err := g.checkBudget(ctx, lease.TaskID, lease.WorkloadName, budget, now); err != nil {
		writeErr(w, http.StatusInternalServerError, "budget check failed")
		return
	} else if exceeded {
		g.audit(ctx, lease.TaskID, lease.WorkloadName, "model_gate:"+provider, "denied", fmt.Sprintf(`{"reason":%q}`, reason))
		writeErr(w, http.StatusTooManyRequests, "budget exceeded: "+reason)
		return
	}

	upCfg, ok := g.upstreams[provider]
	if !ok {
		writeErr(w, http.StatusBadGateway, "provider upstream not configured")
		return
	}

	var upstreamKey string
	if upCfg.SecretName != "" && g.secrets != nil {
		key, err := g.secrets.Get(ctx, upCfg.SecretName)
		if err != nil {
			if upCfg.RequireSecret || !errors.Is(err, secret.ErrSecretNotFound) {
				g.audit(ctx, lease.TaskID, lease.WorkloadName, "model_gate:"+provider, "error", `{"reason":"missing_upstream_secret"}`)
				writeErr(w, http.StatusBadGateway, "upstream provider credential unavailable")
				return
			}
		} else {
			upstreamKey = key
		}
	} else if upCfg.RequireSecret {
		writeErr(w, http.StatusBadGateway, "upstream provider credential unavailable")
		return
	}

	reqBody, err := io.ReadAll(io.LimitReader(r.Body, 16<<20))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "failed to read request body")
		return
	}
	reqModel := extractRequestModel(provider, upstreamPath, reqBody)

	targetURL, err := joinUpstreamURL(upCfg.BaseURL, upstreamPath, r.URL.RawQuery)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "invalid upstream URL")
		return
	}

	upReq, err := http.NewRequestWithContext(ctx, r.Method, targetURL, bytes.NewReader(reqBody))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to build upstream request")
		return
	}
	copySafeRequestHeaders(upReq.Header, r.Header)
	injectUpstreamAuth(upReq.Header, provider, upstreamKey, upCfg.AnthropicVer)

	upResp, err := g.client.Do(upReq)
	activeProvider := provider
	if err != nil || isFallbackStatus(upResp.StatusCode) {
		if fbProv, fbResp, fbErr := g.tryFallback(ctx, r, provider, upstreamPath, reqBody, workload); fbErr == nil && fbResp != nil {
			if upResp != nil && upResp.Body != nil {
				_ = upResp.Body.Close()
			}
			upResp = fbResp
			err = nil
			activeProvider = fbProv
			g.audit(ctx, lease.TaskID, lease.WorkloadName, "model.fallback", "ok", fmt.Sprintf(`{"from":%q,"to":%q}`, provider, fbProv))
			w.Header().Set("X-Sablecourt-Fallback-Provider", fbProv)
		}
	}
	if err != nil {
		g.audit(ctx, lease.TaskID, lease.WorkloadName, "model_gate:"+provider, "error", `{"reason":"upstream_request_failed"}`)
		writeErr(w, http.StatusBadGateway, "upstream request failed")
		return
	}
	defer upResp.Body.Close()

	copyResponseHeaders(w.Header(), upResp.Header)
	w.WriteHeader(upResp.StatusCode)

	var captured bytes.Buffer
	flusher, canFlush := w.(http.Flusher)
	buf := make([]byte, 32*1024)
	for {
		n, readErr := upResp.Body.Read(buf)
		if n > 0 {
			chunk := buf[:n]
			_, _ = w.Write(chunk)
			if canFlush {
				flusher.Flush()
			}
			if captured.Len() < 8<<20 {
				captured.Write(chunk)
			}
		}
		if readErr != nil {
			break
		}
	}

	if upResp.StatusCode >= 200 && upResp.StatusCode < 300 {
		usage := parseUsage(activeProvider, captured.Bytes(), upResp.Header.Get("Content-Type"))
		modelName := usage.Model
		if modelName == "" {
			modelName = reqModel
		}
		costMicro := EstimateMicroUSD(activeProvider, modelName, usage.InputTokens, usage.OutputTokens, g.pricing)
		_ = g.store.RecordBudgetUsage(ctx, domain.BudgetEntry{
			TaskID:       lease.TaskID,
			WorkloadName: lease.WorkloadName,
			Provider:     activeProvider,
			Model:        modelName,
			InputTokens:  usage.InputTokens,
			OutputTokens: usage.OutputTokens,
			CostMicroUSD: costMicro,
			CreatedAt:    g.now(),
		})
		g.audit(ctx, lease.TaskID, lease.WorkloadName, "model_gate:"+activeProvider, "allowed",
			fmt.Sprintf(`{"model":%q,"input_tokens":%d,"output_tokens":%d,"cost_microusd":%d}`,
				modelName, usage.InputTokens, usage.OutputTokens, costMicro))
	}
}

func isFallbackStatus(code int) bool {
	return code == http.StatusTooManyRequests || code == http.StatusBadGateway || code == http.StatusServiceUnavailable || code == http.StatusGatewayTimeout
}

func (g *Gate) tryFallback(ctx context.Context, r *http.Request, failedProvider, upstreamPath string, reqBody []byte, wl domain.Workload) (string, *http.Response, error) {
	if failedProvider != "openai" && failedProvider != "local" {
		return "", nil, errors.New("no wire-compatible fallback")
	}
	for _, cand := range wl.Spec.Grants.ModelGate.Providers {
		if cand == failedProvider || (cand != "openai" && cand != "local") {
			continue
		}
		upCfg, ok := g.upstreams[cand]
		if !ok {
			continue
		}
		var key string
		if upCfg.SecretName != "" && g.secrets != nil {
			k, err := g.secrets.Get(ctx, upCfg.SecretName)
			if err != nil {
				if upCfg.RequireSecret {
					continue
				}
			} else {
				key = k
			}
		} else if upCfg.RequireSecret {
			continue
		}
		targetURL, err := joinUpstreamURL(upCfg.BaseURL, upstreamPath, r.URL.RawQuery)
		if err != nil {
			continue
		}
		req, err := http.NewRequestWithContext(ctx, r.Method, targetURL, bytes.NewReader(reqBody))
		if err != nil {
			continue
		}
		copySafeRequestHeaders(req.Header, r.Header)
		injectUpstreamAuth(req.Header, cand, key, upCfg.AnthropicVer)
		resp, err := g.client.Do(req)
		if err == nil && resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return cand, resp, nil
		}
		if resp != nil && resp.Body != nil {
			_ = resp.Body.Close()
		}
	}
	return "", nil, errors.New("fallback unavailable")
}

func (g *Gate) checkBudget(ctx context.Context, taskID, workloadName string, b domain.Budget, now time.Time) (bool, string, error) {
	taskTotals, err := g.store.TaskBudgetTotals(ctx, taskID)
	if err != nil {
		return false, "", err
	}
	if b.Spec.PerTask.MaxInputTokens > 0 && taskTotals.InputTokens >= b.Spec.PerTask.MaxInputTokens {
		return true, "per_task_input_tokens", nil
	}
	if b.Spec.PerTask.MaxOutputTokens > 0 && taskTotals.OutputTokens >= b.Spec.PerTask.MaxOutputTokens {
		return true, "per_task_output_tokens", nil
	}
	if b.Spec.PerTask.MaxCostUSD > 0 && taskTotals.CostMicroUSD >= int64(b.Spec.PerTask.MaxCostUSD*1_000_000) {
		return true, "per_task_cost_usd", nil
	}

	if b.Spec.Daily.MaxCostUSD > 0 {
		dayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
		dailyTotals, err := g.store.WorkloadDailyBudgetTotals(ctx, workloadName, dayStart)
		if err != nil {
			return false, "", err
		}
		if dailyTotals.CostMicroUSD >= int64(b.Spec.Daily.MaxCostUSD*1_000_000) {
			return true, "daily_cost_usd", nil
		}
	}
	return false, "", nil
}

func (g *Gate) audit(ctx context.Context, taskID, workload, action, outcome, detailJSON string) {
	_ = g.store.AppendAudit(ctx, domain.AuditRecord{
		Category:     "model_gate",
		TaskID:       taskID,
		WorkloadName: workload,
		Action:       action,
		Outcome:      outcome,
		DetailJSON:   detailJSON,
		CreatedAt:    g.now(),
	})
}

func routeProvider(path string) (provider, upstreamPath string, ok bool) {
	for _, p := range []string{"openai", "anthropic", "gemini", "local"} {
		prefix := "/" + p + "/"
		if strings.HasPrefix(path, prefix) {
			rest := strings.TrimPrefix(path, "/"+p)
			switch p {
			case "openai", "local":
				if rest == "/v1/chat/completions" || rest == "/v1/responses" {
					return p, rest, true
				}
			case "anthropic":
				if rest == "/v1/messages" {
					return p, rest, true
				}
			case "gemini":
				if strings.HasPrefix(rest, "/v1beta/models/") &&
					(strings.HasSuffix(rest, ":generateContent") || strings.HasSuffix(rest, ":streamGenerateContent")) {
					return p, rest, true
				}
			}
			return "", "", false
		}
	}
	return "", "", false
}

func extractTaskToken(r *http.Request) string {
	if auth := r.Header.Get("Authorization"); strings.HasPrefix(auth, "Bearer ") {
		return strings.TrimSpace(strings.TrimPrefix(auth, "Bearer "))
	}
	if key := strings.TrimSpace(r.Header.Get("x-api-key")); key != "" {
		return key
	}
	if key := strings.TrimSpace(r.Header.Get("x-goog-api-key")); key != "" {
		return key
	}
	return ""
}

func copySafeRequestHeaders(dst, src http.Header) {
	for k, vals := range src {
		lower := strings.ToLower(k)
		switch lower {
		case "authorization", "x-api-key", "x-goog-api-key", "host", "content-length", "connection":
			continue
		}
		for _, v := range vals {
			dst.Add(k, v)
		}
	}
}

func injectUpstreamAuth(h http.Header, provider, key, anthropicVer string) {
	switch provider {
	case "openai", "local":
		if key != "" {
			h.Set("Authorization", "Bearer "+key)
		}
	case "anthropic":
		if key != "" {
			h.Set("x-api-key", key)
		}
		if h.Get("anthropic-version") == "" && anthropicVer != "" {
			h.Set("anthropic-version", anthropicVer)
		}
	case "gemini":
		if key != "" {
			h.Set("x-goog-api-key", key)
		}
	}
}

func copyResponseHeaders(dst, src http.Header) {
	for k, vals := range src {
		lower := strings.ToLower(k)
		if lower == "content-length" || lower == "connection" || lower == "transfer-encoding" {
			continue
		}
		for _, v := range vals {
			dst.Add(k, v)
		}
	}
}

func joinUpstreamURL(baseURL, path, rawQuery string) (string, error) {
	u, err := url.Parse(baseURL)
	if err != nil {
		return "", err
	}
	u.Path = strings.TrimRight(u.Path, "/") + path
	u.RawQuery = rawQuery
	return u.String(), nil
}

func extractRequestModel(provider, upstreamPath string, body []byte) string {
	if provider == "gemini" {
		trimmed := strings.TrimPrefix(upstreamPath, "/v1beta/models/")
		if idx := strings.Index(trimmed, ":"); idx > 0 {
			return trimmed[:idx]
		}
	}
	var req struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(body, &req); err == nil {
		return req.Model
	}
	return ""
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
