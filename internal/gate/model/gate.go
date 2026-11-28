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
	"path"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/shadewright-labs/sablecourt/internal/config"
	"github.com/shadewright-labs/sablecourt/internal/domain"
	"github.com/shadewright-labs/sablecourt/internal/gatehttp"
	"github.com/shadewright-labs/sablecourt/internal/secret"
	"github.com/shadewright-labs/sablecourt/internal/store"
)

const TokenPrefix = "sc_task_"

type UpstreamConfig struct {
	BaseURL       string
	SecretName    string
	RequireSecret bool
	AnthropicVer  string
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
		"openai": { // #nosec G101 -- Gate.serveHTTP looks up the openai key in SecretStore by this name.
			BaseURL:       "https://api.openai.com",
			SecretName:    "openai_api_key",
			RequireSecret: true,
		},
		"anthropic": { // #nosec G101 -- Gate.serveHTTP looks up the anthropic key in SecretStore by this name.
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
		"local": { // #nosec G101 -- Gate.serveHTTP looks up the local key in SecretStore by this name.
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
	client := gatehttp.NewClient(opts.Client, 5*time.Minute)
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
	now := time.Now().UTC()
	return MintTaskTokenUntil(ctx, st, taskID, workloadName, now, now.Add(ttl))
}

// MintTaskTokenUntil generates a scoped task bearer token with an explicit expiry bound.
func MintTaskTokenUntil(ctx context.Context, st store.Store, taskID, workloadName string, now, expiresAt time.Time) (string, error) {
	return MintPinnedTaskTokenUntil(ctx, st, taskID, workloadName, now, expiresAt, nil)
}

// MintPinnedTaskTokenUntil is MintTaskTokenUntil for a task whose context
// packet carried learnings: a non-nil pinned list is the provider set the
// packet was admitted under, and the lease reaches no other provider, even
// after a reload widens the workload grant (ADR-0012).
func MintPinnedTaskTokenUntil(ctx context.Context, st store.Store, taskID, workloadName string, now, expiresAt time.Time, pinned []string) (string, error) {
	now = now.UTC()
	expiresAt = expiresAt.UTC()
	if !expiresAt.After(now) {
		return "", errors.New("task lease expiry must be in the future")
	}
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("generate task token: %w", err)
	}
	token := TokenPrefix + hex.EncodeToString(raw[:])
	lease := domain.Lease{
		TokenSHA256:  HashToken(token),
		TaskID:       taskID,
		WorkloadName: workloadName,
		ExpiresAt:    expiresAt,
		CreatedAt:    now,
	}
	if pinned != nil {
		lease.PinnedProviders = slices.Clone(pinned)
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

	// A packet that carried learnings was admitted for one provider set. A
	// reload can widen the grant, so this lease, including its fallbacks,
	// stays inside the pinned set (ADR-0012).
	if lease.PinnedProviders != nil {
		if !slices.Contains(lease.PinnedProviders, provider) {
			g.audit(ctx, lease.TaskID, lease.WorkloadName, "model_gate:"+provider, "denied", `{"reason":"provider_not_admitted_with_learnings"}`)
			writeErr(w, http.StatusForbidden, "provider not admitted for this task's context packet")
			return
		}
		workload.Spec.Grants.ModelGate.Providers = pinnedGrant(workload.Spec.Grants.ModelGate.Providers, lease.PinnedProviders)
	}

	budget, ok := bundle.Budgets[workload.Spec.Grants.ModelGate.BudgetRef]
	if !ok {
		g.audit(ctx, lease.TaskID, lease.WorkloadName, "model_gate:"+provider, "denied", `{"reason":"missing_budget"}`)
		writeErr(w, http.StatusForbidden, "workload budget not found")
		return
	}

	reqBody, err := io.ReadAll(io.LimitReader(r.Body, maxRequestBodyBytes))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "failed to read request body")
		return
	}
	reqModel := extractRequestModel(provider, upstreamPath, reqBody)
	reserve := estimateRequestReserve(provider, reqModel, reqBody, g.pricing, g.fallbackProviders(provider, workload)...)

	// Take a fresh timestamp: the body read above is not bounded by time, and the
	// reservation lifetime must start when the reservation is inserted.
	reservationID, exceeded, reason, err := g.reserveBudget(ctx, lease.TaskID, lease.WorkloadName, budget, reserve, g.now())
	if errors.Is(err, errInvalidChildBudgetCeiling) {
		g.audit(ctx, lease.TaskID, lease.WorkloadName, "model_gate:"+provider, "denied", `{"reason":"invalid_child_budget_ceiling"}`)
		writeErr(w, http.StatusForbidden, errInvalidChildBudgetCeiling.Error())
		return
	} else if err != nil {
		writeErr(w, http.StatusInternalServerError, "budget check failed")
		return
	} else if exceeded {
		g.audit(ctx, lease.TaskID, lease.WorkloadName, "model_gate:"+provider, "denied", fmt.Sprintf(`{"reason":%q}`, reason))
		writeErr(w, http.StatusTooManyRequests, "budget exceeded: "+reason)
		return
	}
	releaseReservation := true
	defer func() {
		if releaseReservation {
			if err := g.settleReservation(ctx, reservationID, nil); err != nil {
				g.audit(context.WithoutCancel(ctx), lease.TaskID, lease.WorkloadName, "model_gate:"+provider, "error", `{"reason":"budget_release_failed"}`)
			}
		}
	}()

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

	upResp, err := g.client.Do(upReq) // #nosec G704 -- joinUpstreamURL pins the configured provider host and scheme, and gatehttp.NewClient refuses redirects.
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
	buf := make([]byte, upstreamReadChunkBytes)
	for {
		n, readErr := upResp.Body.Read(buf)
		if n > 0 {
			chunk := buf[:n]
			_, _ = w.Write(chunk)
			if canFlush {
				flusher.Flush()
			}
			// Keep up to maxCapturedResponseBytes + 1 bytes so oversized
			// responses are detected deterministically by modelusage.Limits.
			if captured.Len() <= maxCapturedResponseBytes {
				remain := maxCapturedResponseBytes + 1 - captured.Len()
				if len(chunk) > remain {
					captured.Write(chunk[:remain])
				} else {
					captured.Write(chunk)
				}
			}
		}
		if readErr != nil {
			break
		}
	}

	if upResp.StatusCode >= 200 && upResp.StatusCode < 300 {
		// Keep the reservation if actual usage cannot be recorded.
		releaseReservation = false
		parsed := parseUsage(activeProvider, upstreamPath, captured.Bytes(), upResp.Header.Get("Content-Type"))
		settled, costMicro := settleUsageForEntry(activeProvider, reqModel, parsed, reserve, g.pricing)
		modelName := settled.Model
		if modelName == "" {
			modelName = reqModel
		}
		entry := domain.BudgetEntry{
			TaskID:           lease.TaskID,
			WorkloadName:     lease.WorkloadName,
			Provider:         activeProvider,
			Model:            modelName,
			InputTokens:      settled.InputTokens,
			CacheWriteTokens: settled.CacheWriteTokens,
			CacheReadTokens:  settled.CacheReadTokens,
			OutputTokens:     settled.OutputTokens,
			CostMicroUSD:     costMicro,
			CreatedAt:        g.now(),
		}
		if err := g.settleReservation(ctx, reservationID, &entry); err != nil {
			g.audit(ctx, lease.TaskID, lease.WorkloadName, "model_gate:"+activeProvider, "error", `{"reason":"budget_settlement_failed"}`)
			return
		}
		g.audit(ctx, lease.TaskID, lease.WorkloadName, "model_gate:"+activeProvider, "allowed",
			fmt.Sprintf(`{"model":%q,"input_tokens":%d,"cache_write_tokens":%d,"cache_read_tokens":%d,"output_tokens":%d,"cost_microusd":%d}`,
				modelName, settled.InputTokens, settled.CacheWriteTokens, settled.CacheReadTokens, settled.OutputTokens, costMicro))
	}
}

