package mcpgate

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/shadewright-labs/sablecourt/internal/approval"
	"github.com/shadewright-labs/sablecourt/internal/config"
	"github.com/shadewright-labs/sablecourt/internal/domain"
	"github.com/shadewright-labs/sablecourt/internal/gate/model"
	"github.com/shadewright-labs/sablecourt/internal/policy"
	"github.com/shadewright-labs/sablecourt/internal/redact"
	"github.com/shadewright-labs/sablecourt/internal/store"
)

type ToolFunc func(ctx context.Context, workload domain.Workload, taskID string, args json.RawMessage) (string, error)

type ToolDef struct {
	Name        string
	Description string
	InputSchema json.RawMessage
	Handler     ToolFunc
}

type ActionFunc func(ctx context.Context, workload domain.Workload, taskID string, payload json.RawMessage) (any, error)

type Options struct {
	Store     store.Store
	Bundle    *config.Bundle
	Policy    *policy.Engine
	Breaker   *policy.CircuitBreaker
	Approvals *approval.Queue
	Now       func() time.Time
}

type Gate struct {
	store     store.Store
	bundle    *config.Bundle
	policy    *policy.Engine
	breaker   *policy.CircuitBreaker
	approvals *approval.Queue
	now       func() time.Time

	mu      sync.RWMutex
	tools   map[string]ToolDef
	actions map[string]ActionFunc
}

func New(opts Options) *Gate {
	nowFn := opts.Now
	if nowFn == nil {
		nowFn = func() time.Time { return time.Now().UTC() }
	}
	return &Gate{
		store:     opts.Store,
		bundle:    opts.Bundle,
		policy:    opts.Policy,
		breaker:   opts.Breaker,
		approvals: opts.Approvals,
		now:       nowFn,
		tools:     make(map[string]ToolDef),
		actions:   make(map[string]ActionFunc),
	}
}

func (g *Gate) Reload(bundle *config.Bundle, pol *policy.Engine) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.bundle = bundle
	g.policy = pol
}

type gateSnapshot struct {
	bundle *config.Bundle
	policy *policy.Engine
}

type snapshotCtxKey struct{}

func (g *Gate) snapshot() gateSnapshot {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return gateSnapshot{
		bundle: g.bundle,
		policy: g.policy,
	}
}

func (g *Gate) snapshotFromContext(ctx context.Context) gateSnapshot {
	if ctx != nil {
		if snap, ok := ctx.Value(snapshotCtxKey{}).(gateSnapshot); ok {
			return snap
		}
	}
	return g.snapshot()
}

func (g *Gate) RegisterTool(def ToolDef) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(def.InputSchema) == 0 {
		def.InputSchema = json.RawMessage(`{"type":"object"}`)
	}
	g.tools[def.Name] = def
}

func (g *Gate) RegisterAction(name string, fn ActionFunc) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.actions[name] = fn
}

type authCtxKey struct{}

type authContext struct {
	Lease    domain.Lease
	Workload domain.Workload
}

// NewServerForWorkload constructs an MCP Server exposing only tools granted to the workload.
func (g *Gate) NewServerForWorkload(lease domain.Lease, workload domain.Workload) *mcp.Server {
	srv := mcp.NewServer(&mcp.Implementation{
		Name:    "sablecourt-data-gate",
		Version: "0.1.0",
	}, nil)

	g.mu.RLock()
	defer g.mu.RUnlock()

	for _, toolName := range workload.Spec.Grants.MCPTools {
		def, ok := g.tools[toolName]
		if !ok {
			continue
		}
		captured := def
		srv.AddTool(&mcp.Tool{
			Name:        captured.Name,
			Description: captured.Description,
			InputSchema: captured.InputSchema,
		}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return g.invokeTool(ctx, lease, workload, captured, req.Params.Arguments)
		})
	}
	return srv
}

