package approval

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/shadewright-labs/sablecourt/internal/domain"
	"github.com/shadewright-labs/sablecourt/internal/store/sqlite"
)

func TestDevicePairingAndSignedApproval(t *testing.T) {
	ctx := context.Background()
	st, err := sqlite.Open(filepath.Join(t.TempDir(), "sablecourt.db"))
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	_ = st.UpsertWorkload(ctx, domain.WorkloadRecord{Name: "triage-agent", Zone: domain.ZoneRuntime, Class: domain.ClassLongLivedAgent})
	_ = st.CreateTask(ctx, domain.Task{ID: "t-sign-1", WorkloadName: "triage-agent", State: domain.TaskStateWorking, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()})

	// Generate an ECDSA P-256 keypair simulating iOS Secure Enclave.
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	der, _ := x509.MarshalPKIXPublicKey(&priv.PublicKey)
	pubPEM := string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}))

	pm := NewPairingManager(st)
	code, _, err := pm.StartPairing(5 * time.Minute)
	if err != nil {
		t.Fatalf("StartPairing: %v", err)
	}

	dev, err := pm.CompletePairing(ctx, code, "iPhone 17 Pro", "ios", pubPEM)
	if err != nil {
		t.Fatalf("CompletePairing: %v", err)
	}

	// Reusing the same pairing code must fail.
	if _, err := pm.CompletePairing(ctx, code, "Replay", "ios", pubPEM); !errors.Is(err, ErrInvalidPairingCode) {
		t.Fatalf("expected ErrInvalidPairingCode on reuse, got %v", err)
	}

	q := NewQueue(st, nil) // headless Linux/K8s: no macOS dialog approver
	apr, err := q.Request(ctx, "t-sign-1", "triage-agent", "action:git.open_pr", map[string]string{"branch": "fix-1"}, 10*time.Minute)
	if err != nil {
		t.Fatalf("Request: %v", err)
	}

	// Sign "sablecourt-approve:<id>:<sha256>".
	digest := sha256.Sum256([]byte("sablecourt-approve:" + apr.ID + ":" + apr.PayloadSHA256))
	sigDER, err := ecdsa.SignASN1(rand.Reader, priv, digest[:])
	if err != nil {
		t.Fatalf("SignASN1: %v", err)
	}
	sigB64 := base64.StdEncoding.EncodeToString(sigDER)

	// Tampered signature must fail.
	if _, err := q.ApproveWithSignature(ctx, apr.ID, apr.PayloadSHA256, dev.ID, "AAAA"); !errors.Is(err, ErrInvalidSignature) {
		t.Fatalf("expected ErrInvalidSignature, got %v", err)
	}

	// Valid signature succeeds even without macOS dialog approver.
	decided, err := q.ApproveWithSignature(ctx, apr.ID, apr.PayloadSHA256, dev.ID, sigB64)
	if err != nil {
		t.Fatalf("ApproveWithSignature: %v", err)
	}
	if decided.Status != domain.ApprovalApproved {
		t.Fatalf("expected approved status, got %s", decided.Status)
	}
}