func isFallbackStatus(code int) bool {
	return code == http.StatusTooManyRequests || code == http.StatusBadGateway || code == http.StatusServiceUnavailable || code == http.StatusGatewayTimeout
}

// pinnedGrant narrows granted to the providers in pinned. An empty grant
// allows every provider, so it narrows to pinned itself. The caller has
// checked that the requested provider is in both, so the result is never
// empty, which would read as every provider.
func pinnedGrant(granted, pinned []string) []string {
	if len(granted) == 0 {
		return slices.Clone(pinned)
	}
	var out []string
	for _, p := range granted {
		if slices.Contains(pinned, p) {
			out = append(out, p)
		}
	}
	return out
}

// fallbackProviders returns the wire-compatible fallback providers granted to wl
// and configured in g.upstreams for a request originally routed to primary.
func (g *Gate) fallbackProviders(primary string, wl domain.Workload) []string {
	if primary != "openai" && primary != "local" {
		return nil
	}
	var out []string
	for _, cand := range wl.Spec.Grants.ModelGate.Providers {
		if cand == primary || (cand != "openai" && cand != "local") {
			continue
		}
		if _, ok := g.upstreams[cand]; !ok {
			continue
		}
		if !slices.Contains(out, cand) {
			out = append(out, cand)
		}
	}
	return out
}

