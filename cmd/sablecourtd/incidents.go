package main

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/shadewright-labs/sablecourt/internal/conclave"
	"github.com/shadewright-labs/sablecourt/internal/config"
	"github.com/shadewright-labs/sablecourt/internal/controlplane"
	"github.com/shadewright-labs/sablecourt/internal/domain"
	"github.com/shadewright-labs/sablecourt/internal/redact"
	"github.com/shadewright-labs/sablecourt/internal/store"
)

var (
	incidentIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,127}$`)
	incidentIntakeMu  sync.Mutex
)

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

func incidentHandler(st store.Store, getBundle func() *config.Bundle, broker *conclave.Broker, reconciler *controlplane.Reconciler, defaultTemplate string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req incidentRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		if !incidentIDPattern.MatchString(req.IncidentID) {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "incidentId must match [A-Za-z0-9][A-Za-z0-9_-]{0,127}"})
			return
		}
		if strings.TrimSpace(req.Summary) == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "summary is required"})
			return
		}

		// Serialize initialization within the daemon. The durable primary key also
		// prevents another daemon from creating a second council for this incident.
		incidentIntakeMu.Lock()
		defer incidentIntakeMu.Unlock()
		sum := sha256.Sum256([]byte("shadewright\x00" + req.IncidentID))
		id := fmt.Sprintf("ccl-shadewright-%x", sum)
		relicName := "incident-" + req.IncidentID + ".json"
		respondExisting := func(sess domain.ConclaveSession) {
			// The Relic marks completed initialization. Its absence cannot count as
			// success. Store has no transaction spanning council and broker writes.
			relic, err := st.GetConclaveRelic(r.Context(), sess.ID, relicName)
			if err != nil {
				writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "incident intake is incomplete or unavailable"})
				return
			}
			writeJSON(w, http.StatusOK, map[string]any{"conclave": sess, "relic": relic})
		}
		sess, err := st.GetConclave(r.Context(), id)
		if err == nil {
			respondExisting(sess)
			return
		}
		if !errors.Is(err, store.ErrNotFound) {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}

		tplName := strings.TrimSpace(req.ConclaveTemplate)
		if tplName == "" {
			tplName = strings.TrimSpace(defaultTemplate)
		}
		var bundle *config.Bundle
		if getBundle != nil {
			bundle = getBundle()
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
		now := time.Now().UTC()
		sess = domain.ConclaveSession{
			ID: id, TemplateName: tplName,
			Title: redact.String(fmt.Sprintf("Shadewright Incident %s (%s/%s)", req.IncidentID, req.Namespace, req.Workload)),
			State: domain.ConclaveStateActive, CreatedAt: now, UpdatedAt: now,
		}
		if err := st.CreateConclave(r.Context(), sess); err != nil {
			if existing, lookupErr := st.GetConclave(r.Context(), id); lookupErr == nil {
				respondExisting(existing)
				return
			}
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		fail := func(err error) {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		}
		if err := st.AppendAudit(r.Context(), domain.AuditRecord{
			Category: "conclave", Action: "conclave.open", Outcome: "active",
			DetailJSON: fmt.Sprintf(`{"conclave_id":%q,"template":%q,"vassals":%d}`, id, tplName, len(tpl.Spec.Vassals)), CreatedAt: now,
		}); err != nil {
			fail(err)
			return
		}
		decree := fmt.Sprintf("Escalated from Shadewright (%s): %s. Review Relic %s and propose a reviewed fix PR.", req.Cluster, req.Summary, relicName)
		// Broker.Open generates random IDs, so initialize the deterministic session
		// here and keep message routing, activation, and redaction in the broker.
		if reconciler != nil {
			for _, v := range tpl.Spec.Vassals {
				if v.Activation != domain.ActivationAlwaysOn {
					continue
				}
				input, _ := json.Marshal(map[string]any{
					"conclaveId": id, "role": v.Role, "title": sess.Title,
					"prompt": redact.String(decree), "activation": string(v.Activation),
				})
				if _, err := reconciler.SubmitTask(r.Context(), v.Workload, fmt.Sprintf("conclave-%s-init-%s", id, v.Role), string(input)); err != nil {
					fail(err)
					return
				}
			}
		}
		if _, err := broker.PostMessage(r.Context(), id, "operator", "all", "decree", decree, ""); err != nil {
			fail(err)
			return
		}
		evidence, _ := json.MarshalIndent(map[string]any{
			"incidentId": req.IncidentID, "cluster": req.Cluster, "namespace": req.Namespace,
			"workload": req.Workload, "summary": req.Summary, "evidence": req.Evidence, "logs": req.Logs,
		}, "", "  ")
		relic, err := broker.PutRelic(r.Context(), id, "shadewright-analyzer", relicName, "application/json", string(evidence))
		if err != nil {
			fail(err)
			return
		}
		if err := st.AppendAudit(r.Context(), domain.AuditRecord{
			Category: "integration", Action: "shadewright.incident_escalated", Outcome: "ok",
			DetailJSON: fmt.Sprintf(`{"incident_id":%q,"conclave_id":%q,"relic":%q}`, req.IncidentID, id, relicName), CreatedAt: now,
		}); err != nil {
			fail(err)
			return
		}

		writeJSON(w, http.StatusCreated, map[string]any{"conclave": sess, "relic": relic})
	})
}
