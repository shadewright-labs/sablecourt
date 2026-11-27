package main

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/shadewright-labs/sablecourt/internal/approval"
	"github.com/shadewright-labs/sablecourt/internal/attach"
	"github.com/shadewright-labs/sablecourt/internal/conclave"
	"github.com/shadewright-labs/sablecourt/internal/config"
	"github.com/shadewright-labs/sablecourt/internal/controlplane"
	"github.com/shadewright-labs/sablecourt/internal/domain"
	"github.com/shadewright-labs/sablecourt/internal/policy"
	"github.com/shadewright-labs/sablecourt/internal/store"
)

// apiEnvConfig selects the optional parts of the daemon handler under test.
type apiEnvConfig struct {
	approver    approval.Approver
	reload      func() error
	noBundle    bool
	noScheduler bool
	wrap        func(store.Store) store.Store
}

type apiEnv struct {
	st        store.Store
	h         http.Handler
	approvals *approval.Queue
	hub       *attach.Hub
}

func newAPIEnv(t *testing.T, cfg apiEnvConfig) apiEnv {
	t.Helper()
	raw := daemonTestStore(t)
	ctx := context.Background()
	bundle := &config.Bundle{
		Workloads: map[string]domain.Workload{
			"coder": {
				Metadata: domain.ObjectMeta{Name: "coder", Zone: domain.ZoneRuntime},
				Spec:     domain.WorkloadSpec{Class: domain.ClassLongLivedAgent},
			},
			"trial": {
				Metadata: domain.ObjectMeta{Name: "trial", Zone: domain.ZoneSandbox},
				Spec:     domain.WorkloadSpec{Class: domain.ClassLongLivedAgent},
			},
		},
	}
	for name, w := range bundle.Workloads {
		if err := raw.UpsertWorkload(ctx, domain.WorkloadRecord{Name: name, Zone: w.Metadata.Zone, Class: w.Spec.Class, AgentCardJSON: "{}"}); err != nil {
			t.Fatal(err)
		}
	}
	st := raw
	if cfg.wrap != nil {
		st = cfg.wrap(raw)
	}
	getBundle := func() *config.Bundle { return bundle }
	if cfg.noBundle {
		getBundle = func() *config.Bundle { return nil }
	}
	pol := policy.NewEngine(nil, policy.NewCircuitBreaker(3, time.Minute))
	approvals := approval.NewQueue(st, cfg.approver)
	reconciler := controlplane.NewReconciler(controlplane.Options{Store: st, Bundle: bundle, Policy: pol})
	scheduler := controlplane.NewScheduler(reconciler)
	if cfg.noScheduler {
		scheduler = nil
	}
	broker := conclave.NewBroker(conclave.Options{Store: st, GetBundle: getBundle, Reconciler: reconciler})
	hub := attach.NewHub()
	h := newHandler(st, defaultIntegrationSecrets(), getBundle, cfg.reload, nil, nil, approvals, reconciler, scheduler, broker, hub, nil, nil,
		handlerConfig{operatorToken: testOperatorToken})
	return apiEnv{st: raw, h: h, approvals: approvals, hub: hub}
}

func apiCall(h http.Handler, method, path, body string) *httptest.ResponseRecorder {
	req := operatorRequest(method, path, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func apiError(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var body struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode error body %q: %v", rec.Body.String(), err)
	}
	return body.Error
}

func (e apiEnv) seedTask(t *testing.T, id string, state domain.TaskState) {
	t.Helper()
	now := time.Now().UTC()
	if err := e.st.CreateTask(context.Background(), domain.Task{ID: id, IdempotencyKey: id, WorkloadName: "coder", State: state, InputJSON: `{"prompt":"p"}`, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
}

func (e apiEnv) seedApproval(t *testing.T) domain.Approval {
	t.Helper()
	e.seedTask(t, "task_apr", domain.TaskStateInputRequired)
	apr, err := e.approvals.Request(context.Background(), "task_apr", "coder", "git.push", map[string]string{"branch": "main"}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	return apr
}

type daemonApprover struct {
	digest string
	err    error
}

func (a daemonApprover) Confirm(_ context.Context, req approval.ConfirmationRequest) (approval.Proof, error) {
	if a.err != nil {
		return approval.Proof{}, a.err
	}
	d := a.digest
	if d == "" {
		d = req.PayloadSHA256
	}
	return approval.Proof{Method: "test-dialog", ConfirmedAt: time.Now().UTC(), Digest: d}, nil
}

func TestAPIApproveRefusalsAndPinnedApproval(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name       string
		approver   approval.Approver
		body       func(apr domain.Approval) string
		wantCode   int
		wantErr    string
		wantStatus domain.ApprovalStatus
		wantBy     string
	}{
		{"no approver", nil, func(a domain.Approval) string { return `{"expectedSha256":"` + a.PayloadSHA256 + `"}` },
			http.StatusConflict, "no out-of-band approver configured", domain.ApprovalPending, ""},
		{"pinned digest differs", daemonApprover{}, func(domain.Approval) string { return `{"expectedSha256":"` + strings.Repeat("ab", 32) + `"}` },
			http.StatusConflict, approval.ErrDigestMismatch.Error(), domain.ApprovalPending, ""},
		{"no expected digest", daemonApprover{}, func(domain.Approval) string { return `{}` },
			http.StatusConflict, approval.ErrDigestRequired.Error(), domain.ApprovalPending, ""},
		{"approver confirms another digest", daemonApprover{digest: strings.Repeat("cd", 32)}, func(a domain.Approval) string { return `{"expectedSha256":"` + a.PayloadSHA256 + `"}` },
			http.StatusConflict, approval.ErrDigestMismatch.Error(), domain.ApprovalPending, ""},
		{"operator declines dialog", daemonApprover{err: errors.New("dialog cancelled")}, func(a domain.Approval) string { return `{"expectedSha256":"` + a.PayloadSHA256 + `"}` },
			http.StatusConflict, approval.ErrNotConfirmed.Error() + ": dialog cancelled", domain.ApprovalPending, ""},
		{"approved with default decider", daemonApprover{}, func(a domain.Approval) string { return `{"expectedSha256":"` + a.PayloadSHA256 + `"}` },
			http.StatusOK, "", domain.ApprovalApproved, "operator"},
		{"approved with named decider", daemonApprover{}, func(a domain.Approval) string {
			return `{"expectedSha256":"` + a.PayloadSHA256 + `","decidedBy":"alice"}`
		},
			http.StatusOK, "", domain.ApprovalApproved, "alice"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := newAPIEnv(t, apiEnvConfig{approver: tc.approver})
			apr := env.seedApproval(t)
			rec := apiCall(env.h, http.MethodPost, "/api/v1/approvals/"+apr.ID+"/approve", tc.body(apr))
			if rec.Code != tc.wantCode {
				t.Fatalf("status %d, want %d: %s", rec.Code, tc.wantCode, rec.Body.String())
			}
			if tc.wantErr != "" && apiError(t, rec) != tc.wantErr {
				t.Fatalf("error %q, want %q", apiError(t, rec), tc.wantErr)
			}
			got, err := env.st.GetApproval(ctx, apr.ID)
			if err != nil || got.Status != tc.wantStatus || got.DecidedBy != tc.wantBy {
				t.Fatalf("stored %q by %q (%v), want %q by %q", got.Status, got.DecidedBy, err, tc.wantStatus, tc.wantBy)
			}
		})
	}

	t.Run("second approval conflicts", func(t *testing.T) {
		env := newAPIEnv(t, apiEnvConfig{approver: daemonApprover{}})
		apr := env.seedApproval(t)
		body := `{"expectedSha256":"` + apr.PayloadSHA256 + `"}`
		if rec := apiCall(env.h, http.MethodPost, "/api/v1/approvals/"+apr.ID+"/approve", body); rec.Code != http.StatusOK {
			t.Fatalf("first approval %d", rec.Code)
		}
		rec := apiCall(env.h, http.MethodPost, "/api/v1/approvals/"+apr.ID+"/approve", body)
		if rec.Code != http.StatusConflict || apiError(t, rec) != store.ErrConflict.Error() {
			t.Fatalf("second approval %d %s", rec.Code, rec.Body.String())
		}
	})
}

func TestAPIDeviceSignedApproval(t *testing.T) {
	ctx := context.Background()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	pubPEM := string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}))
	sign := func(apr domain.Approval) string {
		digest := sha256.Sum256([]byte("sablecourt-approve:" + apr.ID + ":" + apr.PayloadSHA256))
		sig, err := ecdsa.SignASN1(rand.Reader, key, digest[:])
		if err != nil {
			t.Fatal(err)
		}
		return base64.StdEncoding.EncodeToString(sig)
	}
	for _, tc := range []struct {
		name     string
		device   string
		revoked  bool
		sig      func(apr domain.Approval) string
		wantCode int
		wantErr  string
	}{
		{"unknown device", "dev_missing", false, sign, http.StatusConflict, store.ErrNotFound.Error()},
		{"revoked device", "dev_1", true, sign, http.StatusConflict, approval.ErrDeviceRevoked.Error()},
		{"signature over other approval", "dev_1", false, func(apr domain.Approval) string {
			apr.ID = "apr_other"
			return sign(apr)
		}, http.StatusConflict, approval.ErrInvalidSignature.Error()},
		{"valid signature", "dev_1", false, sign, http.StatusOK, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := newAPIEnv(t, apiEnvConfig{})
			if err := env.st.CreateDevice(ctx, domain.PairedDevice{ID: "dev_1", Name: "phone", Platform: "ios", PublicKeyPEM: pubPEM, CreatedAt: time.Now().UTC()}); err != nil {
				t.Fatal(err)
			}
			if tc.revoked {
				if rec := apiCall(env.h, http.MethodPost, "/api/v1/devices/dev_1/revoke", ""); rec.Code != http.StatusOK {
					t.Fatalf("revoke %d %s", rec.Code, rec.Body.String())
				}
			}
			apr := env.seedApproval(t)
			body, _ := json.Marshal(map[string]string{"deviceId": tc.device, "signature": tc.sig(apr), "expectedSha256": apr.PayloadSHA256})
			rec := apiCall(env.h, http.MethodPost, "/api/v1/approvals/"+apr.ID+"/approve", string(body))
			if rec.Code != tc.wantCode {
				t.Fatalf("status %d, want %d: %s", rec.Code, tc.wantCode, rec.Body.String())
			}
			got, _ := env.st.GetApproval(ctx, apr.ID)
			if tc.wantErr != "" {
				if apiError(t, rec) != tc.wantErr || got.Status != domain.ApprovalPending {
					t.Fatalf("error %q status %q, want %q and pending", apiError(t, rec), got.Status, tc.wantErr)
				}
				return
			}
			if got.Status != domain.ApprovalApproved || got.DecidedBy != "device:dev_1" {
				t.Fatalf("stored %q by %q", got.Status, got.DecidedBy)
			}
		})
	}
}

