package postgres

import (
	"context"
	"database/sql"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"strings"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/shadewright-labs/sablecourt/internal/domain"
	"github.com/shadewright-labs/sablecourt/internal/store"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

type PostgresStore struct {
	db *sql.DB
}

var _ store.Store = (*PostgresStore)(nil)

func Open(dsn string) (*PostgresStore, error) {
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, fmt.Errorf("open postgres: %w", err)
	}
	db.SetMaxOpenConns(16)
	db.SetMaxIdleConns(4)
	db.SetConnMaxLifetime(30 * time.Minute)
	return &PostgresStore{db: db}, nil
}

func (s *PostgresStore) Close() error {
	return s.db.Close()
}

func (s *PostgresStore) Migrate(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version TEXT PRIMARY KEY,
			applied_at TEXT NOT NULL
		);
	`); err != nil {
		return fmt.Errorf("create schema_migrations: %w", err)
	}

	entries, err := fs.ReadDir(migrationFS, "migrations")
	if err != nil {
		return fmt.Errorf("read migrations: %w", err)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })

	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".sql") {
			continue
		}
		version := entry.Name()
		var existing string
		err := s.db.QueryRowContext(ctx, `SELECT version FROM schema_migrations WHERE version = $1`, version).Scan(&existing)
		if err == nil {
			continue
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("check migration %s: %w", version, err)
		}

		sqlBytes, err := migrationFS.ReadFile("migrations/" + version)
		if err != nil {
			return fmt.Errorf("read migration %s: %w", version, err)
		}

		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, string(sqlBytes)); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("apply migration %s: %w", version, err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO schema_migrations (version, applied_at) VALUES ($1, $2)`, version, nowRFC3339()); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("record migration %s: %w", version, err)
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

func (s *PostgresStore) UpsertWorkload(ctx context.Context, w domain.WorkloadRecord) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO workloads (name, zone, class, manifest_digest, agent_card_json, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT(name) DO UPDATE SET
			zone = EXCLUDED.zone,
			class = EXCLUDED.class,
			manifest_digest = EXCLUDED.manifest_digest,
			agent_card_json = EXCLUDED.agent_card_json,
			updated_at = EXCLUDED.updated_at
	`, w.Name, string(w.Zone), string(w.Class), w.ManifestDigest, w.AgentCardJSON, formatTime(w.UpdatedAt))
	return err
}

func (s *PostgresStore) ListWorkloads(ctx context.Context) ([]domain.WorkloadRecord, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT name, zone, class, manifest_digest, agent_card_json, updated_at
		FROM workloads ORDER BY name ASC
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []domain.WorkloadRecord
	for rows.Next() {
		var rec domain.WorkloadRecord
		var zone, class, updated string
		if err := rows.Scan(&rec.Name, &zone, &class, &rec.ManifestDigest, &rec.AgentCardJSON, &updated); err != nil {
			return nil, err
		}
		rec.Zone = domain.WorkloadZone(zone)
		rec.Class = domain.WorkloadClass(class)
		rec.UpdatedAt, _ = time.Parse(time.RFC3339Nano, updated)
		out = append(out, rec)
	}
	return out, rows.Err()
}

func (s *PostgresStore) CreateTask(ctx context.Context, t domain.Task) error {
	now := nowRFC3339()
	created := now
	if !t.CreatedAt.IsZero() {
		created = t.CreatedAt.UTC().Format(time.RFC3339Nano)
	}
	updated := created
	if !t.UpdatedAt.IsZero() {
		updated = t.UpdatedAt.UTC().Format(time.RFC3339Nano)
	}
	err := store.InsertTask(ctx, s.db, nil, t, func(inserter store.TaskInserter) error {
		_, err := inserter.ExecContext(ctx, `
			INSERT INTO tasks (
				id, parent_task_id, idempotency_key, workload_name, state,
				input_json, artifacts_json, budget_ceiling_json, created_at, updated_at
			) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		`, t.ID, t.ParentTaskID, t.IdempotencyKey, t.WorkloadName, string(t.State),
			defaultJSON(t.InputJSON, "{}"), defaultJSON(t.ArtifactsJSON, "[]"), defaultJSON(t.BudgetCeilingJSON, "{}"), created, updated)
		return err
	})
	if err != nil && strings.Contains(err.Error(), "duplicate key") {
		return fmt.Errorf("%w: %v", store.ErrConflict, err)
	}
	return err
}

func (s *PostgresStore) GetTask(ctx context.Context, id string) (domain.Task, error) {
	return store.GetTask(ctx, s.db, id)
}

func (s *PostgresStore) GetTaskByIdempotencyKey(ctx context.Context, key string) (domain.Task, error) {
	return store.GetTaskByIdempotencyKey(ctx, s.db, key)
}

func (s *PostgresStore) ListTasks(ctx context.Context) ([]domain.Task, error) {
	return store.ListTasks(ctx, s.db)
}

func (s *PostgresStore) ListReconcileTasks(ctx context.Context, handleIDs []string) ([]domain.Task, error) {
	return store.ListReconcileTasks(ctx, s.db, handleIDs, "SELECT json_array_elements_text($1::json)")
}

func (s *PostgresStore) ListTaskEvents(ctx context.Context, taskID string) ([]domain.TaskEvent, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, task_id, from_state, to_state, reason, fact_json, created_at
		FROM task_events WHERE task_id = $1 ORDER BY id ASC
	`, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []domain.TaskEvent
	for rows.Next() {
		var ev domain.TaskEvent
		var from, to, created string
		if err := rows.Scan(&ev.ID, &ev.TaskID, &from, &to, &ev.Reason, &ev.FactJSON, &created); err != nil {
			return nil, err
		}
		ev.FromState = domain.TaskState(from)
		ev.ToState = domain.TaskState(to)
		ev.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
		out = append(out, ev)
	}
	return out, rows.Err()
}

