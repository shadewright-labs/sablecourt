package sqlite

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/shadewright-labs/sablecourt/internal/domain"
	"github.com/shadewright-labs/sablecourt/internal/store"
)

var _ store.LearningStore = (*SQLiteStore)(nil)

func (s *SQLiteStore) ProposeLearning(ctx context.Context, in domain.Learning) (domain.Learning, error) {
	if !store.ValidLearningID(in.ID, store.MaxLearningFieldBytes) {
		return domain.Learning{}, errors.New("invalid learning id")
	}
	if in.Status != "" && in.Status != domain.LearningStatusProposed {
		return domain.Learning{}, errors.New("new learnings must start in proposed status")
	}
	if !store.ValidLearningSourceKind(in.SourceKind, false) {
		return domain.Learning{}, errors.New("invalid learning source kind")
	}
	if !store.ValidLearningID(in.Author, store.MaxLearningFieldBytes) {
		return domain.Learning{}, errors.New("authenticated author is required")
	}
	if in.SourceID != "" && !store.ValidLearningID(in.SourceID, store.MaxLearningFieldBytes) {
		return domain.Learning{}, errors.New("invalid learning source id")
	}
	if in.PredecessorID != "" {
		if in.PredecessorID == in.ID || !store.ValidLearningID(in.PredecessorID, store.MaxLearningFieldBytes) {
			return domain.Learning{}, errors.New("invalid learning predecessor id")
		}
	}
	if !store.ValidLearningText(in.Statement, store.MaxLearningStatementBytes) {
		return domain.Learning{}, errors.New("invalid learning statement")
	}
	statement := strings.TrimSpace(store.RedactLearningText(in.Statement))
	if !store.ValidLearningText(statement, store.MaxLearningStatementBytes) {
		return domain.Learning{}, errors.New("invalid redacted learning statement")
	}

	evidenceRef := ""
	if strings.TrimSpace(in.EvidenceRef) != "" {
		if !store.ValidLearningText(in.EvidenceRef, store.MaxLearningFieldBytes) {
			return domain.Learning{}, errors.New("invalid learning evidence reference")
		}
		evidenceRef = strings.TrimSpace(store.RedactLearningText(in.EvidenceRef))
		if !store.ValidLearningText(evidenceRef, store.MaxLearningFieldBytes) {
			return domain.Learning{}, errors.New("invalid redacted learning evidence reference")
		}
	}

	reviewReason := ""
	if strings.TrimSpace(in.ReviewReason) != "" {
		if !store.ValidLearningText(in.ReviewReason, store.MaxLearningFieldBytes) {
			return domain.Learning{}, errors.New("invalid learning review reason")
		}
		reviewReason = strings.TrimSpace(store.RedactLearningText(in.ReviewReason))
		if !store.ValidLearningText(reviewReason, store.MaxLearningFieldBytes) {
			return domain.Learning{}, errors.New("invalid redacted learning review reason")
		}
	}

	scope, scopeKind, scopeKey, specificity, err := store.NormalizeLearningScope(in.Scope, false)
	if err != nil {
		return domain.Learning{}, err
	}

	createdAt := in.CreatedAt.UTC()
	if in.CreatedAt.IsZero() {
		createdAt = time.Now().UTC()
	}

	rec := domain.Learning{
		ID:               in.ID,
		Statement:        statement,
		Scope:            scope,
		ScopeKind:        scopeKind,
		ScopeKey:         scopeKey,
		ScopeSpecificity: specificity,
		SourceKind:       in.SourceKind,
		SourceID:         in.SourceID,
		EvidenceRef:      evidenceRef,
		Author:           in.Author,
		PredecessorID:    in.PredecessorID,
		Status:           domain.LearningStatusProposed,
		ReviewReason:     reviewReason,
		CreatedAt:        createdAt,
		UpdatedAt:        createdAt,
	}
	digest, err := store.ComputeLearningVersionDigest(rec)
	if err != nil {
		return domain.Learning{}, err
	}
	rec.VersionDigest = digest

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return domain.Learning{}, err
	}
	defer tx.Rollback()

	rec.ProvenanceVerified, err = learningEvidenceExists(ctx, tx, rec.SourceKind, rec.SourceID, rec.EvidenceRef)
	if err != nil {
		return domain.Learning{}, err
	}

	if rec.PredecessorID != "" {
		var exists int
		err := tx.QueryRowContext(ctx, `SELECT 1 FROM learnings WHERE id = ?`, rec.PredecessorID).Scan(&exists)
		if errors.Is(err, sql.ErrNoRows) {
			return domain.Learning{}, fmt.Errorf("%w: predecessor %s", store.ErrNotFound, rec.PredecessorID)
		}
		if err != nil {
			return domain.Learning{}, err
		}
	}

	if err := insertLearningTx(ctx, tx, rec, "proposed", rec.Author, rec.ReviewReason); err != nil {
		return domain.Learning{}, err
	}
	if err := tx.Commit(); err != nil {
		return domain.Learning{}, err
	}
	return rec, nil
}

func (s *SQLiteStore) GetLearning(ctx context.Context, id string) (domain.Learning, error) {
	if !store.ValidLearningID(id, store.MaxLearningFieldBytes) {
		return domain.Learning{}, errors.New("invalid learning id")
	}
	return getLearningRow(ctx, s.db, id)
}

