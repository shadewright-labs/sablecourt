package sqlite

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

	_ "modernc.org/sqlite"

	"github.com/shadewright-labs/sablecourt/internal/domain"
	"github.com/shadewright-labs/sablecourt/internal/store"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

type SQLiteStore struct {
	db *sql.DB
}

var _ store.Store = (*SQLiteStore)(nil)

func Open(path string) (*SQLiteStore, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open sqlite %q: %w", path, err)
	}
	db.SetMaxOpenConns(1)

	pragmas := []string{
		"PRAGMA journal_mode=WAL;",
		"PRAGMA foreign_keys=ON;",
		"PRAGMA busy_timeout=5000;",
	}
	for _, p := range pragmas {
		if _, err := db.Exec(p); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("exec %q: %w", p, err)
		}
	}

	return &SQLiteStore{db: db}, nil
}

func (s *SQLiteStore) Close() error {
	return s.db.Close()
}

func (s *SQLiteStore) Migrate(ctx context.Context) error {
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
		err := s.db.QueryRowContext(ctx, `SELECT version FROM schema_migrations WHERE version = ?`, version).Scan(&existing)
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
			return fmt.Errorf("begin migration %s: %w", version, err)
		}
		if _, err := tx.ExecContext(ctx, string(sqlBytes)); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("apply migration %s: %w", version, err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO schema_migrations (version, applied_at) VALUES (?, ?)`, version, nowRFC3339()); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("record migration %s: %w", version, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit migration %s: %w", version, err)
		}
	}
	return nil
}

func (s *SQLiteStore) UpsertWorkload(ctx context.Context, w domain.WorkloadRecord) error {
	updatedAt := formatTime(w.UpdatedAt)
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO workloads (name, zone, class, manifest_digest, agent_card_json, updated_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(name) DO UPDATE SET
			zone = excluded.zone,
			class = excluded.class,
			manifest_digest = excluded.manifest_digest,
			agent_card_json = excluded.agent_card_json,
			updated_at = excluded.updated_at
	`, w.Name, string(w.Zone), string(w.Class), w.ManifestDigest, w.AgentCardJSON, updatedAt)
	return err
}

