package controlplane

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/shadewright-labs/sablecourt/internal/domain"
	"github.com/shadewright-labs/sablecourt/internal/store"
)

func TestCouncilCheckoutAtDispatch(t *testing.T) {
	commit := strings.Repeat("a", 40)
	for _, tc := range []struct {
		name     string
		checkout store.CouncilCheckout
		allowed  bool
	}{
		{"working tree", store.CouncilCheckout{}, false},
		{"pinned reader", store.CouncilCheckout{Commit: commit, Source: "project"}, true},
		{"wrong project", store.CouncilCheckout{Commit: commit, Source: "other"}, false},
		{"writer", store.CouncilCheckout{Commit: commit, Source: "project", WriterRole: "reader"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			r, st, _, _ := regressionFixture(t)
			w := r.bundle.Workloads["worker"]
			w.Metadata.Zone = domain.ZoneSandbox
			w.Spec.Class = domain.ClassCodingAgent
			w.Spec.Coding = &domain.CodingConfig{RepoPath: "project"}
			if err := st.UpsertWorkload(ctx, domain.WorkloadRecord{Name: "worker", Zone: w.Metadata.Zone, Class: w.Spec.Class}); err != nil {
				t.Fatal(err)
			}
			spec := store.CouncilSpec{Question: "Review?", Checkout: tc.checkout, Members: []store.CouncilMember{{Vassal: domain.ConclaveVassal{Role: "reader", Workload: "worker"}, Zone: w.Metadata.Zone}}}
			if err := st.CreateCouncil(ctx, domain.ConclaveSession{ID: "room", State: domain.ConclaveStateActive}, spec); err != nil {
				t.Fatal(err)
			}
			a := store.ConclaveAssignment{Round: 1, Attempt: 1, ConclaveID: "room", Role: "reader"}
			input, err := json.Marshal(struct {
				store.ConclaveAssignment
				Packet string `json:"context_packet"`
			}{a, "Review?"})
			if err != nil {
				t.Fatal(err)
			}
			task, err := store.SubmitConclaveTurn(ctx, "room", a.TurnKey(), store.ConclaveLimits{}, func(ctx context.Context) (domain.Task, error) {
				return r.SubmitTask(ctx, "worker", a.TurnKey(), string(input))
			})
			if err != nil {
				t.Fatal(err)
			}
			adapter := &pinnedAdapter{}
			err = launchForTest(ctx, r, task, w, adapter)
			if tc.allowed {
				if err != nil || adapter.pin != commit || !adapter.readOnly {
					t.Fatalf("pinned checkout %q, read-only %t, err %v", adapter.pin, adapter.readOnly, err)
				}
			} else if err == nil || adapter.lastSpec.TaskID != "" {
				t.Fatal("council launched with an unverified checkout")
			}
		})
	}
}