func (s *PostgresStore) AppendTaskMessage(ctx context.Context, msg domain.TaskMessage) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO task_messages (task_id, role, content, created_at)
		VALUES ($1, $2, $3, $4)
	`, msg.TaskID, msg.Role, msg.Content, formatTime(msg.CreatedAt))
	return err
}

func (s *PostgresStore) ListTaskMessages(ctx context.Context, taskID string) ([]domain.TaskMessage, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, task_id, role, content, created_at
		FROM task_messages WHERE task_id = $1 ORDER BY id ASC
	`, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []domain.TaskMessage
	for rows.Next() {
		var m domain.TaskMessage
		var created string
		if err := rows.Scan(&m.ID, &m.TaskID, &m.Role, &m.Content, &created); err != nil {
			return nil, err
		}
		m.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
		out = append(out, m)
	}
	return out, rows.Err()
}

func (s *PostgresStore) UpdateTaskState(ctx context.Context, taskID string, from, to domain.TaskState, reason, factJSON string) error {
	tx, to, reason, err := store.BeginTaskTransition(ctx, s.db, nil, to, reason)
	if err != nil {
		return err
	}
	now := nowRFC3339()
	res, err := tx.ExecContext(ctx, `
		UPDATE tasks SET state = $1, updated_at = $2, exit_pending = $3 WHERE id = $4 AND state = $5
	`, string(to), now, reason == "runtime_exit_observed" && (to == domain.TaskStateCompleted || to == domain.TaskStateFailed), taskID, string(from))
	if err != nil {
		_ = tx.Rollback()
		return err
	}
	affected, err := res.RowsAffected()
	if err != nil || affected == 0 {
		_ = tx.Rollback()
		if err != nil {
			return err
		}
		return store.ErrConflict
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO task_events (task_id, from_state, to_state, reason, fact_json, created_at)
		VALUES ($1, $2, $3, $4, $5, $6)
	`, taskID, string(from), string(to), reason, defaultJSON(factJSON, "{}"), now); err != nil {
		_ = tx.Rollback()
		return err
	}
	return store.CommitTaskTransition(ctx, tx, taskID, to, now)
}

func (s *PostgresStore) UpdateTaskArtifacts(ctx context.Context, taskID, artifactsJSON string) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE tasks SET artifacts_json = $1, updated_at = $2 WHERE id = $3
	`, defaultJSON(artifactsJSON, "[]"), nowRFC3339(), taskID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return store.ErrNotFound
	}
	return nil
}

