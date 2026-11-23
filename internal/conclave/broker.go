package conclave

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/shadewright-labs/sablecourt/internal/config"
	"github.com/shadewright-labs/sablecourt/internal/controlplane"
	"github.com/shadewright-labs/sablecourt/internal/domain"
	mcpgate "github.com/shadewright-labs/sablecourt/internal/gate/mcp"
	"github.com/shadewright-labs/sablecourt/internal/notify"
	"github.com/shadewright-labs/sablecourt/internal/redact"
	"github.com/shadewright-labs/sablecourt/internal/store"
)

var (
	ErrWardTripped     = errors.New("conclave ward tripped")
	ErrMessageDenied   = errors.New("conclave message denied by vassal policy")
	ErrConclaveClosed  = errors.New("conclave is closed")
	ErrConclavePaused  = errors.New("conclave paused by ward; operator decree required")
)

type Options struct {
	Store      store.Store
	Reconciler *controlplane.Reconciler
	GetBundle  func() *config.Bundle
	Notifier   *notify.Notifier
	Now        func() time.Time
}

type Broker struct {
	store      store.Store
	reconciler *controlplane.Reconciler
	getBundle  func() *config.Bundle
	notifier   *notify.Notifier
	now        func() time.Time
}

func NewBroker(opts Options) *Broker {
	nowFn := opts.Now
	if nowFn == nil {
		nowFn = func() time.Time { return time.Now().UTC() }
	}
	return &Broker{
		store:      opts.Store,
		reconciler: opts.Reconciler,
		getBundle:  opts.GetBundle,
		notifier:   opts.Notifier,
		now:        nowFn,
	}
}

func (b *Broker) template(name string) (domain.Conclave, bool) {
	if b.getBundle == nil {
		return domain.Conclave{}, false
	}
	bundle := b.getBundle()
	if bundle == nil {
		return domain.Conclave{}, false
	}
	cc, ok := bundle.Conclaves[name]
	return cc, ok
}

// Open convenes a new Conclave from a manifest template, dispatches always-on Vassals,
// and optionally posts an opening operator Decree.
func (b *Broker) Open(ctx context.Context, templateName, title, initialDecree string) (domain.ConclaveSession, error) {
	tpl, ok := b.template(templateName)
	if !ok {
		return domain.ConclaveSession{}, fmt.Errorf("conclave template %q not found", templateName)
	}
	if strings.TrimSpace(title) == "" {
		title = templateName
	}
	now := b.now()
	session := domain.ConclaveSession{
		ID:           newID("ccl"),
		TemplateName: templateName,
		Title:        strings.TrimSpace(title),
		State:        domain.ConclaveStateActive,
		CreatedAt:    now,
		UpdatedAt:    now,
	}
	if err := b.store.CreateConclave(ctx, session); err != nil {
		return domain.ConclaveSession{}, err
	}

	_ = b.store.AppendAudit(ctx, domain.AuditRecord{
		Category:   "conclave",
		Action:     "conclave.open",
		Outcome:    "active",
		DetailJSON: fmt.Sprintf(`{"conclave_id":%q,"template":%q,"vassals":%d}`, session.ID, templateName, len(tpl.Spec.Vassals)),
		CreatedAt:  now,
	})

	// Dispatch always-on Vassals immediately.
	if b.reconciler != nil {
		for _, v := range tpl.Spec.Vassals {
			if v.Activation != domain.ActivationAlwaysOn {
				continue
			}
			inputBytes, _ := json.Marshal(map[string]any{
				"conclaveId": session.ID,
				"role":       v.Role,
				"title":      session.Title,
				"prompt":     strings.TrimSpace(initialDecree),
				"activation": string(v.Activation),
			})
			idem := fmt.Sprintf("conclave-%s-init-%s", session.ID, v.Role)
			_, _ = b.reconciler.SubmitTask(ctx, v.Workload, idem, string(inputBytes))
		}
	}

	if strings.TrimSpace(initialDecree) != "" {
		_, _ = b.PostMessage(ctx, session.ID, "operator", "all", "decree", initialDecree, "")
	}
	return session, nil
}

