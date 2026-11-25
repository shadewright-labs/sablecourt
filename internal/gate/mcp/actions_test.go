package mcpgate

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/shadewright-labs/sablecourt/internal/approval"
	"github.com/shadewright-labs/sablecourt/internal/config"
	"github.com/shadewright-labs/sablecourt/internal/domain"
	"github.com/shadewright-labs/sablecourt/internal/gate/model"
	"github.com/shadewright-labs/sablecourt/internal/policy"
	"github.com/shadewright-labs/sablecourt/internal/secret"
	"github.com/shadewright-labs/sablecourt/internal/store/sqlite"
)

type stubPROpener struct {
	last OpenPRInput
}

func (s *stubPROpener) OpenPR(_ context.Context, in OpenPRInput) (string, error) {
	s.last = in
	return "https://github.com/" + in.Owner + "/" + in.Repo + "/pull/99", nil
}

func initGitRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v (%s)", args, err, string(out))
		}
	}
	run("init")
	run("config", "user.email", "test@sablecourt.local")
	run("config", "user.name", "Sablecourt Test")
	_ = os.WriteFile(filepath.Join(dir, "README.md"), []byte("hi\n"), 0o600)
	run("add", "README.md")
	run("commit", "-m", "init")
	return dir
}

func TestBuiltinGitOpsActions(t *testing.T) {
	ctx := context.Background()
	repo := initGitRepo(t)
	// git.open_pr only targets the GitHub repository of the repo root's origin.
	if out, err := exec.Command("git", "-C", repo, "remote", "add", "origin", "https://github.com/shadewright-labs/sablecourt.git").CombinedOutput(); err != nil {
		t.Fatalf("git remote add: %v (%s)", err, out)
	}
	st, err := sqlite.Open(filepath.Join(t.TempDir(), "sablecourt.db"))
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	_ = st.Migrate(ctx)

	bundle := &config.Bundle{
		Policies: map[string]domain.Policy{
			"default": {
				Metadata: domain.ObjectMeta{Name: "default"},
				Spec: domain.PolicySpec{
					DefaultEffect: domain.EffectDeny,
					Rules: []domain.PolicyRule{
						{Workload: "ops-agent", Capability: "action:git.create_branch", Effect: domain.EffectAllow},
						{Workload: "ops-agent", Capability: "action:git.open_pr", Effect: domain.EffectAllow},
						{Workload: "ops-agent", Capability: "action:sandbox.propose_promotion", Effect: domain.EffectAllow},
					},
				},
			},
		},
		Workloads: map[string]domain.Workload{
			"ops-agent": {
				Metadata: domain.ObjectMeta{Name: "ops-agent", Zone: domain.ZoneRuntime},
				Spec: domain.WorkloadSpec{
					Class: domain.ClassLongLivedAgent,
					Grants: domain.GrantSet{
						Actions: []string{"git.create_branch", "git.open_pr", "sandbox.propose_promotion"},
					},
				},
			},
			"exp-sandbox": {
				Metadata: domain.ObjectMeta{Name: "exp-sandbox", Zone: domain.ZoneSandbox},
				Spec:     domain.WorkloadSpec{Class: domain.ClassDeterministicController},
			},
		},
	}

	_ = st.UpsertWorkload(ctx, domain.WorkloadRecord{
		Name: "ops-agent", Zone: domain.ZoneRuntime, Class: domain.ClassLongLivedAgent, ManifestDigest: "d1", AgentCardJSON: "{}",
	})
	_ = st.CreateTask(ctx, domain.Task{
		ID: "t1", IdempotencyKey: "k1", WorkloadName: "ops-agent", State: domain.TaskStateWorking,
	})
	token, _ := model.MintTaskToken(ctx, st, "t1", "ops-agent", time.Hour)

	eng := policy.NewEngine([]domain.Policy{bundle.Policies["default"]}, nil)
	gate := New(Options{Store: st, Bundle: bundle, Policy: eng, Approvals: approval.NewQueue(st, autoApprover{})})
	prStub := &stubPROpener{}
	RegisterBuiltinGitOps(gate, repo, prStub)

	h := gate.ActionHandler()

	// 1. Rejects unsafe branch name (main) as invalid input, before any git call.
	req := httptest.NewRequest(http.MethodPost, "http://sablecourt.local/git.create_branch", strings.NewReader(`{"payload":{"branch":"main"}}`))
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected refusal for protected branch main, got %d: %s", rec.Code, rec.Body.String())
	}

	// 2. Creates sablecourt/fix-1 branch in repo from an explicit base.
	base := strings.TrimSpace(gitOut(t, repo, "symbolic-ref", "--short", "HEAD"))
	req = httptest.NewRequest(http.MethodPost, "http://sablecourt.local/git.create_branch", strings.NewReader(`{"payload":{"branch":"sablecourt/fix-1","base":"`+base+`"}}`))
	req.Header.Set("Authorization", "Bearer "+token)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 creating branch, got %d: %s", rec.Code, rec.Body.String())
	}

	// 3. Opens PR via stub PROpener.
	req = httptest.NewRequest(http.MethodPost, "http://sablecourt.local/git.open_pr", strings.NewReader(`{"payload":{"owner":"shadewright-labs","repo":"sablecourt","head":"sablecourt/fix-1","base":"main","title":"Fix 1"}}`))
	req.Header.Set("Authorization", "Bearer "+token)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "/pull/99") {
		t.Fatalf("expected PR URL, got %d: %s", rec.Code, rec.Body.String())
	}

	// 4. Proposes sandbox promotion for exp-sandbox.
	req = httptest.NewRequest(http.MethodPost, "http://sablecourt.local/sandbox.propose_promotion", strings.NewReader(`{"payload":{"workload":"exp-sandbox","branch":"sablecourt/promote-exp","reason":"tests passed"}}`))
	req.Header.Set("Authorization", "Bearer "+token)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 for sandbox promotion proposal, got %d: %s", rec.Code, rec.Body.String())
	}
	var out map[string]string
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if out["status"] != "proposal_ready" {
		t.Fatalf("unexpected promotion response: %+v", out)
	}
}

