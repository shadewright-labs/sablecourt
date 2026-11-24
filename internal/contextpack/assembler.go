package contextpack

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/shadewright-labs/sablecourt/internal/domain"
	"github.com/shadewright-labs/sablecourt/internal/redact"
	"github.com/shadewright-labs/sablecourt/internal/store"
)

const (
	precheckTimeout  = 10 * time.Second
	maxPrecheckBytes = 16 * 1024
	maxPacketBytes   = 32 * 1024
)

type Packet struct {
	Markdown      string
	LearningCount int
	RelicCount    int
	PrecheckRan   bool
	ChangedPaths  []string
}

type taskInputMeta struct {
	Prompt         string `json:"prompt"`
	Input          string `json:"input"`
	Workflow       string `json:"workflow"`
	Step           string `json:"step"`
	Role           string `json:"role"`
	ConclaveID     string `json:"conclaveId"`
	FailureHandoff string `json:"failure_handoff"`
	RepoPath       string `json:"repo_path"`
}

// Assemble builds a bounded, redacted context-packet.md for a task before dispatch.
func Assemble(ctx context.Context, st store.Store, task domain.Task, wl domain.Workload) Packet {
	var meta taskInputMeta
	_ = json.Unmarshal([]byte(task.InputJSON), &meta)

	repoPath := strings.TrimSpace(meta.RepoPath)
	if repoPath == "" && wl.Spec.Coding != nil {
		repoPath = strings.TrimSpace(wl.Spec.Coding.RepoPath)
	}
	repoName := ""
	if repoPath != "" {
		repoName = filepath.Base(filepath.Clean(repoPath))
	}

	var changedPaths []string
	var precheckMD string
	precheckRan := false
	if repoPath != "" {
		if info, err := os.Stat(repoPath); err == nil && info.IsDir() {
			precheckMD, changedPaths = runReadOnlyPrecheck(ctx, repoPath)
			precheckRan = precheckMD != ""
		}
	}

	learnings := MatchLearnings(ctx, st, repoName, changedPaths, 20)

	var relics []domain.ConclaveRelic
	var decrees []domain.ConclaveMessage
	if st != nil && meta.ConclaveID != "" {
		if rs, err := st.ListConclaveRelics(ctx, meta.ConclaveID); err == nil {
			if len(rs) > 4 {
				rs = rs[len(rs)-4:]
			}
			relics = rs
		}
		if msgs, err := st.ListConclaveMessages(ctx, meta.ConclaveID, 0); err == nil {
			for _, m := range msgs {
				if m.Kind == "decree" || m.Kind == "decision" {
					decrees = append(decrees, m)
				}
			}
			if len(decrees) > 5 {
				decrees = decrees[len(decrees)-5:]
			}
		}
	}

	var b strings.Builder
	b.WriteString("# SableCourt Context Packet\n\n")
	b.WriteString(fmt.Sprintf("- Task: `%s`\n- Workload: `%s`\n", task.ID, task.WorkloadName))
	if meta.Workflow != "" {
		b.WriteString(fmt.Sprintf("- Workflow: `%s` (step `%s`)\n", meta.Workflow, meta.Step))
	}
	if meta.Role != "" {
		b.WriteString(fmt.Sprintf("- Role: `%s`\n", meta.Role))
	}
	if meta.ConclaveID != "" {
		b.WriteString(fmt.Sprintf("- Conclave: `%s`\n", meta.ConclaveID))
	}
	b.WriteString("\n## Intent\n\n")
	if strings.TrimSpace(meta.Prompt) != "" {
		b.WriteString(strings.TrimSpace(meta.Prompt) + "\n")
	}
	if strings.TrimSpace(meta.Input) != "" && strings.TrimSpace(meta.Input) != strings.TrimSpace(meta.Prompt) {
		b.WriteString("\n" + strings.TrimSpace(meta.Input) + "\n")
	}
	if strings.TrimSpace(meta.Prompt) == "" && strings.TrimSpace(meta.Input) == "" {
		b.WriteString(strings.TrimSpace(task.InputJSON) + "\n")
	}

	if len(learnings) > 0 {
		b.WriteString("\n## Scoped Learnings\n\n")
		for _, l := range learnings {
			b.WriteString(fmt.Sprintf("- [%s / %s] %s\n", l.Namespace, l.Key, l.Value))
		}
	}

	if len(decrees) > 0 {
		b.WriteString("\n## Recent Conclave Decrees\n\n")
		for _, d := range decrees {
			b.WriteString(fmt.Sprintf("- (%s) %s\n", d.FromRole, d.Body))
		}
	}

	if len(relics) > 0 {
		b.WriteString("\n## Conclave Relics\n\n")
		for _, r := range relics {
			body := r.Body
			if len(body) > 2048 {
				body = body[:2048] + "\n...[truncated]"
			}
			b.WriteString(fmt.Sprintf("### %s (by %s)\n\n```\n%s\n```\n\n", r.Name, r.AuthorRole, body))
		}
	}

	if precheckMD != "" {
		b.WriteString("\n## Repository Pre-Check (read-only)\n\n")
		b.WriteString(precheckMD)
	}

	if strings.TrimSpace(meta.FailureHandoff) != "" {
		b.WriteString("\n## Failure Handoff\n\n")
		b.WriteString(strings.TrimSpace(meta.FailureHandoff) + "\n")
	}

	md := redact.String(b.String())
	if len(md) > maxPacketBytes {
		md = md[:maxPacketBytes] + "\n\n...[context packet truncated]"
	}
	return Packet{
		Markdown:      md,
		LearningCount: len(learnings),
		RelicCount:    len(relics),
		PrecheckRan:   precheckRan,
		ChangedPaths:  changedPaths,
	}
}

