package mcpgate

import (
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

// ActionFunc runs an action registered without a typed input. It receives the
// canonical encoding of the decoded payload, never the caller's bytes.
type ActionFunc func(ctx context.Context, workload domain.Workload, taskID string, payload json.RawMessage) (any, error)

// actionDef is one entry of the closed action catalog. decode turns the
// caller's payload into the one value that is approved, hashed, shown and
// executed; run executes that value.
type actionDef struct {
	decode func(raw json.RawMessage) (any, error)
	run    func(ctx context.Context, workload domain.Workload, taskID string, value any) (any, error)
}

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

	mu          sync.RWMutex
	tools       map[string]ToolDef
	actions     map[string]actionDef
	allowedRepo string
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
		actions:   make(map[string]actionDef),
	}
}

func (g *Gate) Reload(bundle *config.Bundle, pol *policy.Engine) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.bundle = bundle
	g.policy = pol
}

type gateSnapshot struct {
	bundle      *config.Bundle
	policy      *policy.Engine
	allowedRepo string
}

type snapshotCtxKey struct{}

func (g *Gate) snapshot() gateSnapshot {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return gateSnapshot{
		bundle:      g.bundle,
		policy:      g.policy,
		allowedRepo: g.allowedRepo,
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

// RegisterAction adds an action without a typed input. Its payload must be a
// JSON object with no duplicate or case-colliding keys at any depth, and fn
// receives the canonical encoding of that object.
func (g *Gate) RegisterAction(name string, fn ActionFunc) {
	g.registerAction(name, actionDef{
		decode: func(raw json.RawMessage) (any, error) { return decodeGeneric(raw) },
		run: func(ctx context.Context, wl domain.Workload, taskID string, value any) (any, error) {
			canonical, err := json.Marshal(value)
			if err != nil {
				return nil, err
			}
			return fn(ctx, wl, taskID, canonical)
		},
	})
}

// RegisterTypedAction adds an action whose payload decodes strictly into T
// (see decodeTyped) and passes validate. The decoded T is the value the
// approval digest covers, the dialog shows and run receives.
func RegisterTypedAction[T any](g *Gate, name string, validate func(T) error, run func(ctx context.Context, workload domain.Workload, taskID string, in T) (any, error)) {
	g.registerAction(name, actionDef{
		decode: func(raw json.RawMessage) (any, error) {
			in, err := decodeTyped[T](raw)
			if err != nil {
				return nil, err
			}
			if validate != nil {
				if err := validate(in); err != nil {
					return nil, fmt.Errorf("%w: %v", errInvalidPayload, err)
				}
			}
			return in, nil
		},
		run: func(ctx context.Context, wl domain.Workload, taskID string, value any) (any, error) {
			in, ok := value.(T)
			if !ok {
				return nil, errors.New("action input has the wrong type")
			}
			return run(ctx, wl, taskID, in)
		},
	})
}

func (g *Gate) registerAction(name string, def actionDef) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.actions[name] = def
}

// SetAllowedRepo records the repository the GitOps actions are bound to, as
// "owner/repo", or "" when none is resolved. Requests started after the call
// use the new value, and each action's audit row names it.
func (g *Gate) SetAllowedRepo(repo string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.allowedRepo = repo
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

// maxActionBody is the largest action request body the gate reads.
const maxActionBody = 2 << 20

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
		def, exists := g.actions[actionName]
		g.mu.RUnlock()
		if !exists {
			g.audit(ctx, ac.Lease.TaskID, ac.Workload.Metadata.Name, "action:"+actionName, "denied", `{"reason":"unknown_action"}`)
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "action not in closed catalog"})
			return
		}

		// One byte past the limit is read so that a longer body is refused
		// instead of cut at the limit: a cut could drop trailing data the
		// strict decoder would otherwise refuse.
		rawBody, err := io.ReadAll(io.LimitReader(r.Body, maxActionBody+1))
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "read body failed"})
			return
		}
		if len(rawBody) > maxActionBody {
			writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "action request larger than 2 MiB"})
			return
		}
		req, err := decodeTyped[actionRequest](rawBody)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid action request: " + err.Error()})
			return
		}

		capability := "action:" + actionName
		wlName := ac.Workload.Metadata.Name
		auditAction := func(outcome string, detail map[string]any) {
			if snap.allowedRepo != "" {
				detail["allowed_repo"] = snap.allowedRepo
			}
			raw, _ := json.Marshal(detail)
			g.audit(ctx, ac.Lease.TaskID, wlName, capability, outcome, string(raw))
		}
		if snap.policy == nil {
			auditAction("denied", map[string]any{"reason": "policy_engine_unavailable"})
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "policy failed closed"})
			return
		}
		decision := snap.policy.Evaluate(ac.Workload, capability)
		if decision.Effect == domain.EffectDeny {
			auditAction("denied", map[string]any{"reason": decision.Reason})
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "policy denied: " + decision.Reason})
			return
		}

		// One strict decode into the action's typed value. From here on the
		// caller's bytes are not used: the digest, the dialog (through the
		// approval's canonical JSON) and the executor all work from value.
		value, err := def.decode(req.Payload)
		if err != nil {
			auditAction("denied", map[string]any{"reason": "invalid_payload", "error": redact.String(err.Error())})
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": redact.String(err.Error())})
			return
		}
		_, digest, err := approval.CanonicalizeAndHash(value)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "canonicalize payload failed"})
			return
		}

		approvalUsed := ""
		switch decision.Effect {
		case domain.EffectApprove:
			if req.ApprovalID == "" {
				if g.approvals == nil {
					writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "approval queue unavailable"})
					return
				}
				apr, err := g.approvals.Request(ctx, ac.Lease.TaskID, wlName, capability, value, 15*time.Minute)
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

			// Consume the approval: the store moves it from approved to consumed
			// only if it is pinned to this task, capability and digest and has
			// not expired. The approval is spent before the side effect runs, so
			// a failed or interrupted action needs a fresh approval.
			if err := g.store.ConsumeApproval(ctx, req.ApprovalID, ac.Lease.TaskID, capability, digest); err != nil {
				reason := "approval_check_failed"
				switch {
				case errors.Is(err, store.ErrExpired):
					reason = "approval_expired"
				case errors.Is(err, store.ErrNotFound):
					reason = "approval_not_found"
				case errors.Is(err, store.ErrConflict):
					reason = "approval_mismatch_or_unapproved"
				}
				auditAction("denied", map[string]any{"reason": reason, "approval_id": req.ApprovalID})
				writeJSON(w, http.StatusForbidden, map[string]string{"error": "approval does not authorize this payload: " + reason})
				return
			}
			approvalUsed = req.ApprovalID
			// Consumption and its audit row are two steps. The order fails
			// closed: the approval is spent first, and the action runs only once
			// the record of that use is written. A failed write stops here and
			// the approval stays spent.
			consumed := map[string]string{"approval_id": req.ApprovalID, "payload_sha256": digest}
			if snap.allowedRepo != "" {
				consumed["allowed_repo"] = snap.allowedRepo
			}
			consumedJSON, _ := json.Marshal(consumed)
			if err := g.store.AppendAudit(ctx, domain.AuditRecord{
				Category:     "approval",
				TaskID:       ac.Lease.TaskID,
				WorkloadName: wlName,
				Action:       capability,
				Outcome:      "consumed",
				DetailJSON:   string(consumedJSON),
				CreatedAt:    g.now(),
			}); err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "recording the approval use failed; the approval is spent and the action did not run, request a new approval"})
				return
			}

		case domain.EffectAllow:
			// Permitted directly.
		default:
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "policy failed closed"})
			return
		}

		result, err := def.run(ctx, ac.Workload, ac.Lease.TaskID, value)
		if errors.Is(err, errToolRefused) {
			// A refused target is a policy outcome, not a failing dependency: the
			// breaker does not count it.
			auditAction("denied", map[string]any{"error": redact.String(err.Error()), "approval_id": approvalUsed})
			writeJSON(w, http.StatusForbidden, map[string]string{"error": redact.String(err.Error())})
			return
		}
		if err != nil {
			if g.breaker != nil {
				g.breaker.RecordFailure(capability)
			}
			auditAction("error", map[string]any{"error": redact.String(err.Error()), "approval_id": approvalUsed})
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": redact.String(err.Error())})
			return
		}
		if g.breaker != nil {
			g.breaker.RecordSuccess(capability)
		}
		resBytes, _ := json.Marshal(result)
		red := redact.Text(string(resBytes))
		auditAction("allowed", map[string]any{"redaction_hits": red.Hits, "approval_id": approvalUsed})

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
