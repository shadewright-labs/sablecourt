package ui

import (
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"sigs.k8s.io/yaml"

	"github.com/shadewright-labs/sablecourt/internal/approval"
	"github.com/shadewright-labs/sablecourt/internal/conclave"
	"github.com/shadewright-labs/sablecourt/internal/config"
	"github.com/shadewright-labs/sablecourt/internal/controlplane"
	"github.com/shadewright-labs/sablecourt/internal/domain"
	"github.com/shadewright-labs/sablecourt/internal/providers"
	"github.com/shadewright-labs/sablecourt/internal/store"
)

//go:embed templates/*.html assets/*
var embeddedFS embed.FS

type Options struct {
	Store           store.Store
	Approvals       *approval.Queue
	Reconciler      *controlplane.Reconciler
	Scheduler       *controlplane.Scheduler
	Conclave        *conclave.Broker
	Providers       *providers.Registry
	GetBundle       func() *config.Bundle
	Reload          func() error
	ManifestsPath   string
	StalenessWindow time.Duration
}

type Dashboard struct {
	store           store.Store
	approvals       *approval.Queue
	reconciler      *controlplane.Reconciler
	scheduler       *controlplane.Scheduler
	conclave        *conclave.Broker
	providers       *providers.Registry
	getBundle       func() *config.Bundle
	reload          func() error
	manifestsPath   string
	stalenessWindow time.Duration
	tmpl            *template.Template
	taskTmpl        *template.Template
	providersTmpl   *template.Template
	studioTmpl      *template.Template
	conclavesTmpl   *template.Template
	conclaveTmpl    *template.Template
	schedulesTmpl   *template.Template
	assets          http.Handler
	faviconICO      []byte
	appleTouchIcon  []byte
}

type taskRow struct {
	Task        domain.Task
	Stale       bool
	Cancelable  bool
	Suspendable bool
	Resumable   bool
	LastReply   string
}

type viewData struct {
	ObservedAt       string
	Notice           string
	ErrMsg           string
	SelectedWorkload string
	StateFilter      string
	ActiveTasks      int
	TotalTasks       int
	TotalCostUSD     string
	TotalTokens      int64
	Workloads        []domain.WorkloadRecord
	Approvals        []domain.Approval
	Tasks            []taskRow
	Audit            []domain.AuditRecord
	Providers        []providers.ProviderStatus
	ModelsByProvider template.JS
	DefaultsByProv   template.JS
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
	providersTmpl, err := template.ParseFS(embeddedFS, "templates/providers.html")
	if err != nil {
		return nil, err
	}
	studioTmpl, err := template.ParseFS(embeddedFS, "templates/studio.html")
	if err != nil {
		return nil, err
	}
	conclavesTmpl, err := template.ParseFS(embeddedFS, "templates/conclaves.html")
	if err != nil {
		return nil, err
	}
	conclaveTmpl, err := template.ParseFS(embeddedFS, "templates/conclave.html")
	if err != nil {
		return nil, err
	}
	schedulesTmpl, err := template.ParseFS(embeddedFS, "templates/schedules.html")
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
		scheduler:       opts.Scheduler,
		conclave:        opts.Conclave,
		providers:       opts.Providers,
		getBundle:       opts.GetBundle,
		reload:          opts.Reload,
		manifestsPath:   opts.ManifestsPath,
		stalenessWindow: win,
		tmpl:            tmpl,
		taskTmpl:        taskTmpl,
		providersTmpl:   providersTmpl,
		studioTmpl:      studioTmpl,
		conclavesTmpl:   conclavesTmpl,
		conclaveTmpl:    conclaveTmpl,
		schedulesTmpl:   schedulesTmpl,
		assets:          http.StripPrefix("/assets/", http.FileServer(http.FS(subAssets))),
		faviconICO:      favICO,
		appleTouchIcon:  appleIcon,
	}, nil
}

