package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/shadewright-labs/sablecourt/internal/domain"
	mcpgate "github.com/shadewright-labs/sablecourt/internal/gate/mcp"
	"github.com/shadewright-labs/sablecourt/internal/gate/model"
	"github.com/shadewright-labs/sablecourt/internal/store/sqlite"
)

type failingOpener struct{ calls int }

func (o *failingOpener) OpenPR(context.Context, mcpgate.OpenPRInput) (string, error) {
	o.calls++
	return "", errors.New("upstream refused the pull request")
}

// The daemon has one circuit breaker. The gate counts failed git.open_pr
// calls on it, and the policy engine of every load checks it, so three
// failures deny the fourth call with circuit_breaker_open, also when a reload
// built a new engine between them (issue 107 row 13, issue 179).
func TestDaemonSharesOneBreakerAcrossGateAndReloads(t *testing.T) {
	cases := []struct {
		name        string
		reloadAfter int // reload the engine after this many failed calls; 0 never
	}{
		{name: "no reload"},
		{name: "reload after the first failure", reloadAfter: 1},
		{name: "reload after the third failure", reloadAfter: 3},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			dir := t.TempDir()
			manifests := filepath.Join(dir, "sablecourt.yaml")
			if err := os.WriteFile(manifests, []byte(pinManifest), 0o600); err != nil {
				t.Fatal(err)
			}
			repo := filepath.Join(dir, "repo")
			if err := os.MkdirAll(repo, 0o700); err != nil {
				t.Fatal(err)
			}
			for _, args := range [][]string{{"init", "-q"}, {"remote", "add", "origin", "https://github.com/acme/widgets.git"}} {
				if out, err := exec.Command("git", append([]string{"-C", repo}, args...)...).CombinedOutput(); err != nil {
					t.Fatalf("git %v: %v (%s)", args, err, out)
				}
			}
			st, err := sqlite.Open(filepath.Join(dir, "sablecourt.db"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = st.Close() })
			if err := st.Migrate(ctx); err != nil {
				t.Fatal(err)
			}
			loader := newPolicyLoader(manifests)
			bundle, pol, err := loader.Load(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if err := recordWorkloads(ctx, st, bundle, false); err != nil {
				t.Fatal(err)
			}
			if err := st.CreateTask(ctx, domain.Task{ID: "task-b", IdempotencyKey: "b", WorkloadName: "pr-agent", State: domain.TaskStateWorking}); err != nil {
				t.Fatal(err)
			}
			token, err := model.MintTaskToken(ctx, st, "task-b", "pr-agent", time.Hour)
			if err != nil {
				t.Fatal(err)
			}
			gate := mcpgate.New(mcpgate.Options{Store: st, Bundle: bundle, Policy: pol, Breaker: loader.Breaker()})
			opener := &failingOpener{}
			mcpgate.RegisterBuiltinGitOps(gate, repo, opener)

			openPR := func() *httptest.ResponseRecorder {
				body := `{"payload":{"owner":"acme","repo":"widgets","head":"sablecourt/x","base":"main","title":"T"}}`
				req := httptest.NewRequest(http.MethodPost, "http://sablecourt.local/git.open_pr", strings.NewReader(body))
				req.Header.Set("Authorization", "Bearer "+token)
				rec := httptest.NewRecorder()
				gate.ActionHandler().ServeHTTP(rec, req)
				return rec
			}
			for i := 1; i <= 3; i++ {
				if rec := openPR(); rec.Code != http.StatusBadGateway {
					t.Fatalf("call %d: want 502 from the failing opener, got %d: %s", i, rec.Code, rec.Body.String())
				}
				if i == tc.reloadAfter {
					b, p, err := loader.Load(ctx)
					if err != nil {
						t.Fatal(err)
					}
					if err := gate.Reload(b, p); err != nil {
						t.Fatal(err)
					}
				}
			}
			rec := openPR()
			if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "circuit_breaker_open") {
				t.Fatalf("fourth call: want 403 circuit_breaker_open, got %d: %s", rec.Code, rec.Body.String())
			}
			if opener.calls != 3 {
				t.Fatalf("opener called %d times, want 3", opener.calls)
			}
		})
	}
}
