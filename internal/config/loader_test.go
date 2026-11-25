package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/shadewright-labs/sablecourt/internal/domain"
)

func writeTempManifest(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "manifests.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write temp manifest: %v", err)
	}
	return path
}

func TestLoadPathValidBundle(t *testing.T) {
	path := writeTempManifest(t, `
apiVersion: sablecourt.io/v1alpha1
kind: Budget
metadata:
  name: triage-daily
spec:
  perTask:
    maxInputTokens: 200000
    maxOutputTokens: 40000
    maxCostUSD: 2.50
  daily:
    maxCostUSD: 15.00
---
apiVersion: sablecourt.io/v1alpha1
kind: Policy
metadata:
  name: default-policy
spec:
  defaultEffect: deny
  rules:
    - workload: triage-agent
      capability: mcp:github.get_issue
      effect: allow
    - workload: triage-agent
      capability: action:git.open_pr
      effect: approve
---
apiVersion: sablecourt.io/v1alpha1
kind: Workload
metadata:
  name: go-coder
  zone: runtime
spec:
  class: coding-agent
  runtime:
    type: worktree-cli
  coding:
    harness: claude
    repoPath: /tmp/repo
    sandbox: seatbelt
  agentCard:
    description: Edits Go code in an isolated worktree.
  grants:
    modelGate:
      allowed: true
      providers: [anthropic]
      budgetRef: triage-daily
---
apiVersion: sablecourt.io/v1alpha1
kind: Workload
metadata:
  name: triage-agent
  zone: runtime
spec:
  class: long-lived-agent
  runtime:
    type: docker
    image: ghcr.io/shadewright-labs/triage:0.1.0
    readOnlyRootfs: true
  agentCard:
    description: Triages issues.
  grants:
    modelGate:
      allowed: true
      providers: [anthropic, openai, gemini]
      budgetRef: triage-daily
    mcpTools: [github.get_issue]
    actions: [git.open_pr]
    delegateTo: [go-coder]
---
apiVersion: sablecourt.io/v1alpha1
kind: Workload
metadata:
  name: health-controller
  zone: runtime
spec:
  class: deterministic-controller
  runtime:
    type: docker
    image: ghcr.io/shadewright-labs/health-ctrl:0.1.0
  agentCard:
    description: Deterministic health check loop.
  grants:
    modelGate:
      allowed: false
`)

	bundle, err := LoadPath(path)
	if err != nil {
		t.Fatalf("LoadPath failed: %v", err)
	}
	if len(bundle.Workloads) != 3 {
		t.Fatalf("expected 3 workloads, got %d", len(bundle.Workloads))
	}
	if len(bundle.Policies) != 1 {
		t.Fatalf("expected 1 policy, got %d", len(bundle.Policies))
	}
	if len(bundle.Budgets) != 1 {
		t.Fatalf("expected 1 budget, got %d", len(bundle.Budgets))
	}
	if bundle.ManifestDigest == "" {
		t.Fatal("expected non-empty manifest digest")
	}
}

func TestLoadPathRejectsDeterministicControllerWithModelGrant(t *testing.T) {
	path := writeTempManifest(t, `
apiVersion: sablecourt.io/v1alpha1
kind: Budget
metadata:
  name: b1
spec:
  daily:
    maxCostUSD: 5.0
---
apiVersion: sablecourt.io/v1alpha1
kind: Workload
metadata:
  name: bad-ctrl
  zone: runtime
spec:
  class: deterministic-controller
  runtime:
    type: docker
    image: alpine:3.21
  agentCard:
    description: Should fail.
  grants:
    modelGate:
      allowed: true
      budgetRef: b1
`)

	_, err := LoadPath(path)
	if err == nil || !strings.Contains(err.Error(), "deterministic-controller must not hold a Model Gate grant") {
		t.Fatalf("expected deterministic-controller error, got: %v", err)
	}
}

func TestLoadPathRejectsSandboxMutatingGrants(t *testing.T) {
	path := writeTempManifest(t, `
apiVersion: sablecourt.io/v1alpha1
kind: Workload
metadata:
  name: sbx-agent
  zone: sandbox
spec:
  class: deterministic-controller
  runtime:
    type: docker
    image: alpine:3.21
  agentCard:
    description: Sandbox workload.
  grants:
    modelGate:
      allowed: false
    actions: [git.open_pr]
`)

	_, err := LoadPath(path)
	if err == nil || !strings.Contains(err.Error(), "sandbox zone workloads cannot hold mutating actions") {
		t.Fatalf("expected sandbox grant error, got: %v", err)
	}
}