func (d *Dashboard) RegisterRoutes(mux *http.ServeMux) {
	mux.Handle("GET /{$}", d)
	mux.HandleFunc("GET /tasks/{id}", d.handleTaskDetail)
	mux.HandleFunc("GET /providers", d.handleProvidersPage)
	mux.HandleFunc("GET /studio", d.handleStudioPage)
	mux.HandleFunc("GET /conclaves", d.handleConclavesPage)
	mux.HandleFunc("GET /conclaves/{id}", d.handleConclaveDetailPage)
	mux.HandleFunc("GET /schedules", d.handleSchedulesPage)
	mux.HandleFunc("POST /ui/schedules/{name}/run", d.handleRunSchedule)
	mux.HandleFunc("POST /ui/conclaves", d.handleOpenConclave)
	mux.HandleFunc("POST /ui/conclaves/{id}/decree", d.handlePostDecree)
	mux.HandleFunc("POST /ui/conclaves/{id}/close", d.handleCloseConclave)
	mux.HandleFunc("POST /ui/studio/conclave", d.handleStudioCreateConclave)
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
	mux.HandleFunc("POST /ui/tasks/{id}/suspend", d.handleSuspendTask)
	mux.HandleFunc("POST /ui/tasks/{id}/resume", d.handleResumeTask)
	mux.HandleFunc("POST /ui/tasks/{id}/messages", d.handlePostMessage)
	mux.HandleFunc("POST /ui/approvals/{id}/approve", d.handleApprove)
	mux.HandleFunc("POST /ui/approvals/{id}/deny", d.handleDeny)
	mux.HandleFunc("POST /ui/providers/{id}/connect", d.handleProviderConnect)
	mux.HandleFunc("POST /ui/providers/{id}/disconnect", d.handleProviderDisconnect)
	mux.HandleFunc("POST /ui/providers/{id}/sync", d.handleProviderSync)
	mux.HandleFunc("POST /ui/providers/{id}/models", d.handleProviderSaveModels)
	mux.HandleFunc("POST /ui/studio/agent", d.handleCreateAgent)
	mux.HandleFunc("POST /ui/studio/workflow", d.handleCreateWorkflow)
	mux.HandleFunc("POST /ui/workflows/{name}/run", d.handleRunWorkflow)
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

	stateFilter := r.URL.Query().Get("state")
	if stateFilter == "" {
		stateFilter = "all"
	}

	var rows []taskRow
	active := 0
	for _, t := range tasks {
		isActive := t.State == domain.TaskStateSubmitted || t.State == domain.TaskStateWorking || t.State == domain.TaskStateInputRequired
		if isActive {
			active++
		}
		switch stateFilter {
		case "active":
			if !isActive {
				continue
			}
		case "completed":
			if t.State != domain.TaskStateCompleted {
				continue
			}
		case "failed":
			if t.State != domain.TaskStateFailed {
				continue
			}
		}

		var lastReply string
		if msgs, err := d.store.ListTaskMessages(ctx, t.ID); err == nil {
			for i := len(msgs) - 1; i >= 0; i-- {
				if msgs[i].Role == "agent" || msgs[i].Role == "system" {
					lastReply = msgs[i].Content
					if len(lastReply) > 360 {
						lastReply = lastReply[:360] + "..."
					}
					break
				}
			}
		}

		stale := t.State == domain.TaskStateWorking && (t.LastHeartbeatAt == nil || now.Sub(*t.LastHeartbeatAt) > d.stalenessWindow)
		rows = append(rows, taskRow{
			Task:        t,
			Stale:       stale,
			Cancelable:  isActive,
			Suspendable: t.State == domain.TaskStateWorking,
			Resumable:   t.State == domain.TaskStateSuspended,
			LastReply:   lastReply,
		})
	}

	var provList []providers.ProviderStatus
	modelsMap := make(map[string][]string)
	defaultsMap := make(map[string]string)
	if d.providers != nil {
		for _, p := range d.providers.List(ctx) {
			if p.Kind == "cli" {
				continue
			}
			provList = append(provList, p)
			defaultsMap[p.ID] = p.DefaultModel
			for _, m := range p.Models {
				if m.Enabled {
					modelsMap[p.ID] = append(modelsMap[p.ID], m.ID)
				}
			}
		}
	}
	modelsJSON, _ := json.Marshal(modelsMap)
	defaultsJSON, _ := json.Marshal(defaultsMap)

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = d.tmpl.Execute(w, viewData{
		ObservedAt:       now.Format(time.RFC3339),
		Notice:           r.URL.Query().Get("notice"),
		ErrMsg:           r.URL.Query().Get("err"),
		SelectedWorkload: r.URL.Query().Get("workload"),
		StateFilter:      stateFilter,
		ActiveTasks:      active,
		TotalTasks:       len(tasks),
		TotalCostUSD:     fmt.Sprintf("%.4f", float64(totals.CostMicroUSD)/1_000_000.0),
		TotalTokens:      totals.InputTokens + totals.OutputTokens,
		Workloads:        workloads,
		Approvals:        approvals,
		Tasks:            rows,
		Audit:            audit,
		Providers:        provList,
		ModelsByProvider: template.JS(modelsJSON),
		DefaultsByProv:   template.JS(defaultsJSON),
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
	messages, _ := d.store.ListTaskMessages(ctx, id)
	budget, _ := d.store.TaskBudgetTotals(ctx, id)
	cancelable := task.State == domain.TaskStateSubmitted || task.State == domain.TaskStateWorking || task.State == domain.TaskStateInputRequired

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = d.taskTmpl.Execute(w, map[string]any{
		"Task":       task,
		"Events":     events,
		"Messages":   messages,
		"Budget":     budget,
		"CostUSD":    fmt.Sprintf("%.4f", float64(budget.CostMicroUSD)/1_000_000.0),
		"Cancelable": cancelable,
	})
}

func (d *Dashboard) handlePostMessage(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	_ = r.ParseForm()
	content := strings.TrimSpace(r.FormValue("content"))
	if content != "" {
		_ = d.store.AppendTaskMessage(r.Context(), domain.TaskMessage{
			TaskID:    id,
			Role:      "user",
			Content:   content,
			CreatedAt: time.Now().UTC(),
		})
	}
	http.Redirect(w, r, "/tasks/"+id, http.StatusSeeOther)
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

func (d *Dashboard) handleSuspendTask(w http.ResponseWriter, r *http.Request) {
	if d.reconciler == nil {
		redirectErr(w, r, "reconciler not configured")
		return
	}
	id := r.PathValue("id")
	if err := d.reconciler.SuspendTask(r.Context(), id, "suspended from Web UI"); err != nil {
		redirectErr(w, r, "Suspend failed: "+err.Error())
		return
	}
	redirectNotice(w, r, "Suspended task "+id+" and saved checkpoint")
}

func (d *Dashboard) handleResumeTask(w http.ResponseWriter, r *http.Request) {
	if d.reconciler == nil {
		redirectErr(w, r, "reconciler not configured")
		return
	}
	id := r.PathValue("id")
	if err := d.reconciler.ResumeTask(r.Context(), id); err != nil {
		redirectErr(w, r, "Resume failed: "+err.Error())
		return
	}
	redirectNotice(w, r, "Resumed task "+id+" from checkpoint")
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
	provider := strings.TrimSpace(r.FormValue("provider"))
	modelName := strings.TrimSpace(r.FormValue("model"))
	rawInput := strings.TrimSpace(r.FormValue("input_json"))
	payload := make(map[string]any)
	if rawInput != "" {
		if err := json.Unmarshal([]byte(rawInput), &payload); err != nil {
			payload = map[string]any{"prompt": rawInput}
		}
	}
	if provider != "" {
		payload["provider"] = provider
	}
	if modelName != "" {
		payload["model"] = modelName
	}
	inputBytes, _ := json.Marshal(payload)
	task, err := d.reconciler.SubmitTask(r.Context(), wl, "", string(inputBytes))
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

func (d *Dashboard) handleProvidersPage(w http.ResponseWriter, r *http.Request) {
	var list []providers.ProviderStatus
	if d.providers != nil {
		list = d.providers.List(r.Context())
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = d.providersTmpl.Execute(w, map[string]any{
		"Providers": list,
		"Notice":    r.URL.Query().Get("notice"),
		"ErrMsg":    r.URL.Query().Get("err"),
	})
}

func (d *Dashboard) handleProviderConnect(w http.ResponseWriter, r *http.Request) {
	if d.providers == nil {
		http.Redirect(w, r, "/providers?err="+url.QueryEscape("provider registry unavailable"), http.StatusSeeOther)
		return
	}
	_ = r.ParseForm()
	id := r.PathValue("id")
	if err := d.providers.Connect(r.Context(), id, r.FormValue("api_key")); err != nil {
		http.Redirect(w, r, "/providers?err="+url.QueryEscape(err.Error()), http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/providers?notice="+url.QueryEscape("Saved key for "+id+" in SecretStore"), http.StatusSeeOther)
}

func (d *Dashboard) handleProviderDisconnect(w http.ResponseWriter, r *http.Request) {
	if d.providers == nil {
		http.Redirect(w, r, "/providers?err="+url.QueryEscape("provider registry unavailable"), http.StatusSeeOther)
		return
	}
	id := r.PathValue("id")
	if err := d.providers.Disconnect(r.Context(), id); err != nil {
		http.Redirect(w, r, "/providers?err="+url.QueryEscape(err.Error()), http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/providers?notice="+url.QueryEscape("Removed key for "+id), http.StatusSeeOther)
}

func (d *Dashboard) handleProviderSync(w http.ResponseWriter, r *http.Request) {
	if d.providers == nil {
		http.Redirect(w, r, "/providers?err="+url.QueryEscape("provider registry unavailable"), http.StatusSeeOther)
		return
	}
	id := r.PathValue("id")
	models, err := d.providers.SyncModels(r.Context(), id)
	if err != nil {
		http.Redirect(w, r, "/providers?err="+url.QueryEscape("Sync "+id+" failed: "+err.Error()), http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/providers?notice="+url.QueryEscape(fmt.Sprintf("Synced %d models from %s", len(models), id)), http.StatusSeeOther)
}

func (d *Dashboard) handleProviderSaveModels(w http.ResponseWriter, r *http.Request) {
	if d.providers == nil {
		http.Redirect(w, r, "/providers?err="+url.QueryEscape("provider registry unavailable"), http.StatusSeeOther)
		return
	}
	_ = r.ParseForm()
	id := r.PathValue("id")
	defModel := strings.TrimSpace(r.FormValue("default_model"))
	enabled := r.Form["enabled_models"]
	if err := d.providers.SavePrefs(r.Context(), id, defModel, enabled); err != nil {
		http.Redirect(w, r, "/providers?err="+url.QueryEscape(err.Error()), http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/providers?notice="+url.QueryEscape(fmt.Sprintf("Saved model selection for %s (default: %s)", id, defModel)), http.StatusSeeOther)
}

func (d *Dashboard) handleStudioPage(w http.ResponseWriter, r *http.Request) {
	workloads, _ := d.store.ListWorkloads(r.Context())
	var workflows []domain.Workflow
	if d.getBundle != nil {
		if b := d.getBundle(); b != nil {
			for _, wf := range b.Workflows {
				workflows = append(workflows, wf)
			}
		}
	}
	sort.Slice(workflows, func(i, j int) bool { return workflows[i].Metadata.Name < workflows[j].Metadata.Name })

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = d.studioTmpl.Execute(w, map[string]any{
		"Workloads": workloads,
		"Workflows": workflows,
		"Notice":    r.URL.Query().Get("notice"),
		"ErrMsg":    r.URL.Query().Get("err"),
	})
}

func (d *Dashboard) handleCreateAgent(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	name := strings.TrimSpace(r.FormValue("name"))
	class := domain.WorkloadClass(strings.TrimSpace(r.FormValue("class")))
	desc := strings.TrimSpace(r.FormValue("description"))
	target := strings.TrimSpace(r.FormValue("runtime_target"))
	rawProvs := strings.TrimSpace(r.FormValue("providers"))

	if name == "" || desc == "" {
		http.Redirect(w, r, "/studio?err="+url.QueryEscape("name and description are required"), http.StatusSeeOther)
		return
	}

	budgetRef := d.defaultBudgetRef()
	wl := domain.Workload{
		TypeMeta: domain.TypeMeta{APIVersion: domain.APIVersionV1Alpha1, Kind: domain.KindWorkload},
		Metadata: domain.ObjectMeta{Name: name, Zone: domain.ZoneSandbox},
		Spec: domain.WorkloadSpec{
			Class:     class,
			AgentCard: domain.AgentCard{Description: desc},
		},
	}

	if class == domain.ClassCodingAgent {
		harness := target
		if harness != "claude" && harness != "codex" && harness != "agy" {
			harness = "claude"
		}
		wl.Spec.Runtime = domain.RuntimeConfig{Type: "worktree-cli"}
		wl.Spec.Coding = &domain.CodingConfig{Harness: harness, RepoPath: ".", Sandbox: "seatbelt"}
	} else {
		img := target
		if img == "" {
			img = "alpine:3.21"
		}
		wl.Spec.Runtime = domain.RuntimeConfig{Type: "docker", Image: img, ReadOnlyRootfs: true}
	}

	if class != domain.ClassDeterministicController && budgetRef != "" {
		var provs []string
		for _, p := range strings.Split(rawProvs, ",") {
			if s := strings.TrimSpace(p); s != "" {
				provs = append(provs, s)
			}
		}
		wl.Spec.Grants.ModelGate = domain.ModelGateGrant{
			Allowed:   true,
			Providers: provs,
			BudgetRef: budgetRef,
		}
	}

	if err := d.appendManifestDoc(wl); err != nil {
		http.Redirect(w, r, "/studio?err="+url.QueryEscape(err.Error()), http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/studio?notice="+url.QueryEscape("Saved sandbox agent "+name+" and reloaded manifests"), http.StatusSeeOther)
}

func (d *Dashboard) handleCreateWorkflow(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	name := strings.TrimSpace(r.FormValue("name"))
	desc := strings.TrimSpace(r.FormValue("description"))
	if name == "" {
		http.Redirect(w, r, "/studio?err="+url.QueryEscape("workflow name is required"), http.StatusSeeOther)
		return
	}

	var steps []domain.WorkflowStep
	for _, prefix := range []string{"step1", "step2"} {
		sName := strings.TrimSpace(r.FormValue(prefix + "_name"))
		sWL := strings.TrimSpace(r.FormValue(prefix + "_workload"))
		if sName == "" || sWL == "" {
			continue
		}
		steps = append(steps, domain.WorkflowStep{
			Name:     sName,
			Workload: sWL,
			Model:    strings.TrimSpace(r.FormValue(prefix + "_model")),
			Prompt:   strings.TrimSpace(r.FormValue(prefix + "_prompt")),
		})
	}
	if len(steps) == 0 {
		http.Redirect(w, r, "/studio?err="+url.QueryEscape("at least one workflow step is required"), http.StatusSeeOther)
		return
	}

	wf := domain.Workflow{
		TypeMeta: domain.TypeMeta{APIVersion: domain.APIVersionV1Alpha1, Kind: domain.KindWorkflow},
		Metadata: domain.ObjectMeta{Name: name, Zone: domain.ZoneSandbox},
		Spec:     domain.WorkflowSpec{Description: desc, Steps: steps},
	}
	if err := d.appendManifestDoc(wf); err != nil {
		http.Redirect(w, r, "/studio?err="+url.QueryEscape(err.Error()), http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/studio?notice="+url.QueryEscape("Saved sandbox workflow "+name+" and reloaded manifests"), http.StatusSeeOther)
}

func (d *Dashboard) handleRunWorkflow(w http.ResponseWriter, r *http.Request) {
	if d.reconciler == nil {
		http.Redirect(w, r, "/studio?err="+url.QueryEscape("reconciler unavailable"), http.StatusSeeOther)
		return
	}
	_ = r.ParseForm()
	wfName := r.PathValue("name")
	task, err := d.reconciler.StartWorkflow(r.Context(), wfName, strings.TrimSpace(r.FormValue("input")))
	if err != nil {
		http.Redirect(w, r, "/studio?err="+url.QueryEscape(err.Error()), http.StatusSeeOther)
		return
	}
	redirectNotice(w, r, fmt.Sprintf("Started workflow %s (step 1 task %s)", wfName, task.ID))
}

func (d *Dashboard) defaultBudgetRef() string {
	if d.getBundle == nil {
		return ""
	}
	b := d.getBundle()
	if b == nil {
		return ""
	}
	for name := range b.Budgets {
		return name
	}
	return ""
}

func (d *Dashboard) appendManifestDoc(doc any) error {
	if d.manifestsPath == "" {
		return fmt.Errorf("sablecourtd was started without --manifests; cannot persist YAML")
	}
	raw, err := yaml.Marshal(doc)
	if err != nil {
		return err
	}
	info, err := os.Stat(d.manifestsPath)
	if err != nil {
		return err
	}
	if info.IsDir() {
		outFile := filepath.Join(d.manifestsPath, fmt.Sprintf("studio-%d.yaml", time.Now().Unix()))
		if err := os.WriteFile(outFile, raw, 0o600); err != nil {
			return err
		}
	} else {
		f, err := os.OpenFile(d.manifestsPath, os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			return err
		}
		if _, err := f.WriteString("\n---\n" + string(raw)); err != nil {
			_ = f.Close()
			return err
		}
		_ = f.Close()
	}
	if d.reload != nil {
		return d.reload()
	}
	return nil
}

func redirectNotice(w http.ResponseWriter, r *http.Request, msg string) {
	http.Redirect(w, r, "/?notice="+url.QueryEscape(msg), http.StatusSeeOther)
}

func redirectErr(w http.ResponseWriter, r *http.Request, msg string) {
	http.Redirect(w, r, "/?err="+url.QueryEscape(msg), http.StatusSeeOther)
}
