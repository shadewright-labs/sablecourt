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
	"strings"
	"testing"
	"time"

	"github.com/shadewright-labs/sablecourt/internal/domain"
	"github.com/shadewright-labs/sablecourt/internal/store"
	"github.com/shadewright-labs/sablecourt/internal/store/sqlite"
)

type countingApprover struct {
	calls int
	err   error
}

func (c *countingApprover) Confirm(_ context.Context, req ConfirmationRequest) (Proof, error) {
	c.calls++
	if c.err != nil {
		return Proof{}, c.err
	}
	return Proof{Method: "count", Digest: req.PayloadSHA256}, nil
}

func requestOne(t *testing.T, q *Queue) domain.Approval {
	t.Helper()
	apr, err := q.Request(context.Background(), "task-1", "triage-agent", "action:git.open_pr", map[string]string{"b": "x"}, time.Minute)
	if err != nil {
		t.Fatalf("Request: %v", err)
	}
	return apr
}

func approvalStatus(t *testing.T, st *sqlite.SQLiteStore, id string) domain.ApprovalStatus {
	t.Helper()
	got, err := st.GetApproval(context.Background(), id)
	if err != nil {
		t.Fatalf("GetApproval: %v", err)
	}
	return got.Status
}

func TestRequestDefaultsAuditAndCallback(t *testing.T) {
	ctx := context.Background()
	st := setupApprovalStore(t)
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	q := NewQueue(st, nil)
	q.now = func() time.Time { return now }
	var seen []domain.Approval
	q.SetOnRequested(func(a domain.Approval) { seen = append(seen, a) })

	apr, err := q.Request(ctx, "task-1", "triage-agent", "action:git.open_pr", map[string]int{"n": 1}, 0)
	if err != nil {
		t.Fatalf("Request: %v", err)
	}
	if !apr.ExpiresAt.Equal(now.Add(15 * time.Minute)) {
		t.Fatalf("ExpiresAt = %s, want the 15 minute default", apr.ExpiresAt)
	}
	if !strings.HasPrefix(apr.ID, "apr_") || apr.PayloadCanonicalJSON != `{"n":1}` {
		t.Fatalf("approval = %+v", apr)
	}
	if len(seen) != 1 || seen[0].ID != apr.ID {
		t.Fatalf("callback saw %+v", seen)
	}
	recs, err := st.ListAudit(ctx, 10)
	if err != nil || len(recs) == 0 {
		t.Fatalf("ListAudit = %v, %v", recs, err)
	}
	if recs[0].Outcome != "pending" || !strings.Contains(recs[0].DetailJSON, apr.ID) {
		t.Fatalf("audit record = %+v", recs[0])
	}

	if _, err := q.Request(ctx, "task-1", "triage-agent", "x", map[string]any{"c": make(chan int)}, 0); err == nil {
		t.Fatal("Request accepted a payload that cannot be marshalled")
	}
	if len(seen) != 1 {
		t.Fatal("callback ran for a failed request")
	}
}