func (s *SQLiteStore) AcceptLearning(ctx context.Context, id, expectedDigest string, now time.Time) (domain.Learning, error) {
	if !store.ValidLearningID(id, store.MaxLearningFieldBytes) {
		return domain.Learning{}, errors.New("invalid learning id")
	}
	if !store.ValidLearningID(expectedDigest, store.MaxLearningFieldBytes) {
		return domain.Learning{}, errors.New("expected learning digest is required")
	}
	operator, ok := store.LearningOperator(ctx)
	if !ok {
		return domain.Learning{}, errors.New("authenticated operator is required")
	}
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return domain.Learning{}, err
	}
	defer tx.Rollback()

	rec, err := getLearningRow(ctx, tx, id)
	if err != nil {
		return domain.Learning{}, err
	}
	if rec.VersionDigest != expectedDigest {
		return domain.Learning{}, fmt.Errorf("%w: learning digest mismatch", store.ErrConflict)
	}
	if rec.Status != domain.LearningStatusProposed {
		return domain.Learning{}, fmt.Errorf("%w: learning is %s", store.ErrConflict, rec.Status)
	}
	if rec.Scope.NeedsScopeMapping {
		return domain.Learning{}, fmt.Errorf("%w: learning scope requires operator mapping before acceptance", store.ErrConflict)
	}

	if err := requireLearningEvidence(ctx, tx, rec.SourceKind, rec.SourceID, rec.EvidenceRef); err != nil {
		return domain.Learning{}, err
	}

	nowStr := store.FormatLearningTime(now)
	expiresAt := now.Add(domain.LearningReviewTTL)
	expiresStr := store.FormatLearningTime(expiresAt)

	if rec.PredecessorID != "" {
		pred, err := getLearningRow(ctx, tx, rec.PredecessorID)
		if err != nil {
			return domain.Learning{}, err
		}
		if pred.Status == domain.LearningStatusRetired {
			var superseded bool
			err := tx.QueryRowContext(ctx, `
				SELECT EXISTS (SELECT 1 FROM learning_history
					WHERE learning_id = ? AND event_kind = 'retired' AND superseded_by = ?)
			`, pred.ID, rec.ID).Scan(&superseded)
			if err != nil {
				return domain.Learning{}, err
			}
			if !superseded {
				return domain.Learning{}, fmt.Errorf("%w: predecessor %s is already retired", store.ErrConflict, pred.ID)
			}
		} else {
			supersededReason := truncateUTF8("superseded by "+rec.ID, store.MaxLearningFieldBytes)
			res, err := tx.ExecContext(ctx, `
				UPDATE learnings
				SET status = 'retired', review_reason = ?, updated_at = ?
				WHERE id = ? AND status <> 'retired'
			`, supersededReason, nowStr, pred.ID)
			if err != nil {
				return domain.Learning{}, err
			}
			if n, _ := res.RowsAffected(); n != 1 {
				return domain.Learning{}, store.ErrConflict
			}
			if err := appendLearningHistoryTx(ctx, tx, domain.LearningHistory{
				LearningID:   pred.ID,
				FromStatus:   pred.Status,
				ToStatus:     domain.LearningStatusRetired,
				EventKind:    "retired",
				Actor:        operator,
				Reason:       supersededReason,
				SupersededBy: rec.ID,
				CreatedAt:    now,
			}); err != nil {
				return domain.Learning{}, err
			}
		}
	}

	res, err := tx.ExecContext(ctx, `
		UPDATE learnings
		SET status = 'accepted',
		    accepted_by = ?,
		    review_reason = '',
		    last_confirmed_at = ?,
		    expires_at = ?,
		    updated_at = ?
		WHERE id = ? AND status = 'proposed' AND version_digest = ?
	`, operator, nowStr, expiresStr, nowStr, rec.ID, expectedDigest)
	if err != nil {
		return domain.Learning{}, err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return domain.Learning{}, store.ErrConflict
	}

	if err := appendLearningHistoryTx(ctx, tx, domain.LearningHistory{
		LearningID:    rec.ID,
		FromStatus:    domain.LearningStatusProposed,
		ToStatus:      domain.LearningStatusAccepted,
		EventKind:     "accepted",
		Actor:         operator,
		EvidenceRef:   rec.EvidenceRef,
		PredecessorID: rec.PredecessorID,
		ExpiresAt:     &expiresAt,
		CreatedAt:     now,
	}); err != nil {
		return domain.Learning{}, err
	}

	updated, err := getLearningRow(ctx, tx, rec.ID)
	if err != nil {
		return domain.Learning{}, err
	}
	if err := tx.Commit(); err != nil {
		return domain.Learning{}, err
	}
	return updated, nil
}

