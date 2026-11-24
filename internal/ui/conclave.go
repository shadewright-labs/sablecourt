package ui

import (
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"github.com/shadewright-labs/sablecourt/internal/config"
	"github.com/shadewright-labs/sablecourt/internal/domain"
)

const (
	defaultWardMaxMessages = 40
	defaultWardMaxPingPong = 4
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
		ErrMsg:    r.URL.Query().Get("err"),
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
		ErrMsg:   r.URL.Query().Get("err"),
		Session:  sess,
		Template: tpl,
		Messages: msgs,
		Relics:   relics,
	})
}

func (d *Dashboard) handleOpenConclave(w http.ResponseWriter, r *http.Request) {
	if d.conclave == nil {
		http.Redirect(w, r, "/conclaves?err="+url.QueryEscape("conclave broker unavailable"), http.StatusSeeOther)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Redirect(w, r, "/conclaves?err="+url.QueryEscape(err.Error()), http.StatusSeeOther)
		return
	}
	tplName := strings.TrimSpace(r.FormValue("template"))
	title := strings.TrimSpace(r.FormValue("title"))
	decree := strings.TrimSpace(r.FormValue("decree"))
	sess, err := d.conclave.Open(r.Context(), tplName, title, decree)
	if err != nil {
		http.Redirect(w, r, "/conclaves?err="+url.QueryEscape(err.Error()), http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, fmt.Sprintf("/conclaves/%s?notice=%s", url.PathEscape(sess.ID), url.QueryEscape("Conclave convened")), http.StatusSeeOther)
}

func (d *Dashboard) handlePostDecree(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if d.conclave == nil {
		redirectConclaveErr(w, r, id, "conclave broker unavailable")
		return
	}
	if err := r.ParseForm(); err != nil {
		redirectConclaveErr(w, r, id, err.Error())
		return
	}
	to := strings.TrimSpace(r.FormValue("to"))
	body := strings.TrimSpace(r.FormValue("body"))
	if _, err := d.conclave.PostMessage(r.Context(), id, "operator", to, "decree", body, ""); err != nil {
		redirectConclaveErr(w, r, id, err.Error())
		return
	}
	http.Redirect(w, r, "/conclaves/"+url.PathEscape(id)+"?notice="+url.QueryEscape("Decree issued"), http.StatusSeeOther)
}

func (d *Dashboard) handleCloseConclave(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if d.conclave == nil {
		redirectConclaveErr(w, r, id, "conclave broker unavailable")
		return
	}
	if err := r.ParseForm(); err != nil {
		redirectConclaveErr(w, r, id, err.Error())
		return
	}
	state := domain.ConclaveState(r.FormValue("state"))
	if err := d.conclave.Close(r.Context(), id, state); err != nil {
		redirectConclaveErr(w, r, id, err.Error())
		return
	}
	http.Redirect(w, r, "/conclaves/"+url.PathEscape(id)+"?notice="+url.QueryEscape("Conclave closed"), http.StatusSeeOther)
}

func (d *Dashboard) handleStudioCreateConclave(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		redirectStudioErr(w, r, err.Error())
		return
	}
	name := strings.TrimSpace(r.FormValue("name"))
	desc := strings.TrimSpace(r.FormValue("description"))
	if name == "" {
		redirectStudioErr(w, r, "conclave name is required")
		return
	}
	if len(name) > 128 || hasControlChars(name) {
		redirectStudioErr(w, r, "invalid conclave name")
		return
	}
	if d.getBundle != nil {
		if b := d.getBundle(); b != nil {
			if _, exists := b.Conclaves[name]; exists {
				redirectStudioErr(w, r, fmt.Sprintf("conclave %q already exists", name))
				return
			}
		}
	}

	maxMsgs, err := parsePositiveIntDefault(r.FormValue("max_messages"), defaultWardMaxMessages, "maxMessages")
	if err != nil {
		redirectStudioErr(w, r, err.Error())
		return
	}
	maxPP, err := parsePositiveIntDefault(r.FormValue("max_ping_pong"), defaultWardMaxPingPong, "maxPingPong")
	if err != nil {
		redirectStudioErr(w, r, err.Error())
		return
	}

	vassals, err := parseStudioConclaveVassals(r.Form)
	if err != nil {
		redirectStudioErr(w, r, err.Error())
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
		redirectStudioErr(w, r, err.Error())
		return
	}
	http.Redirect(w, r, "/studio?notice="+url.QueryEscape("Saved Conclave "+name+" and reloaded manifests"), http.StatusSeeOther)
}

