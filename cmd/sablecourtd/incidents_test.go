package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/shadewright-labs/sablecourt/internal/conclave"
	"github.com/shadewright-labs/sablecourt/internal/config"
	"github.com/shadewright-labs/sablecourt/internal/controlplane"
	"github.com/shadewright-labs/sablecourt/internal/domain"
	"github.com/shadewright-labs/sablecourt/internal/store"
)

func incidentTestHandler(st store.Store, defaultTemplate string) http.Handler {
	bundle := &config.Bundle{Conclaves: map[string]domain.Conclave{"primary": {}, "secondary": {}}}
	getBundle := func() *config.Bundle { return bundle }
	broker := conclave.NewBroker(conclave.Options{Store: st, GetBundle: getBundle})
	return newHandler(st, nil, getBundle, nil, nil, nil, nil, nil, nil, broker, nil, nil, nil, handlerConfig{incidentTemplate: defaultTemplate})
}

func postIncident(h http.Handler, id, template string) *httptest.ResponseRecorder {
	body, _ := json.Marshal(map[string]string{"incidentId": id, "summary": "Review failure", "conclaveTemplate": template})
	r := httptest.NewRequest(http.MethodPost, "/api/v1/integrations/shadewright/incidents", strings.NewReader(string(body)))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestIncidentIntakeIdempotent(t *testing.T) {
	for _, tc := range []struct {
		name     string
		rebuild  bool
		parallel bool
	}{
		{name: "sequential"}, {name: "handler restart", rebuild: true}, {name: "concurrent", parallel: true}, {name: "concurrent handlers", parallel: true, rebuild: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := daemonTestStore(t)
			bundle := &config.Bundle{Conclaves: map[string]domain.Conclave{
				"primary": {Spec: domain.ConclaveSpec{Vassals: []domain.ConclaveVassal{
					{Role: "maintainer", Workload: "maintainer", Activation: domain.ActivationAlwaysOn, CanMessage: []string{"all"}},
					{Role: "analyst", Workload: "analyst", Activation: domain.ActivationOnMention, CanMessage: []string{"all"}},
				}}},
			}, Workloads: map[string]domain.Workload{}}
			for _, name := range []string{"maintainer", "analyst"} {
				if err := st.UpsertWorkload(context.Background(), domain.WorkloadRecord{Name: name, Zone: domain.ZoneRuntime, Class: domain.ClassLongLivedAgent, AgentCardJSON: "{}"}); err != nil {
					t.Fatal(err)
				}
				bundle.Workloads[name] = domain.Workload{Metadata: domain.ObjectMeta{Name: name}}
			}
			getBundle := func() *config.Bundle { return bundle }
			reconciler := controlplane.NewReconciler(controlplane.Options{Store: st, Bundle: bundle})
			buildHandler := func() http.Handler {
				broker := conclave.NewBroker(conclave.Options{Store: st, GetBundle: getBundle, Reconciler: reconciler})
				return newHandler(st, nil, getBundle, nil, nil, nil, nil, reconciler, nil, broker, nil, nil, nil)
			}
			h := buildHandler()
			const count = 6
			responses := make([]*httptest.ResponseRecorder, count)
			var wg sync.WaitGroup
			for i := range responses {
				if tc.rebuild {
					h = buildHandler()
				}
				if tc.parallel {
					wg.Add(1)
					go func(i int, handler http.Handler) {
						defer wg.Done()
						responses[i] = postIncident(handler, "INC-42", "primary")
					}(i, h)
				} else {
					responses[i] = postIncident(h, "INC-42", "primary")
				}
			}
			wg.Wait()
			created := 0
			id := ""
			for _, w := range responses {
				if w.Code == 201 {
					created++
				} else if w.Code != 200 {
					t.Errorf("status = %d: %s", w.Code, w.Body.String())
				}
				var got struct {
					Conclave domain.ConclaveSession
					Relic    domain.ConclaveRelic
				}
				if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil || got.Conclave.ID == "" {
					t.Fatalf("invalid response: %s", w.Body.String())
				}
				if id == "" {
					id = got.Conclave.ID
				}
				if got.Conclave.ID != id {
					t.Errorf("Conclave ID changed: %s != %s", got.Conclave.ID, id)
				}
			}
			if created != 1 {
				t.Errorf("created %d councils, want 1", created)
			}
			sessions, err := st.ListConclaves(context.Background())
			if err != nil || len(sessions) != 1 {
				t.Fatalf("sessions = %d, error = %v", len(sessions), err)
			}
			relics, err := st.ListConclaveRelics(context.Background(), id)
			if err != nil || len(relics) != 1 {
				t.Fatalf("relics = %d, error = %v", len(relics), err)
			}
			tasks, err := st.ListTasks(context.Background())
			if err != nil || len(tasks) != 2 {
				t.Fatalf("activation tasks = %d, error = %v", len(tasks), err)
			}
			msgs, err := st.ListConclaveMessages(context.Background(), id, 0)
			if err != nil || len(msgs) != 1 {
				t.Fatalf("opening messages = %d, error = %v", len(msgs), err)
			}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/conclaves/"+id+"/relics/"+relics[0].Name, nil))
			if w.Code != 200 {
				t.Fatalf("Relic fetch status = %d", w.Code)
			}
		})
	}
}

