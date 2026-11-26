package sqlite

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/shadewright-labs/sablecourt/internal/domain"
	"github.com/shadewright-labs/sablecourt/internal/store"
)

// acceptedLearning returns an accepted global learning with task evidence, accepted at now.
func acceptedLearning(t *testing.T, st *SQLiteStore, now time.Time) domain.Learning {
	t.Helper()
	rec := reviewProposal(t, st, domain.LearningScope{Global: true}, now.Add(-time.Hour))
	accepted, err := st.AcceptLearning(store.WithLearningOperator(t.Context(), "operator"), rec.ID, rec.VersionDigest, now)
	if err != nil {
		t.Fatal(err)
	}
	return accepted
}

func supporting(l domain.Learning, at time.Time) domain.LearningOutcome {
	return domain.LearningOutcome{LearningID: l.ID, Kind: domain.LearningOutcomeSupporting, Actor: "reviewer",
		SourceKind: l.SourceKind, SourceID: l.SourceID, EvidenceRef: l.EvidenceRef, CreatedAt: at}
}

func TestRecordLearningOutcomeValidation(t *testing.T) {
	st, _ := openLearningStore(t)
	ctx := t.Context()
	now := time.Date(2026, 11, 25, 0, 0, 0, 0, time.UTC)
	l := acceptedLearning(t, st, now)
	token := "ghp_" + strings.Repeat("a1B2", 9)
	for _, tc := range []struct {
		name   string
		mutate func(*domain.LearningOutcome)
		err    error
	}{
		{"invalid learning id", func(o *domain.LearningOutcome) { o.LearningID = " " }, nil},
		{"unknown learning", func(o *domain.LearningOutcome) { o.LearningID = "missing" }, store.ErrNotFound},
		{"unknown kind", func(o *domain.LearningOutcome) { o.Kind = "supports" }, nil},
		{"legacy source", func(o *domain.LearningOutcome) { o.SourceKind = domain.LearningSourceLegacy }, nil},
		{"no actor", func(o *domain.LearningOutcome) { o.Actor = "" }, nil},
		{"secret actor", func(o *domain.LearningOutcome) { o.Actor = token }, nil},
		{"source id with control", func(o *domain.LearningOutcome) { o.SourceID = "a\x1bb" }, nil},
		{"evidence with NUL", func(o *domain.LearningOutcome) { o.EvidenceRef = "artifact:\x00" }, nil},
		{"oversize evidence", func(o *domain.LearningOutcome) { o.EvidenceRef = strings.Repeat("e", store.MaxLearningFieldBytes+1) }, nil},
		{"contradiction without evidence", func(o *domain.LearningOutcome) {
			o.Kind, o.EvidenceRef = domain.LearningOutcomeContradiction, " "
		}, nil},
		{"oversize summary", func(o *domain.LearningOutcome) {
			o.Summary = strings.Repeat("s", store.MaxLearningOutcomeSummaryBytes+1)
		}, nil},
		{"summary with NUL", func(o *domain.LearningOutcome) { o.Summary = "a\x00b" }, nil},
		{"outcome before acceptance", func(o *domain.LearningOutcome) { o.CreatedAt = now.Add(-time.Minute) }, store.ErrConflict},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := supporting(l, now.Add(time.Hour))
			tc.mutate(&in)
			out, err := st.RecordLearningOutcome(ctx, in)
			if err == nil || (tc.err != nil && !errors.Is(err, tc.err)) {
				t.Fatalf("RecordLearningOutcome = %+v, %v; want %v", out, err, tc.err)
			}
		})
	}
	if outcomes, err := st.ListLearningOutcomes(ctx, l.ID); err != nil || len(outcomes) != 0 {
		t.Fatalf("refused outcomes were stored: %+v, %v", outcomes, err)
	}

	// Secrets in a summary are redacted before storage.
	in := supporting(l, now.Add(time.Hour))
	in.Summary = "  saw " + token + " in logs  "
	out, err := st.RecordLearningOutcome(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.Summary, token) || strings.HasPrefix(out.Summary, " ") {
		t.Fatalf("summary stored as %q", out.Summary)
	}
	stored, err := st.ListLearningOutcomes(ctx, l.ID)
	if err != nil || len(stored) != 1 || stored[0].Summary != out.Summary || strings.Contains(stored[0].Summary, token) {
		t.Fatalf("stored outcomes = %+v, %v", stored, err)
	}
	// An inconclusive note is allowed on a proposed learning; other kinds are not.
	retired, err := st.RetireLearning(store.WithLearningOperator(ctx, "operator"), l.ID, "obsolete", now.Add(2*time.Hour))
	if err != nil || retired.Status != domain.LearningStatusRetired {
		t.Fatalf("retire = %+v, %v", retired, err)
	}
	if _, err := st.RecordLearningOutcome(ctx, supporting(l, now.Add(3*time.Hour))); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("supporting outcome on a retired learning = %v", err)
	}
	note := supporting(l, now.Add(3*time.Hour))
	note.Kind = domain.LearningOutcomeInconclusive
	if _, err := st.RecordLearningOutcome(ctx, note); err != nil {
		t.Fatalf("inconclusive note on a retired learning = %v", err)
	}
}

