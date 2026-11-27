package config

import (
	"reflect"
	"strings"
	"testing"

	"github.com/shadewright-labs/sablecourt/internal/domain"
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
kind: Workload
metadata: {name: memory-writer, zone: runtime}
spec:
  class: coding-agent
  runtime: {type: worktree-cli}
  coding: {harness: codex, repoPath: repo}
  grants:
    mcpTools: [memory.get, memory.put]
---
apiVersion: sablecourt.io/v1alpha1
kind: Workload
metadata: {name: memory-deleter, zone: runtime}
spec:
  class: coding-agent
  runtime: {type: worktree-cli}
  coding: {harness: codex, repoPath: repo}
  grants:
    mcpTools: [memory.delete]
---
apiVersion: sablecourt.io/v1alpha1
kind: Workload
metadata: {name: poster, zone: runtime}
spec:
  class: coding-agent
  runtime: {type: worktree-cli}
  coding: {harness: codex, repoPath: repo}
  grants:
    mcpTools: [conclave.post]
---
apiVersion: sablecourt.io/v1alpha1
kind: Workload
metadata: {name: relic-writer, zone: runtime}
spec:
  class: coding-agent
  runtime: {type: worktree-cli}
  coding: {harness: codex, repoPath: repo}
  grants:
    mcpTools: [conclave.relic_put]
---
apiVersion: sablecourt.io/v1alpha1
kind: Workload
metadata: {name: upstream-user, zone: runtime}
spec:
  class: coding-agent
  runtime: {type: worktree-cli}
  coding: {harness: codex, repoPath: repo}
  grants:
    mcpTools: [docs.search]
---
apiVersion: sablecourt.io/v1alpha1
kind: Workload
metadata: {name: upstream-holder, zone: runtime}
spec:
  class: coding-agent
  runtime: {type: worktree-cli}
  coding: {harness: codex, repoPath: repo}
  grants:
    mcpServers: [{name: docs, url: "https://mcp.example.test/mcp", allowedTools: [search]}]
---
apiVersion: sablecourt.io/v1alpha1
kind: Workload
metadata: {name: reader, zone: runtime}
spec:
  class: coding-agent
  runtime: {type: worktree-cli}
  coding: {harness: codex, repoPath: repo}
  grants:
    mcpTools: [memory.get, memory.search, conclave.read, conclave.relic_get, sablecourt.list_workloads]
---
apiVersion: sablecourt.io/v1alpha1
kind: Workload
metadata: {name: idle-gate, zone: runtime}
spec:
  class: coding-agent
  runtime: {type: worktree-cli}
  coding: {harness: codex, repoPath: repo}
  grants:
    modelGate: {allowed: false, providers: [openai], budgetRef: daily}
---
apiVersion: sablecourt.io/v1alpha1
kind: Workload
metadata: {name: empty-actions, zone: runtime}
spec:
  class: coding-agent
  runtime: {type: worktree-cli}
  coding: {harness: codex, repoPath: repo}
  grants:
    actions: []
---
apiVersion: sablecourt.io/v1alpha1
kind: Workload
metadata: {name: empty-repos, zone: runtime}
spec:
  class: coding-agent
  runtime: {type: worktree-cli}
  coding: {harness: codex, repoPath: repo}
  grants:
    repos: []
---
apiVersion: sablecourt.io/v1alpha1
kind: Workload
metadata: {name: empty-delegates, zone: runtime}
spec:
  class: coding-agent
  runtime: {type: worktree-cli}
  coding: {harness: codex, repoPath: repo}
  grants:
    delegateTo: []
---
apiVersion: sablecourt.io/v1alpha1
kind: Workload
metadata: {name: empty-servers, zone: runtime}
spec:
  class: coding-agent
  runtime: {type: worktree-cli}
  coding: {harness: codex, repoPath: repo}
  grants:
    mcpServers: []
---
apiVersion: sablecourt.io/v1alpha1
kind: Workload
metadata: {name: empty-lists, zone: runtime}
spec:
  class: coding-agent
  runtime: {type: worktree-cli}
  coding: {harness: codex, repoPath: repo}
  grants: {actions: [], repos: [], delegateTo: [], mcpServers: [], mcpTools: []}
---
apiVersion: sablecourt.io/v1alpha1
kind: Workload
metadata: {name: empty-gate, zone: runtime}
spec:
  class: coding-agent
  runtime: {type: worktree-cli}
  coding: {harness: codex, repoPath: repo}
  grants:
    modelGate: {providers: []}
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
		{"verifier holding only read-only tools", edit + "    - {name: review, workload: reader, role: verifier, tier: critic}\n", ""},
		{"no verifier steps at all", edit + "    - {name: deliver, workload: actor, provider: 'cli:codex', model: gpt-6.2-astra}\n", ""},
		{"verifier declaring an empty actions list", edit + "    - {name: review, workload: empty-actions, role: verifier, tier: critic}\n", ""},
		{"verifier declaring an empty repos list", edit + "    - {name: review, workload: empty-repos, role: verifier, tier: critic}\n", ""},
		{"verifier declaring an empty delegateTo list", edit + "    - {name: review, workload: empty-delegates, role: verifier, tier: critic}\n", ""},
		{"verifier declaring an empty mcpServers list", edit + "    - {name: review, workload: empty-servers, role: verifier, tier: critic}\n", ""},
		{"verifier declaring every list empty", edit + "    - {name: review, workload: empty-lists, role: verifier, tier: critic}\n", ""},

		{"verifier on the author's workload", edit + "    - {name: review, workload: author, role: verifier, provider: 'cli:claude', model: claude-opus-5-5}\n", `verifier workload "author" is also used by step "edit"`},
		{"verifier on a fast step's workload", "    - {name: triage, workload: fixer, tier: fast}\n" + edit + "    - {name: review, workload: fixer, role: verifier, provider: 'cli:agy', model: gemini-3.8-pro}\n", `also used by step "triage"`},
		{"deterministic verifier on the author's workload", "    - {name: prep, workload: checker}\n    - {name: check, workload: checker, role: verifier}\n", `also used by step "prep"`},
		{"verifier with a Model Gate grant, coding agent", edit + "    - {name: review, workload: gated-coder, role: verifier, tier: critic}\n", "grants.modelGate"},
		{"verifier with a Model Gate grant, long-lived agent", edit + "    - {name: review, workload: gated-agent, role: verifier, provider: openai, model: gpt-6.2-astra}\n", "grants.modelGate"},
		{"verifier holding actions", edit + "    - {name: review, workload: actor, role: verifier, tier: critic}\n", "not grants.actions"},
		{"verifier holding delegateTo", edit + "    - {name: review, workload: delegator, role: verifier, tier: critic}\n", "not grants.delegateTo"},
		{"deterministic verifier holding actions", edit + "    - {name: check, workload: acting-checker, role: verifier}\n", "not grants.actions"},
		{"verifier holding memory.put", edit + "    - {name: review, workload: memory-writer, role: verifier, tier: critic}\n", `mcpTools entry "memory.put" is not read-only`},
		{"verifier holding memory.delete", edit + "    - {name: review, workload: memory-deleter, role: verifier, tier: critic}\n", `mcpTools entry "memory.delete" is not read-only`},
		{"verifier holding conclave.post", edit + "    - {name: review, workload: poster, role: verifier, tier: critic}\n", `mcpTools entry "conclave.post" is not read-only`},
		{"verifier holding conclave.relic_put", edit + "    - {name: review, workload: relic-writer, role: verifier, tier: critic}\n", `mcpTools entry "conclave.relic_put" is not read-only`},
		{"verifier holding an upstream tool", edit + "    - {name: review, workload: upstream-user, role: verifier, tier: critic}\n", `mcpTools entry "docs.search" is not read-only`},
		{"verifier holding mcpServers", edit + "    - {name: review, workload: upstream-holder, role: verifier, tier: critic}\n", "not grants.mcpServers"},
		{"verifier with an idle Model Gate grant", edit + "    - {name: review, workload: idle-gate, role: verifier, tier: critic}\n", "not grants.modelGate"},
		{"verifier with a Model Gate grant of empty providers", edit + "    - {name: review, workload: empty-gate, role: verifier, tier: critic}\n", "not grants.modelGate"},
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

// TestVerifierGrantFieldsClassifyEveryGrant fails when GrantSet gains a field
// the verifier allow-list does not classify, so a new grant cannot reach a
// verifier unreviewed.
func TestVerifierGrantFieldsClassifyEveryGrant(t *testing.T) {
	typ := reflect.TypeOf(domain.GrantSet{})
	seen := map[string]bool{}
	for i := range typ.NumField() {
		name := typ.Field(i).Name
		seen[name] = true
		if _, ok := verifierGrantFields[name]; !ok {
			t.Errorf("GrantSet.%s is not classified in verifierGrantFields", name)
		}
	}
	for name := range verifierGrantFields {
		if !seen[name] {
			t.Errorf("verifierGrantFields names %s, which GrantSet does not have", name)
		}
	}
}

// TestVerifierGrantsRefuseEachNonReadOnlyField sets each field classified as
// empty to a non-zero value and expects a refusal.
func TestVerifierGrantsRefuseEachNonReadOnlyField(t *testing.T) {
	bundle := &Bundle{Workloads: map[string]domain.Workload{}}
	typ := reflect.TypeOf(domain.GrantSet{})
	for i := range typ.NumField() {
		f := typ.Field(i)
		if verifierGrantFields[f.Name] != verifierGrantEmpty {
			continue
		}
		var g domain.GrantSet
		v := reflect.ValueOf(&g).Elem().Field(i)
		switch v.Kind() {
		case reflect.Slice:
			v.Set(reflect.MakeSlice(v.Type(), 1, 1))
		case reflect.Struct:
			v.Field(0).Set(reflect.ValueOf(true).Convert(v.Field(0).Type()))
		default:
			t.Fatalf("GrantSet.%s has kind %s; extend this test", f.Name, v.Kind())
		}
		if err := validateVerifierGrants(bundle, "v", g); err == nil {
			t.Errorf("GrantSet.%s set: verifier grants accepted", f.Name)
		}
		// An empty, non-nil list grants nothing and is accepted.
		if v.Kind() == reflect.Slice {
			v.Set(reflect.MakeSlice(v.Type(), 0, 0))
			if err := validateVerifierGrants(bundle, "v", g); err != nil {
				t.Errorf("GrantSet.%s empty list refused: %v", f.Name, err)
			}
		}
	}
	ok := domain.GrantSet{MCPTools: append([]string(nil), verifierReadOnlyTools...)}
	if err := validateVerifierGrants(bundle, "v", ok); err != nil {
		t.Fatalf("read-only tools refused: %v", err)
	}
}

// A read-only tool name that an upstream server in the bundle also registers
// would reach that server, because the gate keeps one tool per name.
func TestVerifierGrantsRefuseShadowedReadOnlyTool(t *testing.T) {
	bundle := &Bundle{Workloads: map[string]domain.Workload{
		"proxy": {Spec: domain.WorkloadSpec{Grants: domain.GrantSet{MCPServers: []domain.UpstreamMCPServer{
			{Name: "memory", URL: "https://mcp.example.test/mcp", AllowedTools: []string{" get "}},
		}}}},
	}}
	err := validateVerifierGrants(bundle, "v", domain.GrantSet{MCPTools: []string{"memory.get"}})
	if err == nil || !strings.Contains(err.Error(), `also an upstream tool of workload "proxy"`) {
		t.Fatalf("validateVerifierGrants = %v, want shadowing refusal", err)
	}
	if err := validateVerifierGrants(bundle, "v", domain.GrantSet{MCPTools: []string{"memory.search"}}); err != nil {
		t.Fatalf("unshadowed tool refused: %v", err)
	}
}