func (s *PostgresStore) RecordHeartbeat(ctx context.Context, taskID string, at time.Time) error {
	ts := formatTime(at)
	res, err := s.db.ExecContext(ctx, `
		UPDATE tasks SET last_heartbeat_at = $1, updated_at = $2 WHERE id = $3
	`, ts, ts, taskID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return store.ErrNotFound
	}
	return nil
}

func (s *PostgresStore) CreateApproval(ctx context.Context, a domain.Approval) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO approvals (
			id, task_id, workload_name, capability, payload_canonical_json,
			payload_sha256, status, decided_by, proof_json, expires_at, created_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, '', '{}', $8, $9)
	`, a.ID, a.TaskID, a.WorkloadName, a.Capability, a.PayloadCanonicalJSON,
		a.PayloadSHA256, string(domain.ApprovalPending), a.ExpiresAt.UTC().Format(time.RFC3339Nano), formatTime(a.CreatedAt))
	return err
}

func (s *PostgresStore) GetApproval(ctx context.Context, id string) (domain.Approval, error) {
	var a domain.Approval
	var st, expires, created string
	var decided sql.NullString
	err := s.db.QueryRowContext(ctx, `
		SELECT id, task_id, workload_name, capability, payload_canonical_json,
		       payload_sha256, status, decided_by, proof_json, expires_at, created_at, decided_at
		FROM approvals WHERE id = $1
	`, id).Scan(
		&a.ID, &a.TaskID, &a.WorkloadName, &a.Capability, &a.PayloadCanonicalJSON,
		&a.PayloadSHA256, &st, &a.DecidedBy, &a.ProofJSON, &expires, &created, &decided,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.Approval{}, store.ErrNotFound
	}
	if err != nil {
		return domain.Approval{}, err
	}
	a.Status = domain.ApprovalStatus(st)
	a.ExpiresAt, _ = time.Parse(time.RFC3339Nano, expires)
	a.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
	if decided.Valid {
		if parsed, err := time.Parse(time.RFC3339Nano, decided.String); err == nil {
			a.DecidedAt = &parsed
		}
	}
	return a, nil
}

func (s *PostgresStore) ListApprovals(ctx context.Context, status domain.ApprovalStatus) ([]domain.Approval, error) {
	query := `
		SELECT id, task_id, workload_name, capability, payload_canonical_json,
		       payload_sha256, status, decided_by, proof_json, expires_at, created_at, decided_at
		FROM approvals
	`
	var args []any
	if status != "" {
		query += ` WHERE status = $1`
		args = append(args, string(status))
	}
	query += ` ORDER BY created_at DESC`

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []domain.Approval
	for rows.Next() {
		var a domain.Approval
		var st, expires, created string
		var decided sql.NullString
		if err := rows.Scan(
			&a.ID, &a.TaskID, &a.WorkloadName, &a.Capability, &a.PayloadCanonicalJSON,
			&a.PayloadSHA256, &st, &a.DecidedBy, &a.ProofJSON, &expires, &created, &decided,
		); err != nil {
			return nil, err
		}
		a.Status = domain.ApprovalStatus(st)
		a.ExpiresAt, _ = time.Parse(time.RFC3339Nano, expires)
		a.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
		if decided.Valid {
			if parsed, err := time.Parse(time.RFC3339Nano, decided.String); err == nil {
				a.DecidedAt = &parsed
			}
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func (s *PostgresStore) DecideApproval(ctx context.Context, id, expectedSHA256 string, status domain.ApprovalStatus, decidedBy, proofJSON string) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE approvals
		SET status = $1, decided_by = $2, proof_json = $3, decided_at = $4
		WHERE id = $5 AND status = $6 AND payload_sha256 = $7
	`, string(status), decidedBy, defaultJSON(proofJSON, "{}"), nowRFC3339(), id, string(domain.ApprovalPending), expectedSHA256)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return store.ErrConflict
	}
	return nil
}

