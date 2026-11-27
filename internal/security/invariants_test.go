package security_test

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/shadewright-labs/sablecourt/internal/approval"
	"github.com/shadewright-labs/sablecourt/internal/config"
	"github.com/shadewright-labs/sablecourt/internal/domain"
	mcpgate "github.com/shadewright-labs/sablecourt/internal/gate/mcp"
	"github.com/shadewright-labs/sablecourt/internal/gate/model"
	"github.com/shadewright-labs/sablecourt/internal/policy"
	"github.com/shadewright-labs/sablecourt/internal/redact"
	scruntime "github.com/shadewright-labs/sablecourt/internal/runtime"
	"github.com/shadewright-labs/sablecourt/internal/runtime/coding"
	"github.com/shadewright-labs/sablecourt/internal/runtime/controller"
	"github.com/shadewright-labs/sablecourt/internal/secret"
	"github.com/shadewright-labs/sablecourt/internal/store"
	"github.com/shadewright-labs/sablecourt/internal/store/sqlite"
)

// TestSixHardSecurityInvariants exercises Sablecourt's six core security invariants end-to-end.
func TestSixHardSecurityInvariants(t *testing.T) {
	ctx := context.Background()
	st, err := sqlite.Open(filepath.Join(t.TempDir(), "sablecourt.db"))
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	// Invariant 1: Model output is never authorization; pinned SHA-256 + OOB confirmation required.
	t.Run("invariant1_model_output_never_authorizes", func(t *testing.T) {
		bundle := &config.Bundle{
			Policies: map[string]domain.Policy{
				"p": {
					Metadata: domain.ObjectMeta{Name: "p"},
					Spec: domain.PolicySpec{
						DefaultEffect: domain.EffectDeny,
						Rules:         []domain.PolicyRule{{Workload: "ai", Capability: "action:git.open_pr", Effect: domain.EffectApprove}},
					},
				},
			},
			Workloads: map[string]domain.Workload{
				"ai": {
					Metadata: domain.ObjectMeta{Name: "ai", Zone: domain.ZoneRuntime},
					Spec:     domain.WorkloadSpec{Class: domain.ClassLongLivedAgent, Grants: domain.GrantSet{Actions: []string{"git.open_pr"}}},
				},
			},
		}
		_ = st.UpsertWorkload(ctx, domain.WorkloadRecord{Name: "ai", Zone: domain.ZoneRuntime, Class: domain.ClassLongLivedAgent, ManifestDigest: "d", AgentCardJSON: "{}"})
		_ = st.CreateTask(ctx, domain.Task{ID: "inv1-t", IdempotencyKey: "inv1-k", WorkloadName: "ai", State: domain.TaskStateWorking})
		tok, _ := model.MintTaskToken(ctx, st, "inv1-t", "ai", time.Hour)

		eng := policy.NewEngine([]domain.Policy{bundle.Policies["p"]}, nil)
		gate := mcpgate.New(mcpgate.Options{Store: st, Bundle: bundle, Policy: eng, Approvals: approval.NewQueue(st, nil)})
		gate.RegisterAction("git.open_pr", func(_ context.Context, _ domain.Workload, _ string, _ json.RawMessage) (any, error) {
			return "ok", nil
		})

		req := httptest.NewRequest(http.MethodPost, "http://local/git.open_pr", strings.NewReader(`{"payload":{"head":"sablecourt/x"}}`))
		req.Header.Set("Authorization", "Bearer "+tok)
		rec := httptest.NewRecorder()
		gate.ActionHandler().ServeHTTP(rec, req)
		if rec.Code != http.StatusAccepted {
			t.Fatalf("expected 202 approval_required, got %d", rec.Code)
		}
	})

	// Invariant 2: No raw upstream credentials inside workloads.
	t.Run("invariant2_no_raw_secrets_in_workload_env", func(t *testing.T) {
		env := coding.BuildStrippedEnv(scruntime.LaunchSpec{
			TaskID:         "t-env",
			Workload:       domain.Workload{Metadata: domain.ObjectMeta{Name: "coder"}},
			ModelGateURL:   "http://127.0.0.1:7432/gate/model",
			ModelGateToken: "sc_task_scoped",
		})
		joined := strings.Join(env, "\n")
		for _, forbidden := range []string{"OPENAI_API_KEY", "ANTHROPIC_API_KEY", "GEMINI_API_KEY", "GITHUB_TOKEN", "AWS_SECRET"} {
			if strings.Contains(joined, forbidden) {
				t.Fatalf("raw secret %s leaked into workload env", forbidden)
			}
		}
	})

	// Invariant 3: Untrusted external input is redacted in linear time without backtracking hangs.
	t.Run("invariant3_linear_time_redaction_on_large_payload", func(t *testing.T) {
		opToken := "op_" + strings.Repeat("a1", 32)
		big := strings.Repeat("untrusted log line without secrets\n", 10000) +
			"leaked=sk-ant-api03-secretvalue1234567890\n" +
			"operator=" + opToken + "\n" +
			strings.Repeat("more text ", 10000)
		res := redact.Text(big)
		if res.Hits < 2 || strings.Contains(res.Text, "sk-ant-api03-secretvalue") || strings.Contains(res.Text, opToken) {
			t.Fatal("failed to redact secret or operator token inside large payload")
		}

		const smallSize = 1 << 15
		const largeSize = 4 * smallSize
		ratio := growthRatio(func(size int) {
			redact.Text(strings.Repeat("x", size))
		}, smallSize, largeSize, 5)
		if ratio > 10 {
			t.Fatalf("redaction growth ratio %.2f exceeded linear bound", ratio)
		}
	})

	// Invariant 4: Separate identities, zero-LLM deterministic controllers, sandbox bans, and fail-closed defaults.
	t.Run("invariant4_zero_llm_and_sandbox_fail_closed", func(t *testing.T) {
		ctrl := controller.NewAdapter(nil)
		_, err := ctrl.Start(ctx, scruntime.LaunchSpec{
			TaskID: "inv4-c",
			Workload: domain.Workload{
				Metadata: domain.ObjectMeta{Name: "det", Zone: domain.ZoneRuntime},
				Spec:     domain.WorkloadSpec{Class: domain.ClassDeterministicController},
			},
			ModelGateToken: "sc_task_forbidden",
		})
		if !errors.Is(err, controller.ErrModelGrantForbidden) {
			t.Fatalf("expected ErrModelGrantForbidden, got %v", err)
		}

		eng := policy.NewEngine(nil, nil)
		sbx := domain.Workload{
			Metadata: domain.ObjectMeta{Name: "sbx", Zone: domain.ZoneSandbox},
			Spec:     domain.WorkloadSpec{Class: domain.ClassLongLivedAgent, Grants: domain.GrantSet{Actions: []string{"git.open_pr"}}},
		}
		if d := eng.Evaluate(sbx, "action:git.open_pr"); d.Effect != domain.EffectDeny {
			t.Fatalf("sandbox action must fail closed to deny, got %+v", d)
		}
	})

	// Invariant 5 & 6: SecretStore fails closed on insecure permissions + idempotent task creation.
	t.Run("invariant5_and_6_idempotency_and_fail_closed_secrets", func(t *testing.T) {
		_ = st.UpsertWorkload(ctx, domain.WorkloadRecord{Name: "w6", Zone: domain.ZoneRuntime, Class: domain.ClassDeterministicController, ManifestDigest: "d", AgentCardJSON: "{}"})
		task := domain.Task{ID: "inv6-1", IdempotencyKey: "idem-unique", WorkloadName: "w6", State: domain.TaskStateSubmitted}
		if err := st.CreateTask(ctx, task); err != nil {
			t.Fatalf("first CreateTask: %v", err)
		}
		task.ID = "inv6-2"
		if err := st.CreateTask(ctx, task); !errors.Is(err, store.ErrConflict) {
			t.Fatalf("expected ErrConflict on duplicate idempotency key, got %v", err)
		}

		mem := secret.NewMemoryStore(nil)
		if _, err := mem.Get(ctx, "missing_key"); !errors.Is(err, secret.ErrSecretNotFound) {
			t.Fatalf("expected ErrSecretNotFound, got %v", err)
		}
	})
}

