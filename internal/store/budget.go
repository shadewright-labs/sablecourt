package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"math"
	"strconv"
	"time"

	"github.com/shadewright-labs/sablecourt/internal/domain"
)

const (
	maxChildBudgetCeilingBytes  = 4 << 10
	defaultBudgetReservationTTL = 15 * time.Minute
)

// ErrInvalidChildBudgetCeiling indicates absent or malformed delegated task budget ceiling JSON.
var ErrInvalidChildBudgetCeiling = errors.New("invalid_child_budget_ceiling")

// BudgetReservationRequest describes an atomic pre-flight budget check and reservation.
type BudgetReservationRequest struct {
	TaskID       string
	WorkloadName string
	Budget       domain.Budget
	Reserve      domain.BudgetTotals
	Now          time.Time
	TTL          time.Duration
}

// BudgetReservationResult reports whether a reservation was granted or refused due to a limit.
type BudgetReservationResult struct {
	ReservationID string
	Exceeded      bool
	Reason        string
}

// BudgetRowLocker locks a workload and task for budget writes on row-locking backends.
type BudgetRowLocker func(ctx context.Context, tx *sql.Tx, taskID, workloadName string) error

func beginBudgetTx(ctx context.Context, db *sql.DB, lockRows BudgetRowLocker) (*sql.Tx, error) {
	if lockRows == nil {
		return db.BeginTx(ctx, nil)
	}
	// Read the totals from a fresh snapshot after acquiring the row locks.
	return db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
}

// ExceedsBudgetLimit reports whether used + reserve would reach or exceed maximum without int64 overflow.
func ExceedsBudgetLimit(used, reserve, maximum int64) bool {
	return used >= maximum || reserve > maximum-used
}

// CheckChildBudgetCeiling validates a child task's ceiling JSON and checks used + reserve against each limit.
func CheckChildBudgetCeiling(raw string, used, reserve domain.BudgetTotals) (string, error) {
	if len(raw) > maxChildBudgetCeilingBytes {
		return "", ErrInvalidChildBudgetCeiling
	}
	var ceiling struct {
		Input  jsontext.Value `json:"maxInputTokens"`
		Output jsontext.Value `json:"maxOutputTokens"`
		Cost   jsontext.Value `json:"maxCostUSD"`
	}
	if err := json.Unmarshal([]byte(raw), &ceiling, json.RejectUnknownMembers(true)); err != nil {
		return "", ErrInvalidChildBudgetCeiling
	}
	if ceiling.Input == nil && ceiling.Output == nil && ceiling.Cost == nil {
		return "", ErrInvalidChildBudgetCeiling
	}

	var reason string
	for _, limit := range []struct {
		value   jsontext.Value
		used    int64
		reserve int64
		reason  string
		usd     bool
	}{
		{value: ceiling.Input, used: used.TotalInputTokens(), reserve: reserve.TotalInputTokens(), reason: "child_input_tokens"},
		{value: ceiling.Output, used: used.OutputTokens, reserve: reserve.OutputTokens, reason: "child_output_tokens"},
		{value: ceiling.Cost, used: used.CostMicroUSD, reserve: reserve.CostMicroUSD, reason: "child_cost_usd", usd: true},
	} {
		if limit.value == nil {
			continue
		}
		maximum, err := parseChildBudgetLimit(limit.value, limit.usd)
		if err != nil {
			return "", err
		}
		if reason == "" && ExceedsBudgetLimit(limit.used, limit.reserve, maximum) {
			reason = limit.reason
		}
	}
	return reason, nil
}

func parseChildBudgetLimit(value jsontext.Value, usd bool) (int64, error) {
	if value.Kind() != '0' {
		return 0, ErrInvalidChildBudgetCeiling
	}
	if usd {
		cost, err := strconv.ParseFloat(string(value), 64)
		microUSD := cost * 1_000_000
		if err != nil || microUSD < 0 || microUSD >= math.MaxInt64 {
			return 0, ErrInvalidChildBudgetCeiling
		}
		return int64(microUSD), nil
	}
	tokens, err := strconv.ParseInt(string(value), 10, 64)
	if err != nil || tokens < 0 {
		return 0, ErrInvalidChildBudgetCeiling
	}
	return tokens, nil
}

