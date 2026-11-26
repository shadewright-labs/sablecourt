package integrations

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/shadewright-labs/sablecourt/internal/conclave"
	"github.com/shadewright-labs/sablecourt/internal/config"
	"github.com/shadewright-labs/sablecourt/internal/controlplane"
	"github.com/shadewright-labs/sablecourt/internal/domain"
	"github.com/shadewright-labs/sablecourt/internal/secret"
	"github.com/shadewright-labs/sablecourt/internal/store"
	"github.com/shadewright-labs/sablecourt/internal/store/sqlite"
)

const testToken = "shadewright-unit-token"

func newTestStore(t *testing.T) store.Store {
	t.Helper()
	st, err := sqlite.Open(filepath.Join(t.TempDir(), "integrations.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	return st
}

func newTestBundle(st store.Store, t *testing.T, wards domain.ConclaveWards) *config.Bundle {
	t.Helper()
	ctx := context.Background()
	bundle := &config.Bundle{
		Workloads: map[string]domain.Workload{
			"worker": {Metadata: domain.ObjectMeta{Name: "worker", Zone: domain.ZoneRuntime}, Spec: domain.WorkloadSpec{Class: domain.ClassLongLivedAgent}},
			"reader": {Metadata: domain.ObjectMeta{Name: "reader", Zone: domain.ZoneRuntime}, Spec: domain.WorkloadSpec{Class: domain.ClassLongLivedAgent}},
		},
		Conclaves: map[string]domain.Conclave{
			"primary": {
				Metadata: domain.ObjectMeta{Name: "primary"},
				Spec: domain.ConclaveSpec{
					Wards: wards,
					Vassals: []domain.ConclaveVassal{
						{Role: "worker", Workload: "worker", Activation: domain.ActivationAlwaysOn, CanMessage: []string{"all"}},
						{Role: "reader", Workload: "reader", Activation: domain.ActivationOnMention, CanMessage: []string{"all"}},
					},
				},
			},
		},
	}
	for _, name := range []string{"worker", "reader"} {
		if err := st.UpsertWorkload(ctx, domain.WorkloadRecord{
			Name: name, Zone: domain.ZoneRuntime, Class: domain.ClassLongLivedAgent, AgentCardJSON: "{}",
		}); err != nil {
			t.Fatal(err)
		}
	}
	return bundle
}

func postRaw(h http.Handler, path, authHeader, body string) *httptest.ResponseRecorder {
	var req *http.Request
	if body == "<nil>" {
		req = httptest.NewRequest(http.MethodPost, path, nil)
		req.Body = nil
	} else {
		req = httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	}
	if authHeader != "" {
		req.Header.Set("Authorization", authHeader)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestShadewrightInputValidationAndOversizedPayloads(t *testing.T) {
	st := newTestStore(t)
	bundle := newTestBundle(st, t, domain.ConclaveWards{})
	sec := secret.NewMemoryStore(map[string]string{ShadewrightTokenSecretName: testToken})
	getBundle := func() *config.Bundle { return bundle }
	broker := conclave.NewBroker(conclave.Options{Store: st, GetBundle: getBundle})
	sw := NewShadewright(ShadewrightOptions{Store: st, Secrets: sec, GetBundle: getBundle, Broker: broker, DefaultTemplate: "primary"})

	// Open one valid Conclave for verification validation cases.
	openRec := postRaw(sw.IncidentHandler(), ShadewrightIncidentPath, "Bearer "+testToken, `{"incidentId":"INC-VAL-1","summary":"Disk full"}`)
	if openRec.Code != http.StatusCreated {
		t.Fatalf("open incident = %d: %s", openRec.Code, openRec.Body.String())
	}
	var opened struct {
		Conclave domain.ConclaveSession `json:"conclave"`
	}
	if err := json.Unmarshal(openRec.Body.Bytes(), &opened); err != nil {
		t.Fatal(err)
	}

	oversized := `{"incidentId":"INC-BIG","summary":"` + strings.Repeat("a", maxRequestBodyBytes+64) + `"}`
	oversizedVer := `{"conclaveId":"` + opened.Conclave.ID + `","status":"Cleared","summary":"` + strings.Repeat("a", maxRequestBodyBytes+64) + `"}`

	incidentCases := []struct {
		name string
		body string
		want int
	}{
		{"nil body", "<nil>", http.StatusBadRequest},
		{"empty body", "", http.StatusBadRequest},
		{"whitespace body", "   \t\n", http.StatusBadRequest},
		{"json null", "null", http.StatusBadRequest},
		{"json array", "[]", http.StatusBadRequest},
		{"json string", `"hello"`, http.StatusBadRequest},
		{"json number", "123", http.StatusBadRequest},
		{"truncated json", `{"incidentId":"INC-1"`, http.StatusBadRequest},
		{"trailing json", `{"incidentId":"INC-1","summary":"x"}{"extra":1}`, http.StatusBadRequest},
		{"wrong incidentId type", `{"incidentId":123,"summary":"x"}`, http.StatusBadRequest},
		{"wrong summary type", `{"incidentId":"INC-1","summary":123}`, http.StatusBadRequest},
		{"wrong evidence type", `{"incidentId":"INC-1","summary":"x","evidence":"not-an-object"}`, http.StatusBadRequest},
		{"whitespace summary", `{"incidentId":"INC-1","summary":"   "}`, http.StatusBadRequest},
		{"oversized body", oversized, http.StatusRequestEntityTooLarge},
	}
	for _, tc := range incidentCases {
		t.Run("incident/"+tc.name, func(t *testing.T) {
			rec := postRaw(sw.IncidentHandler(), ShadewrightIncidentPath, "Bearer "+testToken, tc.body)
			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d: %s", rec.Code, tc.want, rec.Body.String())
			}
		})
	}

	verificationCases := []struct {
		name string
		body string
		want int
	}{
		{"nil body", "<nil>", http.StatusBadRequest},
		{"empty body", "", http.StatusBadRequest},
		{"json null", "null", http.StatusBadRequest},
		{"json array", "[]", http.StatusBadRequest},
		{"trailing json", `{"conclaveId":"` + opened.Conclave.ID + `","status":"Cleared"}{}`, http.StatusBadRequest},
		{"wrong conclaveId type", `{"conclaveId":42,"status":"Cleared"}`, http.StatusBadRequest},
		{"wrong prNumber type", `{"conclaveId":"` + opened.Conclave.ID + `","status":"Cleared","prNumber":"bad"}`, http.StatusBadRequest},
		{"missing status", `{"conclaveId":"` + opened.Conclave.ID + `"}`, http.StatusBadRequest},
		{"missing conclaveId", `{"status":"Cleared"}`, http.StatusBadRequest},
		{"control char in status", fmt.Sprintf(`{"conclaveId":%q,"status":"Cleared\u001b"}`, opened.Conclave.ID), http.StatusBadRequest},
		{"overlong status", fmt.Sprintf(`{"conclaveId":%q,"status":%q}`, opened.Conclave.ID, strings.Repeat("x", maxStatusRunes+1)), http.StatusBadRequest},
		{"path traversal incidentId", fmt.Sprintf(`{"conclaveId":%q,"incidentId":"../evil","status":"Cleared"}`, opened.Conclave.ID), http.StatusBadRequest},
		{"mismatched incidentId", fmt.Sprintf(`{"conclaveId":%q,"incidentId":"INC-OTHER","status":"Cleared"}`, opened.Conclave.ID), http.StatusBadRequest},
		{"oversized body", oversizedVer, http.StatusRequestEntityTooLarge},
	}
	for _, tc := range verificationCases {
		t.Run("verification/"+tc.name, func(t *testing.T) {
			rec := postRaw(sw.VerificationHandler(), ShadewrightVerificationPath, "Bearer "+testToken, tc.body)
			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d: %s", rec.Code, tc.want, rec.Body.String())
			}
		})
	}
}

func TestShadewrightTokenRotationAndDuplicateHeaders(t *testing.T) {
	st := newTestStore(t)
	bundle := newTestBundle(st, t, domain.ConclaveWards{})
	sec := secret.NewMemoryStore(map[string]string{ShadewrightTokenSecretName: testToken})
	getBundle := func() *config.Bundle { return bundle }
	broker := conclave.NewBroker(conclave.Options{Store: st, GetBundle: getBundle})
	sw := NewShadewright(ShadewrightOptions{Store: st, Secrets: sec, GetBundle: getBundle, Broker: broker, DefaultTemplate: "primary"})

	// Duplicate Authorization headers fail closed even if one matches.
	req := httptest.NewRequest(http.MethodPost, ShadewrightIncidentPath, strings.NewReader(`{"incidentId":"INC-DUP","summary":"x"}`))
	req.Header.Add("Authorization", "Bearer "+testToken)
	req.Header.Add("Authorization", "Bearer other")
	rec := httptest.NewRecorder()
	sw.IncidentHandler().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("duplicate auth header status = %d, want 401", rec.Code)
	}

	// Trailing space on token fails closed.
	rec = postRaw(sw.IncidentHandler(), ShadewrightIncidentPath, "Bearer "+testToken+" ", `{"incidentId":"INC-SPACE","summary":"x"}`)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("trailing space token status = %d, want 401", rec.Code)
	}

	// Token rotation revokes the previous token immediately without restarting the handler.
	if err := sec.Set(context.Background(), ShadewrightTokenSecretName, "rotated-token-v2"); err != nil {
		t.Fatal(err)
	}
	rec = postRaw(sw.IncidentHandler(), ShadewrightIncidentPath, "Bearer "+testToken, `{"incidentId":"INC-ROT-1","summary":"x"}`)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("old token after rotation status = %d, want 401", rec.Code)
	}
	rec = postRaw(sw.IncidentHandler(), ShadewrightIncidentPath, "Bearer rotated-token-v2", `{"incidentId":"INC-ROT-1","summary":"x"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("new token after rotation status = %d, want 201: %s", rec.Code, rec.Body.String())
	}

	// Deleting the secret fails closed immediately.
	if err := sec.Delete(context.Background(), ShadewrightTokenSecretName); err != nil {
		t.Fatal(err)
	}
	rec = postRaw(sw.IncidentHandler(), ShadewrightIncidentPath, "Bearer rotated-token-v2", `{"incidentId":"INC-ROT-2","summary":"x"}`)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("deleted token status = %d, want 401", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "rotated-token-v2") {
		t.Fatal("error response leaked bearer token")
	}
}

func TestRejectShadewrightTokenOnOtherRoutesCaseInsensitiveBearer(t *testing.T) {
	sec := secret.NewMemoryStore(map[string]string{ShadewrightTokenSecretName: testToken})
	ok := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	h := RejectShadewrightTokenOnOtherRoutes(ok, sec)

	for _, tc := range []struct {
		name   string
		method string
		path   string
		auth   []string
		want   int
	}{
		{"no auth on other route passes", http.MethodGet, "/api/v1/workloads", nil, http.StatusOK},
		{"other token on other route passes", http.MethodGet, "/api/v1/workloads", []string{"Bearer op-token"}, http.StatusOK},
		{"exact bearer rejected", http.MethodGet, "/api/v1/workloads", []string{"Bearer " + testToken}, http.StatusUnauthorized},
		{"lowercase bearer rejected", http.MethodPost, "/api/v1/conclaves/c1/decree", []string{"bearer " + testToken}, http.StatusUnauthorized},
		{"padded bearer rejected", http.MethodGet, "/healthz", []string{"BEARER   " + testToken + "  "}, http.StatusUnauthorized},
		{"second header carrying token rejected", http.MethodGet, "/api/v1/tasks", []string{"Bearer op-token", "Bearer " + testToken}, http.StatusUnauthorized},
		{"integration incident route allowed through middleware", http.MethodPost, ShadewrightIncidentPath, []string{"Bearer " + testToken}, http.StatusOK},
		{"integration verification route allowed through middleware", http.MethodPost, ShadewrightVerificationPath, []string{"Bearer " + testToken}, http.StatusOK},
		{"GET on integration path rejected", http.MethodGet, ShadewrightIncidentPath, []string{"Bearer " + testToken}, http.StatusUnauthorized},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, tc.path, nil)
			for _, a := range tc.auth {
				req.Header.Add("Authorization", a)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d", rec.Code, tc.want)
			}
		})
	}
}

func TestShadewrightRedactsSecretsInPersistedRecords(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	bundle := newTestBundle(st, t, domain.ConclaveWards{})
	sec := secret.NewMemoryStore(map[string]string{ShadewrightTokenSecretName: testToken})
	getBundle := func() *config.Bundle { return bundle }
	broker := conclave.NewBroker(conclave.Options{Store: st, GetBundle: getBundle})
	sw := NewShadewright(ShadewrightOptions{Store: st, Secrets: sec, GetBundle: getBundle, Broker: broker, DefaultTemplate: "primary"})

	const leaked = "sk-1234567890abcdefghij123456"
	rec := postRaw(sw.IncidentHandler(), ShadewrightIncidentPath, "Bearer "+testToken, fmt.Sprintf(
		`{"incidentId":"INC-REDACT","namespace":"ns-%s","workload":"wl","summary":"Found token %s","logs":"api_key=%s"}`,
		leaked, leaked, leaked,
	))
	if rec.Code != http.StatusCreated {
		t.Fatalf("incident status = %d: %s", rec.Code, rec.Body.String())
	}
	var opened struct {
		Conclave domain.ConclaveSession `json:"conclave"`
		Relic    domain.ConclaveRelic   `json:"relic"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &opened); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(opened.Conclave.Title, leaked) || strings.Contains(opened.Relic.Body, leaked) {
		t.Fatalf("secret leaked in incident conclave or relic: title=%q relic=%q", opened.Conclave.Title, opened.Relic.Body)
	}
	msgs, err := st.ListConclaveMessages(ctx, opened.Conclave.ID, 0)
	if err != nil || len(msgs) != 1 || strings.Contains(msgs[0].Body, leaked) {
		t.Fatalf("secret leaked in incident message: %+v, err=%v", msgs, err)
	}

	verRec := postRaw(sw.VerificationHandler(), ShadewrightVerificationPath, "Bearer "+testToken, fmt.Sprintf(
		`{"conclaveId":%q,"incidentId":"INC-REDACT","status":"Failed","summary":"Error with %s"}`,
		opened.Conclave.ID, leaked,
	))
	if verRec.Code != http.StatusCreated {
		t.Fatalf("verification status = %d: %s", verRec.Code, verRec.Body.String())
	}
	msgs, err = st.ListConclaveMessages(ctx, opened.Conclave.ID, 0)
	if err != nil || len(msgs) != 2 || strings.Contains(msgs[1].Body, leaked) {
		t.Fatalf("secret leaked in verification message: %+v, err=%v", msgs, err)
	}
}