func (g *Gate) tryFallback(ctx context.Context, r *http.Request, failedProvider, upstreamPath string, reqBody []byte, wl domain.Workload) (string, *http.Response, error) {
	cands := g.fallbackProviders(failedProvider, wl)
	if len(cands) == 0 {
		return "", nil, errors.New("no wire-compatible fallback")
	}
	for _, cand := range cands {
		upCfg := g.upstreams[cand]
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
		resp, err := g.client.Do(req) // #nosec G704 -- joinUpstreamURL pins the configured fallback host and scheme, and gatehttp.NewClient refuses redirects.
		if err == nil && resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return cand, resp, nil
		}
		if resp != nil && resp.Body != nil {
			_ = resp.Body.Close()
		}
	}
	return "", nil, errors.New("fallback unavailable")
}

func (g *Gate) reserveBudget(ctx context.Context, taskID, workloadName string, b domain.Budget, reserve domain.BudgetTotals, now time.Time) (string, bool, string, error) {
	if _, err := g.store.GetTask(ctx, taskID); err != nil {
		return "", false, "", err
	}
	res, err := g.store.ReserveBudget(ctx, store.BudgetReservationRequest{
		TaskID:       taskID,
		WorkloadName: workloadName,
		Budget:       b,
		Reserve:      reserve,
		Now:          now,
	})
	if err != nil {
		return "", false, "", err
	}
	return res.ReservationID, res.Exceeded, res.Reason, nil
}

const (
	// settleReservationAttempts bounds the immediate retries of a failed settlement.
	settleReservationAttempts = 3
	// settleReservationTimeout bounds detached reservation settlement and release
	// so a held lock or exhausted pool cannot block a request goroutine indefinitely.
	settleReservationTimeout = 10 * time.Second
)

// settleReservation settles or, when actual is nil, releases a reservation. A failed
// transaction rolls back and leaves the reservation in place, so retrying is safe.
// A missing or mismatched reservation is final and is returned without retry.
func (g *Gate) settleReservation(ctx context.Context, reservationID string, actual *domain.BudgetEntry) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), settleReservationTimeout)
	defer cancel()
	var err error
	for range settleReservationAttempts {
		err = g.store.SettleBudgetReservation(ctx, reservationID, actual)
		if err == nil || errors.Is(err, store.ErrNotFound) || errors.Is(err, store.ErrConflict) {
			return err
		}
		if ctx.Err() != nil {
			return err
		}
	}
	return err
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

const (
	// maxRequestBodyBytes is how much of a request body the gate reads.
	maxRequestBodyBytes = 16 << 20
	// maxCapturedResponseBytes is how much of a response the gate parses for
	// token usage. One additional sentinel byte is retained so responses past
	// the limit are reported as oversized.
	maxCapturedResponseBytes = 8 << 20
	// upstreamReadChunkBytes is the size of one read from the upstream response.
	upstreamReadChunkBytes = 32 << 10
)