func TestContradictionReturnsLearningToReview(t *testing.T) {
	st, _ := openLearningStore(t)
	ctx := t.Context()
	now := time.Date(2026, 11, 25, 0, 0, 0, 0, time.UTC)
	l := acceptedLearning(t, st, now)
	in := supporting(l, now.Add(time.Hour))
	in.Kind, in.Summary = domain.LearningOutcomeContradiction, "failed again "+strings.Repeat("é", store.MaxLearningFieldBytes)
	out, err := st.RecordLearningOutcome(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	got, err := st.GetLearning(ctx, l.ID)
	if err != nil || got.Status != domain.LearningStatusProposed || !strings.HasPrefix(got.ReviewReason, "contradiction: failed again") ||
		len(got.ReviewReason) > store.MaxLearningFieldBytes {
		t.Fatalf("contradicted learning = %+v, %v", got, err)
	}
	history, err := st.ListLearningHistory(ctx, l.ID)
	if err != nil || history[len(history)-1].EventKind != "contradicted" {
		t.Fatalf("history = %+v, %v", history, err)
	}
	// A contradicted learning takes no supporting outcome until it is accepted again.
	if _, err := st.RecordLearningOutcome(ctx, supporting(l, now.Add(2*time.Hour))); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("supporting outcome after a contradiction = %v", err)
	}
	if _, err := st.ConfirmLearning(store.WithLearningOperator(ctx, "operator"), l.ID, out.ID, now.Add(3*time.Hour)); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("confirmation after a contradiction = %v", err)
	}
}

func TestConfirmLearningRefusals(t *testing.T) {
	st, _ := openLearningStore(t)
	ctx := store.WithLearningOperator(t.Context(), "operator")
	now := time.Date(2026, 11, 25, 0, 0, 0, 0, time.UTC)
	l := acceptedLearning(t, st, now)
	support, err := st.RecordLearningOutcome(ctx, supporting(l, now.Add(time.Hour)))
	if err != nil {
		t.Fatal(err)
	}
	note := supporting(l, now.Add(time.Hour))
	note.Kind = domain.LearningOutcomeInconclusive
	inconclusive, err := st.RecordLearningOutcome(ctx, note)
	if err != nil {
		t.Fatal(err)
	}
	confirmAt := now.Add(2 * time.Hour)
	for _, tc := range []struct {
		name    string
		ctx     bool
		id      string
		outcome int64
		err     error
	}{
		{name: "no operator", id: l.ID, outcome: support.ID},
		{name: "invalid id", ctx: true, id: "", outcome: support.ID},
		{name: "zero outcome", ctx: true, id: l.ID, outcome: 0},
		{name: "unknown learning", ctx: true, id: "missing", outcome: support.ID, err: store.ErrNotFound},
		{name: "unknown outcome", ctx: true, id: l.ID, outcome: support.ID + 100, err: store.ErrNotFound},
		{name: "inconclusive outcome", ctx: true, id: l.ID, outcome: inconclusive.ID, err: store.ErrConflict},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := t.Context()
			if tc.ctx {
				c = ctx
			}
			_, err := st.ConfirmLearning(c, tc.id, tc.outcome, confirmAt)
			if err == nil || (tc.err != nil && !errors.Is(err, tc.err)) {
				t.Fatalf("ConfirmLearning = %v, want %v", err, tc.err)
			}
		})
	}
	before, _ := st.GetLearning(ctx, l.ID)

	confirmed, err := st.ConfirmLearning(ctx, l.ID, support.ID, confirmAt)
	if err != nil {
		t.Fatal(err)
	}
	if confirmed.LastConfirmedAt == nil || !confirmed.LastConfirmedAt.Equal(confirmAt) || !confirmed.ExpiresAt.Equal(confirmAt.Add(domain.LearningReviewTTL)) {
		t.Fatalf("confirmed = %+v", confirmed)
	}
	if before.ExpiresAt != nil && !before.ExpiresAt.Before(*confirmed.ExpiresAt) {
		t.Fatalf("confirmation did not extend expiry: %v -> %v", before.ExpiresAt, confirmed.ExpiresAt)
	}
	// One outcome confirms at most once.
	if _, err := st.ConfirmLearning(ctx, l.ID, support.ID, confirmAt.Add(time.Minute)); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("second confirmation with the same outcome = %v", err)
	}
	outcomes, err := st.ListLearningOutcomes(ctx, l.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range outcomes {
		if o.ID == support.ID && (o.ConfirmedBy != "operator" || o.ConfirmedAt == nil) {
			t.Fatalf("confirmed outcome = %+v", o)
		}
	}

	// A retired learning cannot be confirmed.
	later, err := st.RecordLearningOutcome(ctx, supporting(l, confirmAt.Add(time.Hour)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.RetireLearning(ctx, l.ID, "replaced", confirmAt.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := st.ConfirmLearning(ctx, l.ID, later.ID, confirmAt.Add(3*time.Hour)); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("confirming a retired learning = %v", err)
	}
	if got, _ := st.GetLearning(ctx, l.ID); got.Status != domain.LearningStatusRetired {
		t.Fatalf("retired learning came back as %s", got.Status)
	}
}