type verificationFaultStore struct {
	store.Store
	mu      sync.Mutex
	failure string
}

func (s *verificationFaultStore) PutConclaveRelic(ctx context.Context, relic domain.ConclaveRelic) error {
	s.mu.Lock()
	f := s.failure
	s.mu.Unlock()
	if f == "relic" {
		return errors.New("relic write failed")
	}
	return s.Store.PutConclaveRelic(ctx, relic)
}

func (s *verificationFaultStore) AppendConclaveMessage(ctx context.Context, msg domain.ConclaveMessage) (int64, error) {
	s.mu.Lock()
	f := s.failure
	s.mu.Unlock()
	if f == "message" {
		return 0, errors.New("message write failed")
	}
	return s.Store.AppendConclaveMessage(ctx, msg)
}

func (s *verificationFaultStore) AppendAudit(ctx context.Context, rec domain.AuditRecord) error {
	s.mu.Lock()
	f := s.failure
	s.mu.Unlock()
	if f == "audit" && rec.Action == "shadewright.verification_posted" {
		return errors.New("audit write failed")
	}
	return s.Store.AppendAudit(ctx, rec)
}

func TestVerificationRetryAfterHalfwayFailure(t *testing.T) {
	for _, fault := range []string{"relic", "message", "audit"} {
		t.Run(fault, func(t *testing.T) {
			ctx := context.Background()
			base := newTestStore(t)
			fs := &verificationFaultStore{Store: base}
			bundle := newTestBundle(base, t, domain.ConclaveWards{})
			sec := secret.NewMemoryStore(map[string]string{ShadewrightTokenSecretName: testToken})
			getBundle := func() *config.Bundle { return bundle }
			broker := conclave.NewBroker(conclave.Options{Store: fs, GetBundle: getBundle})
			sw := NewShadewright(ShadewrightOptions{Store: fs, Secrets: sec, GetBundle: getBundle, Broker: broker, DefaultTemplate: "primary"})

			openRec := postRaw(sw.IncidentHandler(), ShadewrightIncidentPath, "Bearer "+testToken, `{"incidentId":"INC-FAULT-1","summary":"Crash"}`)
			if openRec.Code != http.StatusCreated {
				t.Fatalf("open = %d: %s", openRec.Code, openRec.Body.String())
			}
			var opened struct {
				Conclave domain.ConclaveSession `json:"conclave"`
			}
			if err := json.Unmarshal(openRec.Body.Bytes(), &opened); err != nil {
				t.Fatal(err)
			}

			verBody := fmt.Sprintf(`{"conclaveId":%q,"incidentId":"INC-FAULT-1","status":"Cleared","summary":"Recovered"}`, opened.Conclave.ID)
			fs.mu.Lock()
			fs.failure = fault
			fs.mu.Unlock()
			if rec := postRaw(sw.VerificationHandler(), ShadewrightVerificationPath, "Bearer "+testToken, verBody); rec.Code != http.StatusInternalServerError {
				t.Fatalf("fault %s status = %d, want 500: %s", fault, rec.Code, rec.Body.String())
			}

			fs.mu.Lock()
			fs.failure = ""
			fs.mu.Unlock()
			if rec := postRaw(sw.VerificationHandler(), ShadewrightVerificationPath, "Bearer "+testToken, verBody); rec.Code != http.StatusCreated {
				t.Fatalf("retry after %s status = %d, want 201: %s", fault, rec.Code, rec.Body.String())
			}

			// Exactly 2 messages total (1 incident report + 1 verification report, no duplicate).
			msgs, err := base.ListConclaveMessages(ctx, opened.Conclave.ID, 0)
			if err != nil || len(msgs) != 2 {
				t.Fatalf("messages = %d, want 2, err=%v", len(msgs), err)
			}
			if msgs[1].FromRole != ShadewrightRole || msgs[1].Kind != ShadewrightReportKind || msgs[1].ToRole != "operator" {
				t.Fatalf("unexpected verification message: %+v", msgs[1])
			}
		})
	}
}

