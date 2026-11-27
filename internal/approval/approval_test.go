package approval

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/shadewright-labs/sablecourt/internal/domain"
	"github.com/shadewright-labs/sablecourt/internal/store/sqlite"
)

type stubApprover struct {
	allow  bool
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

// The dialog shows the action's target in full ahead of free text, however
// long the free text is, and shows the full digest.
func TestConfirmationMessageShowsTargetAndDigest(t *testing.T) {
	payload := map[string]string{
		"body":  strings.Repeat("x", 10*1024),
		"title": "Fix bug",
		"owner": "acme",
		"repo":  "widgets",
		"head":  "sablecourt/task-1",
		"base":  "main",
	}
	_, digest, err := CanonicalizeAndHash(payload)
	if err != nil {
		t.Fatal(err)
	}
	canonical, _, _ := CanonicalizeAndHash(payload)
	msg := confirmationMessage(ConfirmationRequest{
		ApprovalID: "apr_1", TaskID: "task-1", WorkloadName: "triage-agent",
		Capability: "action:git.open_pr", PayloadSHA256: digest, Summary: canonical,
	})
	for _, want := range []string{`owner: "acme"`, `repo: "widgets"`, `head: "sablecourt/task-1"`, `base: "main"`, digest} {
		if !strings.Contains(msg, want) {
			t.Errorf("dialog message lacks %q:\n%s", want, msg)
		}
	}
	if strings.Index(msg, `owner: "acme"`) > strings.Index(msg, "title") {
		t.Errorf("target fields must come before free text:\n%s", msg)
	}
	if len(msg) > 2048 {
		t.Errorf("free text must be truncated, message is %d bytes", len(msg))
	}
}

// The dialogs of git.push and git.merge_pr show the approved commit SHA in
// full among the target fields, before any other field.
func TestConfirmationMessageShowsApprovedCommit(t *testing.T) {
	sha := strings.Repeat("ab", 20)
	cases := []struct {
		capability string
		payload    map[string]any
		targets    []string
	}{
		{
			capability: "action:git.push",
			payload:    map[string]any{"branch": "sablecourt/task-1", "commit_sha": sha, "remote": "origin"},
			targets:    []string{`branch: "sablecourt/task-1"`, `commit_sha: "` + sha + `"`},
		},
		{
			capability: "action:git.merge_pr",
			payload:    map[string]any{"owner": "acme", "repo": "widgets", "pr_number": 7, "commit_sha": sha, "merge_method": "squash"},
			targets:    []string{`owner: "acme"`, `repo: "widgets"`, `pr_number: "7"`, `commit_sha: "` + sha + `"`},
		},
	}
	for _, tc := range cases {
		t.Run(tc.capability, func(t *testing.T) {
			canonical, digest, err := CanonicalizeAndHash(tc.payload)
			if err != nil {
				t.Fatal(err)
			}
			msg := confirmationMessage(ConfirmationRequest{Capability: tc.capability, PayloadSHA256: digest, Summary: canonical})
			target := msg[strings.Index(msg, "\nTarget:\n"):strings.Index(msg, "\nDigest")]
			for _, want := range tc.targets {
				if !strings.Contains(target, want) {
					t.Errorf("target section lacks %q:\n%s", want, msg)
				}
			}
			if strings.Contains(target, "(missing)") {
				t.Errorf("target section has a missing field:\n%s", msg)
			}
		})
	}
}

// A target value cannot fake another line of the dialog.
func TestConfirmationMessageQuotesTargetValues(t *testing.T) {
	payload := map[string]string{"owner": "evil\nrepo: widgets", "repo": "other", "head": "sablecourt/x", "base": "main", "title": "t"}
	canonical, digest, _ := CanonicalizeAndHash(payload)
	msg := confirmationMessage(ConfirmationRequest{Capability: "action:git.open_pr", PayloadSHA256: digest, Summary: canonical})
	if strings.Contains(msg, "\nrepo: widgets") {
		t.Fatalf("newline in a value produced a fake line:\n%s", msg)
	}
}

// Both approve paths refuse an empty expected digest and leave the row pending.
func TestApproveRequiresExpectedDigest(t *testing.T) {
	ctx := context.Background()
	st := setupApprovalStore(t)
	q := NewQueue(st, stubApprover{allow: true})
	apr, err := q.Request(ctx, "task-1", "triage-agent", "action:git.open_pr", map[string]string{"head": "sablecourt/x"}, 10*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := q.Approve(ctx, apr.ID, "", "operator"); !errors.Is(err, ErrDigestRequired) {
		t.Errorf("Approve without digest: want ErrDigestRequired, got %v", err)
	}
	if _, err := q.ApproveWithSignature(ctx, apr.ID, "", "dev-unknown", "AAAA"); !errors.Is(err, ErrDigestRequired) {
		t.Errorf("ApproveWithSignature without digest: want ErrDigestRequired, got %v", err)
	}
	got, err := st.GetApproval(ctx, apr.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != domain.ApprovalPending {
		t.Fatalf("status %q, want pending", got.Status)
	}
}
