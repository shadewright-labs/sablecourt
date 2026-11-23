package ui

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

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
	orig, err := d.store.GetTask(r.Context(), id)
	if err != nil {
		http.Redirect(w, r, "/?err="+url.QueryEscape("task not found"), http.StatusSeeOther)
		return
	}
	idem := fmt.Sprintf("ui-retry-%s-%d", orig.ID, time.Now().UTC().UnixNano())
	retried, err := d.reconciler.SubmitTask(r.Context(), orig.WorkloadName, idem, orig.InputJSON)
	if err != nil {
		http.Redirect(w, r, "/tasks/"+url.PathEscape(id)+"?err="+url.QueryEscape(err.Error()), http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/tasks/"+url.PathEscape(retried.ID)+"?notice="+url.QueryEscape("Retried from "+orig.ID), http.StatusSeeOther)
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