func (g *Gate) invokeTool(ctx context.Context, lease domain.Lease, workload domain.Workload, def ToolDef, args json.RawMessage) (*mcp.CallToolResult, error) {
	capability := "mcp:" + def.Name
	snap := g.snapshotFromContext(ctx)
	if snap.policy == nil {
		g.audit(ctx, lease.TaskID, workload.Metadata.Name, capability, "denied", `{"reason":"policy_engine_unavailable"}`)
		return errorToolResult("policy failed closed"), nil
	}
	decision := snap.policy.Evaluate(workload, capability)

	switch decision.Effect {
	case domain.EffectDeny:
		g.audit(ctx, lease.TaskID, workload.Metadata.Name, capability, "denied", fmt.Sprintf(`{"reason":%q}`, decision.Reason))
		return errorToolResult("policy denied: " + decision.Reason), nil

	case domain.EffectApprove:
		if g.approvals == nil {
			return errorToolResult("approval required but approval queue unavailable"), nil
		}
		var payload any
		if len(args) > 0 {
			_ = json.Unmarshal(args, &payload)
		}
		apr, err := g.approvals.Request(ctx, lease.TaskID, workload.Metadata.Name, capability, payload, 15*time.Minute)
		if err != nil {
			return errorToolResult("failed to enqueue approval: " + err.Error()), nil
		}
		_ = g.store.UpdateTaskState(ctx, lease.TaskID, domain.TaskStateWorking, domain.TaskStateInputRequired, "approval_required", fmt.Sprintf(`{"approval_id":%q}`, apr.ID))
		msg, _ := json.Marshal(map[string]string{
			"status":         "approval_required",
			"approval_id":    apr.ID,
			"payload_sha256": apr.PayloadSHA256,
		})
		return textToolResult(string(msg)), nil

	case domain.EffectAllow:
		out, err := def.Handler(ctx, workload, lease.TaskID, args)
		if err != nil {
			outcome := "error"
			if errors.Is(err, errToolRefused) {
				outcome = "denied"
			}
			g.audit(ctx, lease.TaskID, workload.Metadata.Name, capability, outcome, fmt.Sprintf(`{"error":%q}`, redact.String(err.Error())))
			return errorToolResult(redact.String(err.Error())), nil
		}
		red := redact.Text(out)
		detail := fmt.Sprintf(`{"redaction_hits":%d}`, red.Hits)
		g.audit(ctx, lease.TaskID, workload.Metadata.Name, capability, "allowed", detail)
		return textToolResult(red.Text), nil

	default:
		return errorToolResult("policy failed closed"), nil
	}
}

// MCPHandler returns an authenticated Streamable HTTP handler for MCP tool calls.
func (g *Gate) MCPHandler() http.Handler {
	streamHandler := mcp.NewStreamableHTTPHandler(func(r *http.Request) *mcp.Server {
		ac, ok := r.Context().Value(authCtxKey{}).(authContext)
		if !ok {
			return nil
		}
		return g.NewServerForWorkload(ac.Lease, ac.Workload)
	}, &mcp.StreamableHTTPOptions{Stateless: true})

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		snap := g.snapshot()
		ac, ok := g.authenticateWithSnapshot(w, r, snap)
		if !ok {
			return
		}
		ctx := context.WithValue(r.Context(), snapshotCtxKey{}, snap)
		ctx = context.WithValue(ctx, authCtxKey{}, ac)
		streamHandler.ServeHTTP(w, r.WithContext(ctx))
	})
}

type actionRequest struct {
	ApprovalID string          `json:"approvalId,omitempty"`
	Payload    json.RawMessage `json:"payload"`
}

