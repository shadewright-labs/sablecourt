package approval

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/shadewright-labs/sablecourt/internal/domain"
	"github.com/shadewright-labs/sablecourt/internal/store"
)

var (
	ErrApprovalExpired = errors.New("approval expired")
	ErrDigestMismatch  = errors.New("approval payload digest mismatch")
	ErrNotConfirmed    = errors.New("out-of-band confirmation denied or cancelled")
	ErrDigestRequired  = errors.New("expected payload digest is required")
)

type ConfirmationRequest struct {
	ApprovalID    string
	TaskID        string
	WorkloadName  string
	Capability    string
	PayloadSHA256 string
	Summary       string
}

type Proof struct {
	Method      string    `json:"method"`
	ConfirmedAt time.Time `json:"confirmedAt"`
	Digest      string    `json:"digest"`
	Detail      string    `json:"detail,omitempty"`
}

// Approver performs out-of-band human confirmation that a rogue local process cannot forge.
type Approver interface {
	Confirm(ctx context.Context, req ConfirmationRequest) (Proof, error)
}

type Queue struct {
	store       store.Store
	approver    Approver
	now         func() time.Time
	onRequested func(domain.Approval)
}

func NewQueue(st store.Store, approver Approver) *Queue {
	return &Queue{
		store:    st,
		approver: approver,
		now:      func() time.Time { return time.Now().UTC() },
	}
}

func (q *Queue) SetOnRequested(fn func(domain.Approval)) {
	q.onRequested = fn
}

func CanonicalizeAndHash(payload any) (string, string, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return "", "", fmt.Errorf("marshal canonical payload: %w", err)
	}
	sum := sha256.Sum256(raw)
	return string(raw), hex.EncodeToString(sum[:]), nil
}

func (q *Queue) Request(ctx context.Context, taskID, workloadName, capability string, payload any, ttl time.Duration) (domain.Approval, error) {
	if ttl <= 0 {
		ttl = 15 * time.Minute
	}
	canonical, digest, err := CanonicalizeAndHash(payload)
	if err != nil {
		return domain.Approval{}, err
	}
	id, err := newApprovalID()
	if err != nil {
		return domain.Approval{}, err
	}
	now := q.now()
	apr := domain.Approval{
		ID:                   id,
		TaskID:               taskID,
		WorkloadName:         workloadName,
		Capability:           capability,
		PayloadCanonicalJSON: canonical,
		PayloadSHA256:        digest,
		Status:               domain.ApprovalPending,
		ExpiresAt:            now.Add(ttl),
		CreatedAt:            now,
	}
	// The request is recorded before the approval exists: a record that
	// cannot be written refuses the request and creates no approval.
	if err := store.RecordAudit(ctx, q.store, domain.AuditRecord{
		Category:     "approval",
		TaskID:       taskID,
		WorkloadName: workloadName,
		Action:       capability,
		Outcome:      "pending",
		DetailJSON:   fmt.Sprintf(`{"approval_id":%q,"payload_sha256":%q}`, apr.ID, apr.PayloadSHA256),
		CreatedAt:    now,
	}); err != nil {
		return domain.Approval{}, err
	}
	if err := q.store.CreateApproval(ctx, apr); err != nil {
		q.recordFailedStep(ctx, apr, "create_failed")
		return domain.Approval{}, err
	}
	if q.onRequested != nil {
		q.onRequested(apr)
	}
	return apr, nil
}

