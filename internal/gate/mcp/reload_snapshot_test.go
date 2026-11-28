package mcpgate

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/shadewright-labs/sablecourt/internal/config"
	"github.com/shadewright-labs/sablecourt/internal/domain"
	"github.com/shadewright-labs/sablecourt/internal/gate/model"
	"github.com/shadewright-labs/sablecourt/internal/policy"
	"github.com/shadewright-labs/sablecourt/internal/secret"
	"github.com/shadewright-labs/sablecourt/internal/store"
)

// reloadOnLeaseStore runs reload once, inside the lease lookup of the next
// authenticated request after armed is set. The request has captured its gate
// snapshot by then, so the reload lands between that capture and the build of
// the per-request MCP server.
type reloadOnLeaseStore struct {
	store.Store
	armed  atomic.Bool
	reload func()
}

func (s *reloadOnLeaseStore) GetActiveLeaseByTokenHash(ctx context.Context, hash string, now time.Time) (domain.Lease, error) {
	if s.armed.Swap(false) && s.reload != nil {
		s.reload()
	}
	return s.Store.GetActiveLeaseByTokenHash(ctx, hash, now)
}

// A tools/call that captured bundle 1 and policy 1 must also use bundle 1's
// upstream server and secret, even when a reload swaps in bundle 2 (which
// points the same server name at another URL and secret) mid-request.
func TestMCPHandlerKeepsConsistentUpstreamSnapshotDuringReload(t *testing.T) {
	ctx := context.Background()
	rawStore := openRegistryStore(t)
	if err := rawStore.UpsertWorkload(ctx, domain.WorkloadRecord{Name: "worker", Zone: domain.ZoneRuntime, Class: domain.ClassLongLivedAgent, AgentCardJSON: "{}"}); err != nil {
		t.Fatal(err)
	}
	if err := rawStore.CreateTask(ctx, domain.Task{ID: "t1", IdempotencyKey: "k1", WorkloadName: "worker", State: domain.TaskStateWorking}); err != nil {
		t.Fatal(err)
	}
	token, err := model.MintTaskToken(ctx, rawStore, "t1", "worker", time.Hour)
	if err != nil {
		t.Fatal(err)
	}

	b1 := &config.Bundle{Workloads: map[string]domain.Workload{
		"worker": {
			Metadata: domain.ObjectMeta{Name: "worker", Zone: domain.ZoneRuntime},
			Spec: domain.WorkloadSpec{
				Class: domain.ClassLongLivedAgent,
				Grants: domain.GrantSet{
					MCPTools: []string{"linear.list"},
					MCPServers: []domain.UpstreamMCPServer{{
						Name: "linear", URL: "https://staging.example/mcp", SecretName: "staging_tok", AllowedTools: []string{"list"},
					}},
				},
			},
		},
	}}
	pol1 := policy.NewEngine([]domain.Policy{{
		Metadata: domain.ObjectMeta{Name: "allow"},
		Spec: domain.PolicySpec{
			DefaultEffect: domain.EffectDeny,
			Rules:         []domain.PolicyRule{{Workload: "worker", Capability: "mcp:linear.list", Effect: domain.EffectAllow}},
		},
	}}, nil)

	b2 := &config.Bundle{Workloads: map[string]domain.Workload{
		"worker": {
			Metadata: domain.ObjectMeta{Name: "worker", Zone: domain.ZoneRuntime},
			Spec:     domain.WorkloadSpec{Class: domain.ClassLongLivedAgent},
		},
		"admin": {
			Metadata: domain.ObjectMeta{Name: "admin", Zone: domain.ZoneRuntime},
			Spec: domain.WorkloadSpec{
				Class: domain.ClassLongLivedAgent,
				Grants: domain.GrantSet{
					MCPServers: []domain.UpstreamMCPServer{{
						Name: "linear", URL: "https://prod.example/mcp", SecretName: "prod_tok", AllowedTools: []string{"list"},
					}},
				},
			},
		},
	}}
	pol2 := policy.NewEngine([]domain.Policy{{
		Metadata: domain.ObjectMeta{Name: "deny"},
		Spec:     domain.PolicySpec{DefaultEffect: domain.EffectDeny},
	}}, nil)

	var (
		mu               sync.Mutex
		gotHost, gotAuth string
	)
	bridge := NewUpstreamBridge(
		secret.NewMemoryStore(map[string]string{"staging_tok": "staging-secret", "prod_tok": "prod-secret"}),
		&http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			mu.Lock()
			gotHost, gotAuth = r.URL.Host, r.Header.Get("Authorization")
			mu.Unlock()
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(`{"jsonrpc":"2.0","id":1,"result":{"ok":true}}`)),
			}, nil
		})},
	)

	wrapped := &reloadOnLeaseStore{Store: rawStore}
	gate := New(Options{Store: wrapped, Bundle: b1, Policy: pol1})
	if err := gate.SetUpstreamBridge(bridge); err != nil {
		t.Fatal(err)
	}
	reloaded := false
	wrapped.reload = func() {
		reloaded = true
		if err := gate.Reload(b2, pol2); err != nil {
			t.Errorf("Reload: %v", err)
		}
	}

	handler := gate.MCPHandler()
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0.1.0"}, nil)
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint:             "/mcp",
		DisableStandaloneSSE: true,
		HTTPClient: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			r = r.Clone(r.Context())
			r.Header.Set("Authorization", "Bearer "+token)
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, r)
			return rec.Result(), nil
		})},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()

	wrapped.armed.Store(true)
	res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "linear.list"})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if !reloaded {
		t.Fatal("the reload did not run during the call")
	}
	mu.Lock()
	defer mu.Unlock()
	if gotHost != "staging.example" || gotAuth != "Bearer staging-secret" {
		t.Fatalf("bundle 1 call reached host %q with auth %q, want bundle 1's staging server and secret (result error %v)", gotHost, gotAuth, res.IsError)
	}
}
