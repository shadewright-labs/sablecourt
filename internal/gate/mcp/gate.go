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
//
// requiresApproval makes the catalog ask for an out-of-band approval even when
// policy allows the capability. precheck, when set, checks live state for the
// decoded value before an approval is requested (and before run on an allowed
// request); it does not run on a request that presents an approval. run checks
// the live state again after the approval is spent, because state can change
// while the approval waits.
type actionDef struct {
	decode           func(raw json.RawMessage) (any, error)
	precheck         func(ctx context.Context, workload domain.Workload, taskID string, value any) error
	run              func(ctx context.Context, workload domain.Workload, taskID string, value any) (any, error)
	requiresApproval bool
}

// actionError is a refusal with its own HTTP status and audit reason. It
// wraps errToolRefused, so every caller that treats errToolRefused as a
// refusal treats it the same way.
type actionError struct {
	status int
	reason string
	msg    string
}

func (e *actionError) Error() string { return e.msg }
func (e *actionError) Unwrap() error { return errToolRefused }

func newActionError(status int, reason, msg string) error {
	return &actionError{status: status, reason: reason, msg: msg}
}

// refusalOf returns the HTTP status and audit reason of a refusal: those of an
// actionError, or 403 and "refused" for any other errToolRefused.
func refusalOf(err error) (int, string) {
	var ae *actionError
	if errors.As(err, &ae) && ae.status != 0 {
		reason := ae.reason
		if reason == "" {
			reason = "refused"
		}
		return ae.status, reason
	}
	return http.StatusForbidden, "refused"
}

type approvalCtxKey struct{}

// approvalFromContext returns the ID of the approval the gate consumed for the
// running action, or "" when the action runs without one.
func approvalFromContext(ctx context.Context) string {
	id, _ := ctx.Value(approvalCtxKey{}).(string)
	return id
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

	mu sync.RWMutex
	// tools holds the gate's own tools. upstream holds the tools proxied to
	// upstream MCP servers, rebuilt from the bundle on every reload. The two
	// never share a name: an upstream tool that takes a built-in name or
	// namespace is refused, and a lookup tries tools first. upstream is
	// replaced, never changed in place, because snapshots share it.
	tools       map[string]ToolDef
	upstream    map[string]ToolDef
	bridge      *UpstreamBridge
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
		upstream:  make(map[string]ToolDef),
		actions:   make(map[string]actionDef),
	}
}

// Reload swaps the bundle and policy. When an upstream bridge is set, it also
// rebuilds the upstream tools from the new bundle only, so the tools of a
// server the bundle no longer declares are gone. If any upstream tool of the
// new bundle takes a built-in name, the whole reload is refused: the previous
// bundle, policy and upstream tools stay as they were.
func (g *Gate) Reload(bundle *config.Bundle, pol *policy.Engine) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.bridge != nil {
		next, err := g.buildUpstreamLocked(bundle, g.bridge)
		if err != nil {
			return fmt.Errorf("reload refused: %w", err)
		}
		g.upstream = next
	}
	g.bundle = bundle
	g.policy = pol
	return nil
}

// gateSnapshot is the state one request uses from start to end. upstream is
// never written after it is stored on the gate (every change builds a new
// map), so a snapshot may read it without the lock.
type gateSnapshot struct {
	bundle      *config.Bundle
	policy      *policy.Engine
	upstream    map[string]ToolDef
	allowedRepo string
}

type snapshotCtxKey struct{}

