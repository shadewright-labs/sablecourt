package integrations

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/shadewright-labs/sablecourt/internal/conclave"
	"github.com/shadewright-labs/sablecourt/internal/config"
	"github.com/shadewright-labs/sablecourt/internal/controlplane"
	"github.com/shadewright-labs/sablecourt/internal/domain"
	"github.com/shadewright-labs/sablecourt/internal/redact"
	"github.com/shadewright-labs/sablecourt/internal/secret"
	"github.com/shadewright-labs/sablecourt/internal/store"
)

const (
	// ShadewrightRole is the identity recorded on Conclave messages and Relics
	// written by the Shadewright integration.
	ShadewrightRole = "integration:shadewright"

	// ShadewrightReportKind is the message kind recorded for all integration posts.
	ShadewrightReportKind = "report"

	// ShadewrightTokenSecretName is the SecretStore key holding the scoped bearer token.
	ShadewrightTokenSecretName = "shadewright_bearer_token"

	// ShadewrightIncidentPath is the HTTP route for incident escalation intake.
	ShadewrightIncidentPath = "/api/v1/integrations/shadewright/incidents"

	// ShadewrightVerificationPath is the HTTP route for rollout verification reports.
	ShadewrightVerificationPath = "/api/v1/integrations/shadewright/verifications"

	maxRequestBodyBytes = 2 << 20 // 2 MiB
	maxStatusRunes      = 64
	maxTitleFieldRunes  = 128
)