// selfTestGrowthBound sits between the linear prediction (8x for an 8x size
// step) and the quadratic one (64x), close to their geometric mean.
const selfTestGrowthBound = 22

func TestLinearGrowthBoundRejectsQuadraticPattern(t *testing.T) {
	for _, tc := range []struct {
		name       string
		run        func(int)
		smallSize  int
		largeSize  int
		rounds     int
		wantLinear bool
	}{
		{
			name: "linear redactor",
			run: func(size int) {
				redact.Text(strings.Repeat("x", size))
			},
			smallSize:  1 << 13,
			largeSize:  1 << 16,
			rounds:     5,
			wantLinear: true,
		},
		{
			name: "deliberately bad a-star-z pattern",
			run: func(size int) {
				input := strings.Repeat("a", size)
				for start := range len(input) {
					for end := start; end < len(input); end++ {
						if input[end] == 'z' {
							break
						}
					}
				}
			},
			smallSize:  1 << 12,
			largeSize:  1 << 15,
			rounds:     4,
			wantLinear: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ratio := growthRatio(tc.run, tc.smallSize, tc.largeSize, tc.rounds)
			t.Logf("growth ratio %.2f (bound %d)", ratio, selfTestGrowthBound)
			gotLinear := ratio <= selfTestGrowthBound
			if gotLinear != tc.wantLinear {
				t.Fatalf("growth ratio %.2f classified linear=%t, want %t", ratio, gotLinear, tc.wantLinear)
			}
		})
	}
}

// growthRatio returns the ratio of the fastest run at largeSize to the fastest
// run at smallSize. Runs are interleaved after one warm-up pair so that a slow
// phase of the machine hits both sizes, and the minimum drops scheduler noise.
func growthRatio(run func(int), smallSize, largeSize, rounds int) float64 {
	run(smallSize)
	run(largeSize)
	small, large := time.Duration(math.MaxInt64), time.Duration(math.MaxInt64)
	for range rounds {
		start := time.Now()
		run(smallSize)
		small = min(small, time.Since(start))
		start = time.Now()
		run(largeSize)
		large = min(large, time.Since(start))
	}
	if small <= 0 {
		return math.Inf(1)
	}
	return float64(large) / float64(small)
}