func TestApproveRefusals(t *testing.T) {
	ctx := context.Background()

	t.Run("no approver", func(t *testing.T) {
		st := setupApprovalStore(t)
		q := NewQueue(st, nil)
		apr := requestOne(t, q)
		if _, err := q.Approve(ctx, apr.ID, "", "op"); err == nil || !strings.Contains(err.Error(), "no out-of-band approver") {
			t.Fatalf("Approve = %v", err)
		}
		if s := approvalStatus(t, st, apr.ID); s != domain.ApprovalPending {
			t.Fatalf("status = %s", s)
		}
	})

	t.Run("unknown approval", func(t *testing.T) {
		q := NewQueue(setupApprovalStore(t), &countingApprover{})
		if _, err := q.Approve(ctx, "apr_missing", "", "op"); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("Approve = %v, want ErrNotFound", err)
		}
	})

	t.Run("already decided", func(t *testing.T) {
		st := setupApprovalStore(t)
		ap := &countingApprover{}
		q := NewQueue(st, ap)
		apr := requestOne(t, q)
		if _, err := q.Approve(ctx, apr.ID, "", "op"); err != nil {
			t.Fatalf("first Approve: %v", err)
		}
		if _, err := q.Approve(ctx, apr.ID, "", "op"); !errors.Is(err, store.ErrConflict) {
			t.Fatalf("second Approve = %v, want ErrConflict", err)
		}
		if ap.calls != 1 {
			t.Fatalf("approver asked %d times, want 1", ap.calls)
		}
	})

	t.Run("expired is recorded and never confirmed", func(t *testing.T) {
		st := setupApprovalStore(t)
		ap := &countingApprover{}
		q := NewQueue(st, ap)
		apr := requestOne(t, q)
		q.now = func() time.Time { return apr.ExpiresAt }
		if _, err := q.Approve(ctx, apr.ID, apr.PayloadSHA256, "op"); !errors.Is(err, ErrApprovalExpired) {
			t.Fatalf("Approve = %v, want ErrApprovalExpired", err)
		}
		if ap.calls != 0 {
			t.Fatal("approver asked for an expired approval")
		}
		if s := approvalStatus(t, st, apr.ID); s != domain.ApprovalExpired {
			t.Fatalf("status = %s, want expired", s)
		}
	})

	t.Run("digest mismatch never reaches the approver", func(t *testing.T) {
		st := setupApprovalStore(t)
		ap := &countingApprover{}
		q := NewQueue(st, ap)
		apr := requestOne(t, q)
		if _, err := q.Approve(ctx, apr.ID, "00", "op"); !errors.Is(err, ErrDigestMismatch) {
			t.Fatalf("Approve = %v", err)
		}
		if ap.calls != 0 {
			t.Fatal("approver asked despite a digest mismatch")
		}
	})

	t.Run("operator declines", func(t *testing.T) {
		st := setupApprovalStore(t)
		q := NewQueue(st, &countingApprover{err: errors.New("cancel")})
		apr := requestOne(t, q)
		if _, err := q.Approve(ctx, apr.ID, "", "op"); !errors.Is(err, ErrNotConfirmed) {
			t.Fatalf("Approve = %v, want ErrNotConfirmed", err)
		}
		if s := approvalStatus(t, st, apr.ID); s != domain.ApprovalPending {
			t.Fatalf("status = %s, want pending", s)
		}
	})

	t.Run("approval writes proof and audit", func(t *testing.T) {
		st := setupApprovalStore(t)
		q := NewQueue(st, &countingApprover{})
		apr := requestOne(t, q)
		got, err := q.Approve(ctx, apr.ID, "", "op")
		if err != nil {
			t.Fatalf("Approve: %v", err)
		}
		if got.Status != domain.ApprovalApproved || got.DecidedBy != "op" || !strings.Contains(got.ProofJSON, `"method":"count"`) {
			t.Fatalf("approval = %+v", got)
		}
		recs, _ := st.ListAudit(ctx, 1)
		if len(recs) != 1 || recs[0].Outcome != "approved" {
			t.Fatalf("audit = %+v", recs)
		}
	})
}

func TestDenyRefusals(t *testing.T) {
	ctx := context.Background()
	st := setupApprovalStore(t)
	q := NewQueue(st, &countingApprover{})
	if _, err := q.Deny(ctx, "apr_missing", "op", "no"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("Deny unknown = %v", err)
	}
	apr := requestOne(t, q)
	if _, err := q.Approve(ctx, apr.ID, "", "op"); err != nil {
		t.Fatalf("Approve: %v", err)
	}
	if _, err := q.Deny(ctx, apr.ID, "op", "late"); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("Deny after approval = %v, want ErrConflict", err)
	}
	if s := approvalStatus(t, st, apr.ID); s != domain.ApprovalApproved {
		t.Fatalf("status = %s", s)
	}
}

func p256PEM(t *testing.T) (*ecdsa.PrivateKey, string) {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, _ := x509.MarshalPKIXPublicKey(&priv.PublicKey)
	return priv, string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}))
}

func signApproval(t *testing.T, priv *ecdsa.PrivateKey, id, sha string) string {
	t.Helper()
	d := sha256.Sum256([]byte("sablecourt-approve:" + id + ":" + sha))
	sig, err := ecdsa.SignASN1(rand.Reader, priv, d[:])
	if err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(sig)
}