// ReserveBudgetTx atomically checks workload and child task budgets (including active reservations)
// and records a pending reservation row if within all ceilings.
func ReserveBudgetTx(ctx context.Context, db *sql.DB, lockRows BudgetRowLocker, req BudgetReservationRequest) (BudgetReservationResult, error) {
	if req.Reserve.InputTokens < 0 || req.Reserve.CacheWriteTokens < 0 || req.Reserve.CacheReadTokens < 0 || req.Reserve.OutputTokens < 0 || req.Reserve.CostMicroUSD < 0 {
		return BudgetReservationResult{}, errors.New("budget reservation amounts must be non-negative")
	}
	now := req.Now.UTC()
	if now.IsZero() {
		now = time.Now().UTC()
	}
	ttl := req.TTL
	if ttl <= 0 {
		ttl = defaultBudgetReservationTTL
	}
	nowStr := now.Format(time.RFC3339Nano)
	expiresStr := now.Add(ttl).Format(time.RFC3339Nano)
	dayStartStr := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC).Format(time.RFC3339Nano)

	tx, err := beginBudgetTx(ctx, db, lockRows)
	if err != nil {
		return BudgetReservationResult{}, err
	}
	defer tx.Rollback()

	if lockRows != nil {
		if err := lockRows(ctx, tx, req.TaskID, req.WorkloadName); err != nil {
			return BudgetReservationResult{}, err
		}
	}

	// SQLite acquires the database writer lock on this statement, before any reads.
	if _, err := tx.ExecContext(ctx, `
		DELETE FROM budget_reservations
		WHERE workload_name = $1 AND rtrim(expires_at, 'Z') <= rtrim($2, 'Z')
	`, req.WorkloadName, nowStr); err != nil {
		return BudgetReservationResult{}, err
	}

	var parentTaskID, budgetCeilingJSON string
	if err := tx.QueryRowContext(ctx, `
		SELECT parent_task_id, budget_ceiling_json FROM tasks WHERE id = $1 AND workload_name = $2
	`, req.TaskID, req.WorkloadName).Scan(&parentTaskID, &budgetCeilingJSON); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return BudgetReservationResult{}, ErrNotFound
		}
		return BudgetReservationResult{}, err
	}

	var taskTotals domain.BudgetTotals
	if err := tx.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(input_tokens), 0), COALESCE(SUM(cache_write_tokens), 0), COALESCE(SUM(cache_read_tokens), 0), COALESCE(SUM(output_tokens), 0), COALESCE(SUM(cost_microusd), 0)
		FROM (
			SELECT input_tokens, cache_write_tokens, cache_read_tokens, output_tokens, cost_microusd
			FROM budget_ledger
			WHERE task_id = $1
			UNION ALL
			SELECT input_tokens, 0 AS cache_write_tokens, 0 AS cache_read_tokens, output_tokens, cost_microusd
			FROM budget_reservations
			WHERE task_id = $1 AND rtrim(expires_at, 'Z') > rtrim($2, 'Z')
		) t
	`, req.TaskID, nowStr).Scan(&taskTotals.InputTokens, &taskTotals.CacheWriteTokens, &taskTotals.CacheReadTokens, &taskTotals.OutputTokens, &taskTotals.CostMicroUSD); err != nil {
		return BudgetReservationResult{}, err
	}

	b := req.Budget
	if b.Spec.PerTask.MaxInputTokens > 0 && ExceedsBudgetLimit(taskTotals.TotalInputTokens(), req.Reserve.TotalInputTokens(), b.Spec.PerTask.MaxInputTokens) {
		return BudgetReservationResult{Exceeded: true, Reason: "per_task_input_tokens"}, nil
	}
	if b.Spec.PerTask.MaxOutputTokens > 0 && ExceedsBudgetLimit(taskTotals.OutputTokens, req.Reserve.OutputTokens, b.Spec.PerTask.MaxOutputTokens) {
		return BudgetReservationResult{Exceeded: true, Reason: "per_task_output_tokens"}, nil
	}
	if b.Spec.PerTask.MaxCostUSD > 0 && ExceedsBudgetLimit(taskTotals.CostMicroUSD, req.Reserve.CostMicroUSD, int64(b.Spec.PerTask.MaxCostUSD*1_000_000)) {
		return BudgetReservationResult{Exceeded: true, Reason: "per_task_cost_usd"}, nil
	}

	if b.Spec.Daily.MaxCostUSD > 0 {
		var dailyCost int64
		if err := tx.QueryRowContext(ctx, `
			-- A live reservation settles into the day it completes, so it counts
			-- toward today even when it was admitted before midnight.
			SELECT COALESCE(SUM(cost_microusd), 0)
			FROM (
				SELECT cost_microusd
				FROM budget_ledger
				WHERE workload_name = $1 AND rtrim(created_at, 'Z') >= rtrim($2, 'Z')
				UNION ALL
				SELECT cost_microusd
				FROM budget_reservations
				WHERE workload_name = $1
				  AND rtrim(expires_at, 'Z') > rtrim($3, 'Z')
			) d
		`, req.WorkloadName, dayStartStr, nowStr).Scan(&dailyCost); err != nil {
			return BudgetReservationResult{}, err
		}
		if ExceedsBudgetLimit(dailyCost, req.Reserve.CostMicroUSD, int64(b.Spec.Daily.MaxCostUSD*1_000_000)) {
			return BudgetReservationResult{Exceeded: true, Reason: "daily_cost_usd"}, nil
		}
	}

	if parentTaskID != "" {
		reason, err := CheckChildBudgetCeiling(budgetCeilingJSON, taskTotals, req.Reserve)
		if err != nil {
			return BudgetReservationResult{}, err
		}
		if reason != "" {
			return BudgetReservationResult{Exceeded: true, Reason: reason}, nil
		}
	}

	resID := "bres-" + rand.Text()
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO budget_reservations (
			id, task_id, workload_name, input_tokens, output_tokens, cost_microusd, expires_at, created_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
	`, resID, req.TaskID, req.WorkloadName, req.Reserve.TotalInputTokens(), req.Reserve.OutputTokens, req.Reserve.CostMicroUSD, expiresStr, nowStr); err != nil {
		return BudgetReservationResult{}, err
	}

	if err := tx.Commit(); err != nil {
		return BudgetReservationResult{}, err
	}
	return BudgetReservationResult{ReservationID: resID}, nil
}