// ActionHandler serves the closed catalog of typed mutating actions at POST /{actionName}.
func (g *Gate) ActionHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "POST required"})
			return
		}
		snap := g.snapshot()
		ac, ok := g.authenticateWithSnapshot(w, r, snap)
		if !ok {
			return
		}
		ctx := context.WithValue(r.Context(), snapshotCtxKey{}, snap)
		actionName := strings.TrimPrefix(r.URL.Path, "/")
		if actionName == "" {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "action name required"})
			return
		}

		g.mu.RLock()
		fn, exists := g.actions[actionName]
		g.mu.RUnlock()
		if !exists {
			g.audit(ctx, ac.Lease.TaskID, ac.Workload.Metadata.Name, "action:"+actionName, "denied", `{"reason":"unknown_action"}`)
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "action not in closed catalog"})
			return
		}

		rawBody, err := io.ReadAll(io.LimitReader(r.Body, 2<<20))
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "read body failed"})
			return
		}
		dec := json.NewDecoder(bytes.NewReader(rawBody))
		dec.DisallowUnknownFields()
		var req actionRequest
		if err := dec.Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid action request: " + err.Error()})
			return
		}

		var canonicalPayload any
		if len(req.Payload) > 0 {
			if err := json.Unmarshal(req.Payload, &canonicalPayload); err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid payload JSON"})
				return
			}
		}

		capability := "action:" + actionName
		if snap.policy == nil {
			g.audit(ctx, ac.Lease.TaskID, ac.Workload.Metadata.Name, capability, "denied", `{"reason":"policy_engine_unavailable"}`)
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "policy failed closed"})
			return
		}
		decision := snap.policy.Evaluate(ac.Workload, capability)
		approvalUsed := ""
		switch decision.Effect {
		case domain.EffectDeny:
			g.audit(ctx, ac.Lease.TaskID, ac.Workload.Metadata.Name, capability, "denied", fmt.Sprintf(`{"reason":%q}`, decision.Reason))
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "policy denied: " + decision.Reason})
			return

		case domain.EffectApprove:
			if req.ApprovalID == "" {
				if g.approvals == nil {
					writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "approval queue unavailable"})
					return
				}
				apr, err := g.approvals.Request(ctx, ac.Lease.TaskID, ac.Workload.Metadata.Name, capability, canonicalPayload, 15*time.Minute)
				if err != nil {
					writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
					return
				}
				_ = g.store.UpdateTaskState(ctx, ac.Lease.TaskID, domain.TaskStateWorking, domain.TaskStateInputRequired, "approval_required", fmt.Sprintf(`{"approval_id":%q}`, apr.ID))
				writeJSON(w, http.StatusAccepted, map[string]string{
					"status":         "approval_required",
					"approval_id":    apr.ID,
					"payload_sha256": apr.PayloadSHA256,
				})
				return
			}

			// Consume the approval: one store transaction checks it is approved, unexpired and
			// pinned to this task, capability and canonical payload digest, and moves it to
			// consumed. The approval is spent before the side effect runs, so a failed or
			// interrupted action needs a fresh approval instead of reusing this one.
			_, digest, err := approval.CanonicalizeAndHash(canonicalPayload)
			if err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": "canonicalize payload failed"})
				return
			}
			if err := g.store.ConsumeApproval(ctx, req.ApprovalID, ac.Lease.TaskID, capability, digest, g.now()); err != nil {
				reason := "approval_mismatch_or_unapproved"
				switch {
				case errors.Is(err, store.ErrExpired):
					reason = "approval_expired"
				case errors.Is(err, store.ErrNotFound):
					reason = "approval_not_found"
				case !errors.Is(err, store.ErrConflict):
					reason = "approval_check_failed"
				}
				g.audit(ctx, ac.Lease.TaskID, ac.Workload.Metadata.Name, capability, "denied", fmt.Sprintf(`{"reason":%q,"approval_id":%q}`, reason, req.ApprovalID))
				writeJSON(w, http.StatusForbidden, map[string]string{"error": "approval does not authorize this payload: " + reason})
				return
			}
			approvalUsed = req.ApprovalID
			// The consumption record is what tells a crash between this point and the side
			// effect apart from a refused call, so the action does not run without it.
			if err := g.store.AppendAudit(ctx, domain.AuditRecord{
				Category:     "approval",
				TaskID:       ac.Lease.TaskID,
				WorkloadName: ac.Workload.Metadata.Name,
				Action:       capability,
				Outcome:      "consumed",
				DetailJSON:   fmt.Sprintf(`{"approval_id":%q,"payload_sha256":%q}`, req.ApprovalID, digest),
				CreatedAt:    g.now(),
			}); err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "record approval use failed"})
				return
			}

		case domain.EffectAllow:
			// Permitted directly.
		default:
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "policy failed closed"})
			return
		}

		result, err := fn(ctx, ac.Workload, ac.Lease.TaskID, req.Payload)
		if errors.Is(err, errToolRefused) {
			// A refused target is a policy outcome, not a failing dependency: the
			// breaker does not count it.
			g.audit(ctx, ac.Lease.TaskID, ac.Workload.Metadata.Name, capability, "denied", fmt.Sprintf(`{"error":%q,"approval_id":%q}`, redact.String(err.Error()), approvalUsed))
			writeJSON(w, http.StatusForbidden, map[string]string{"error": redact.String(err.Error())})
			return
		}
		if err != nil {
			if g.breaker != nil {
				g.breaker.RecordFailure(capability)
			}
			g.audit(ctx, ac.Lease.TaskID, ac.Workload.Metadata.Name, capability, "error", fmt.Sprintf(`{"error":%q,"approval_id":%q}`, redact.String(err.Error()), approvalUsed))
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": redact.String(err.Error())})
			return
		}
		if g.breaker != nil {
			g.breaker.RecordSuccess(capability)
		}
		resBytes, _ := json.Marshal(result)
		red := redact.Text(string(resBytes))
		g.audit(ctx, ac.Lease.TaskID, ac.Workload.Metadata.Name, capability, "allowed", fmt.Sprintf(`{"redaction_hits":%d,"approval_id":%q}`, red.Hits, approvalUsed))

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(red.Text))
	})
}

