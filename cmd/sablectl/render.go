package main

import (
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/lipgloss/table"

	"github.com/shadewright-labs/sablecourt/internal/domain"
	"github.com/shadewright-labs/sablecourt/internal/providers"
)

var (
	colJade   = lipgloss.Color("#2DD4BF")
	colIndigo = lipgloss.Color("#6D28D9")
	colBone   = lipgloss.Color("#EFE9DC")
	colMuted  = lipgloss.Color("#9CA3AF")
	colAmber  = lipgloss.Color("#F0B86A")
	colRed    = lipgloss.Color("#F87171")
	colBorder = lipgloss.Color("#312B4A")

	stTitle  = lipgloss.NewStyle().Bold(true).Foreground(colBone)
	stJade   = lipgloss.NewStyle().Foreground(colJade)
	stAmber  = lipgloss.NewStyle().Foreground(colAmber)
	stRed    = lipgloss.NewStyle().Foreground(colRed)
	stMuted  = lipgloss.NewStyle().Foreground(colMuted)
	stHeader = lipgloss.NewStyle().Bold(true).Foreground(colJade).Padding(0, 1)
	stCell   = lipgloss.NewStyle().Foreground(colBone).Padding(0, 1)
	stBox    = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(colIndigo).
			Padding(0, 2)
)

func newStyledTable(headers ...string) *table.Table {
	return table.New().
		Border(lipgloss.RoundedBorder()).
		BorderStyle(lipgloss.NewStyle().Foreground(colBorder)).
		Headers(headers...).
		StyleFunc(func(row, col int) lipgloss.Style {
			if row == table.HeaderRow {
				return stHeader
			}
			return stCell
		})
}

func renderWorkloads(w io.Writer, items []domain.WorkloadRecord) {
	if len(items) == 0 {
		fmt.Fprintln(w, stMuted.Render("No workloads registered."))
		return
	}
	t := newStyledTable("WORKLOAD", "ZONE", "CLASS", "DIGEST")
	for _, item := range items {
		zone := stJade.Render(string(item.Zone))
		if item.Zone == domain.ZoneSandbox {
			zone = stAmber.Render(string(item.Zone))
		}
		digest := item.ManifestDigest
		if len(digest) > 12 {
			digest = digest[:12]
		}
		t.Row(item.Name, zone, stMuted.Render(string(item.Class)), stMuted.Render(digest))
	}
	fmt.Fprintln(w, t.Render())
}

func renderProviders(w io.Writer, items []providers.ProviderStatus) {
	if len(items) == 0 {
		fmt.Fprintln(w, stMuted.Render("No providers configured."))
		return
	}
	t := newStyledTable("PROVIDER", "ID", "KIND", "STATUS", "MODELS", "DETAIL")
	for _, p := range items {
		status := stMuted.Render("○ not connected")
		if p.Connected {
			status = stJade.Render("● connected")
		}
		models := "-"
		if len(p.Models) > 0 {
			models = fmt.Sprintf("%d", len(p.Models))
		}
		t.Row(p.DisplayName, stMuted.Render(p.ID), stMuted.Render(p.Kind), status, models, stMuted.Render(truncate(p.Detail, 42)))
	}
	fmt.Fprintln(w, t.Render())
}

func renderTasks(w io.Writer, items []domain.Task) {
	if len(items) == 0 {
		fmt.Fprintln(w, stMuted.Render("No tasks submitted."))
		return
	}
	t := newStyledTable("TASK ID", "WORKLOAD", "STATE", "PARENT", "HEARTBEAT")
	for _, item := range items {
		parent := "-"
		if item.ParentTaskID != "" {
			parent = item.ParentTaskID
		}
		hb := "none"
		if item.LastHeartbeatAt != nil {
			hb = item.LastHeartbeatAt.Format("15:04:05Z")
		}
		t.Row(item.ID, item.WorkloadName, styleTaskState(item.State), stMuted.Render(parent), stMuted.Render(hb))
	}
	fmt.Fprintln(w, t.Render())
}

