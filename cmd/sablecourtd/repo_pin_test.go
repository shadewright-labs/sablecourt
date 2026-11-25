package main

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/shadewright-labs/sablecourt/internal/config"
	"github.com/shadewright-labs/sablecourt/internal/domain"
	mcpgate "github.com/shadewright-labs/sablecourt/internal/gate/mcp"
	"github.com/shadewright-labs/sablecourt/internal/gate/model"
	"github.com/shadewright-labs/sablecourt/internal/store/sqlite"
)

type countingOpener struct{ calls int }

func (o *countingOpener) OpenPR(_ context.Context, in mcpgate.OpenPRInput) (string, error) {
	o.calls++
	return "https://github.com/" + in.Owner + "/" + in.Repo + "/pull/1", nil
}

// High 1: the daemon pins the repository once. Start pinned to acme/widgets,
// point the checkout at acme/other, then reload by each trigger: the operator
// reload route and the reload function the dashboard and the manifests git
// sync call. acme/other stays refused after every one.
func TestDaemonReloadKeepsTheStartPin(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	manifests := filepath.Join(dir, "sablecourt.yaml")
	if err := os.WriteFile(manifests, []byte(breakerManifest), 0o600); err != nil {
		t.Fatal(err)
	}
	repo := filepath.Join(dir, "repo")
	git := func(args ...string) {
		t.Helper()
		if out, err := exec.Command("git", append([]string{"-C", repo}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v (%s)", args, err, out)
		}
	}
	if err := os.MkdirAll(repo, 0o700); err != nil {
		t.Fatal(err)
	}
	git("init", "-q")
	git("remote", "add", "origin", "https://github.com/acme/widgets.git")

	st, err := sqlite.Open(filepath.Join(dir, "sablecourt.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	loader := newPolicyLoader(st, manifests)
	bundle, pol, err := loader.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.CreateTask(ctx, domain.Task{ID: "task-p", IdempotencyKey: "p", WorkloadName: "pr-agent", State: domain.TaskStateWorking}); err != nil {
		t.Fatal(err)
	}
	token, err := model.MintTaskToken(ctx, st, "task-p", "pr-agent", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	gate := mcpgate.New(mcpgate.Options{Store: st, Bundle: bundle, Policy: pol})
	opener := &countingOpener{}
	binding := mcpgate.RegisterBuiltinGitOps(gate, repo, opener)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	reloadFn := func() error {
		b, p, err := loader.Load(ctx)
		if err != nil {
			return err
		}
		reloadGitOps(ctx, logger, gate, binding, b, p)
		return nil
	}
	h := newHandler(st, nil, func() *config.Bundle { return bundle }, reloadFn, nil, gate, nil, nil, nil, nil, nil, nil, nil, handlerConfig{operatorToken: testOperatorToken})

	openPR := func(repoName string) *httptest.ResponseRecorder {
		body := `{"payload":{"owner":"acme","repo":"` + repoName + `","head":"sablecourt/x","base":"main","title":"T"}}`
		req := httptest.NewRequest(http.MethodPost, "http://sablecourt.local/git.open_pr", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		gate.ActionHandler().ServeHTTP(rec, req)
		return rec
	}

	git("remote", "set-url", "origin", "https://github.com/acme/other.git")
	triggers := map[string]func(){
		"POST /api/v1/reload": func() {
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, operatorRequest(http.MethodPost, "/api/v1/reload", nil))
			if rec.Code != http.StatusOK {
				t.Fatalf("reload route: %d %s", rec.Code, rec.Body.String())
			}
		},
		"dashboard and git sync reload": func() {
			if err := reloadFn(); err != nil {
				t.Fatal(err)
			}
		},
	}
	for name, trigger := range triggers {
		trigger()
		if rec := openPR("other"); rec.Code != http.StatusForbidden {
			t.Fatalf("after %s: acme/other must stay refused, got %d: %s", name, rec.Code, rec.Body.String())
		}
	}
	if opener.calls != 0 {
		t.Fatalf("%d pull requests opened", opener.calls)
	}
	records, err := st.ListAudit(ctx, 50)
	if err != nil {
		t.Fatal(err)
	}
	drift := 0
	for _, r := range records {
		if strings.Contains(r.DetailJSON, "repo_origin_changed") {
			drift++
		}
	}
	if drift < len(triggers) {
		t.Fatalf("want an origin drift audit row per reload, got %d", drift)
	}
}