func (g *Gate) authenticate(w http.ResponseWriter, r *http.Request) (authContext, bool) {
	return g.authenticateWithSnapshot(w, r, g.snapshot())
}

func (g *Gate) authenticateWithSnapshot(w http.ResponseWriter, r *http.Request, snap gateSnapshot) (authContext, bool) {
	auth := r.Header.Get("Authorization")
	if !strings.HasPrefix(auth, "Bearer ") {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "bearer task token required"})
		return authContext{}, false
	}
	token := strings.TrimSpace(strings.TrimPrefix(auth, "Bearer "))
	if !strings.HasPrefix(token, model.TokenPrefix) {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid task token format"})
		return authContext{}, false
	}
	lease, err := g.store.GetActiveLeaseByTokenHash(r.Context(), model.HashToken(token), g.now())
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid, expired, or revoked task token"})
			return authContext{}, false
		}
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "lease lookup failed"})
		return authContext{}, false
	}
	if snap.bundle == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "no workload bundle loaded"})
		return authContext{}, false
	}
	wl, ok := snap.bundle.Workloads[lease.WorkloadName]
	if !ok {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "workload not declared"})
		return authContext{}, false
	}
	return authContext{Lease: lease, Workload: wl}, true
}

func (g *Gate) audit(ctx context.Context, taskID, workload, action, outcome, detailJSON string) {
	_ = g.store.AppendAudit(ctx, domain.AuditRecord{
		Category:     "data_action_gate",
		TaskID:       taskID,
		WorkloadName: workload,
		Action:       action,
		Outcome:      outcome,
		DetailJSON:   detailJSON,
		CreatedAt:    g.now(),
	})
}

func textToolResult(text string) *mcp.CallToolResult {
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: text}},
	}
}

func errorToolResult(msg string) *mcp.CallToolResult {
	return &mcp.CallToolResult{
		IsError: true,
		Content: []mcp.Content{&mcp.TextContent{Text: msg}},
	}
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
