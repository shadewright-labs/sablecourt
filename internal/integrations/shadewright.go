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
)

var (
	incidentIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,127}$`)
	conclaveLocks     = &keyedLocker{entries: make(map[string]*keyedLockEntry)}
)

// IsShadewrightRequest reports whether the request matches one of the
// self-authenticated Shadewright integration endpoints.
func IsShadewrightRequest(r *http.Request) bool {
	path := r.URL.Path
	return r.Method == http.MethodPost &&
		(path == ShadewrightIncidentPath || path == ShadewrightVerificationPath) &&
		strings.Count(r.URL.EscapedPath(), "/") == strings.Count(path, "/")
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
		if IsShadewrightRequest(r) || secrets == nil {
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
		if err != nil && !errors.Is(err, secret.ErrSecretNotFound) {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "integration token lookup unavailable"})
			return
		}
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
		res := h.recordIncident(r.Context(), req)
		writeJSON(w, res.code, res.body)
	})
}

func (h *ShadewrightHandler) recordIncident(ctx context.Context, req incidentRequest) integrationResponse {
	if !incidentIDPattern.MatchString(req.IncidentID) {
		return errorResponse(http.StatusBadRequest, "incidentId must match [A-Za-z0-9][A-Za-z0-9_-]{0,127}")
	}
	summary := strings.TrimSpace(req.Summary)
	if summary == "" {
		return errorResponse(http.StatusBadRequest, "summary is required")
	}

	id := shadewrightConclaveID(req.IncidentID)
	title := shadewrightIncidentTitle(id)
	relicName := "incident-" + id + ".json"
	evidence, err := json.Marshal(map[string]any{
		"incidentId": req.IncidentID,
		"cluster":    req.Cluster,
		"namespace":  req.Namespace,
		"workload":   req.Workload,
		"summary":    req.Summary,
		"evidence":   req.Evidence,
		"logs":       req.Logs,
	})
	if err != nil {
		return errorResponse(http.StatusBadRequest, "invalid incident evidence")
	}
	redactedEvidence := redact.String(string(evidence))

	unlock := conclaveLocks.Lock(id)
	defer unlock()

	sess, err := h.store.GetConclave(ctx, id)
	exists := err == nil
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return internalErrorResponse(err)
	}

	var relic domain.ConclaveRelic
	var hasRelic, opened bool
	tplName := strings.TrimSpace(req.ConclaveTemplate)
	if tplName == "" {
		tplName = h.defaultTemplate
	}
	if exists {
		if !isShadewrightOwned(sess) {
			return errorResponse(http.StatusConflict, "incident Conclave ID is already in use")
		}
		tplName = sess.TemplateName
		relic, err = h.store.GetConclaveRelic(ctx, id, relicName)
		hasRelic = err == nil
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return internalErrorResponse(err)
		}
		var finished bool
		opened, finished, err = incidentAuditProgress(ctx, h.store, id)
		if err != nil {
			return internalErrorResponse(err)
		}
		if finished {
			if !hasRelic {
				return errorResponse(http.StatusServiceUnavailable, "incident Relic is missing")
			}
			if relic.Body != redactedEvidence {
				return errorResponse(http.StatusConflict, "retry evidence differs from stored incident")
			}
			return integrationResponse{http.StatusOK, map[string]any{"conclave": sess, "relic": relic}}
		}
		if sess.State != domain.ConclaveStateActive {
			return errorResponse(http.StatusConflict, "incident Conclave is inactive")
		}
	}

	var bundle *config.Bundle
	if h.getBundle != nil {
		bundle = h.getBundle()
	}
	if tplName == "" || bundle == nil {
		return errorResponse(http.StatusBadRequest, "conclaveTemplate or a configured incident template is required")
	}
	tpl, ok := bundle.Conclaves[tplName]
	if !ok {
		return errorResponse(http.StatusBadRequest, "conclave template not found")
	}

	now := h.now()
	if !exists {
		sess = domain.ConclaveSession{
			ID:           id,
			TemplateName: tplName,
			Title:        title,
			State:        domain.ConclaveStateActive,
			CreatedAt:    now,
			UpdatedAt:    now,
		}
		if err := h.store.CreateConclave(ctx, sess); err != nil {
			return internalErrorResponse(err)
		}
	}
	if !opened {
		detail, err := marshalDetailJSON(map[string]any{
			"conclave_id": id,
			"creator":     ShadewrightRole,
			"template":    tplName,
			"vassals":     len(tpl.Spec.Vassals),
		})
		if err != nil {
			return internalErrorResponse(err)
		}
		if err := h.store.AppendAudit(ctx, domain.AuditRecord{
			Category:   "conclave",
			Action:     "conclave.open",
			Outcome:    "active",
			DetailJSON: detail,
			CreatedAt:  now,
		}); err != nil {
			return internalErrorResponse(err)
		}
	}
	if !hasRelic {
		relic, err = h.broker.PutRelic(ctx, id, ShadewrightRole, relicName, "application/json", string(evidence))
		if err != nil {
			return conclaveWriteError(err)
		}
	} else if relic.Body != redactedEvidence {
		return errorResponse(http.StatusConflict, "retry evidence differs from stored incident")
	}

	// Keep untrusted caller fields out of always-on task prompts; vassals read
	// the redacted incident Relic and the untrusted integration report instead.
	initPrompt := fmt.Sprintf("Review untrusted Shadewright incident Relic %s and report findings.", relicName)
	if h.reconciler != nil {
		if err := h.broker.ActivateAlwaysOn(ctx, sess, tpl, initPrompt); err != nil {
			return conclaveWriteError(err)
		}
	}

	reportBody := redact.String(fmt.Sprintf("Shadewright incident escalated (Relic: %s): %s", relicName, summary))
	msgs, err := h.store.ListConclaveMessages(ctx, id, 0)
	if err != nil {
		return internalErrorResponse(err)
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
			return internalErrorResponse(err)
		}
		if current.State != domain.ConclaveStateActive {
			return errorResponse(http.StatusConflict, "incident Conclave is inactive")
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
			return internalErrorResponse(err)
		}
		msg.ID = msgID
		opening = &msg
	}
	if err := h.broker.WakeMentions(ctx, tpl, *opening); err != nil {
		return internalErrorResponse(err)
	}
	detail, err := marshalDetailJSON(map[string]any{
		"conclave_id": id,
		"relic":       relicName,
	})
	if err != nil {
		return internalErrorResponse(err)
	}
	if err := h.store.AppendAudit(ctx, domain.AuditRecord{
		Category:   "integration",
		Action:     "shadewright.incident_escalated",
		Outcome:    "ok",
		DetailJSON: detail,
		CreatedAt:  now,
	}); err != nil {
		return internalErrorResponse(err)
	}

	status := http.StatusCreated
	if exists {
		status = http.StatusOK
	}
	return integrationResponse{status, map[string]any{"conclave": sess, "relic": relic}}
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
		res := h.recordVerification(r.Context(), req)
		writeJSON(w, res.code, res.body)
	})
}

func (h *ShadewrightHandler) recordVerification(ctx context.Context, req verificationRequest) integrationResponse {
	conclaveID := strings.TrimSpace(req.ConclaveID)
	statusText := strings.TrimSpace(req.Status)
	incidentID := strings.TrimSpace(req.IncidentID)
	if conclaveID == "" || statusText == "" {
		return errorResponse(http.StatusBadRequest, "conclaveId and status are required")
	}
	if !validStatusText(statusText) {
		return errorResponse(http.StatusBadRequest, "status must be at most 64 printable characters")
	}
	if incidentID != "" && !incidentIDPattern.MatchString(incidentID) {
		return errorResponse(http.StatusBadRequest, "incidentId must match [A-Za-z0-9][A-Za-z0-9_-]{0,127}")
	}

	unlock := conclaveLocks.Lock(conclaveID)
	defer unlock()

	sess, err := h.store.GetConclave(ctx, conclaveID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return errorResponse(http.StatusNotFound, "conclave not found")
		}
		return internalErrorResponse(err)
	}
	if !isShadewrightOwned(sess) {
		return errorResponse(http.StatusNotFound, "conclave not found")
	}
	_, finished, err := incidentAuditProgress(ctx, h.store, conclaveID)
	if err != nil {
		return internalErrorResponse(err)
	}
	if !finished {
		return errorResponse(http.StatusNotFound, "conclave not found")
	}
	if incidentID != "" && shadewrightConclaveID(incidentID) != conclaveID {
		return errorResponse(http.StatusBadRequest, "incidentId does not match conclaveId")
	}
	switch sess.State {
	case domain.ConclaveStateActive:
	case domain.ConclaveStateInputRequired:
		return conclaveWriteError(conclave.ErrConclavePaused)
	default:
		return conclaveWriteError(conclave.ErrConclaveClosed)
	}

	payload, err := json.Marshal(map[string]any{
		"conclaveId": conclaveID,
		"incidentId": incidentID,
		"prNumber":   req.PRNumber,
		"status":     statusText,
		"summary":    req.Summary,
		"report":     req.Report,
	})
	if err != nil {
		return errorResponse(http.StatusBadRequest, "invalid verification report")
	}

	// The report key is stored in the relic name, the message's relic reference,
	// and the audit. The same request can finish a partial write after a restart.
	relicName := fmt.Sprintf("verification-%x.json", sha256.Sum256(payload))
	redactedPayload := redact.String(string(payload))
	relic, err := h.store.GetConclaveRelic(ctx, conclaveID, relicName)
	hasRelic := err == nil
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return internalErrorResponse(err)
	}
	if hasRelic && (relic.AuthorRole != ShadewrightRole || relic.ContentType != "application/json" ||
		relic.Body != redactedPayload || relic.SHA256 != fmt.Sprintf("%x", sha256.Sum256([]byte(redactedPayload)))) {
		return errorResponse(http.StatusConflict, "stored verification relic does not match report")
	}

	relicReference := " (Relic: " + relicName + ")"
	msgText := fmt.Sprintf("Shadewright Verifier report (%s)", statusText)
	if summary := strings.TrimSpace(req.Summary); summary != "" {
		msgText += ": " + summary
	}
	msgText = redact.String(msgText) + relicReference

	var msg domain.ConclaveMessage
	msgs, err := h.store.ListConclaveMessages(ctx, conclaveID, 0)
	if err != nil {
		return internalErrorResponse(err)
	}
	for _, candidate := range msgs {
		if candidate.FromRole == ShadewrightRole && candidate.ToRole == "operator" &&
			candidate.Kind == ShadewrightReportKind && candidate.TaskID == "" &&
			strings.HasSuffix(candidate.Body, relicReference) {
			msg = candidate
			break
		}
	}

	var audited bool
	if msg.ID != 0 {
		if !hasRelic {
			return errorResponse(http.StatusServiceUnavailable, "verification relic is missing")
		}
		audited, err = verificationMessageAudited(ctx, h.store, conclaveID, msg.ID)
		if err != nil {
			return internalErrorResponse(err)
		}
	} else {
		var newRelic *domain.ConclaveRelic
		if !hasRelic {
			newRelic = &domain.ConclaveRelic{Name: relicName, ContentType: "application/json", Body: string(payload)}
		}
		broker := conclave.NewBroker(conclave.Options{Store: h.store, GetBundle: h.getBundle, Now: h.now})
		var writtenRelic domain.ConclaveRelic
		msg, writtenRelic, err = broker.PostIntegrationReport(ctx, sess, ShadewrightRole, msgText, newRelic)
		if err != nil {
			return conclaveWriteError(err)
		}
		if newRelic != nil {
			relic = writtenRelic
		}
	}
	if !audited {
		redactedStatus := redact.String(statusText)
		detail, err := marshalDetailJSON(map[string]any{
			"conclave_id": conclaveID,
			"status":      redactedStatus,
			"relic":       relic.Name,
			"message_id":  msg.ID,
		})
		if err != nil {
			return internalErrorResponse(err)
		}
		if err := h.store.AppendAudit(ctx, domain.AuditRecord{
			Category:   "integration",
			Action:     "shadewright.verification_posted",
			Outcome:    redactedStatus,
			DetailJSON: detail,
			CreatedAt:  h.now(),
		}); err != nil {
			return internalErrorResponse(err)
		}
	}

	return integrationResponse{http.StatusCreated, map[string]any{
		"status":  "recorded",
		"relic":   relic,
		"message": msg,
	}}
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
	if got == "" || strings.HasPrefix(got, "op_") || strings.HasPrefix(got, "sc_task_") || subtle.ConstantTimeCompare([]byte(got), []byte(want)) != 1 {
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
	dec.UseNumber()
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

func marshalDetailJSON(v map[string]any) (string, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

type integrationResponse struct {
	code int
	body any
}

func errorResponse(code int, detail string) integrationResponse {
	return integrationResponse{code, map[string]string{"error": detail}}
}

func internalErrorResponse(err error) integrationResponse {
	return errorResponse(http.StatusInternalServerError, redact.String(err.Error()))
}

func conclaveWriteError(err error) integrationResponse {
	switch {
	case errors.Is(err, store.ErrNotFound):
		return errorResponse(http.StatusNotFound, "conclave not found")
	case errors.Is(err, conclave.ErrConclavePaused), errors.Is(err, conclave.ErrConclaveClosed), errors.Is(err, conclave.ErrWardTripped), errors.Is(err, store.ErrConclaveRunLimit):
		return errorResponse(http.StatusConflict, err.Error())
	default:
		return internalErrorResponse(err)
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

func shadewrightIncidentTitle(id string) string {
	return "Shadewright Incident " + id
}

// The title is set in the same insert as the room ID and has no update path.
// Operator rooms have broker-generated IDs outside the integration namespace.
func isShadewrightOwned(sess domain.ConclaveSession) bool {
	return isShadewrightConclaveID(sess.ID) && sess.Title == shadewrightIncidentTitle(sess.ID)
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