func (q *Queue) Approve(ctx context.Context, approvalID, expectedSHA256, decidedBy string) (domain.Approval, error) {
	if q.approver == nil {
		return domain.Approval{}, errors.New("no out-of-band approver configured")
	}
	if expectedSHA256 == "" {
		return domain.Approval{}, ErrDigestRequired
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

	proof, err := q.approver.Confirm(ctx, ConfirmationRequest{
		ApprovalID:    apr.ID,
		TaskID:        apr.TaskID,
		WorkloadName:  apr.WorkloadName,
		Capability:    apr.Capability,
		PayloadSHA256: apr.PayloadSHA256,
		Summary:       apr.PayloadCanonicalJSON,
	})
	if err != nil {
		return domain.Approval{}, fmt.Errorf("%w: %v", ErrNotConfirmed, err)
	}
	if proof.Digest != apr.PayloadSHA256 {
		return domain.Approval{}, ErrDigestMismatch
	}

	proofBytes, err := json.Marshal(proof)
	if err != nil {
		return domain.Approval{}, err
	}
	// The decision is recorded before it is stored: a record that cannot be
	// written refuses the approval and leaves it pending.
	if err := store.RecordAudit(ctx, q.store, domain.AuditRecord{
		Category:     "approval",
		TaskID:       apr.TaskID,
		WorkloadName: apr.WorkloadName,
		Action:       apr.Capability,
		Outcome:      "approved",
		DetailJSON:   fmt.Sprintf(`{"approval_id":%q,"decided_by":%q,"method":%q}`, apr.ID, decidedBy, proof.Method),
		CreatedAt:    q.now(),
	}); err != nil {
		return domain.Approval{}, err
	}
	if err := q.store.DecideApproval(ctx, apr.ID, apr.PayloadSHA256, domain.ApprovalApproved, decidedBy, string(proofBytes)); err != nil {
		q.recordFailedStep(ctx, apr, "approve_failed")
		return domain.Approval{}, err
	}
	return q.store.GetApproval(ctx, apr.ID)
}

// Deny requires no out-of-band dialog because denying cannot widen permissions.
func (q *Queue) Deny(ctx context.Context, approvalID, decidedBy, reason string) (domain.Approval, error) {
	apr, err := q.store.GetApproval(ctx, approvalID)
	if err != nil {
		return domain.Approval{}, err
	}
	// A decided approval is a conflict before any row is written, as in
	// Approve, so the log never shows a denial that cannot happen.
	if apr.Status != domain.ApprovalPending {
		return domain.Approval{}, store.ErrConflict
	}
	proofJSON, _ := json.Marshal(map[string]string{"method": "operator_deny", "reason": reason})
	// Recorded before it is stored, like an approval: a record that cannot
	// be written refuses the denial and leaves the approval pending, where it
	// cannot be used and expires.
	if err := store.RecordAudit(ctx, q.store, domain.AuditRecord{
		Category:     "approval",
		TaskID:       apr.TaskID,
		WorkloadName: apr.WorkloadName,
		Action:       apr.Capability,
		Outcome:      "denied",
		DetailJSON:   fmt.Sprintf(`{"approval_id":%q,"decided_by":%q,"reason":%q}`, apr.ID, decidedBy, reason),
		CreatedAt:    q.now(),
	}); err != nil {
		return domain.Approval{}, err
	}
	if err := q.store.DecideApproval(ctx, apr.ID, apr.PayloadSHA256, domain.ApprovalDenied, decidedBy, string(proofJSON)); err != nil {
		q.recordFailedStep(ctx, apr, "deny_failed")
		return domain.Approval{}, err
	}
	return q.store.GetApproval(ctx, apr.ID)
}

// recordFailedStep follows a row written before a store change that then
// failed, so the log does not claim a change that did not happen.
func (q *Queue) recordFailedStep(ctx context.Context, apr domain.Approval, reason string) {
	store.LogClosingAudit(ctx, q.store, domain.AuditRecord{
		Category:     "approval",
		TaskID:       apr.TaskID,
		WorkloadName: apr.WorkloadName,
		Action:       apr.Capability,
		Outcome:      "error",
		DetailJSON:   fmt.Sprintf(`{"approval_id":%q,"reason":%q}`, apr.ID, reason),
		CreatedAt:    q.now(),
	})
}

func newApprovalID() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return "apr_" + hex.EncodeToString(b[:]), nil
}
