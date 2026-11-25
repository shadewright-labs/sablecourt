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
	// Probe is true when this call is the one call a breaker admits after its
	// cool-down. The caller records its outcome (RecordOutcome) or, when the
	// call ends without one, calls ReleaseProbe.
	Probe bool
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

// BreakerKey is the circuit breaker key for one workload using one
// capability. Breakers are kept per workload so a workload whose calls keep
// failing stops only itself, not every workload granted the capability.
func BreakerKey(capability, workload string) string {
	return capability + "\x00" + workload
}

// RecordOutcome records the result of workload running capability on the
// engine's circuit breaker, the same one Evaluate checks. Only failures of the
// downstream system should be recorded as failed; a refusal of the caller's
// own input is not a failure of the capability. It does nothing without a breaker.
func (e *Engine) RecordOutcome(capability, workload string, failed bool) {
	if e == nil || e.breaker == nil {
		return
	}
	key := BreakerKey(capability, workload)
	if failed {
		e.breaker.RecordFailure(key)
		return
	}
	e.breaker.RecordSuccess(key)
}

// ReleaseProbe gives back the breaker's probe slot taken by a call that ended
// without an outcome to record.
func (e *Engine) ReleaseProbe(capability, workload string) {
	if e == nil || e.breaker == nil {
		return
	}
	e.breaker.Release(BreakerKey(capability, workload))
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

	// Circuit breaker fails closed for mutating actions when tripped. After
	// its cool-down one call is admitted as the probe; the gate records its
	// outcome or releases the slot (ReleaseProbe).
	probe := false
	if strings.HasPrefix(capability, "action:") && e.breaker != nil {
		ok, isProbe := e.breaker.Admit(BreakerKey(capability, w.Metadata.Name))
		if !ok {
			return Decision{Effect: domain.EffectDeny, Reason: "circuit_breaker_open"}
		}
		probe = isProbe
	}
	release := func(d Decision) Decision {
		if probe && d.Effect == domain.EffectDeny {
			e.breaker.Release(BreakerKey(capability, w.Metadata.Name))
		} else {
			d.Probe = probe
		}
		return d
	}

	var matchedAllow, matchedApprove bool
	for _, p := range e.policies {
		for _, r := range p.Spec.Rules {
			if !matchPattern(r.Workload, w.Metadata.Name) || !matchPattern(r.Capability, capability) {
				continue
			}
			switch r.Effect {
			case domain.EffectDeny:
				return release(Decision{Effect: domain.EffectDeny, Reason: "explicit_policy_deny"})
			case domain.EffectApprove:
				matchedApprove = true
			case domain.EffectAllow:
				matchedAllow = true
			default:
				return release(Decision{Effect: domain.EffectDeny, Reason: "invalid_policy_effect"})
			}
		}
	}

	if matchedApprove {
		return release(Decision{Effect: domain.EffectApprove, Reason: "policy_requires_approval"})
	}
	if matchedAllow {
		return release(Decision{Effect: domain.EffectAllow, Reason: "policy_allow"})
	}
	return release(Decision{Effect: domain.EffectDeny, Reason: "default_deny"})
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

// maxBreakerKeys caps the breaker's map. The keys are capability and workload
// pairs; Prune on reload keeps them to the loaded workloads, and the cap is a
// backstop for a manifest set that churns between reloads.
const maxBreakerKeys = 1024

// CircuitBreaker tracks consecutive failures per key (BreakerKey) and fails
// closed when tripped. After the cool-down it admits one probe call
// (half-open); the probe's outcome closes the breaker or opens it again.
type CircuitBreaker struct {
	mu        sync.Mutex
	threshold int
	cooldown  time.Duration
	now       func() time.Time
	entries   map[string]*breakerEntry
}

type breakerEntry struct {
	failures  int
	openUntil time.Time // zero while closed
	probing   bool      // a probe was admitted after the cool-down
	probeAt   time.Time
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
		entries:   make(map[string]*breakerEntry),
	}
}

func (cb *CircuitBreaker) RecordFailure(key string) {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	e, ok := cb.entries[key]
	if !ok {
		cb.makeRoom()
		e = &breakerEntry{}
		cb.entries[key] = e
	}
	e.failures++
	if e.probing || e.failures >= cb.threshold {
		e.openUntil = cb.now().Add(cb.cooldown)
		e.probing = false
	}
}

// RecordSuccess resets the key. It does not close an open breaker before its
// cool-down ends: a success that was in flight when the breaker opened says
// nothing about the calls that tripped it. After the cool-down, the success
// of the probe closes the breaker.
func (cb *CircuitBreaker) RecordSuccess(key string) {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	e, ok := cb.entries[key]
	if !ok {
		return
	}
	if !e.openUntil.IsZero() && cb.now().Before(e.openUntil) {
		return
	}
	delete(cb.entries, key)
}

// IsOpen reports whether a call for key would be refused now, without
// admitting one.
func (cb *CircuitBreaker) IsOpen(key string) bool {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	e, ok := cb.entries[key]
	if !ok || e.openUntil.IsZero() {
		return false
	}
	now := cb.now()
	return now.Before(e.openUntil) || e.probing && now.Before(e.probeAt.Add(cb.cooldown))
}

// Admit decides whether a call for key may run. A closed breaker admits every
// call. An open one admits none until its cool-down ends, then exactly one
// probe (probe is true) until that probe records an outcome or is released.
// A probe that never reports gives its slot up after another cool-down.
func (cb *CircuitBreaker) Admit(key string) (ok, probe bool) {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	e, found := cb.entries[key]
	if !found || e.openUntil.IsZero() {
		return true, false
	}
	now := cb.now()
	if now.Before(e.openUntil) {
		return false, false
	}
	if e.probing && now.Before(e.probeAt.Add(cb.cooldown)) {
		return false, false
	}
	e.probing, e.probeAt = true, now
	return true, true
}

// Release gives back the probe slot of a call that ended without an outcome
// for the breaker (refused input, an approval request), so the next call
// becomes the probe.
func (cb *CircuitBreaker) Release(key string) {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	if e, ok := cb.entries[key]; ok {
		e.probing = false
	}
}

// Prune drops the entries of workloads keep rejects. The daemon calls it on
// every manifest load with the loaded workloads.
func (cb *CircuitBreaker) Prune(keep func(workload string) bool) {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	for key := range cb.entries {
		_, workload, _ := strings.Cut(key, "\x00")
		if !keep(workload) {
			delete(cb.entries, key)
		}
	}
}

// Tracked returns the number of keys with state.
func (cb *CircuitBreaker) Tracked() int {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	return len(cb.entries)
}

// makeRoom keeps the map under maxBreakerKeys before a new key is added. It
// drops closed entries first (a failure count below the threshold), then open
// entries whose cool-down has ended, and only then the open entry closest to
// the end of its cool-down. Called with cb.mu held.
func (cb *CircuitBreaker) makeRoom() {
	if len(cb.entries) < maxBreakerKeys {
		return
	}
	now := cb.now()
	for key, e := range cb.entries {
		if e.openUntil.IsZero() {
			delete(cb.entries, key)
		}
	}
	for key, e := range cb.entries {
		if len(cb.entries) < maxBreakerKeys {
			return
		}
		if !now.Before(e.openUntil) && !e.probing {
			delete(cb.entries, key)
		}
	}
	for len(cb.entries) >= maxBreakerKeys {
		var oldest string
		for key, e := range cb.entries {
			if oldest == "" || e.openUntil.Before(cb.entries[oldest].openUntil) {
				oldest = key
			}
		}
		delete(cb.entries, oldest)
	}
}
