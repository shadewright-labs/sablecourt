package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/shadewright-labs/sablecourt/internal/domain"
	"github.com/shadewright-labs/sablecourt/internal/store"
)

func TestCreateTaskWithMemory(t *testing.T) {
	r := newConclaveRig(t)
	ctx := t.Context()
	task := domain.Task{ID: "job", IdempotencyKey: "job", WorkloadName: "worker", State: domain.TaskStateSubmitted}
	entry := domain.MemoryEntry{Namespace: "system:runtime-handles", Key: "job", Value: `{"h":1}`, Author: "controlplane", UpdatedAt: r.now}

	mismatch := entry
	mismatch.Key = "other"
	if err := store.CreateTaskWithMemory(ctx, r.st, task, mismatch); err == nil {
		t.Fatal("registration for another task accepted")
	}
	if err := store.CreateTaskWithMemory(ctx, r.st, task, entry); err != nil {
		t.Fatal(err)
	}
	if got, err := r.st.GetMemory(ctx, entry.Namespace, "job"); err != nil || got.Value != entry.Value {
		t.Fatalf("registration = %+v, %v", got, err)
	}
	// A failed task insert leaves no registration behind.
	dup := domain.Task{ID: "job2", IdempotencyKey: "job", WorkloadName: "worker", State: domain.TaskStateSubmitted}
	entry2 := entry
	entry2.Key = "job2"
	if err := store.CreateTaskWithMemory(ctx, r.st, dup, entry2); err == nil {
		t.Fatal("task with a duplicate idempotency key accepted")
	}
	if _, err := r.st.GetMemory(ctx, entry.Namespace, "job2"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("registration survived the failed insert: %v", err)
	}
	// A registration cannot ride along with a conclave turn reservation.
	turn := turnTask(t, "room", "a", 0)
	r.room(t, "room", domain.ConclaveStateActive, 0)
	turnEntry := entry
	turnEntry.Key = turn.ID
	_, err := store.SubmitConclaveTurn(ctx, "room", turn.IdempotencyKey, r.limits(), func(ctx context.Context) (domain.Task, error) {
		return turn, store.CreateTaskWithMemory(ctx, r.st, turn, turnEntry)
	})
	if err == nil {
		t.Fatal("registration inside a turn reservation accepted")
	}
}

func TestClaimRecoveryReplacesOnlyTheVerifiedOwner(t *testing.T) {
	r := newConclaveRig(t)
	ctx := t.Context()
	if ok, err := r.st.ClaimRecovery(ctx, "job", "", "owner-1"); err != nil || !ok {
		t.Fatalf("first claim = %v, %v", ok, err)
	}
	if ok, err := r.st.ClaimRecovery(ctx, "job", "someone-else", "owner-2"); err != nil || ok {
		t.Fatalf("claim with a stale previous owner = %v, %v", ok, err)
	}
	if ok, err := r.st.ClaimRecovery(ctx, "job", "owner-1", "owner-2"); err != nil || !ok {
		t.Fatalf("claim from the verified owner = %v, %v", ok, err)
	}
	got, err := r.st.GetMemory(ctx, store.RecoveryOwners, "job")
	if err != nil || got.Value != "owner-2" || got.Author != "controlplane" {
		t.Fatalf("owner record = %+v, %v", got, err)
	}
	// A record written by someone else is never taken over.
	if err := r.st.PutMemory(ctx, domain.MemoryEntry{Namespace: store.RecoveryOwners, Key: "forged", Value: "x", Author: "agent", UpdatedAt: r.now}); err != nil {
		t.Fatal(err)
	}
	if ok, err := r.st.ClaimRecovery(ctx, "forged", "x", "owner-3"); err != nil || ok {
		t.Fatalf("claim over a foreign record = %v, %v", ok, err)
	}
}

type bareStore struct{ store.Store }

func TestOptionalCapabilitiesFailClosed(t *testing.T) {
	if _, err := store.Learnings(bareStore{}); !errors.Is(err, store.ErrLearningUnsupported) {
		t.Fatalf("Learnings(bare) = %v", err)
	}
	if _, err := store.Councils(bareStore{}); !errors.Is(err, store.ErrCouncilUnsupported) {
		t.Fatalf("Councils(bare) = %v", err)
	}
	r := newConclaveRig(t)
	if _, err := store.Learnings(r.st); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Councils(r.st); err != nil {
		t.Fatal(err)
	}

	ctx := t.Context()
	if op, ok := store.LearningOperator(ctx); ok || op != "" {
		t.Fatalf("operator without binding = %q, %v", op, ok)
	}
	for _, tc := range []struct {
		operator string
		want     bool
	}{{"alice", true}, {"", false}, {" padded", false}, {githubToken(), false}, {"a\nb", false}} {
		if _, ok := store.LearningOperator(store.WithLearningOperator(ctx, tc.operator)); ok != tc.want {
			t.Errorf("LearningOperator(%q) valid = %v, want %v", tc.operator, ok, tc.want)
		}
	}
}

