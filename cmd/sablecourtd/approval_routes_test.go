package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/shadewright-labs/sablecourt/internal/approval"
	"github.com/shadewright-labs/sablecourt/internal/attach"
	"github.com/shadewright-labs/sablecourt/internal/conclave"
	"github.com/shadewright-labs/sablecourt/internal/config"
	"github.com/shadewright-labs/sablecourt/internal/controlplane"
	"github.com/shadewright-labs/sablecourt/internal/domain"
	mcpgate "github.com/shadewright-labs/sablecourt/internal/gate/mcp"
	"github.com/shadewright-labs/sablecourt/internal/gate/model"
	"github.com/shadewright-labs/sablecourt/internal/policy"
	"github.com/shadewright-labs/sablecourt/internal/secret"
	"github.com/shadewright-labs/sablecourt/internal/store/sqlite"
	"github.com/shadewright-labs/sablecourt/internal/ui"
)

type routeTestApprover struct{}

func (routeTestApprover) Confirm(_ context.Context, req approval.ConfirmationRequest) (approval.Proof, error) {
	return approval.Proof{Method: "test_oob", ConfirmedAt: time.Now().UTC(), Digest: req.PayloadSHA256}, nil
}

// Both approve paths (operator dialog and device signature) refuse a request
// that does not name the digest the approver looked at.
func TestApproveRoutesRequireExpectedDigest(t *testing.T) {
	ctx := context.Background()
	st, err := sqlite.Open(filepath.Join(t.TempDir(), "sablecourt.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertWorkload(ctx, domain.WorkloadRecord{Name: "triage-agent", Zone: domain.ZoneRuntime, Class: domain.ClassLongLivedAgent, AgentCardJSON: "{}"}); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateTask(ctx, domain.Task{ID: "task-d", IdempotencyKey: "d", WorkloadName: "triage-agent", State: domain.TaskStateWorking}); err != nil {
		t.Fatal(err)
	}

	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, _ := x509.MarshalPKIXPublicKey(&priv.PublicKey)
	if err := st.CreateDevice(ctx, domain.PairedDevice{
		ID: "dev-test", Name: "test", Platform: "ios", CreatedAt: time.Now().UTC(),
		PublicKeyPEM: string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})),
	}); err != nil {
		t.Fatal(err)
	}

	bundle := &config.Bundle{}
	approvals := approval.NewQueue(st, routeTestApprover{})
	pol := policy.NewEngine(nil, nil)
	modelGate := model.New(model.Options{Store: st, Secrets: secret.NewMemoryStore(nil), Bundle: bundle})
	dataGate := mcpgate.New(mcpgate.Options{Store: st, Bundle: bundle, Policy: pol, Approvals: approvals})
	reconciler := controlplane.NewReconciler(controlplane.Options{Store: st, Bundle: bundle, Policy: pol})
	broker := conclave.NewBroker(conclave.Options{Store: st, Reconciler: reconciler, GetBundle: func() *config.Bundle { return bundle }})
	dash, err := ui.NewDashboard(ui.Options{Store: st, Approvals: approvals, Reconciler: reconciler, Conclave: broker, GetBundle: func() *config.Bundle { return bundle }})
	if err != nil {
		t.Fatal(err)
	}
	h := newHandler(st, defaultIntegrationSecrets(), func() *config.Bundle { return bundle }, nil, modelGate, dataGate, approvals, reconciler, controlplane.NewScheduler(reconciler), broker, attach.NewHub(), nil, dash, handlerConfig{operatorToken: testOperatorToken})

	request := func() domain.Approval {
		apr, err := approvals.Request(ctx, "task-d", "triage-agent", "action:git.open_pr", map[string]string{"head": "sablecourt/x"}, 10*time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		return apr
	}
	post := func(id string, body map[string]string) *httptest.ResponseRecorder {
		raw, _ := json.Marshal(body)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, operatorRequest(http.MethodPost, "/api/v1/approvals/"+id+"/approve", strings.NewReader(string(raw))))
		return rec
	}
	assertPending := func(id string) {
		t.Helper()
		got, err := st.GetApproval(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if got.Status != domain.ApprovalPending {
			t.Fatalf("approval %s is %q, want pending", id, got.Status)
		}
	}

	dialog := request()
	if rec := post(dialog.ID, map[string]string{}); rec.Code != http.StatusConflict {
		t.Fatalf("dialog path without expectedSha256: want 409, got %d: %s", rec.Code, rec.Body.String())
	}
	assertPending(dialog.ID)

	signed := request()
	sum := sha256.Sum256([]byte("sablecourt-approve:" + signed.ID + ":" + signed.PayloadSHA256))
	sig, err := ecdsa.SignASN1(rand.Reader, priv, sum[:])
	if err != nil {
		t.Fatal(err)
	}
	if rec := post(signed.ID, map[string]string{"deviceId": "dev-test", "signature": base64.StdEncoding.EncodeToString(sig)}); rec.Code != http.StatusConflict {
		t.Fatalf("device path without expectedSha256: want 409, got %d: %s", rec.Code, rec.Body.String())
	}
	assertPending(signed.ID)

	// With the digest named, both paths still work.
	if rec := post(dialog.ID, map[string]string{"expectedSha256": dialog.PayloadSHA256}); rec.Code != http.StatusOK {
		t.Fatalf("dialog path with digest: want 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if rec := post(signed.ID, map[string]string{"expectedSha256": signed.PayloadSHA256, "deviceId": "dev-test", "signature": base64.StdEncoding.EncodeToString(sig)}); rec.Code != http.StatusOK {
		t.Fatalf("device path with digest: want 200, got %d: %s", rec.Code, rec.Body.String())
	}
}