type blockingIncidentStore struct {
	store.Store
	slowID  string
	entered chan struct{}
	release chan struct{}
}

func (s *blockingIncidentStore) CreateConclave(ctx context.Context, c domain.ConclaveSession) error {
	if c.ID == s.slowID {
		close(s.entered)
		<-s.release
	}
	return s.Store.CreateConclave(ctx, c)
}

func TestPerConclaveLockDoesNotBlockDistinctIncidentsAndHandlesReload(t *testing.T) {
	base := newTestStore(t)
	bundleA := newTestBundle(base, t, domain.ConclaveWards{})
	bundleB := newTestBundle(base, t, domain.ConclaveWards{})
	var curBundle atomic.Pointer[config.Bundle]
	curBundle.Store(bundleA)
	getBundle := func() *config.Bundle { return curBundle.Load() }

	bs := &blockingIncidentStore{
		Store:   base,
		slowID:  shadewrightConclaveID("INC-SLOW"),
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}
	sec := secret.NewMemoryStore(map[string]string{ShadewrightTokenSecretName: testToken})
	rec := controlplane.NewReconciler(controlplane.Options{Store: base, Bundle: bundleA})
	broker := conclave.NewBroker(conclave.Options{Store: bs, GetBundle: getBundle, Reconciler: rec})
	sw := NewShadewright(ShadewrightOptions{Store: bs, Secrets: sec, GetBundle: getBundle, Broker: broker, Reconciler: rec, DefaultTemplate: "primary"})

	slowDone := make(chan int, 1)
	go func() {
		w := postRaw(sw.IncidentHandler(), ShadewrightIncidentPath, "Bearer "+testToken, `{"incidentId":"INC-SLOW","summary":"Slow incident"}`)
		slowDone <- w.Code
	}()
	<-bs.entered

	// Swap bundle concurrently while INC-SLOW is in flight and INC-FAST executes.
	curBundle.Store(bundleB)
	fastDone := make(chan int, 1)
	go func() {
		w := postRaw(sw.IncidentHandler(), ShadewrightIncidentPath, "Bearer "+testToken, `{"incidentId":"INC-FAST","summary":"Fast incident"}`)
		fastDone <- w.Code
	}()

	select {
	case code := <-fastDone:
		if code != http.StatusCreated {
			t.Fatalf("INC-FAST status = %d, want 201", code)
		}
	case <-time.After(2 * time.Second):
		close(bs.release)
		t.Fatal("INC-FAST was blocked by INC-SLOW lock")
	}

	close(bs.release)
	if code := <-slowDone; code != http.StatusCreated {
		t.Fatalf("INC-SLOW status = %d, want 201", code)
	}
}

