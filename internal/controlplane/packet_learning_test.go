package controlplane

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/shadewright-labs/sablecourt/internal/contextpack"
	"github.com/shadewright-labs/sablecourt/internal/domain"
	"github.com/shadewright-labs/sablecourt/internal/store"
)

// TestLaunchPacketLearningDestination checks that the reconciler hands its
// startup destination to the assembler, so the launch input carries a
// learning only for a local destination.
func TestLaunchPacketLearningDestination(t *testing.T) {
	const advice = "Accepted advice for the launch packet"
	local := domain.ModelGateGrant{Allowed: true, Providers: []string{"local"}}
	for _, tc := range []struct {
		name  string
		dest  contextpack.Destination
		grant domain.ModelGateGrant
		want  bool
	}{
		{"local destination", contextpack.Destination{DockerAgents: true}, local, true},
		{"unknown destination", contextpack.Destination{}, local, false},
		{"model gateway set", contextpack.Destination{DockerAgents: true, ModelGateway: true}, local, false},
		{"remote provider", contextpack.Destination{DockerAgents: true}, domain.ModelGateGrant{Allowed: true, Providers: []string{"anthropic"}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			r, st, _, task := regressionFixture(t)
			r.packetDest = tc.dest
			wl := r.bundle.Workloads["worker"]
			wl.Spec.Class = domain.ClassLongLivedAgent
			wl.Spec.Grants.ModelGate = tc.grant

			raw, err := json.Marshal([]map[string]string{{"name": "record", "text": "observation"}})
			if err != nil {
				t.Fatal(err)
			}
			if err := st.CreateTask(ctx, domain.Task{ID: "evidence", IdempotencyKey: "evidence", WorkloadName: "worker", State: domain.TaskStateCompleted, ArtifactsJSON: string(raw)}); err != nil {
				t.Fatal(err)
			}
			l, err := st.ProposeLearning(ctx, domain.Learning{ID: "advice", Statement: advice, Scope: domain.LearningScope{Global: true}, SourceKind: domain.LearningSourceTask, SourceID: "evidence", EvidenceRef: "artifact:record", Author: "operator-1"})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := st.AcceptLearning(store.WithLearningOperator(ctx, "operator-1"), l.ID, l.VersionDigest, time.Now().UTC()); err != nil {
				t.Fatal(err)
			}

			stub := &fakeAdapter{}
			if err := launchForTest(ctx, r, task, wl, stub); err != nil {
				t.Fatal(err)
			}
			if got := strings.Contains(stub.lastSpec.InputJSON, advice); got != tc.want {
				t.Fatalf("learning in launch input = %v, want %v: %s", got, tc.want, stub.lastSpec.InputJSON)
			}
		})
	}
}