// PostMessage validates routing policy and Wards, redacts secrets, persists the message,
// and wakes any on-mention Vassals addressed by toRole.
func (b *Broker) PostMessage(ctx context.Context, conclaveID, fromRole, toRole, kind, body, taskID string) (domain.ConclaveMessage, error) {
	session, err := b.store.GetConclave(ctx, conclaveID)
	if err != nil {
		return domain.ConclaveMessage{}, err
	}
	if session.State == domain.ConclaveStateCompleted || session.State == domain.ConclaveStateCancelled {
		return domain.ConclaveMessage{}, ErrConclaveClosed
	}

	fromRole = strings.TrimSpace(strings.TrimPrefix(fromRole, "@"))
	toRole = strings.TrimSpace(strings.TrimPrefix(toRole, "@"))
	if toRole == "" {
		toRole = "all"
	}
	if kind == "" {
		if fromRole == "operator" {
			kind = "decree"
		} else {
			kind = "proposal"
		}
	}
	body = redact.String(strings.TrimSpace(body))
	if body == "" {
		return domain.ConclaveMessage{}, errors.New("message body is required")
	}

	tpl, hasTpl := b.template(session.TemplateName)
	isOperator := fromRole == "operator"

	if !isOperator {
		if session.State == domain.ConclaveStateInputRequired {
			return domain.ConclaveMessage{}, ErrConclavePaused
		}
		if hasTpl {
			if err := checkVassalCanMessage(tpl, fromRole, toRole); err != nil {
				_ = b.store.AppendAudit(ctx, domain.AuditRecord{
					Category:   "conclave",
					TaskID:     taskID,
					Action:     "conclave.post",
					Outcome:    "denied",
					DetailJSON: fmt.Sprintf(`{"conclave_id":%q,"from":%q,"to":%q,"reason":%q}`, conclaveID, fromRole, toRole, err.Error()),
					CreatedAt:  b.now(),
				})
				return domain.ConclaveMessage{}, err
			}
			if wardReason, tripped := b.checkWards(ctx, conclaveID, tpl, fromRole); tripped {
				_ = b.store.UpdateConclaveState(ctx, conclaveID, domain.ConclaveStateInputRequired, wardReason)
				_, _ = b.store.AppendConclaveMessage(ctx, domain.ConclaveMessage{
					ConclaveID: conclaveID,
					FromRole:   "system",
					ToRole:     "operator",
					Kind:       "system",
					Body:       fmt.Sprintf("Ward tripped (%s). Conclave paused; issue an operator Decree to resume or close.", wardReason),
					CreatedAt:  b.now(),
				})
				_ = b.store.AppendAudit(ctx, domain.AuditRecord{
					Category:   "conclave",
					TaskID:     taskID,
					Action:     "conclave.ward",
					Outcome:    "paused",
					DetailJSON: fmt.Sprintf(`{"conclave_id":%q,"ward":%q}`, conclaveID, wardReason),
					CreatedAt:  b.now(),
				})
				if b.notifier != nil {
					go func(id, title, reason string) {
						_ = b.notifier.Send(context.Background(), notify.Event{
							Type:   "conclave.ward_paused",
							Title:  "Conclave Ward tripped: " + title,
							Detail: fmt.Sprintf("Conclave %s paused (%s). Operator Decree required.", id, reason),
							Path:   "/conclaves/" + id,
						})
					}(conclaveID, session.Title, wardReason)
				}
				return domain.ConclaveMessage{}, fmt.Errorf("%w: %s", ErrWardTripped, wardReason)
			}
		}
	} else if session.State == domain.ConclaveStateInputRequired {
		// Operator Decree resumes a Ward-paused Conclave.
		_ = b.store.UpdateConclaveState(ctx, conclaveID, domain.ConclaveStateActive, "")
	}

	msg := domain.ConclaveMessage{
		ConclaveID: conclaveID,
		FromRole:   fromRole,
		ToRole:     toRole,
		Kind:       kind,
		Body:       body,
		TaskID:     taskID,
		CreatedAt:  b.now(),
	}
	id, err := b.store.AppendConclaveMessage(ctx, msg)
	if err != nil {
		return domain.ConclaveMessage{}, err
	}
	msg.ID = id

	// Wake on-mention Vassals addressed by this message.
	if hasTpl && b.reconciler != nil {
		for _, v := range tpl.Spec.Vassals {
			if v.Activation != domain.ActivationOnMention {
				continue
			}
			if v.Role == fromRole {
				continue
			}
			if toRole == v.Role || toRole == "all" {
				inputBytes, _ := json.Marshal(map[string]any{
					"conclaveId": conclaveID,
					"role":       v.Role,
					"fromRole":   fromRole,
					"messageId":  msg.ID,
					"prompt":     body,
					"activation": string(v.Activation),
				})
				idem := fmt.Sprintf("conclave-%s-msg-%d-%s", conclaveID, msg.ID, v.Role)
				_, _ = b.reconciler.SubmitTask(ctx, v.Workload, idem, string(inputBytes))
			}
		}
	}

	return msg, nil
}