func reviewHandler(st store.Store, bundle *config.Bundle, reconciler *controlplane.Reconciler) *ShadewrightHandler {
	getBundle := func() *config.Bundle { return bundle }
	return NewShadewright(ShadewrightOptions{
		Store: st, Secrets: secret.NewMemoryStore(map[string]string{ShadewrightTokenSecretName: testToken}),
		GetBundle: getBundle, Broker: conclave.NewBroker(conclave.Options{Store: st, GetBundle: getBundle, Reconciler: reconciler}),
		Reconciler: reconciler, DefaultTemplate: "primary",
	})
}

func openReviewIncident(t *testing.T, sw *ShadewrightHandler, id string) string {
	t.Helper()
	rec := postRaw(sw.IncidentHandler(), ShadewrightIncidentPath, "Bearer "+testToken, fmt.Sprintf(`{"incidentId":%q,"summary":"Crash"}`, id))
	if rec.Code != http.StatusCreated {
		t.Fatalf("incident status = %d: %s", rec.Code, rec.Body.String())
	}
	var opened struct{ Conclave domain.ConclaveSession }
	if err := json.Unmarshal(rec.Body.Bytes(), &opened); err != nil {
		t.Fatal(err)
	}
	return opened.Conclave.ID
}

func TestIncidentRejectsUnownedRoom(t *testing.T) {
	for _, withAudit := range []bool{false, true} {
		t.Run(fmt.Sprintf("audit=%t", withAudit), func(t *testing.T) {
			ctx := context.Background()
			st := newTestStore(t)
			sw := reviewHandler(st, newTestBundle(st, t, domain.ConclaveWards{}), nil)
			const incidentID = "INC-UNOWNED"
			id := shadewrightConclaveID(incidentID)
			if err := st.CreateConclave(ctx, domain.ConclaveSession{
				ID: id, TemplateName: "primary", Title: "Operator room", State: domain.ConclaveStateActive,
			}); err != nil {
				t.Fatal(err)
			}
			if withAudit {
				if err := st.AppendAudit(ctx, domain.AuditRecord{
					Category: "integration", Action: "shadewright.incident_escalated", Outcome: "ok",
					DetailJSON: fmt.Sprintf(`{"conclave_id":%q}`, id),
				}); err != nil {
					t.Fatal(err)
				}
			}
			rec := postRaw(sw.IncidentHandler(), ShadewrightIncidentPath, "Bearer "+testToken, `{"incidentId":"INC-UNOWNED","summary":"Crash"}`)
			if rec.Code != http.StatusConflict {
				t.Errorf("intake status = %d, want 409", rec.Code)
			}
			rec = postRaw(sw.VerificationHandler(), ShadewrightVerificationPath, "Bearer "+testToken, fmt.Sprintf(`{"conclaveId":%q,"status":"Cleared"}`, id))
			if rec.Code != http.StatusNotFound {
				t.Errorf("verification status = %d, want 404", rec.Code)
			}
			msgs, err := st.ListConclaveMessages(ctx, id, 0)
			if err != nil || len(msgs) != 0 {
				t.Errorf("messages = %d, want 0, err=%v", len(msgs), err)
			}
		})
	}
}

