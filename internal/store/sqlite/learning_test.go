package sqlite

import (
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/shadewright-labs/sablecourt/internal/domain"
	"github.com/shadewright-labs/sablecourt/internal/store"
	"github.com/shadewright-labs/sablecourt/internal/store/postgres"
)

func openLearningStore(t *testing.T) (*SQLiteStore, store.LearningStore) {
	t.Helper()
	st := openTestStore(t)
	ls, err := store.Learnings(st)
	if err != nil {
		t.Fatalf("store.Learnings(sqlite): %v", err)
	}
	return st, ls
}

func TestLearningBackendCapability(t *testing.T) {
	for _, tc := range []struct {
		name    string
		st      store.Store
		allowed bool
	}{
		{name: "sqlite", st: openTestStore(t), allowed: true},
		{name: "postgres", st: &postgres.PostgresStore{}, allowed: false},
		{name: "nil", st: nil, allowed: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ls, err := store.Learnings(tc.st)
			if tc.allowed {
				if err != nil || ls == nil {
					t.Fatalf("expected learning capability, got ls=%v err=%v", ls, err)
				}
				return
			}
			if !errors.Is(err, store.ErrLearningUnsupported) || ls != nil {
				t.Fatalf("expected ErrLearningUnsupported, got ls=%v err=%v", ls, err)
			}
		})
	}
}

