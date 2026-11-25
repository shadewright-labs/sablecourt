package sqlite

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/shadewright-labs/sablecourt/internal/domain"
	"github.com/shadewright-labs/sablecourt/internal/redact"
	"github.com/shadewright-labs/sablecourt/internal/store"
)

type attemptEvidence struct {
	room, role string
	taskState  domain.TaskState
	roomState  domain.ConclaveState
	answer     sql.NullInt64
	vote       bool
	policy     *store.CouncilPolicy
}

func lockAttempt(ctx context.Context, tx *sql.Tx, task string) (attemptEvidence, error) {
	var a attemptEvidence
	err := tx.QueryRowContext(ctx, `
		UPDATE council_attempts SET task_id=task_id WHERE task_id=?
		RETURNING council_id,role,answer_id,nothing_to_add
	`, task).Scan(&a.room, &a.role, &a.answer, &a.vote)
	if errors.Is(err, sql.ErrNoRows) {
		return a, store.ErrNotFound
	}
	if err != nil {
		return a, err
	}
	err = tx.QueryRowContext(ctx, "SELECT t.state,c.state FROM tasks t, conclaves c WHERE t.id=? AND c.id=?", task, a.room).Scan(&a.taskState, &a.roomState)
	if err != nil {
		return a, err
	}
	a.policy, err = readCouncilPolicy(ctx, tx, a.room)
	if err == nil && a.policy == nil {
		err = store.ErrNotFound
	}
	return a, err
}

func (s *SQLiteStore) RecordCouncilAnswer(ctx context.Context, msg domain.ConclaveMessage, vote bool) (int64, string, error) {
	return store.AppendReservedCouncilAnswer(ctx, s, msg, vote, store.ConclaveLimits{})
}

func (s *SQLiteStore) AcceptCouncilAnswer(ctx context.Context, task string, message int64, vote bool) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	a, err := lockAttempt(ctx, tx, task)
	if err != nil {
		return err
	}
	if a.answer.Valid {
		if a.answer.Int64 == message && a.vote == vote {
			return nil
		}
		return store.ErrConflict
	}
	if a.taskState != domain.TaskStateCompleted || (a.roomState != domain.ConclaveStateActive && a.roomState != domain.ConclaveStateInputRequired) {
		return errors.New("council answer requires a completed task in an open room")
	}
	var recipient, body string
	err = tx.QueryRowContext(ctx, `
		SELECT to_role,body FROM conclave_messages WHERE id=? AND conclave_id=? AND task_id=? AND from_role=?
		AND kind IN ('review','question','proposal','blocker')
	`, message, a.room, task, a.role).Scan(&recipient, &body)
	if err != nil {
		return err
	}
	if !store.ValidCouncilText(body) || redact.String(body) != body {
		return errors.New("invalid council answer text")
	}
	if !permittedRecipient(a.policy, a.role, recipient) {
		return errors.New("invalid council answer recipient")
	}
	result, err := tx.ExecContext(ctx, `
		UPDATE council_attempts AS a SET answer_id=?,nothing_to_add=? WHERE task_id=?
		AND NOT EXISTS (
			SELECT 1 FROM council_attempts newer WHERE newer.council_id=a.council_id
			AND newer.round=a.round AND newer.role=a.role AND newer.attempt>a.attempt
		)
	`, message, vote, task)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return store.ErrConflict
	}
	return tx.Commit()
}

func permittedRecipient(p *store.CouncilPolicy, role, to string) bool {
	var routes []string
	known := to == "all" || to == "operator"
	for _, m := range p.Members {
		if m.Vassal.Role == role {
			routes = m.Vassal.CanMessage
		}
		known = known || m.Vassal.Role == to
	}
	return known && (slices.Contains(routes, to) || slices.Contains(routes, "all"))
}

func (s *SQLiteStore) PutCouncilRelic(ctx context.Context, task string, r domain.ConclaveRelic) error {
	if r.ContentType == "" {
		r.ContentType = "text/plain"
	}
	if r.Name == store.CouncilDigestRelicName || len(r.Name) > store.MaxCouncilRelicName || !store.ValidCouncilText(r.Name) || !store.ValidCouncilText(r.ContentType) || redact.String(r.Name) != r.Name || redact.String(r.ContentType) != r.ContentType {
		return errors.New("invalid council relic metadata")
	}
	if len(r.Body) > store.MaxConclaveTurnInput || !utf8.ValidString(r.Body) || strings.TrimSpace(r.Body) == "" {
		return errors.New("invalid council relic body")
	}
	r.Body = redact.String(r.Body)
	if len(r.Body) > store.MaxConclaveTurnInput {
		return errors.New("redacted council relic too large")
	}
	r.SHA256 = fmt.Sprintf("%x", sha256.Sum256([]byte(r.Body)))
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	a, err := lockAttempt(ctx, tx, task)
	if err != nil {
		return err
	}
	if a.room != r.ConclaveID || a.role != r.AuthorRole {
		return errors.New("council relic does not belong to attempt")
	}
	var prevTask sql.NullString
	var sum, kind string
	err = tx.QueryRowContext(ctx, "SELECT council_task_id,sha256,content_type FROM conclave_relics WHERE conclave_id=? AND name=?", a.room, r.Name).Scan(&prevTask, &sum, &kind)
	if err == nil {
		if prevTask.Valid && prevTask.String == task && sum == r.SHA256 && kind == r.ContentType {
			return nil
		}
		return store.ErrConflict
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if a.roomState != domain.ConclaveStateActive || !time.Now().Before(a.policy.Deadline) || a.taskState != domain.TaskStateWorking && a.taskState != domain.TaskStateSubmitted {
		return errors.New("council attempt cannot publish a relic")
	}
	var count int
	if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM conclave_relics WHERE council_task_id=?", task).Scan(&count); err != nil {
		return err
	}
	if count >= store.MaxCouncilRelicsPerAttempt {
		return errors.New("council relic limit reached")
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO conclave_relics(conclave_id,name,author_role,content_type,body,sha256,created_at,council_task_id)
		VALUES (?,?,?,?,?,?,?,?)`, a.room, r.Name, a.role, r.ContentType, r.Body, r.SHA256, formatTime(r.CreatedAt), task)
	if err != nil {
		return err
	}
	return tx.Commit()
}