func (s *PostgresStore) ConsumeApproval(ctx context.Context, id, taskID, capability, payloadSHA256 string) error {
	// One statement in its own implicit transaction, compared against the
	// database's clock. expires_at is RFC 3339 text and is cast to
	// timestamptz, which compares it as a time (as text, "...:05Z" would sort
	// after "...:05.5Z") and rounds it to microseconds. clock_timestamp() is
	// read when the predicate is evaluated, not at statement start as now()
	// is, and PostgreSQL evaluates the predicate again after waiting for a
	// concurrent update of the row, so neither that wait nor a pause of the
	// daemon before the statement can carry an earlier time past the expiry.
	res, err := s.db.ExecContext(ctx, `
		UPDATE approvals SET status = $1
		WHERE id = $2 AND status = $3 AND task_id = $4 AND capability = $5 AND payload_sha256 = $6
		  AND expires_at::timestamptz > clock_timestamp()
	`, string(domain.ApprovalConsumed), id, string(domain.ApprovalApproved), taskID, capability, payloadSHA256)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 1 {
		return nil
	}
	if n != 0 {
		return fmt.Errorf("consume approval: %d rows updated", n)
	}
	a, err := s.GetApproval(ctx, id)
	if err != nil {
		return err
	}
	return store.ConsumeRefusal(a, taskID, capability, payloadSHA256)
}

func (s *PostgresStore) CreateLease(ctx context.Context, lease domain.Lease) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO leases (token_sha256, task_id, workload_name, expires_at, created_at)
		VALUES ($1, $2, $3, $4, $5)
	`, lease.TokenSHA256, lease.TaskID, lease.WorkloadName, lease.ExpiresAt.UTC().Format(time.RFC3339Nano), formatTime(lease.CreatedAt))
	if err != nil && strings.Contains(err.Error(), "duplicate key") {
		return fmt.Errorf("%w: %v", store.ErrConflict, err)
	}
	return err
}

func (s *PostgresStore) GetActiveLeaseByTokenHash(ctx context.Context, tokenSHA256 string, now time.Time) (domain.Lease, error) {
	var lease domain.Lease
	var expires, created string
	var revoked sql.NullString
	err := s.db.QueryRowContext(ctx, `
		SELECT token_sha256, task_id, workload_name, expires_at, revoked_at, created_at
		FROM leases WHERE token_sha256 = $1
	`, tokenSHA256).Scan(&lease.TokenSHA256, &lease.TaskID, &lease.WorkloadName, &expires, &revoked, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.Lease{}, store.ErrNotFound
	}
	if err != nil {
		return domain.Lease{}, err
	}
	lease.ExpiresAt, _ = time.Parse(time.RFC3339Nano, expires)
	lease.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
	if revoked.Valid || !now.UTC().Before(lease.ExpiresAt) {
		return domain.Lease{}, store.ErrNotFound
	}
	return lease, nil
}

func (s *PostgresStore) RenewTaskLeases(ctx context.Context, taskID string, now, expiresAt time.Time) error {
	return store.RenewTaskLeases(ctx, s.db, taskID, now, expiresAt)
}

func (s *PostgresStore) RevokeTaskLeases(ctx context.Context, taskID string) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE leases SET revoked_at = $1 WHERE task_id = $2 AND revoked_at IS NULL
	`, nowRFC3339(), taskID)
	return err
}

