package contextpack

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/shadewright-labs/sablecourt/internal/domain"
	"github.com/shadewright-labs/sablecourt/internal/redact"
	scruntime "github.com/shadewright-labs/sablecourt/internal/runtime"
)

const (
	VerdictPass             = "PASS"
	VerdictChangesRequested = "CHANGES_REQUESTED"
)

// BuildFailureHandoff packages failure reason, redacted artifacts, and recent events
// into a standardized Markdown block for closed-loop remediation.
func BuildFailureHandoff(task domain.Task, events []domain.TaskEvent, messages []domain.TaskMessage, reason string) string {
	var b strings.Builder
	b.WriteString("# Failure Handoff\n\n")
	b.WriteString(fmt.Sprintf("- Failed Task: `%s`\n- Workload: `%s`\n- State: `%s`\n", task.ID, task.WorkloadName, task.State))
	if strings.TrimSpace(reason) != "" {
		b.WriteString(fmt.Sprintf("- Failure Reason: %s\n", strings.TrimSpace(reason)))
	}

	if len(events) > 0 {
		start := 0
		if len(events) > 6 {
			start = len(events) - 6
		}
		b.WriteString("\n## Recent Lifecycle Events\n\n")
		for _, ev := range events[start:] {
			b.WriteString(fmt.Sprintf("- %s -> %s (%s) %s\n", ev.FromState, ev.ToState, ev.Reason, ev.FactJSON))
		}
	}

	if task.ArtifactsJSON != "" && task.ArtifactsJSON != "[]" {
		var arts []scruntime.Artifact
		if err := json.Unmarshal([]byte(task.ArtifactsJSON), &arts); err == nil && len(arts) > 0 {
			b.WriteString("\n## Captured Artifacts\n\n")
			for _, a := range arts {
				txt := strings.TrimSpace(a.Text)
				if txt == "" {
					continue
				}
				if len(txt) > 4096 {
					txt = "...[truncated]\n" + txt[len(txt)-4096:]
				}
				b.WriteString(fmt.Sprintf("### %s\n```\n%s\n```\n\n", a.Name, txt))
			}
		}
	}

	if len(messages) > 0 {
		start := 0
		if len(messages) > 5 {
			start = len(messages) - 5
		}
		b.WriteString("\n## Recent Messages\n\n")
		for _, m := range messages[start:] {
			body := strings.TrimSpace(m.Content)
			if len(body) > 1024 {
				body = body[:1024] + "...[truncated]"
			}
			b.WriteString(fmt.Sprintf("- [%s] %s\n", m.Role, body))
		}
	}

	b.WriteString("\n## Remediation Instruction\n\n")
	b.WriteString("Inspect the failure evidence above, fix the root cause, and verify with tests before completing.\n")
	return redact.String(b.String())
}

// BuildVerifierPrompt wraps a workflow verifier step with an independent adversarial rubric.
func BuildVerifierPrompt(authorTask domain.Task, stepPrompt string) string {
	var b strings.Builder
	b.WriteString("You are an independent Verifier / Critic. Do not assume the previous agent's work is correct.\n")
	b.WriteString("Look for regressions, missing edge cases, security issues, and unverified claims.\n")
	b.WriteString("Every issue you report must cite concrete evidence (file/line, test output, or log excerpt).\n\n")
	if strings.TrimSpace(stepPrompt) != "" {
		b.WriteString("## Verification Scope\n\n" + strings.TrimSpace(stepPrompt) + "\n\n")
	}
	b.WriteString(fmt.Sprintf("## Author Task\n\n- Task ID: `%s`\n- Workload: `%s`\n\n", authorTask.ID, authorTask.WorkloadName))
	if authorTask.ArtifactsJSON != "" && authorTask.ArtifactsJSON != "[]" {
		var arts []scruntime.Artifact
		if err := json.Unmarshal([]byte(authorTask.ArtifactsJSON), &arts); err == nil && len(arts) > 0 {
			b.WriteString("## Author Artifacts\n\n")
			for _, a := range arts {
				txt := strings.TrimSpace(a.Text)
				if txt == "" {
					continue
				}
				if len(txt) > 4096 {
					txt = txt[:4096] + "\n...[truncated]"
				}
				b.WriteString(fmt.Sprintf("### %s\n```\n%s\n```\n\n", a.Name, txt))
			}
		}
	}
	b.WriteString("## Required Verdict Format\n\n")
	b.WriteString("End your response with one exact line:\n")
	b.WriteString("VERDICT: PASS\n")
	b.WriteString("or\n")
	b.WriteString("VERDICT: CHANGES_REQUESTED\n")
	return redact.String(b.String())
}

// ParseVerdict scans task artifacts and messages for a structured verifier verdict line.
func ParseVerdict(artifactsJSON string, messages []domain.TaskMessage) (string, string) {
	var corpus strings.Builder
	if artifactsJSON != "" && artifactsJSON != "[]" {
		var arts []scruntime.Artifact
		if err := json.Unmarshal([]byte(artifactsJSON), &arts); err == nil {
			for _, a := range arts {
				corpus.WriteString(a.Text + "\n")
			}
		}
	}
	for _, m := range messages {
		if m.Role == "agent" {
			corpus.WriteString(m.Content + "\n")
		}
	}
	text := corpus.String()
	upper := strings.ToUpper(text)
	if strings.Contains(upper, "VERDICT: CHANGES_REQUESTED") {
		return VerdictChangesRequested, strings.TrimSpace(text)
	}
	if strings.Contains(upper, "VERDICT: PASS") {
		return VerdictPass, strings.TrimSpace(text)
	}
	return "", strings.TrimSpace(text)
}