func TestCompletePairingRefusals(t *testing.T) {
	ctx := context.Background()
	st := setupApprovalStore(t)
	_, goodPEM := p256PEM(t)
	p384, _ := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	der384, _ := x509.MarshalPKIXPublicKey(&p384.PublicKey)
	pem384 := string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der384}))
	garbagePEM := string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: []byte("nope")}))

	now := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	pm := NewPairingManager(st)
	pm.now = func() time.Time { return now }

	cases := []struct {
		name    string
		key     string
		wantErr string
	}{
		{"not PEM", "hello", "PEM encoded"},
		{"not PKIX", garbagePEM, "parse PKIX"},
		{"wrong curve", pem384, "ECDSA P-256"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, _, err := pm.StartPairing(time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := pm.CompletePairing(ctx, code, "n", "p", tc.key); err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("CompletePairing = %v, want %q", err, tc.wantErr)
			}
			// The code is spent even on a bad key.
			if _, err := pm.CompletePairing(ctx, code, "n", "p", goodPEM); !errors.Is(err, ErrInvalidPairingCode) {
				t.Fatalf("reuse after a failed attempt = %v", err)
			}
		})
	}

	t.Run("unknown code", func(t *testing.T) {
		if _, err := pm.CompletePairing(ctx, "pair_unknown", "n", "p", goodPEM); !errors.Is(err, ErrInvalidPairingCode) {
			t.Fatalf("CompletePairing = %v", err)
		}
	})

	t.Run("expired code", func(t *testing.T) {
		code, exp, err := pm.StartPairing(0)
		if err != nil {
			t.Fatal(err)
		}
		if !exp.Equal(now.Add(5 * time.Minute)) {
			t.Fatalf("expiry = %s, want the 5 minute default", exp)
		}
		pm.now = func() time.Time { return exp.Add(time.Nanosecond) }
		defer func() { pm.now = func() time.Time { return now } }()
		if _, err := pm.CompletePairing(ctx, code, "n", "p", goodPEM); !errors.Is(err, ErrInvalidPairingCode) {
			t.Fatalf("CompletePairing = %v", err)
		}
	})

	t.Run("blank name and platform get defaults", func(t *testing.T) {
		code, _, _ := pm.StartPairing(time.Minute)
		dev, err := pm.CompletePairing(ctx, "  "+code+"\n", " ", "", goodPEM)
		if err != nil {
			t.Fatalf("CompletePairing: %v", err)
		}
		if dev.Name != "unnamed-device" || dev.Platform != "ios" || !strings.HasPrefix(dev.ID, "dev-") {
			t.Fatalf("device = %+v", dev)
		}
		stored, err := st.GetDevice(ctx, dev.ID)
		if err != nil || stored.PublicKeyPEM != strings.TrimSpace(goodPEM) {
			t.Fatalf("stored device = %+v, %v", stored, err)
		}
	})
}