// setupOpenPRGate wires git.open_pr with the real GitHub opener against a fake
// API, for a repo root whose origin is github.com/acme/widgets.
func setupOpenPRGate(t *testing.T, apiURL string) (http.Handler, string) {
	t.Helper()
	ctx := context.Background()
	repo := initGitRepo(t)
	if out, err := exec.Command("git", "-C", repo, "remote", "add", "origin", "https://github.com/acme/widgets.git").CombinedOutput(); err != nil {
		t.Fatalf("git remote add: %v (%s)", err, out)
	}
	st, err := sqlite.Open(filepath.Join(t.TempDir(), "sablecourt.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	pol := domain.Policy{
		Metadata: domain.ObjectMeta{Name: "default"},
		Spec: domain.PolicySpec{DefaultEffect: domain.EffectDeny, Rules: []domain.PolicyRule{
			{Workload: "pr-agent", Capability: "action:git.open_pr", Effect: domain.EffectAllow},
		}},
	}
	bundle := &config.Bundle{
		Policies: map[string]domain.Policy{"default": pol},
		Workloads: map[string]domain.Workload{"pr-agent": {
			Metadata: domain.ObjectMeta{Name: "pr-agent", Zone: domain.ZoneRuntime},
			Spec:     domain.WorkloadSpec{Class: domain.ClassLongLivedAgent, Grants: domain.GrantSet{Actions: []string{"git.open_pr"}}},
		}},
	}
	_ = st.UpsertWorkload(ctx, domain.WorkloadRecord{Name: "pr-agent", Zone: domain.ZoneRuntime, Class: domain.ClassLongLivedAgent, AgentCardJSON: "{}"})
	if err := st.CreateTask(ctx, domain.Task{ID: "t-pr", IdempotencyKey: "pr", WorkloadName: "pr-agent", State: domain.TaskStateWorking}); err != nil {
		t.Fatal(err)
	}
	token, err := model.MintTaskToken(ctx, st, "t-pr", "pr-agent", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	gate := New(Options{Store: st, Bundle: bundle, Policy: policy.NewEngine([]domain.Policy{pol}, nil)})
	RegisterBuiltinGitOps(gate, repo, &GitHubPROpener{
		Secrets: secret.NewMemoryStore(map[string]string{"github_token": "test-token-not-real"}),
		APIURL:  apiURL,
	})
	return gate.ActionHandler(), token
}

func TestOpenPRValidatesOwnerAndRepoBeforeAnyHTTPCall(t *testing.T) {
	var hits atomic.Int32
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"html_url":"https://github.com/acme/widgets/pull/7"}`))
	}))
	defer api.Close()
	h, token := setupOpenPRGate(t, api.URL)

	cases := []struct {
		name, owner, repo string
		wantOK            bool
	}{
		{"path escape in owner", "../../user", "widgets", false},
		{"path escape in repo", "acme", "../../orgs/acme", false},
		{"dot dot repo", "acme", "..", false},
		{"slash in owner", "acme/widgets", "pulls", false},
		{"encoded slash", "acme%2Fx", "widgets", false},
		{"query in repo", "acme", "widgets?x=1", false},
		{"repo outside the grant", "acme", "other", false},
		{"owner outside the grant", "someone-else", "widgets", false},
		{"granted repo", "acme", "widgets", true},
		{"granted repo, other case", "ACME", "Widgets", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before := hits.Load()
			body, _ := json.Marshal(map[string]any{"payload": map[string]string{
				"owner": tc.owner, "repo": tc.repo, "head": "sablecourt/fix-1", "base": "main", "title": "Fix",
			}})
			req := httptest.NewRequest(http.MethodPost, "http://sablecourt.local/git.open_pr", strings.NewReader(string(body)))
			req.Header.Set("Authorization", "Bearer "+token)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			called := hits.Load() != before
			if tc.wantOK {
				if rec.Code != http.StatusOK || !called {
					t.Fatalf("want 200 and one API call, got %d (called=%v): %s", rec.Code, called, rec.Body.String())
				}
				return
			}
			if rec.Code == http.StatusOK || called {
				t.Fatalf("want refusal before any HTTP call, got %d (called=%v): %s", rec.Code, called, rec.Body.String())
			}
		})
	}
}

func TestGitHubPROpenerRefusesUnsafeNamesAndRedirects(t *testing.T) {
	var followed atomic.Int32
	elsewhere := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		followed.Add(1)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"html_url":"https://example.test/pull/1"}`))
	}))
	defer elsewhere.Close()
	var hits atomic.Int32
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		http.Redirect(w, r, elsewhere.URL+"/steal", http.StatusTemporaryRedirect)
	}))
	defer api.Close()

	gh := &GitHubPROpener{Secrets: secret.NewMemoryStore(map[string]string{"github_token": "test-token-not-real"}), APIURL: api.URL}
	in := OpenPRInput{Owner: "../../user", Repo: "widgets", Head: "sablecourt/x", Base: "main", Title: "t"}
	if _, err := gh.OpenPR(context.Background(), in); err == nil || hits.Load() != 0 {
		t.Fatalf("unsafe owner: want error before any call, got err=%v hits=%d", err, hits.Load())
	}

	in.Owner = "acme"
	if _, err := gh.OpenPR(context.Background(), in); err == nil {
		t.Fatalf("redirect must not be followed and must fail")
	}
	if followed.Load() != 0 {
		t.Fatalf("client followed a redirect carrying the token")
	}
}