// PutRelic stores a redacted shared artifact (diff, log, report) in the Conclave.
func (b *Broker) PutRelic(ctx context.Context, conclaveID, authorRole, name, contentType, body string) (domain.ConclaveRelic, error) {
	if _, err := b.store.GetConclave(ctx, conclaveID); err != nil {
		return domain.ConclaveRelic{}, err
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return domain.ConclaveRelic{}, errors.New("relic name is required")
	}
	if contentType == "" {
		contentType = "text/plain"
	}
	redacted := redact.String(body)
	sum := sha256.Sum256([]byte(redacted))
	relic := domain.ConclaveRelic{
		ConclaveID:  conclaveID,
		Name:        name,
		AuthorRole:  strings.TrimSpace(authorRole),
		ContentType: contentType,
		Body:        redacted,
		SHA256:      hex.EncodeToString(sum[:]),
		CreatedAt:   b.now(),
	}
	if err := b.store.PutConclaveRelic(ctx, relic); err != nil {
		return domain.ConclaveRelic{}, err
	}
	_ = b.store.AppendAudit(ctx, domain.AuditRecord{
		Category:   "conclave",
		Action:     "conclave.relic_put",
		Outcome:    "stored",
		DetailJSON: fmt.Sprintf(`{"conclave_id":%q,"name":%q,"author":%q,"sha256":%q}`, conclaveID, name, relic.AuthorRole, relic.SHA256[:12]),
		CreatedAt:  relic.CreatedAt,
	})
	return relic, nil
}

func (b *Broker) Close(ctx context.Context, conclaveID string, state domain.ConclaveState) error {
	if state != domain.ConclaveStateCompleted && state != domain.ConclaveStateCancelled {
		state = domain.ConclaveStateCompleted
	}
	if err := b.store.UpdateConclaveState(ctx, conclaveID, state, ""); err != nil {
		return err
	}
	_ = b.store.AppendAudit(ctx, domain.AuditRecord{
		Category:   "conclave",
		Action:     "conclave.close",
		Outcome:    string(state),
		DetailJSON: fmt.Sprintf(`{"conclave_id":%q}`, conclaveID),
		CreatedAt:  b.now(),
	})
	return nil
}

// ResolveRoleForWorkload maps a workload name to its declared Vassal role inside a Conclave.
func (b *Broker) ResolveRoleForWorkload(ctx context.Context, conclaveID, workloadName string) (string, error) {
	session, err := b.store.GetConclave(ctx, conclaveID)
	if err != nil {
		return "", err
	}
	tpl, ok := b.template(session.TemplateName)
	if !ok {
		return "", fmt.Errorf("conclave template %q not found", session.TemplateName)
	}
	for _, v := range tpl.Spec.Vassals {
		if v.Workload == workloadName {
			return v.Role, nil
		}
	}
	return "", fmt.Errorf("%w: workload %q is not a vassal in conclave %q", ErrMessageDenied, workloadName, conclaveID)
}

