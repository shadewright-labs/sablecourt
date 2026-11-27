package approval

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/shadewright-labs/sablecourt/internal/domain"
	"github.com/shadewright-labs/sablecourt/internal/store"
)

var (
	ErrInvalidPairingCode = errors.New("invalid or expired pairing code")
	ErrInvalidSignature   = errors.New("invalid ECDSA P-256 device signature")
	ErrDeviceRevoked      = errors.New("paired device is revoked")
)

type pendingPair struct {
	ExpiresAt time.Time
}

// PairingManager issues single-use pairing codes (e.g. for QR onboarding on iOS or CLI)
// and registers ECDSA P-256 public keys in the store.
type PairingManager struct {
	store store.Store
	now   func() time.Time

	mu      sync.Mutex
	pending map[string]pendingPair
}

func NewPairingManager(st store.Store) *PairingManager {
	return &PairingManager{
		store:   st,
		now:     func() time.Time { return time.Now().UTC() },
		pending: make(map[string]pendingPair),
	}
}

func (pm *PairingManager) StartPairing(ttl time.Duration) (code string, expiresAt time.Time, err error) {
	if ttl <= 0 {
		ttl = 5 * time.Minute
	}
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", time.Time{}, err
	}
	code = "pair_" + hex.EncodeToString(raw[:])
	expiresAt = pm.now().Add(ttl)

	pm.mu.Lock()
	pm.pending[code] = pendingPair{ExpiresAt: expiresAt}
	pm.mu.Unlock()
	return code, expiresAt, nil
}

func (pm *PairingManager) CompletePairing(ctx context.Context, code, name, platform, publicKeyPEM string) (domain.PairedDevice, error) {
	code = strings.TrimSpace(code)
	pm.mu.Lock()
	entry, ok := pm.pending[code]
	if ok {
		delete(pm.pending, code)
	}
	pm.mu.Unlock()

	if !ok || pm.now().After(entry.ExpiresAt) {
		return domain.PairedDevice{}, ErrInvalidPairingCode
	}
	if _, err := parseP256PublicKey(publicKeyPEM); err != nil {
		return domain.PairedDevice{}, err
	}
	if strings.TrimSpace(name) == "" {
		name = "unnamed-device"
	}
	if strings.TrimSpace(platform) == "" {
		platform = "ios"
	}
	var rawID [6]byte
	_, _ = rand.Read(rawID[:])
	dev := domain.PairedDevice{
		ID:           "dev-" + hex.EncodeToString(rawID[:]),
		Name:         strings.TrimSpace(name),
		Platform:     strings.TrimSpace(platform),
		PublicKeyPEM: strings.TrimSpace(publicKeyPEM),
		CreatedAt:    pm.now(),
	}
	if err := pm.store.CreateDevice(ctx, dev); err != nil {
		return domain.PairedDevice{}, err
	}
	_ = pm.store.AppendAudit(ctx, domain.AuditRecord{
		Category:   "device",
		Action:     "device.paired",
		Outcome:    "paired",
		DetailJSON: fmt.Sprintf(`{"device_id":%q,"name":%q,"platform":%q}`, dev.ID, dev.Name, dev.Platform),
		CreatedAt:  dev.CreatedAt,
	})
	return dev, nil
}

// ApproveWithSignature verifies an ECDSA P-256 signature from a paired device over
// "sablecourt-approve:<approvalID>:<payloadSHA256>", enabling cryptographic out-of-band
// approvals on headless Kubernetes and iOS Secure Enclave.
func (q *Queue) ApproveWithSignature(ctx context.Context, approvalID, expectedSHA256, deviceID, sigBase64 string) (domain.Approval, error) {
	if expectedSHA256 == "" {
		return domain.Approval{}, ErrDigestRequired
	}
	dev, err := q.store.GetDevice(ctx, deviceID)
	if err != nil {
		return domain.Approval{}, err
	}
	if dev.RevokedAt != nil {
		return domain.Approval{}, ErrDeviceRevoked
	}
	apr, err := q.store.GetApproval(ctx, approvalID)
	if err != nil {
		return domain.Approval{}, err
	}
	if apr.Status != domain.ApprovalPending {
		return domain.Approval{}, store.ErrConflict
	}
	now := q.now()
	if !now.Before(apr.ExpiresAt) {
		_ = q.store.DecideApproval(ctx, apr.ID, apr.PayloadSHA256, domain.ApprovalExpired, "system", `{"reason":"expired"}`)
		return domain.Approval{}, ErrApprovalExpired
	}
	if expectedSHA256 != apr.PayloadSHA256 {
		return domain.Approval{}, ErrDigestMismatch
	}
	if err := VerifySignedApproval(dev.PublicKeyPEM, apr.ID, apr.PayloadSHA256, sigBase64); err != nil {
		return domain.Approval{}, err
	}
	proof := Proof{
		Method:      "ecdsa-p256-device",
		ConfirmedAt: now,
		Digest:      apr.PayloadSHA256,
		Detail:      dev.ID + " (" + dev.Name + ")",
	}
	proofBytes, _ := json.Marshal(proof)
	decidedBy := "device:" + dev.ID
	if err := q.store.DecideApproval(ctx, apr.ID, apr.PayloadSHA256, domain.ApprovalApproved, decidedBy, string(proofBytes)); err != nil {
		return domain.Approval{}, err
	}
	_ = q.store.AppendAudit(ctx, domain.AuditRecord{
		Category:     "approval",
		TaskID:       apr.TaskID,
		WorkloadName: apr.WorkloadName,
		Action:       apr.Capability,
		Outcome:      "approved",
		DetailJSON:   fmt.Sprintf(`{"approval_id":%q,"device_id":%q,"method":"ecdsa-p256-device"}`, apr.ID, dev.ID),
		CreatedAt:    now,
	})
	return q.store.GetApproval(ctx, apr.ID)
}

func VerifySignedApproval(pubKeyPEM, approvalID, payloadSHA256, sigBase64 string) error {
	pub, err := parseP256PublicKey(pubKeyPEM)
	if err != nil {
		return err
	}
	sig, err := base64.StdEncoding.DecodeString(strings.TrimSpace(sigBase64))
	if err != nil {
		return fmt.Errorf("%w: decode base64: %v", ErrInvalidSignature, err)
	}
	digest := sha256.Sum256([]byte("sablecourt-approve:" + approvalID + ":" + payloadSHA256))
	if !ecdsa.VerifyASN1(pub, digest[:], sig) {
		return ErrInvalidSignature
	}
	return nil
}

func parseP256PublicKey(pemStr string) (*ecdsa.PublicKey, error) {
	block, _ := pem.Decode([]byte(strings.TrimSpace(pemStr)))
	if block == nil {
		return nil, errors.New("public key must be PEM encoded")
	}
	pubAny, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse PKIX public key: %w", err)
	}
	ecPub, ok := pubAny.(*ecdsa.PublicKey)
	if !ok || ecPub.Curve != elliptic.P256() {
		return nil, errors.New("public key must be ECDSA P-256")
	}
	return ecPub, nil
}