func (s *SQLiteStore) ConfirmLearning(ctx context.Context, id string, outcomeID int64, now time.Time) (domain.Learning, error) {
	if !store.ValidLearningID(id, store.MaxLearningFieldBytes) {
		return domain.Learning{}, errors.New("invalid learning id")
	}
	if outcomeID <= 0 {
		return domain.Learning{}, errors.New("supporting outcome id is required")
	}
	operator, ok := store.LearningOperator(ctx)
	if !ok {
		return domain.Learning{}, errors.New("authenticated operator is required")
	}
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return domain.Learning{}, err
	}
	defer tx.Rollback()

	rec, err := getLearningRow(ctx, tx, id)
	if err != nil {
		return domain.Learning{}, err
	}
	if !rec.IsEligible(now) {
		return domain.Learning{}, fmt.Errorf("%w: learning is not eligible for confirmation", store.ErrConflict)
	}
	contradicted, err := hasUnaddressedContradictionTx(ctx, tx, rec)
	if err != nil {
		return domain.Learning{}, err
	}
	if contradicted {
		return domain.Learning{}, fmt.Errorf("%w: learning has an unaddressed contradiction", store.ErrConflict)
	}

	var outcomeKind, evidenceRef, confirmedBy, sourceID, createdAt string
	var sourceKind domain.LearningSourceKind
	err = tx.QueryRowContext(ctx, `
		SELECT kind, evidence_ref, confirmed_by, source_kind, source_id, created_at
		FROM learning_outcomes
		WHERE id = ? AND learning_id = ?
	`, outcomeID, id).Scan(&outcomeKind, &evidenceRef, &confirmedBy, &sourceKind, &sourceID, &createdAt)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.Learning{}, store.ErrNotFound
	}
	if err != nil {
		return domain.Learning{}, err
	}
	if domain.LearningOutcomeKind(outcomeKind) != domain.LearningOutcomeSupporting || strings.TrimSpace(evidenceRef) == "" {
		return domain.Learning{}, fmt.Errorf("%w: confirmation requires supporting outcome with evidence", store.ErrConflict)
	}
	if confirmedBy != "" {
		return domain.Learning{}, fmt.Errorf("%w: outcome already confirmed", store.ErrConflict)
	}

	nowStr := store.FormatLearningTime(now)
	if createdAt > nowStr || rec.LastConfirmedAt == nil || now.Before(*rec.LastConfirmedAt) {
		return domain.Learning{}, fmt.Errorf("%w: outcome or last confirmation is in the future", store.ErrConflict)
	}
	if createdAt < store.FormatLearningTime(*rec.LastConfirmedAt) {
		return domain.Learning{}, fmt.Errorf("%w: outcome predates the current review window", store.ErrConflict)
	}
	if err := subsequentLearningOutcome(ctx, tx, id, createdAt); err != nil {
		return domain.Learning{}, err
	}
	if err := requireLearningEvidence(ctx, tx, sourceKind, sourceID, evidenceRef); err != nil {
		return domain.Learning{}, err
	}

	expiresAt := now.Add(domain.LearningReviewTTL)
	expiresStr := store.FormatLearningTime(expiresAt)

	res, err := tx.ExecContext(ctx, `
		UPDATE learning_outcomes
		SET confirmed_by = ?, confirmed_at = ?
		WHERE id = ? AND learning_id = ? AND confirmed_by = ''
	`, operator, nowStr, outcomeID, id)
	if err != nil {
		return domain.Learning{}, err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return domain.Learning{}, store.ErrConflict
	}

	res, err = tx.ExecContext(ctx, `
		UPDATE learnings
		SET last_confirmed_at = ?, expires_at = ?, updated_at = ?
		WHERE id = ? AND status = 'accepted'
	`, nowStr, expiresStr, nowStr, id)
	if err != nil {
		return domain.Learning{}, err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return domain.Learning{}, store.ErrConflict
	}

	if err := appendLearningHistoryTx(ctx, tx, domain.LearningHistory{
		LearningID:  id,
		FromStatus:  domain.LearningStatusAccepted,
		ToStatus:    domain.LearningStatusAccepted,
		EventKind:   "confirmed",
		Actor:       operator,
		Reason:      fmt.Sprintf("confirmed outcome %d", outcomeID),
		EvidenceRef: evidenceRef,
		ExpiresAt:   &expiresAt,
		CreatedAt:   now,
	}); err != nil {
		return domain.Learning{}, err
	}

	updated, err := getLearningRow(ctx, tx, id)
	if err != nil {
		return domain.Learning{}, err
	}
	if err := tx.Commit(); err != nil {
		return domain.Learning{}, err
	}
	return updated, nil
}

func (s *SQLiteStore) RetireLearning(ctx context.Context, id, reason string, now time.Time) (domain.Learning, error) {
	if !store.ValidLearningID(id, store.MaxLearningFieldBytes) {
		return domain.Learning{}, errors.New("invalid learning id")
	}
	operator, ok := store.LearningOperator(ctx)
	if !ok {
		return domain.Learning{}, errors.New("authenticated operator is required")
	}
	cleanReason := ""
	if strings.TrimSpace(reason) != "" {
		if !store.ValidLearningText(reason, store.MaxLearningFieldBytes) {
			return domain.Learning{}, errors.New("invalid retirement reason")
		}
		cleanReason = strings.TrimSpace(store.RedactLearningText(reason))
		if !store.ValidLearningText(cleanReason, store.MaxLearningFieldBytes) {
			return domain.Learning{}, errors.New("invalid redacted retirement reason")
		}
	}
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return domain.Learning{}, err
	}
	defer tx.Rollback()

	rec, err := getLearningRow(ctx, tx, id)
	if err != nil {
		return domain.Learning{}, err
	}
	if rec.Status == domain.LearningStatusRetired {
		return domain.Learning{}, fmt.Errorf("%w: learning is already retired", store.ErrConflict)
	}

	nowStr := store.FormatLearningTime(now)
	res, err := tx.ExecContext(ctx, `
		UPDATE learnings
		SET status = 'retired', review_reason = ?, updated_at = ?
		WHERE id = ? AND status <> 'retired'
	`, cleanReason, nowStr, id)
	if err != nil {
		return domain.Learning{}, err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return domain.Learning{}, store.ErrConflict
	}

	if err := appendLearningHistoryTx(ctx, tx, domain.LearningHistory{
		LearningID: id,
		FromStatus: rec.Status,
		ToStatus:   domain.LearningStatusRetired,
		EventKind:  "retired",
		Actor:      operator,
		Reason:     cleanReason,
		CreatedAt:  now,
	}); err != nil {
		return domain.Learning{}, err
	}

	updated, err := getLearningRow(ctx, tx, id)
	if err != nil {
		return domain.Learning{}, err
	}
	if err := tx.Commit(); err != nil {
		return domain.Learning{}, err
	}
	return updated, nil
}