func TestLearningLifecycleAndClock(t *testing.T) {
	ctx := t.Context()
	st, ls := openLearningStore(t)
	seedTaskEvidence(t, st, "task-42", "go-test-log")
	seedTaskEvidence(t, st, "task-106", "pass-evidence")
	seedTaskEvidence(t, st, "task-110", "contradiction-log")

	base := time.Date(2026, 11, 1, 10, 0, 0, 123456789, time.UTC)

	// Propose without evidence: stays proposed with unverified provenance.
	unverified, err := ls.ProposeLearning(ctx, domain.Learning{
		ID:         "learn-unverified",
		Statement:  "Run race detector before merge",
		Scope:      domain.LearningScope{Global: true},
		SourceKind: domain.LearningSourceAgent,
		Author:     "coder-agent",
		CreatedAt:  base,
	})
	if err != nil {
		t.Fatalf("ProposeLearning(unverified): %v", err)
	}
	if unverified.Status != domain.LearningStatusProposed || unverified.ProvenanceVerified {
		t.Fatalf("unexpected unverified state: status=%q verified=%v", unverified.Status, unverified.ProvenanceVerified)
	}

	// Propose with verifiable source and evidence: still starts proposed until operator accepts.
	l1, err := ls.ProposeLearning(ctx, domain.Learning{
		ID:          "learn-1",
		Statement:   "Always pin Go dependencies in go.sum",
		Scope:       domain.LearningScope{RepoRemoteURL: "https://github.com/shadewright-labs/sablecourt.git"},
		SourceKind:  domain.LearningSourceTask,
		SourceID:    "task-42",
		EvidenceRef: "artifact:go-test-log",
		Author:      "verifier-agent",
		CreatedAt:   base,
	})
	if err != nil {
		t.Fatalf("ProposeLearning(l1): %v", err)
	}
	if l1.Status != domain.LearningStatusProposed || !l1.ProvenanceVerified {
		t.Fatalf("unexpected l1 initial state: %+v", l1)
	}
	if l1.Scope.RepoRemoteURL != "https://github.com/shadewright-labs/sablecourt" {
		t.Fatalf("expected canonical remote URL without .git, got %q", l1.Scope.RepoRemoteURL)
	}
	if l1.VersionDigest == "" {
		t.Fatal("expected non-empty VersionDigest")
	}

	// Accept requires exact version digest and authenticated operator.
	if _, err := ls.AcceptLearning(store.WithLearningOperator(ctx, "operator-1"), l1.ID, "", base.Add(time.Hour)); err == nil {
		t.Fatal("expected error on empty digest")
	}
	if _, err := ls.AcceptLearning(store.WithLearningOperator(ctx, "operator-1"), l1.ID, "wrong-digest", base.Add(time.Hour)); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("expected ErrConflict on wrong digest, got %v", err)
	}
	if _, err := ls.AcceptLearning(ctx, l1.ID, l1.VersionDigest, base.Add(time.Hour)); err == nil {
		t.Fatal("expected error on empty operator")
	}

	acceptAt := base.Add(time.Hour)
	accepted, err := ls.AcceptLearning(store.WithLearningOperator(ctx, "operator-1"), l1.ID, l1.VersionDigest, acceptAt)
	if err != nil {
		t.Fatalf("AcceptLearning: %v", err)
	}
	wantExpiry := acceptAt.Add(domain.LearningReviewTTL)
	if accepted.Status != domain.LearningStatusAccepted || accepted.AcceptedBy != "operator-1" ||
		accepted.LastConfirmedAt == nil || !accepted.LastConfirmedAt.Equal(acceptAt) ||
		accepted.ExpiresAt == nil || !accepted.ExpiresAt.Equal(wantExpiry) {
		t.Fatalf("unexpected accepted learning: %+v", accepted)
	}

	// Recording injection counts once per packet ID and does not change the 90-day clock.
	injectAt := acceptAt.Add(24 * time.Hour)
	recorded, err := ls.RecordLearningInjection(ctx, l1.ID, "pkt-1", "task-100", injectAt)
	if err != nil || !recorded {
		t.Fatalf("RecordLearningInjection(pkt-1): recorded=%v err=%v", recorded, err)
	}
	recordedDup, err := ls.RecordLearningInjection(ctx, l1.ID, "pkt-1", "task-100", injectAt.Add(time.Minute))
	if err != nil || recordedDup {
		t.Fatalf("duplicate RecordLearningInjection(pkt-1): recorded=%v err=%v", recordedDup, err)
	}
	recorded2, err := ls.RecordLearningInjection(ctx, l1.ID, "pkt-2", "task-101", injectAt.Add(2*time.Minute))
	if err != nil || !recorded2 {
		t.Fatalf("RecordLearningInjection(pkt-2): recorded=%v err=%v", recorded2, err)
	}
	afterInject, err := ls.GetLearning(ctx, l1.ID)
	if err != nil {
		t.Fatalf("GetLearning: %v", err)
	}
	if afterInject.InjectionCount != 2 || !afterInject.ExpiresAt.Equal(wantExpiry) || !afterInject.LastConfirmedAt.Equal(acceptAt) {
		t.Fatalf("injection mutated clock or miscounted: %+v", afterInject)
	}

	// Injecting a proposed learning fails closed.
	if _, err := ls.RecordLearningInjection(ctx, unverified.ID, "pkt-3", "task-102", injectAt); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("expected ErrConflict injecting proposed learning, got %v", err)
	}

	// Inconclusive and unconfirmed supporting outcomes leave the 90-day clock alone.
	inconclusiveAt := acceptAt.Add(10 * 24 * time.Hour)
	incOutcome, err := ls.RecordLearningOutcome(ctx, domain.LearningOutcome{
		LearningID:  l1.ID,
		Kind:        domain.LearningOutcomeInconclusive,
		SourceKind:  domain.LearningSourceVerifier,
		SourceID:    "task-105",
		EvidenceRef: "artifact:inconclusive",
		Summary:     "Unrelated flake",
		Actor:       "verifier",
		CreatedAt:   inconclusiveAt,
	})
	if err != nil || incOutcome.ID <= 0 {
		t.Fatalf("RecordLearningOutcome(inconclusive): %+v err=%v", incOutcome, err)
	}
	supAt := acceptAt.Add(20 * 24 * time.Hour)
	supOutcome, err := ls.RecordLearningOutcome(ctx, domain.LearningOutcome{
		LearningID:  l1.ID,
		Kind:        domain.LearningOutcomeSupporting,
		SourceKind:  domain.LearningSourceVerifier,
		SourceID:    "task-106",
		EvidenceRef: "artifact:pass-evidence",
		Summary:     "Check caught stale checksum",
		Actor:       "verifier",
		CreatedAt:   supAt,
	})
	if err != nil || supOutcome.ID <= 0 {
		t.Fatalf("RecordLearningOutcome(supporting): %+v err=%v", supOutcome, err)
	}
	beforeConfirm, err := ls.GetLearning(ctx, l1.ID)
	if err != nil || !beforeConfirm.ExpiresAt.Equal(wantExpiry) {
		t.Fatalf("unconfirmed outcomes must not renew clock: %+v err=%v", beforeConfirm, err)
	}

	// Confirming an inconclusive outcome fails closed.
	confirmAt := acceptAt.Add(30 * 24 * time.Hour)
	if _, err := ls.ConfirmLearning(store.WithLearningOperator(ctx, "operator-2"), l1.ID, incOutcome.ID, confirmAt); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("expected ErrConflict confirming inconclusive outcome, got %v", err)
	}

	// Confirming a supporting outcome with evidence restarts the 90-day clock.
	confirmed, err := ls.ConfirmLearning(store.WithLearningOperator(ctx, "operator-2"), l1.ID, supOutcome.ID, confirmAt)
	if err != nil {
		t.Fatalf("ConfirmLearning: %v", err)
	}
	wantRenewedExpiry := confirmAt.Add(domain.LearningReviewTTL)
	if confirmed.LastConfirmedAt == nil || !confirmed.LastConfirmedAt.Equal(confirmAt) ||
		confirmed.ExpiresAt == nil || !confirmed.ExpiresAt.Equal(wantRenewedExpiry) {
		t.Fatalf("ConfirmLearning did not restart 90-day clock: %+v", confirmed)
	}

	// Expiry withholds immediately on ListLearnings even before ExpireLearnings runs.
	pastExpiry := wantRenewedExpiry.Add(time.Nanosecond)
	stillActive, err := ls.ListLearnings(ctx, store.LearningFilter{
		Status:        domain.LearningStatusAccepted,
		RepoRemoteURL: "https://github.com/shadewright-labs/sablecourt",
		Now:           wantRenewedExpiry.Add(-time.Nanosecond),
	})
	if err != nil || len(stillActive) != 1 || stillActive[0].ID != l1.ID {
		t.Fatalf("expected learning eligible 1ns before expiry: %+v err=%v", stillActive, err)
	}
	expiredRead, err := ls.ListLearnings(ctx, store.LearningFilter{
		Status:        domain.LearningStatusAccepted,
		RepoRemoteURL: "https://github.com/shadewright-labs/sablecourt",
		Now:           pastExpiry,
	})
	if err != nil || len(expiredRead) != 0 {
		t.Fatalf("expired learning must be withheld before sweep: %+v err=%v", expiredRead, err)
	}
	if _, err := ls.RecordLearningInjection(ctx, l1.ID, "pkt-expired", "task-109", pastExpiry); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("expected ErrConflict injecting expired learning, got %v", err)
	}

	// Contradiction without evidence fails closed; contradiction with evidence returns to proposed immediately.
	contraAt := confirmAt.Add(2 * time.Hour)
	if _, err := ls.RecordLearningOutcome(ctx, domain.LearningOutcome{
		LearningID: l1.ID,
		Kind:       domain.LearningOutcomeContradiction,
		SourceKind: domain.LearningSourceVerifier,
		SourceID:   "task-110",
		Summary:    "No evidence",
		Actor:      "verifier",
		CreatedAt:  contraAt,
	}); err == nil {
		t.Fatal("expected error recording contradiction without evidence")
	}
	if _, err := ls.RecordLearningOutcome(ctx, domain.LearningOutcome{
		LearningID:  l1.ID,
		Kind:        domain.LearningOutcomeContradiction,
		SourceKind:  domain.LearningSourceVerifier,
		SourceID:    "task-110",
		EvidenceRef: "artifact:contradiction-log",
		Summary:     "Rule caused false failure",
		Actor:       "verifier",
		CreatedAt:   contraAt,
	}); err != nil {
		t.Fatalf("RecordLearningOutcome(contradiction): %v", err)
	}
	afterContra, err := ls.GetLearning(ctx, l1.ID)
	if err != nil {
		t.Fatalf("GetLearning(afterContra): %v", err)
	}
	if afterContra.Status != domain.LearningStatusProposed || !strings.Contains(afterContra.ReviewReason, "contradiction") {
		t.Fatalf("expected return to proposed on evidenced contradiction, got %+v", afterContra)
	}

	// Re-accept l1, then propose replacement l2 and accept l2 -> retires l1 in the same transaction.
	reacceptAt := contraAt.Add(time.Hour)
	if _, err := ls.AcceptLearning(store.WithLearningOperator(ctx, "operator-1"), l1.ID, l1.VersionDigest, reacceptAt); err != nil {
		t.Fatalf("re-accept l1: %v", err)
	}
	l2, err := ls.ProposeLearning(ctx, domain.Learning{
		ID:            "learn-2",
		Statement:     "Always pin Go dependencies in go.sum and verify with go mod verify",
		Scope:         domain.LearningScope{RepoRemoteURL: "https://github.com/shadewright-labs/sablecourt"},
		SourceKind:    domain.LearningSourceTask,
		SourceID:      "task-42",
		EvidenceRef:   "artifact:go-test-log",
		Author:        "operator-1",
		PredecessorID: l1.ID,
		CreatedAt:     reacceptAt.Add(time.Minute),
	})
	if err != nil {
		t.Fatalf("ProposeLearning(l2): %v", err)
	}
	replaceAt := reacceptAt.Add(2 * time.Minute)
	if _, err := ls.AcceptLearning(store.WithLearningOperator(ctx, "operator-1"), l2.ID, l2.VersionDigest, replaceAt); err != nil {
		t.Fatalf("AcceptLearning(l2): %v", err)
	}
	l1Retired, err := ls.GetLearning(ctx, l1.ID)
	if err != nil || l1Retired.Status != domain.LearningStatusRetired {
		t.Fatalf("expected predecessor l1 to be retired atomically, got %+v err=%v", l1Retired, err)
	}

	// Second replacement targeting already-retired l1 cannot be accepted.
	l3, err := ls.ProposeLearning(ctx, domain.Learning{
		ID:            "learn-3",
		Statement:     "Duplicate replacement",
		Scope:         domain.LearningScope{RepoRemoteURL: "https://github.com/shadewright-labs/sablecourt"},
		SourceKind:    domain.LearningSourceTask,
		SourceID:      "task-42",
		EvidenceRef:   "artifact:go-test-log",
		Author:        "operator-1",
		PredecessorID: l1.ID,
		CreatedAt:     replaceAt.Add(time.Minute),
	})
	if err != nil {
		t.Fatalf("ProposeLearning(l3): %v", err)
	}
	if _, err := ls.AcceptLearning(store.WithLearningOperator(ctx, "operator-1"), l3.ID, l3.VersionDigest, replaceAt.Add(2*time.Minute)); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("expected ErrConflict accepting replacement for already-retired predecessor, got %v", err)
	}

	// ExpireLearnings sweeps l2 after 90 days into proposed with review reason and history.
	sweepAt := replaceAt.Add(domain.LearningReviewTTL).Add(time.Second)
	expiredCount, err := ls.ExpireLearnings(ctx, sweepAt)
	if err != nil || expiredCount != 1 {
		t.Fatalf("ExpireLearnings: count=%d err=%v", expiredCount, err)
	}
	l2Expired, err := ls.GetLearning(ctx, l2.ID)
	if err != nil || l2Expired.Status != domain.LearningStatusProposed || l2Expired.ReviewReason != "expired" {
		t.Fatalf("expected expired l2 in proposed state, got %+v err=%v", l2Expired, err)
	}
	expiredAgain, err := ls.ExpireLearnings(ctx, sweepAt)
	if err != nil || expiredAgain != 0 {
		t.Fatalf("second ExpireLearnings should be no-op: count=%d err=%v", expiredAgain, err)
	}

	// Retire l2 explicitly and verify history and outcomes are preserved.
	if _, err := ls.RetireLearning(store.WithLearningOperator(ctx, "operator-1"), l2.ID, "no longer needed", sweepAt.Add(time.Minute)); err != nil {
		t.Fatalf("RetireLearning(l2): %v", err)
	}
	if _, err := ls.RetireLearning(store.WithLearningOperator(ctx, "operator-1"), l2.ID, "again", sweepAt.Add(2*time.Minute)); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("expected ErrConflict retiring already-retired learning, got %v", err)
	}

	hist1, err := ls.ListLearningHistory(ctx, l1.ID)
	if err != nil || len(hist1) < 6 {
		t.Fatalf("expected full history for l1, got len=%d err=%v", len(hist1), err)
	}
	outcomes1, err := ls.ListLearningOutcomes(ctx, l1.ID)
	if err != nil || len(outcomes1) != 3 {
		t.Fatalf("expected 3 outcomes for l1, got len=%d err=%v", len(outcomes1), err)
	}
}