func TestIncidentIDStaysInRedactedEvidence(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	bundle := newTestBundle(st, t, domain.ConclaveWards{})
	sw := reviewHandler(st, bundle, controlplane.NewReconciler(controlplane.Options{Store: st, Bundle: bundle}))
	for _, externalID := range []string{"INC-UNTRUSTED", "sk-" + strings.Repeat("x", 24)} {
		t.Run(externalID[:3], func(t *testing.T) {
			id := openReviewIncident(t, sw, externalID)
			rec := postRaw(sw.VerificationHandler(), ShadewrightVerificationPath, "Bearer "+testToken,
				fmt.Sprintf(`{"conclaveId":%q,"incidentId":%q,"status":"Cleared"}`, id, externalID))
			if rec.Code != http.StatusCreated {
				t.Fatalf("verification status = %d", rec.Code)
			}
			sess, err := st.GetConclave(ctx, id)
			if err != nil || strings.Contains(sess.Title, externalID) {
				t.Errorf("conclave title contains external ID or read failed: %v", err)
			}
			tasks, err := st.ListTasks(ctx)
			if err != nil || len(tasks) == 0 {
				t.Fatalf("tasks = %d, err=%v", len(tasks), err)
			}
			for _, task := range tasks {
				if strings.Contains(task.InputJSON, externalID) {
					t.Error("task input contains external incident ID")
				}
			}
			relics, err := st.ListConclaveRelics(ctx, id)
			if err != nil || len(relics) != 2 {
				t.Fatalf("relics = %d, err=%v", len(relics), err)
			}
			for _, relic := range relics {
				if strings.Contains(relic.Name, externalID) {
					t.Error("relic name contains external incident ID")
				}
				want := externalID
				if strings.HasPrefix(externalID, "sk-") {
					want = "[REDACTED:openai_key]"
				}
				if !strings.Contains(relic.Body, want) || (want != externalID && strings.Contains(relic.Body, externalID)) {
					t.Error("relic body does not contain the expected redacted incident ID")
				}
			}
			records, err := st.ListAudit(ctx, 100)
			if err != nil {
				t.Fatal(err)
			}
			for _, record := range records {
				if strings.Contains(record.DetailJSON, externalID) {
					t.Errorf("%s audit contains external incident ID", record.Action)
				}
			}
		})
	}
}