func TestAPIDenyAndListApprovals(t *testing.T) {
	ctx := context.Background()
	env := newAPIEnv(t, apiEnvConfig{})
	apr := env.seedApproval(t)

	rec := apiCall(env.h, http.MethodGet, "/api/v1/approvals?status=pending", "")
	var pending []domain.Approval
	if err := json.Unmarshal(rec.Body.Bytes(), &pending); err != nil || rec.Code != http.StatusOK || len(pending) != 1 || pending[0].ID != apr.ID {
		t.Fatalf("pending list %d %s", rec.Code, rec.Body.String())
	}

	rec = apiCall(env.h, http.MethodPost, "/api/v1/approvals/"+apr.ID+"/deny", `{"reason":"not today"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("deny %d %s", rec.Code, rec.Body.String())
	}
	got, _ := env.st.GetApproval(ctx, apr.ID)
	if got.Status != domain.ApprovalDenied || got.DecidedBy != "operator" || !strings.Contains(got.ProofJSON, "not today") {
		t.Fatalf("stored %+v", got)
	}
	rec = apiCall(env.h, http.MethodPost, "/api/v1/approvals/apr_missing/deny", `{}`)
	if rec.Code != http.StatusConflict || apiError(t, rec) != store.ErrNotFound.Error() {
		t.Fatalf("deny missing %d %s", rec.Code, rec.Body.String())
	}
	rec = apiCall(env.h, http.MethodGet, "/api/v1/approvals?status=pending", "")
	if strings.TrimSpace(rec.Body.String()) != "[]" && strings.TrimSpace(rec.Body.String()) != "null" {
		t.Fatalf("pending after deny: %s", rec.Body.String())
	}
}

func TestAPIDevicePairingAndRevoke(t *testing.T) {
	ctx := context.Background()
	env := newAPIEnv(t, apiEnvConfig{})
	for _, p := range []string{"/api/v1/devices/pair/start", "/api/v1/devices/pair/complete"} {
		rec := apiCall(env.h, http.MethodPost, p, `{"name":"phone","publicKeyPem":"x"}`)
		if rec.Code != http.StatusServiceUnavailable || apiError(t, rec) != "device enrollment requires independent confirmation" {
			t.Fatalf("%s: %d %s", p, rec.Code, rec.Body.String())
		}
	}
	if devs, _ := env.st.ListDevices(ctx); len(devs) != 0 {
		t.Fatalf("pairing route stored devices: %+v", devs)
	}
	if err := env.st.CreateDevice(ctx, domain.PairedDevice{ID: "dev_9", Name: "laptop", Platform: "macos", PublicKeyPEM: "pem", CreatedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	rec := apiCall(env.h, http.MethodGet, "/api/v1/devices", "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"dev_9"`) {
		t.Fatalf("list %d %s", rec.Code, rec.Body.String())
	}
	rec = apiCall(env.h, http.MethodPost, "/api/v1/devices/dev_none/revoke", "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("revoke missing %d", rec.Code)
	}
	rec = apiCall(env.h, http.MethodPost, "/api/v1/devices/dev_9/revoke", "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"revoked"`) {
		t.Fatalf("revoke %d %s", rec.Code, rec.Body.String())
	}
	if d, err := env.st.GetDevice(ctx, "dev_9"); err != nil || d.RevokedAt == nil {
		t.Fatalf("device not revoked: %+v %v", d, err)
	}
}

