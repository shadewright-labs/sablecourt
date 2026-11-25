package sqlite

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"slices"
	"time"

	"github.com/shadewright-labs/sablecourt/internal/domain"
	"github.com/shadewright-labs/sablecourt/internal/store"
)

func (s *SQLiteStore) ListReconcileCouncils(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT c.id FROM councils p JOIN conclaves c ON c.id=p.id
		WHERE c.state IN ('active','input-required') OR p.cleanup_pending = 1 OR EXISTS (
			SELECT 1 FROM council_attempts a JOIN tasks t ON t.id=a.task_id
			WHERE a.council_id=c.id AND t.state IN ('submitted','working','input-required')
		)
		ORDER BY c.created_at DESC, c.id
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func (s *SQLiteStore) CouncilSnapshot(ctx context.Context, id string) (store.CouncilSnapshot, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return store.CouncilSnapshot{}, err
	}
	defer tx.Rollback()
	snap, err := readCouncilSnapshot(ctx, tx, id)
	if err != nil {
		return store.CouncilSnapshot{}, err
	}
	if err := tx.Commit(); err != nil {
		return store.CouncilSnapshot{}, err
	}
	return snap, nil
}

func readCouncilSnapshot(ctx context.Context, tx *sql.Tx, id string) (store.CouncilSnapshot, error) {
	var snap store.CouncilSnapshot
	p, err := readCouncilPolicy(ctx, tx, id)
	if err != nil {
		return snap, err
	}
	if p == nil {
		return snap, store.ErrNotFound
	}
	snap.CouncilPolicy = *p
	var cleanupPending int
	if err := tx.QueryRowContext(ctx, "SELECT cleanup_pending FROM councils WHERE id=?", id).Scan(&cleanupPending); err != nil {
		return snap, err
	}
	snap.CleanupPending = cleanupPending != 0
	r := &snap.Session
	var created, updated string
	err = tx.QueryRowContext(ctx, `SELECT id,template_name,title,state,ward_reason,created_at,updated_at FROM conclaves WHERE id=?`, id).
		Scan(&r.ID, &r.TemplateName, &r.Title, &r.State, &r.WardReason, &created, &updated)
	if err != nil {
		return snap, err
	}
	if r.CreatedAt, err = time.Parse(time.RFC3339Nano, created); err != nil {
		return snap, err
	}
	if r.UpdatedAt, err = time.Parse(time.RFC3339Nano, updated); err != nil {
		return snap, err
	}
	answers, decrees, err := councilMessages(ctx, tx, id)
	if err != nil {
		return snap, err
	}
	snap.Decrees = decrees
	relics, err := councilRelics(ctx, tx, id)
	if err != nil {
		return snap, err
	}
	rows, err := tx.QueryContext(ctx, `
		SELECT a.round,a.role,a.attempt,a.task_id,t.state,a.answer_id,a.nothing_to_add,a.policy_routed
		FROM council_attempts a JOIN tasks t ON t.id=a.task_id WHERE a.council_id=?
	`, id)
	if err != nil {
		return snap, err
	}
	defer rows.Close()
	seats := map[string]int{}
	for i, m := range p.Members {
		seats[m.Vassal.Role] = i
	}
	policyRouted := map[string]bool{}
	for rows.Next() {
		var routed bool
		var a store.CouncilAttempt
		var answer sql.NullInt64
		if err := rows.Scan(&a.Round, &a.Role, &a.Number, &a.TaskID, &a.State, &answer, &a.NothingToAdd, &routed); err != nil {
			return snap, err
		}
		if _, ok := seats[a.Role]; !ok {
			return snap, errors.New("unknown reserved seat")
		}
		if answer.Valid {
			a.Answer = answers[answer.Int64]
			if a.Answer == nil || a.Answer.FromRole != a.Role || a.Answer.TaskID != a.TaskID || a.State != domain.TaskStateCompleted {
				return snap, errors.New("invalid council answer provenance")
			}
		}
		policyRouted[a.TaskID] = routed
		a.Relics = relics[a.TaskID]
		snap.Attempts = append(snap.Attempts, a)
	}
	if err := rows.Err(); err != nil {
		return snap, err
	}
	slices.SortFunc(snap.Attempts, func(a, b store.CouncilAttempt) int {
		return cmp.Or(cmp.Compare(a.Round, b.Round), cmp.Compare(seats[a.Role], seats[b.Role]), cmp.Compare(a.Number, b.Number))
	})
	snap.Turns = []store.CouncilTurn{}
	for i, a := range snap.Attempts {
		if i+1 < len(snap.Attempts) && snap.Attempts[i+1].Round == a.Round && snap.Attempts[i+1].Role == a.Role {
			continue
		}
		// A daemon final is shared according to CanMessage and zones in the round
		// packet. Keep the addressed message intact in the attempt history.
		if a.Answer != nil && policyRouted[a.TaskID] {
			answer := *a.Answer
			answer.ToRole = "all"
			a.Answer = &answer
		}
		snap.Turns = append(snap.Turns, store.CouncilTurn{Round: a.Round, Role: a.Role, Starts: a.Number, TaskID: a.TaskID, State: a.State, Answer: a.Answer, NothingToAdd: a.NothingToAdd})
	}
	return snap, nil
}

func councilMessages(ctx context.Context, tx *sql.Tx, id string) (map[int64]*domain.ConclaveMessage, []domain.ConclaveMessage, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT m.id,m.conclave_id,m.from_role,m.to_role,m.kind,m.body,m.task_id,m.created_at,a.answer_id IS NOT NULL
		FROM conclave_messages m LEFT JOIN council_attempts a ON a.answer_id=m.id
		WHERE m.conclave_id=? AND (a.answer_id IS NOT NULL OR (from_role='operator' AND kind IN ('decree','decision') AND m.task_id=''))
		ORDER BY m.id
	`, id)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	answers := map[int64]*domain.ConclaveMessage{}
	var decrees []domain.ConclaveMessage
	for rows.Next() {
		var m domain.ConclaveMessage
		var created string
		var answer bool
		if err := rows.Scan(&m.ID, &m.ConclaveID, &m.FromRole, &m.ToRole, &m.Kind, &m.Body, &m.TaskID, &created, &answer); err != nil {
			return nil, nil, err
		}
		if m.CreatedAt, err = time.Parse(time.RFC3339Nano, created); err != nil {
			return nil, nil, err
		}
		if answer {
			answers[m.ID] = &m
		} else {
			decrees = append(decrees, m)
		}
	}
	return answers, decrees, rows.Err()
}

func councilRelics(ctx context.Context, tx *sql.Tx, id string) (map[string][]domain.ConclaveRelic, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT council_task_id,conclave_id,name,author_role,content_type,body,sha256,created_at
		FROM conclave_relics WHERE conclave_id=? AND council_task_id IS NOT NULL ORDER BY name
	`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][]domain.ConclaveRelic{}
	for rows.Next() {
		var r domain.ConclaveRelic
		var task, created string
		if err := rows.Scan(&task, &r.ConclaveID, &r.Name, &r.AuthorRole, &r.ContentType, &r.Body, &r.SHA256, &created); err != nil {
			return nil, err
		}
		if r.CreatedAt, err = time.Parse(time.RFC3339Nano, created); err != nil {
			return nil, err
		}
		out[task] = append(out[task], r)
	}
	return out, rows.Err()
}
