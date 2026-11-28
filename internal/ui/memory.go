package ui

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/shadewright-labs/sablecourt/internal/contextpack"
	"github.com/shadewright-labs/sablecourt/internal/controlplane"
	"github.com/shadewright-labs/sablecourt/internal/domain"
	"github.com/shadewright-labs/sablecourt/internal/redact"
	"github.com/shadewright-labs/sablecourt/internal/store"
)

type memoryViewData struct {
	Scope   string
	Query   string
	Notice  string
	ErrMsg  string
	Entries []domain.MemoryEntry
}

func (d *Dashboard) handleMemoryPage(w http.ResponseWriter, r *http.Request) {
	scope := strings.TrimSpace(r.URL.Query().Get("scope"))
	query := strings.TrimSpace(r.URL.Query().Get("q"))
	notice := r.URL.Query().Get("notice")
	errMsg := r.URL.Query().Get("err")

	entries, err := store.SearchVisibleMemory(r.Context(), d.store, scope, query, 100, store.IsLearningNamespace)
	if err != nil && errMsg == "" {
		errMsg = err.Error()
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = d.memoryTmpl.Execute(w, memoryViewData{
		Scope:   scope,
		Query:   query,
		Notice:  notice,
		ErrMsg:  errMsg,
		Entries: entries,
	})
}

func (d *Dashboard) handleSetMemory(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Redirect(w, r, "/memory?err="+url.QueryEscape(err.Error()), http.StatusSeeOther)
		return
	}
	scope := strings.TrimSpace(r.FormValue("scope"))
	if scope == "" {
		scope = "global"
	}
	if store.IsLearningNamespace(scope) {
		http.Error(w, "learning namespace is reserved", http.StatusForbidden)
		return
	}
	key := strings.TrimSpace(r.FormValue("key"))
	value := strings.TrimSpace(r.FormValue("value"))
	if key == "" || value == "" {
		http.Redirect(w, r, "/memory?err="+url.QueryEscape("key and value are required"), http.StatusSeeOther)
		return
	}
	cleanVal := redact.Text(value).Text
	now := time.Now().UTC()
	if err := d.store.PutMemory(r.Context(), domain.MemoryEntry{
		Namespace: scope,
		Key:       key,
		Value:     cleanVal,
		Author:    "operator",
		UpdatedAt: now,
	}); err != nil {
		http.Redirect(w, r, "/memory?err="+url.QueryEscape(err.Error()), http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/memory?notice="+url.QueryEscape(fmt.Sprintf("Saved memory %s/%s", scope, key)), http.StatusSeeOther)
}

func (d *Dashboard) handleDeleteMemory(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Redirect(w, r, "/memory?err="+url.QueryEscape(err.Error()), http.StatusSeeOther)
		return
	}
	scope := strings.TrimSpace(r.FormValue("scope"))
	if store.IsLearningNamespace(scope) {
		http.Error(w, "learning namespace is reserved", http.StatusForbidden)
		return
	}
	key := strings.TrimSpace(r.FormValue("key"))
	if scope == "" || key == "" {
		http.Redirect(w, r, "/memory?err="+url.QueryEscape("scope and key are required"), http.StatusSeeOther)
		return
	}
	if err := d.store.DeleteMemory(r.Context(), scope, key); err != nil {
		http.Redirect(w, r, "/memory?err="+url.QueryEscape(err.Error()), http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/memory?notice="+url.QueryEscape(fmt.Sprintf("Deleted memory %s/%s", scope, key)), http.StatusSeeOther)
}

func (d *Dashboard) handleRetryTask(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	_ = r.ParseForm()
	withHandoff := r.FormValue("handoff") == "1" || r.URL.Query().Get("handoff") == "1"
	// Same task.retry row as POST /api/v1/tasks/{id}/retry in cmd/sablecourtd,
	// written before the retried task exists and not on an idempotent replay.
	retried, err := d.reconciler.RetryTaskWithHandoff(controlplane.WithRetryAudit(r.Context()), id, withHandoff, "operator_ui_retry")
	if err != nil {
		http.Redirect(w, r, "/tasks/"+url.PathEscape(id)+"?err="+url.QueryEscape(errText(r.Context(), err, "retry", "")), http.StatusSeeOther)
		return
	}
	msg := "Retried from " + id
	if withHandoff {
		msg = "Closed-loop remediation started from " + id + " (with failure-handoff.md)"
	}
	http.Redirect(w, r, "/tasks/"+url.PathEscape(retried.ID)+"?notice="+url.QueryEscape(msg), http.StatusSeeOther)
}

const proposedLearningPrefix = "proposed:"

type proposedLearningSource struct {
	Kind        string `json:"kind"`
	ConclaveID  string `json:"conclave_id"`
	MessageID   int64  `json:"message_id,omitempty"`
	FromRole    string `json:"from_role,omitempty"`
	ToRole      string `json:"to_role,omitempty"`
	MessageKind string `json:"message_kind,omitempty"`
	MessageBody string `json:"message_body"`
}

type proposedLearningRecord struct {
	Status          string                 `json:"status"`
	TargetNamespace string                 `json:"target_namespace"`
	Rule            string                 `json:"rule"`
	Source          proposedLearningSource `json:"source"`
}

func parseConclaveMessageID(rawID, key string) int64 {
	if s := strings.TrimSpace(rawID); s != "" {
		if id, err := strconv.ParseInt(s, 10, 64); err == nil && id > 0 {
			return id
		}
	}
	if strings.HasPrefix(key, "conclave-msg-") {
		if id, err := strconv.ParseInt(strings.TrimPrefix(key, "conclave-msg-"), 10, 64); err == nil && id > 0 {
			return id
		}
	}
	return 0
}

func (d *Dashboard) handleConclaveSaveLearning(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := r.ParseForm(); err != nil {
		redirectConclaveErr(w, r, id, err.Error())
		return
	}
	scope := strings.TrimSpace(r.FormValue("scope"))
	scope = strings.TrimPrefix(scope, proposedLearningPrefix)
	targetNS := contextpack.NormalizeLearningNamespace(scope)
	proposalNS := proposedLearningPrefix + targetNS

	now := time.Now().UTC()
	key := strings.TrimSpace(r.FormValue("key"))
	msgID := parseConclaveMessageID(r.FormValue("message_id"), key)
	if key == "" {
		key = fmt.Sprintf("conclave-%s-%d", id, now.Unix())
	}
	rule := strings.TrimSpace(r.FormValue("rule"))
	if rule == "" {
		redirectConclaveErr(w, r, id, "learning rule is required")
		return
	}
	cleanRule := redact.String(rule)

	if msgID == 0 || d.store == nil {
		redirectConclaveErr(w, r, id, "source conclave message is unavailable")
		return
	}
	msgs, err := d.store.ListConclaveMessages(r.Context(), id, msgID-1)
	if err != nil {
		redirectConclaveErr(w, r, id, "source conclave message is unavailable")
		return
	}
	var src proposedLearningSource
	for _, m := range msgs {
		if m.ID == msgID && m.ConclaveID == id {
			src = proposedLearningSource{
				Kind:        "conclave_message",
				ConclaveID:  m.ConclaveID,
				MessageID:   m.ID,
				FromRole:    m.FromRole,
				ToRole:      m.ToRole,
				MessageKind: m.Kind,
				MessageBody: redact.String(strings.TrimSpace(m.Body)),
			}
			break
		}
	}
	if src.MessageID == 0 {
		redirectConclaveErr(w, r, id, "source conclave message is unavailable")
		return
	}

	proposal := proposedLearningRecord{
		Status:          "proposed",
		TargetNamespace: targetNS,
		Rule:            cleanRule,
		Source:          src,
	}
	payload, err := json.MarshalIndent(proposal, "", "  ")
	if err != nil {
		redirectConclaveErr(w, r, id, err.Error())
		return
	}

	entry := domain.MemoryEntry{
		Namespace: proposalNS,
		Key:       key,
		Value:     string(payload),
		Author:    "operator",
		UpdatedAt: now,
	}
	detailBytes, _ := json.Marshal(map[string]any{
		"namespace":        entry.Namespace,
		"target_namespace": targetNS,
		"key":              entry.Key,
		"conclave_id":      id,
		"message_id":       msgID,
	})
	// Recorded before it is stored: a record that cannot be written refuses
	// the proposal.
	if err := store.RecordAudit(r.Context(), d.store, domain.AuditRecord{
		Category:     "memory",
		WorkloadName: "operator",
		Action:       "learning.propose",
		Outcome:      "proposed",
		DetailJSON:   string(detailBytes),
		CreatedAt:    now,
	}); err != nil {
		redirectConclaveErr(w, r, id, errText(r.Context(), err, "proposal", ""))
		return
	}
	if err := d.store.PutMemory(r.Context(), entry); err != nil {
		store.LogAudit(r.Context(), d.store, domain.AuditRecord{
			Category:     "memory",
			WorkloadName: "operator",
			Action:       "learning.propose",
			Outcome:      "error",
			DetailJSON:   string(detailBytes),
			CreatedAt:    time.Now().UTC(),
		})
		redirectConclaveErr(w, r, id, err.Error())
		return
	}
	http.Redirect(w, r, "/conclaves/"+url.PathEscape(id)+"?notice="+url.QueryEscape("Proposed learning saved (pending review): "+entry.Namespace+"/"+entry.Key), http.StatusSeeOther)
}

func (d *Dashboard) handleProviderSaveTiers(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Redirect(w, r, "/providers?err="+url.QueryEscape(err.Error()), http.StatusSeeOther)
		return
	}
	var routes []contextpack.TierRoute
	for _, t := range []string{"fast", "coding", "critic"} {
		routes = append(routes, contextpack.TierRoute{
			Tier:     t,
			Provider: strings.TrimSpace(r.FormValue("tier_" + t + "_provider")),
			Model:    strings.TrimSpace(r.FormValue("tier_" + t + "_model")),
			Family:   strings.TrimSpace(r.FormValue("tier_" + t + "_family")),
		})
	}
	if err := contextpack.SaveModelTiers(r.Context(), d.store, routes); err != nil {
		http.Redirect(w, r, "/providers?err="+url.QueryEscape(err.Error()), http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/providers?notice="+url.QueryEscape("Saved role-based model tiers (fast / coding / critic)"), http.StatusSeeOther)
}

const (
	maxRelicUploadBytes = 1 << 20
	maxRelicFormBytes   = maxRelicUploadBytes + (256 << 10)
)

func (d *Dashboard) handleConclaveUploadRelic(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if d.conclave == nil {
		redirectConclaveErr(w, r, id, "conclave disabled")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxRelicFormBytes)

	name, contentType, content, err := parseRelicUploadForm(r)
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			redirectConclaveErr(w, r, id, "relic exceeds maximum size (1 MiB)")
			return
		}
		redirectConclaveErr(w, r, id, err.Error())
		return
	}

	if _, err := d.conclave.PutRelic(r.Context(), id, "operator", name, contentType, content); err != nil {
		redirectConclaveErr(w, r, id, errText(r.Context(), err, "storing the relic", ""))
		return
	}
	http.Redirect(w, r, "/conclaves/"+url.PathEscape(id)+"?notice="+url.QueryEscape("Relic "+name+" added"), http.StatusSeeOther)
}

func parseRelicUploadForm(r *http.Request) (string, string, string, error) {
	mediaType := ""
	if rawCT := strings.TrimSpace(r.Header.Get("Content-Type")); rawCT != "" {
		mt, _, err := mime.ParseMediaType(rawCT)
		if err != nil {
			return "", "", "", fmt.Errorf("invalid form content type")
		}
		mediaType = mt
	}

	var name, contentType, content string
	switch mediaType {
	case "multipart/form-data":
		if err := r.ParseMultipartForm(maxRelicFormBytes); err != nil { // #nosec G120 -- handleConclaveUploadRelic caps the entire body with http.MaxBytesReader before calling this parser.
			return "", "", "", err
		}
		if r.MultipartForm != nil {
			defer func() { _ = r.MultipartForm.RemoveAll() }()
		}
		name = strings.TrimSpace(r.FormValue("name"))
		contentType = strings.TrimSpace(r.FormValue("content_type"))
		content = r.FormValue("content")

		file, hdr, err := r.FormFile("file")
		if err == nil {
			defer func() { _ = file.Close() }()
			if hdr.Filename != "" {
				if strings.TrimSpace(content) != "" {
					return "", "", "", fmt.Errorf("provide either relic content or a file, not both")
				}
				raw, readErr := io.ReadAll(io.LimitReader(file, maxRelicUploadBytes+1))
				if readErr != nil {
					return "", "", "", readErr
				}
				if len(raw) > maxRelicUploadBytes {
					return "", "", "", fmt.Errorf("relic exceeds maximum size (1 MiB)")
				}
				if len(bytes.TrimSpace(raw)) == 0 {
					return "", "", "", fmt.Errorf("uploaded relic file is empty")
				}
				content = string(raw)
				if name == "" {
					cleanFN := strings.ReplaceAll(hdr.Filename, "\\", "/")
					name = strings.TrimSpace(path.Base(cleanFN))
				}
			}
		} else if !errors.Is(err, http.ErrMissingFile) {
			return "", "", "", err
		}
	case "", "application/x-www-form-urlencoded":
		if err := r.ParseForm(); err != nil {
			return "", "", "", err
		}
		name = strings.TrimSpace(r.FormValue("name"))
		contentType = strings.TrimSpace(r.FormValue("content_type"))
		content = r.FormValue("content")
	default:
		return "", "", "", fmt.Errorf("unsupported form content type")
	}

	if name == "" || strings.TrimSpace(content) == "" {
		return "", "", "", fmt.Errorf("relic name and content are required")
	}
	if !validRelicName(name) || redact.ContainsSensitive(name) {
		return "", "", "", fmt.Errorf("invalid relic name")
	}
	if len(content) > maxRelicUploadBytes {
		return "", "", "", fmt.Errorf("relic exceeds maximum size (1 MiB)")
	}
	if !utf8.ValidString(content) || strings.ContainsRune(content, 0) {
		return "", "", "", fmt.Errorf("relic content must be valid UTF-8 text")
	}
	normalizedCT, err := normalizeRelicContentType(contentType)
	if err != nil {
		return "", "", "", err
	}
	return name, normalizedCT, content, nil
}

func validRelicName(name string) bool {
	if name == "" || len(name) > 200 || name == "." || name == ".." {
		return false
	}
	if strings.Contains(name, "..") || strings.ContainsAny(name, `/\?#%`) {
		return false
	}
	return !hasControlChars(name)
}

func normalizeRelicContentType(raw string) (string, error) {
	if raw == "" {
		return "text/plain", nil
	}
	if hasControlChars(raw) {
		return "", fmt.Errorf("unsupported relic content type")
	}
	mt, _, err := mime.ParseMediaType(raw)
	if err != nil {
		return "", fmt.Errorf("unsupported relic content type")
	}
	if strings.HasPrefix(mt, "text/") || mt == "application/json" || mt == "application/yaml" || mt == "application/x-yaml" || mt == "application/xml" {
		return mt, nil
	}
	return "", fmt.Errorf("unsupported relic content type")
}