func TestIncidentTemplateSelection(t *testing.T) {
	for _, tc := range []struct {
		name, template, defaultTemplate, wantTemplate string
		want                                          int
	}{
		{name: "no default", want: 400},
		{name: "configured default", defaultTemplate: "secondary", wantTemplate: "secondary", want: 201},
		{name: "explicit overrides default", template: "primary", defaultTemplate: "secondary", wantTemplate: "primary", want: 201},
		{name: "unknown explicit", template: "missing", defaultTemplate: "primary", want: 400},
		{name: "unknown default", defaultTemplate: "missing", want: 400},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := daemonTestStore(t)
			w := postIncident(incidentTestHandler(st, tc.defaultTemplate), "INC-42", tc.template)
			if w.Code != tc.want {
				t.Fatalf("status = %d, want %d: %s", w.Code, tc.want, w.Body.String())
			}
			sessions, err := st.ListConclaves(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if tc.want == 400 && len(sessions) != 0 {
				t.Fatal("refused intake created a Conclave")
			}
			if tc.want == 201 && (len(sessions) != 1 || sessions[0].TemplateName != tc.wantTemplate) {
				t.Fatalf("unexpected sessions: %+v", sessions)
			}
		})
	}
}

func TestIncidentIDPattern(t *testing.T) {
	for _, tc := range []struct {
		id   string
		want int
	}{
		{"", 400}, {"INC/42", 400}, {"../42", 400}, {"INC%2f42", 400}, {" INC-42", 400},
		{"INC 42", 400}, {"INC\n42", 400}, {"INC?42", 400}, {"incident-ą", 400},
		{"_INC", 400}, {"-INC", 400}, {strings.Repeat("a", 129), 400},
		{"INC-42", 201}, {"a", 201}, {"Incident_42-abc", 201}, {strings.Repeat("a", 128), 201},
	} {
		t.Run(fmt.Sprintf("%q", tc.id), func(t *testing.T) {
			st := daemonTestStore(t)
			w := postIncident(incidentTestHandler(st, ""), tc.id, "primary")
			if w.Code != tc.want {
				t.Fatalf("status = %d, want %d", w.Code, tc.want)
			}
			if tc.want == 400 {
				sessions, err := st.ListConclaves(context.Background())
				if err != nil || len(sessions) != 0 {
					t.Fatalf("refused intake sessions = %+v, error = %v", sessions, err)
				}
			}
		})
	}
}

type incidentFailureStore struct {
	store.Store
	failure string
}

func (s incidentFailureStore) GetConclave(ctx context.Context, id string) (domain.ConclaveSession, error) {
	if s.failure == "lookup" {
		return domain.ConclaveSession{}, fmt.Errorf("lookup unavailable")
	}
	return s.Store.GetConclave(ctx, id)
}

func (s incidentFailureStore) PutConclaveRelic(ctx context.Context, relic domain.ConclaveRelic) error {
	if s.failure == "relic" {
		return fmt.Errorf("relic unavailable")
	}
	return s.Store.PutConclaveRelic(ctx, relic)
}

func (s incidentFailureStore) AppendConclaveMessage(ctx context.Context, msg domain.ConclaveMessage) (int64, error) {
	if s.failure == "message" {
		return 0, fmt.Errorf("message unavailable")
	}
	return s.Store.AppendConclaveMessage(ctx, msg)
}

