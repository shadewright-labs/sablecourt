package ui

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/shadewright-labs/sablecourt/internal/contextpack"
	"github.com/shadewright-labs/sablecourt/internal/domain"
	"github.com/shadewright-labs/sablecourt/internal/redact"
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

	entries, err := d.store.SearchMemory(r.Context(), scope, query, 100)
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
	retried, err := d.reconciler.RetryTaskWithHandoff(r.Context(), id, withHandoff, "operator_ui_retry")
	if err != nil {
		http.Redirect(w, r, "/tasks/"+url.PathEscape(id)+"?err="+url.QueryEscape(err.Error()), http.StatusSeeOther)
		return
	}
	msg := "Retried from " + id
	if withHandoff {
		msg = "Closed-loop remediation started from " + id + " (with failure-handoff.md)"
	}
	http.Redirect(w, r, "/tasks/"+url.PathEscape(retried.ID)+"?notice="+url.QueryEscape(msg), http.StatusSeeOther)
}

func (d *Dashboard) handleSetLearning(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Redirect(w, r, "/memory?err="+url.QueryEscape(err.Error()), http.StatusSeeOther)
		return
	}
	scope := strings.TrimSpace(r.FormValue("scope"))
	key := strings.TrimSpace(r.FormValue("key"))
	rule := strings.TrimSpace(r.FormValue("rule"))
	entry, err := contextpack.PutLearning(r.Context(), d.store, scope, key, rule, "operator")
	if err != nil {
		http.Redirect(w, r, "/memory?err="+url.QueryEscape(err.Error()), http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/memory?notice="+url.QueryEscape(fmt.Sprintf("Saved learning %s/%s", entry.Namespace, entry.Key)), http.StatusSeeOther)
}

func (d *Dashboard) handleConclaveSaveLearning(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := r.ParseForm(); err != nil {
		http.Redirect(w, r, "/conclaves/"+url.PathEscape(id)+"?err="+url.QueryEscape(err.Error()), http.StatusSeeOther)
		return
	}
	scope := strings.TrimSpace(r.FormValue("scope"))
	if scope == "" {
		scope = "global"
	}
	key := strings.TrimSpace(r.FormValue("key"))
	if key == "" {
		key = fmt.Sprintf("conclave-%s-%d", id, time.Now().UTC().Unix())
	}
	rule := strings.TrimSpace(r.FormValue("rule"))
	entry, err := contextpack.PutLearning(r.Context(), d.store, scope, key, rule, "operator")
	if err != nil {
		http.Redirect(w, r, "/conclaves/"+url.PathEscape(id)+"?err="+url.QueryEscape(err.Error()), http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/conclaves/"+url.PathEscape(id)+"?notice="+url.QueryEscape("Saved as learning: "+entry.Namespace+"/"+entry.Key), http.StatusSeeOther)
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
		})
	}
	if err := contextpack.SaveModelTiers(r.Context(), d.store, routes); err != nil {
		http.Redirect(w, r, "/providers?err="+url.QueryEscape(err.Error()), http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/providers?notice="+url.QueryEscape("Saved role-based model tiers (fast / coding / critic)"), http.StatusSeeOther)
}

func (d *Dashboard) handleConclaveUploadRelic(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if d.conclave == nil {
		http.Redirect(w, r, "/conclaves/"+url.PathEscape(id)+"?err=conclave+disabled", http.StatusSeeOther)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Redirect(w, r, "/conclaves/"+url.PathEscape(id)+"?err="+url.QueryEscape(err.Error()), http.StatusSeeOther)
		return
	}
	name := strings.TrimSpace(r.FormValue("name"))
	content := r.FormValue("content")
	if name == "" || strings.TrimSpace(content) == "" {
		http.Redirect(w, r, "/conclaves/"+url.PathEscape(id)+"?err="+url.QueryEscape("relic name and content are required"), http.StatusSeeOther)
		return
	}
	if _, err := d.conclave.PutRelic(r.Context(), id, "operator", name, "text/plain", content); err != nil {
		http.Redirect(w, r, "/conclaves/"+url.PathEscape(id)+"?err="+url.QueryEscape(err.Error()), http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/conclaves/"+url.PathEscape(id)+"?notice="+url.QueryEscape("Relic "+name+" added"), http.StatusSeeOther)
}