func TestVerificationRelicsAreImmutable(t *testing.T) {
	ctx := context.Background()
	base := newTestStore(t)
	st := &verificationFaultStore{Store: base}
	sw := reviewHandler(st, newTestBundle(base, t, domain.ConclaveWards{}), nil)
	const incidentID = "INC-IMMUTABLE"
	id := openReviewIncident(t, sw, incidentID)
	post := func(status string) *httptest.ResponseRecorder {
		return postRaw(sw.VerificationHandler(), ShadewrightVerificationPath, "Bearer "+testToken,
			fmt.Sprintf(`{"conclaveId":%q,"incidentId":%q,"status":%q}`, id, incidentID, status))
	}
	first := post("RolledOut")
	if first.Code != http.StatusCreated {
		t.Fatalf("first report = %d: %s", first.Code, first.Body.String())
	}
	var accepted struct{ Relic domain.ConclaveRelic }
	if err := json.Unmarshal(first.Body.Bytes(), &accepted); err != nil {
		t.Fatal(err)
	}
	st.failure = "message"
	if rec := post("Cleared"); rec.Code != http.StatusInternalServerError {
		t.Fatalf("failed update = %d, want 500", rec.Code)
	}
	got, err := base.GetConclaveRelic(ctx, id, accepted.Relic.Name)
	if err != nil || got.SHA256 != accepted.Relic.SHA256 || got.Body != accepted.Relic.Body {
		t.Errorf("accepted report's relic changed after failed update, err=%v", err)
	}
	st.failure = ""
	rec := post("Cleared")
	var cleared struct{ Relic domain.ConclaveRelic }
	if err := json.Unmarshal(rec.Body.Bytes(), &cleared); err != nil || rec.Code != http.StatusCreated {
		t.Fatalf("retry = %d, err=%v", rec.Code, err)
	}
	if cleared.Relic.Name == accepted.Relic.Name {
		t.Error("distinct reports share a relic name")
	}
}

func TestVerificationAuditRetryFindsOriginalReport(t *testing.T) {
	for _, tc := range []struct {
		name, incidentField string
		intervene           bool
	}{
		{name: "omitted incident ID"},
		{name: "intervening message", incidentField: `,"incidentId":"INC-AUDIT-RETRY"`, intervene: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			base := newTestStore(t)
			st := &verificationFaultStore{Store: base}
			bundle := newTestBundle(base, t, domain.ConclaveWards{})
			sw := reviewHandler(st, bundle, nil)
			id := openReviewIncident(t, sw, "INC-AUDIT-RETRY")
			body := fmt.Sprintf(`{"conclaveId":%q,"status":"Cleared"%s}`, id, tc.incidentField)
			st.failure = "audit"
			if rec := postRaw(sw.VerificationHandler(), ShadewrightVerificationPath, "Bearer "+testToken, body); rec.Code != http.StatusInternalServerError {
				t.Fatalf("audit failure = %d, want 500", rec.Code)
			}
			before, err := base.ListConclaveMessages(ctx, id, 0)
			if err != nil || len(before) != 2 {
				t.Fatalf("messages after audit failure = %d, want 2, err=%v", len(before), err)
			}
			reportID := before[1].ID
			if tc.intervene {
				if _, err := sw.broker.PostMessage(ctx, id, "worker", "all", "review", "Checked", ""); err != nil {
					t.Fatal(err)
				}
			}
			st.failure = ""
			sw = reviewHandler(st, bundle, nil)
			for range 2 {
				rec := postRaw(sw.VerificationHandler(), ShadewrightVerificationPath, "Bearer "+testToken, body)
				var result struct{ Message domain.ConclaveMessage }
				if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil || rec.Code != http.StatusCreated {
					t.Fatalf("retry = %d, err=%v", rec.Code, err)
				}
				if result.Message.ID != reportID {
					t.Errorf("retry message ID = %d, want %d", result.Message.ID, reportID)
				}
			}
			relics, err := base.ListConclaveRelics(ctx, id)
			if err != nil || len(relics) != 2 {
				t.Errorf("relics = %d, want 2, err=%v", len(relics), err)
			}
			audits, err := base.ListAudit(ctx, 20)
			if err != nil {
				t.Fatal(err)
			}
			count := 0
			for _, audit := range audits {
				if audit.Action == "shadewright.verification_posted" {
					count++
				}
			}
			if count != 1 {
				t.Errorf("verification audit rows = %d, want 1", count)
			}
		})
	}
}