func TestLearningLegacyMigration(t *testing.T) {
	ctx := t.Context()
	st, err := Open(filepath.Join(t.TempDir(), "legacy.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	// Apply only migrations prior to 0012_learnings.sql.
	if _, err := st.db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (version TEXT PRIMARY KEY, applied_at TEXT NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	for _, mig := range []string{
		"0001_init.sql", "0002_task_messages.sql", "0003_provider_prefs.sql",
		"0004_conclaves.sql", "0005_audit_chain.sql", "0006_devices.sql",
		"0007_memory.sql", "0008_conclave_run_windows.sql", "0009_reconcile_tasks.sql",
		"0010_council_attempts.sql", "0011_council_results.sql",
	} {
		raw, err := migrationFS.ReadFile("migrations/" + mig)
		if err != nil {
			t.Fatalf("read %s: %v", mig, err)
		}
		if _, err := st.db.ExecContext(ctx, string(raw)); err != nil {
			t.Fatalf("apply %s: %v", mig, err)
		}
		if _, err := st.db.ExecContext(ctx, `INSERT INTO schema_migrations(version, applied_at) VALUES (?, ?)`, mig, nowRFC3339()); err != nil {
			t.Fatal(err)
		}
	}

	legacyTS := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	legacyRows := []domain.MemoryEntry{
		{Namespace: "learning:global", Key: "race", Value: "Always run go test -race", Author: "alice", UpdatedAt: legacyTS},
		{Namespace: "learning:global", Key: "secret-leak", Value: "Never commit sk-ant-1234567890abcdef123456", Author: "bob", UpdatedAt: legacyTS},
		{Namespace: "learning:repo:sablecourt", Key: "style", Value: "No em dashes", Author: "operator", UpdatedAt: legacyTS},
		{Namespace: "learning:path:internal/store/**", Key: "tx", Value: "Use single transaction for multi-table updates", Author: "agent-1", UpdatedAt: legacyTS},
		{Namespace: "notes", Key: "not-a-learning", Value: "Ordinary memory row", Author: "agent-1", UpdatedAt: legacyTS},
	}
	for _, row := range legacyRows {
		if err := st.PutMemory(ctx, row); err != nil {
			t.Fatalf("PutMemory(%s/%s): %v", row.Namespace, row.Key, err)
		}
	}
	// Seed 250 newer court_memory rows so old MatchLearnings(200) would have dropped them.
	for i := range 250 {
		if err := st.PutMemory(ctx, domain.MemoryEntry{
			Namespace: "notes",
			Key:       fmt.Sprintf("noise-%03d", i),
			Value:     "recent agent note",
			Author:    "agent-noise",
			UpdatedAt: legacyTS.Add(time.Duration(i+1) * time.Minute),
		}); err != nil {
			t.Fatalf("PutMemory(noise-%d): %v", i, err)
		}
	}

	// Run Migrate twice to verify one-time idempotent migration.
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("Migrate(1): %v", err)
	}
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("Migrate(2): %v", err)
	}

	ls, err := store.Learnings(st)
	if err != nil {
		t.Fatalf("store.Learnings: %v", err)
	}

	all, err := ls.ListLearnings(ctx, store.LearningFilter{Status: domain.LearningStatusProposed})
	if err != nil {
		t.Fatalf("ListLearnings(proposed): %v", err)
	}
	if len(all) != 4 {
		t.Fatalf("expected 4 migrated proposed learnings, got %d: %+v", len(all), all)
	}
	accepted, err := ls.ListLearnings(ctx, store.LearningFilter{Status: domain.LearningStatusAccepted})
	if err != nil || len(accepted) != 0 {
		t.Fatalf("legacy migration must be proposal-only (ADR-0012), got accepted=%d err=%v", len(accepted), err)
	}

	byOrigin := map[string]domain.Learning{}
	for _, l := range all {
		if l.ProvenanceVerified {
			t.Errorf("migrated learning %s must have ProvenanceVerified=false", l.ID)
		}
		if l.SourceKind != domain.LearningSourceLegacy {
			t.Errorf("migrated learning %s SourceKind=%q, want legacy", l.ID, l.SourceKind)
		}
		if l.LegacyUpdatedAt == "" {
			t.Errorf("migrated learning %s missing LegacyUpdatedAt", l.ID)
		}
		byOrigin[l.LegacyNamespace+"/"+l.LegacyKey] = l
	}

	globalRace, ok := byOrigin["learning:global/race"]
	if !ok || !globalRace.Scope.Global || globalRace.Scope.NeedsScopeMapping || globalRace.LegacyAuthor != "alice" {
		t.Fatalf("unexpected migrated learning:global/race: %+v", globalRace)
	}
	secretLeak, ok := byOrigin["learning:global/secret-leak"]
	if !ok || strings.Contains(secretLeak.Statement, "sk-ant-") || !strings.Contains(secretLeak.Statement, "[REDACTED:anthropic_key]") {
		t.Fatalf("legacy secret was not redacted: %+v", secretLeak)
	}
	repoLegacy, ok := byOrigin["learning:repo:sablecourt/style"]
	if !ok || !repoLegacy.Scope.NeedsScopeMapping || repoLegacy.LegacyNamespace != "learning:repo:sablecourt" {
		t.Fatalf("ambiguous repo scope must require operator mapping: %+v", repoLegacy)
	}
	pathLegacy, ok := byOrigin["learning:path:internal/store/**/tx"]
	if !ok || !pathLegacy.Scope.NeedsScopeMapping {
		t.Fatalf("ambiguous path scope must require operator mapping: %+v", pathLegacy)
	}

	now := time.Date(2026, 11, 25, 0, 0, 0, 0, time.UTC)
	// Direct acceptance of unmapped legacy scope fails closed.
	if _, err := ls.AcceptLearning(store.WithLearningOperator(ctx, "operator-1"), repoLegacy.ID, repoLegacy.VersionDigest, now); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("expected ErrConflict accepting unmapped legacy scope, got %v", err)
	}
	// Operator resolves it via linked replacement with canonical remote URL.
	seedTaskEvidence(t, st, "source", "record")
	mapped, err := ls.ProposeLearning(ctx, domain.Learning{
		ID:            "mapped-style",
		Statement:     repoLegacy.Statement,
		Scope:         domain.LearningScope{RepoRemoteURL: "https://github.com/shadewright-labs/sablecourt"},
		SourceKind:    domain.LearningSourceTask,
		SourceID:      "source",
		EvidenceRef:   "artifact:record",
		Author:        "operator-1",
		PredecessorID: repoLegacy.ID,
		CreatedAt:     now,
	})
	if err != nil {
		t.Fatalf("ProposeLearning(mapped): %v", err)
	}
	if _, err := ls.AcceptLearning(store.WithLearningOperator(ctx, "operator-1"), mapped.ID, mapped.VersionDigest, now.Add(time.Second)); err != nil {
		t.Fatalf("AcceptLearning(mapped): %v", err)
	}
	retiredLegacy, err := ls.GetLearning(ctx, repoLegacy.ID)
	if err != nil || retiredLegacy.Status != domain.LearningStatusRetired {
		t.Fatalf("expected legacy predecessor retired after mapping, got %+v err=%v", retiredLegacy, err)
	}
}