func (s *SQLiteStore) ListWorkloads(ctx context.Context) ([]domain.WorkloadRecord, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT name, zone, class, manifest_digest, agent_card_json, updated_at
		FROM workloads
		ORDER BY name ASC
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

func (s *SQLiteStore) CreateTask(ctx context.Context, t domain.Task) error {
	now := nowRFC3339()
	created := now
	if !t.CreatedAt.IsZero() {
		created = t.CreatedAt.UTC().Format(time.RFC3339Nano)
	}
	updated := created
	if !t.UpdatedAt.IsZero() {
		updated = t.UpdatedAt.UTC().Format(time.RFC3339Nano)
	}
	inputJSON := defaultJSON(t.InputJSON, "{}")
	artifactsJSON := defaultJSON(t.ArtifactsJSON, "[]")
	budgetJSON := defaultJSON(t.BudgetCeilingJSON, "{}")

	_, err := s.db.ExecContext(ctx, `
		INSERT INTO tasks (
			id, parent_task_id, idempotency_key, workload_name, state,
			input_json, artifacts_json, budget_ceiling_json, created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, t.ID, t.ParentTaskID, t.IdempotencyKey, t.WorkloadName, string(t.State),
		inputJSON, artifactsJSON, budgetJSON, created, updated)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint failed") {
			return fmt.Errorf("%w: %v", store.ErrConflict, err)
		}
		return err
	}
	return nil
}

func (s *SQLiteStore) GetTask(ctx context.Context, id string) (domain.Task, error) {
	var t domain.Task
	var state, created, updated string
	var hb sql.NullString
	err := s.db.QueryRowContext(ctx, `
		SELECT id, parent_task_id, idempotency_key, workload_name, state,
		       input_json, artifacts_json, budget_ceiling_json, last_heartbeat_at,
		       created_at, updated_at
		FROM tasks WHERE id = ?
	`, id).Scan(
		&t.ID, &t.ParentTaskID, &t.IdempotencyKey, &t.WorkloadName, &state,
		&t.InputJSON, &t.ArtifactsJSON, &t.BudgetCeilingJSON, &hb,
		&created, &updated,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.Task{}, store.ErrNotFound
	}
	if err != nil {
		return domain.Task{}, err
	}
	t.State = domain.TaskState(state)
	t.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
	t.UpdatedAt, _ = time.Parse(time.RFC3339Nano, updated)
	if hb.Valid {
		parsed, err := time.Parse(time.RFC3339Nano, hb.String)
		if err == nil {
			t.LastHeartbeatAt = &parsed
		}
	}
	return t, nil
}

func (s *SQLiteStore) ListTasks(ctx context.Context) ([]domain.Task, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, parent_task_id, idempotency_key, workload_name, state,
		       input_json, artifacts_json, budget_ceiling_json, last_heartbeat_at,
		       created_at, updated_at
		FROM tasks
		ORDER BY created_at DESC
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []domain.Task
	for rows.Next() {
		var t domain.Task
		var state, created, updated string
		var hb sql.NullString
		if err := rows.Scan(
			&t.ID, &t.ParentTaskID, &t.IdempotencyKey, &t.WorkloadName, &state,
			&t.InputJSON, &t.ArtifactsJSON, &t.BudgetCeilingJSON, &hb,
			&created, &updated,
		); err != nil {
			return nil, err
		}
		t.State = domain.TaskState(state)
		t.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
		t.UpdatedAt, _ = time.Parse(time.RFC3339Nano, updated)
		if hb.Valid {
			parsed, err := time.Parse(time.RFC3339Nano, hb.String)
			if err == nil {
				t.LastHeartbeatAt = &parsed
			}
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (s *SQLiteStore) ListTaskEvents(ctx context.Context, taskID string) ([]domain.TaskEvent, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, task_id, from_state, to_state, reason, fact_json, created_at
		FROM task_events
		WHERE task_id = ?
		ORDER BY id ASC
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

func (s *SQLiteStore) AppendTaskMessage(ctx context.Context, msg domain.TaskMessage) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO task_messages (task_id, role, content, created_at)
		VALUES (?, ?, ?, ?)
	`, msg.TaskID, msg.Role, msg.Content, formatTime(msg.CreatedAt))
	return err
}

func (s *SQLiteStore) ListTaskMessages(ctx context.Context, taskID string) ([]domain.TaskMessage, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, task_id, role, content, created_at
		FROM task_messages
		WHERE task_id = ?
		ORDER BY id ASC
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

func (s *SQLiteStore) UpdateTaskState(ctx context.Context, taskID string, from, to domain.TaskState, reason, factJSON string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	now := nowRFC3339()
	res, err := tx.ExecContext(ctx, `
		UPDATE tasks SET state = ?, updated_at = ? WHERE id = ? AND state = ?
	`, string(to), now, taskID, string(from))
	if err != nil {
		_ = tx.Rollback()
		return err
	}
	affected, err := res.RowsAffected()
	if err != nil {
		_ = tx.Rollback()
		return err
	}
	if affected == 0 {
		_ = tx.Rollback()
		return store.ErrConflict
	}

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO task_events (task_id, from_state, to_state, reason, fact_json, created_at)
		VALUES (?, ?, ?, ?, ?, ?)
	`, taskID, string(from), string(to), reason, defaultJSON(factJSON, "{}"), now); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

func (s *SQLiteStore) UpdateTaskArtifacts(ctx context.Context, taskID, artifactsJSON string) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE tasks SET artifacts_json = ?, updated_at = ? WHERE id = ?
	`, defaultJSON(artifactsJSON, "[]"), nowRFC3339(), taskID)
	if err != nil {
		return err
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		return store.ErrNotFound
	}
	return nil
}

func (s *SQLiteStore) RecordHeartbeat(ctx context.Context, taskID string, at time.Time) error {
	ts := formatTime(at)
	res, err := s.db.ExecContext(ctx, `
		UPDATE tasks SET last_heartbeat_at = ?, updated_at = ? WHERE id = ?
	`, ts, ts, taskID)
	if err != nil {
		return err
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		return store.ErrNotFound
	}
	return nil
}

func (s *SQLiteStore) CreateApproval(ctx context.Context, a domain.Approval) error {
	now := nowRFC3339()
	created := now
	if !a.CreatedAt.IsZero() {
		created = a.CreatedAt.UTC().Format(time.RFC3339Nano)
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO approvals (
			id, task_id, workload_name, capability, payload_canonical_json,
			payload_sha256, status, decided_by, proof_json, expires_at, created_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, '', '{}', ?, ?)
	`, a.ID, a.TaskID, a.WorkloadName, a.Capability, a.PayloadCanonicalJSON,
		a.PayloadSHA256, string(domain.ApprovalPending), a.ExpiresAt.UTC().Format(time.RFC3339Nano), created)
	return err
}

func (s *SQLiteStore) GetApproval(ctx context.Context, id string) (domain.Approval, error) {
	var a domain.Approval
	var status, expires, created string
	var decided sql.NullString
	err := s.db.QueryRowContext(ctx, `
		SELECT id, task_id, workload_name, capability, payload_canonical_json,
		       payload_sha256, status, decided_by, proof_json, expires_at, created_at, decided_at
		FROM approvals WHERE id = ?
	`, id).Scan(
		&a.ID, &a.TaskID, &a.WorkloadName, &a.Capability, &a.PayloadCanonicalJSON,
		&a.PayloadSHA256, &status, &a.DecidedBy, &a.ProofJSON, &expires, &created, &decided,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.Approval{}, store.ErrNotFound
	}
	if err != nil {
		return domain.Approval{}, err
	}
	a.Status = domain.ApprovalStatus(status)
	a.ExpiresAt, _ = time.Parse(time.RFC3339Nano, expires)
	a.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
	if decided.Valid {
		parsed, err := time.Parse(time.RFC3339Nano, decided.String)
		if err == nil {
			a.DecidedAt = &parsed
		}
	}
	return a, nil
}

func (s *SQLiteStore) ListApprovals(ctx context.Context, status domain.ApprovalStatus) ([]domain.Approval, error) {
	query := `
		SELECT id, task_id, workload_name, capability, payload_canonical_json,
		       payload_sha256, status, decided_by, proof_json, expires_at, created_at, decided_at
		FROM approvals
	`
	var args []any
	if status != "" {
		query += ` WHERE status = ?`
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
			parsed, err := time.Parse(time.RFC3339Nano, decided.String)
			if err == nil {
				a.DecidedAt = &parsed
			}
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func (s *SQLiteStore) DecideApproval(ctx context.Context, id, expectedSHA256 string, status domain.ApprovalStatus, decidedBy, proofJSON string) error {
	now := nowRFC3339()
	res, err := s.db.ExecContext(ctx, `
		UPDATE approvals
		SET status = ?, decided_by = ?, proof_json = ?, decided_at = ?
		WHERE id = ? AND status = ? AND payload_sha256 = ?
	`, string(status), decidedBy, defaultJSON(proofJSON, "{}"), now, id, string(domain.ApprovalPending), expectedSHA256)
	if err != nil {
		return err
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		return store.ErrConflict
	}
	return nil
}

func (s *SQLiteStore) CreateLease(ctx context.Context, lease domain.Lease) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO leases (token_sha256, task_id, workload_name, expires_at, created_at)
		VALUES (?, ?, ?, ?, ?)
	`, lease.TokenSHA256, lease.TaskID, lease.WorkloadName,
		lease.ExpiresAt.UTC().Format(time.RFC3339Nano), formatTime(lease.CreatedAt))
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint failed") {
			return fmt.Errorf("%w: %v", store.ErrConflict, err)
		}
		return err
	}
	return nil
}