func runReadOnlyPrecheck(ctx context.Context, repoPath string) (string, []string) {
	tctx, cancel := context.WithTimeout(ctx, precheckTimeout)
	defer cancel()

	var b strings.Builder
	var changed []string

	if _, err := exec.LookPath("git"); err == nil {
		if out, err := exec.CommandContext(tctx, "git", "-C", repoPath, "status", "--short").Output(); err == nil {
			txt := strings.TrimSpace(string(out))
			if txt != "" {
				b.WriteString("### git status --short\n```\n" + txt + "\n```\n\n")
				for _, line := range strings.Split(txt, "\n") {
					if len(line) > 3 {
						p := strings.TrimSpace(line[3:])
						if idx := strings.Index(p, " -> "); idx >= 0 {
							p = strings.TrimSpace(p[idx+4:])
						}
						if p != "" {
							changed = append(changed, p)
						}
					}
				}
			}
		}
		if out, err := exec.CommandContext(tctx, "git", "-C", repoPath, "diff", "--stat").Output(); err == nil {
			txt := strings.TrimSpace(string(out))
			if txt != "" {
				b.WriteString("### git diff --stat\n```\n" + txt + "\n```\n\n")
			}
		}
	}

	if _, err := os.Stat(filepath.Join(repoPath, "go.mod")); err == nil {
		if _, err := exec.LookPath("go"); err == nil {
			cmd := exec.CommandContext(tctx, "go", "vet", "./...")
			cmd.Dir = repoPath
			out, err := cmd.CombinedOutput()
			txt := strings.TrimSpace(string(out))
			if err != nil && txt != "" {
				b.WriteString("### go vet ./... (issues found)\n```\n" + txt + "\n```\n\n")
			}
		}
	}

	if _, err := os.Stat(filepath.Join(repoPath, "sgconfig.yml")); err == nil {
		if _, err := exec.LookPath("sg"); err == nil {
			cmd := exec.CommandContext(tctx, "sg", "scan")
			cmd.Dir = repoPath
			out, err := cmd.CombinedOutput()
			txt := strings.TrimSpace(string(out))
			if err != nil && txt != "" {
				b.WriteString("### ast-grep scan (issues found)\n```\n" + txt + "\n```\n\n")
			}
		}
	}

	res := redact.String(b.String())
	if len(res) > maxPrecheckBytes {
		res = res[:maxPrecheckBytes] + "\n...[pre-check truncated]\n"
	}
	return res, changed
}