func TestLearningListByScopeUsesIndex(t *testing.T) {
	ctx := t.Context()
	st, ls := openLearningStore(t)
	seedTaskEvidence(t, st, "source", "record")

	now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	repoURL := "https://github.com/shadewright-labs/sablecourt"

	seed := []struct {
		id     string
		scope  domain.LearningScope
		accept bool
	}{
		{id: "g-1", scope: domain.LearningScope{Global: true}, accept: true},
		{id: "g-proposed", scope: domain.LearningScope{Global: true}, accept: false},
		{id: "r-only", scope: domain.LearningScope{RepoRemoteURL: repoURL}, accept: true},
		{id: "r-path", scope: domain.LearningScope{RepoRemoteURL: repoURL, PathGlobs: []string{"internal/store/**"}}, accept: true},
		{id: "inc-cluster", scope: domain.LearningScope{Cluster: "prod-eu"}, accept: true},
		{id: "inc-full", scope: domain.LearningScope{Cluster: "prod-eu", Namespace: "payments", WorkloadKind: "Deployment", Symptom: "OOMKilled"}, accept: true},
	}
	for i, s := range seed {
		l, err := ls.ProposeLearning(ctx, domain.Learning{
			ID:          s.id,
			Statement:   "Rule " + s.id,
			Scope:       s.scope,
			SourceKind:  domain.LearningSourceTask,
			SourceID:    "source",
			EvidenceRef: "artifact:record",
			Author:      "operator-1",
			CreatedAt:   now.Add(time.Duration(i) * time.Second),
		})
		if err != nil {
			t.Fatalf("ProposeLearning(%s): %v", s.id, err)
		}
		if s.accept {
			if _, err := ls.AcceptLearning(store.WithLearningOperator(ctx, "operator-1"), l.ID, l.VersionDigest, now.Add(time.Duration(i)*time.Second)); err != nil {
				t.Fatalf("AcceptLearning(%s): %v", s.id, err)
			}
		}
	}

	// Add 300 court_memory rows to prove listing learnings never scans court_memory.
	for i := range 300 {
		if err := st.PutMemory(ctx, domain.MemoryEntry{
			Namespace: "notes",
			Key:       fmt.Sprintf("k-%03d", i),
			Value:     "v",
			Author:    "agent",
			UpdatedAt: now.Add(time.Duration(i) * time.Second),
		}); err != nil {
			t.Fatal(err)
		}
	}

	for _, tc := range []struct {
		name   string
		filter store.LearningFilter
		want   []string
	}{
		{
			name:   "global accepted",
			filter: store.LearningFilter{Status: domain.LearningStatusAccepted, GlobalOnly: true, Now: now.Add(time.Minute)},
			want:   []string{"g-1"},
		},
		{
			name:   "exact scope_key global proposed",
			filter: store.LearningFilter{Status: domain.LearningStatusProposed, ScopeKey: "global", Now: now.Add(time.Minute)},
			want:   []string{"g-proposed"},
		},
		{
			name:   "repo accepted specificity order",
			filter: store.LearningFilter{Status: domain.LearningStatusAccepted, RepoRemoteURL: repoURL, Paths: []string{"internal/store/sqlite/learning.go"}, Now: now.Add(time.Minute)},
			want:   []string{"r-path", "r-only", "g-1"},
		},
		{
			name:   "incident cluster only",
			filter: store.LearningFilter{Status: domain.LearningStatusAccepted, IncidentCluster: "prod-eu", Now: now.Add(time.Minute)},
			want:   []string{"inc-cluster", "g-1"},
		},
		{
			name: "incident full selector",
			filter: store.LearningFilter{
				Status:               domain.LearningStatusAccepted,
				IncidentCluster:      "prod-eu",
				IncidentNamespace:    "payments",
				IncidentWorkloadKind: "Deployment",
				IncidentSymptom:      "OOMKilled",
				Now:                  now.Add(time.Minute),
			},
			want: []string{"inc-full", "inc-cluster", "g-1"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ls.ListLearnings(ctx, tc.filter)
			if err != nil {
				t.Fatalf("ListLearnings: %v", err)
			}
			ids := make([]string, len(got))
			for i, l := range got {
				ids[i] = l.ID
			}
			if !slices.Equal(ids, tc.want) {
				t.Fatalf("got %v, want %v", ids, tc.want)
			}
		})
	}

	// Verify EXPLAIN QUERY PLAN uses indexes on learnings, never SCAN learnings or court_memory.
	for _, tc := range []struct {
		name   string
		filter store.LearningFilter
	}{
		{
			name:   "by scope_key and status",
			filter: store.LearningFilter{ScopeKey: "global", Status: domain.LearningStatusAccepted, Now: now},
		},
		{
			name:   "by repo_remote_url and status",
			filter: store.LearningFilter{RepoRemoteURL: repoURL, Status: domain.LearningStatusAccepted, Now: now},
		},
		{
			name: "by repo_remote_url and path",
			filter: store.LearningFilter{
				RepoRemoteURL: repoURL, Paths: []string{"internal/store/sqlite/learning.go"}, Status: domain.LearningStatusAccepted, Now: now,
			},
		},
		{
			name: "by incident selectors and status",
			filter: store.LearningFilter{
				IncidentCluster: "prod-eu", IncidentNamespace: "payments", Status: domain.LearningStatusAccepted, Now: now,
			},
		},
	} {
		t.Run("plan_"+tc.name, func(t *testing.T) {
			query, err := buildLearningListQuery(tc.filter)
			if err != nil {
				t.Fatal(err)
			}
			rows, err := st.db.QueryContext(ctx, "EXPLAIN QUERY PLAN "+query.sql, query.args...)
			if err != nil {
				t.Fatalf("EXPLAIN QUERY PLAN: %v", err)
			}
			defer rows.Close()
			var details []string
			for rows.Next() {
				var id, parent, notused int
				var detail string
				if err := rows.Scan(&id, &parent, &notused, &detail); err != nil {
					t.Fatal(err)
				}
				details = append(details, detail)
			}
			if err := rows.Err(); err != nil {
				t.Fatal(err)
			}
			plan := strings.Join(details, " | ")
			if !slices.ContainsFunc(details, func(detail string) bool {
				return strings.HasPrefix(detail, "SEARCH learnings USING INDEX ") ||
					strings.HasPrefix(detail, "SEARCH learnings USING COVERING INDEX ")
			}) {
				t.Fatalf("expected indexed search on learnings, got: %s", plan)
			}
			if strings.Contains(plan, "SCAN learnings") || strings.Contains(plan, "court_memory") {
				t.Fatalf("unexpected table scan or memory scan in plan: %s", plan)
			}
		})
	}
}

