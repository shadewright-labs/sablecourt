package approval

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/shadewright-labs/sablecourt/internal/domain"
	"github.com/shadewright-labs/sablecourt/internal/store/sqlite"
)

type stubApprover struct {
	allow bool
	tamper bool
}

func (s stubApprover) Confirm(_ context.Context, req ConfirmationRequest) (Proof, error) {
	if !s.allow {
		return Proof{}, errors.New("user clicked Deny")
	}
	d := req.PayloadSHA256
	if s.tamper {
		d = "tampered"
	}
	return Proof{Method: "stub_oob", ConfirmedAt: time.Now().UTC(), Digest: d}, nil
}

func setupApprovalStore(t *testing.T) *sqlite.SQLiteStore {
	t.Helper()
	ctx := context.Background()
	st, err := sqlite.Open(filepath.Join(t.TempDir(), "sablecourt.db"))
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	_ = st.UpsertWorkload(ctx, domain.WorkloadRecord{
		Name: "triage-agent", Zone: domain.ZoneRuntime, Class: domain.ClassLongLivedAgent,
		ManifestDigest: "m1", AgentCardJSON: "{}",
	})
	_ = st.CreateTask(ctx, domain.Task{
		ID: "task-1", IdempotencyKey: "k1", WorkloadName: "triage-agent", State: domain.TaskStateWorking,
	})
	return st
}

func TestApprovalQueuePinnedDigestAndOOBConfirmation(t *testing.T) {
	ctx := context.Background()
	st := setupApprovalStore(t)

	q := NewQueue(st, stubApprover{allow: true})
	apr, err := q.Request(ctx, "task-1", "triage-agent", "action:git.open_pr", map[string]string{"branch": "sablecourt/task-1"}, 10*time.Minute)
	if err != nil {
		t.Fatalf("Request: %v", err)
	}

	if _, err := q.Approve(ctx, apr.ID, "wrong-sha", "operator"); !errors.Is(err, ErrDigestMismatch) {
		t.Fatalf("expected ErrDigestMismatch, got %v", err)
	}

	approved, err := q.Approve(ctx, apr.ID, apr.PayloadSHA256, "operator")
	if err != nil {
		t.Fatalf("Approve: %v", err)
	}
	if approved.Status != domain.ApprovalApproved {
		t.Fatalf("expected approved status, got %q", approved.Status)
	}
}

func TestApprovalQueueFailsClosedOnDenialOrTamperedProof(t *testing.T) {
	ctx := context.Background()
	st := setupApprovalStore(t)

	qTamper := NewQueue(st, stubApprover{allow: true, tamper: true})
	apr, err := qTamper.Request(ctx, "task-1", "triage-agent", "action:git.open_pr", map[string]string{"branch": "b2"}, 10*time.Minute)
	if err != nil {
		t.Fatalf("Request: %v", err)
	}
	if _, err := qTamper.Approve(ctx, apr.ID, apr.PayloadSHA256, "operator"); !errors.Is(err, ErrDigestMismatch) {
		t.Fatalf("expected ErrDigestMismatch on tampered proof, got %v", err)
	}

	denied, err := qTamper.Deny(ctx, apr.ID, "operator", "not needed")
	if err != nil {
		t.Fatalf("Deny: %v", err)
	}
	if denied.Status != domain.ApprovalDenied {
		t.Fatalf("expected denied, got %q", denied.Status)
	}
}