// RegisterMCPTools wires conclave.post, conclave.read, conclave.relic_put, and conclave.relic_get
// into the Data/Action Gate so Vassals can coordinate via standard MCP tool calls.
func (b *Broker) RegisterMCPTools(gate *mcpgate.Gate) {
	if gate == nil {
		return
	}
	gate.RegisterTool(mcpgate.ToolDef{
		Name:        "conclave.post",
		Description: "Post a message to a Conclave addressed to a Vassal role, operator, or all.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"conclave_id":{"type":"string"},"to":{"type":"string"},"kind":{"type":"string"},"body":{"type":"string"}},"required":["conclave_id","to","body"]}`),
		Handler: func(ctx context.Context, wl domain.Workload, taskID string, args json.RawMessage) (string, error) {
			var in struct {
				ConclaveID string `json:"conclave_id"`
				To         string `json:"to"`
				Kind       string `json:"kind"`
				Body       string `json:"body"`
			}
			if err := json.Unmarshal(args, &in); err != nil {
				return "", err
			}
			role, err := b.ResolveRoleForWorkload(ctx, in.ConclaveID, wl.Metadata.Name)
			if err != nil {
				return "", err
			}
			msg, err := b.PostMessage(ctx, in.ConclaveID, role, in.To, in.Kind, in.Body, taskID)
			if err != nil {
				return "", err
			}
			out, _ := json.Marshal(msg)
			return string(out), nil
		},
	})
	gate.RegisterTool(mcpgate.ToolDef{
		Name:        "conclave.read",
		Description: "Read messages in a Conclave since an optional sequence ID.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"conclave_id":{"type":"string"},"since_id":{"type":"integer"}},"required":["conclave_id"]}`),
		Handler: func(ctx context.Context, wl domain.Workload, taskID string, args json.RawMessage) (string, error) {
			var in struct {
				ConclaveID string `json:"conclave_id"`
				SinceID    int64  `json:"since_id"`
			}
			if err := json.Unmarshal(args, &in); err != nil {
				return "", err
			}
			if _, err := b.ResolveRoleForWorkload(ctx, in.ConclaveID, wl.Metadata.Name); err != nil {
				return "", err
			}
			msgs, err := b.store.ListConclaveMessages(ctx, in.ConclaveID, in.SinceID)
			if err != nil {
				return "", err
			}
			out, _ := json.Marshal(msgs)
			return string(out), nil
		},
	})
	gate.RegisterTool(mcpgate.ToolDef{
		Name:        "conclave.relic_put",
		Description: "Store a named shared Relic (diff, report, log) inside a Conclave.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"conclave_id":{"type":"string"},"name":{"type":"string"},"content_type":{"type":"string"},"body":{"type":"string"}},"required":["conclave_id","name","body"]}`),
		Handler: func(ctx context.Context, wl domain.Workload, taskID string, args json.RawMessage) (string, error) {
			var in struct {
				ConclaveID  string `json:"conclave_id"`
				Name        string `json:"name"`
				ContentType string `json:"content_type"`
				Body        string `json:"body"`
			}
			if err := json.Unmarshal(args, &in); err != nil {
				return "", err
			}
			role, err := b.ResolveRoleForWorkload(ctx, in.ConclaveID, wl.Metadata.Name)
			if err != nil {
				return "", err
			}
			relic, err := b.PutRelic(ctx, in.ConclaveID, role, in.Name, in.ContentType, in.Body)
			if err != nil {
				return "", err
			}
			out, _ := json.Marshal(relic)
			return string(out), nil
		},
	})
	gate.RegisterTool(mcpgate.ToolDef{
		Name:        "conclave.relic_get",
		Description: "Fetch a named shared Relic from a Conclave.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"conclave_id":{"type":"string"},"name":{"type":"string"}},"required":["conclave_id","name"]}`),
		Handler: func(ctx context.Context, wl domain.Workload, taskID string, args json.RawMessage) (string, error) {
			var in struct {
				ConclaveID string `json:"conclave_id"`
				Name       string `json:"name"`
			}
			if err := json.Unmarshal(args, &in); err != nil {
				return "", err
			}
			if _, err := b.ResolveRoleForWorkload(ctx, in.ConclaveID, wl.Metadata.Name); err != nil {
				return "", err
			}
			relic, err := b.store.GetConclaveRelic(ctx, in.ConclaveID, in.Name)
			if err != nil {
				return "", err
			}
			out, _ := json.Marshal(relic)
			return string(out), nil
		},
	})
}

func checkVassalCanMessage(tpl domain.Conclave, fromRole, toRole string) error {
	for _, v := range tpl.Spec.Vassals {
		if v.Role != fromRole {
			continue
		}
		for _, allowed := range v.CanMessage {
			if allowed == toRole || allowed == "all" {
				return nil
			}
		}
		return fmt.Errorf("%w: vassal %q cannot message %q", ErrMessageDenied, fromRole, toRole)
	}
	return fmt.Errorf("%w: unknown vassal role %q", ErrMessageDenied, fromRole)
}