func TestLearningImmutabilityAndNoDelete(t *testing.T) {
	ctx := t.Context()
	st, ls := openLearningStore(t)

	l, err := ls.ProposeLearning(ctx, domain.Learning{
		ID:         "immutable-1",
		Statement:  "Original statement",
		Scope:      domain.LearningScope{Global: true},
		SourceKind: domain.LearningSourceOperator,
		Author:     "operator-1",
	})
	if err != nil {
		t.Fatalf("ProposeLearning: %v", err)
	}

	for _, tc := range []struct {
		name string
		sql  string
		args []any
	}{
		{name: "mutate statement", sql: `UPDATE learnings SET statement = 'tampered' WHERE id = ?`, args: []any{l.ID}},
		{name: "mutate scope_json", sql: `UPDATE learnings SET scope_json = '{}' WHERE id = ?`, args: []any{l.ID}},
		{name: "mutate author", sql: `UPDATE learnings SET author = 'spoofed' WHERE id = ?`, args: []any{l.ID}},
		{name: "delete learning", sql: `DELETE FROM learnings WHERE id = ?`, args: []any{l.ID}},
		{name: "mutate history", sql: `UPDATE learning_history SET actor = 'spoofed' WHERE learning_id = ?`, args: []any{l.ID}},
		{name: "delete history", sql: `DELETE FROM learning_history WHERE learning_id = ?`, args: []any{l.ID}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := st.db.ExecContext(ctx, tc.sql, tc.args...); err == nil {
				t.Fatalf("expected immutability trigger to block %s", tc.name)
			}
		})
	}
}