func (s *SQLiteStore) GetActiveLeaseByTokenHash(ctx context.Context, tokenSHA256 string, now time.Time) (domain.Lease, error) {
	var lease domain.Lease
	var expires, created string
	var revoked sql.NullString
	err := s.db.QueryRowContext(ctx, `
		SELECT token_sha256, task_id, workload_name, expires_at, revoked_at, created_at
		FROM leases
		WHERE token_sha256 = ?
	`, tokenSHA256).Scan(&lease.TokenSHA256, &lease.TaskID, &lease.WorkloadName, &expires, &revoked, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.Lease{}, store.ErrNotFound
	}
	if err != nil {
		return domain.Lease{}, err
	}
	lease.ExpiresAt, _ = time.Parse(time.RFC3339Nano, expires)
	lease.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
	if revoked.Valid {
		parsed, err := time.Parse(time.RFC3339Nano, revoked.String)
		if err == nil {
			lease.RevokedAt = &parsed
		}
		return domain.Lease{}, store.ErrNotFound
	}
	if !now.UTC().Before(lease.ExpiresAt) {
		return domain.Lease{}, store.ErrNotFound
	}
	return lease, nil
}

func (s *SQLiteStore) RevokeTaskLeases(ctx context.Context, taskID string) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE leases SET revoked_at = ? WHERE task_id = ? AND revoked_at IS NULL
	`, nowRFC3339(), taskID)
	return err
}

func (s *SQLiteStore) RecordBudgetUsage(ctx context.Context, entry domain.BudgetEntry) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO budget_ledger (
			task_id, workload_name, provider, model, input_tokens, output_tokens, cost_microusd, created_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?)
	`, entry.TaskID, entry.WorkloadName, entry.Provider, entry.Model,
		entry.InputTokens, entry.OutputTokens, entry.CostMicroUSD, formatTime(entry.CreatedAt))
	return err
}

