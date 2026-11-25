package policy

import (
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/shadewright-labs/sablecourt/internal/domain"
)

type Decision struct {
	Effect domain.PolicyEffect
	Reason string
}

type Engine struct {
	policies []domain.Policy
	breaker  *CircuitBreaker
}

func NewEngine(policies []domain.Policy, breaker *CircuitBreaker) *Engine {
	cp := make([]domain.Policy, len(policies))
	copy(cp, policies)
	return &Engine{
		policies: cp,
		breaker:  breaker,
	}
}

// RecordOutcome records the result of running capability on the engine's
// circuit breaker, the same one Evaluate checks. It does nothing without a breaker.
func (e *Engine) RecordOutcome(capability string, failed bool) {
	if e == nil || e.breaker == nil {
		return
	}
	if failed {
		e.breaker.RecordFailure(capability)
		return
	}
	e.breaker.RecordSuccess(capability)
}

// Evaluate determines allow, approve, or deny deterministically.
// Defaults and errors always fail closed to deny.
func (e *Engine) Evaluate(w domain.Workload, capability string) Decision {
	if strings.TrimSpace(w.Metadata.Name) == "" || strings.TrimSpace(capability) == "" {
		return Decision{Effect: domain.EffectDeny, Reason: "empty_workload_or_capability"}
	}

	// Hard invariant: no workload may modify policies, secrets, or RBAC.
	for _, forbidden := range []string{"policy:", "secret:", "rbac:"} {
		if strings.HasPrefix(capability, forbidden) {
			return Decision{Effect: domain.EffectDeny, Reason: "forbidden_self_modification_capability"}
		}
	}

	// Hard invariant: sandbox workloads cannot perform mutating actions or delegation.
	if w.Metadata.Zone == domain.ZoneSandbox {
		if strings.HasPrefix(capability, "action:") || strings.HasPrefix(capability, "delegate:") {
			return Decision{Effect: domain.EffectDeny, Reason: "sandbox_zone_forbids_mutation"}
		}
	}

	// Hard invariant: workload must hold the declared capability grant.
	if !workloadHasGrant(w, capability) {
		return Decision{Effect: domain.EffectDeny, Reason: "capability_not_granted_on_workload"}
	}

	// Circuit breaker fails closed for mutating actions when tripped.
	if strings.HasPrefix(capability, "action:") && e.breaker != nil && e.breaker.IsOpen(capability) {
		return Decision{Effect: domain.EffectDeny, Reason: "circuit_breaker_open"}
	}

	var matchedAllow, matchedApprove bool
	for _, p := range e.policies {
		for _, r := range p.Spec.Rules {
			if !matchPattern(r.Workload, w.Metadata.Name) || !matchPattern(r.Capability, capability) {
				continue
			}
			switch r.Effect {
			case domain.EffectDeny:
				return Decision{Effect: domain.EffectDeny, Reason: "explicit_policy_deny"}
			case domain.EffectApprove:
				matchedApprove = true
			case domain.EffectAllow:
				matchedAllow = true
			default:
				return Decision{Effect: domain.EffectDeny, Reason: "invalid_policy_effect"}
			}
		}
	}

	if matchedApprove {
		return Decision{Effect: domain.EffectApprove, Reason: "policy_requires_approval"}
	}
	if matchedAllow {
		return Decision{Effect: domain.EffectAllow, Reason: "policy_allow"}
	}
	return Decision{Effect: domain.EffectDeny, Reason: "default_deny"}
}

func workloadHasGrant(w domain.Workload, capability string) bool {
	switch {
	case strings.HasPrefix(capability, "mcp:"):
		tool := strings.TrimPrefix(capability, "mcp:")
		return slices.Contains(w.Spec.Grants.MCPTools, tool)
	case strings.HasPrefix(capability, "action:"):
		act := strings.TrimPrefix(capability, "action:")
		return slices.Contains(w.Spec.Grants.Actions, act)
	case strings.HasPrefix(capability, "delegate:"):
		target := strings.TrimPrefix(capability, "delegate:")
		return slices.Contains(w.Spec.Grants.DelegateTo, target)
	default:
		return false
	}
}

func matchPattern(pattern, value string) bool {
	if pattern == "*" || pattern == value {
		return true
	}
	if strings.HasSuffix(pattern, "*") {
		prefix := strings.TrimSuffix(pattern, "*")
		return strings.HasPrefix(value, prefix)
	}
	return false
}

// CircuitBreaker tracks consecutive failures per capability and fails closed when tripped.
type CircuitBreaker struct {
	mu        sync.Mutex
	threshold int
	cooldown  time.Duration
	now       func() time.Time
	failures  map[string]int
	openUntil map[string]time.Time
}

func NewCircuitBreaker(threshold int, cooldown time.Duration) *CircuitBreaker {
	if threshold <= 0 {
		threshold = 3
	}
	if cooldown <= 0 {
		cooldown = 2 * time.Minute
	}
	return &CircuitBreaker{
		threshold: threshold,
		cooldown:  cooldown,
		now:       func() time.Time { return time.Now().UTC() },
		failures:  make(map[string]int),
		openUntil: make(map[string]time.Time),
	}
}

func (cb *CircuitBreaker) RecordFailure(key string) {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	cb.failures[key]++
	if cb.failures[key] >= cb.threshold {
		cb.openUntil[key] = cb.now().Add(cb.cooldown)
	}
}

func (cb *CircuitBreaker) RecordSuccess(key string) {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	delete(cb.failures, key)
	delete(cb.openUntil, key)
}

func (cb *CircuitBreaker) IsOpen(key string) bool {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	until, ok := cb.openUntil[key]
	if !ok {
		return false
	}
	if cb.now().Before(until) {
		return true
	}
	delete(cb.openUntil, key)
	delete(cb.failures, key)
	return false
}