func renderTaskDetail(w io.Writer, task domain.Task, events []domain.TaskEvent, budget domain.BudgetTotals) {
	costUSD := float64(budget.CostMicroUSD) / 1_000_000.0
	lines := []string{
		fmt.Sprintf("%s  %s", stTitle.Render("Task"), stJade.Render(task.ID)),
		fmt.Sprintf("%s  %s   %s  %s", stMuted.Render("Workload:"), task.WorkloadName, stMuted.Render("State:"), styleTaskState(task.State)),
		fmt.Sprintf("%s  $%.4f (%d in / %d out)", stMuted.Render("Spend:   "), costUSD, budget.TotalInputTokens(), budget.OutputTokens),
		fmt.Sprintf("%s  %s", stMuted.Render("Input:   "), truncate(task.InputJSON, 80)),
	}
	fmt.Fprintln(w, stBox.Render(strings.Join(lines, "\n")))

	if len(events) > 0 {
		t := newStyledTable("TIME", "TRANSITION", "REASON")
		for _, ev := range events {
			t.Row(stMuted.Render(ev.CreatedAt.Format("15:04:05Z")), fmt.Sprintf("%s -> %s", ev.FromState, ev.ToState), ev.Reason)
		}
		fmt.Fprintln(w, t.Render())
	}
}

func renderApprovals(w io.Writer, items []domain.Approval) {
	if len(items) == 0 {
		fmt.Fprintln(w, stMuted.Render("No pending approvals."))
		return
	}
	t := newStyledTable("APPROVAL ID", "WORKLOAD", "CAPABILITY", "SHA-256", "EXPIRES")
	for _, a := range items {
		t.Row(a.ID, a.WorkloadName, stAmber.Render(a.Capability), stMuted.Render(truncate(a.PayloadSHA256, 16)), stMuted.Render(a.ExpiresAt.Format("15:04:05Z")))
	}
	fmt.Fprintln(w, t.Render())
}

func renderBudget(w io.Writer, totals domain.BudgetTotals) {
	costUSD := float64(totals.CostMicroUSD) / 1_000_000.0
	content := strings.Join([]string{
		stTitle.Render("Sablecourt Observed Model Budget"),
		fmt.Sprintf("%s  %d", stMuted.Render("Input tokens:      "), totals.InputTokens),
		fmt.Sprintf("%s  %d", stMuted.Render("Cache write tokens:"), totals.CacheWriteTokens),
		fmt.Sprintf("%s  %d", stMuted.Render("Cache read tokens: "), totals.CacheReadTokens),
		fmt.Sprintf("%s  %d", stMuted.Render("Output tokens:     "), totals.OutputTokens),
		fmt.Sprintf("%s  %s", stMuted.Render("Total spend:       "), stJade.Render(fmt.Sprintf("$%.4f", costUSD))),
	}, "\n")
	fmt.Fprintln(w, stBox.Render(content))
}

func renderAudit(w io.Writer, items []domain.AuditRecord) {
	if len(items) == 0 {
		fmt.Fprintln(w, stMuted.Render("No audit events recorded."))
		return
	}
	t := newStyledTable("TIME", "CATEGORY", "WORKLOAD", "ACTION", "OUTCOME")
	for _, rec := range items {
		outcome := stJade.Render(rec.Outcome)
		if rec.Outcome == "denied" || rec.Outcome == "error" {
			outcome = stRed.Render(rec.Outcome)
		} else if rec.Outcome == "pending" {
			outcome = stAmber.Render(rec.Outcome)
		}
		wl := rec.WorkloadName
		if wl == "" {
			wl = "-"
		}
		t.Row(stMuted.Render(rec.CreatedAt.Format(time.TimeOnly+"Z")), stMuted.Render(rec.Category), wl, rec.Action, outcome)
	}
	fmt.Fprintln(w, t.Render())
}

func styleTaskState(s domain.TaskState) string {
	switch s {
	case domain.TaskStateWorking, domain.TaskStateCompleted:
		return stJade.Render(string(s))
	case domain.TaskStateInputRequired, domain.TaskStateSubmitted:
		return stAmber.Render(string(s))
	case domain.TaskStateFailed:
		return stRed.Render(string(s))
	default:
		return stMuted.Render(string(s))
	}
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max-3] + "..."
}