func TestParseGitHubRemote(t *testing.T) {
	cases := []struct {
		remote, owner, repo string
		ok                  bool
	}{
		{"https://github.com/acme/widgets.git", "acme", "widgets", true},
		{"https://github.com/acme/widgets", "acme", "widgets", true},
		{"git@github.com:acme/widgets.git", "acme", "widgets", true},
		{"ssh://git@github.com/acme/widgets.git", "acme", "widgets", true},
		{"https://GitHub.com/acme/widgets/", "acme", "widgets", true},
		{"https://gitlab.com/acme/widgets.git", "", "", false},
		{"https://github.com.evil.test/acme/widgets.git", "", "", false},
		{"http://github.com/acme/widgets.git", "", "", false},
		{"https://github.com/acme/widgets/extra", "", "", false},
		{"https://github.com/../widgets", "", "", false},
		{"https://github.com/acme/widgets?x=1", "", "", false},
		{"/srv/git/widgets.git", "", "", false},
		{"", "", "", false},
	}
	for _, tc := range cases {
		owner, repo, ok := parseGitHubRemote(tc.remote)
		if ok != tc.ok || owner != tc.owner || repo != tc.repo {
			t.Errorf("parseGitHubRemote(%q) = %q, %q, %v; want %q, %q, %v", tc.remote, owner, repo, ok, tc.owner, tc.repo, tc.ok)
		}
	}
}

func TestGitHubPROpenerTimesOut(t *testing.T) {
	release := make(chan struct{})
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
	}))
	defer api.Close()
	defer close(release)
	gh := &GitHubPROpener{Secrets: secret.NewMemoryStore(map[string]string{"github_token": "test-token-not-real"}), APIURL: api.URL, Timeout: 100 * time.Millisecond}
	start := time.Now()
	_, err := gh.OpenPR(context.Background(), OpenPRInput{Owner: "acme", Repo: "widgets", Head: "sablecourt/x", Base: "main", Title: "t"})
	if err == nil || time.Since(start) > 5*time.Second {
		t.Fatalf("want a timeout error within the bound, got %v after %v", err, time.Since(start))
	}
	if (&GitHubPROpener{}).httpClient().Timeout != defaultGitHubTimeout {
		t.Fatalf("default client has no timeout")
	}
}