// modelPlaceholder in a route stands for any model name.
const modelPlaceholder = "{model}"

// routes lists, per provider and in matching order, the upstream paths the
// gate forwards. The gate forwards the request method unchanged.
var routes = []struct{ provider, path string }{
	{"openai", "/v1/chat/completions"},
	{"openai", "/v1/responses"},
	{"anthropic", "/v1/messages"},
	{"gemini", "/v1beta/models/" + modelPlaceholder + ":generateContent"},
	{"gemini", "/v1beta/models/" + modelPlaceholder + ":streamGenerateContent"},
	{"local", "/v1/chat/completions"},
	{"local", "/v1/responses"},
}

// Routes returns the paths the gate serves, relative to its mount point, as
// "/<provider><upstream path>". A {model} segment matches any model name.
func Routes() []string {
	out := make([]string, 0, len(routes))
	for _, r := range routes {
		out = append(out, "/"+r.provider+r.path)
	}
	return out
}

// routeProvider finds the route of requestPath in routes, the one list of providers
// and paths the gate dispatches and documents.
func routeProvider(requestPath string) (provider, upstreamPath string, ok bool) {
	for _, r := range routes {
		rest, found := strings.CutPrefix(requestPath, "/"+r.provider)
		if found && strings.HasPrefix(rest, "/") && matchRoute(r.path, rest) {
			return r.provider, rest, true
		}
	}
	return "", "", false
}

func matchRoute(pattern, rest string) bool {
	before, after, wildcard := strings.Cut(pattern, modelPlaceholder)
	if !wildcard {
		return rest == pattern
	}
	return len(rest) >= len(before)+len(after) && strings.HasPrefix(rest, before) && strings.HasSuffix(rest, after)
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
		case "authorization", "x-api-key", "x-goog-api-key", "host", "content-length", "connection", "accept-encoding":
			continue
		}
		for _, v := range vals {
			dst.Add(k, v)
		}
	}
}

func injectUpstreamAuth(h http.Header, provider, key, anthropicVer string) {
	// Strip any prior provider auth headers before injecting the active provider's credential
	// so fallback transitions (e.g. openai -> local) can never leak another provider's key.
	h.Del("Authorization")
	h.Del("x-api-key")
	h.Del("x-goog-api-key")
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

func joinUpstreamURL(baseURL, reqPath, rawQuery string) (string, error) {
	u, err := url.Parse(baseURL)
	if err != nil {
		return "", err
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("unsupported upstream scheme %q", u.Scheme)
	}
	if u.Host == "" {
		return "", errors.New("upstream base URL missing host")
	}
	if strings.ContainsAny(reqPath, "\\\r\n") || strings.Contains(reqPath, "@") || strings.Contains(reqPath, "://") {
		return "", errors.New("invalid upstream path characters")
	}
	unescaped, err := url.PathUnescape(reqPath)
	if err != nil {
		return "", fmt.Errorf("invalid escaped path: %w", err)
	}
	if strings.Contains(unescaped, "..") || strings.ContainsAny(unescaped, "\\\r\n") {
		return "", errors.New("path traversal not allowed in upstream path")
	}
	cleanReq := path.Clean("/" + reqPath)
	if !strings.HasPrefix(cleanReq, "/") || strings.Contains(cleanReq, "..") {
		return "", errors.New("invalid cleaned upstream path")
	}
	baseHost := u.Host
	baseScheme := u.Scheme
	u.Path = strings.TrimRight(u.Path, "/") + cleanReq
	u.RawPath = ""
	u.RawQuery = rawQuery
	u.Fragment = ""

	finalURL := u.String()
	parsed, err := url.Parse(finalURL)
	if err != nil || parsed.Host != baseHost || parsed.Scheme != baseScheme {
		return "", errors.New("upstream URL host or scheme mismatch")
	}
	return finalURL, nil
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
