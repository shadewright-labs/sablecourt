package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"strings"

	"github.com/shadewright-labs/sablecourt/internal/contextpack"
	"github.com/shadewright-labs/sablecourt/internal/domain"
	"github.com/shadewright-labs/sablecourt/internal/redact"
	scruntime "github.com/shadewright-labs/sablecourt/internal/runtime"
	"github.com/shadewright-labs/sablecourt/internal/store"
)

const maxFinalMessage = 64 * 1024

var errAuthorCheckoutUnavailable = errors.New("runtime cannot guarantee the author's checkout for this step")

func (r *Reconciler) prepareLaunchInput(ctx context.Context, task domain.Task, wl domain.Workload) (string, error) {
	var meta struct {
		Workflow       string `json:"workflow"`
		Role           string `json:"role"`
		StepIndex      int    `json:"step_index"`
		RetryCount     int    `json:"retry_count"`
		FailureHandoff string `json:"failure_handoff"`
	}
	_ = json.Unmarshal([]byte(task.InputJSON), &meta)
	if meta.Workflow != "" {
		r.mu.Lock()
		bundle := r.bundle
		r.mu.Unlock()
		role := meta.Role
		if bundle != nil {
			steps := bundle.Workflows[meta.Workflow].Spec.Steps
			if meta.StepIndex >= 0 && meta.StepIndex < len(steps) && steps[meta.StepIndex].Workload == task.WorkloadName && steps[meta.StepIndex].Role != "" {
				if steps[meta.StepIndex].Role == "verifier" || role == "" {
					role = steps[meta.StepIndex].Role
				}
			}
		}
		remediation := strings.HasPrefix(task.IdempotencyKey, "wf:"+meta.Workflow+":remediate:") ||
			(role == "author" && meta.RetryCount > 0 && meta.FailureHandoff != "")
		if role == "verifier" || remediation {
			return "", errAuthorCheckoutUnavailable
		}
	}
	if wl.Spec.Class == domain.ClassDeterministicController {
		return task.InputJSON, nil
	}

	var payload map[string]json.RawMessage
	if err := json.Unmarshal([]byte(task.InputJSON), &payload); err != nil || payload == nil {
		prompt, _ := json.Marshal(task.InputJSON)
		payload = map[string]json.RawMessage{"prompt": prompt}
	}
	for key := range payload {
		if strings.EqualFold(key, "repo_path") {
			precheckInput := maps.Clone(payload)
			maps.DeleteFunc(precheckInput, func(key string, _ json.RawMessage) bool {
				return strings.EqualFold(key, "repo_path")
			})
			sanitized, err := json.Marshal(precheckInput)
			if err != nil {
				return "", err
			}
			task.InputJSON = string(sanitized)
			break
		}
	}

	if raw, ok := payload["tier"]; ok {
		var tier, provider, model string
		if err := json.Unmarshal(raw, &tier); err != nil || tier == "" {
			return "", errors.New("invalid model tier")
		}
		if raw, ok := payload["provider"]; ok {
			if err := json.Unmarshal(raw, &provider); err != nil || provider == "" {
				return "", errors.New("invalid tier provider")
			}
		}
		if raw, ok := payload["model"]; ok {
			if err := json.Unmarshal(raw, &model); err != nil || model == "" {
				return "", errors.New("invalid tier model")
			}
		}
		provider, model, err := resolveTierRoute(ctx, r.store, wl, tier, provider, model)
		if err != nil {
			return "", err
		}
		payload["provider"], _ = json.Marshal(provider)
		payload["model"], _ = json.Marshal(model)
	}

	packet := contextpack.Assemble(ctx, r.store, task, wl)
	payload["context_packet"], _ = json.Marshal(packet.Markdown)
	raw, err := json.Marshal(payload)
	return string(raw), err
}

func resolveTierRoute(ctx context.Context, st store.Store, wl domain.Workload, tier, provider, model string) (string, string, error) {
	tier = strings.ToLower(strings.TrimSpace(tier))
	switch tier {
	case "fast", "coding", "critic":
	default:
		return "", "", errors.New("unknown model tier")
	}
	if provider == "" && model == "" {
		entry, err := st.GetMemory(ctx, contextpack.ModelTiersNamespace, tier)
		switch {
		case errors.Is(err, store.ErrNotFound):
			for _, route := range contextpack.DefaultModelTiers {
				if route.Tier == tier {
					provider, model = route.Provider, route.Model
					break
				}
			}
		case err != nil:
			return "", "", fmt.Errorf("load model tier: %w", err)
		default:
			var route contextpack.TierRoute
			if len(entry.Value) > 1024 || json.Unmarshal([]byte(entry.Value), &route) != nil || route.Tier != tier {
				return "", "", errors.New("invalid stored model tier")
			}
			provider, model = route.Provider, route.Model
		}
	}
	if !validRouteID(provider, 64) || !validRouteID(model, 128) {
		return "", "", errors.New("invalid tier provider or model")
	}
	switch wl.Spec.Class {
	case domain.ClassDeterministicController:
		return "", "", errors.New("deterministic workload cannot use a model tier")
	case domain.ClassCodingAgent:
		if wl.Spec.Coding == nil || provider != "cli:"+wl.Spec.Coding.Harness || !harnessAcceptsModel(wl.Spec.Coding.Harness, model) {
			return "", "", errors.New("tier model is incompatible with the workload harness")
		}
	}
	return provider, model, nil
}

func validRouteID(value string, maxLen int) bool {
	if len(value) == 0 || len(value) > maxLen || value[0] == '-' {
		return false
	}
	for _, c := range value {
		if ('a' <= c && c <= 'z') || ('A' <= c && c <= 'Z') || ('0' <= c && c <= '9') || strings.ContainsRune("-._:/", c) {
			continue
		}
		return false
	}
	return true
}

func harnessAcceptsModel(harness, model string) bool {
	switch harness {
	case "claude":
		return strings.HasPrefix(model, "claude-") || model == "opus" || model == "sonnet" || model == "haiku"
	case "codex":
		return strings.HasPrefix(model, "gpt-") || strings.HasPrefix(model, "codex-")
	case "agy":
		return strings.HasPrefix(model, "gemini-") || strings.HasPrefix(model, "claude-")
	default:
		return false
	}
}

// The adapter must identify the final message separately from logs and intermediate messages.
func finalVerifierVerdict(artifactsJSON, prompt string) (verdict, findings string) {
	if len(artifactsJSON) > 8*maxFinalMessage {
		return "", ""
	}
	var artifacts []scruntime.Artifact
	if err := json.Unmarshal([]byte(artifactsJSON), &artifacts); err != nil {
		return "", ""
	}
	var final *scruntime.Artifact
	for i := range artifacts {
		if artifacts[i].Name == "final-message" {
			if final != nil {
				return "", ""
			}
			final = &artifacts[i]
		}
	}
	if final == nil || final.ContentType != "text/plain" || len(final.Text) > maxFinalMessage {
		return "", ""
	}
	text := strings.TrimRight(final.Text, "\r\n")
	if strings.TrimSpace(text) == strings.TrimSpace(prompt) {
		return "", ""
	}
	line := text[strings.LastIndexByte(text, '\n')+1:]
	switch line {
	case "VERDICT: PASS":
		return contextpack.VerdictPass, ""
	case "VERDICT: CHANGES_REQUESTED":
		return contextpack.VerdictChangesRequested, redact.String(text)
	default:
		return "", ""
	}
}