type blockedResponse struct {
	*httptest.ResponseRecorder
	entered, release chan struct{}
}

func (w blockedResponse) WriteHeader(code int) {
	close(w.entered)
	<-w.release
	w.ResponseRecorder.WriteHeader(code)
}

func TestIntegrationUnlocksBeforeResponse(t *testing.T) {
	for _, name := range []string{"incident", "verification", "verification error"} {
		t.Run(name, func(t *testing.T) {
			st := newTestStore(t)
			sw := reviewHandler(st, newTestBundle(st, t, domain.ConclaveWards{}), nil)
			path, handler := ShadewrightIncidentPath, sw.IncidentHandler()
			body := `{"incidentId":"INC-BLOCKED-RESPONSE","summary":"Crash"}`
			want := http.StatusOK
			if name != "incident" {
				id := openReviewIncident(t, sw, "INC-BLOCKED-RESPONSE")
				path, handler = ShadewrightVerificationPath, sw.VerificationHandler()
				body = fmt.Sprintf(`{"conclaveId":%q,"status":"Cleared"}`, id)
				want = http.StatusCreated
				if name == "verification error" {
					body = fmt.Sprintf(`{"conclaveId":%q,"incidentId":"INC-WRONG","status":"Cleared"}`, id)
					want = http.StatusBadRequest
				}
			}
			writer := blockedResponse{httptest.NewRecorder(), make(chan struct{}), make(chan struct{})}
			firstDone := make(chan struct{})
			secondFinished := make(chan struct{})
			defer func() { close(writer.release); <-firstDone; <-secondFinished }()
			go func() {
				defer close(firstDone)
				req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
				req.Header.Set("Authorization", "Bearer "+testToken)
				handler.ServeHTTP(writer, req)
			}()
			<-writer.entered
			secondDone := make(chan int, 1)
			go func() {
				defer close(secondFinished)
				secondDone <- postRaw(handler, path, "Bearer "+testToken, body).Code
			}()
			select {
			case got := <-secondDone:
				if got != want {
					t.Errorf("second request = %d, want %d", got, want)
				}
			case <-time.After(time.Second):
				t.Error("second request blocked behind the first response")
			}
		})
	}
}

func TestIntegrationCompactEvidence(t *testing.T) {
	st := newTestStore(t)
	sw := reviewHandler(st, newTestBundle(st, t, domain.ConclaveWards{}), nil)
	const depth = 1100
	nested := strings.Repeat("[", depth) + "0" + strings.Repeat("]", depth)
	id := shadewrightConclaveID("INC-COMPACT")
	for _, tc := range []struct {
		path string
		h    http.Handler
		body string
	}{
		{ShadewrightIncidentPath, sw.IncidentHandler(), `{"incidentId":"INC-COMPACT","summary":"Crash","evidence":{"nested":` + nested + `}}`},
		{ShadewrightVerificationPath, sw.VerificationHandler(), fmt.Sprintf(`{"conclaveId":%q,"status":"Cleared","report":{"nested":%s}}`, id, nested)},
	} {
		t.Run(tc.path, func(t *testing.T) {
			rec := postRaw(tc.h, tc.path, "Bearer "+testToken, tc.body)
			var result struct{ Relic domain.ConclaveRelic }
			if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil || rec.Code != http.StatusCreated {
				t.Fatalf("status = %d, err=%v", rec.Code, err)
			}
			if len(result.Relic.Body) > len(tc.body)+256 || !strings.Contains(result.Relic.Body, nested) {
				t.Errorf("evidence expanded from %d to %d bytes", len(tc.body), len(result.Relic.Body))
			}
		})
	}
}

func TestIntegrationPreservesEvidenceNumbers(t *testing.T) {
	st := newTestStore(t)
	sw := reviewHandler(st, newTestBundle(st, t, domain.ConclaveWards{}), nil)
	id := shadewrightConclaveID("INC-NUMBER")
	for _, tc := range []struct {
		path string
		h    http.Handler
		body string
	}{
		{ShadewrightIncidentPath, sw.IncidentHandler(), `{"incidentId":"INC-NUMBER","summary":"Crash","evidence":{"value":9007199254740993}}`},
		{ShadewrightVerificationPath, sw.VerificationHandler(), fmt.Sprintf(`{"conclaveId":%q,"status":"Cleared","report":{"value":9007199254740993}}`, id)},
	} {
		t.Run(tc.path, func(t *testing.T) {
			rec := postRaw(tc.h, tc.path, "Bearer "+testToken, tc.body)
			var result struct{ Relic domain.ConclaveRelic }
			if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil || rec.Code != http.StatusCreated {
				t.Fatalf("status = %d, err=%v", rec.Code, err)
			}
			if !strings.Contains(result.Relic.Body, "9007199254740993") {
				t.Errorf("evidence number changed: %s", result.Relic.Body)
			}
		})
	}
}