func (s *SQLiteStore) RecordLearningInjection(ctx context.Context, learningID, packetID, taskID string, at time.Time) (bool, error) {
	if !store.ValidLearningID(learningID, store.MaxLearningFieldBytes) {
		return false, errors.New("invalid learning id")
	}
	if !store.ValidLearningID(packetID, store.MaxLearningFieldBytes) {
		return false, errors.New("invalid packet id")
	}
	if taskID != "" && !store.ValidLearningID(taskID, store.MaxLearningFieldBytes) {
		return false, errors.New("invalid task id")
	}
	if at.IsZero() {
		at = time.Now().UTC()
	} else {
		at = at.UTC()
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()

	var alreadyRecorded int
	err = tx.QueryRowContext(ctx, `
		SELECT 1 FROM learning_injections WHERE learning_id = ? AND packet_id = ?
	`, learningID, packetID).Scan(&alreadyRecorded)
	if err == nil {
		return false, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return false, err
	}

	rec, err := getLearningRow(ctx, tx, learningID)
	if err != nil {
		return false, err
	}
	if !rec.IsEligible(at) {
		return false, fmt.Errorf("%w: learning is not eligible for injection", store.ErrConflict)
	}
	contradicted, err := hasUnaddressedContradictionTx(ctx, tx, rec)
	if err != nil {
		return false, err
	}
	if contradicted {
		return false, fmt.Errorf("%w: learning has an unaddressed contradiction", store.ErrConflict)
	}

	atStr := store.FormatLearningTime(at)
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO learning_injections (learning_id, packet_id, task_id, injected_at)
		VALUES (?, ?, ?, ?)
	`, learningID, packetID, taskID, atStr); err != nil {
		return false, err
	}

	res, err := tx.ExecContext(ctx, `
		UPDATE learnings
		SET injection_count = injection_count + 1, updated_at = ?
		WHERE id = ?
	`, atStr, learningID)
	if err != nil {
		return false, err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return false, store.ErrNotFound
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}

func (s *SQLiteStore) RecordLearningOutcome(ctx context.Context, in domain.LearningOutcome) (domain.LearningOutcome, error) {
	if !store.ValidLearningID(in.LearningID, store.MaxLearningFieldBytes) {
		return domain.LearningOutcome{}, errors.New("invalid learning id")
	}
	if !store.ValidLearningOutcomeKind(in.Kind) {
		return domain.LearningOutcome{}, errors.New("invalid learning outcome kind")
	}
	if !store.ValidLearningSourceKind(in.SourceKind, false) {
		return domain.LearningOutcome{}, errors.New("invalid learning outcome source kind")
	}
	if !store.ValidLearningID(in.Actor, store.MaxLearningFieldBytes) {
		return domain.LearningOutcome{}, errors.New("authenticated actor is required")
	}
	if in.SourceID != "" && !store.ValidLearningID(in.SourceID, store.MaxLearningFieldBytes) {
		return domain.LearningOutcome{}, errors.New("invalid outcome source id")
	}

	evidenceRef := ""
	if strings.TrimSpace(in.EvidenceRef) != "" {
		if !store.ValidLearningText(in.EvidenceRef, store.MaxLearningFieldBytes) {
			return domain.LearningOutcome{}, errors.New("invalid outcome evidence reference")
		}
		evidenceRef = strings.TrimSpace(store.RedactLearningText(in.EvidenceRef))
		if !store.ValidLearningText(evidenceRef, store.MaxLearningFieldBytes) {
			return domain.LearningOutcome{}, errors.New("invalid redacted outcome evidence reference")
		}
	}
	if in.Kind == domain.LearningOutcomeContradiction && evidenceRef == "" {
		return domain.LearningOutcome{}, errors.New("contradiction outcome requires an evidence reference")
	}

	summary := ""
	if strings.TrimSpace(in.Summary) != "" {
		if !store.ValidLearningText(in.Summary, store.MaxLearningOutcomeSummaryBytes) {
			return domain.LearningOutcome{}, errors.New("invalid outcome summary")
		}
		summary = strings.TrimSpace(store.RedactLearningText(in.Summary))
		if !store.ValidLearningText(summary, store.MaxLearningOutcomeSummaryBytes) {
			return domain.LearningOutcome{}, errors.New("invalid redacted outcome summary")
		}
	}

	createdAt := in.CreatedAt.UTC()
	if in.CreatedAt.IsZero() {
		createdAt = time.Now().UTC()
	}
	createdStr := store.FormatLearningTime(createdAt)

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return domain.LearningOutcome{}, err
	}
	defer tx.Rollback()

	rec, err := getLearningRow(ctx, tx, in.LearningID)
	if err != nil {
		return domain.LearningOutcome{}, err
	}

	if in.Kind != domain.LearningOutcomeInconclusive {
		if rec.Status != domain.LearningStatusAccepted {
			return domain.LearningOutcome{}, fmt.Errorf("%w: outcome requires an accepted learning", store.ErrConflict)
		}
		if err := subsequentLearningOutcome(ctx, tx, rec.ID, createdStr); err != nil {
			return domain.LearningOutcome{}, err
		}
	}
	if in.Kind == domain.LearningOutcomeContradiction {
		if err := requireLearningEvidence(ctx, tx, in.SourceKind, in.SourceID, evidenceRef); err != nil {
			return domain.LearningOutcome{}, err
		}
	}

	res, err := tx.ExecContext(ctx, `
		INSERT INTO learning_outcomes (
			learning_id, kind, source_kind, source_id, evidence_ref, summary, actor, created_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?)
	`, in.LearningID, string(in.Kind), string(in.SourceKind), in.SourceID, evidenceRef, summary, in.Actor, createdStr)
	if err != nil {
		return domain.LearningOutcome{}, err
	}
	outcomeID, err := res.LastInsertId()
	if err != nil {
		return domain.LearningOutcome{}, err
	}

	if in.Kind == domain.LearningOutcomeContradiction && rec.Status == domain.LearningStatusAccepted {
		reason := "contradiction"
		if summary != "" {
			reason = "contradiction: " + truncateUTF8(summary, store.MaxLearningFieldBytes-len("contradiction: "))
		}
		upd, err := tx.ExecContext(ctx, `
			UPDATE learnings
			SET status = 'proposed', review_reason = ?, updated_at = ?
			WHERE id = ? AND status = 'accepted'
		`, reason, createdStr, rec.ID)
		if err != nil {
			return domain.LearningOutcome{}, err
		}
		if n, _ := upd.RowsAffected(); n != 1 {
			return domain.LearningOutcome{}, store.ErrConflict
		}
		if err := appendLearningHistoryTx(ctx, tx, domain.LearningHistory{
			LearningID:  rec.ID,
			FromStatus:  domain.LearningStatusAccepted,
			ToStatus:    domain.LearningStatusProposed,
			EventKind:   "contradicted",
			Actor:       in.Actor,
			Reason:      reason,
			EvidenceRef: evidenceRef,
			CreatedAt:   createdAt,
		}); err != nil {
			return domain.LearningOutcome{}, err
		}
	}

	if err := tx.Commit(); err != nil {
		return domain.LearningOutcome{}, err
	}
	return domain.LearningOutcome{
		ID:          outcomeID,
		LearningID:  in.LearningID,
		Kind:        in.Kind,
		SourceKind:  in.SourceKind,
		SourceID:    in.SourceID,
		EvidenceRef: evidenceRef,
		Summary:     summary,
		Actor:       in.Actor,
		CreatedAt:   createdAt,
	}, nil
}

func (s *SQLiteStore) ExpireLearnings(ctx context.Context, now time.Time) (int, error) {
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	nowStr := store.FormatLearningTime(now)

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	rows, err := tx.QueryContext(ctx, `
		SELECT id FROM learnings
		WHERE status = 'accepted' AND expires_at IS NOT NULL AND expires_at <= ?
		ORDER BY id ASC
	`, nowStr)
	if err != nil {
		return 0, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return 0, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return 0, err
	}
	_ = rows.Close()

	for _, id := range ids {
		if _, err := tx.ExecContext(ctx, `
			UPDATE learnings
			SET status = 'proposed', review_reason = 'expired', updated_at = ?
			WHERE id = ? AND status = 'accepted'
		`, nowStr, id); err != nil {
			return 0, err
		}
		if err := appendLearningHistoryTx(ctx, tx, domain.LearningHistory{
			LearningID: id,
			FromStatus: domain.LearningStatusAccepted,
			ToStatus:   domain.LearningStatusProposed,
			EventKind:  "expired",
			Actor:      "system",
			Reason:     "expired",
			CreatedAt:  now,
		}); err != nil {
			return 0, err
		}
	}

	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return len(ids), nil
}

func (s *SQLiteStore) ListLearnings(ctx context.Context, filter store.LearningFilter) ([]domain.Learning, error) {
	query, err := buildLearningListQuery(filter)
	if err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, query.sql, query.args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []domain.Learning
	for rows.Next() {
		rec, err := scanLearningRow(rows)
		if err != nil {
			return nil, err
		}
		if query.matchPaths && !store.MatchesLearningPaths(rec.Scope.PathGlobs, filter.Paths) {
			continue
		}
		out = append(out, rec)
		if len(out) == query.limit {
			break
		}
	}
	return out, rows.Err()
}

type learningListQuery struct {
	sql        string
	args       []any
	matchPaths bool
	limit      int
}

func buildLearningListQuery(filter store.LearningFilter) (learningListQuery, error) {
	if filter.Status != "" && !store.ValidLearningStatus(filter.Status) {
		return learningListQuery{}, errors.New("invalid learning status filter")
	}
	if filter.Limit < 0 {
		return learningListQuery{}, errors.New("invalid learning list limit")
	}
	limit := filter.Limit
	if limit == 0 {
		limit = store.DefaultLearningListLimit
	}
	if limit > store.MaxLearningListLimit {
		limit = store.MaxLearningListLimit
	}
	var conds []string
	var args []any

	if filter.ScopeKey != "" {
		if !store.ValidLearningText(filter.ScopeKey, store.MaxLearningScopeKeyBytes) {
			return learningListQuery{}, errors.New("invalid scope key filter")
		}
		conds = append(conds, "scope_key = ?")
		args = append(args, filter.ScopeKey)
	}
	if filter.GlobalOnly {
		conds = append(conds, "scope_global = 1")
	}
	match := filter.Status == "" || filter.Status == domain.LearningStatusAccepted
	repo := strings.TrimSpace(filter.RepoRemoteURL)
	if repo != "" {
		var err error
		repo, err = store.NormalizeRepoRemoteURL(repo)
		if err != nil {
			return learningListQuery{}, err
		}
	}
	for _, sel := range []struct {
		column, value string
		max           int
	}{
		{"repo_remote_url", repo, store.MaxLearningFieldBytes},
		{"incident_cluster", filter.IncidentCluster, store.MaxLearningSelectorBytes},
		{"incident_namespace", filter.IncidentNamespace, store.MaxLearningSelectorBytes},
		{"incident_workload_kind", filter.IncidentWorkloadKind, store.MaxLearningSelectorBytes},
		{"incident_symptom", filter.IncidentSymptom, store.MaxLearningSymptomBytes},
	} {
		v := strings.TrimSpace(sel.value)
		if v != "" && !store.ValidLearningID(v, sel.max) {
			return learningListQuery{}, errors.New("invalid learning selector")
		}
		if match {
			conds = append(conds, sel.column+" IN ('', ?)")
			args = append(args, v)
		} else if v != "" {
			conds = append(conds, sel.column+" = ?")
			args = append(args, v)
		}
	}
	if match && len(filter.Paths) == 0 {
		conds = append(conds, "path_globs_json = '[]'")
	}
	if filter.NeedsScopeMapping != nil {
		if *filter.NeedsScopeMapping {
			conds = append(conds, "needs_scope_mapping = 1")
		} else {
			conds = append(conds, "needs_scope_mapping = 0")
		}
	}
	if match {
		filter.Status = domain.LearningStatusAccepted
	}
	if filter.Status != "" {
		conds = append(conds, "status = ?")
		args = append(args, string(filter.Status))
	}
	if match {
		if !filter.IncludeExpired {
			conds = append(conds, "expires_at > ?")
			args = append(args, store.FormatLearningTime(filter.Now))
		}
		conds = append(conds, `needs_scope_mapping = 0 AND NOT EXISTS (
			SELECT 1 FROM learning_outcomes o WHERE o.learning_id = learnings.id
			AND o.kind = 'contradiction' AND o.evidence_ref <> ''
			AND o.created_at >= COALESCE(learnings.last_confirmed_at, learnings.created_at)
		)`)
	}

	query := selectLearningColumns + " FROM learnings"
	if len(conds) > 0 {
		query += " WHERE " + strings.Join(conds, " AND ")
	}
	query += " ORDER BY scope_specificity DESC, id ASC"
	matchPaths := match && len(filter.Paths) > 0
	if !matchPaths {
		query += " LIMIT ?"
		args = append(args, limit)
	}

	return learningListQuery{sql: query, args: args, matchPaths: matchPaths, limit: limit}, nil
}

func (s *SQLiteStore) ListLearningHistory(ctx context.Context, learningID string) ([]domain.LearningHistory, error) {
	if !store.ValidLearningID(learningID, store.MaxLearningFieldBytes) {
		return nil, errors.New("invalid learning id")
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, learning_id, from_status, to_status, event_kind, actor,
		       reason, evidence_ref, predecessor_id, superseded_by, expires_at, created_at
		FROM learning_history
		WHERE learning_id = ?
		ORDER BY id ASC
	`, learningID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []domain.LearningHistory
	for rows.Next() {
		var h domain.LearningHistory
		var fromSt, toSt, expiresStr, createdStr string
		if err := rows.Scan(
			&h.ID, &h.LearningID, &fromSt, &toSt, &h.EventKind, &h.Actor,
			&h.Reason, &h.EvidenceRef, &h.PredecessorID, &h.SupersededBy, &expiresStr, &createdStr,
		); err != nil {
			return nil, err
		}
		h.FromStatus = domain.LearningStatus(fromSt)
		h.ToStatus = domain.LearningStatus(toSt)
		if expiresStr != "" {
			t, err := store.ParseLearningTime(expiresStr)
			if err != nil {
				return nil, err
			}
			h.ExpiresAt = &t
		}
		h.CreatedAt, err = store.ParseLearningTime(createdStr)
		if err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

func (s *SQLiteStore) ListLearningOutcomes(ctx context.Context, learningID string) ([]domain.LearningOutcome, error) {
	if !store.ValidLearningID(learningID, store.MaxLearningFieldBytes) {
		return nil, errors.New("invalid learning id")
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, learning_id, kind, source_kind, source_id, evidence_ref,
		       summary, actor, confirmed_by, confirmed_at, created_at
		FROM learning_outcomes
		WHERE learning_id = ?
		ORDER BY id ASC
	`, learningID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []domain.LearningOutcome
	for rows.Next() {
		var o domain.LearningOutcome
		var kind, sourceKind, confirmedAtStr, createdStr string
		if err := rows.Scan(
			&o.ID, &o.LearningID, &kind, &sourceKind, &o.SourceID, &o.EvidenceRef,
			&o.Summary, &o.Actor, &o.ConfirmedBy, &confirmedAtStr, &createdStr,
		); err != nil {
			return nil, err
		}
		o.Kind = domain.LearningOutcomeKind(kind)
		o.SourceKind = domain.LearningSourceKind(sourceKind)
		if confirmedAtStr != "" {
			t, err := store.ParseLearningTime(confirmedAtStr)
			if err != nil {
				return nil, err
			}
			o.ConfirmedAt = &t
		}
		o.CreatedAt, err = store.ParseLearningTime(createdStr)
		if err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

const selectLearningColumns = `
	SELECT id, version_digest, statement, scope_kind, scope_key, scope_specificity,
	       scope_json, source_kind, source_id, evidence_ref, provenance_verified,
	       author, predecessor_id, status, accepted_by, review_reason,
	       legacy_namespace, legacy_key, legacy_author, legacy_updated_at,
	       created_at, last_confirmed_at, expires_at, updated_at, injection_count`

type rowQuerier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

func getLearningRow(ctx context.Context, q rowQuerier, id string) (domain.Learning, error) {
	row := q.QueryRowContext(ctx, selectLearningColumns+" FROM learnings WHERE id = ?", id)
	rec, err := scanLearningRow(row)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.Learning{}, store.ErrNotFound
	}
	return rec, err
}

func scanLearningRow(row interface{ Scan(...any) error }) (domain.Learning, error) {
	var rec domain.Learning
	var scopeJSON, sourceKind, status string
	var provenanceVerified int
	var predecessorID, lastConfirmedStr, expiresStr sql.NullString
	var createdStr, updatedStr string

	err := row.Scan(
		&rec.ID, &rec.VersionDigest, &rec.Statement, &rec.ScopeKind, &rec.ScopeKey, &rec.ScopeSpecificity,
		&scopeJSON, &sourceKind, &rec.SourceID, &rec.EvidenceRef, &provenanceVerified,
		&rec.Author, &predecessorID, &status, &rec.AcceptedBy, &rec.ReviewReason,
		&rec.LegacyNamespace, &rec.LegacyKey, &rec.LegacyAuthor, &rec.LegacyUpdatedAt,
		&createdStr, &lastConfirmedStr, &expiresStr, &updatedStr, &rec.InjectionCount,
	)
	if err != nil {
		return domain.Learning{}, err
	}
	if err := json.Unmarshal([]byte(scopeJSON), &rec.Scope); err != nil {
		return domain.Learning{}, err
	}
	rec.SourceKind = domain.LearningSourceKind(sourceKind)
	rec.Status = domain.LearningStatus(status)
	rec.ProvenanceVerified = provenanceVerified == 1
	if predecessorID.Valid {
		rec.PredecessorID = predecessorID.String
	}
	rec.CreatedAt, err = store.ParseLearningTime(createdStr)
	if err != nil {
		return domain.Learning{}, err
	}
	rec.UpdatedAt, err = store.ParseLearningTime(updatedStr)
	if err != nil {
		return domain.Learning{}, err
	}
	if lastConfirmedStr.Valid && lastConfirmedStr.String != "" {
		t, err := store.ParseLearningTime(lastConfirmedStr.String)
		if err != nil {
			return domain.Learning{}, err
		}
		rec.LastConfirmedAt = &t
	}
	if expiresStr.Valid && expiresStr.String != "" {
		t, err := store.ParseLearningTime(expiresStr.String)
		if err != nil {
			return domain.Learning{}, err
		}
		rec.ExpiresAt = &t
	}
	return rec, nil
}

func insertLearningTx(ctx context.Context, tx *sql.Tx, rec domain.Learning, eventKind, actor, reason string) error {
	scopeBytes, err := json.Marshal(rec.Scope)
	if err != nil {
		return err
	}
	globs := rec.Scope.PathGlobs
	if globs == nil {
		globs = []string{}
	}
	globsBytes, err := json.Marshal(globs)
	if err != nil {
		return err
	}
	var pred sql.NullString
	if rec.PredecessorID != "" {
		pred = sql.NullString{String: rec.PredecessorID, Valid: true}
	}
	createdStr := store.FormatLearningTime(rec.CreatedAt)
	updatedStr := store.FormatLearningTime(rec.UpdatedAt)

	_, err = tx.ExecContext(ctx, `
		INSERT INTO learnings (
			id, version_digest, statement, scope_kind, scope_key, scope_specificity,
			scope_global, repo_remote_url, path_globs_json,
			incident_cluster, incident_namespace, incident_workload_kind, incident_symptom,
			needs_scope_mapping, scope_json,
			source_kind, source_id, evidence_ref, provenance_verified,
			author, predecessor_id, status, accepted_by, review_reason,
			legacy_namespace, legacy_key, legacy_author, legacy_updated_at,
			created_at, updated_at, injection_count
		) VALUES (
			?, ?, ?, ?, ?, ?,
			?, ?, ?,
			?, ?, ?, ?,
			?, ?,
			?, ?, ?, ?,
			?, ?, ?, ?, ?,
			?, ?, ?, ?,
			?, ?, ?
		)
	`,
		rec.ID, rec.VersionDigest, rec.Statement, rec.ScopeKind, rec.ScopeKey, rec.ScopeSpecificity,
		boolToInt(rec.Scope.Global), rec.Scope.RepoRemoteURL, string(globsBytes),
		rec.Scope.Cluster, rec.Scope.Namespace, rec.Scope.WorkloadKind, rec.Scope.Symptom,
		boolToInt(rec.Scope.NeedsScopeMapping), string(scopeBytes),
		string(rec.SourceKind), rec.SourceID, rec.EvidenceRef, boolToInt(rec.ProvenanceVerified),
		rec.Author, pred, string(rec.Status), rec.AcceptedBy, rec.ReviewReason,
		rec.LegacyNamespace, rec.LegacyKey, rec.LegacyAuthor, rec.LegacyUpdatedAt,
		createdStr, updatedStr, rec.InjectionCount,
	)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint failed") {
			return fmt.Errorf("%w: %v", store.ErrConflict, err)
		}
		return err
	}

	return appendLearningHistoryTx(ctx, tx, domain.LearningHistory{
		LearningID:    rec.ID,
		FromStatus:    "",
		ToStatus:      rec.Status,
		EventKind:     eventKind,
		Actor:         actor,
		Reason:        reason,
		EvidenceRef:   rec.EvidenceRef,
		PredecessorID: rec.PredecessorID,
		CreatedAt:     rec.CreatedAt,
	})
}

func appendLearningHistoryTx(ctx context.Context, tx *sql.Tx, h domain.LearningHistory) error {
	expiresStr := ""
	if h.ExpiresAt != nil {
		expiresStr = store.FormatLearningTime(*h.ExpiresAt)
	}
	_, err := tx.ExecContext(ctx, `
		INSERT INTO learning_history (
			learning_id, from_status, to_status, event_kind, actor,
			reason, evidence_ref, predecessor_id, superseded_by, expires_at, created_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`,
		h.LearningID, string(h.FromStatus), string(h.ToStatus), h.EventKind, h.Actor,
		h.Reason, h.EvidenceRef, h.PredecessorID, h.SupersededBy, expiresStr, store.FormatLearningTime(h.CreatedAt),
	)
	return err
}

// migrateLegacyLearningsTx imports pre-existing court_memory learning:* rows once as
// unverified proposals per ADR-0012, preserving original metadata and queuing ambiguous
// repository or path scopes for operator mapping.
func migrateLegacyLearningsTx(ctx context.Context, tx *sql.Tx) error {
	const pageSize = 32
	type legacyRow struct {
		rowID                             int64
		namespace, key, author, updatedAt string
		value                             sql.NullString
	}

	// Never pass a partial credential to a redactor. Oversized statements stay
	// in Court Memory and get a quarantine record instead of a clipped copy.
	const pageQuery = `SELECT rowid, namespace, key,
		CASE WHEN octet_length(value) <= ? THEN value END,
		CASE WHEN octet_length(author) <= ? THEN author ELSE '[oversized label]' END,
		CASE WHEN octet_length(updated_at) <= ? THEN updated_at ELSE '[oversized timestamp]' END
		FROM court_memory WHERE lower(trim(namespace, ?)) LIKE 'learning:%'`
	const spaces = " \t\n\v\f\r\u0085\u00a0\u1680\u2000\u2001\u2002\u2003\u2004\u2005\u2006\u2007\u2008\u2009\u200a\u2028\u2029\u202f\u205f\u3000"
	migratedAt := time.Now().UTC()
	var after int64
	for first := true; ; first = false {
		query := pageQuery
		args := []any{store.MaxLearningStatementBytes, store.MaxLearningFieldBytes, store.MaxLearningFieldBytes, spaces}
		if !first {
			query += " AND rowid > ?"
			args = append(args, after)
		}
		rows, err := tx.QueryContext(ctx, query+" ORDER BY rowid LIMIT ?", append(args, pageSize)...)
		if err != nil {
			return err
		}
		var page []legacyRow
		for rows.Next() {
			var r legacyRow
			if err := rows.Scan(&r.rowID, &r.namespace, &r.key, &r.value, &r.author, &r.updatedAt); err != nil {
				rows.Close()
				return err
			}
			page = append(page, r)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return err
		}
		if err := rows.Close(); err != nil {
			return err
		}
		for _, r := range page {
			if err := importLegacyLearning(ctx, tx, r.namespace, r.key, r.value, r.author, r.updatedAt, migratedAt); err != nil {
				return err
			}
			after = r.rowID
		}
		if len(page) < pageSize {
			return nil
		}
	}
}

func legacyMetadata(raw string) string {
	raw = strings.ToValidUTF8(strings.ReplaceAll(raw, "\x00", ""), "")
	return truncateUTF8(store.RedactLearningText(raw), store.MaxLearningFieldBytes)
}

func importLegacyLearning(ctx context.Context, tx *sql.Tx, origNS, origKey string, value sql.NullString, author, updatedAt string, migratedAt time.Time) error {
	stmt := strings.TrimSpace(store.RedactLearningText(value.String))
	cleanNS := legacyMetadata(origNS)
	cleanKey := legacyMetadata(origKey)
	cleanAuthor := legacyMetadata(author)
	cleanUpdatedAt := legacyMetadata(updatedAt)
	sum := sha256.Sum256([]byte(fmt.Sprintf("%d:%s|%d:%s", len(origNS), origNS, len(origKey), origKey)))
	id := "legacy-" + hex.EncodeToString(sum[:16])

	var scopeIn domain.LearningScope
	reason := "migrated_legacy_scope_mapping_required"
	if strings.EqualFold(strings.TrimSpace(origNS), "learning:global") {
		scopeIn = domain.LearningScope{
			Global:          true,
			LegacyNamespace: cleanNS,
		}
		reason = "migrated_legacy_learning"
	} else {
		scopeIn = domain.LearningScope{
			LegacyNamespace:   cleanNS,
			NeedsScopeMapping: true,
		}
	}

	createdAt, stampErr := store.ParseLearningTime(updatedAt)
	if stampErr != nil {
		createdAt = migratedAt
	}
	if stampErr != nil || !store.ValidLearningText(author, store.MaxLearningFieldBytes) ||
		!value.Valid || !store.ValidLearningText(stmt, store.MaxLearningStatementBytes) ||
		!store.ValidLearningText(origKey, store.MaxLearningFieldBytes) || !store.ValidLearningText(origNS, store.MaxLearningFieldBytes) {
		reason = "malformed_legacy_learning"
		scopeIn = domain.LearningScope{LegacyNamespace: cleanNS, NeedsScopeMapping: true}
		if strings.TrimSpace(cleanNS) == "" {
			scopeIn.LegacyNamespace = "learning:unmapped"
		}
		if !store.ValidLearningText(stmt, store.MaxLearningStatementBytes) {
			stmt = "[invalid legacy statement]"
		}
	}

	scope, scopeKind, scopeKey, specificity, err := store.NormalizeLearningScope(scopeIn, true)
	if err != nil {
		return err
	}

	rec := domain.Learning{
		ID:                 id,
		Statement:          stmt,
		Scope:              scope,
		ScopeKind:          scopeKind,
		ScopeKey:           scopeKey,
		ScopeSpecificity:   specificity,
		SourceKind:         domain.LearningSourceLegacy,
		ProvenanceVerified: false,
		Author:             "legacy-migration",
		Status:             domain.LearningStatusProposed,
		ReviewReason:       reason,
		LegacyNamespace:    cleanNS,
		LegacyKey:          cleanKey,
		LegacyAuthor:       cleanAuthor,
		LegacyUpdatedAt:    cleanUpdatedAt,
		CreatedAt:          createdAt,
		UpdatedAt:          migratedAt,
	}
	digest, err := store.ComputeLearningVersionDigest(rec)
	if err != nil {
		return err
	}
	rec.VersionDigest = digest

	return insertLearningTx(ctx, tx, rec, "migrated", "system", reason)
}

// Compare against the latest acceptance, including reacceptance after review.
func subsequentLearningOutcome(ctx context.Context, tx *sql.Tx, id, createdAt string) error {
	var acceptedAt string
	err := tx.QueryRowContext(ctx, `SELECT created_at FROM learning_history
		WHERE learning_id = ? AND event_kind = 'accepted' ORDER BY id DESC LIMIT 1`, id).Scan(&acceptedAt)
	if err != nil {
		return err
	}
	if createdAt <= acceptedAt {
		return fmt.Errorf("%w: outcome must follow acceptance", store.ErrConflict)
	}
	return nil
}

func requireLearningEvidence(ctx context.Context, tx *sql.Tx, kind domain.LearningSourceKind, id, ref string) error {
	verified, err := learningEvidenceExists(ctx, tx, kind, id, ref)
	if err != nil {
		return err
	}
	if !verified {
		return fmt.Errorf("%w: learning evidence does not resolve for its source", store.ErrConflict)
	}
	return nil
}

func learningEvidenceExists(ctx context.Context, tx *sql.Tx, kind domain.LearningSourceKind, id, ref string) (bool, error) {
	prefix, name, ok := strings.Cut(ref, ":")
	if !ok || id == "" || name == "" {
		return false, nil
	}
	var query string
	switch kind {
	case domain.LearningSourceTask, domain.LearningSourceAgent, domain.LearningSourceVerifier:
		if prefix != "artifact" {
			return false, nil
		}
		query = `SELECT count(*) = 1 FROM tasks t, json_each(CASE WHEN json_valid(t.artifacts_json) THEN t.artifacts_json ELSE '[]' END) a
			WHERE t.id = ? AND a.type = 'object' AND a.value ->> 'name' = ?
			AND json_type(a.value, '$.text') = 'text' AND trim(a.value ->> 'text') <> ''`
	case domain.LearningSourceCouncil:
		if prefix != "relic" {
			return false, nil
		}
		query = `SELECT EXISTS (SELECT 1 FROM councils c JOIN conclave_relics r ON r.conclave_id = c.id
			WHERE c.id = ? AND r.name = ? AND trim(r.body) <> '')`
	case domain.LearningSourceIncident, domain.LearningSourceShadewright:
		if prefix != "relic" {
			return false, nil
		}
		// Contract 3 derives the incident room from the external incident ID.
		id = fmt.Sprintf("ccl-shadewright-%x", sha256.Sum256([]byte("shadewright\x00"+id)))
		query = `SELECT EXISTS (SELECT 1 FROM conclaves c JOIN conclave_relics r ON r.conclave_id = c.id
			WHERE c.id = ? AND c.title = 'Shadewright Incident ' || c.id AND r.name = ? AND trim(r.body) <> '')`
	default:
		return false, nil
	}
	var verified bool
	err := tx.QueryRowContext(ctx, query, id, name).Scan(&verified)
	return verified, err
}

func hasUnaddressedContradictionTx(ctx context.Context, tx *sql.Tx, rec domain.Learning) (bool, error) {
	since := rec.CreatedAt
	if rec.LastConfirmedAt != nil {
		since = *rec.LastConfirmedAt
	}
	var exists int
	err := tx.QueryRowContext(ctx, `
		SELECT 1 FROM learning_outcomes
		WHERE learning_id = ?
		  AND kind = 'contradiction'
		  AND evidence_ref <> ''
		  AND created_at >= ?
		LIMIT 1
	`, rec.ID, store.FormatLearningTime(since)).Scan(&exists)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func truncateUTF8(s string, maxBytes int) string {
	if len(s) <= maxBytes {
		return s
	}
	if maxBytes <= 0 {
		return ""
	}
	n := maxBytes
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}
