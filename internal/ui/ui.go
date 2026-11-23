package ui

import (
	"embed"
	"fmt"
	"html/template"
	"io/fs"
	"net/http"
	"net/url"
	"time"

	"github.com/shadewright-labs/sablecourt/internal/approval"
	"github.com/shadewright-labs/sablecourt/internal/controlplane"
	"github.com/shadewright-labs/sablecourt/internal/domain"
	"github.com/shadewright-labs/sablecourt/internal/store"
)

//go:embed templates/*.html assets/*
var embeddedFS embed.FS

type Options struct {
	Store           store.Store
	Approvals       *approval.Queue
	Reconciler      *controlplane.Reconciler
	StalenessWindow time.Duration
}

type Dashboard struct {
	store           store.Store
	approvals       *approval.Queue
	reconciler      *controlplane.Reconciler
	stalenessWindow time.Duration
	tmpl            *template.Template
	taskTmpl        *template.Template
	assets          http.Handler
	faviconICO      []byte
	appleTouchIcon  []byte
}

type taskRow struct {
	Task  domain.Task
	Stale bool
}

type viewData struct {
	ObservedAt   string
	Notice       string
	ErrMsg       string
	ActiveTasks  int
	TotalCostUSD string
	TotalTokens  int64
	Workloads    []domain.WorkloadRecord
	Approvals    []domain.Approval
	Tasks        []taskRow
	Audit        []domain.AuditRecord
}

func NewDashboard(opts Options) (*Dashboard, error) {
	tmpl, err := template.ParseFS(embeddedFS, "templates/index.html")
	if err != nil {
		return nil, err
	}
	taskTmpl, err := template.ParseFS(embeddedFS, "templates/task.html")
	if err != nil {
		return nil, err
	}
	subAssets, err := fs.Sub(embeddedFS, "assets")
	if err != nil {
		return nil, err
	}
	favICO, err := embeddedFS.ReadFile("assets/favicon.ico")
	if err != nil {
		return nil, err
	}
	appleIcon, err := embeddedFS.ReadFile("assets/apple-touch-icon.png")
	if err != nil {
		return nil, err
	}
	win := opts.StalenessWindow
	if win <= 0 {
		win = 2 * time.Minute
	}
	return &Dashboard{
		store:           opts.Store,
		approvals:       opts.Approvals,
		reconciler:      opts.Reconciler,
		stalenessWindow: win,
		tmpl:            tmpl,
		taskTmpl:        taskTmpl,
		assets:          http.StripPrefix("/assets/", http.FileServer(http.FS(subAssets))),
		faviconICO:      favICO,
		appleTouchIcon:  appleIcon,
	}, nil
}

func (d *Dashboard) RegisterRoutes(mux *http.ServeMux) {
	mux.Handle("GET /{$}", d)
	mux.HandleFunc("GET /tasks/{id}", d.handleTaskDetail)
	mux.Handle("GET /assets/", d.assets)
	mux.HandleFunc("GET /favicon.ico", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/x-icon")
		w.Header().Set("Cache-Control", "public, max-age=3600")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(d.faviconICO)
	})
	mux.HandleFunc("GET /apple-touch-icon.png", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		w.Header().Set("Cache-Control", "public, max-age=3600")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(d.appleTouchIcon)
	})
	mux.HandleFunc("POST /ui/tasks", d.handleSubmitTask)
	mux.HandleFunc("POST /ui/tasks/{id}/cancel", d.handleCancelTask)
	mux.HandleFunc("POST /ui/approvals/{id}/approve", d.handleApprove)
	mux.HandleFunc("POST /ui/approvals/{id}/deny", d.handleDeny)
}

