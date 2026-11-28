package ui

import (
	"fmt"
	"net/http"
	"net/url"
	"sort"

	"github.com/shadewright-labs/sablecourt/internal/domain"
)

type schedulesViewData struct {
	Notice    string
	ErrMsg    string
	Schedules []domain.Schedule
}

func (d *Dashboard) handleSchedulesPage(w http.ResponseWriter, r *http.Request) {
	var schedules []domain.Schedule
	if d.getBundle != nil {
		if b := d.getBundle(); b != nil {
			for _, sc := range b.Schedules {
				schedules = append(schedules, sc)
			}
			sort.Slice(schedules, func(i, j int) bool {
				return schedules[i].Metadata.Name < schedules[j].Metadata.Name
			})
		}
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = d.schedulesTmpl.Execute(w, schedulesViewData{
		Notice:    r.URL.Query().Get("notice"),
		ErrMsg:    r.URL.Query().Get("err"),
		Schedules: schedules,
	})
}

func (d *Dashboard) handleRunSchedule(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if d.scheduler == nil || d.getBundle == nil {
		http.Redirect(w, r, "/schedules?err="+url.QueryEscape("scheduler unavailable"), http.StatusSeeOther)
		return
	}
	b := d.getBundle()
	if b == nil {
		http.Redirect(w, r, "/schedules?err="+url.QueryEscape("no manifest bundle loaded"), http.StatusSeeOther)
		return
	}
	sc, ok := b.Schedules[name]
	if !ok {
		http.Redirect(w, r, "/schedules?err="+url.QueryEscape("schedule not found: "+name), http.StatusSeeOther)
		return
	}
	if err := d.scheduler.TriggerNow(r.Context(), sc); err != nil {
		http.Redirect(w, r, "/schedules?err="+url.QueryEscape(errText(r.Context(), err, "schedule run", "")), http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/schedules?notice="+url.QueryEscape(fmt.Sprintf("Triggered schedule %s (%s/%s)", name, sc.Spec.TargetKind, sc.Spec.TargetName)), http.StatusSeeOther)
}