func TestAPIDeviceRoutesRequireOperator(t *testing.T) {
	env := newAPIEnv(t, apiEnvConfig{approver: daemonApprover{}})
	apr := env.seedApproval(t)
	for _, tc := range []struct{ method, path, token string }{
		{http.MethodPost, "/api/v1/approvals/" + apr.ID + "/approve", ""},
		{http.MethodPost, "/api/v1/approvals/" + apr.ID + "/approve", testObserverToken},
		{http.MethodPost, "/api/v1/approvals/" + apr.ID + "/deny", "op_" + strings.Repeat("0", 64)},
		{http.MethodPost, "/api/v1/devices/pair/start", ""},
		{http.MethodPost, "/api/v1/tasks", testObserverToken},
		{http.MethodPost, "/api/v1/budget", ""},
	} {
		t.Run(tc.method+tc.path+tc.token, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(`{}`))
			if tc.token != "" {
				req.Header.Set("Authorization", "Bearer "+tc.token)
			}
			rec := httptest.NewRecorder()
			env.h.ServeHTTP(rec, req)
			if rec.Code != http.StatusUnauthorized || apiError(t, rec) != "operator credentials required" {
				t.Fatalf("status %d %s", rec.Code, rec.Body.String())
			}
		})
	}
	got, _ := env.st.GetApproval(context.Background(), apr.ID)
	if got.Status != domain.ApprovalPending {
		t.Fatalf("unauthenticated request changed approval to %q", got.Status)
	}
}

func TestAPIWriteGuards(t *testing.T) {
	env := newAPIEnv(t, apiEnvConfig{})
	big := `{"workload":"coder","inputJson":"` + strings.Repeat("a", maxOperatorWriteBody) + `"}`
	for _, tc := range []struct {
		name     string
		origin   string
		fetch    string
		body     string
		wantCode int
		wantErr  string
	}{
		{"cross-site fetch metadata", "", "cross-site", `{"workload":"coder"}`, http.StatusForbidden, "cross-origin request blocked"},
		{"foreign origin", "https://attacker.invalid", "", `{"workload":"coder"}`, http.StatusForbidden, "cross-origin request blocked"},
		{"oversize body", "", "", big, http.StatusBadRequest, "http: request body too large"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := operatorRequest(http.MethodPost, "/api/v1/tasks", strings.NewReader(tc.body))
			if tc.origin != "" {
				req.Header.Set("Origin", tc.origin)
			}
			if tc.fetch != "" {
				req.Header.Set("Sec-Fetch-Site", tc.fetch)
			}
			rec := httptest.NewRecorder()
			env.h.ServeHTTP(rec, req)
			if rec.Code != tc.wantCode || apiError(t, rec) != tc.wantErr {
				t.Fatalf("status %d %q, want %d %q", rec.Code, apiError(t, rec), tc.wantCode, tc.wantErr)
			}
			if tasks, _ := env.st.ListTasks(context.Background()); len(tasks) != 0 {
				t.Fatalf("refused request created tasks: %d", len(tasks))
			}
		})
	}
}

func TestAPITaskRoutes(t *testing.T) {
	ctx := context.Background()
	env := newAPIEnv(t, apiEnvConfig{})
	env.seedTask(t, "task_live", domain.TaskStateWorking)
	env.seedTask(t, "task_failed", domain.TaskStateFailed)

	for _, tc := range []struct {
		name, method, path, body string
		wantCode                 int
		wantErr                  string
	}{
		{"submit bad json", http.MethodPost, "/api/v1/tasks", `{"workload":`, http.StatusBadRequest, "unexpected EOF"},
		{"submit wrong type", http.MethodPost, "/api/v1/tasks", `{"workload":7}`, http.StatusBadRequest, ""},
		{"submit unknown workload", http.MethodPost, "/api/v1/tasks", `{"workload":"ghost"}`, http.StatusBadRequest, `unknown workload "ghost"`},
		{"submit missing workload", http.MethodPost, "/api/v1/tasks", `{}`, http.StatusBadRequest, `unknown workload ""`},
		{"submit reserved workflow key", http.MethodPost, "/api/v1/tasks", `{"workload":"coder","idempotencyKey":"wf:x"}`, http.StatusBadRequest, ""},
		{"get unknown", http.MethodGet, "/api/v1/tasks/task_none", "", http.StatusNotFound, "task not found"},
		{"cancel unknown", http.MethodPost, "/api/v1/tasks/task_none/cancel", "", http.StatusConflict, ""},
		{"suspend unknown", http.MethodPost, "/api/v1/tasks/task_none/suspend", "", http.StatusConflict, ""},
		{"resume not suspended", http.MethodPost, "/api/v1/tasks/task_live/resume", "", http.StatusConflict, ""},
		{"retry unknown", http.MethodPost, "/api/v1/tasks/task_none/retry", "", http.StatusNotFound, store.ErrNotFound.Error()},
		{"retry live task", http.MethodPost, "/api/v1/tasks/task_live/retry", "", http.StatusBadRequest, store.ErrConflict.Error() + ": retry requires a terminal task"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := apiCall(env.h, tc.method, tc.path, tc.body)
			if rec.Code != tc.wantCode {
				t.Fatalf("status %d, want %d: %s", rec.Code, tc.wantCode, rec.Body.String())
			}
			msg := apiError(t, rec)
			if msg == "" || (tc.wantErr != "" && msg != tc.wantErr) {
				t.Fatalf("error %q, want %q", msg, tc.wantErr)
			}
		})
	}
	if tasks, _ := env.st.ListTasks(ctx); len(tasks) != 2 {
		t.Fatalf("refused requests changed the task list: %d tasks", len(tasks))
	}

	t.Run("submit and read back", func(t *testing.T) {
		rec := apiCall(env.h, http.MethodPost, "/api/v1/tasks", `{"workload":"coder","idempotencyKey":"k-1","inputJson":"{\"prompt\":\"hi\"}"}`)
		var task domain.Task
		if rec.Code != http.StatusCreated || json.Unmarshal(rec.Body.Bytes(), &task) != nil || task.State != domain.TaskStateSubmitted {
			t.Fatalf("submit %d %s", rec.Code, rec.Body.String())
		}
		again := apiCall(env.h, http.MethodPost, "/api/v1/tasks", `{"workload":"coder","idempotencyKey":"k-1","inputJson":"{}"}`)
		var dup domain.Task
		if again.Code != http.StatusCreated || json.Unmarshal(again.Body.Bytes(), &dup) != nil || dup.ID != task.ID {
			t.Fatalf("idempotent resubmit returned %d %s, want task %s", again.Code, again.Body.String(), task.ID)
		}
		rec = apiCall(env.h, http.MethodGet, "/api/v1/tasks/"+task.ID, "")
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"events"`) || !strings.Contains(rec.Body.String(), task.ID) {
			t.Fatalf("get %d %s", rec.Code, rec.Body.String())
		}
		rec = apiCall(env.h, http.MethodGet, "/api/v1/tasks", "")
		var all []domain.Task
		if json.Unmarshal(rec.Body.Bytes(), &all) != nil || len(all) != 3 {
			t.Fatalf("list %s", rec.Body.String())
		}
	})

	t.Run("retry failed task with handoff", func(t *testing.T) {
		rec := apiCall(env.h, http.MethodPost, "/api/v1/tasks/task_failed/retry?handoff=true", "")
		var retried domain.Task
		if rec.Code != http.StatusCreated || json.Unmarshal(rec.Body.Bytes(), &retried) != nil || retried.ID == "task_failed" || retried.WorkloadName != "coder" {
			t.Fatalf("retry %d %s", rec.Code, rec.Body.String())
		}
		audit, _ := env.st.ListAudit(ctx, 10)
		found := false
		for _, a := range audit {
			if a.Action == "task.retry" && a.TaskID == retried.ID && strings.Contains(a.DetailJSON, `"retried_from":"task_failed","handoff":true`) {
				found = true
			}
		}
		if !found {
			t.Fatalf("retry audit missing: %+v", audit)
		}
	})

	t.Run("cancel live task", func(t *testing.T) {
		rec := apiCall(env.h, http.MethodPost, "/api/v1/tasks/task_live/cancel", "")
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"cancelled"`) {
			t.Fatalf("cancel %d %s", rec.Code, rec.Body.String())
		}
		if task, _ := env.st.GetTask(ctx, "task_live"); task.State != domain.TaskStateCancelled {
			t.Fatalf("state %q", task.State)
		}
	})
}

