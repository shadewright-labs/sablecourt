package ui

import (
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"

	"github.com/shadewright-labs/sablecourt/internal/domain"
)

type conclavesViewData struct {
	Notice    string
	ErrMsg    string
	Templates []domain.Conclave
	Sessions  []domain.ConclaveSession
}

type conclaveDetailViewData struct {
	Notice   string
	ErrMsg   string
	Session  domain.ConclaveSession
	Template *domain.Conclave
	Messages []domain.ConclaveMessage
	Relics   []domain.ConclaveRelic
}

func (d *Dashboard) handleConclavesPage(w http.ResponseWriter, r *http.Request) {
	sessions, _ := d.store.ListConclaves(r.Context())
	var templates []domain.Conclave
	if d.getBundle != nil {
		if b := d.getBundle(); b != nil {
			for _, cc := range b.Conclaves {
				templates = append(templates, cc)
			}
			sort.Slice(templates, func(i, j int) bool {
				return templates[i].Metadata.Name < templates[j].Metadata.Name
			})
		}
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = d.conclavesTmpl.Execute(w, conclavesViewData{
		Notice:    r.URL.Query().Get("notice"),
		ErrMsg:    r.URL.Query().Get("error"),
		Templates: templates,
		Sessions:  sessions,
	})
}

func (d *Dashboard) handleConclaveDetailPage(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	sess, err := d.store.GetConclave(r.Context(), id)
	if err != nil {
		http.Error(w, "conclave not found", http.StatusNotFound)
		return
	}
	msgs, _ := d.store.ListConclaveMessages(r.Context(), id, 0)
	relics, _ := d.store.ListConclaveRelics(r.Context(), id)
	var tpl *domain.Conclave
	if d.getBundle != nil {
		if b := d.getBundle(); b != nil {
			if cc, ok := b.Conclaves[sess.TemplateName]; ok {
				copyCC := cc
				tpl = &copyCC
			}
		}
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = d.conclaveTmpl.Execute(w, conclaveDetailViewData{
		Notice:   r.URL.Query().Get("notice"),
		ErrMsg:   r.URL.Query().Get("error"),
		Session:  sess,
		Template: tpl,
		Messages: msgs,
		Relics:   relics,
	})
}

func (d *Dashboard) handleOpenConclave(w http.ResponseWriter, r *http.Request) {
	if d.conclave == nil {
		http.Redirect(w, r, "/conclaves?error="+url.QueryEscape("conclave broker unavailable"), http.StatusSeeOther)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Redirect(w, r, "/conclaves?error="+url.QueryEscape(err.Error()), http.StatusSeeOther)
		return
	}
	tplName := strings.TrimSpace(r.FormValue("template"))
	title := strings.TrimSpace(r.FormValue("title"))
	decree := strings.TrimSpace(r.FormValue("decree"))
	sess, err := d.conclave.Open(r.Context(), tplName, title, decree)
	if err != nil {
		http.Redirect(w, r, "/conclaves?error="+url.QueryEscape(err.Error()), http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, fmt.Sprintf("/conclaves/%s?notice=%s", sess.ID, url.QueryEscape("Conclave convened")), http.StatusSeeOther)
}

func (d *Dashboard) handlePostDecree(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if d.conclave == nil {
		http.Redirect(w, r, "/conclaves/"+id+"?error="+url.QueryEscape("conclave broker unavailable"), http.StatusSeeOther)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Redirect(w, r, "/conclaves/"+id+"?error="+url.QueryEscape(err.Error()), http.StatusSeeOther)
		return
	}
	to := strings.TrimSpace(r.FormValue("to"))
	body := strings.TrimSpace(r.FormValue("body"))
	if _, err := d.conclave.PostMessage(r.Context(), id, "operator", to, "decree", body, ""); err != nil {
		http.Redirect(w, r, "/conclaves/"+id+"?error="+url.QueryEscape(err.Error()), http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/conclaves/"+id+"?notice="+url.QueryEscape("Decree issued"), http.StatusSeeOther)
}

func (d *Dashboard) handleCloseConclave(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if d.conclave == nil {
		http.Redirect(w, r, "/conclaves/"+id+"?error="+url.QueryEscape("conclave broker unavailable"), http.StatusSeeOther)
		return
	}
	_ = r.ParseForm()
	state := domain.ConclaveState(r.FormValue("state"))
	if err := d.conclave.Close(r.Context(), id, state); err != nil {
		http.Redirect(w, r, "/conclaves/"+id+"?error="+url.QueryEscape(err.Error()), http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/conclaves/"+id+"?notice="+url.QueryEscape("Conclave closed"), http.StatusSeeOther)
}

func (d *Dashboard) handleStudioCreateConclave(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	name := strings.TrimSpace(r.FormValue("name"))
	desc := strings.TrimSpace(r.FormValue("description"))
	if name == "" {
		http.Redirect(w, r, "/studio?err="+url.QueryEscape("conclave name is required"), http.StatusSeeOther)
		return
	}
	var maxMsgs, maxPP int
	_, _ = fmt.Sscanf(r.FormValue("max_messages"), "%d", &maxMsgs)
	_, _ = fmt.Sscanf(r.FormValue("max_ping_pong"), "%d", &maxPP)

	var vassals []domain.ConclaveVassal
	for _, p := range []string{"v1", "v2"} {
		role := strings.TrimSpace(r.FormValue(p + "_role"))
		wl := strings.TrimSpace(r.FormValue(p + "_workload"))
		act := domain.ConclaveActivation(strings.TrimSpace(r.FormValue(p + "_activation")))
		if role == "" || wl == "" {
			continue
		}
		if act != domain.ActivationAlwaysOn && act != domain.ActivationOnMention {
			act = domain.ActivationOnMention
		}
		vassals = append(vassals, domain.ConclaveVassal{
			Role:       role,
			Workload:   wl,
			Activation: act,
			CanMessage: []string{"all"},
		})
	}
	if len(vassals) == 0 {
		http.Redirect(w, r, "/studio?err="+url.QueryEscape("at least one vassal is required"), http.StatusSeeOther)
		return
	}

	cc := domain.Conclave{
		TypeMeta: domain.TypeMeta{APIVersion: domain.APIVersionV1Alpha1, Kind: domain.KindConclave},
		Metadata: domain.ObjectMeta{Name: name},
		Spec: domain.ConclaveSpec{
			Description: desc,
			Wards:       domain.ConclaveWards{MaxMessages: maxMsgs, MaxPingPong: maxPP},
			Vassals:     vassals,
		},
	}
	if err := d.appendManifestDoc(cc); err != nil {
		http.Redirect(w, r, "/studio?err="+url.QueryEscape(err.Error()), http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/studio?notice="+url.QueryEscape("Saved Conclave "+name+" and reloaded manifests"), http.StatusSeeOther)
}