func (g *Gate) snapshot() gateSnapshot {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return gateSnapshot{
		bundle:      g.bundle,
		policy:      g.policy,
		upstream:    g.upstream,
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
	registerTypedActionDef(g, name, typedAction[T]{validate: validate, run: run})
}

// typedAction holds the steps of one typed action. precheck and
// requiresApproval are described on actionDef.
type typedAction[T any] struct {
	validate         func(T) error
	precheck         func(ctx context.Context, workload domain.Workload, taskID string, in T) error
	run              func(ctx context.Context, workload domain.Workload, taskID string, in T) (any, error)
	requiresApproval bool
}

func registerTypedActionDef[T any](g *Gate, name string, a typedAction[T]) {
	validate, run := a.validate, a.run
	def := actionDef{
		requiresApproval: a.requiresApproval,
		decode: func(raw json.RawMessage) (any, error) {
			in, err := decodeTyped[T](raw)
			if err != nil {
				return nil, err
			}
			if validate != nil {
				if err := validate(in); err != nil {
					if errors.Is(err, errToolRefused) {
						return nil, err // a well-formed request the gate refuses
					}
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
	}
	if a.precheck != nil {
		precheck := a.precheck
		def.precheck = func(ctx context.Context, wl domain.Workload, taskID string, value any) error {
			in, ok := value.(T)
			if !ok {
				return errors.New("action input has the wrong type")
			}
			return precheck(ctx, wl, taskID, in)
		}
	}
	g.registerAction(name, def)
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

type authContext struct {
	Lease    domain.Lease
	Workload domain.Workload
}

// NewServerForWorkload constructs an MCP Server exposing only tools granted to
// the workload, using the gate's current snapshot for every call it serves.
func (g *Gate) NewServerForWorkload(lease domain.Lease, workload domain.Workload) *mcp.Server {
	return g.newServerForSnapshot(lease, workload, g.snapshot())
}

// newServerForSnapshot builds the per-request MCP server from snap: upstream
// tools come from snap.upstream and each call evaluates snap.policy, so a
// reload after snap was taken changes neither.
func (g *Gate) newServerForSnapshot(lease domain.Lease, workload domain.Workload, snap gateSnapshot) *mcp.Server {
	srv := mcp.NewServer(&mcp.Implementation{
		Name:    "sablecourt-data-gate",
		Version: "0.1.0",
	}, nil)

	g.mu.RLock()
	defer g.mu.RUnlock()

	for _, toolName := range workload.Spec.Grants.MCPTools {
		def, ok := g.tools[toolName]
		if !ok {
			def, ok = snap.upstream[toolName]
		}
		if !ok {
			continue
		}
		captured := def
		srv.AddTool(&mcp.Tool{
			Name:        captured.Name,
			Description: captured.Description,
			InputSchema: captured.InputSchema,
		}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			ctx = context.WithValue(ctx, snapshotCtxKey{}, snap)
			return g.invokeTool(ctx, lease, workload, captured, req.Params.Arguments)
		})
	}
	return srv
}

func (g *Gate) invokeTool(ctx context.Context, lease domain.Lease, workload domain.Workload, def ToolDef, args json.RawMessage) (*mcp.CallToolResult, error) {
	// Each call gets its own effects recorder, even when calls share a
	// request, so a failed audit write names only what this call changed.
	ctx, _ = store.TrackNewEffects(ctx)
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
			if msg, ok := store.AuditFailureText(ctx, err, "the approval request"); ok {
				return errorToolResult(msg), nil
			}
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
		// A tool declares no effect, so every call is treated as one that may
		// change state: it is recorded before it runs, and a record that
		// cannot be written refuses it.
		if err := g.record(ctx, lease.TaskID, workload.Metadata.Name, capability, "started", `{}`); err != nil {
			msg, _ := store.AuditFailureText(ctx, err, "the tool call")
			return errorToolResult(msg), nil
		}
		out, err := def.Handler(ctx, workload, lease.TaskID, args)
		store.NoteEffectIf(ctx, err, "tool %s ran", def.Name)
		if err != nil {
			outcome := "error"
			if errors.Is(err, errToolRefused) {
				outcome = "denied"
			}
			g.audit(ctx, lease.TaskID, workload.Metadata.Name, capability, outcome, fmt.Sprintf(`{"error":%q}`, redact.String(err.Error())))
			// A failed audit write inside the tool says what already
			// happened, as it does for an action.
			if msg, ok := store.AuditFailureText(ctx, err, "the tool call"); ok {
				return errorToolResult(msg), nil
			}
			return errorToolResult(redact.String(err.Error())), nil
		}
		red := redact.Text(out)
		detail := fmt.Sprintf(`{"redaction_hits":%d}`, red.Hits)
		if err := g.record(ctx, lease.TaskID, workload.Metadata.Name, capability, "allowed", detail); err != nil {
			msg, _ := store.AuditFailureText(ctx, err, "the tool call")
			return errorToolResult(msg), nil
		}
		return textToolResult(red.Text), nil

	default:
		return errorToolResult("policy failed closed"), nil
	}
}

// MCPHandler returns an authenticated Streamable HTTP handler for MCP tool calls.
func (g *Gate) MCPHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		snap := g.snapshot()
		ac, ok := g.authenticateWithSnapshot(w, r, snap)
		if !ok {
			return
		}
		// The server is built here from the snapshot authentication used,
		// not read back from the request, so every call it serves uses
		// that snapshot. A stateless handler keeps no state between
		// requests, so one per request costs only its allocation.
		srv := g.newServerForSnapshot(ac.Lease, ac.Workload, snap)
		mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server {
			return srv
		}, &mcp.StreamableHTTPOptions{Stateless: true}).ServeHTTP(w, r)
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
		ctx, _ = store.TrackEffects(ctx)
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
		// Some actions always need a person: policy can deny them but never
		// let them run without an out-of-band approval.
		if decision.Effect == domain.EffectAllow && def.requiresApproval {
			decision.Effect = domain.EffectApprove
			decision.Reason = "catalog_requires_approval"
		}

		// One strict decode into the action's typed value. From here on the
		// caller's bytes are not used: the digest, the dialog (through the
		// approval's canonical JSON) and the executor all work from value.
		value, err := def.decode(req.Payload)
		if err != nil {
			// The executor checked the payload before the typed catalog, so an
			// allowed request it refused counted on the breaker. It still does.
			if decision.Effect == domain.EffectAllow && g.breaker != nil {
				g.breaker.RecordFailure(capability)
			}
			if errors.Is(err, errToolRefused) {
				status, reason := refusalOf(err)
				auditAction("denied", map[string]any{"reason": reason, "error": redact.String(err.Error())})
				writeJSON(w, status, map[string]string{"error": redact.String(err.Error())})
				return
			}
			auditAction("denied", map[string]any{"reason": "invalid_payload", "error": redact.String(err.Error())})
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": redact.String(err.Error())})
			return
		}
		_, digest, err := approval.CanonicalizeAndHash(value)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "canonicalize payload failed"})
			return
		}
		// Live state is checked before an approval is requested, so the
		// operator is never asked to approve an action that would be refused
		// now (for git.merge_pr: before its checks have passed). A request that
		// presents an approval skips this step: the approval is spent first,
		// and the action's run step checks the live state again right before
		// its side effect, so drift after the approval still spends it, and an
		// approval that does not authorize the request reads nothing.
		precheckNow := (decision.Effect == domain.EffectApprove && req.ApprovalID == "") || decision.Effect == domain.EffectAllow
		if def.precheck != nil && precheckNow {
			if err := def.precheck(ctx, ac.Workload, ac.Lease.TaskID, value); err != nil {
				// On an allowed request a target refused at the check counts on
				// the breaker, like one refused when the action runs.
				if decision.Effect == domain.EffectAllow && g.breaker != nil {
					g.breaker.RecordFailure(capability)
				}
				if errors.Is(err, errToolRefused) {
					status, reason := refusalOf(err)
					auditAction("denied", map[string]any{"reason": reason, "error": redact.String(err.Error())})
					writeJSON(w, status, map[string]string{"error": redact.String(err.Error())})
					return
				}
				auditAction("error", map[string]any{"error": redact.String(err.Error())})
				// A failed audit write in the check (git.merge_pr writes its
				// evidence row there) is a local failure, answered with HTTP
				// 500 like every other failed audit write.
				if msg, ok := store.AuditFailureText(ctx, err, "the action"); ok {
					writeJSON(w, http.StatusInternalServerError, map[string]string{"error": msg})
					return
				}
				writeJSON(w, http.StatusBadGateway, map[string]string{"error": redact.String(err.Error())})
				return
			}
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
				if msg, ok := store.AuditFailureText(ctx, err, "the approval request"); ok {
					writeJSON(w, http.StatusInternalServerError, map[string]string{"error": msg})
					return
				}
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
			// the record of that use is written and the request is still live. A
			// failed write or a cancelled request stops here, and the approval
			// stays spent.
			consumed := map[string]string{"approval_id": req.ApprovalID, "payload_sha256": digest}
			if snap.allowedRepo != "" {
				consumed["allowed_repo"] = snap.allowedRepo
			}
			consumedJSON, _ := json.Marshal(consumed)
			if err := store.RecordAudit(ctx, g.store, domain.AuditRecord{
				Category:     "approval",
				TaskID:       ac.Lease.TaskID,
				WorkloadName: wlName,
				Action:       capability,
				Outcome:      "consumed",
				DetailJSON:   string(consumedJSON),
				CreatedAt:    g.now(),
			}); err != nil {
				msg, _ := store.AuditFailureText(ctx, err, "the action")
				writeJSON(w, http.StatusInternalServerError, map[string]string{"error": msg})
				return
			} else if ctx.Err() != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "request cancelled before the action ran; approval " + req.ApprovalID + " is spent, request a new one"})
				return
			}
			ctx = context.WithValue(ctx, approvalCtxKey{}, req.ApprovalID)

		case domain.EffectAllow:
			// Permitted directly. The run is recorded before it starts, as an
			// approved run is by its consumed row: a record that cannot be
			// written refuses the action before its side effect.
			started := map[string]any{}
			if snap.allowedRepo != "" {
				started["allowed_repo"] = snap.allowedRepo
			}
			startedJSON, _ := json.Marshal(started)
			if err := g.record(ctx, ac.Lease.TaskID, wlName, capability, "started", string(startedJSON)); err != nil {
				msg, _ := store.AuditFailureText(ctx, err, "the action")
				writeJSON(w, http.StatusInternalServerError, map[string]string{"error": msg})
				return
			} else if ctx.Err() != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "request cancelled"})
				return
			}
		default:
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "policy failed closed"})
			return
		}

		result, err := def.run(ctx, ac.Workload, ac.Lease.TaskID, value)
		store.NoteEffectIf(ctx, err, "action %s ran", actionName)
		if err != nil {
			// Every error of the executor counts on the breaker, as before the
			// typed catalog, a refused target included.
			if g.breaker != nil {
				g.breaker.RecordFailure(capability)
			}
			if errors.Is(err, errToolRefused) {
				status, reason := refusalOf(err)
				auditAction("denied", map[string]any{"reason": reason, "error": redact.String(err.Error()), "approval_id": approvalUsed})
				writeJSON(w, status, map[string]string{"error": redact.String(err.Error())})
				return
			}
			auditAction("error", map[string]any{"error": redact.String(err.Error()), "approval_id": approvalUsed})
			// A failed audit write inside the executor is a local failure,
			// answered with HTTP 500 like every other failed audit write. The
			// answer lists what the request changed, a spent approval included.
			if msg, ok := store.AuditFailureText(ctx, err, "the action"); ok {
				writeJSON(w, http.StatusInternalServerError, map[string]string{"error": msg})
				return
			}
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": redact.String(err.Error())})
			return
		}
		if g.breaker != nil {
			g.breaker.RecordSuccess(capability)
		}
		resBytes, _ := json.Marshal(result)
		red := redact.Text(string(resBytes))
		allowed := map[string]any{"redaction_hits": red.Hits, "approval_id": approvalUsed}
		if snap.allowedRepo != "" {
			allowed["allowed_repo"] = snap.allowedRepo
		}
		allowedJSON, _ := json.Marshal(allowed)
		if err := g.record(ctx, ac.Lease.TaskID, wlName, capability, "allowed", string(allowedJSON)); err != nil {
			// The side effect has happened and cannot be undone here; the
			// caller is told what changed instead of receiving the result.
			msg, _ := store.AuditFailureText(ctx, err, "the action")
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": msg})
			return
		}

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

// audit writes a record that cannot change what happens next, a refusal or
// a failed call, and logs a failed write.
func (g *Gate) audit(ctx context.Context, taskID, workload, action, outcome, detailJSON string) {
	store.LogAudit(ctx, g.store, g.auditRecord(taskID, workload, action, outcome, detailJSON))
}

// record writes a record the caller depends on and returns an error wrapping
// store.ErrAuditWrite when the write fails.
func (g *Gate) record(ctx context.Context, taskID, workload, action, outcome, detailJSON string) error {
	return store.RecordAudit(ctx, g.store, g.auditRecord(taskID, workload, action, outcome, detailJSON))
}

func (g *Gate) auditRecord(taskID, workload, action, outcome, detailJSON string) domain.AuditRecord {
	return domain.AuditRecord{
		Category:     "data_action_gate",
		TaskID:       taskID,
		WorkloadName: workload,
		Action:       action,
		Outcome:      outcome,
		DetailJSON:   detailJSON,
		CreatedAt:    g.now(),
	}
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
