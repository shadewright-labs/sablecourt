package ui

import (
	"embed"
	"html/template"
	"net/http"
	"time"

	"github.com/shadewright-labs/sablecourt/internal/domain"
	"github.com/shadewright-labs/sablecourt/internal/store"
)

//go:embed templates/*.html
var templateFS embed.FS

type Dashboard struct {
	store store.Store
	tmpl  *template.Template
}

type viewData struct {
	ObservedAt string
	Workloads  []domain.WorkloadRecord
	Approvals  []domain.Approval
	Tasks      []domain.Task
	Audit      []domain.AuditRecord
}

func NewDashboard(st store.Store) (*Dashboard, error) {
	tmpl, err := template.ParseFS(templateFS, "templates/index.html")
	if err != nil {
		return nil, err
	}
	return &Dashboard{store: st, tmpl: tmpl}, nil
}

func (d *Dashboard) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	workloads, err := d.store.ListWorkloads(ctx)
	if err != nil {
		http.Error(w, "load workloads: "+err.Error(), http.StatusInternalServerError)
		return
	}
	approvals, err := d.store.ListApprovals(ctx, domain.ApprovalPending)
	if err != nil {
		http.Error(w, "load approvals: "+err.Error(), http.StatusInternalServerError)
		return
	}
	tasks, err := d.store.ListTasks(ctx)
	if err != nil {
		http.Error(w, "load tasks: "+err.Error(), http.StatusInternalServerError)
		return
	}
	audit, err := d.store.ListAudit(ctx, 25)
	if err != nil {
		http.Error(w, "load audit: "+err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = d.tmpl.Execute(w, viewData{
		ObservedAt: time.Now().UTC().Format(time.RFC3339),
		Workloads:  workloads,
		Approvals:  approvals,
		Tasks:      tasks,
		Audit:      audit,
	})
}