var (
	incidentIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,127}$`)
	conclaveLocks     = &keyedLocker{entries: make(map[string]*keyedLockEntry)}
)

// IsShadewrightRoute reports whether method and path identify one of the
// self-authenticated Shadewright integration endpoints.
func IsShadewrightRoute(method, path string) bool {
	return method == http.MethodPost && (path == ShadewrightIncidentPath || path == ShadewrightVerificationPath)
}

// RejectShadewrightTokenOnOtherRoutes refuses requests carrying the configured
// Shadewright bearer token on any route other than the Shadewright integration
// endpoints, even when operator bearer authentication is disabled.
func RejectShadewrightTokenOnOtherRoutes(next http.Handler, secrets secret.SecretStore, secretNames ...string) http.Handler {
	secName := ShadewrightTokenSecretName
	if len(secretNames) > 0 && strings.TrimSpace(secretNames[0]) != "" {
		secName = strings.TrimSpace(secretNames[0])
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if IsShadewrightRoute(r.Method, r.URL.Path) || secrets == nil {
			next.ServeHTTP(w, r)
			return
		}
		authValues := r.Header.Values("Authorization")
		hasBearer := false
		for _, v := range authValues {
			if _, ok := extractBearerLoose(v); ok {
				hasBearer = true
				break
			}
		}
		if !hasBearer {
			next.ServeHTTP(w, r)
			return
		}
		rawWant, err := secrets.Get(r.Context(), secName)
		want := strings.TrimSpace(rawWant)
		if err == nil && want != "" {
			for _, v := range authValues {
				got, ok := extractBearerLoose(v)
				if ok && subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1 {
					w.Header().Set("WWW-Authenticate", `Bearer realm="sablecourt"`)
					writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "shadewright integration token is only valid on integration routes"})
					return
				}
			}
		}
		next.ServeHTTP(w, r)
	})
}

func extractBearerLoose(headerVal string) (string, bool) {
	if len(headerVal) < len("Bearer ") || !strings.EqualFold(headerVal[:len("Bearer ")], "Bearer ") {
		return "", false
	}
	tok := strings.TrimSpace(headerVal[len("Bearer "):])
	return tok, tok != ""
}

// ShadewrightOptions configures the Shadewright integration handlers.
type ShadewrightOptions struct {
	Store           store.Store
	Secrets         secret.SecretStore
	SecretName      string
	GetBundle       func() *config.Bundle
	Broker          *conclave.Broker
	Reconciler      *controlplane.Reconciler
	DefaultTemplate string
	Now             func() time.Time
}

// ShadewrightHandler serves authenticated Shadewright incident escalation and
// rollout verification endpoints.
type ShadewrightHandler struct {
	store           store.Store
	secrets         secret.SecretStore
	secretName      string
	getBundle       func() *config.Bundle
	broker          *conclave.Broker
	reconciler      *controlplane.Reconciler
	defaultTemplate string
	now             func() time.Time
}

// NewShadewright constructs a ShadewrightHandler with fail-closed defaults.
func NewShadewright(opts ShadewrightOptions) *ShadewrightHandler {
	secName := strings.TrimSpace(opts.SecretName)
	if secName == "" {
		secName = ShadewrightTokenSecretName
	}
	nowFn := opts.Now
	if nowFn == nil {
		nowFn = func() time.Time { return time.Now().UTC() }
	}
	return &ShadewrightHandler{
		store:           opts.Store,
		secrets:         opts.Secrets,
		secretName:      secName,
		getBundle:       opts.GetBundle,
		broker:          opts.Broker,
		reconciler:      opts.Reconciler,
		defaultTemplate: strings.TrimSpace(opts.DefaultTemplate),
		now:             nowFn,
	}
}

// RegisterRoutes attaches the Shadewright incident and verification endpoints to mux.
func (h *ShadewrightHandler) RegisterRoutes(mux *http.ServeMux) {
	mux.Handle("POST "+ShadewrightIncidentPath, h.IncidentHandler())
	mux.Handle("POST "+ShadewrightVerificationPath, h.VerificationHandler())
}

type incidentRequest struct {
	ConclaveTemplate string         `json:"conclaveTemplate"`
	IncidentID       string         `json:"incidentId"`
	Cluster          string         `json:"cluster"`
	Namespace        string         `json:"namespace"`
	Workload         string         `json:"workload"`
	Summary          string         `json:"summary"`
	Evidence         map[string]any `json:"evidence"`
	Logs             string         `json:"logs"`
}

type verificationRequest struct {
	ConclaveID string         `json:"conclaveId"`
	IncidentID string         `json:"incidentId"`
	PRNumber   int            `json:"prNumber"`
	Status     string         `json:"status"`
	Summary    string         `json:"summary"`
	Report     map[string]any `json:"report"`
}

// IncidentHandler handles POST /api/v1/integrations/shadewright/incidents.
func (h *ShadewrightHandler) IncidentHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !h.authenticate(w, r) {
			return
		}
		if h.store == nil || h.broker == nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "integration handler unavailable"})
			return
		}

		var req incidentRequest
		if !decodeJSONObject(w, r, &req) {
			return
		}
		if !incidentIDPattern.MatchString(req.IncidentID) {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "incidentId must match [A-Za-z0-9][A-Za-z0-9_-]{0,127}"})
			return
		}
		summary := strings.TrimSpace(req.Summary)
		if summary == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "summary is required"})
			return
		}

		id := shadewrightConclaveID(req.IncidentID)
		relicName := "incident-" + req.IncidentID + ".json"
		evidence, err := json.MarshalIndent(map[string]any{
			"incidentId": req.IncidentID,
			"cluster":    req.Cluster,
			"namespace":  req.Namespace,
			"workload":   req.Workload,
			"summary":    req.Summary,
			"evidence":   req.Evidence,
			"logs":       req.Logs,
		}, "", "  ")
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid incident evidence"})
			return
		}
		redactedEvidence := redact.String(string(evidence))

		unlock := conclaveLocks.Lock(id)
		defer unlock()

		ctx := r.Context()
		fail := func(err error) {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": redact.String(err.Error())})
		}

		sess, err := h.store.GetConclave(ctx, id)
		exists := err == nil
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			fail(err)
			return
		}

		var relic domain.ConclaveRelic
		var hasRelic, opened bool
		tplName := strings.TrimSpace(req.ConclaveTemplate)
		if tplName == "" {
			tplName = h.defaultTemplate
		}
		if exists {
			tplName = sess.TemplateName
			relic, err = h.store.GetConclaveRelic(ctx, id, relicName)
			hasRelic = err == nil
			if err != nil && !errors.Is(err, store.ErrNotFound) {
				fail(err)
				return
			}
			var finished bool
			opened, finished, err = incidentAuditProgress(ctx, h.store, id)
			if err != nil {
				fail(err)
				return
			}
			if finished {
				if !hasRelic {
					writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "incident Relic is missing"})
					return
				}
				if relic.Body != redactedEvidence {
					writeJSON(w, http.StatusConflict, map[string]string{"error": "retry evidence differs from stored incident"})
					return
				}
				writeJSON(w, http.StatusOK, map[string]any{"conclave": sess, "relic": relic})
				return
			}
			if sess.State != domain.ConclaveStateActive {
				writeJSON(w, http.StatusConflict, map[string]string{"error": "incident Conclave is inactive"})
				return
			}
		}

		var bundle *config.Bundle
		if h.getBundle != nil {
			bundle = h.getBundle()
		}
		if tplName == "" || bundle == nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "conclaveTemplate or a configured incident template is required"})
			return
		}
		tpl, ok := bundle.Conclaves[tplName]
		if !ok {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "conclave template not found"})
			return
		}

		now := h.now()
		if !exists {
			title := redact.String(fmt.Sprintf(
				"Shadewright Incident %s (%s/%s)",
				req.IncidentID,
				sanitizeTitleField(req.Namespace),
				sanitizeTitleField(req.Workload),
			))
			sess = domain.ConclaveSession{
				ID:           id,
				TemplateName: tplName,
				Title:        title,
				State:        domain.ConclaveStateActive,
				CreatedAt:    now,
				UpdatedAt:    now,
			}
			if err := h.store.CreateConclave(ctx, sess); err != nil {
				fail(err)
				return
			}
		}
		if !opened {
			detail, err := marshalDetailJSON(map[string]any{
				"conclave_id": id,
				"template":    tplName,
				"vassals":     len(tpl.Spec.Vassals),
			})
			if err != nil {
				fail(err)
				return
			}
			if err := h.store.AppendAudit(ctx, domain.AuditRecord{
				Category:   "conclave",
				Action:     "conclave.open",
				Outcome:    "active",
				DetailJSON: detail,
				CreatedAt:  now,
			}); err != nil {
				fail(err)
				return
			}
		}
		if !hasRelic {
			relic, err = h.broker.PutRelic(ctx, id, ShadewrightRole, relicName, "application/json", string(evidence))
			if err != nil {
				writeConclaveWriteError(w, err)
				return
			}
		} else if relic.Body != redactedEvidence {
			writeJSON(w, http.StatusConflict, map[string]string{"error": "retry evidence differs from stored incident"})
			return
		}

		// Keep untrusted caller fields out of always-on task prompts; vassals read
		// the redacted incident Relic and the untrusted integration report instead.
		initPrompt := fmt.Sprintf("Review untrusted Shadewright incident Relic %s and report findings.", relicName)
		initTitle := fmt.Sprintf("Shadewright Incident %s", req.IncidentID)
		if h.reconciler != nil {
			for _, v := range tpl.Spec.Vassals {
				if v.Activation != domain.ActivationAlwaysOn {
					continue
				}
				input, _ := json.Marshal(map[string]any{
					"conclaveId": id,
					"role":       v.Role,
					"title":      initTitle,
					"prompt":     initPrompt,
					"activation": string(v.Activation),
				})
				if _, err := h.reconciler.SubmitTask(ctx, v.Workload, fmt.Sprintf("conclave-%s-init-%s", id, v.Role), string(input)); err != nil {
					fail(err)
					return
				}
			}
		}

		reportBody := redact.String(fmt.Sprintf("Shadewright incident %s escalated (Relic: %s): %s", req.IncidentID, relicName, summary))
		msgs, err := h.store.ListConclaveMessages(ctx, id, 0)
		if err != nil {
			fail(err)
			return
		}
		var opening *domain.ConclaveMessage
		for i := range msgs {
			msg := &msgs[i]
			if msg.FromRole == ShadewrightRole && msg.ToRole == "all" && msg.Kind == ShadewrightReportKind && msg.TaskID == "" && msg.Body == reportBody {
				opening = msg
				break
			}
		}
		if opening == nil {
			current, err := h.store.GetConclave(ctx, id)
			if err != nil {
				fail(err)
				return
			}
			if current.State != domain.ConclaveStateActive {
				writeJSON(w, http.StatusConflict, map[string]string{"error": "incident Conclave is inactive"})
				return
			}
			msg := domain.ConclaveMessage{
				ConclaveID: id,
				FromRole:   ShadewrightRole,
				ToRole:     "all",
				Kind:       ShadewrightReportKind,
				Body:       reportBody,
				CreatedAt:  now,
			}
			msgID, err := h.store.AppendConclaveMessage(ctx, msg)
			if err != nil {
				fail(err)
				return
			}
			msg.ID = msgID
			opening = &msg
		}
		if err := h.broker.WakeMentions(ctx, tpl, *opening); err != nil {
			fail(err)
			return
		}
		detail, err := marshalDetailJSON(map[string]any{
			"incident_id": req.IncidentID,
			"conclave_id": id,
			"relic":       relicName,
		})
		if err != nil {
			fail(err)
			return
		}
		if err := h.store.AppendAudit(ctx, domain.AuditRecord{
			Category:   "integration",
			Action:     "shadewright.incident_escalated",
			Outcome:    "ok",
			DetailJSON: detail,
			CreatedAt:  now,
		}); err != nil {
			fail(err)
			return
		}

		status := http.StatusCreated
		if exists {
			status = http.StatusOK
		}
		writeJSON(w, status, map[string]any{"conclave": sess, "relic": relic})
	})
}

// VerificationHandler handles POST /api/v1/integrations/shadewright/verifications.
func (h *ShadewrightHandler) VerificationHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !h.authenticate(w, r) {
			return
		}
		if h.store == nil || h.broker == nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "integration handler unavailable"})
			return
		}

		var req verificationRequest
		if !decodeJSONObject(w, r, &req) {
			return
		}
		conclaveID := strings.TrimSpace(req.ConclaveID)
		statusText := strings.TrimSpace(req.Status)
		incidentID := strings.TrimSpace(req.IncidentID)
		if conclaveID == "" || statusText == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "conclaveId and status are required"})
			return
		}
		if !validStatusText(statusText) {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "status must be at most 64 printable characters"})
			return
		}
		if incidentID != "" && !incidentIDPattern.MatchString(incidentID) {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "incidentId must match [A-Za-z0-9][A-Za-z0-9_-]{0,127}"})
			return
		}

		unlock := conclaveLocks.Lock(conclaveID)
		defer unlock()

		ctx := r.Context()
		fail := func(err error) {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": redact.String(err.Error())})
		}

		sess, err := h.store.GetConclave(ctx, conclaveID)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				writeJSON(w, http.StatusNotFound, map[string]string{"error": "conclave not found"})
				return
			}
			fail(err)
			return
		}
		owned, err := conclaveOpenedByShadewright(ctx, h.store, conclaveID)
		if err != nil {
			fail(err)
			return
		}
		if !owned {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "conclave not found"})
			return
		}
		if incidentID != "" && shadewrightConclaveID(incidentID) != conclaveID {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "incidentId does not match conclaveId"})
			return
		}
		switch sess.State {
		case domain.ConclaveStateActive:
		case domain.ConclaveStateInputRequired:
			writeJSON(w, http.StatusConflict, map[string]string{"error": conclave.ErrConclavePaused.Error()})
			return
		default:
			writeJSON(w, http.StatusConflict, map[string]string{"error": conclave.ErrConclaveClosed.Error()})
			return
		}

		now := h.now()
		relicName := fmt.Sprintf("verification-%d.json", now.UnixNano())
		if incidentID != "" {
			relicName = fmt.Sprintf("verification-%s.json", incidentID)
		}
		payload, err := json.MarshalIndent(map[string]any{
			"conclaveId": conclaveID,
			"incidentId": incidentID,
			"prNumber":   req.PRNumber,
			"status":     statusText,
			"summary":    req.Summary,
			"report":     req.Report,
		}, "", "  ")
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid verification report"})
			return
		}

		relic, err := h.broker.PutRelic(ctx, conclaveID, ShadewrightRole, relicName, "application/json", string(payload))
		if err != nil {
			writeConclaveWriteError(w, err)
			return
		}

		summary := strings.TrimSpace(req.Summary)
		msgText := fmt.Sprintf("Shadewright Verifier report (%s) (Relic: %s)", statusText, relic.Name)
		if summary != "" {
			msgText = fmt.Sprintf("Shadewright Verifier report (%s): %s (Relic: %s)", statusText, summary, relic.Name)
		}
		msgText = redact.String(msgText)

		// Reuse an unaudited trailing identical report when retrying after an audit failure.
		var msg domain.ConclaveMessage
		msgs, err := h.store.ListConclaveMessages(ctx, conclaveID, 0)
		if err != nil {
			fail(err)
			return
		}
		if len(msgs) > 0 {
			last := msgs[len(msgs)-1]
			if last.FromRole == ShadewrightRole && last.ToRole == "operator" && last.Kind == ShadewrightReportKind && last.TaskID == "" && last.Body == msgText {
				audited, auditErr := verificationMessageAudited(ctx, h.store, conclaveID, last.ID)
				if auditErr != nil {
					fail(auditErr)
					return
				}
				if !audited {
					msg = last
				}
			}
		}
		if msg.ID == 0 {
			current, err := h.store.GetConclave(ctx, conclaveID)
			if err != nil {
				fail(err)
				return
			}
			if current.State != domain.ConclaveStateActive {
				if current.State == domain.ConclaveStateInputRequired {
					writeJSON(w, http.StatusConflict, map[string]string{"error": conclave.ErrConclavePaused.Error()})
					return
				}
				writeJSON(w, http.StatusConflict, map[string]string{"error": conclave.ErrConclaveClosed.Error()})
				return
			}
			msg = domain.ConclaveMessage{
				ConclaveID: conclaveID,
				FromRole:   ShadewrightRole,
				ToRole:     "operator",
				Kind:       ShadewrightReportKind,
				Body:       msgText,
				CreatedAt:  now,
			}
			msgID, err := h.store.AppendConclaveMessage(ctx, msg)
			if err != nil {
				fail(err)
				return
			}
			msg.ID = msgID
		}

		redactedStatus := redact.String(statusText)
		detail, err := marshalDetailJSON(map[string]any{
			"conclave_id": conclaveID,
			"incident_id": incidentID,
			"status":      redactedStatus,
			"relic":       relic.Name,
			"message_id":  msg.ID,
		})
		if err != nil {
			fail(err)
			return
		}
		if err := h.store.AppendAudit(ctx, domain.AuditRecord{
			Category:   "integration",
			Action:     "shadewright.verification_posted",
			Outcome:    redactedStatus,
			DetailJSON: detail,
			CreatedAt:  now,
		}); err != nil {
			fail(err)
			return
		}

		writeJSON(w, http.StatusCreated, map[string]any{
			"status":  "recorded",
			"relic":   relic,
			"message": msg,
		})
	})
}

func (h *ShadewrightHandler) authenticate(w http.ResponseWriter, r *http.Request) bool {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "POST required"})
		return false
	}
	if h.secrets == nil {
		w.Header().Set("WWW-Authenticate", `Bearer realm="sablecourt"`)
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "shadewright_bearer_token not configured"})
		return false
	}
	rawWant, err := h.secrets.Get(r.Context(), h.secretName)
	want := strings.TrimSpace(rawWant)
	if err != nil || want == "" {
		w.Header().Set("WWW-Authenticate", `Bearer realm="sablecourt"`)
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "shadewright_bearer_token not configured"})
		return false
	}
	authValues := r.Header.Values("Authorization")
	if len(authValues) != 1 || !strings.HasPrefix(authValues[0], "Bearer ") {
		w.Header().Set("WWW-Authenticate", `Bearer realm="sablecourt"`)
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "integration bearer token required"})
		return false
	}
	got := strings.TrimPrefix(authValues[0], "Bearer ")
	if got == "" || subtle.ConstantTimeCompare([]byte(got), []byte(want)) != 1 {
		w.Header().Set("WWW-Authenticate", `Bearer realm="sablecourt"`)
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid integration bearer token"})
		return false
	}
	return true
}

func decodeJSONObject(w http.ResponseWriter, r *http.Request, dst any) bool {
	if r.Body == nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "request body is required"})
		return false
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBodyBytes)
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "request body too large"})
			return false
		}
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "failed to read request body"})
		return false
	}
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "JSON object body is required"})
		return false
	}
	dec := json.NewDecoder(bytes.NewReader(trimmed))
	if err := dec.Decode(dst); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON payload"})
		return false
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "unexpected trailing data in JSON payload"})
		return false
	}
	return true
}

func validStatusText(s string) bool {
	if s == "" {
		return false
	}
	count := 0
	for _, r := range s {
		count++
		if count > maxStatusRunes || unicode.IsControl(r) {
			return false
		}
	}
	return true
}

func sanitizeTitleField(s string) string {
	var b strings.Builder
	count := 0
	for _, r := range strings.TrimSpace(s) {
		if unicode.IsControl(r) {
			continue
		}
		count++
		if count > maxTitleFieldRunes {
			break
		}
		b.WriteRune(r)
	}
	return b.String()
}

func marshalDetailJSON(v map[string]any) (string, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

func writeConclaveWriteError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "conclave not found"})
	case errors.Is(err, conclave.ErrConclavePaused), errors.Is(err, conclave.ErrConclaveClosed):
		writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
	default:
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": redact.String(err.Error())})
	}
}

func shadewrightConclaveID(incidentID string) string {
	sum := sha256.Sum256([]byte("shadewright\x00" + incidentID))
	return fmt.Sprintf("ccl-shadewright-%x", sum)
}

func isShadewrightConclaveID(id string) bool {
	const prefix = "ccl-shadewright-"
	if !strings.HasPrefix(id, prefix) {
		return false
	}
	hexPart := id[len(prefix):]
	if len(hexPart) != sha256.Size*2 {
		return false
	}
	for i := 0; i < len(hexPart); i++ {
		c := hexPart[i]
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}
	return true
}

func incidentAuditProgress(ctx context.Context, st store.Store, id string) (opened, finished bool, err error) {
	for limit := 64; limit > 0; limit *= 2 {
		records, err := st.ListAudit(ctx, limit)
		if err != nil {
			return false, false, err
		}
		for _, rec := range records {
			open := rec.Category == "conclave" && rec.Action == "conclave.open" && rec.Outcome == "active"
			done := rec.Category == "integration" && rec.Action == "shadewright.incident_escalated" && rec.Outcome == "ok"
			if !open && !done {
				continue
			}
			var detail struct {
				ConclaveID string `json:"conclave_id"`
			}
			if err := json.Unmarshal([]byte(rec.DetailJSON), &detail); err != nil {
				return false, false, err
			}
			if detail.ConclaveID == id {
				return true, done, nil
			}
		}
		if len(records) < limit {
			return false, false, nil
		}
	}
	return false, false, errors.New("audit scan limit overflow")
}

func conclaveOpenedByShadewright(ctx context.Context, st store.Store, id string) (bool, error) {
	if !isShadewrightConclaveID(id) {
		return false, nil
	}
	for limit := 64; limit > 0; limit *= 2 {
		records, err := st.ListAudit(ctx, limit)
		if err != nil {
			return false, err
		}
		for _, rec := range records {
			if rec.Category != "integration" || rec.Action != "shadewright.incident_escalated" || rec.Outcome != "ok" {
				continue
			}
			var detail struct {
				ConclaveID string `json:"conclave_id"`
			}
			if err := json.Unmarshal([]byte(rec.DetailJSON), &detail); err != nil {
				return false, err
			}
			if detail.ConclaveID == id {
				return true, nil
			}
		}
		if len(records) < limit {
			return false, nil
		}
	}
	return false, errors.New("audit scan limit overflow")
}

func verificationMessageAudited(ctx context.Context, st store.Store, conclaveID string, messageID int64) (bool, error) {
	for limit := 64; limit > 0; limit *= 2 {
		records, err := st.ListAudit(ctx, limit)
		if err != nil {
			return false, err
		}
		for _, rec := range records {
			if rec.Category != "integration" {
				continue
			}
			if rec.Action == "shadewright.incident_escalated" && rec.Outcome == "ok" {
				var detail struct {
					ConclaveID string `json:"conclave_id"`
				}
				if err := json.Unmarshal([]byte(rec.DetailJSON), &detail); err != nil {
					return false, err
				}
				if detail.ConclaveID == conclaveID {
					return false, nil
				}
				continue
			}
			if rec.Action != "shadewright.verification_posted" {
				continue
			}
			var detail struct {
				ConclaveID string `json:"conclave_id"`
				MessageID  int64  `json:"message_id"`
			}
			if err := json.Unmarshal([]byte(rec.DetailJSON), &detail); err != nil {
				return false, err
			}
			if detail.ConclaveID == conclaveID && detail.MessageID == messageID {
				return true, nil
			}
		}
		if len(records) < limit {
			return false, nil
		}
	}
	return false, errors.New("audit scan limit overflow")
}

type keyedLockEntry struct {
	mu   sync.Mutex
	refs int
}

type keyedLocker struct {
	mu      sync.Mutex
	entries map[string]*keyedLockEntry
}

func (k *keyedLocker) Lock(key string) func() {
	k.mu.Lock()
	entry, ok := k.entries[key]
	if !ok {
		entry = &keyedLockEntry{}
		k.entries[key] = entry
	}
	entry.refs++
	k.mu.Unlock()

	entry.mu.Lock()
	return func() {
		entry.mu.Unlock()
		k.mu.Lock()
		entry.refs--
		if entry.refs == 0 {
			delete(k.entries, key)
		}
		k.mu.Unlock()
	}
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