func (s *SQLiteStore) TaskBudgetTotals(ctx context.Context, taskID string) (domain.BudgetTotals, error) {
	var totals domain.BudgetTotals
	err := s.db.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(input_tokens), 0), COALESCE(SUM(output_tokens), 0), COALESCE(SUM(cost_microusd), 0)
		FROM budget_ledger
		WHERE task_id = ?
	`, taskID).Scan(&totals.InputTokens, &totals.OutputTokens, &totals.CostMicroUSD)
	return totals, err
}

func (s *SQLiteStore) WorkloadDailyBudgetTotals(ctx context.Context, workloadName string, dayStartUTC time.Time) (domain.BudgetTotals, error) {
	var totals domain.BudgetTotals
	start := dayStartUTC.UTC().Format(time.RFC3339Nano)
	err := s.db.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(input_tokens), 0), COALESCE(SUM(output_tokens), 0), COALESCE(SUM(cost_microusd), 0)
		FROM budget_ledger
		WHERE workload_name = ? AND created_at >= ?
	`, workloadName, start).Scan(&totals.InputTokens, &totals.OutputTokens, &totals.CostMicroUSD)
	return totals, err
}

func (s *SQLiteStore) TotalBudgetTotals(ctx context.Context) (domain.BudgetTotals, error) {
	var totals domain.BudgetTotals
	err := s.db.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(input_tokens), 0), COALESCE(SUM(output_tokens), 0), COALESCE(SUM(cost_microusd), 0)
		FROM budget_ledger
	`).Scan(&totals.InputTokens, &totals.OutputTokens, &totals.CostMicroUSD)
	return totals, err
}

func (s *SQLiteStore) AppendAudit(ctx context.Context, rec domain.AuditRecord) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO audit_log (category, task_id, workload_name, action, outcome, detail_json, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
	`, rec.Category, rec.TaskID, rec.WorkloadName, rec.Action, rec.Outcome,
		defaultJSON(rec.DetailJSON, "{}"), formatTime(rec.CreatedAt))
	return err
}

func (s *SQLiteStore) ListAudit(ctx context.Context, limit int) ([]domain.AuditRecord, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, category, task_id, workload_name, action, outcome, detail_json, created_at
		FROM audit_log
		ORDER BY id DESC
		LIMIT ?
	`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []domain.AuditRecord
	for rows.Next() {
		var rec domain.AuditRecord
		var created string
		if err := rows.Scan(&rec.ID, &rec.Category, &rec.TaskID, &rec.WorkloadName, &rec.Action, &rec.Outcome, &rec.DetailJSON, &created); err != nil {
			return nil, err
		}
		rec.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
		out = append(out, rec)
	}
	return out, rows.Err()
}

func (s *SQLiteStore) SaveCheckpoint(ctx context.Context, taskID, workloadName, refJSON string) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO checkpoints (task_id, workload_name, ref_json, created_at)
		VALUES (?, ?, ?, ?)
	`, taskID, workloadName, defaultJSON(refJSON, "{}"), nowRFC3339())
	return err
}

func (s *SQLiteStore) LatestCheckpoint(ctx context.Context, taskID string) (string, error) {
	var refJSON string
	err := s.db.QueryRowContext(ctx, `
		SELECT ref_json FROM checkpoints
		WHERE task_id = ?
		ORDER BY id DESC
		LIMIT 1
	`, taskID).Scan(&refJSON)
	if errors.Is(err, sql.ErrNoRows) {
		return "", store.ErrNotFound
	}
	return refJSON, err
}

func (s *SQLiteStore) UpsertProviderPrefs(ctx context.Context, prefs domain.ProviderPrefs) error {
	raw, err := json.Marshal(prefs.EnabledModels)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO provider_prefs (provider_id, default_model, enabled_models_json, updated_at)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(provider_id) DO UPDATE SET
			default_model = excluded.default_model,
			enabled_models_json = excluded.enabled_models_json,
			updated_at = excluded.updated_at
	`, prefs.ProviderID, prefs.DefaultModel, string(raw), formatTime(prefs.UpdatedAt))
	return err
}

func (s *SQLiteStore) ListProviderPrefs(ctx context.Context) (map[string]domain.ProviderPrefs, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT provider_id, default_model, enabled_models_json, updated_at
		FROM provider_prefs
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