func parsePositiveIntDefault(raw string, def int, field string) (int, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return def, nil
	}
	n, err := strconv.Atoi(s)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("%s must be a positive integer", field)
	}
	return n, nil
}

func parseStudioConclaveVassals(form url.Values) ([]domain.ConclaveVassal, error) {
	var roles, workloads, activations []string
	_, hasRoles := form["vassal_role"]
	_, hasWorkloads := form["vassal_workload"]
	_, hasActivations := form["vassal_activation"]
	if hasRoles || hasWorkloads || hasActivations {
		roles = form["vassal_role"]
		workloads = form["vassal_workload"]
		activations = form["vassal_activation"]
		if len(roles) != len(workloads) {
			return nil, fmt.Errorf("mismatched vassal role and workload fields")
		}
		if len(activations) > 0 && len(activations) != len(roles) {
			return nil, fmt.Errorf("mismatched vassal activation fields")
		}
	} else {
		// Support legacy v1_*/v2_*/... field prefixes when repeated fields are absent.
		for i := 1; i <= 16; i++ {
			prefix := fmt.Sprintf("v%d_", i)
			_, rOK := form[prefix+"role"]
			_, wOK := form[prefix+"workload"]
			_, aOK := form[prefix+"activation"]
			if !rOK && !wOK && !aOK {
				continue
			}
			roles = append(roles, form.Get(prefix+"role"))
			workloads = append(workloads, form.Get(prefix+"workload"))
			activations = append(activations, form.Get(prefix+"activation"))
		}
	}

	var vassals []domain.ConclaveVassal
	seenRoles := make(map[string]struct{}, len(roles))
	for i := range roles {
		role := strings.TrimSpace(roles[i])
		wl := strings.TrimSpace(workloads[i])
		if role == "" && wl == "" {
			continue
		}
		if role == "" || wl == "" {
			return nil, fmt.Errorf("each vassal row requires both role and workload")
		}
		if !config.ValidVassalRole(role) || len(role) > 64 || hasControlChars(role) {
			return nil, fmt.Errorf("invalid vassal role %q", role)
		}
		if _, dup := seenRoles[role]; dup {
			return nil, fmt.Errorf("duplicate vassal role %q", role)
		}
		seenRoles[role] = struct{}{}

		act := domain.ActivationOnMention
		if len(activations) > 0 {
			rawAct := strings.TrimSpace(activations[i])
			if rawAct != "" {
				act = domain.ConclaveActivation(rawAct)
			}
		}
		if act != domain.ActivationAlwaysOn && act != domain.ActivationOnMention {
			return nil, fmt.Errorf("invalid vassal activation %q", act)
		}
		vassals = append(vassals, domain.ConclaveVassal{
			Role:       role,
			Workload:   wl,
			Activation: act,
			CanMessage: []string{"all"},
		})
	}
	if len(vassals) == 0 {
		return nil, fmt.Errorf("at least one vassal is required")
	}
	return vassals, nil
}

func hasControlChars(s string) bool {
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return true
		}
	}
	return false
}

func redirectStudioErr(w http.ResponseWriter, r *http.Request, msg string) {
	http.Redirect(w, r, "/studio?err="+url.QueryEscape(msg), http.StatusSeeOther)
}

func redirectConclaveErr(w http.ResponseWriter, r *http.Request, id, msg string) {
	http.Redirect(w, r, "/conclaves/"+url.PathEscape(id)+"?err="+url.QueryEscape(msg), http.StatusSeeOther)
}
