package policy

import (
	"testing"
	"time"

	"github.com/shadewright-labs/sablecourt/internal/domain"
)

func TestEngineEvaluateDecisionTable(t *testing.T) {
	policies := []domain.Policy{{
		Metadata: domain.ObjectMeta{Name: "p"},
		Spec: domain.PolicySpec{Rules: []domain.PolicyRule{
			{Workload: "coder-*", Capability: "mcp:repo.*", Effect: domain.EffectAllow},
			{Workload: "lead", Capability: "delegate:coder-a", Effect: domain.EffectAllow},
			{Workload: "lead", Capability: "delegate:coder-b", Effect: domain.EffectApprove},
			{Workload: "lead", Capability: "delegate:coder-b", Effect: domain.EffectAllow},
			{Workload: "lead", Capability: "mcp:weird.tool", Effect: domain.PolicyEffect("maybe")},
		}},
	}}
	eng := NewEngine(policies, nil)

	coder := domain.Workload{
		Metadata: domain.ObjectMeta{Name: "coder-a", Zone: domain.ZoneRuntime},
		Spec: domain.WorkloadSpec{Grants: domain.GrantSet{
			MCPTools: []string{"repo.read", "other.read"},
			Actions:  []string{"git.push"},
		}},
	}
	lead := domain.Workload{
		Metadata: domain.ObjectMeta{Name: "lead", Zone: domain.ZoneRuntime},
		Spec: domain.WorkloadSpec{Grants: domain.GrantSet{
			MCPTools:   []string{"weird.tool"},
			DelegateTo: []string{"coder-a", "coder-b"},
		}},
	}
	sandboxLead := lead
	sandboxLead.Metadata.Zone = domain.ZoneSandbox
	unnamed := coder
	unnamed.Metadata.Name = "   "

	cases := []struct {
		name       string
		w          domain.Workload
		capability string
		effect     domain.PolicyEffect
		reason     string
	}{
		{"blank workload name", unnamed, "mcp:repo.read", domain.EffectDeny, "empty_workload_or_capability"},
		{"blank capability", coder, "  ", domain.EffectDeny, "empty_workload_or_capability"},
		{"secret capability", coder, "secret:read", domain.EffectDeny, "forbidden_self_modification_capability"},
		{"rbac capability", coder, "rbac:bind", domain.EffectDeny, "forbidden_self_modification_capability"},
		{"sandbox delegation", sandboxLead, "delegate:coder-a", domain.EffectDeny, "sandbox_zone_forbids_mutation"},
		{"sandbox read stays possible", sandboxLead, "mcp:weird.tool", domain.EffectDeny, "invalid_policy_effect"},
		{"prefix pattern allow", coder, "mcp:repo.read", domain.EffectAllow, "policy_allow"},
		{"granted but no rule", coder, "mcp:other.read", domain.EffectDeny, "default_deny"},
		{"granted action without rule", coder, "action:git.push", domain.EffectDeny, "default_deny"},
		{"ungranted delegation", coder, "delegate:lead", domain.EffectDeny, "capability_not_granted_on_workload"},
		{"unknown capability kind", coder, "shell:rm", domain.EffectDeny, "capability_not_granted_on_workload"},
		{"granted delegation allowed", lead, "delegate:coder-a", domain.EffectAllow, "policy_allow"},
		{"approve wins over allow", lead, "delegate:coder-b", domain.EffectApprove, "policy_requires_approval"},
		{"unknown effect fails closed", lead, "mcp:weird.tool", domain.EffectDeny, "invalid_policy_effect"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := eng.Evaluate(tc.w, tc.capability)
			if d.Effect != tc.effect || d.Reason != tc.reason {
				t.Fatalf("Evaluate(%q, %q) = %+v, want %s/%s", tc.w.Metadata.Name, tc.capability, d, tc.effect, tc.reason)
			}
		})
	}
}

func TestNewEngineCopiesPolicies(t *testing.T) {
	policies := []domain.Policy{{Spec: domain.PolicySpec{Rules: []domain.PolicyRule{
		{Workload: "*", Capability: "*", Effect: domain.EffectAllow},
	}}}}
	eng := NewEngine(policies, nil)
	policies[0] = domain.Policy{Spec: domain.PolicySpec{Rules: []domain.PolicyRule{
		{Workload: "*", Capability: "*", Effect: domain.EffectDeny},
	}}}
	w := domain.Workload{
		Metadata: domain.ObjectMeta{Name: "a"},
		Spec:     domain.WorkloadSpec{Grants: domain.GrantSet{MCPTools: []string{"t"}}},
	}
	if d := eng.Evaluate(w, "mcp:t"); d.Effect != domain.EffectAllow {
		t.Fatalf("caller changed the engine's policies after NewEngine: %+v", d)
	}
}

func TestCircuitBreakerStates(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	cb := NewCircuitBreaker(0, 0)
	cb.now = func() time.Time { return now }
	if cb.threshold != 3 || cb.cooldown != 2*time.Minute {
		t.Fatalf("defaults = %d, %s; want 3, 2m", cb.threshold, cb.cooldown)
	}

	cb.RecordFailure("k")
	cb.RecordFailure("k")
	if cb.IsOpen("k") {
		t.Fatal("open below the threshold")
	}
	cb.RecordFailure("k")
	if !cb.IsOpen("k") {
		t.Fatal("closed at the threshold")
	}
	if cb.IsOpen("other") {
		t.Fatal("a key with no failures is open")
	}

	// A success closes it and forgets the count.
	cb.RecordSuccess("k")
	if cb.IsOpen("k") {
		t.Fatal("open after success")
	}
	cb.RecordFailure("k")
	if cb.IsOpen("k") {
		t.Fatal("success did not reset the failure count")
	}

	// After the cooldown it closes and the count starts again.
	cb.RecordFailure("k")
	cb.RecordFailure("k")
	if !cb.IsOpen("k") {
		t.Fatal("not open after three failures")
	}
	now = now.Add(2*time.Minute - time.Nanosecond)
	if !cb.IsOpen("k") {
		t.Fatal("closed before the cooldown ended")
	}
	now = now.Add(time.Nanosecond)
	if cb.IsOpen("k") {
		t.Fatal("still open when the cooldown ended")
	}
	cb.RecordFailure("k")
	if cb.IsOpen("k") {
		t.Fatal("failure count was kept across the cooldown")
	}
}