func (d *Dashboard) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	now := time.Now().UTC()

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
	totals, err := d.store.TotalBudgetTotals(ctx)
	if err != nil {
		http.Error(w, "load budget totals: "+err.Error(), http.StatusInternalServerError)
		return
	}

	var rows []taskRow
	active := 0
	for _, t := range tasks {
		if t.State == domain.TaskStateSubmitted || t.State == domain.TaskStateWorking || t.State == domain.TaskStateInputRequired {
			active++
		}
		stale := t.State == domain.TaskStateWorking && (t.LastHeartbeatAt == nil || now.Sub(*t.LastHeartbeatAt) > d.stalenessWindow)
		rows = append(rows, taskRow{Task: t, Stale: stale})
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = d.tmpl.Execute(w, viewData{
		ObservedAt:   now.Format(time.RFC3339),
		Notice:       r.URL.Query().Get("notice"),
		ErrMsg:       r.URL.Query().Get("err"),
		ActiveTasks:  active,
		TotalCostUSD: fmt.Sprintf("%.4f", float64(totals.CostMicroUSD)/1_000_000.0),
		TotalTokens:  totals.InputTokens + totals.OutputTokens,
		Workloads:    workloads,
		Approvals:    approvals,
		Tasks:        rows,
		Audit:        audit,
	})
}

func (d *Dashboard) handleTaskDetail(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := r.PathValue("id")
	task, err := d.store.GetTask(ctx, id)
	if err != nil {
		http.Error(w, "task not found", http.StatusNotFound)
		return
	}
	events, _ := d.store.ListTaskEvents(ctx, id)
	budget, _ := d.store.TaskBudgetTotals(ctx, id)
	cancelable := task.State == domain.TaskStateSubmitted || task.State == domain.TaskStateWorking || task.State == domain.TaskStateInputRequired

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = d.taskTmpl.Execute(w, map[string]any{
		"Task":       task,
		"Events":     events,
		"Budget":     budget,
		"CostUSD":    fmt.Sprintf("%.4f", float64(budget.CostMicroUSD)/1_000_000.0),
		"Cancelable": cancelable,
	})
}

func (d *Dashboard) handleCancelTask(w http.ResponseWriter, r *http.Request) {
	if d.reconciler == nil {
		redirectErr(w, r, "reconciler not configured")
		return
	}
	id := r.PathValue("id")
	if err := d.reconciler.CancelTask(r.Context(), id, "cancelled from Web UI"); err != nil {
		redirectErr(w, r, "Cancel failed: "+err.Error())
		return
	}
	redirectNotice(w, r, "Cancelled task "+id)
}

func (d *Dashboard) handleSubmitTask(w http.ResponseWriter, r *http.Request) {
	if d.reconciler == nil {
		redirectErr(w, r, "reconciler not configured")
		return
	}
	if err := r.ParseForm(); err != nil {
		redirectErr(w, r, "invalid form submission")
		return
	}
	wl := r.FormValue("workload")
	inputJSON := r.FormValue("input_json")
	if inputJSON == "" {
		inputJSON = "{}"
	}
	task, err := d.reconciler.SubmitTask(r.Context(), wl, "", inputJSON)
	if err != nil {
		redirectErr(w, r, err.Error())
		return
	}
	redirectNotice(w, r, fmt.Sprintf("Submitted task %s for %s", task.ID, wl))
}

func (d *Dashboard) handleApprove(w http.ResponseWriter, r *http.Request) {
	if d.approvals == nil {
		redirectErr(w, r, "approval queue unavailable")
		return
	}
	_ = r.ParseForm()
	id := r.PathValue("id")
	expected := r.FormValue("expected_sha256")
	if _, err := d.approvals.Approve(r.Context(), id, expected, "operator-ui"); err != nil {
		redirectErr(w, r, "Approval failed: "+err.Error())
		return
	}
	redirectNotice(w, r, "Approved "+id+" via out-of-band confirmation")
}

func (d *Dashboard) handleDeny(w http.ResponseWriter, r *http.Request) {
	if d.approvals == nil {
		redirectErr(w, r, "approval queue unavailable")
		return
	}
	id := r.PathValue("id")
	if _, err := d.approvals.Deny(r.Context(), id, "operator-ui", "denied from Web UI"); err != nil {
		redirectErr(w, r, "Deny failed: "+err.Error())
		return
	}
	redirectNotice(w, r, "Denied "+id)
}

func redirectNotice(w http.ResponseWriter, r *http.Request, msg string) {
	http.Redirect(w, r, "/?notice="+url.QueryEscape(msg), http.StatusSeeOther)
}

func redirectErr(w http.ResponseWriter, r *http.Request, msg string) {
	http.Redirect(w, r, "/?err="+url.QueryEscape(msg), http.StatusSeeOther)
}