func TestLearningAtomicRollbackAndRetry(t *testing.T) {
	ctx := t.Context()
	st, ls := openLearningStore(t)
	seedTaskEvidence(t, st, "source", "record")
	now := time.Date(2026, 11, 25, 14, 0, 0, 0, time.UTC)

	t.Run("propose rollback on history failure", func(t *testing.T) {
		if _, err := st.db.ExecContext(ctx, `CREATE TRIGGER fail_hist_ins BEFORE INSERT ON learning_history BEGIN SELECT RAISE(FAIL, 'history unavailable'); END`); err != nil {
			t.Fatal(err)
		}
		_, err := ls.ProposeLearning(ctx, domain.Learning{
			ID:         "atomic-prop",
			Statement:  "Test atomic propose",
			Scope:      domain.LearningScope{Global: true},
			SourceKind: domain.LearningSourceOperator,
			Author:     "operator-1",
			CreatedAt:  now,
		})
		if err == nil {
			t.Fatal("expected ProposeLearning failure")
		}
		if _, err := ls.GetLearning(ctx, "atomic-prop"); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("partial learning survived rollback: %v", err)
		}
		if _, err := st.db.ExecContext(ctx, `DROP TRIGGER fail_hist_ins`); err != nil {
			t.Fatal(err)
		}
		if _, err := ls.ProposeLearning(ctx, domain.Learning{
			ID:         "atomic-prop",
			Statement:  "Test atomic propose",
			Scope:      domain.LearningScope{Global: true},
			SourceKind: domain.LearningSourceOperator,
			Author:     "operator-1",
			CreatedAt:  now,
		}); err != nil {
			t.Fatalf("retry ProposeLearning failed: %v", err)
		}
	})

	t.Run("accept replacement rollback on predecessor retirement failure", func(t *testing.T) {
		orig, err := ls.ProposeLearning(ctx, domain.Learning{
			ID:          "atomic-orig",
			Statement:   "Original rule",
			Scope:       domain.LearningScope{Global: true},
			SourceKind:  domain.LearningSourceTask,
			SourceID:    "source",
			EvidenceRef: "artifact:record",
			Author:      "operator-1",
			CreatedAt:   now,
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := ls.AcceptLearning(store.WithLearningOperator(ctx, "operator-1"), orig.ID, orig.VersionDigest, now); err != nil {
			t.Fatal(err)
		}
		repl, err := ls.ProposeLearning(ctx, domain.Learning{
			ID:            "atomic-repl",
			Statement:     "Replacement rule",
			Scope:         domain.LearningScope{Global: true},
			SourceKind:    domain.LearningSourceTask,
			SourceID:      "source",
			EvidenceRef:   "artifact:record",
			Author:        "operator-1",
			PredecessorID: orig.ID,
			CreatedAt:     now.Add(time.Second),
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := st.db.ExecContext(ctx, `CREATE TRIGGER fail_retire_pred BEFORE UPDATE ON learnings WHEN NEW.id = 'atomic-orig' AND NEW.status = 'retired' BEGIN SELECT RAISE(FAIL, 'retire blocked'); END`); err != nil {
			t.Fatal(err)
		}
		if _, err := ls.AcceptLearning(store.WithLearningOperator(ctx, "operator-1"), repl.ID, repl.VersionDigest, now.Add(2*time.Second)); err == nil {
			t.Fatal("expected AcceptLearning failure when predecessor retirement fails")
		}
		gotRepl, err := ls.GetLearning(ctx, repl.ID)
		if err != nil || gotRepl.Status != domain.LearningStatusProposed {
			t.Fatalf("replacement state leaked after rollback: %+v err=%v", gotRepl, err)
		}
		gotOrig, err := ls.GetLearning(ctx, orig.ID)
		if err != nil || gotOrig.Status != domain.LearningStatusAccepted {
			t.Fatalf("predecessor state mutated after rollback: %+v err=%v", gotOrig, err)
		}
		if _, err := st.db.ExecContext(ctx, `DROP TRIGGER fail_retire_pred`); err != nil {
			t.Fatal(err)
		}
		if _, err := ls.AcceptLearning(store.WithLearningOperator(ctx, "operator-1"), repl.ID, repl.VersionDigest, now.Add(2*time.Second)); err != nil {
			t.Fatalf("retry AcceptLearning failed: %v", err)
		}
		gotOrig, _ = ls.GetLearning(ctx, orig.ID)
		if gotOrig.Status != domain.LearningStatusRetired {
			t.Fatalf("expected predecessor retired after retry, got %q", gotOrig.Status)
		}
	})

	t.Run("injection rollback on count update failure", func(t *testing.T) {
		if _, err := st.db.ExecContext(ctx, `CREATE TRIGGER fail_inj_count BEFORE UPDATE ON learnings WHEN NEW.id = 'atomic-repl' AND NEW.injection_count > OLD.injection_count BEGIN SELECT RAISE(FAIL, 'count blocked'); END`); err != nil {
			t.Fatal(err)
		}
		if _, err := ls.RecordLearningInjection(ctx, "atomic-repl", "pkt-atomic", "task-1", now.Add(3*time.Second)); err == nil {
			t.Fatal("expected RecordLearningInjection failure")
		}
		if _, err := st.db.ExecContext(ctx, `DROP TRIGGER fail_inj_count`); err != nil {
			t.Fatal(err)
		}
		recorded, err := ls.RecordLearningInjection(ctx, "atomic-repl", "pkt-atomic", "task-1", now.Add(3*time.Second))
		if err != nil || !recorded {
			t.Fatalf("retry RecordLearningInjection failed: recorded=%v err=%v", recorded, err)
		}
		got, err := ls.GetLearning(ctx, "atomic-repl")
		if err != nil || got.InjectionCount != 1 {
			t.Fatalf("expected InjectionCount=1 after retry, got %+v err=%v", got, err)
		}
	})
}

func TestLearningInputValidationAndSecrets(t *testing.T) {
	ctx := t.Context()
	_, ls := openLearningStore(t)

	validScope := domain.LearningScope{Global: true}
	oversizedStmt := strings.Repeat("a", store.MaxLearningStatementBytes+1)
	oversizedID := strings.Repeat("x", store.MaxLearningFieldBytes+1)
	tooManyGlobs := make([]string, store.MaxLearningPathGlobs+1)
	for i := range tooManyGlobs {
		tooManyGlobs[i] = fmt.Sprintf("dir-%d/*.go", i)
	}

	for _, tc := range []struct {
		name string
		in   domain.Learning
	}{
		{name: "empty id", in: domain.Learning{ID: "", Statement: "rule", Scope: validScope, SourceKind: domain.LearningSourceOperator, Author: "op"}},
		{name: "whitespace id", in: domain.Learning{ID: "  ", Statement: "rule", Scope: validScope, SourceKind: domain.LearningSourceOperator, Author: "op"}},
		{name: "control char id", in: domain.Learning{ID: "id\n1", Statement: "rule", Scope: validScope, SourceKind: domain.LearningSourceOperator, Author: "op"}},
		{name: "nul byte id", in: domain.Learning{ID: "id\x001", Statement: "rule", Scope: validScope, SourceKind: domain.LearningSourceOperator, Author: "op"}},
		{name: "invalid utf8 id", in: domain.Learning{ID: string([]byte{0xff}), Statement: "rule", Scope: validScope, SourceKind: domain.LearningSourceOperator, Author: "op"}},
		{name: "oversized id", in: domain.Learning{ID: oversizedID, Statement: "rule", Scope: validScope, SourceKind: domain.LearningSourceOperator, Author: "op"}},
		{name: "secret in id", in: domain.Learning{ID: "ghp_1234567890abcdefghij1234", Statement: "rule", Scope: validScope, SourceKind: domain.LearningSourceOperator, Author: "op"}},
		{name: "empty statement", in: domain.Learning{ID: "v-1", Statement: "   ", Scope: validScope, SourceKind: domain.LearningSourceOperator, Author: "op"}},
		{name: "nul byte statement", in: domain.Learning{ID: "v-2", Statement: "ok\x00bad", Scope: validScope, SourceKind: domain.LearningSourceOperator, Author: "op"}},
		{name: "invalid utf8 statement", in: domain.Learning{ID: "v-3", Statement: string([]byte{0xff, 0xfe}), Scope: validScope, SourceKind: domain.LearningSourceOperator, Author: "op"}},
		{name: "oversized statement", in: domain.Learning{ID: "v-4", Statement: oversizedStmt, Scope: validScope, SourceKind: domain.LearningSourceOperator, Author: "op"}},
		{name: "empty scope", in: domain.Learning{ID: "v-5", Statement: "rule", Scope: domain.LearningScope{}, SourceKind: domain.LearningSourceOperator, Author: "op"}},
		{name: "global combined with repo", in: domain.Learning{ID: "v-6", Statement: "rule", Scope: domain.LearningScope{Global: true, RepoRemoteURL: "https://github.com/org/repo"}, SourceKind: domain.LearningSourceOperator, Author: "op"}},
		{name: "bare repo basename", in: domain.Learning{ID: "v-7", Statement: "rule", Scope: domain.LearningScope{RepoRemoteURL: "sablecourt"}, SourceKind: domain.LearningSourceOperator, Author: "op"}},
		{name: "repo url with credentials", in: domain.Learning{ID: "v-8", Statement: "rule", Scope: domain.LearningScope{RepoRemoteURL: "https://user:pass@github.com/org/repo"}, SourceKind: domain.LearningSourceOperator, Author: "op"}},
		{name: "repo url with query token", in: domain.Learning{ID: "v-9", Statement: "rule", Scope: domain.LearningScope{RepoRemoteURL: "https://github.com/org/repo?token=secret"}, SourceKind: domain.LearningSourceOperator, Author: "op"}},
		{name: "file scheme repo url", in: domain.Learning{ID: "v-10", Statement: "rule", Scope: domain.LearningScope{RepoRemoteURL: "file:///tmp/repo"}, SourceKind: domain.LearningSourceOperator, Author: "op"}},
		{name: "absolute path glob", in: domain.Learning{ID: "v-11", Statement: "rule", Scope: domain.LearningScope{PathGlobs: []string{"/etc/passwd"}}, SourceKind: domain.LearningSourceOperator, Author: "op"}},
		{name: "traversal path glob", in: domain.Learning{ID: "v-12", Statement: "rule", Scope: domain.LearningScope{PathGlobs: []string{"../secret/**"}}, SourceKind: domain.LearningSourceOperator, Author: "op"}},
		{name: "malformed path glob", in: domain.Learning{ID: "v-13", Statement: "rule", Scope: domain.LearningScope{PathGlobs: []string{"internal/[unclosed"}}, SourceKind: domain.LearningSourceOperator, Author: "op"}},
		{name: "too many path globs", in: domain.Learning{ID: "v-14", Statement: "rule", Scope: domain.LearningScope{PathGlobs: tooManyGlobs}, SourceKind: domain.LearningSourceOperator, Author: "op"}},
		{name: "secret in incident selector", in: domain.Learning{ID: "v-15", Statement: "rule", Scope: domain.LearningScope{Symptom: "api_key=supersecretvalue123"}, SourceKind: domain.LearningSourceOperator, Author: "op"}},
		{name: "unmapped legacy scope via propose", in: domain.Learning{ID: "v-16", Statement: "rule", Scope: domain.LearningScope{LegacyNamespace: "learning:repo:foo", NeedsScopeMapping: true}, SourceKind: domain.LearningSourceOperator, Author: "op"}},
		{name: "wrong source kind", in: domain.Learning{ID: "v-17", Statement: "rule", Scope: validScope, SourceKind: "unknown", Author: "op"}},
		{name: "legacy source kind via propose", in: domain.Learning{ID: "v-18", Statement: "rule", Scope: validScope, SourceKind: domain.LearningSourceLegacy, Author: "op"}},
		{name: "accepted status via propose", in: domain.Learning{ID: "v-19", Statement: "rule", Scope: validScope, SourceKind: domain.LearningSourceOperator, Author: "op", Status: domain.LearningStatusAccepted}},
		{name: "empty author", in: domain.Learning{ID: "v-20", Statement: "rule", Scope: validScope, SourceKind: domain.LearningSourceOperator, Author: ""}},
		{name: "secret in author", in: domain.Learning{ID: "v-21", Statement: "rule", Scope: validScope, SourceKind: domain.LearningSourceOperator, Author: "sk-proj-1234567890abcdefghijklmnop"}},
		{name: "missing predecessor", in: domain.Learning{ID: "v-22", Statement: "rule", Scope: validScope, SourceKind: domain.LearningSourceOperator, Author: "op", PredecessorID: "does-not-exist"}},
		{name: "self predecessor", in: domain.Learning{ID: "v-23", Statement: "rule", Scope: validScope, SourceKind: domain.LearningSourceOperator, Author: "op", PredecessorID: "v-23"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ls.ProposeLearning(ctx, tc.in); err == nil {
				t.Fatalf("expected rejection for %s", tc.name)
			}
		})
	}

	// Secret redaction in statement and evidence_ref (including truncated PEM block).
	redacted, err := ls.ProposeLearning(ctx, domain.Learning{
		ID:          "redact-1",
		Statement:   "Never pass Authorization: Bearer supersecrettoken12345 or -----BEGIN" + " RSA PRIVATE KEY-----\nMIIEowIBAAKCAQEA",
		Scope:       validScope,
		SourceKind:  domain.LearningSourceAgent,
		SourceID:    "task-99",
		EvidenceRef: "token=ghp_" + "1234567890abcdefghij1234",
		Author:      "agent-1",
	})
	if err != nil {
		t.Fatalf("ProposeLearning(redact): %v", err)
	}
	for _, leaked := range []string{"supersecrettoken12345", "MIIEowIBAAKCAQEA", "ghp_1234567890abcdefghij1234"} {
		if strings.Contains(redacted.Statement, leaked) || strings.Contains(redacted.EvidenceRef, leaked) {
			t.Fatalf("secret %q leaked into stored learning: %+v", leaked, redacted)
		}
	}

	// Filter validation fails closed on invalid status, invalid repo URL, or negative limit.
	for _, tc := range []struct {
		name   string
		filter store.LearningFilter
	}{
		{name: "invalid status", filter: store.LearningFilter{Status: "bogus"}},
		{name: "invalid repo url", filter: store.LearningFilter{RepoRemoteURL: "https://user:pass@github.com/org/repo"}},
		{name: "negative limit", filter: store.LearningFilter{Limit: -1}},
	} {
		t.Run("filter_"+tc.name, func(t *testing.T) {
			if _, err := ls.ListLearnings(ctx, tc.filter); err == nil {
				t.Fatalf("expected ListLearnings error for %s", tc.name)
			}
		})
	}
}

func TestLearningAuditEdgeCases(t *testing.T) {
	ctx := t.Context()

	t.Run("unaddressed contradiction withholds injection and confirmation even before state sync", func(t *testing.T) {
		st, ls := openLearningStore(t)
		seedTaskEvidence(t, st, "source", "record")
		now := time.Date(2026, 11, 25, 16, 0, 0, 0, time.UTC)

		l, err := ls.ProposeLearning(ctx, domain.Learning{
			ID:          "withhold-1",
			Statement:   "Always check return errors",
			Scope:       domain.LearningScope{Global: true},
			SourceKind:  domain.LearningSourceTask,
			SourceID:    "source",
			EvidenceRef: "artifact:record",
			Author:      "operator-1",
			CreatedAt:   now,
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := ls.AcceptLearning(store.WithLearningOperator(ctx, "operator-1"), l.ID, l.VersionDigest, now); err != nil {
			t.Fatal(err)
		}
		sup, err := ls.RecordLearningOutcome(ctx, domain.LearningOutcome{
			LearningID:  l.ID,
			Kind:        domain.LearningOutcomeSupporting,
			SourceKind:  domain.LearningSourceTask,
			SourceID:    "source",
			EvidenceRef: "artifact:record",
			Actor:       "verifier",
			CreatedAt:   now.Add(time.Minute),
		})
		if err != nil {
			t.Fatal(err)
		}
		// Simulate an evidenced contradiction recorded before a background job updates learnings.status.
		if _, err := st.db.ExecContext(ctx, `
			INSERT INTO learning_outcomes (learning_id, kind, source_kind, source_id, evidence_ref, summary, actor, created_at)
			VALUES (?, 'contradiction', 'verifier', 'task-x', 'artifact:contra', 'broke build', 'verifier', ?)
		`, l.ID, store.FormatLearningTime(now.Add(2*time.Minute))); err != nil {
			t.Fatal(err)
		}

		listed, err := ls.ListLearnings(ctx, store.LearningFilter{
			Status:     domain.LearningStatusAccepted,
			GlobalOnly: true,
			Now:        now.Add(3 * time.Minute),
		})
		if err != nil || len(listed) != 0 {
			t.Fatalf("ListLearnings must withhold contradicted learning: len=%d err=%v", len(listed), err)
		}
		if _, err := ls.RecordLearningInjection(ctx, l.ID, "pkt-withheld", "task-1", now.Add(3*time.Minute)); !errors.Is(err, store.ErrConflict) {
			t.Fatalf("RecordLearningInjection must withhold contradicted learning, got %v", err)
		}
		if _, err := ls.ConfirmLearning(store.WithLearningOperator(ctx, "operator-1"), l.ID, sup.ID, now.Add(3*time.Minute)); !errors.Is(err, store.ErrConflict) {
			t.Fatalf("ConfirmLearning must withhold contradicted learning, got %v", err)
		}
	})

	t.Run("legacy migration redacts updated_at and survives redacted namespace collisions", func(t *testing.T) {
		st, err := Open(filepath.Join(t.TempDir(), "legacy-edge.db"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = st.Close() })
		if _, err := st.db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (version TEXT PRIMARY KEY, applied_at TEXT NOT NULL)`); err != nil {
			t.Fatal(err)
		}
		for _, mig := range []string{
			"0001_init.sql", "0002_task_messages.sql", "0003_provider_prefs.sql",
			"0004_conclaves.sql", "0005_audit_chain.sql", "0006_devices.sql",
			"0007_memory.sql", "0008_conclave_run_windows.sql", "0009_reconcile_tasks.sql",
			"0010_council_attempts.sql", "0011_council_results.sql",
		} {
			raw, err := migrationFS.ReadFile("migrations/" + mig)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := st.db.ExecContext(ctx, string(raw)); err != nil {
				t.Fatal(err)
			}
			if _, err := st.db.ExecContext(ctx, `INSERT INTO schema_migrations(version, applied_at) VALUES (?, ?)`, mig, nowRFC3339()); err != nil {
				t.Fatal(err)
			}
		}
		// Two rows whose secrets redact to identical namespaces, plus a secret in updated_at.
		for _, r := range []struct{ ns, key, val, upd string }{
			{"learning:repo:sk-ant-11111111111111111111", "same", "Rule A", "ghp_1234567890abcdefghij1234"},
			{"learning:repo:sk-ant-22222222222222222222", "same", "Rule B", "2026-10-01T00:00:00Z"},
		} {
			if _, err := st.db.ExecContext(ctx, `INSERT INTO court_memory(namespace, key, value, author, updated_at) VALUES (?, ?, ?, 'op', ?)`, r.ns, r.key, r.val, r.upd); err != nil {
				t.Fatal(err)
			}
		}
		if err := st.Migrate(ctx); err != nil {
			t.Fatalf("Migrate with colliding redacted legacy namespaces failed: %v", err)
		}
		ls, err := store.Learnings(st)
		if err != nil {
			t.Fatal(err)
		}
		items, err := ls.ListLearnings(ctx, store.LearningFilter{Status: domain.LearningStatusProposed})
		if err != nil || len(items) != 2 {
			t.Fatalf("expected 2 migrated learnings, got %d err=%v", len(items), err)
		}
		for _, item := range items {
			if strings.Contains(item.LegacyUpdatedAt, "ghp_1234567890abcdefghij1234") {
				t.Fatalf("secret leaked in LegacyUpdatedAt: %+v", item)
			}
		}
	})

	t.Run("max-length replacement id keeps predecessor review reason within limit", func(t *testing.T) {
		st, ls := openLearningStore(t)
		seedTaskEvidence(t, st, "source", "record")
		now := time.Date(2026, 11, 25, 17, 0, 0, 0, time.UTC)
		orig, err := ls.ProposeLearning(ctx, domain.Learning{
			ID:         "short-orig",
			Statement:  "Original",
			Scope:      domain.LearningScope{Global: true},
			SourceKind: domain.LearningSourceOperator,
			Author:     "operator-1",
			CreatedAt:  now,
		})
		if err != nil {
			t.Fatal(err)
		}
		maxID := strings.Repeat("r", store.MaxLearningFieldBytes)
		repl, err := ls.ProposeLearning(ctx, domain.Learning{
			ID:            maxID,
			Statement:     "Replacement",
			Scope:         domain.LearningScope{Global: true},
			SourceKind:    domain.LearningSourceTask,
			SourceID:      "source",
			EvidenceRef:   "artifact:record",
			Author:        "operator-1",
			PredecessorID: orig.ID,
			CreatedAt:     now.Add(time.Second),
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := ls.AcceptLearning(store.WithLearningOperator(ctx, "operator-1"), repl.ID, repl.VersionDigest, now.Add(2*time.Second)); err != nil {
			t.Fatal(err)
		}
		gotOrig, err := ls.GetLearning(ctx, orig.ID)
		if err != nil {
			t.Fatal(err)
		}
		if len(gotOrig.ReviewReason) > store.MaxLearningFieldBytes {
			t.Fatalf("predecessor ReviewReason exceeded MaxLearningFieldBytes: %d > %d", len(gotOrig.ReviewReason), store.MaxLearningFieldBytes)
		}
	})
}
