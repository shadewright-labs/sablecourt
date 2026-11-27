package config

import (
	"strings"
	"testing"
)

// verifierWorkloads is a bundle of the workloads the verifier cases use.
const verifierWorkloads = `apiVersion: sablecourt.io/v1alpha1
kind: Budget
metadata: {name: daily}
spec: {daily: {maxCostUSD: 5}}
---
apiVersion: sablecourt.io/v1alpha1
kind: Workload
metadata: {name: author, zone: runtime}
spec:
  class: coding-agent
  runtime: {type: worktree-cli}
  coding: {harness: claude, repoPath: repo}
---
apiVersion: sablecourt.io/v1alpha1
kind: Workload
metadata: {name: fixer, zone: runtime}
spec:
  class: coding-agent
  runtime: {type: worktree-cli}
  coding: {harness: agy, repoPath: repo}
---
apiVersion: sablecourt.io/v1alpha1
kind: Workload
metadata: {name: reviewer, zone: runtime}
spec:
  class: coding-agent
  runtime: {type: worktree-cli}
  coding: {harness: codex, repoPath: repo}
---
apiVersion: sablecourt.io/v1alpha1
kind: Workload
metadata: {name: gated-coder, zone: runtime}
spec:
  class: coding-agent
  runtime: {type: worktree-cli}
  coding: {harness: codex, repoPath: repo}
  grants:
    modelGate: {allowed: true, providers: [openai], budgetRef: daily}
---
apiVersion: sablecourt.io/v1alpha1
kind: Workload
metadata: {name: gated-agent, zone: runtime}
spec:
  class: long-lived-agent
  runtime: {type: docker, image: agent:0.1.0}
  grants:
    modelGate: {allowed: true, providers: [openai], budgetRef: daily}
---
apiVersion: sablecourt.io/v1alpha1
kind: Workload
metadata: {name: actor, zone: runtime}
spec:
  class: coding-agent
  runtime: {type: worktree-cli}
  coding: {harness: codex, repoPath: repo}
  grants:
    actions: [git.open_pr]
---
apiVersion: sablecourt.io/v1alpha1
kind: Workload
metadata: {name: delegator, zone: runtime}
spec:
  class: coding-agent
  runtime: {type: worktree-cli}
  coding: {harness: codex, repoPath: repo}
  grants:
    delegateTo: [author]
---
apiVersion: sablecourt.io/v1alpha1
kind: Workload
metadata: {name: checker, zone: runtime}
spec:
  class: deterministic-controller
  runtime: {type: docker, image: checker:0.1.0}
---
apiVersion: sablecourt.io/v1alpha1
kind: Workload
metadata: {name: acting-checker, zone: runtime}
spec:
  class: deterministic-controller
  runtime: {type: docker, image: checker:0.1.0}
  grants:
    actions: [git.open_pr]
---
apiVersion: sablecourt.io/v1alpha1
kind: Workflow
metadata: {name: court}
spec:
  steps:
`

func TestLoadPathVerifierSteps(t *testing.T) {
	const (
		edit     = "    - {name: edit, workload: author, role: author, tier: coding}\n"
		review   = "    - {name: review, workload: reviewer, role: verifier, tier: critic}\n"
		security = "    - {name: security, workload: reviewer, role: verifier, provider: 'cli:codex', model: gpt-6.2-astra}\n"
	)
	cases := []struct {
		name    string
		steps   string
		wantErr string
	}{
		{"author then critic verifier", edit + review, ""},
		{"consecutive verifiers on one read-only workload", edit + review + security, ""},
		{"fast step, author, verifier", "    - {name: triage, workload: fixer, tier: fast}\n" + edit + review, ""},
		{"deterministic verifier", edit + "    - {name: check, workload: checker, role: verifier}\n", ""},
		{"deterministic verifier first", "    - {name: check, workload: checker, role: verifier}\n" + edit, ""},
		{"verifier workload reused by a later author", edit + review + "    - {name: docs, workload: reviewer, role: author, provider: 'cli:codex', model: gpt-6.2-astra}\n", ""},
		{"no verifier steps at all", edit + "    - {name: deliver, workload: actor, provider: 'cli:codex', model: gpt-6.2-astra}\n", ""},

		{"verifier on the author's workload", edit + "    - {name: review, workload: author, role: verifier, provider: 'cli:claude', model: claude-opus-5-5}\n", `verifier workload "author" is also used by step "edit"`},
		{"verifier on a fast step's workload", "    - {name: triage, workload: fixer, tier: fast}\n" + edit + "    - {name: review, workload: fixer, role: verifier, provider: 'cli:agy', model: gemini-3.8-pro}\n", `also used by step "triage"`},
		{"deterministic verifier on the author's workload", "    - {name: prep, workload: checker}\n    - {name: check, workload: checker, role: verifier}\n", `also used by step "prep"`},
		{"verifier with a Model Gate grant, coding agent", edit + "    - {name: review, workload: gated-coder, role: verifier, tier: critic}\n", "Model Gate grant"},
		{"verifier with a Model Gate grant, long-lived agent", edit + "    - {name: review, workload: gated-agent, role: verifier, provider: openai, model: gpt-6.2-astra}\n", "Model Gate grant"},
		{"verifier holding actions", edit + "    - {name: review, workload: actor, role: verifier, tier: critic}\n", "actions or delegateTo"},
		{"verifier holding delegateTo", edit + "    - {name: review, workload: delegator, role: verifier, tier: critic}\n", "actions or delegateTo"},
		{"deterministic verifier holding actions", edit + "    - {name: check, workload: acting-checker, role: verifier}\n", "actions or delegateTo"},
		{"model verifier as the first step", review + edit, "no earlier step ran a model"},
		{"model verifier after only a deterministic step", "    - {name: prep, workload: checker}\n" + review, "no earlier step ran a model"},
		{"model verifier after only verifiers", "    - {name: check, workload: checker, role: verifier}\n" + review, "no earlier step ran a model"},
		{"critic tier on a claude workload", "    - {name: edit, workload: fixer, role: author, tier: fast}\n    - {name: review, workload: author, role: verifier, tier: critic}\n", "incompatible with the workload harness"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := LoadPath(writeTempManifest(t, verifierWorkloads+tc.steps))
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("LoadPath refused: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("LoadPath = %v, want error containing %q", err, tc.wantErr)
			}
		})
	}
}
