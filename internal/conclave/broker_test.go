package conclave

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/shadewright-labs/sablecourt/internal/config"
	"github.com/shadewright-labs/sablecourt/internal/controlplane"
	"github.com/shadewright-labs/sablecourt/internal/domain"
	"github.com/shadewright-labs/sablecourt/internal/policy"
	"github.com/shadewright-labs/sablecourt/internal/store/sqlite"
)

func TestConclaveLifecycleWardsAndActivation(t *testing.T) {
	ctx := context.Background()
	st, err := sqlite.Open(filepath.Join(t.TempDir(), "sablecourt.db"))
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	for _, name := range []string{"coder-wl", "reviewer-wl"} {
		if err := st.UpsertWorkload(ctx, domain.WorkloadRecord{
			Name:  name,
			Zone:  domain.ZoneRuntime,
			Class: domain.ClassLongLivedAgent,
		}); err != nil {
			t.Fatalf("UpsertWorkload %s: %v", name, err)
		}
	}

	bundle := &config.Bundle{
		Workloads: map[string]domain.Workload{
			"coder-wl": {
				Metadata: domain.ObjectMeta{Name: "coder-wl", Zone: domain.ZoneRuntime},
				Spec:     domain.WorkloadSpec{Class: domain.ClassLongLivedAgent, Runtime: domain.RuntimeConfig{Type: "docker", Image: "alpine:3.21"}},
			},
			"reviewer-wl": {
				Metadata: domain.ObjectMeta{Name: "reviewer-wl", Zone: domain.ZoneRuntime},
				Spec:     domain.WorkloadSpec{Class: domain.ClassLongLivedAgent, Runtime: domain.RuntimeConfig{Type: "docker", Image: "alpine:3.21"}},
			},
		},
		Conclaves: map[string]domain.Conclave{
			"feature-delivery": {
				Metadata: domain.ObjectMeta{Name: "feature-delivery"},
				Spec: domain.ConclaveSpec{
					Wards: domain.ConclaveWards{MaxMessages: 10, MaxPingPong: 2},
					Vassals: []domain.ConclaveVassal{
						{Role: "coder", Workload: "coder-wl", Activation: domain.ActivationAlwaysOn, CanMessage: []string{"reviewer", "operator"}},
						{Role: "reviewer", Workload: "reviewer-wl", Activation: domain.ActivationOnMention, CanMessage: []string{"coder", "operator"}},
					},
				},
			},
		},
	}

	rec := controlplane.NewReconciler(controlplane.Options{
		Store:  st,
		Bundle: bundle,
		Policy: policy.NewEngine(nil, policy.NewCircuitBreaker(3, time.Minute)),
	})
	broker := NewBroker(Options{
		Store:      st,
		Reconciler: rec,
		GetBundle:  func() *config.Bundle { return bundle },
	})

	// 1. Open dispatches always-on Vassal (coder) immediately, not on-mention (reviewer).
	sess, err := broker.Open(ctx, "feature-delivery", "Add retry logic", "")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	tasks, err := st.ListTasks(ctx)
	if err != nil {
		t.Fatalf("ListTasks: %v", err)
	}
	if len(tasks) != 1 || tasks[0].WorkloadName != "coder-wl" {
		t.Fatalf("expected 1 always-on task for coder-wl, got %+v", tasks)
	}

	// 2. Coder mentions reviewer -> wakes on-mention Vassal + redacts secret.
	msg1, err := broker.PostMessage(ctx, sess.ID, "coder", "reviewer", "proposal", "Ready for review; token sk-1234567890abcdefghij123456", tasks[0].ID)
	if err != nil {
		t.Fatalf("PostMessage coder->reviewer: %v", err)
	}
	if strings.Contains(msg1.Body, "sk-1234567890") {
		t.Fatalf("expected secret redaction in message body, got %q", msg1.Body)
	}
	tasks, _ = st.ListTasks(ctx)
	if len(tasks) != 2 {
		t.Fatalf("expected on-mention reviewer task to be dispatched, got %d tasks", len(tasks))
	}

	// 3. Reviewer replies to coder (2nd message between same 2 roles).
	if _, err := broker.PostMessage(ctx, sess.ID, "reviewer", "coder", "review", "Please add timeout test", ""); err != nil {
		t.Fatalf("PostMessage reviewer->coder: %v", err)
	}

	// 4. 3rd ping-pong message trips MaxPingPong=2 Ward and pauses Conclave.
	_, err = broker.PostMessage(ctx, sess.ID, "coder", "reviewer", "question", "Which timeout?", "")
	if !errors.Is(err, ErrWardTripped) {
		t.Fatalf("expected ErrWardTripped, got %v", err)
	}
	updated, _ := st.GetConclave(ctx, sess.ID)
	if updated.State != domain.ConclaveStateInputRequired {
		t.Fatalf("expected input-required after ward trip, got %s", updated.State)
	}

	// 5. Operator Decree resumes Conclave and resets ping-pong counter.
	if _, err := broker.PostMessage(ctx, sess.ID, "operator", "all", "decree", "Use 5s timeout and ship it", ""); err != nil {
		t.Fatalf("operator Decree failed: %v", err)
	}
	resumed, _ := st.GetConclave(ctx, sess.ID)
	if resumed.State != domain.ConclaveStateActive {
		t.Fatalf("expected active after operator Decree, got %s", resumed.State)
	}

	// 6. Store and retrieve a redacted Relic.
	relic, err := broker.PutRelic(ctx, sess.ID, "coder", "patch.diff", "text/x-diff", "diff --git\napi_key=supersecretvalue12345")
	if err != nil {
		t.Fatalf("PutRelic: %v", err)
	}
	if strings.Contains(relic.Body, "supersecretvalue12345") {
		t.Fatalf("expected secret redaction in Relic, got %q", relic.Body)
	}
	gotRelic, err := st.GetConclaveRelic(ctx, sess.ID, "patch.diff")
	if err != nil || gotRelic.SHA256 != relic.SHA256 {
		t.Fatalf("GetConclaveRelic mismatch: %v %+v", err, gotRelic)
	}
}