func TestIncidentStorageFailuresFailClosed(t *testing.T) {
	for _, failure := range []string{"lookup", "relic", "message"} {
		t.Run(failure, func(t *testing.T) {
			st := daemonTestStore(t)
			h := incidentTestHandler(incidentFailureStore{Store: st, failure: failure}, "")
			for attempt := 0; attempt < 2; attempt++ {
				w := postIncident(h, "INC-42", "primary")
				if w.Code < 500 {
					t.Errorf("attempt %d: status = %d, want server refusal", attempt, w.Code)
				}
			}
			sessions, err := st.ListConclaves(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if failure == "lookup" && len(sessions) != 0 {
				t.Fatal("failed lookup created a Conclave")
			}
			if len(sessions) > 1 {
				t.Fatalf("failure created %d Conclaves", len(sessions))
			}
			if failure == "relic" {
				audits, err := st.ListAudit(context.Background(), 100)
				if err != nil {
					t.Fatal(err)
				}
				for _, audit := range audits {
					if audit.Action == "shadewright.incident_escalated" && audit.Outcome == "ok" {
						t.Fatal("failed intake recorded a successful escalation")
					}
				}
			}
		})
	}
}

func TestIncidentRetryAfterFault(t *testing.T) {
	for _, failure := range []string{"relic", "message", "wake", "audit"} {
		t.Run(failure, func(t *testing.T) {
			ctx := context.Background()
			base := daemonTestStore(t)
			st := &incidentFailureStore{Store: base, failure: failure}
			b := &config.Bundle{Workloads: map[string]domain.Workload{}, Conclaves: map[string]domain.Conclave{
				"primary": {Spec: domain.ConclaveSpec{Vassals: []domain.ConclaveVassal{
					{Role: "worker", Workload: "worker", Activation: domain.ActivationAlwaysOn},
					{Role: "reader", Workload: "reader", Activation: domain.ActivationOnMention},
				}}},
			}}
			for _, name := range []string{"worker", "reader"} {
				if err := base.UpsertWorkload(ctx, domain.WorkloadRecord{Name: name, Class: domain.ClassLongLivedAgent}); err != nil {
					t.Fatal(err)
				}
				b.Workloads[name] = domain.Workload{Metadata: domain.ObjectMeta{Name: name}}
			}
			build := func() http.Handler {
				get := func() *config.Bundle { return b }
				r := controlplane.NewReconciler(controlplane.Options{Store: st, Bundle: b})
				return incidentHandler(st, get, conclave.NewBroker(conclave.Options{Store: st, GetBundle: get, Reconciler: r}), r, "")
			}
			if w := postIncident(build(), "incident", "primary"); w.Code < 500 {
				t.Errorf("fault returned %d", w.Code)
			}
			tasks, err := base.ListTasks(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if failure == "relic" && len(tasks) > 0 {
				t.Error("dispatched before Relic")
			}
			st.failure = ""
			for range 2 {
				w := postIncident(build(), "incident", "primary")
				if w.Code != 200 && w.Code != 201 {
					t.Fatalf("retry returned %d: %s", w.Code, w.Body.String())
				}
			}
			tasks, err = base.ListTasks(ctx)
			if err != nil || len(tasks) != 2 {
				t.Errorf("tasks=%d err=%v", len(tasks), err)
			}
			sessions, err := base.ListConclaves(ctx)
			if err != nil || len(sessions) != 1 {
				t.Fatalf("sessions=%d err=%v", len(sessions), err)
			}
			msgs, err := base.ListConclaveMessages(ctx, sessions[0].ID, 0)
			if err != nil || len(msgs) != 1 {
				t.Errorf("messages=%d err=%v", len(msgs), err)
			}
			audits, err := base.ListAudit(ctx, 100)
			if err != nil {
				t.Fatal(err)
			}
			count := 0
			for _, row := range audits {
				if row.Action == "shadewright.incident_escalated" {
					count++
				}
			}
			if count != 1 {
				t.Errorf("escalation audits=%d", count)
			}
		})
	}
}
func (s incidentFailureStore) CreateTask(ctx context.Context, t domain.Task) error {
	if s.failure == "wake" && t.WorkloadName == "reader" {
		return fmt.Errorf("wake unavailable")
	}
	return s.Store.CreateTask(ctx, t)
}
func (s incidentFailureStore) AppendAudit(ctx context.Context, a domain.AuditRecord) error {
	if s.failure == "audit" && a.Action == "shadewright.incident_escalated" {
		return fmt.Errorf("audit unavailable")
	}
	return s.Store.AppendAudit(ctx, a)
}
