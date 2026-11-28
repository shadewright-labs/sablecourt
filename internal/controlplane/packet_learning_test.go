package controlplane

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/shadewright-labs/sablecourt/internal/config"
	"github.com/shadewright-labs/sablecourt/internal/contextpack"
	"github.com/shadewright-labs/sablecourt/internal/domain"
	"github.com/shadewright-labs/sablecourt/internal/gate/model"
	"github.com/shadewright-labs/sablecourt/internal/secret"
	"github.com/shadewright-labs/sablecourt/internal/store"
	"github.com/shadewright-labs/sablecourt/internal/store/sqlite"
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

			acceptGlobalLearning(t, st, advice)

			stub := &fakeAdapter{}
			if err := launchForTest(ctx, r, task, wl, stub); err != nil {
				t.Fatal(err)
			}
			if got := strings.Contains(stub.lastSpec.InputJSON, advice); got != tc.want {
				t.Fatalf("learning in launch input = %v, want %v: %s", got, tc.want, stub.lastSpec.InputJSON)
			}
			lease, err := st.GetActiveLeaseByTokenHash(ctx, model.HashToken(stub.lastSpec.TaskToken), time.Now().UTC())
			if err != nil {
				t.Fatal(err)
			}
			if pinned := lease.PinnedProviders != nil; pinned != tc.want {
				t.Fatalf("lease pinned = %v (%v), want %v", pinned, lease.PinnedProviders, tc.want)
			}
		})
	}
}

// acceptGlobalLearning stores an accepted global learning with evidence.
func acceptGlobalLearning(t *testing.T, st *sqlite.SQLiteStore, statement string) {
	t.Helper()
	ctx := t.Context()
	raw, err := json.Marshal([]map[string]string{{"name": "record", "text": "observation"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.CreateTask(ctx, domain.Task{ID: "evidence", IdempotencyKey: "evidence", WorkloadName: "worker", State: domain.TaskStateCompleted, ArtifactsJSON: string(raw)}); err != nil {
		t.Fatal(err)
	}
	l, err := st.ProposeLearning(ctx, domain.Learning{ID: "advice", Statement: statement, Scope: domain.LearningScope{Global: true}, SourceKind: domain.LearningSourceTask, SourceID: "evidence", EvidenceRef: "artifact:record", Author: "operator-1"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.AcceptLearning(store.WithLearningOperator(ctx, "operator-1"), l.ID, l.VersionDigest, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
}

// TestLearningLeaseSurvivesReload admits a packet with learnings on a
// local-only route, then reloads a grant that adds a remote provider. The
// running task's lease stays pinned to local, so the Model Gate refuses its
// remote request, and a task launched after the reload gets no learnings.
func TestLearningLeaseSurvivesReload(t *testing.T) {
	const advice = "Accepted advice kept on this machine"
	ctx := t.Context()
	r, st, _, task := regressionFixture(t)
	r.packetDest = contextpack.Destination{DockerAgents: true}
	acceptGlobalLearning(t, st, advice)

	workload := func(providers ...string) domain.Workload {
		wl := r.bundle.Workloads["worker"]
		wl.Spec.Class = domain.ClassLongLivedAgent
		wl.Spec.Grants.ModelGate = domain.ModelGateGrant{Allowed: true, Providers: providers, BudgetRef: "b1"}
		return wl
	}
	bundle := func(wl domain.Workload) *config.Bundle {
		return &config.Bundle{
			Budgets:   map[string]domain.Budget{"b1": {Metadata: domain.ObjectMeta{Name: "b1"}, Spec: domain.BudgetSpec{PerTask: domain.PerTaskBudget{MaxCostUSD: 5}}}},
			Workloads: map[string]domain.Workload{"worker": wl},
		}
	}

	first := &fakeAdapter{}
	if err := launchForTest(ctx, r, task, workload("local"), first); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(first.lastSpec.InputJSON, advice) {
		t.Fatalf("local-only packet lacks the learning: %s", first.lastSpec.InputJSON)
	}

	widened := workload("local", "openai")
	r.Reload(bundle(widened), r.policy)
	var remote atomic.Bool
	gate := model.New(model.Options{
		Store:   st,
		Secrets: secret.NewMemoryStore(map[string]string{"openai_api_key": "sk-" + "test"}),
		Bundle:  bundle(widened),
		Client: &http.Client{Transport: roundTrip(func(req *http.Request) (*http.Response, error) {
			if strings.Contains(req.URL.Host, "openai.com") {
				remote.Store(true)
			}
			return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"model":"m","usage":{"prompt_tokens":1,"completion_tokens":1}}`))}, nil
		})},
	})
	req := httptest.NewRequest(http.MethodPost, "/openai/v1/chat/completions", strings.NewReader(`{"model":"m"}`))
	req.Header.Set("Authorization", "Bearer "+first.lastSpec.TaskToken)
	rec := httptest.NewRecorder()
	gate.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden || remote.Load() {
		t.Fatalf("remote request of the learning lease: status %d, remote reached %v: %s", rec.Code, remote.Load(), rec.Body.String())
	}

	second, err := r.SubmitTask(ctx, "worker", "after-reload", "{}")
	if err != nil {
		t.Fatal(err)
	}
	next := &fakeAdapter{}
	if err := launchForTest(ctx, r, second, widened, next); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(next.lastSpec.InputJSON, advice) {
		t.Fatalf("task after the reload carries the learning: %s", next.lastSpec.InputJSON)
	}
	lease, err := st.GetActiveLeaseByTokenHash(ctx, model.HashToken(next.lastSpec.TaskToken), time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if lease.PinnedProviders != nil {
		t.Fatalf("lease of a packet without learnings is pinned: %v", lease.PinnedProviders)
	}
	if first, _ := st.GetActiveLeaseByTokenHash(ctx, model.HashToken(first.lastSpec.TaskToken), time.Now().UTC()); !slices.Equal(first.PinnedProviders, []string{"local"}) {
		t.Fatalf("first lease pinned to %v, want [local]", first.PinnedProviders)
	}
}

type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