// SettleBudgetReservationTx atomically removes a reservation and, when actual is non-nil,
// records settled usage into budget_ledger in the same transaction.
func SettleBudgetReservationTx(ctx context.Context, db *sql.DB, lockRows BudgetRowLocker, reservationID string, actual *domain.BudgetEntry) error {
	tx, err := beginBudgetTx(ctx, db, lockRows)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if lockRows != nil {
		var taskID, workloadName string
		err := tx.QueryRowContext(ctx, `
			SELECT task_id, workload_name FROM budget_reservations WHERE id = $1
		`, reservationID).Scan(&taskID, &workloadName)
		if errors.Is(err, sql.ErrNoRows) {
			if actual == nil {
				return nil
			}
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if err := lockRows(ctx, tx, taskID, workloadName); err != nil {
			return err
		}
	}

	var taskID, workloadName string
	err = tx.QueryRowContext(ctx, `
		DELETE FROM budget_reservations WHERE id = $1 RETURNING task_id, workload_name
	`, reservationID).Scan(&taskID, &workloadName)
	if errors.Is(err, sql.ErrNoRows) {
		if actual == nil {
			return nil
		}
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if actual == nil {
		return tx.Commit()
	}
	if actual.TaskID != taskID || actual.WorkloadName != workloadName {
		return ErrConflict
	}

	in := max(actual.InputTokens, 0)
	cacheWrite := max(actual.CacheWriteTokens, 0)
	cacheRead := max(actual.CacheReadTokens, 0)
	out := max(actual.OutputTokens, 0)
	cost := max(actual.CostMicroUSD, 0)
	created := actual.CreatedAt
	if created.IsZero() {
		created = time.Now().UTC()
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO budget_ledger (
			task_id, workload_name, provider, model, input_tokens, cache_write_tokens, cache_read_tokens, output_tokens, cost_microusd, created_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
	`, taskID, workloadName, actual.Provider, actual.Model,
		in, cacheWrite, cacheRead, out, cost, created.UTC().Format(time.RFC3339Nano)); err != nil {
		return err
	}
	return tx.Commit()
}
