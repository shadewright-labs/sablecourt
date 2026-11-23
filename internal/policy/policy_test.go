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
	breaker.RecordFailure("action:git.open_pr")
	breaker.RecordFailure("action:git.open_pr")
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