func TestAPIMemoryRoutes(t *testing.T) {
	ctx := context.Background()
	secretValue := "ghp_" + strings.Repeat("a1B2", 9)
	for _, tc := range []struct {
		name, method, path, body string
		wantCode                 int
		wantErr                  string
	}{
		{"bad json", http.MethodPost, "/api/v1/memory", `{"key":`, http.StatusBadRequest, "unexpected EOF"},
		{"blank key", http.MethodPost, "/api/v1/memory", `{"namespace":"n","key":"  ","value":"v"}`, http.StatusBadRequest, "key is required"},
		{"learning namespace", http.MethodPost, "/api/v1/memory", `{"namespace":" learning:repo:x","key":"k","value":"v"}`, http.StatusForbidden, "learning namespace is reserved"},
		{"delete missing", http.MethodDelete, "/api/v1/memory/default/none", "", http.StatusNotFound, store.ErrNotFound.Error()},
		{"delete learning", http.MethodDelete, "/api/v1/memory/Learning:global/k", "", http.StatusForbidden, "learning namespace is reserved"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := newAPIEnv(t, apiEnvConfig{})
			rec := apiCall(env.h, tc.method, tc.path, tc.body)
			if rec.Code != tc.wantCode || apiError(t, rec) != tc.wantErr {
				t.Fatalf("status %d %q, want %d %q", rec.Code, apiError(t, rec), tc.wantCode, tc.wantErr)
			}
			if items, _ := env.st.SearchMemory(ctx, "", "", 10); len(items) != 0 {
				t.Fatalf("refused request stored memory: %+v", items)
			}
		})
	}

	t.Run("put defaults namespace and redacts, then delete", func(t *testing.T) {
		env := newAPIEnv(t, apiEnvConfig{})
		rec := apiCall(env.h, http.MethodPost, "/api/v1/memory", `{"key":" deploy ","value":"token `+secretValue+`"}`)
		if rec.Code != http.StatusCreated {
			t.Fatalf("put %d %s", rec.Code, rec.Body.String())
		}
		got, err := env.st.GetMemory(ctx, "default", "deploy")
		if err != nil || strings.Contains(got.Value, secretValue) || !strings.HasPrefix(got.Value, "token ") || got.Author != "operator" {
			t.Fatalf("stored %+v %v", got, err)
		}
		if strings.Contains(rec.Body.String(), secretValue) {
			t.Fatal("response echoes the secret")
		}
		rec = apiCall(env.h, http.MethodGet, "/api/v1/memory?namespace=default", "")
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"deploy"`) {
			t.Fatalf("search %d %s", rec.Code, rec.Body.String())
		}
		rec = apiCall(env.h, http.MethodDelete, "/api/v1/memory/default/deploy", "")
		if rec.Code != http.StatusOK {
			t.Fatalf("delete %d", rec.Code)
		}
		if _, err := env.st.GetMemory(ctx, "default", "deploy"); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("entry still present: %v", err)
		}
	})
}

func TestAPIProviderTiersAndLearnings(t *testing.T) {
	env := newAPIEnv(t, apiEnvConfig{})
	rec := apiCall(env.h, http.MethodPost, "/api/v1/providers/tiers", `{"tier":"fast"}`)
	if rec.Code != http.StatusBadRequest || !strings.HasPrefix(apiError(t, rec), "invalid json: ") {
		t.Fatalf("bad tiers %d %s", rec.Code, rec.Body.String())
	}
	rec = apiCall(env.h, http.MethodPost, "/api/v1/providers/tiers", `[{"tier":"FAST","provider":"cli:codex","model":"m-fast"},{"tier":"bogus","provider":"x","model":"y"}]`)
	if rec.Code != http.StatusOK {
		t.Fatalf("save tiers %d %s", rec.Code, rec.Body.String())
	}
	rec = apiCall(env.h, http.MethodGet, "/api/v1/providers/tiers", "")
	var tiers []struct{ Tier, Provider, Model string }
	if err := json.Unmarshal(rec.Body.Bytes(), &tiers); err != nil {
		t.Fatal(err)
	}
	byTier := map[string]string{}
	for _, tr := range tiers {
		byTier[tr.Tier] = tr.Provider + "/" + tr.Model
	}
	if byTier["fast"] != "cli:codex/m-fast" || len(byTier) != 3 {
		t.Fatalf("tiers %v", byTier)
	}

	rec = apiCall(env.h, http.MethodPost, "/api/v1/learnings", `{"scope":"global","key":"k","rule":"r"}`)
	if rec.Code != http.StatusGone || apiError(t, rec) != "legacy learning publication is disabled" {
		t.Fatalf("learnings %d %s", rec.Code, rec.Body.String())
	}
	seedAPILearning(t, env.st, "Accepted advice for local agents")
	if err := env.st.PutMemory(context.Background(), domain.MemoryEntry{Namespace: "learning:global", Key: "legacy", Value: "Legacy advice from Court Memory", Author: "operator", UpdatedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	rec = apiCall(env.h, http.MethodGet, "/api/v1/learnings", "")
	var listed []domain.Learning
	if err := json.Unmarshal(rec.Body.Bytes(), &listed); rec.Code != http.StatusOK || err != nil {
		t.Fatalf("list learnings %d %s %v", rec.Code, rec.Body.String(), err)
	}
	if len(listed) != 1 || listed[0].Statement != "Accepted advice for local agents" || strings.Contains(rec.Body.String(), "Legacy advice") {
		t.Fatalf("listed learnings %s", rec.Body.String())
	}
	rec = apiCall(env.h, http.MethodGet, "/api/v1/providers", "")
	if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != "[]" {
		t.Fatalf("providers without registry %d %s", rec.Code, rec.Body.String())
	}
	rec = apiCall(env.h, http.MethodPost, "/api/v1/providers/openai/sync", "")
	if rec.Code != http.StatusServiceUnavailable || apiError(t, rec) != "provider registry unavailable" {
		t.Fatalf("sync without registry %d %s", rec.Code, rec.Body.String())
	}
}

// seedAPILearning proposes and accepts a global learning backed by a task artifact.
func seedAPILearning(t *testing.T, st store.Store, statement string) {
	t.Helper()
	ctx := context.Background()
	ls, err := store.Learnings(st)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertWorkload(ctx, domain.WorkloadRecord{Name: "learning-evidence", AgentCardJSON: "{}"}); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateTask(ctx, domain.Task{ID: "learning-evidence", IdempotencyKey: "learning-evidence", WorkloadName: "learning-evidence", State: domain.TaskStateCompleted, ArtifactsJSON: `[{"name":"record","text":"recorded observation"}]`}); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Add(-time.Minute)
	l, err := ls.ProposeLearning(ctx, domain.Learning{
		ID: "advice", Statement: statement, Scope: domain.LearningScope{Global: true}, SourceKind: domain.LearningSourceTask,
		SourceID: "learning-evidence", EvidenceRef: "artifact:record", Author: "operator-1", CreatedAt: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ls.AcceptLearning(store.WithLearningOperator(ctx, "operator-1"), l.ID, l.VersionDigest, now); err != nil {
		t.Fatal(err)
	}
}

func TestAPIReload(t *testing.T) {
	for _, tc := range []struct {
		name     string
		reload   func() error
		wantCode int
		wantBody string
	}{
		{"not configured", nil, http.StatusNotImplemented, `"reload not configured"`},
		{"invalid manifests", func() error { return errors.New("manifest: bad zone") }, http.StatusBadRequest, `"manifest: bad zone"`},
		{"reloaded", func() error { return nil }, http.StatusOK, `"reloaded"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := newAPIEnv(t, apiEnvConfig{reload: tc.reload})
			rec := apiCall(env.h, http.MethodPost, "/api/v1/reload", "")
			if rec.Code != tc.wantCode || !strings.Contains(rec.Body.String(), tc.wantBody) {
				t.Fatalf("status %d %s", rec.Code, rec.Body.String())
			}
		})
	}
}

