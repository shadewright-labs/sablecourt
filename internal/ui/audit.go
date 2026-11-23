package ui

import (
	"net/http"
	"strings"

	"github.com/shadewright-labs/sablecourt/internal/domain"
)

type auditViewData struct {
	Category     string
	Workload     string
	Outcome      string
	ChainValid   bool
	VerifiedRows int
	ChainError   string
	Records      []domain.AuditRecord
}

func (d *Dashboard) handleAuditPage(w http.ResponseWriter, r *http.Request) {
	cat := strings.TrimSpace(r.URL.Query().Get("category"))
	wl := strings.TrimSpace(r.URL.Query().Get("workload"))
	outcome := strings.TrimSpace(r.URL.Query().Get("outcome"))

	all, _ := d.store.ListAudit(r.Context(), 250)
	var filtered []domain.AuditRecord
	for _, rec := range all {
		if cat != "" && rec.Category != cat {
			continue
		}
		if wl != "" && rec.WorkloadName != wl {
			continue
		}
		if outcome != "" && rec.Outcome != outcome {
			continue
		}
		filtered = append(filtered, rec)
	}

	verified, err := d.store.VerifyAuditChain(r.Context())
	data := auditViewData{
		Category:     cat,
		Workload:     wl,
		Outcome:      outcome,
		ChainValid:   err == nil,
		VerifiedRows: verified,
		Records:      filtered,
	}
	if err != nil {
		data.ChainError = err.Error()
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = d.auditTmpl.Execute(w, data)
}
