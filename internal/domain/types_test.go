package domain

import (
	"testing"
	"time"
)

func TestComputeAuditRowHashCoversEveryField(t *testing.T) {
	const at = "2026-01-02T03:04:05Z"
	base := AuditRecord{ID: 7, Seq: 3, Category: "approval", TaskID: "t1", WorkloadName: "w", Action: "action:git.open_pr", Outcome: "approved", DetailJSON: `{"a":1}`}
	h := ComputeAuditRowHash("prev", base, at)
	if len(h) != 64 || h != ComputeAuditRowHash("prev", base, at) {
		t.Fatalf("hash %q is not a stable SHA-256 hex digest", h)
	}

	changes := map[string]func(*AuditRecord){
		"id":       func(r *AuditRecord) { r.ID = 8 },
		"sequence": func(r *AuditRecord) { r.Seq = 4 },
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
	if ComputeAuditRowHash("", base, at) == ComputeAuditRowHash(AuditGenesis, base, at) {
		t.Fatal("an empty previous hash is treated as genesis")
	}
	if ComputeAuditRowHashV1("prev", base, at) == h {
		t.Fatal("version 1 and version 2 hashes agree")
	}
	// Ignored fields: the stored hashes, the hash version and the parsed time.
	withMeta := base
	withMeta.PrevHash, withMeta.RowHash, withMeta.HashVersion, withMeta.CreatedAt = "x", "y", 9, time.Now()
	if ComputeAuditRowHash("prev", withMeta, at) != h {
		t.Fatal("stored hash fields, HashVersion or CreatedAt changed the hash")
	}
}

// Moving bytes between adjacent fields, or between the number fields, must change
// the hash. Version 1 is kept only to verify older rows and fails these cases.
func TestComputeAuditRowHashSeparatesFields(t *testing.T) {
	cases := []struct {
		name string
		a, b AuditRecord
		atA  string
		atB  string
	}{
		{"pipe between action and outcome", AuditRecord{Category: "approval", Action: "x|y", Outcome: "z"}, AuditRecord{Category: "approval", Action: "x", Outcome: "y|z"}, "t", "t"},
		{"pipe between category and task", AuditRecord{Category: "a|b", TaskID: "c"}, AuditRecord{Category: "a", TaskID: "b|c"}, "t", "t"},
		{"bytes between outcome and detail", AuditRecord{Outcome: "ok", DetailJSON: "{}"}, AuditRecord{Outcome: "ok{", DetailJSON: "}"}, "t", "t"},
		{"bytes between detail and time", AuditRecord{DetailJSON: "{}"}, AuditRecord{DetailJSON: "{}|t"}, "t|x", "x"},
		{"digits between id and sequence", AuditRecord{ID: 12, Seq: 3}, AuditRecord{ID: 1, Seq: 23}, "t", "t"},
		{"empty field moved", AuditRecord{TaskID: "", WorkloadName: "w"}, AuditRecord{TaskID: "w", WorkloadName: ""}, "t", "t"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if ComputeAuditRowHash("p", tc.a, tc.atA) == ComputeAuditRowHash("p", tc.b, tc.atB) {
				t.Fatal("two different rows have the same chain hash")
			}
		})
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
