package integrations

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/shadewright-labs/sablecourt/internal/conclave"
	"github.com/shadewright-labs/sablecourt/internal/config"
	"github.com/shadewright-labs/sablecourt/internal/controlplane"
	"github.com/shadewright-labs/sablecourt/internal/domain"
	"github.com/shadewright-labs/sablecourt/internal/secret"
	"github.com/shadewright-labs/sablecourt/internal/store"
	"github.com/shadewright-labs/sablecourt/internal/store/audittest"
)

// An incident escalation meets a failed audit write after each kind of
// effect it has: the room created, its relic written, the always-on task
// created, the opening message stored and the on-mention task woken. Each
// answer is HTTP 500 listing exactly the effects the request recorded, never
// that nothing changed (issue 179, round 3 finding 1).
func TestIncidentAuditFailuresListRecordedEffects(t *testing.T) {
	cases := []struct {
		name   string
		action string
		skip   int // matching writes that pass before the one that fails
		// want are the recorded effects; {room}, {worker} and {reader} stand
		// for the room and the IDs of the always-on and woken tasks.
		want []string
	}{
		{name: "open row after the room is created", action: "conclave.open",
			want: []string{"conclave {room} was opened"}},
		{name: "relic row after the room is created", action: "conclave.relic_put",
			want: []string{"conclave {room} was opened"}},
		{name: "always-on submit row after the relic is written", action: "task.submit",
			want: []string{"conclave {room} was opened", "relic incident-{room}.json was written to conclave {room}"}},
		{name: "on-mention wake row after the opening message is stored", action: "task.submit", skip: 1,
			want: []string{
				"conclave {room} was opened",
				"relic incident-{room}.json was written to conclave {room}",
				"task {worker} was created",
				"a message was added to task {worker}",
				"message 1 was stored in conclave {room}",
			}},
		{name: "escalation row after every effect", action: "shadewright.incident_escalated",
			want: []string{
				"conclave {room} was opened",
				"relic incident-{room}.json was written to conclave {room}",
				"task {worker} was created",
				"a message was added to task {worker}",
				"message 1 was stored in conclave {room}",
				"task {reader} was created",
				"a message was added to task {reader}",
			}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			base := newTestStore(t)
			bundle := newTestBundle(base, t, domain.ConclaveWards{})
			faults := audittest.Wrap(base)
			seen := 0
			faults.Fail(func(rec domain.AuditRecord) bool {
				if rec.Action != tc.action {
					return false
				}
				seen++
				return seen > tc.skip
			})
			sec := secret.NewMemoryStore(map[string]string{ShadewrightTokenSecretName: testToken})
			getBundle := func() *config.Bundle { return bundle }
			rec := controlplane.NewReconciler(controlplane.Options{Store: faults, Bundle: bundle})
			broker := conclave.NewBroker(conclave.Options{Store: faults, GetBundle: getBundle, Reconciler: rec})
			sw := NewShadewright(ShadewrightOptions{Store: faults, Secrets: sec, GetBundle: getBundle, Broker: broker, Reconciler: rec, DefaultTemplate: "primary"})

			req := httptest.NewRequest(http.MethodPost, ShadewrightIncidentPath, strings.NewReader(`{"incidentId":"INC-AUD-1","summary":"Crash"}`))
			req.Header.Set("Authorization", "Bearer "+testToken)
			ctx, effects := store.TrackEffects(req.Context())
			resp := httptest.NewRecorder()
			sw.IncidentHandler().ServeHTTP(resp, req.WithContext(ctx))
			if resp.Code != http.StatusInternalServerError {
				t.Fatalf("got %d %s, want 500", resp.Code, resp.Body.String())
			}
			var body struct {
				Error string `json:"error"`
			}
			if err := json.Unmarshal(resp.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			got := effects.List()
			if body.Error != audittest.Answer("the escalation", got...) {
				t.Fatalf("answer %q does not list the recorded effects %q", body.Error, got)
			}
			vars := map[string]string{"{room}": shadewrightConclaveID("INC-AUD-1")}
			tasks, err := base.ListTasks(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			for _, task := range tasks {
				if a, ok := store.TaskConclaveAssignment(task); ok {
					vars["{"+a.Role+"}"] = task.ID
				}
			}
			var want []string
			for _, w := range tc.want {
				for k, v := range vars {
					w = strings.ReplaceAll(w, k, v)
				}
				want = append(want, w)
			}
			if !slices.Equal(got, want) {
				t.Fatalf("effects %q, want %q", got, want)
			}
		})
	}
}

// A verification whose report and relic are stored before its row fails
// lists both.
func TestVerificationAuditFailuresListRecordedEffects(t *testing.T) {
	cases := []struct {
		name, action string
		want         func(room string) []string
	}{
		{"verification row after the report is stored", "shadewright.verification_posted", func(room string) []string {
			return []string{"message 2 was stored in conclave " + room}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			base := newTestStore(t)
			bundle := newTestBundle(base, t, domain.ConclaveWards{})
			faults := audittest.Wrap(base)
			sec := secret.NewMemoryStore(map[string]string{ShadewrightTokenSecretName: testToken})
			getBundle := func() *config.Bundle { return bundle }
			broker := conclave.NewBroker(conclave.Options{Store: faults, GetBundle: getBundle})
			sw := NewShadewright(ShadewrightOptions{Store: faults, Secrets: sec, GetBundle: getBundle, Broker: broker, DefaultTemplate: "primary"})
			if rec := postRaw(sw.IncidentHandler(), ShadewrightIncidentPath, "Bearer "+testToken, `{"incidentId":"INC-VER-1","summary":"Crash"}`); rec.Code != http.StatusCreated {
				t.Fatalf("open = %d: %s", rec.Code, rec.Body.String())
			}
			room := shadewrightConclaveID("INC-VER-1")
			faults.Fail(audittest.Action(tc.action))
			req := httptest.NewRequest(http.MethodPost, ShadewrightVerificationPath, strings.NewReader(`{"conclaveId":"`+room+`","incidentId":"INC-VER-1","status":"Cleared","summary":"Recovered"}`))
			req.Header.Set("Authorization", "Bearer "+testToken)
			ctx, effects := store.TrackEffects(req.Context())
			resp := httptest.NewRecorder()
			sw.VerificationHandler().ServeHTTP(resp, req.WithContext(ctx))
			var body struct {
				Error string `json:"error"`
			}
			_ = json.Unmarshal(resp.Body.Bytes(), &body)
			got := effects.List()
			if resp.Code != http.StatusInternalServerError || body.Error != audittest.Answer("the verification", got...) {
				t.Fatalf("got %d %q, want 500 listing %q", resp.Code, body.Error, got)
			}
			// The relic is named by a digest of the report.
			if len(got) != 2 || !strings.HasPrefix(got[0], "relic verification-") || !strings.HasSuffix(got[0], ".json was written to conclave "+room) || !slices.Equal(got[1:], tc.want(room)) {
				t.Fatalf("effects %q, want the relic and %q", got, tc.want(room))
			}
		})
	}
}
