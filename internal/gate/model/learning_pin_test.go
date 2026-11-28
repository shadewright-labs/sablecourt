package model

import (
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/shadewright-labs/sablecourt/internal/config"
	"github.com/shadewright-labs/sablecourt/internal/domain"
	"github.com/shadewright-labs/sablecourt/internal/secret"
	"github.com/shadewright-labs/sablecourt/internal/store/sqlite"
)

// TestLearningLeaseKeepsAdmittedProviders checks ADR-0012 across a reload: a
// lease whose packet carried learnings reaches only the providers its packet
// was admitted under, even after the workload grant widens, and a local
// failure never falls back to a provider added later.
func TestLearningLeaseKeepsAdmittedProviders(t *testing.T) {
	for _, tc := range []struct {
		name       string
		pinned     []string
		reloaded   []string
		path       string
		localFails bool
		wantCode   int
		wantRemote bool
	}{
		{"pinned lease to an added remote provider", []string{"local"}, []string{"local", "openai"}, "/openai/v1/chat/completions", false, http.StatusForbidden, false},
		{"pinned lease when the grant allows every provider", []string{"local"}, nil, "/openai/v1/chat/completions", false, http.StatusForbidden, false},
		{"pinned lease to its admitted provider", []string{"local"}, []string{"local", "openai"}, "/local/v1/chat/completions", false, http.StatusOK, false},
		{"pinned lease never falls back to an added provider", []string{"local"}, []string{"local", "openai"}, "/local/v1/chat/completions", true, http.StatusServiceUnavailable, false},
		{"empty pin refuses every provider", []string{}, []string{"local", "openai"}, "/local/v1/chat/completions", false, http.StatusForbidden, false},
		{"lease without learnings follows the current grant", nil, []string{"local", "openai"}, "/openai/v1/chat/completions", false, http.StatusOK, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			st, err := sqlite.Open(filepath.Join(t.TempDir(), "mg.db"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = st.Close() })
			if err := st.Migrate(ctx); err != nil {
				t.Fatal(err)
			}
			now := time.Now().UTC()
			_ = st.UpsertWorkload(ctx, domain.WorkloadRecord{Name: "agent-1", Zone: domain.ZoneRuntime, Class: domain.ClassLongLivedAgent})
			_ = st.CreateTask(ctx, domain.Task{ID: "t-pin", WorkloadName: "agent-1", State: domain.TaskStateWorking, CreatedAt: now, UpdatedAt: now})
			tok, err := MintPinnedTaskTokenUntil(ctx, st, "t-pin", "agent-1", now, now.Add(time.Hour), tc.pinned)
			if err != nil {
				t.Fatal(err)
			}

			bundle := func(providers []string) *config.Bundle {
				return &config.Bundle{
					Budgets: map[string]domain.Budget{
						"b1": {Metadata: domain.ObjectMeta{Name: "b1"}, Spec: domain.BudgetSpec{PerTask: domain.PerTaskBudget{MaxCostUSD: 5}}},
					},
					Workloads: map[string]domain.Workload{"agent-1": {
						Metadata: domain.ObjectMeta{Name: "agent-1", Zone: domain.ZoneRuntime},
						Spec: domain.WorkloadSpec{Class: domain.ClassLongLivedAgent, Grants: domain.GrantSet{
							ModelGate: domain.ModelGateGrant{Allowed: true, Providers: providers, BudgetRef: "b1"},
						}},
					}},
				}
			}
			var remote atomic.Bool
			cli := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				if strings.Contains(r.URL.Host, "openai.com") {
					remote.Store(true)
				} else if tc.localFails {
					return &http.Response{StatusCode: http.StatusServiceUnavailable, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"error":"down"}`))}, nil
				}
				return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"model":"m","usage":{"prompt_tokens":1,"completion_tokens":1}}`))}, nil
			})}
			gate := New(Options{
				Store:   st,
				Secrets: secret.NewMemoryStore(map[string]string{"openai_api_key": "sk-" + "test-key"}),
				Bundle:  bundle([]string{"local"}),
				Client:  cli,
			})
			gate.SetBundle(bundle(tc.reloaded))

			req := httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(`{"model":"m"}`))
			req.Header.Set("Authorization", "Bearer "+tok)
			rec := httptest.NewRecorder()
			gate.Handler().ServeHTTP(rec, req)
			if rec.Code != tc.wantCode {
				t.Fatalf("status %d, want %d: %s", rec.Code, tc.wantCode, rec.Body.String())
			}
			if got := rec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
				t.Fatalf("X-Content-Type-Options = %q, want nosniff", got)
			}
			if remote.Load() != tc.wantRemote {
				t.Fatalf("remote provider reached = %v, want %v", remote.Load(), tc.wantRemote)
			}
		})
	}
}
