package main

import (
	"context"
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
		ctx := r.Context()
		fail := func(err error) {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		}
		sess, err := st.GetConclave(ctx, id)
		exists := err == nil
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			fail(err)
			return
		}
		var relic domain.ConclaveRelic
		var hasRelic, opened bool
		tplName := strings.TrimSpace(req.ConclaveTemplate)
		if tplName == "" {
			tplName = strings.TrimSpace(defaultTemplate)
		}
		if exists {
			tplName = sess.TemplateName
			relic, err = st.GetConclaveRelic(ctx, id, relicName)
			hasRelic = err == nil
			if err != nil && !errors.Is(err, store.ErrNotFound) {
				fail(err)
				return
			}
			var finished bool
			opened, finished, err = incidentAuditProgress(ctx, st, id)
			if err != nil {
				fail(err)
				return
			}
			if finished {
				if !hasRelic {
					writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "incident Relic is missing"})
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
		if !exists {
			sess = domain.ConclaveSession{
				ID: id, TemplateName: tplName, Title: redact.String(fmt.Sprintf("Shadewright Incident %s (%s/%s)", req.IncidentID, req.Namespace, req.Workload)),
				State: domain.ConclaveStateActive, CreatedAt: now, UpdatedAt: now,
			}
			if err := st.CreateConclave(ctx, sess); err != nil {
				fail(err)
				return
			}
		}
		if !opened {
			if err := st.AppendAudit(ctx, domain.AuditRecord{
				Category: "conclave", Action: "conclave.open", Outcome: "active",
				DetailJSON: fmt.Sprintf(`{"conclave_id":%q,"template":%q,"vassals":%d}`, id, tplName, len(tpl.Spec.Vassals)), CreatedAt: now,
			}); err != nil {
				fail(err)
				return
			}
		}
		evidence, _ := json.MarshalIndent(map[string]any{
			"incidentId": req.IncidentID, "cluster": req.Cluster, "namespace": req.Namespace,
			"workload": req.Workload, "summary": req.Summary, "evidence": req.Evidence, "logs": req.Logs,
		}, "", "  ")
		if !hasRelic {
			relic, err = broker.PutRelic(ctx, id, "shadewright-analyzer", relicName, "application/json", string(evidence))
			if err != nil {
				fail(err)
				return
			}
		} else if relic.Body != redact.String(string(evidence)) {
			writeJSON(w, http.StatusConflict, map[string]string{"error": "retry evidence differs from stored incident"})
			return
		}
		decree := redact.String(fmt.Sprintf("Escalated from Shadewright (%s): %s. Review Relic %s and propose a reviewed fix PR.", req.Cluster, req.Summary, relicName))
		// Broker.Open generates random IDs, so initialize the deterministic session
		// here and keep message routing, activation, and redaction in the broker.
		if reconciler != nil {
			for _, v := range tpl.Spec.Vassals {
				if v.Activation != domain.ActivationAlwaysOn {
					continue
				}
				input, _ := json.Marshal(map[string]any{
					"conclaveId": id, "role": v.Role, "title": sess.Title,
					"prompt": decree, "activation": string(v.Activation),
				})
				if _, err := reconciler.SubmitTask(ctx, v.Workload, fmt.Sprintf("conclave-%s-init-%s", id, v.Role), string(input)); err != nil {
					fail(err)
					return
				}
			}
		}
		// A crash can leave the opening decree durable before all mentions wake.
		msgs, err := st.ListConclaveMessages(ctx, id, 0)
		if err != nil {
			fail(err)
			return
		}
		var opening *domain.ConclaveMessage
		for i := range msgs {
			msg := &msgs[i]
			if msg.FromRole == "operator" && msg.ToRole == "all" && msg.Kind == "decree" && msg.TaskID == "" && msg.Body == decree {
				opening = msg
				break
			}
		}
		if opening == nil {
			_, err = broker.PostMessage(ctx, id, "operator", "all", "decree", decree, "")
		} else {
			err = broker.WakeMentions(ctx, tpl, *opening)
		}
		if err != nil {
			fail(err)
			return
		}
		if err := st.AppendAudit(ctx, domain.AuditRecord{
			Category: "integration", Action: "shadewright.incident_escalated", Outcome: "ok",
			DetailJSON: fmt.Sprintf(`{"incident_id":%q,"conclave_id":%q,"relic":%q}`, req.IncidentID, id, relicName), CreatedAt: now,
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

// The opening and completion audit records survive daemon restarts. Older
// incidents used the same records. Read back only as far as their opening.
func incidentAuditProgress(ctx context.Context, st store.Store, id string) (opened, finished bool, err error) {
	for limit := 64; ; limit *= 2 {
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
}