func TestAPISchedulesAndPromote(t *testing.T) {
	for _, tc := range []struct {
		name     string
		cfg      apiEnvConfig
		path     string
		wantCode int
		wantErr  string
	}{
		{"run without scheduler", apiEnvConfig{noScheduler: true}, "/api/v1/schedules/nightly/run", http.StatusServiceUnavailable, "scheduler unavailable"},
		{"run without bundle", apiEnvConfig{noBundle: true}, "/api/v1/schedules/nightly/run", http.StatusServiceUnavailable, "no manifest bundle loaded"},
		{"run unknown schedule", apiEnvConfig{}, "/api/v1/schedules/nightly/run", http.StatusNotFound, "schedule not found"},
		{"promote without bundle", apiEnvConfig{noBundle: true}, "/api/v1/promote/coder", http.StatusServiceUnavailable, "no manifest bundle loaded"},
		{"promote unknown workload", apiEnvConfig{}, "/api/v1/promote/ghost", http.StatusNotFound, "workload not found"},
		{"promote runtime workload", apiEnvConfig{}, "/api/v1/promote/coder", http.StatusConflict, "workload is already in zone runtime"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := newAPIEnv(t, tc.cfg)
			rec := apiCall(env.h, http.MethodPost, tc.path, "")
			if rec.Code != tc.wantCode || apiError(t, rec) != tc.wantErr {
				t.Fatalf("status %d %q, want %d %q", rec.Code, apiError(t, rec), tc.wantCode, tc.wantErr)
			}
			if audit, _ := env.st.ListAudit(context.Background(), 5); len(audit) != 0 {
				t.Fatalf("refused request wrote audit: %+v", audit)
			}
		})
	}
	env := newAPIEnv(t, apiEnvConfig{})
	if rec := apiCall(env.h, http.MethodGet, "/api/v1/schedules", ""); rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != "null" {
		t.Fatalf("schedules %d %s", rec.Code, rec.Body.String())
	}
}

// apiFailStore fails the listing and verification calls the read routes make.
type apiFailStore struct{ store.Store }

var errAPIStore = errors.New("disk unavailable")

func (apiFailStore) ListWorkloads(context.Context) ([]domain.WorkloadRecord, error) {
	return nil, errAPIStore
}
func (apiFailStore) ListTasks(context.Context) ([]domain.Task, error) { return nil, errAPIStore }
func (apiFailStore) ListAudit(context.Context, int) ([]domain.AuditRecord, error) {
	return nil, errAPIStore
}
func (apiFailStore) ListApprovals(context.Context, domain.ApprovalStatus) ([]domain.Approval, error) {
	return nil, errAPIStore
}
func (apiFailStore) ListDevices(context.Context) ([]domain.PairedDevice, error) {
	return nil, errAPIStore
}
func (apiFailStore) ListConclaves(context.Context) ([]domain.ConclaveSession, error) {
	return nil, errAPIStore
}
func (apiFailStore) TotalBudgetTotals(context.Context) (domain.BudgetTotals, error) {
	return domain.BudgetTotals{}, errAPIStore
}
func (apiFailStore) VerifyAuditChain(context.Context) (int, error) {
	return 7, errors.New("row 8 hash mismatch")
}

func TestAPIReadRoutesReportStoreErrors(t *testing.T) {
	env := newAPIEnv(t, apiEnvConfig{wrap: func(s store.Store) store.Store { return apiFailStore{s} }})
	for _, path := range []string{"/api/v1/workloads", "/api/v1/tasks", "/api/v1/audit", "/api/v1/audit/export", "/api/v1/approvals", "/api/v1/devices", "/api/v1/conclaves", "/api/v1/budget"} {
		t.Run(path, func(t *testing.T) {
			rec := apiCall(env.h, http.MethodGet, path, "")
			if rec.Code != http.StatusInternalServerError || apiError(t, rec) != errAPIStore.Error() {
				t.Fatalf("status %d %s", rec.Code, rec.Body.String())
			}
		})
	}
	rec := apiCall(env.h, http.MethodGet, "/api/v1/audit/verify", "")
	var v struct {
		Valid        bool   `json:"valid"`
		VerifiedRows int    `json:"verifiedRows"`
		Error        string `json:"error"`
	}
	if rec.Code != http.StatusConflict || json.Unmarshal(rec.Body.Bytes(), &v) != nil || v.Valid || v.VerifiedRows != 7 || v.Error != "row 8 hash mismatch" {
		t.Fatalf("verify %d %s", rec.Code, rec.Body.String())
	}
}