type unavailableTokenStore struct{ secret.SecretStore }

func (unavailableTokenStore) Get(context.Context, string) (string, error) {
	return "", errors.New("secret store unavailable")
}

func TestTokenScopeFailsClosedOnLookupError(t *testing.T) {
	for _, tc := range []struct {
		name, auth string
		secrets    secret.SecretStore
		want       int
	}{
		{"lookup failed", "Bearer " + testToken, unavailableTokenStore{}, http.StatusServiceUnavailable},
		{"token absent", "Bearer other-token", secret.NewMemoryStore(nil), http.StatusOK},
		{"no bearer", "", unavailableTokenStore{}, http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
			req := httptest.NewRequest(http.MethodGet, "/api/v1/tasks", nil)
			req.Header.Set("Authorization", tc.auth)
			rec := httptest.NewRecorder()
			RejectShadewrightTokenOnOtherRoutes(next, tc.secrets).ServeHTTP(rec, req)
			if rec.Code != tc.want {
				t.Errorf("status = %d, want %d", rec.Code, tc.want)
			}
		})
	}
}

func (s *verificationFaultStore) ListTasks(ctx context.Context) ([]domain.Task, error) {
	if s.failure == "task list" {
		return nil, errors.New("task list unavailable")
	}
	return s.Store.ListTasks(ctx)
}

func (s *verificationFaultStore) TaskBudgetTotals(ctx context.Context, id string) (domain.BudgetTotals, error) {
	if s.failure == "budget totals" {
		return domain.BudgetTotals{}, errors.New("budget totals unavailable")
	}
	return s.Store.TaskBudgetTotals(ctx, id)
}

func TestVerificationWardBudgetEvidence(t *testing.T) {
	for _, tc := range []struct {
		name string
		want int
	}{
		{"exhausted", http.StatusConflict},
		{"task list", http.StatusInternalServerError},
		{"budget totals", http.StatusInternalServerError},
		{"missing budget", http.StatusInternalServerError},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			base := newTestStore(t)
			st := &verificationFaultStore{Store: base}
			bundle := newTestBundle(base, t, domain.ConclaveWards{})
			tpl := bundle.Conclaves["primary"]
			tpl.Spec.BudgetRef = "cap"
			bundle.Conclaves["primary"] = tpl
			bundle.Budgets = map[string]domain.Budget{"cap": {
				Spec: domain.BudgetSpec{PerTask: domain.PerTaskBudget{MaxCostUSD: 1}},
			}}
			sw := reviewHandler(st, bundle, controlplane.NewReconciler(controlplane.Options{Store: base, Bundle: bundle}))
			id := openReviewIncident(t, sw, "INC-BUDGET")
			if tc.name == "missing budget" {
				delete(bundle.Budgets, "cap")
			}
			if tc.name == "exhausted" {
				tasks, err := base.ListTasks(ctx)
				if err != nil || len(tasks) == 0 {
					t.Fatalf("tasks = %d, err=%v", len(tasks), err)
				}
				if err := base.RecordBudgetUsage(ctx, domain.BudgetEntry{
					TaskID: tasks[0].ID, WorkloadName: tasks[0].WorkloadName,
					CostMicroUSD: 1_000_000, CreatedAt: time.Now().UTC(),
				}); err != nil {
					t.Fatal(err)
				}
			}
			st.failure = tc.name
			rec := postRaw(sw.VerificationHandler(), ShadewrightVerificationPath, "Bearer "+testToken,
				fmt.Sprintf(`{"conclaveId":%q,"status":"Cleared"}`, id))
			if rec.Code != tc.want {
				t.Errorf("verification status = %d, want %d: %s", rec.Code, tc.want, rec.Body.String())
			}
			relics, err := base.ListConclaveRelics(ctx, id)
			if err != nil || len(relics) != 1 {
				t.Errorf("relics = %d, want 1, err=%v", len(relics), err)
			}
		})
	}
}

func TestReservedIntegrationCredentials(t *testing.T) {
	for name, token := range map[string]string{"operator": "op_" + strings.Repeat("1", 64), "task": "sc_task_example"} {
		t.Run(name, func(t *testing.T) {
			h := ShadewrightHandler{secrets: secret.NewMemoryStore(map[string]string{ShadewrightTokenSecretName: token}), secretName: ShadewrightTokenSecretName}
			r := httptest.NewRequest(http.MethodPost, "/", nil)
			r.Header.Set("Authorization", "Bearer "+token)
			w := httptest.NewRecorder()
			if h.authenticate(w, r) || w.Code != 401 {
				t.Errorf("reserved credential accepted, status %d", w.Code)
			}
		})
	}
}
