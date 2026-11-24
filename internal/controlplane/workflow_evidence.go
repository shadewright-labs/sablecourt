package controlplane

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"strings"

	"github.com/shadewright-labs/sablecourt/internal/config"
	"github.com/shadewright-labs/sablecourt/internal/contextpack"
	"github.com/shadewright-labs/sablecourt/internal/domain"
	"github.com/shadewright-labs/sablecourt/internal/redact"
	scruntime "github.com/shadewright-labs/sablecourt/internal/runtime"
	"github.com/shadewright-labs/sablecourt/internal/store"
)

const maxFinalMessage = 64 * 1024

var errAuthorCheckoutUnavailable = errors.New("runtime cannot guarantee the author's checkout for this step")

// Only the control plane writes step registrations in the reserved system namespace.
const workflowStepsNamespace = "system:workflow-steps"

type workflowStepRecord struct {
	RunID        string
	Workflow     string
	Index        int
	AuthorCommit string
	Status       string `json:",omitempty"`
	Reason       string `json:",omitempty"`
}

func (r *Reconciler) workflowStepEntry(id string, step workflowStepRecord) (domain.MemoryEntry, error) {
	raw, err := json.Marshal(step)
	if err != nil {
		return domain.MemoryEntry{}, err
	}
	return domain.MemoryEntry{Namespace: workflowStepsNamespace, Key: id, Value: string(raw), Author: "controlplane", UpdatedAt: r.now()}, nil
}

func (r *Reconciler) workflowStep(ctx context.Context, id string) (*workflowStepRecord, error) {
	entry, err := r.store.GetMemory(ctx, workflowStepsNamespace, id)
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var step workflowStepRecord
	if err := json.Unmarshal([]byte(entry.Value), &step); err != nil || entry.Author != "controlplane" || step.RunID == "" || step.Workflow == "" || step.Index < 0 {
		return nil, errors.New("invalid workflow step registration")
	}
	return &step, nil
}

func authorCommit(artifacts string) string {
	var items []scruntime.Artifact
	if json.Unmarshal([]byte(artifacts), &items) != nil {
		return ""
	}
	commit := ""
	for _, item := range items {
		if item.Name != "git-commit" {
			continue
		}
		if commit != "" || item.ContentType != "text/plain" || (len(item.Text) != 40 && len(item.Text) != 64) {
			return ""
		}
		if _, err := hex.DecodeString(item.Text); err != nil {
			return ""
		}
		commit = item.Text
	}
	return commit
}

type pinnedStarter interface {
	StartAtCommit(context.Context, scruntime.LaunchSpec, string, bool) (scruntime.Handle, error)
}

func (r *Reconciler) workflowCheckout(ctx context.Context, task domain.Task, wl domain.Workload) (string, bool, error) {
	var meta struct {
		Workflow   string `json:"workflow"`
		Role       string `json:"role"`
		StepIndex  int    `json:"step_index"`
		RetryCount int    `json:"retry_count"`
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
			((role == "author" || wl.Spec.Class == domain.ClassCodingAgent) && meta.RetryCount > 0)
		if role == "verifier" || remediation {
			member, err := r.workflowStep(ctx, task.ID)
			if err != nil {
				return "", false, err
			}
			if member == nil || member.Workflow != meta.Workflow || member.Index != meta.StepIndex || member.AuthorCommit == "" || wl.Spec.Class != domain.ClassCodingAgent {
				return "", false, errAuthorCheckoutUnavailable
			}
			return member.AuthorCommit, role == "verifier", nil
		}
	}
	return "", false, nil
}

func (r *Reconciler) prepareLaunchInput(ctx context.Context, task domain.Task, wl domain.Workload) (string, error) {
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
	if err := config.ValidateTierRoute(wl, provider, model); err != nil {
		return "", "", err
	}
	return provider, model, nil
}

type verifierLaunch struct {
	Commit       string
	PromptDigest string
}

func promptDigest(text string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(text)))
	return hex.EncodeToString(sum[:])
}

func (r *Reconciler) verifierDigest(ctx context.Context, taskID, commit string) (string, error) {
	events, err := r.store.ListTaskEvents(ctx, taskID)
	if err != nil {
		return "", err
	}
	for _, event := range events {
		if event.Reason != "verifier_checkout_pinned" {
			continue
		}
		var evidence verifierLaunch
		if json.Unmarshal([]byte(event.FactJSON), &evidence) == nil && evidence.Commit == commit && commit != "" {
			return evidence.PromptDigest, nil
		}
	}
	return "", nil
}

// The adapter must identify the final message separately from logs and intermediate messages.
func finalVerifierVerdict(artifactsJSON, prompt, digest string) (verdict, findings string) {
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
	if digest == "" || promptDigest(text) == digest || strings.TrimSpace(text) == strings.TrimSpace(prompt) {
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