func (s *PostgresStore) RecordBudgetUsage(ctx context.Context, entry domain.BudgetEntry) error {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := lockPostgresBudgetRows(ctx, tx, entry.TaskID, entry.WorkloadName); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO budget_ledger (
			task_id, workload_name, provider, model, input_tokens, output_tokens, cost_microusd, created_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
	`, entry.TaskID, entry.WorkloadName, entry.Provider, entry.Model,
		entry.InputTokens, entry.OutputTokens, entry.CostMicroUSD, formatTime(entry.CreatedAt)); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *PostgresStore) ReserveBudget(ctx context.Context, req store.BudgetReservationRequest) (store.BudgetReservationResult, error) {
	return store.ReserveBudgetTx(ctx, s.db, lockPostgresBudgetRows, req)
}

func (s *PostgresStore) SettleBudgetReservation(ctx context.Context, reservationID string, actual *domain.BudgetEntry) error {
	return store.SettleBudgetReservationTx(ctx, s.db, lockPostgresBudgetRows, reservationID, actual)
}

func lockPostgresBudgetRows(ctx context.Context, tx *sql.Tx, taskID, workloadName string) error {
	var wl string
	if err := tx.QueryRowContext(ctx, `SELECT name FROM workloads WHERE name = $1 FOR UPDATE`, workloadName).Scan(&wl); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return store.ErrNotFound
		}
		return err
	}
	var id string
	if err := tx.QueryRowContext(ctx, `SELECT id FROM tasks WHERE id = $1 FOR UPDATE`, taskID).Scan(&id); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return store.ErrNotFound
		}
		return err
	}
	return nil
}

func (s *PostgresStore) TaskBudgetTotals(ctx context.Context, taskID string) (domain.BudgetTotals, error) {
	var totals domain.BudgetTotals
	err := s.db.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(input_tokens), 0), COALESCE(SUM(output_tokens), 0), COALESCE(SUM(cost_microusd), 0)
		FROM budget_ledger WHERE task_id = $1
	`, taskID).Scan(&totals.InputTokens, &totals.OutputTokens, &totals.CostMicroUSD)
	return totals, err
}

func (s *PostgresStore) WorkloadDailyBudgetTotals(ctx context.Context, workloadName string, dayStartUTC time.Time) (domain.BudgetTotals, error) {
	var totals domain.BudgetTotals
	err := s.db.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(input_tokens), 0), COALESCE(SUM(output_tokens), 0), COALESCE(SUM(cost_microusd), 0)
		FROM budget_ledger WHERE workload_name = $1 AND created_at >= $2
	`, workloadName, dayStartUTC.UTC().Format(time.RFC3339Nano)).Scan(&totals.InputTokens, &totals.OutputTokens, &totals.CostMicroUSD)
	return totals, err
}

func (s *PostgresStore) TotalBudgetTotals(ctx context.Context) (domain.BudgetTotals, error) {
	var totals domain.BudgetTotals
	err := s.db.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(input_tokens), 0), COALESCE(SUM(output_tokens), 0), COALESCE(SUM(cost_microusd), 0)
		FROM budget_ledger
	`).Scan(&totals.InputTokens, &totals.OutputTokens, &totals.CostMicroUSD)
	return totals, err
}