func TestAPIReadRoutes(t *testing.T) {
	ctx := context.Background()
	env := newAPIEnv(t, apiEnvConfig{})
	env.seedTask(t, "task_a", domain.TaskStateWorking)
	env.seedTask(t, "task_b", domain.TaskStateCompleted)
	env.seedTask(t, "task_c", domain.TaskStateSuspended)
	for i, action := range []string{"first.action", "second.action"} {
		if err := env.st.AppendAudit(ctx, domain.AuditRecord{Category: "policy", Action: action, Outcome: "allowed", CreatedAt: time.Now().UTC().Add(time.Duration(i) * time.Second)}); err != nil {
			t.Fatal(err)
		}
	}
	if err := env.st.RecordBudgetUsage(ctx, domain.BudgetEntry{TaskID: "task_a", WorkloadName: "coder", InputTokens: 10, OutputTokens: 5, CostMicroUSD: 42, CreatedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}

	t.Run("budget", func(t *testing.T) {
		rec := apiCall(env.h, http.MethodGet, "/api/v1/budget", "")
		var b domain.BudgetTotals
		if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &b) != nil || b.CostMicroUSD != 42 || b.InputTokens != 10 {
			t.Fatalf("budget %d %s", rec.Code, rec.Body.String())
		}
	})
	t.Run("audit verify", func(t *testing.T) {
		rec := apiCall(env.h, http.MethodGet, "/api/v1/audit/verify", "")
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"valid":true`) {
			t.Fatalf("verify %d %s", rec.Code, rec.Body.String())
		}
	})
	t.Run("audit export oldest first", func(t *testing.T) {
		rec := apiCall(env.h, http.MethodGet, "/api/v1/audit/export", "")
		lines := strings.Split(strings.TrimSpace(rec.Body.String()), "\n")
		if rec.Header().Get("Content-Type") != "application/x-ndjson" || len(lines) != 2 ||
			!strings.Contains(lines[0], "first.action") || !strings.Contains(lines[1], "second.action") {
			t.Fatalf("export %q: %s", rec.Header().Get("Content-Type"), rec.Body.String())
		}
	})
	t.Run("observer snapshot lists active tasks only", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/observer/snapshot", nil)
		req.Header.Set("Authorization", "Bearer "+testObserverToken)
		rec := httptest.NewRecorder()
		env.h.ServeHTTP(rec, req)
		var snap struct {
			Source      string `json:"source"`
			ActiveTasks []struct {
				ID string `json:"id"`
			} `json:"activeTasks"`
			Budget domain.BudgetTotals `json:"budget"`
		}
		if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &snap) != nil {
			t.Fatalf("snapshot %d %s", rec.Code, rec.Body.String())
		}
		var ids []string
		for _, a := range snap.ActiveTasks {
			ids = append(ids, a.ID)
		}
		got := strings.Join(ids, ",")
		if snap.Source != "sablecourt" || !strings.Contains(got, "task_a") || !strings.Contains(got, "task_c") || strings.Contains(got, "task_b") || snap.Budget.CostMicroUSD != 42 {
			t.Fatalf("snapshot %+v", snap)
		}
	})
	t.Run("observer token cannot write", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/memory", strings.NewReader(`{"key":"k","value":"v"}`))
		req.Header.Set("Authorization", "Bearer "+testObserverToken)
		rec := httptest.NewRecorder()
		env.h.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("observer write %d", rec.Code)
		}
	})
	t.Run("workloads", func(t *testing.T) {
		rec := apiCall(env.h, http.MethodGet, "/api/v1/workloads", "")
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"coder"`) || !strings.Contains(rec.Body.String(), `"trial"`) {
			t.Fatalf("workloads %d %s", rec.Code, rec.Body.String())
		}
	})
}

