package controlplane

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"unicode/utf8"

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

// stepAuthorCommit is the commit a verifier following the completed step must
// review. Only an author step's artifacts set a new commit; a completed verifier
// passes on the commit it was pinned to, whatever artifacts it emitted.
func stepAuthorCommit(completed *workflowStepRecord, completedVerifier bool, artifacts string) string {
	if completedVerifier {
		return completed.AuthorCommit
	}
	return authorCommit(artifacts)
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

type councilContainerCapable interface {
	SupportsCouncilContainers() bool
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

func sameCheckoutSource(a, b string) bool {
	if a == b {
		return true
	}
	if a == "" || b == "" {
		return false
	}
	absA, errA := filepath.Abs(a)
	absB, errB := filepath.Abs(b)
	if errA != nil || errB != nil {
		return false
	}
	ra, errA := filepath.EvalSymlinks(absA)
	rb, errB := filepath.EvalSymlinks(absB)
	return errA == nil && errB == nil && ra == rb
}

// The coding adapter chooses its checkout in Start from these reserved fields.
func (r *Reconciler) prepareCouncilCheckout(ctx context.Context, payload map[string]json.RawMessage, assignment store.ConclaveAssignment, wl domain.Workload) (domain.Workload, error) {
	councils, err := store.Councils(r.store)
	if err != nil {
		return wl, err
	}
	s, err := councils.CouncilSnapshot(ctx, assignment.ConclaveID)
	if err != nil {
		return wl, err
	}
	c := s.Checkout
	if wl.Spec.Coding == nil && c == (store.CouncilCheckout{}) {
		return wl, nil
	}
	if wl.Spec.Class != domain.ClassCodingAgent || wl.Spec.Coding == nil {
		return wl, errors.New("council checkout isolation unavailable")
	}
	if c.Source != "" {
		if wl.Spec.Coding.RepoPath != "" && !sameCheckoutSource(c.Source, wl.Spec.Coding.RepoPath) {
			return wl, errors.New("council checkout isolation unavailable")
		}
		admitted := c.Source
		if c.SourceDev != 0 || c.SourceIno != 0 {
			if !filepath.IsAbs(admitted) {
				return wl, errors.New("council checkout isolation unavailable")
			}
			resolved, err := filepath.EvalSymlinks(admitted)
			if err != nil || resolved != admitted {
				return wl, errors.New("council checkout isolation unavailable")
			}
			info, err := os.Lstat(admitted)
			if err != nil || !info.IsDir() {
				return wl, errors.New("council checkout isolation unavailable")
			}
			stat, ok := info.Sys().(*syscall.Stat_t)
			if !ok || uint64(stat.Dev) != c.SourceDev || stat.Ino != c.SourceIno {
				return wl, errors.New("council checkout isolation unavailable")
			}
		} else if filepath.IsAbs(admitted) {
			resolved, err := filepath.EvalSymlinks(admitted)
			if err != nil {
				return wl, errors.New("council checkout isolation unavailable")
			}
			admitted = resolved
		} else if wl.Spec.Coding.RepoPath == "" {
			return wl, errors.New("council checkout isolation unavailable")
		}
		cfg := *wl.Spec.Coding
		cfg.RepoPath = admitted
		wl.Spec.Coding = &cfg
	} else if wl.Spec.Coding.RepoPath == "" {
		return wl, errors.New("council checkout isolation unavailable")
	}

	// Rebuild from the pinned policy and seat, including reservations made
	// before the dispatcher started writing explicit checkout fields.
	for key := range payload {
		if strings.EqualFold(key, "role") || isCheckoutField(key) {
			delete(payload, key)
		}
	}
	mode := "live"
	if c.Commit != "" {
		mode = "ref"
		payload["checkoutRef"], _ = json.Marshal(c.Commit)
	}
	if c.WriterRole != "" {
		mode = "writer"
		payload["checkoutWriter"], _ = json.Marshal(c.WriterRole)
	}
	if c.SourceDev != 0 || c.SourceIno != 0 {
		payload["checkoutSourceId"], _ = json.Marshal(fmt.Sprintf("%d:%d", c.SourceDev, c.SourceIno))
	}
	payload["checkoutMode"], _ = json.Marshal(mode)
	payload["role"], _ = json.Marshal(assignment.Role)
	return wl, nil
}

func (r *Reconciler) prepareLaunchInput(ctx context.Context, task domain.Task, wl domain.Workload) (domain.Workload, string, []string, error) {
	assignment, assigned := store.TaskConclaveAssignment(task)
	council := assigned && assignment.Round > 0
	var payload map[string]json.RawMessage
	if err := json.Unmarshal([]byte(task.InputJSON), &payload); err != nil || payload == nil {
		prompt, _ := json.Marshal(task.InputJSON)
		payload = map[string]json.RawMessage{"prompt": prompt}
	}
	var unreservedCouncil bool
	if !assigned {
		var err error
		unreservedCouncil, err = referencesCouncil(ctx, r.store, payload)
		if err != nil {
			return wl, "", nil, err
		}
	}
	if wl.Spec.Class == domain.ClassDeterministicController && !council && !unreservedCouncil {
		return wl, task.InputJSON, nil, nil
	}

	// Only the dispatcher can create a round reservation. Its immutable input
	// carries the already filtered packet. Other tasks receive an assembled one.
	var markdown string
	if council {
		var err error
		wl, err = r.prepareCouncilCheckout(ctx, payload, assignment, wl)
		if err != nil {
			return wl, "", nil, err
		}
		const maxCouncilPacketBytes = 32 << 10 // AssembleCouncil's total packet budget.
		packet := payload["context_packet"]
		// Check the stored bytes before JSON decoding can replace invalid UTF-8.
		if !utf8.Valid(packet) || json.Unmarshal(packet, &markdown) != nil ||
			strings.TrimSpace(markdown) == "" || len(markdown) > maxCouncilPacketBytes ||
			redact.String(markdown) != markdown {
			return wl, "", nil, errors.New("invalid reserved council packet")
		}
	}
	// Typed consumers may match field names case-insensitively. The checkout
	// kind comes from the task type: only a council round (rebuilt above from
	// its reservation) carries checkout fields, so an ordinary task always
	// runs in its own task worktree whatever its submission says.
	rebuild := false
	for key := range payload {
		if strings.EqualFold(key, "context_packet") || ((assigned || unreservedCouncil) && strings.EqualFold(key, "conclaveId")) ||
			(!council && isCheckoutField(key)) {
			delete(payload, key)
			rebuild = true
		}
		if strings.EqualFold(key, "repo_path") {
			rebuild = true
		}
	}
	if assigned {
		payload["conclaveId"], _ = json.Marshal(assignment.ConclaveID)
	}
	if wl.Spec.Class == domain.ClassDeterministicController && unreservedCouncil {
		raw, err := json.Marshal(payload)
		return wl, string(raw), nil, err
	}
	if rebuild && !council {
		precheckInput := maps.Clone(payload)
		maps.DeleteFunc(precheckInput, func(key string, _ json.RawMessage) bool {
			return strings.EqualFold(key, "repo_path")
		})
		sanitized, err := json.Marshal(precheckInput)
		if err != nil {
			return wl, "", nil, err
		}
		task.InputJSON = string(sanitized)
	}

	if raw, ok := payload["tier"]; ok {
		var tier, provider, model string
		if err := json.Unmarshal(raw, &tier); err != nil || tier == "" {
			return wl, "", nil, errors.New("invalid model tier")
		}
		if raw, ok := payload["provider"]; ok {
			if err := json.Unmarshal(raw, &provider); err != nil || provider == "" {
				return wl, "", nil, errors.New("invalid tier provider")
			}
		}
		if raw, ok := payload["model"]; ok {
			if err := json.Unmarshal(raw, &model); err != nil || model == "" {
				return wl, "", nil, errors.New("invalid tier model")
			}
		}
		provider, model, err := resolveTierRoute(ctx, r.store, wl, tier, provider, model)
		if err != nil {
			return wl, "", nil, err
		}
		payload["provider"], _ = json.Marshal(provider)
		payload["model"], _ = json.Marshal(model)
	}

	// A packet with learnings pins the task's leases to the providers it was
	// admitted under, so a later reload cannot widen where they go (ADR-0012).
	var pinned []string
	if !council {
		packet := contextpack.Assemble(ctx, r.store, task, wl, r.packetDest)
		markdown = packet.Markdown
		if packet.LearningCount > 0 {
			pinned = append([]string{}, wl.Spec.Grants.ModelGate.Providers...)
		}
	}
	payload["context_packet"], _ = json.Marshal(markdown)
	raw, err := json.Marshal(payload)
	return wl, string(raw), pinned, err
}

// isCheckoutField reports whether key selects a council checkout, in any case.
func isCheckoutField(key string) bool {
	return strings.EqualFold(key, "checkoutMode") || strings.EqualFold(key, "checkoutRef") ||
		strings.EqualFold(key, "checkoutWriter") || strings.EqualFold(key, "checkoutSourceId")
}

func referencesCouncil(ctx context.Context, st store.Store, payload map[string]json.RawMessage) (bool, error) {
	councils, err := store.Councils(st)
	if err != nil {
		return false, nil
	}
	// A typed reader can match any case variant of the room field.
	for key, value := range payload {
		if !strings.EqualFold(key, "conclaveId") {
			continue
		}
		var room string
		if json.Unmarshal(value, &room) != nil || room == "" {
			continue
		}
		_, err := councils.CouncilSnapshot(ctx, room)
		if err == nil {
			return true, nil
		}
		if !errors.Is(err, store.ErrNotFound) {
			return false, fmt.Errorf("check council room: %w", err)
		}
	}
	return false, nil
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

// CouncilWriterDiffName identifies the immutable Relic for one attempt.
func CouncilWriterDiffName(taskID string) string { return "git-diff-" + taskID }

// CouncilWriterDiff selects exactly one inline diff. A nil Relic with valid
// evidence means the writer made no changes.
func CouncilWriterDiff(a store.ConclaveAssignment, taskID string, artifacts []*scruntime.Artifact) (relic *domain.ConclaveRelic, valid bool) {
	for _, artifact := range artifacts {
		if artifact == nil {
			return nil, false
		}
		if artifact.Name != "git-diff" {
			continue
		}
		if relic != nil || artifact.URI != "" || artifact.ContentType != "text/x-diff" || !store.ValidCouncilText(artifact.Text) {
			return nil, false
		}
		body := store.RedactCouncilAnswer(artifact.Text)
		if !store.ValidCouncilText(body) {
			return nil, false
		}
		relic = &domain.ConclaveRelic{ConclaveID: a.ConclaveID, AuthorRole: a.Role, Name: CouncilWriterDiffName(taskID),
			ContentType: "text/x-diff", Body: body}
	}
	return relic, true
}