func (s *PostgresStore) AppendAudit(ctx context.Context, rec domain.AuditRecord) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	if err := s.appendAuditTx(ctx, tx, rec); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *PostgresStore) appendAuditTx(ctx context.Context, tx *sql.Tx, rec domain.AuditRecord) error {
	if _, err := tx.ExecContext(ctx, "LOCK TABLE audit_log IN SHARE ROW EXCLUSIVE MODE"); err != nil {
		return err
	}
	prevHash := "genesis"
	var last string
	err := tx.QueryRowContext(ctx, `SELECT row_hash FROM audit_log ORDER BY id DESC LIMIT 1`).Scan(&last)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if last != "" {
		prevHash = last
	}
	rec.DetailJSON = defaultJSON(rec.DetailJSON, "{}")
	createdStr := formatTime(rec.CreatedAt)
	rowHash := domain.ComputeAuditRowHash(prevHash, rec, createdStr)

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO audit_log (category, task_id, workload_name, action, outcome, detail_json, prev_hash, row_hash, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
	`, rec.Category, rec.TaskID, rec.WorkloadName, rec.Action, rec.Outcome, rec.DetailJSON, prevHash, rowHash, createdStr); err != nil {
		return err
	}
	return nil
}

func (s *PostgresStore) ListAudit(ctx context.Context, limit int) ([]domain.AuditRecord, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, category, task_id, workload_name, action, outcome, detail_json, prev_hash, row_hash, created_at
		FROM audit_log ORDER BY id DESC LIMIT $1
	`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []domain.AuditRecord
	for rows.Next() {
		var rec domain.AuditRecord
		var created string
		if err := rows.Scan(&rec.ID, &rec.Category, &rec.TaskID, &rec.WorkloadName, &rec.Action, &rec.Outcome, &rec.DetailJSON, &rec.PrevHash, &rec.RowHash, &created); err != nil {
			return nil, err
		}
		rec.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
		out = append(out, rec)
	}
	return out, rows.Err()
}

func (s *PostgresStore) VerifyAuditChain(ctx context.Context) (int, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, category, task_id, workload_name, action, outcome, detail_json, prev_hash, row_hash, created_at
		FROM audit_log ORDER BY id ASC
	`)
	if err != nil {
		return 0, err
	}
	defer rows.Close()

	expectedPrev := "genesis"
	verified := 0
	for rows.Next() {
		var rec domain.AuditRecord
		var created string
		if err := rows.Scan(&rec.ID, &rec.Category, &rec.TaskID, &rec.WorkloadName, &rec.Action, &rec.Outcome, &rec.DetailJSON, &rec.PrevHash, &rec.RowHash, &created); err != nil {
			return verified, err
		}
		if rec.RowHash == "" {
			continue
		}
		if rec.PrevHash != expectedPrev {
			return verified, fmt.Errorf("audit chain broken at row %d: prev_hash=%q expected=%q", rec.ID, rec.PrevHash, expectedPrev)
		}
		want := domain.ComputeAuditRowHash(rec.PrevHash, rec, created)
		if rec.RowHash != want {
			return verified, fmt.Errorf("audit row %d tampered: row_hash=%q computed=%q", rec.ID, rec.RowHash, want)
		}
		expectedPrev = rec.RowHash
		verified++
	}
	return verified, rows.Err()
}

func (s *PostgresStore) SaveCheckpoint(ctx context.Context, taskID, workloadName, refJSON string) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO checkpoints (task_id, workload_name, ref_json, created_at)
		VALUES ($1, $2, $3, $4)
	`, taskID, workloadName, defaultJSON(refJSON, "{}"), nowRFC3339())
	return err
}

func (s *PostgresStore) LatestCheckpoint(ctx context.Context, taskID string) (string, error) {
	var refJSON string
	err := s.db.QueryRowContext(ctx, `
		SELECT ref_json FROM checkpoints WHERE task_id = $1 ORDER BY id DESC LIMIT 1
	`, taskID).Scan(&refJSON)
	if errors.Is(err, sql.ErrNoRows) {
		return "", store.ErrNotFound
	}
	return refJSON, err
}

func (s *PostgresStore) UpsertProviderPrefs(ctx context.Context, prefs domain.ProviderPrefs) error {
	raw, err := json.Marshal(prefs.EnabledModels)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO provider_prefs (provider_id, default_model, enabled_models_json, updated_at)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT(provider_id) DO UPDATE SET
			default_model = EXCLUDED.default_model,
			enabled_models_json = EXCLUDED.enabled_models_json,
			updated_at = EXCLUDED.updated_at
	`, prefs.ProviderID, prefs.DefaultModel, string(raw), formatTime(prefs.UpdatedAt))
	return err
}

func (s *PostgresStore) ListProviderPrefs(ctx context.Context) (map[string]domain.ProviderPrefs, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT provider_id, default_model, enabled_models_json, updated_at FROM provider_prefs
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make(map[string]domain.ProviderPrefs)
	for rows.Next() {
		var p domain.ProviderPrefs
		var raw, updated string
		if err := rows.Scan(&p.ProviderID, &p.DefaultModel, &raw, &updated); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(raw), &p.EnabledModels)
		p.UpdatedAt, _ = time.Parse(time.RFC3339Nano, updated)
		out[p.ProviderID] = p
	}
	return out, rows.Err()
}

func nowRFC3339() string {
	return time.Now().UTC().Format(time.RFC3339Nano)
}

func formatTime(t time.Time) string {
	if t.IsZero() {
		return nowRFC3339()
	}
	return t.UTC().Format(time.RFC3339Nano)
}

func defaultJSON(s, fallback string) string {
	if strings.TrimSpace(s) == "" {
		return fallback
	}
	return s
}

func (s *PostgresStore) ClaimRecovery(ctx context.Context, taskID, previous, owner string) (bool, error) {
	return store.ClaimRecovery(ctx, s.db, taskID, previous, owner)
}
