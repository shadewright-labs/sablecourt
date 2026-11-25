package policy

import (
	"testing"
	"time"

	"github.com/shadewright-labs/sablecourt/internal/domain"
)

func TestEngineEvaluateInvariants(t *testing.T) {
	breaker := NewCircuitBreaker(2, time.Minute)
	policies := []domain.Policy{
		{
			Metadata: domain.ObjectMeta{Name: "default"},
			Spec: domain.PolicySpec{
				DefaultEffect: domain.EffectDeny,
				Rules: []domain.PolicyRule{
					{Workload: "triage-agent", Capability: "mcp:github.get_issue", Effect: domain.EffectAllow},
					{Workload: "triage-agent", Capability: "action:git.open_pr", Effect: domain.EffectApprove},
					{Workload: "*", Capability: "action:git.force_push", Effect: domain.EffectDeny},
				},
			},
		},
	}
	eng := NewEngine(policies, breaker)

	runtimeAgent := domain.Workload{
		Metadata: domain.ObjectMeta{Name: "triage-agent", Zone: domain.ZoneRuntime},
		Spec: domain.WorkloadSpec{
			Class: domain.ClassLongLivedAgent,
			Grants: domain.GrantSet{
				MCPTools: []string{"github.get_issue"},
				Actions:  []string{"git.open_pr", "git.force_push"},
			},
		},
	}

	if d := eng.Evaluate(runtimeAgent, "mcp:github.get_issue"); d.Effect != domain.EffectAllow {
		t.Fatalf("expected allow, got %+v", d)
	}
	if d := eng.Evaluate(runtimeAgent, "action:git.open_pr"); d.Effect != domain.EffectApprove {
		t.Fatalf("expected approve, got %+v", d)
	}
	if d := eng.Evaluate(runtimeAgent, "action:git.force_push"); d.Effect != domain.EffectDeny {
		t.Fatalf("expected explicit deny, got %+v", d)
	}
	if d := eng.Evaluate(runtimeAgent, "mcp:github.delete_repo"); d.Effect != domain.EffectDeny {
		t.Fatalf("expected deny for ungranted tool, got %+v", d)
	}
	if d := eng.Evaluate(runtimeAgent, "policy:write"); d.Effect != domain.EffectDeny {
		t.Fatalf("expected deny for policy self-modification, got %+v", d)
	}

	// Circuit breaker trips after 2 failures and fails closed.
	breaker.RecordFailure(BreakerKey("action:git.open_pr", runtimeAgent.Metadata.Name))
	breaker.RecordFailure(BreakerKey("action:git.open_pr", runtimeAgent.Metadata.Name))
	if d := eng.Evaluate(runtimeAgent, "action:git.open_pr"); d.Effect != domain.EffectDeny || d.Reason != "circuit_breaker_open" {
		t.Fatalf("expected circuit_breaker_open deny, got %+v", d)
	}

	// Sandbox zone cannot run mutating actions even if listed.
	sandboxAgent := domain.Workload{
		Metadata: domain.ObjectMeta{Name: "triage-agent", Zone: domain.ZoneSandbox},
		Spec:     runtimeAgent.Spec,
	}
	if d := eng.Evaluate(sandboxAgent, "action:git.open_pr"); d.Effect != domain.EffectDeny {
		t.Fatalf("expected sandbox deny, got %+v", d)
	}
}

func TestRecordOutcomeOpensTheBreakerEvaluateChecks(t *testing.T) {
	wl := domain.Workload{
		Metadata: domain.ObjectMeta{Name: "pr-agent", Zone: domain.ZoneRuntime},
		Spec:     domain.WorkloadSpec{Grants: domain.GrantSet{Actions: []string{"git.open_pr"}}},
	}
	pols := []domain.Policy{{Spec: domain.PolicySpec{Rules: []domain.PolicyRule{{Workload: "pr-agent", Capability: "action:git.open_pr", Effect: domain.EffectAllow}}}}}
	breaker := NewCircuitBreaker(3, time.Minute)
	now := time.Now().UTC()
	breaker.now = func() time.Time { return now }
	first := NewEngine(pols, breaker)
	for i := 0; i < 3; i++ {
		first.RecordOutcome("action:git.open_pr", "pr-agent", true)
	}
	// An engine rebuilt on reload with the same breaker sees the open breaker.
	if d := NewEngine(pols, breaker).Evaluate(wl, "action:git.open_pr"); d.Reason != "circuit_breaker_open" {
		t.Fatalf("want circuit_breaker_open, got %+v", d)
	}
	// Another workload with the same grant keeps its own breaker.
	other := wl
	other.Metadata.Name = "other-agent"
	otherPols := []domain.Policy{{Spec: domain.PolicySpec{Rules: []domain.PolicyRule{{Workload: "*", Capability: "action:git.open_pr", Effect: domain.EffectAllow}}}}}
	if d := NewEngine(otherPols, breaker).Evaluate(other, "action:git.open_pr"); d.Effect != domain.EffectAllow {
		t.Fatalf("another workload must not share the open breaker, got %+v", d)
	}
	// A success does not close the open breaker; the cool-down does.
	first.RecordOutcome("action:git.open_pr", "pr-agent", false)
	if d := first.Evaluate(wl, "action:git.open_pr"); d.Reason != "circuit_breaker_open" {
		t.Fatalf("success closed the breaker before its cool-down, got %+v", d)
	}
	now = now.Add(2 * time.Minute)
	if d := first.Evaluate(wl, "action:git.open_pr"); d.Effect != domain.EffectAllow {
		t.Fatalf("breaker still open after its cool-down, got %+v", d)
	}
	var nilEngine *Engine
	nilEngine.RecordOutcome("action:git.open_pr", "pr-agent", true)
	NewEngine(pols, nil).RecordOutcome("action:git.open_pr", "pr-agent", true)
}

// A success that started before the breaker opened does not close it before
// its cool-down ends.
func TestInFlightSuccessDoesNotCloseAnOpenBreaker(t *testing.T) {
	breaker := NewCircuitBreaker(2, time.Minute)
	now := time.Now().UTC()
	breaker.now = func() time.Time { return now }
	breaker.RecordFailure("k")
	breaker.RecordFailure("k")
	breaker.RecordSuccess("k")
	if !breaker.IsOpen("k") {
		t.Fatal("a success closed the open breaker before its cool-down")
	}
	now = now.Add(2 * time.Minute)
	if breaker.IsOpen("k") {
		t.Fatal("breaker still open after its cool-down")
	}
}
