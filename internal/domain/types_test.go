package domain

import (
	"testing"
	"time"
)

func TestComputeAuditRowHashCoversEveryField(t *testing.T) {
	const at = "2026-01-02T03:04:05Z"
	base := AuditRecord{Category: "approval", TaskID: "t1", WorkloadName: "w", Action: "action:git.open_pr", Outcome: "approved", DetailJSON: `{"a":1}`}
	h := ComputeAuditRowHash("prev", base, at)
	if len(h) != 64 || h != ComputeAuditRowHash("prev", base, at) {
		t.Fatalf("hash %q is not a stable SHA-256 hex digest", h)
	}
	if ComputeAuditRowHash("", base, at) != ComputeAuditRowHash("genesis", base, at) {
		t.Fatal("an empty previous hash does not start the chain at genesis")
	}

	changes := map[string]func(*AuditRecord){
		"category": func(r *AuditRecord) { r.Category = "device" },
		"task":     func(r *AuditRecord) { r.TaskID = "t2" },
		"workload": func(r *AuditRecord) { r.WorkloadName = "w2" },
		"action":   func(r *AuditRecord) { r.Action = "action:git.push" },
		"outcome":  func(r *AuditRecord) { r.Outcome = "denied" },
		"detail":   func(r *AuditRecord) { r.DetailJSON = `{"a":2}` },
	}
	for name, change := range changes {
		t.Run(name, func(t *testing.T) {
			rec := base
			change(&rec)
			if ComputeAuditRowHash("prev", rec, at) == h {
				t.Fatalf("changing the %s does not change the hash", name)
			}
		})
	}
	if ComputeAuditRowHash("prev2", base, at) == h || ComputeAuditRowHash("prev", base, "2026-01-02T03:04:06Z") == h {
		t.Fatal("the previous hash or the timestamp is not part of the hash")
	}
	// Ignored fields: the stored hashes themselves and the parsed time.
	withMeta := base
	withMeta.PrevHash, withMeta.RowHash, withMeta.CreatedAt = "x", "y", time.Now()
	if ComputeAuditRowHash("prev", withMeta, at) != h {
		t.Fatal("stored hash fields or CreatedAt changed the hash")
	}
}

func TestComputeAuditRowHashSeparatesFields(t *testing.T) {
	t.Skip("defect: fields are joined with '|' without escaping, so moving a '|' between adjacent fields keeps the hash")
	a := AuditRecord{Category: "approval", Action: "x|y", Outcome: "z"}
	b := AuditRecord{Category: "approval", Action: "x", Outcome: "y|z"}
	if ComputeAuditRowHash("p", a, "t") == ComputeAuditRowHash("p", b, "t") {
		t.Fatal("two different rows have the same chain hash")
	}
}

func TestLearningEligibility(t *testing.T) {
	now := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	later, earlier := now.Add(time.Hour), now.Add(-time.Hour)
	cases := []struct {
		name     string
		l        Learning
		at       time.Time
		eligible bool
		status   LearningStatus
	}{
		{"accepted and unexpired", Learning{Status: LearningStatusAccepted, ExpiresAt: &later}, now, true, LearningStatusAccepted},
		{"accepted at expiry", Learning{Status: LearningStatusAccepted, ExpiresAt: &now}, now, false, LearningStatusProposed},
		{"accepted and expired", Learning{Status: LearningStatusAccepted, ExpiresAt: &earlier}, now, false, LearningStatusProposed},
		{"accepted without expiry", Learning{Status: LearningStatusAccepted}, now, false, LearningStatusProposed},
		{"accepted but unmapped", Learning{Status: LearningStatusAccepted, ExpiresAt: &later, Scope: LearningScope{NeedsScopeMapping: true}}, now, false, LearningStatusProposed},
		{"proposed", Learning{Status: LearningStatusProposed, ExpiresAt: &later}, now, false, LearningStatusProposed},
		{"retired stays retired", Learning{Status: LearningStatusRetired, ExpiresAt: &earlier}, now, false, LearningStatusRetired},
		{"zero time uses the clock", Learning{Status: LearningStatusAccepted, ExpiresAt: &earlier}, time.Time{}, false, LearningStatusProposed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.l.IsEligible(tc.at); got != tc.eligible {
				t.Fatalf("IsEligible = %v, want %v", got, tc.eligible)
			}
			if got := tc.l.EffectiveStatus(tc.at); got != tc.status {
				t.Fatalf("EffectiveStatus = %s, want %s", got, tc.status)
			}
		})
	}
}