func TestLearningTimeAndDigest(t *testing.T) {
	at := time.Date(2026, 11, 26, 1, 2, 3, 4, time.FixedZone("x", 3600))
	formatted := store.FormatLearningTime(at)
	if formatted != "2026-11-26T00:02:03.000000004Z" {
		t.Fatalf("FormatLearningTime = %q", formatted)
	}
	back, err := store.ParseLearningTime(formatted)
	if err != nil || !back.Equal(at) || back.Location() != time.UTC {
		t.Fatalf("ParseLearningTime = %v, %v", back, err)
	}
	if _, err := store.ParseLearningTime("yesterday"); err == nil {
		t.Fatal("unparseable time accepted")
	}
	if got := store.FormatLearningTime(time.Time{}); len(got) != len(formatted) {
		t.Fatalf("zero time formatted as %q", got)
	}

	base := domain.Learning{Statement: "s", Scope: domain.LearningScope{Global: true}, SourceKind: domain.LearningSourceOperator}
	d1, err := store.ComputeLearningVersionDigest(base)
	if err != nil || len(d1) != 64 {
		t.Fatal(d1, err)
	}
	// Mutable review fields do not change the version; immutable ones do.
	reviewed := base
	reviewed.Status, reviewed.AcceptedBy = domain.LearningStatusAccepted, "alice"
	if d, _ := store.ComputeLearningVersionDigest(reviewed); d != d1 {
		t.Fatal("review fields changed the version digest")
	}
	for name, mutate := range map[string]func(*domain.Learning){
		"statement":   func(l *domain.Learning) { l.Statement = "t" },
		"scope":       func(l *domain.Learning) { l.Scope = domain.LearningScope{Cluster: "c"} },
		"source":      func(l *domain.Learning) { l.SourceKind = domain.LearningSourceAgent },
		"evidence":    func(l *domain.Learning) { l.EvidenceRef = "task:1" },
		"predecessor": func(l *domain.Learning) { l.PredecessorID = "p" },
		"legacy key":  func(l *domain.Learning) { l.LegacyKey = "k" },
	} {
		l := base
		mutate(&l)
		if d, _ := store.ComputeLearningVersionDigest(l); d == d1 {
			t.Errorf("%s did not change the version digest", name)
		}
	}
}

func TestConclaveReportBudgetFromTurns(t *testing.T) {
	r := newConclaveRig(t)
	ctx := t.Context()
	r.room(t, "room", domain.ConclaveStateActive, 0)
	turn := turnTask(t, "room", "a", 0)
	if err := r.submit(t, "room", turn, r.limits()); err != nil {
		t.Fatal(err)
	}
	if err := r.st.CreateTask(ctx, domain.Task{ID: "unrelated", IdempotencyKey: "unrelated", WorkloadName: "worker", State: domain.TaskStateWorking}); err != nil {
		t.Fatal(err)
	}
	for _, e := range []domain.BudgetEntry{
		{TaskID: turn.ID, WorkloadName: "worker", InputTokens: 60, CreatedAt: r.now},
		{TaskID: "unrelated", WorkloadName: "worker", InputTokens: 1000, CreatedAt: r.now},
	} {
		if err := r.st.RecordBudgetUsage(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	msg := domain.ConclaveMessage{ConclaveID: "room", FromRole: "verifier", ToRole: "operator", Kind: "review", Body: "report", CreatedAt: r.now}
	limits := r.limits()
	limits.Budget = &domain.PerTaskBudget{MaxInputTokens: 100}
	if id, reason, err := store.AppendConclaveReport(ctx, r.st, msg, limits, nil); err != nil || id == 0 || reason != "" {
		t.Fatalf("report under budget = %d, %q, %v", id, reason, err)
	}
	limits.Budget = &domain.PerTaskBudget{MaxInputTokens: 60}
	id, reason, err := store.AppendConclaveReport(ctx, r.st, msg, limits, nil)
	if !errors.Is(err, store.ErrWardTripped) || id != 0 || reason != "input_tokens_exceeded(60 >= 60)" {
		t.Fatalf("report over budget = %d, %q, %v", id, reason, err)
	}
	if c := roomState(t, r.st, "room"); c.State != domain.ConclaveStateInputRequired || c.WardReason != reason {
		t.Fatalf("room = %+v", c)
	}
}