func TestAPIAttachRoutesRefuseUnknownSessions(t *testing.T) {
	env := newAPIEnv(t, apiEnvConfig{})
	for _, tc := range []struct {
		name, path, body string
		wantCode         int
	}{
		{"takeover", "/api/v1/tasks/task_x/takeover", `{"operator":"me"}`, http.StatusNotFound},
		{"stdin bad json", "/api/v1/tasks/task_x/stdin", `{"data":`, http.StatusBadRequest},
		{"stdin unknown", "/api/v1/tasks/task_x/stdin", `{"operator":"me","data":"ls\n"}`, http.StatusNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := apiCall(env.h, http.MethodPost, tc.path, tc.body)
			if rec.Code != tc.wantCode || apiError(t, rec) == "" {
				t.Fatalf("status %d %s", rec.Code, rec.Body.String())
			}
		})
	}
	rec := apiCall(env.h, http.MethodGet, "/api/v1/tasks/task_x/scry", "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("scry %d", rec.Code)
	}
	if audit, _ := env.st.ListAudit(context.Background(), 5); len(audit) != 0 {
		t.Fatalf("refused attach wrote audit: %+v", audit)
	}
	rec = apiCall(env.h, http.MethodPost, "/api/v1/tasks/task_x/detach", `{"operator":"me"}`)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"detached"`) {
		t.Fatalf("detach %d %s", rec.Code, rec.Body.String())
	}
}

func TestAPIVoiceInputValidation(t *testing.T) {
	env := newAPIEnv(t, apiEnvConfig{})
	multipartBody := func(field string) (string, string) {
		var buf bytes.Buffer
		mw := multipart.NewWriter(&buf)
		_ = mw.WriteField(field, "x")
		_ = mw.Close()
		return buf.String(), mw.FormDataContentType()
	}
	noFile, noFileType := multipartBody("language")
	for _, tc := range []struct {
		name, path, ctype, body string
		wantCode                int
		wantErr                 string
	}{
		{"transcribe empty audio", "/api/v1/voice/transcribe", "audio/webm", "", http.StatusBadGateway, "audio payload is empty"},
		{"transcribe multipart without file", "/api/v1/voice/transcribe", noFileType, noFile, http.StatusBadRequest, "missing audio form field 'file': http: no such file"},
		{"transcribe broken multipart", "/api/v1/voice/transcribe", "multipart/form-data; boundary=zz", "not multipart", http.StatusBadRequest, ""},
		{"speak bad json", "/api/v1/voice/speak", "application/json", `{"text":`, http.StatusBadRequest, "unexpected EOF"},
		{"speak empty text", "/api/v1/voice/speak", "application/json", `{"text":"   "}`, http.StatusBadGateway, "text is required"},
		{"voice decree empty audio", "/api/v1/conclaves/c1/voice-decree", "audio/webm", "", http.StatusBadGateway, "audio payload is empty"},
		{"voice decree without file", "/api/v1/conclaves/c1/voice-decree", noFileType, noFile, http.StatusBadRequest, "missing audio form field 'file': http: no such file"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := operatorRequest(http.MethodPost, tc.path, strings.NewReader(tc.body))
			req.Header.Set("Content-Type", tc.ctype)
			rec := httptest.NewRecorder()
			env.h.ServeHTTP(rec, req)
			msg := apiError(t, rec)
			if rec.Code != tc.wantCode || msg == "" || (tc.wantErr != "" && msg != tc.wantErr) {
				t.Fatalf("status %d %q, want %d %q", rec.Code, msg, tc.wantCode, tc.wantErr)
			}
		})
	}
	t.Run("voice decree oversize", func(t *testing.T) {
		req := operatorRequest(http.MethodPost, "/api/v1/conclaves/c1/voice-decree", bytes.NewReader(make([]byte, maxOperatorWriteBody+1)))
		req.Header.Set("Content-Type", "audio/webm")
		rec := httptest.NewRecorder()
		env.h.ServeHTTP(rec, req)
		if rec.Code != http.StatusRequestEntityTooLarge || apiError(t, rec) != "audio payload too large" {
			t.Fatalf("status %d %s", rec.Code, rec.Body.String())
		}
	})
}

func TestAPIConclaveRoutesRefuseBadInput(t *testing.T) {
	env := newAPIEnv(t, apiEnvConfig{})
	for _, tc := range []struct {
		name, method, path, body string
		wantCode                 int
		wantErr                  string
	}{
		{"open bad json", http.MethodPost, "/api/v1/conclaves", `{"template":`, http.StatusBadRequest, "unexpected EOF"},
		{"open unknown template", http.MethodPost, "/api/v1/conclaves", `{"template":"ghost","title":"t","decree":"d"}`, http.StatusBadRequest, ""},
		{"get unknown", http.MethodGet, "/api/v1/conclaves/c_none", "", http.StatusNotFound, "conclave not found"},
		{"decree bad json", http.MethodPost, "/api/v1/conclaves/c_none/decree", `{"to":`, http.StatusBadRequest, "unexpected EOF"},
		{"decree unknown", http.MethodPost, "/api/v1/conclaves/c_none/decree", `{"to":"all","body":"go"}`, http.StatusConflict, ""},
		{"close unknown", http.MethodPost, "/api/v1/conclaves/c_none/close", `{"state":"closed"}`, http.StatusConflict, ""},
		{"relic bad json", http.MethodPost, "/api/v1/conclaves/c_none/relics", `{"name":`, http.StatusBadRequest, "unexpected EOF"},
		{"relic unknown conclave", http.MethodPost, "/api/v1/conclaves/c_none/relics", `{"name":"a.md","contentType":"text/markdown","body":"x"}`, http.StatusBadRequest, ""},
		{"relic read unknown", http.MethodGet, "/api/v1/conclaves/c_none/relics/a.md", "", http.StatusNotFound, "relic not found"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := apiCall(env.h, tc.method, tc.path, tc.body)
			msg := apiError(t, rec)
			if rec.Code != tc.wantCode || msg == "" || (tc.wantErr != "" && msg != tc.wantErr) {
				t.Fatalf("status %d %q, want %d %q", rec.Code, msg, tc.wantCode, tc.wantErr)
			}
		})
	}
	if list, _ := env.st.ListConclaves(context.Background()); len(list) != 0 {
		t.Fatalf("refused requests opened conclaves: %+v", list)
	}
}

func TestGateTaskMessagesAuthentication(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name, auth string
		state      domain.TaskState
		wantCode   int
		wantErr    string
	}{
		{"no bearer", "", domain.TaskStateWorking, http.StatusUnauthorized, "bearer task token required"},
		{"basic auth", "Basic dXNlcjpwYXNz", domain.TaskStateWorking, http.StatusUnauthorized, "bearer task token required"},
		{"wrong token", "Bearer other-token", domain.TaskStateWorking, http.StatusUnauthorized, "invalid or expired task token"},
		{"terminal task", "Bearer test-task-token", domain.TaskStateCompleted, http.StatusUnauthorized, "invalid or expired task token"},
		{"valid lease", "Bearer test-task-token", domain.TaskStateWorking, http.StatusOK, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := daemonTestStore(t)
			addMessageTestLease(t, st)
			if err := st.AppendTaskMessage(ctx, domain.TaskMessage{TaskID: "task", Role: "user", Content: "hello worker", CreatedAt: time.Now().UTC()}); err != nil {
				t.Fatal(err)
			}
			if tc.state != domain.TaskStateWorking {
				if err := st.UpdateTaskState(ctx, "task", domain.TaskStateWorking, tc.state, "done", "{}"); err != nil {
					t.Fatal(err)
				}
			}
			h := newHandler(st, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)
			req := httptest.NewRequest(http.MethodGet, "/gate/task/messages", nil)
			if tc.auth != "" {
				req.Header.Set("Authorization", tc.auth)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != tc.wantCode {
				t.Fatalf("status %d, want %d: %s", rec.Code, tc.wantCode, rec.Body.String())
			}
			if tc.wantErr != "" {
				if apiError(t, rec) != tc.wantErr || strings.Contains(rec.Body.String(), "hello worker") {
					t.Fatalf("body %s", rec.Body.String())
				}
				return
			}
			if !strings.Contains(rec.Body.String(), "hello worker") {
				t.Fatalf("messages %s", rec.Body.String())
			}
		})
	}
	t.Run("blank content refused", func(t *testing.T) {
		st := daemonTestStore(t)
		token := addMessageTestLease(t, st)
		h := newHandler(st, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)
		for _, body := range []string{`{"content":"   "}`, `{"content":`} {
			req := httptest.NewRequest(http.MethodPost, "/gate/task/messages", strings.NewReader(body))
			req.Header.Set("Authorization", "Bearer "+token)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != http.StatusBadRequest || apiError(t, rec) != "content is required" {
				t.Fatalf("%s: %d %s", body, rec.Code, rec.Body.String())
			}
		}
		if msgs, _ := st.ListTaskMessages(ctx, "task"); len(msgs) != 0 {
			t.Fatalf("refused message stored: %+v", msgs)
		}
	})
}

type stdinBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *stdinBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *stdinBuffer) Close() error { return nil }

func (b *stdinBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func TestAPITakeoverIsExclusive(t *testing.T) {
	env := newAPIEnv(t, apiEnvConfig{})
	env.seedTask(t, "task_tty", domain.TaskStateWorking)
	stdin := &stdinBuffer{}
	env.hub.Register("task_tty", stdin)
	steps := []struct {
		name, path, body string
		wantCode         int
	}{
		{"alice takes over", "/api/v1/tasks/task_tty/takeover", `{"operator":"alice"}`, http.StatusOK},
		{"bob refused takeover", "/api/v1/tasks/task_tty/takeover", `{"operator":"bob"}`, http.StatusConflict},
		{"bob refused input", "/api/v1/tasks/task_tty/stdin", `{"operator":"bob","data":"rm -rf /\n"}`, http.StatusConflict},
		{"alice writes", "/api/v1/tasks/task_tty/stdin", `{"operator":"alice","data":"ls\n"}`, http.StatusOK},
		{"bob cannot release alice", "/api/v1/tasks/task_tty/detach", `{"operator":"bob"}`, http.StatusOK},
		{"bob still refused", "/api/v1/tasks/task_tty/takeover", `{"operator":"bob"}`, http.StatusConflict},
		{"alice releases", "/api/v1/tasks/task_tty/detach", `{"operator":"alice"}`, http.StatusOK},
		{"bob takes over", "/api/v1/tasks/task_tty/takeover", `{"operator":"bob"}`, http.StatusOK},
	}
	for _, step := range steps {
		rec := apiCall(env.h, http.MethodPost, step.path, step.body)
		if rec.Code != step.wantCode {
			t.Fatalf("%s: status %d, want %d: %s", step.name, rec.Code, step.wantCode, rec.Body.String())
		}
	}
	if got := stdin.String(); got != "ls\n" {
		t.Fatalf("stdin received %q, want only alice's input", got)
	}
	audit, _ := env.st.ListAudit(context.Background(), 20)
	var acquired []string
	for _, a := range audit {
		if a.Action == "task.takeover" {
			acquired = append(acquired, a.DetailJSON)
		}
	}
	if len(acquired) != 2 {
		t.Fatalf("takeover audit %v, want two acquisitions", acquired)
	}
}

func TestAPIScryReplaysRedactedHistory(t *testing.T) {
	env := newAPIEnv(t, apiEnvConfig{})
	env.seedTask(t, "task_tty", domain.TaskStateWorking)
	env.hub.Register("task_tty", nil)
	secretValue := "ghp_" + strings.Repeat("Z9y8", 9)
	env.hub.WriteOutput("task_tty", []byte("build ok token="+secretValue+"\n"))
	env.hub.Close("task_tty")

	rec := apiCall(env.h, http.MethodGet, "/api/v1/tasks/task_tty/scry", "")
	body := rec.Body.String()
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "text/event-stream" {
		t.Fatalf("scry %d %q", rec.Code, rec.Header().Get("Content-Type"))
	}
	if !strings.Contains(body, "event: history\n") || !strings.Contains(body, "build ok") || !strings.HasSuffix(body, "event: closed\ndata: {}\n\n") {
		t.Fatalf("scry body %q", body)
	}
	if strings.Contains(body, secretValue) {
		t.Fatal("scry history leaks the token")
	}
	rec = apiCall(env.h, http.MethodPost, "/api/v1/tasks/task_tty/takeover", `{}`)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("takeover of closed session %d", rec.Code)
	}
}

// flushSignal records a response and reports the first Flush on a channel.
type flushSignal struct {
	*httptest.ResponseRecorder
	once    sync.Once
	flushed chan struct{}
}

func (f *flushSignal) Flush() {
	f.ResponseRecorder.Flush()
	f.once.Do(func() { close(f.flushed) })
}

// firstEvent runs a streaming route until it flushes once, then cancels the request.
func firstEvent(t *testing.T, h http.Handler, req *http.Request) string {
	t.Helper()
	ctx, cancel := context.WithCancel(req.Context())
	defer cancel()
	rec := &flushSignal{ResponseRecorder: httptest.NewRecorder(), flushed: make(chan struct{})}
	done := make(chan struct{})
	go func() {
		defer close(done)
		h.ServeHTTP(rec, req.WithContext(ctx))
	}()
	select {
	case <-rec.flushed:
	case <-done:
	}
	cancel()
	<-done
	return rec.Body.String()
}

func TestAPIStreamsEmitCurrentState(t *testing.T) {
	env := newAPIEnv(t, apiEnvConfig{})
	env.seedTask(t, "task_s1", domain.TaskStateWorking)
	env.seedTask(t, "task_s2", domain.TaskStateCompleted)
	if body := firstEvent(t, env.h, operatorRequest(http.MethodGet, "/api/v1/stream", nil)); !strings.HasPrefix(body, "event: state\ndata: 2:0:") {
		t.Fatalf("stream %q", body)
	}
	obs := httptest.NewRequest(http.MethodGet, "/api/v1/observer/stream", nil)
	obs.Header.Set("Authorization", "Bearer "+testObserverToken)
	if body := firstEvent(t, env.h, obs); body != "event: observer\ndata: 2:0:0\n\n" {
		t.Fatalf("observer stream %q", body)
	}
	if body := firstEvent(t, env.h, operatorRequest(http.MethodGet, "/api/v1/conclaves/c_none/stream", nil)); body != "" {
		t.Fatalf("stream of unknown conclave %q", body)
	}
}

func TestAPIConclaveLifecycle(t *testing.T) {
	ctx := context.Background()
	st, _, _, h, _ := newIntegrationTestEnv(t, domain.ConclaveWards{}, defaultIntegrationSecrets())
	rec := apiCall(h, http.MethodPost, "/api/v1/conclaves", `{"template":"primary","title":"Release review","decree":"Check the release"}`)
	var sess domain.ConclaveSession
	if rec.Code != http.StatusCreated || json.Unmarshal(rec.Body.Bytes(), &sess) != nil || sess.ID == "" {
		t.Fatalf("open %d %s", rec.Code, rec.Body.String())
	}
	base := "/api/v1/conclaves/" + sess.ID
	if rec := apiCall(h, http.MethodPost, base+"/decree", `{"to":"analyst","body":"look at the logs"}`); rec.Code != http.StatusCreated {
		t.Fatalf("decree %d %s", rec.Code, rec.Body.String())
	}
	if rec := apiCall(h, http.MethodPost, base+"/relics", `{"name":"notes.md","contentType":"text/markdown","body":"# notes"}`); rec.Code != http.StatusCreated {
		t.Fatalf("relic %d %s", rec.Code, rec.Body.String())
	}
	rec = apiCall(h, http.MethodGet, base+"/relics/notes.md", "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "# notes") {
		t.Fatalf("relic read %d %s", rec.Code, rec.Body.String())
	}
	rec = apiCall(h, http.MethodGet, base, "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "look at the logs") || !strings.Contains(rec.Body.String(), "notes.md") {
		t.Fatalf("detail %d %s", rec.Code, rec.Body.String())
	}
	if body := firstEvent(t, h, operatorRequest(http.MethodGet, base+"/stream", nil)); !strings.HasPrefix(body, "event: state\ndata: ") || !strings.HasSuffix(body, ":1\n\n") {
		t.Fatalf("conclave stream %q", body)
	}
	rec = apiCall(h, http.MethodGet, "/api/v1/conclaves", "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), sess.ID) {
		t.Fatalf("list %d %s", rec.Code, rec.Body.String())
	}
	if rec := apiCall(h, http.MethodPost, base+"/close", `{"state":"unknown-state"}`); rec.Code != http.StatusOK {
		t.Fatalf("close %d %s", rec.Code, rec.Body.String())
	}
	got, err := st.GetConclave(ctx, sess.ID)
	if err != nil || got.State != domain.ConclaveStateCompleted {
		t.Fatalf("state after close %q %v", got.State, err)
	}
	if rec := apiCall(h, http.MethodPost, base+"/decree", `{"to":"all","body":"late"}`); rec.Code != http.StatusConflict {
		t.Fatalf("decree after close %d %s", rec.Code, rec.Body.String())
	}
}

func TestAPIApproveRefusesMalformedBody(t *testing.T) {
	env := newAPIEnv(t, apiEnvConfig{approver: daemonApprover{}})
	apr := env.seedApproval(t)
	// The client pins a digest that does not match, but the body is cut short.
	rec := apiCall(env.h, http.MethodPost, "/api/v1/approvals/"+apr.ID+"/approve", `{"expectedSha256":"`+strings.Repeat("ab", 32)+`"`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status %d, want 400: %s", rec.Code, rec.Body.String())
	}
	if got, _ := env.st.GetApproval(context.Background(), apr.ID); got.Status != domain.ApprovalPending {
		t.Fatalf("malformed approval body decided the approval: %q", got.Status)
	}
}