func TestApproveWithSignatureRefusals(t *testing.T) {
	ctx := context.Background()

	setup := func(t *testing.T) (*sqlite.SQLiteStore, *Queue, *ecdsa.PrivateKey, domain.PairedDevice, domain.Approval) {
		st := setupApprovalStore(t)
		priv, pubPEM := p256PEM(t)
		pm := NewPairingManager(st)
		code, _, _ := pm.StartPairing(time.Minute)
		dev, err := pm.CompletePairing(ctx, code, "phone", "ios", pubPEM)
		if err != nil {
			t.Fatal(err)
		}
		q := NewQueue(st, nil)
		return st, q, priv, dev, requestOne(t, q)
	}

	t.Run("unknown device", func(t *testing.T) {
		_, q, priv, _, apr := setup(t)
		if _, err := q.ApproveWithSignature(ctx, apr.ID, "", "dev-nope", signApproval(t, priv, apr.ID, apr.PayloadSHA256)); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("got %v", err)
		}
	})

	t.Run("revoked device", func(t *testing.T) {
		st, q, priv, dev, apr := setup(t)
		if err := st.RevokeDevice(ctx, dev.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := q.ApproveWithSignature(ctx, apr.ID, "", dev.ID, signApproval(t, priv, apr.ID, apr.PayloadSHA256)); !errors.Is(err, ErrDeviceRevoked) {
			t.Fatalf("got %v", err)
		}
		if s := approvalStatus(t, st, apr.ID); s != domain.ApprovalPending {
			t.Fatalf("status = %s", s)
		}
	})

	t.Run("unknown approval", func(t *testing.T) {
		_, q, priv, dev, _ := setup(t)
		if _, err := q.ApproveWithSignature(ctx, "apr_x", "", dev.ID, signApproval(t, priv, "apr_x", "")); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("got %v", err)
		}
	})

	t.Run("expired", func(t *testing.T) {
		st, q, priv, dev, apr := setup(t)
		q.now = func() time.Time { return apr.ExpiresAt.Add(time.Second) }
		if _, err := q.ApproveWithSignature(ctx, apr.ID, "", dev.ID, signApproval(t, priv, apr.ID, apr.PayloadSHA256)); !errors.Is(err, ErrApprovalExpired) {
			t.Fatalf("got %v", err)
		}
		if s := approvalStatus(t, st, apr.ID); s != domain.ApprovalExpired {
			t.Fatalf("status = %s, want expired", s)
		}
	})

	t.Run("digest mismatch", func(t *testing.T) {
		_, q, priv, dev, apr := setup(t)
		if _, err := q.ApproveWithSignature(ctx, apr.ID, "ff", dev.ID, signApproval(t, priv, apr.ID, apr.PayloadSHA256)); !errors.Is(err, ErrDigestMismatch) {
			t.Fatalf("got %v", err)
		}
	})

	t.Run("signature over another approval", func(t *testing.T) {
		st, q, priv, dev, apr := setup(t)
		if _, err := q.ApproveWithSignature(ctx, apr.ID, "", dev.ID, signApproval(t, priv, "apr_other", apr.PayloadSHA256)); !errors.Is(err, ErrInvalidSignature) {
			t.Fatalf("got %v", err)
		}
		if s := approvalStatus(t, st, apr.ID); s != domain.ApprovalPending {
			t.Fatalf("status = %s", s)
		}
	})

	t.Run("signature from another key", func(t *testing.T) {
		_, q, _, dev, apr := setup(t)
		other, _ := p256PEM(t)
		if _, err := q.ApproveWithSignature(ctx, apr.ID, "", dev.ID, signApproval(t, other, apr.ID, apr.PayloadSHA256)); !errors.Is(err, ErrInvalidSignature) {
			t.Fatalf("got %v", err)
		}
	})

	t.Run("signature not base64", func(t *testing.T) {
		_, q, _, dev, apr := setup(t)
		if _, err := q.ApproveWithSignature(ctx, apr.ID, "", dev.ID, "!!!"); !errors.Is(err, ErrInvalidSignature) {
			t.Fatalf("got %v", err)
		}
	})

	t.Run("empty expected digest approves once, then conflicts", func(t *testing.T) {
		st, q, priv, dev, apr := setup(t)
		sig := signApproval(t, priv, apr.ID, apr.PayloadSHA256)
		got, err := q.ApproveWithSignature(ctx, apr.ID, "", dev.ID, sig)
		if err != nil {
			t.Fatalf("ApproveWithSignature: %v", err)
		}
		if got.Status != domain.ApprovalApproved || got.DecidedBy != "device:"+dev.ID {
			t.Fatalf("approval = %+v", got)
		}
		if _, err := q.ApproveWithSignature(ctx, apr.ID, "", dev.ID, sig); !errors.Is(err, store.ErrConflict) {
			t.Fatalf("replay = %v, want ErrConflict", err)
		}
		recs, _ := st.ListAudit(ctx, 1)
		if len(recs) != 1 || !strings.Contains(recs[0].DetailJSON, "ecdsa-p256-device") {
			t.Fatalf("audit = %+v", recs)
		}
	})
}

func TestVerifySignedApprovalRejectsBadKey(t *testing.T) {
	if err := VerifySignedApproval("not a key", "a", "b", "AAAA"); err == nil || errors.Is(err, ErrInvalidSignature) {
		t.Fatalf("VerifySignedApproval with a bad key = %v, want a key error", err)
	}
}