func (b *Broker) checkWards(ctx context.Context, conclaveID string, tpl domain.Conclave, nextFromRole string) (string, bool) {
	wards := tpl.Spec.Wards
	msgs, err := b.store.ListConclaveMessages(ctx, conclaveID, 0)
	if err != nil {
		return "", false
	}
	// 1. Check aggregate session budget ceiling when budgetRef is declared.
	if tpl.Spec.BudgetRef != "" && b.getBundle != nil {
		if bundle := b.getBundle(); bundle != nil {
			if budget, ok := bundle.Budgets[tpl.Spec.BudgetRef]; ok {
				totals := b.sessionBudgetTotals(ctx, conclaveID, msgs)
				if budget.Spec.PerTask.MaxCostUSD > 0 && float64(totals.CostMicroUSD)/1_000_000.0 >= budget.Spec.PerTask.MaxCostUSD {
					return fmt.Sprintf("budget_exceeded($%.4f >= $%.2f)", float64(totals.CostMicroUSD)/1_000_000.0, budget.Spec.PerTask.MaxCostUSD), true
				}
				if budget.Spec.PerTask.MaxInputTokens > 0 && totals.InputTokens >= budget.Spec.PerTask.MaxInputTokens {
					return fmt.Sprintf("input_tokens_exceeded(%d >= %d)", totals.InputTokens, budget.Spec.PerTask.MaxInputTokens), true
				}
				if budget.Spec.PerTask.MaxOutputTokens > 0 && totals.OutputTokens >= budget.Spec.PerTask.MaxOutputTokens {
					return fmt.Sprintf("output_tokens_exceeded(%d >= %d)", totals.OutputTokens, budget.Spec.PerTask.MaxOutputTokens), true
				}
			}
		}
	}
	// 2. Count non-system messages since the last operator Decree.
	var sinceDecree []domain.ConclaveMessage
	for _, m := range msgs {
		if m.FromRole == "operator" {
			sinceDecree = nil
			continue
		}
		if m.FromRole == "system" {
			continue
		}
		sinceDecree = append(sinceDecree, m)
	}
	if wards.MaxMessages > 0 && len(sinceDecree) >= wards.MaxMessages {
		return fmt.Sprintf("max_messages_reached(%d)", wards.MaxMessages), true
	}
	if wards.MaxPingPong > 0 && len(sinceDecree) >= wards.MaxPingPong {
		// Check if the last MaxPingPong messages bounce between at most 2 non-operator roles.
		tail := sinceDecree[len(sinceDecree)-wards.MaxPingPong:]
		participants := map[string]struct{}{nextFromRole: {}}
		for _, m := range tail {
			participants[m.FromRole] = struct{}{}
		}
		if len(participants) <= 2 {
			return fmt.Sprintf("ping_pong_detected(%d)", wards.MaxPingPong), true
		}
	}
	return "", false
}

func (b *Broker) sessionBudgetTotals(ctx context.Context, conclaveID string, msgs []domain.ConclaveMessage) domain.BudgetTotals {
	taskIDs := make(map[string]struct{})
	for _, m := range msgs {
		if m.TaskID != "" {
			taskIDs[m.TaskID] = struct{}{}
		}
	}
	if tasks, err := b.store.ListTasks(ctx); err == nil {
		needle := fmt.Sprintf(`"conclaveId":%q`, conclaveID)
		for _, t := range tasks {
			if strings.Contains(t.InputJSON, needle) {
				taskIDs[t.ID] = struct{}{}
			}
		}
	}
	var sum domain.BudgetTotals
	for tid := range taskIDs {
		if tot, err := b.store.TaskBudgetTotals(ctx, tid); err == nil {
			sum.InputTokens += tot.InputTokens
			sum.OutputTokens += tot.OutputTokens
			sum.CostMicroUSD += tot.CostMicroUSD
		}
	}
	return sum
}

func newID(prefix string) string {
	var raw [6]byte
	_, _ = rand.Read(raw[:])
	return prefix + "-" + hex.EncodeToString(raw[:])
}