func TestLoadPathRejectsUnknownFields(t *testing.T) {
	path := writeTempManifest(t, `
apiVersion: sablecourt.io/v1alpha1
kind: Budget
metadata:
  name: b1
spec:
  daily:
    maxCostUSD: 10.0
  unexpectedField: true
`)

	if _, err := LoadPath(path); err == nil {
		t.Fatal("expected unknown field error, got nil")
	}
}

func TestVassalRoleIdentity(t *testing.T) {
	for _, role := range []string{"@operator", "@system", "@all", " operator ", " coder "} {
		t.Run(role, func(t *testing.T) {
			cc := domain.Conclave{Spec: domain.ConclaveSpec{Vassals: []domain.ConclaveVassal{{Role: role, Workload: "worker", Activation: domain.ActivationAlwaysOn}}}}
			if validateConclave("room", cc, &Bundle{Workloads: map[string]domain.Workload{"worker": {}}}) == nil {
				t.Error("accepted noncanonical role")
			}
		})
	}
}

func TestManifestTierHarness(t *testing.T) {
	const manifest = `
apiVersion: sablecourt.io/v1alpha1
kind: Workload
metadata: {name: author, zone: runtime}
spec:
  class: coding-agent
  runtime: {type: worktree-cli}
  coding: {harness: claude, repoPath: repo}
---
apiVersion: sablecourt.io/v1alpha1
kind: Workflow
metadata: {name: review}
spec:
  steps:
    - {name: edit, workload: author, ROUTE}
`
	for _, tc := range []struct {
		route string
		valid bool
	}{
		{"tier: coding, provider: 'cli:claude', model: sonnet", true},
		{"tier: coding, provider: 'cli:claude', model: gemini-test", false},
		{"tier: coding, provider: 'cli:agy', model: gemini-test", false},
		{"tier: fast", false},
	} {
		t.Run(tc.route, func(t *testing.T) {
			_, err := LoadPath(writeTempManifest(t, strings.Replace(manifest, "ROUTE", tc.route, 1)))
			if (err == nil) != tc.valid {
				t.Fatalf("load returned %v", err)
			}
		})
	}
}

func TestConclaveManifestMaxRuns(t *testing.T) {
	for _, tc := range []struct {
		name string
		max  string
		ok   bool
	}{
		{"default", "", true},
		{"one", "  wards: {maxRuns: 1}\n", true},
		{"ceiling", "  wards: {maxRuns: 6}\n", true},
		{"above ceiling", "  wards: {maxRuns: 7}\n", false},
		{"negative", "  wards: {maxRuns: -1}\n", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			data := `apiVersion: sablecourt.io/v1alpha1
kind: Workload
metadata:
  name: worker
  zone: runtime
spec:
  class: deterministic-controller
  runtime:
    type: docker
    image: worker:0.1.0
---
apiVersion: sablecourt.io/v1alpha1
kind: Conclave
metadata:
  name: council
spec:
  vassals:
    - {role: reader, workload: worker, activation: on-mention}
` + tc.max
			if err := os.WriteFile(filepath.Join(root, "conclave.yaml"), []byte(data), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := LoadPath(root); (err == nil) != tc.ok {
				t.Errorf("manifest error = %v, want valid = %v", err, tc.ok)
			}
		})
	}
}

func TestLoadPathRefusesReservedCouncilPanelNames(t *testing.T) {
	for _, name := range []string{"claude", "codex", "agy"} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			data := `apiVersion: sablecourt.io/v1alpha1
kind: Workload
metadata:
  name: ` + name + `
  zone: sandbox
spec:
  class: coding-agent
  coding:
    harness: ` + name + `
    repoPath: /tmp/repo
    sandbox: seatbelt
  runtime:
    type: worktree-cli
  grants:
    mcpTools:
      - conclave.read
`
			if err := os.WriteFile(filepath.Join(root, "panel.yaml"), []byte(data), 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := LoadPath(root)
			if err == nil {
				t.Fatalf("LoadPath accepted reserved council panel workload name %q", name)
			}
			if !strings.Contains(err.Error(), "reserved") {
				t.Fatalf("LoadPath error = %q, want clear reserved error", err)
			}
		})
	}
}
