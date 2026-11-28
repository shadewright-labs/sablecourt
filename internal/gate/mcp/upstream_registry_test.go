package mcpgate

import (
	"context"
	"io"
	"net/http"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/shadewright-labs/sablecourt/internal/config"
	"github.com/shadewright-labs/sablecourt/internal/domain"
	"github.com/shadewright-labs/sablecourt/internal/policy"
	"github.com/shadewright-labs/sablecourt/internal/secret"
	"github.com/shadewright-labs/sablecourt/internal/store/sqlite"
)

// countingBridge returns an upstream bridge whose HTTP client counts requests
// and answers each one with a fixed result.
func countingBridge(calls *atomic.Int32) *UpstreamBridge {
	return NewUpstreamBridge(
		secret.NewMemoryStore(map[string]string{"up_token": "up-secret"}),
		&http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			calls.Add(1)
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(`{"jsonrpc":"2.0","id":1,"result":{"from":"upstream"}}`)),
			}, nil
		})},
	)
}

func mustReload(t testing.TB, g *Gate, b *config.Bundle, pol *policy.Engine) {
	t.Helper()
	if err := g.Reload(b, pol); err != nil {
		t.Fatalf("Reload: %v", err)
	}
}

func openRegistryStore(t *testing.T) *sqlite.SQLiteStore {
	t.Helper()
	st, err := sqlite.Open(filepath.Join(t.TempDir(), "sablecourt.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	return st
}

func proxyWorkload(server string, tools ...string) domain.Workload {
	return domain.Workload{
		Metadata: domain.ObjectMeta{Name: "proxy", Zone: domain.ZoneRuntime},
		Spec: domain.WorkloadSpec{
			Class: domain.ClassLongLivedAgent,
			Grants: domain.GrantSet{MCPServers: []domain.UpstreamMCPServer{{
				Name: server, URL: "https://mcp.example.test/mcp", SecretName: "up_token", AllowedTools: tools,
			}}},
		},
	}
}

// callerBundle holds one workload that may call tool, and nothing else.
func callerBundle(tool string) (*config.Bundle, *policy.Engine, domain.Workload) {
	pol := domain.Policy{
		Metadata: domain.ObjectMeta{Name: "allow"},
		Spec: domain.PolicySpec{
			DefaultEffect: domain.EffectDeny,
			Rules:         []domain.PolicyRule{{Workload: "reader", Capability: "mcp:" + tool, Effect: domain.EffectAllow}},
		},
	}
	reader := domain.Workload{
		Metadata: domain.ObjectMeta{Name: "reader", Zone: domain.ZoneRuntime},
		Spec: domain.WorkloadSpec{
			Class:  domain.ClassCodingAgent,
			Grants: domain.GrantSet{MCPTools: []string{tool}},
		},
	}
	b := &config.Bundle{
		Policies:  map[string]domain.Policy{"allow": pol},
		Workloads: map[string]domain.Workload{"reader": reader},
	}
	return b, policy.NewEngine([]domain.Policy{pol}, nil), reader
}

func callAs(t *testing.T, gate *Gate, wl domain.Workload, tool string, args map[string]any) (*mcp.CallToolResult, error) {
	t.Helper()
	ctx := context.Background()
	srv := gate.NewServerForWorkload(domain.Lease{TaskID: "t1", WorkloadName: wl.Metadata.Name}, wl)
	ct, stp := mcp.NewInMemoryTransports()
	srvSession, err := srv.Connect(ctx, stp, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = srvSession.Close() })
	cliSession, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0.1.0"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cliSession.Close() })
	return cliSession.CallTool(ctx, &mcp.CallToolParams{Name: tool, Arguments: args})
}

// The reviewer's scenario: a bundle registers an upstream server named
// "memory", then a reload replaces that bundle. A call to memory.get must
// reach the built-in handler, and no request may reach the upstream server.
func TestBuiltinToolAnswersAfterShadowingBundleIsReplaced(t *testing.T) {
	ctx := context.Background()
	st := openRegistryStore(t)
	if err := st.PutMemory(ctx, domain.MemoryEntry{Namespace: "default", Key: "k", Value: "local-value"}); err != nil {
		t.Fatal(err)
	}

	b1 := &config.Bundle{Workloads: map[string]domain.Workload{"proxy": proxyWorkload("memory", "get")}}
	gate := New(Options{Store: st, Bundle: b1, Policy: policy.NewEngine(nil, nil)})
	RegisterBuiltinMemory(gate, st)
	var calls atomic.Int32
	if err := gate.RegisterUpstreamTools(b1.Workloads["proxy"].Spec.Grants.MCPServers, countingBridge(&calls)); err == nil {
		t.Fatal("RegisterUpstreamTools accepted an upstream memory.get")
	}

	b2, pol2, reader := callerBundle("memory.get")
	if err := gate.Reload(b2, pol2); err != nil {
		t.Fatal(err)
	}

	res, err := callAs(t, gate, reader, "memory.get", map[string]any{"key": "k"})
	if err != nil {
		t.Fatal(err)
	}
	if n := calls.Load(); n != 0 {
		t.Fatalf("memory.get reached the upstream server %d time(s): %+v", n, res)
	}
	if res.IsError || len(res.Content) != 1 {
		t.Fatalf("memory.get result = %+v", res)
	}
	if txt, _ := res.Content[0].(*mcp.TextContent); txt == nil || !strings.Contains(txt.Text, "local-value") {
		t.Fatalf("memory.get did not answer from Court Memory: %+v", res.Content[0])
	}
}

// A reload rebuilds the upstream tools from the new bundle only, so the tools
// of a server the new bundle no longer declares are gone.
func TestReloadRemovesToolsOfRemovedUpstreamServer(t *testing.T) {
	b1, pol1, reader := callerBundle("linear.list")
	b1.Workloads["proxy"] = proxyWorkload("linear", "list")
	gate := New(Options{Store: openRegistryStore(t), Bundle: b1, Policy: pol1})
	var calls atomic.Int32
	if err := gate.RegisterUpstreamTools(b1.Workloads["proxy"].Spec.Grants.MCPServers, countingBridge(&calls)); err != nil {
		t.Fatal(err)
	}

	if res, err := callAs(t, gate, reader, "linear.list", nil); err != nil || res.IsError || calls.Load() != 1 {
		t.Fatalf("before reload: res=%+v err=%v calls=%d", res, err, calls.Load())
	}

	b2, pol2, reader2 := callerBundle("linear.list") // proxy and its server removed
	if err := gate.Reload(b2, pol2); err != nil {
		t.Fatal(err)
	}

	res, err := callAs(t, gate, reader2, "linear.list", nil)
	if n := calls.Load(); n != 1 {
		t.Fatalf("removed upstream tool still reached the server (%d calls): res=%+v err=%v", n, res, err)
	}
	if err == nil || !strings.Contains(err.Error(), "unknown tool") {
		t.Fatalf("call after reload = %+v, %v; want unknown tool", res, err)
	}
}

// The gate refuses an upstream tool that takes a built-in name, as a second
// line behind the loader, both at registration and when a reload rebuilds
// the upstream tools.
func TestGateRefusesUpstreamToolWithBuiltinName(t *testing.T) {
	cases := []struct {
		server, tool, wantErr string
	}{
		{"memory", "get", `"memory.get" refused: a built-in tool has that name`},
		{"memory", "anything", `"memory" is a built-in tool namespace`},
		{"conclave", "read", `"conclave" is a built-in tool namespace`},
		{"sablecourt", "list_workloads", `"sablecourt.list_workloads" refused: a built-in tool has that name`},
		{"git", "open_pr", `"git" is a built-in tool namespace`},
		{"Sandbox", "x", `"sandbox" is a built-in tool namespace`},
	}
	for _, tc := range cases {
		t.Run(tc.server+"."+tc.tool, func(t *testing.T) {
			st := openRegistryStore(t)
			b := &config.Bundle{Workloads: map[string]domain.Workload{"proxy": proxyWorkload(tc.server, tc.tool, "ok")}}
			gate := New(Options{Store: st, Bundle: b, Policy: policy.NewEngine(nil, nil)})
			RegisterBuiltinMemory(gate, st)
			RegisterBuiltinGitOps(gate, t.TempDir(), nil)
			var calls atomic.Int32
			bridge := countingBridge(&calls)

			// A valid server in the same call is not added either: a refusal adds nothing.
			servers := slices.Concat(b.Workloads["proxy"].Spec.Grants.MCPServers,
				[]domain.UpstreamMCPServer{{Name: "linear", URL: "https://mcp.example.test/mcp", AllowedTools: []string{"list"}}})
			err := gate.RegisterUpstreamTools(servers, bridge)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("RegisterUpstreamTools = %v, want %q", err, tc.wantErr)
			}
			if len(gate.upstream) != 0 || gate.bridge != nil {
				t.Fatalf("refused registration changed the gate: upstream=%v bridge=%v", gate.upstream, gate.bridge)
			}
			if err := gate.SetUpstreamBridge(bridge); err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("SetUpstreamBridge = %v, want %q", err, tc.wantErr)
			}
			if len(gate.upstream) != 0 {
				t.Fatalf("refused start-up registry is not empty: %v", gate.upstream)
			}
			if err := gate.Reload(b, policy.NewEngine(nil, nil)); err == nil || !strings.Contains(err.Error(), "reload refused") || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("Reload = %v, want reload refused and %q", err, tc.wantErr)
			}
			full := tc.server + "." + tc.tool
			if _, ok := gate.upstream[full]; ok {
				t.Fatalf("%s is in the upstream registry", full)
			}
			if def, ok := gate.tools[full]; ok && strings.HasPrefix(def.Description, "Proxied") {
				t.Fatalf("%s was replaced by an upstream tool", full)
			}
		})
	}
}

// A reload whose bundle declares an upstream tool with a built-in name is
// refused whole: the gate keeps its previous bundle, policy and upstream
// tools, and the old upstream tool still answers.
func TestRefusedReloadKeepsPreviousBundlePolicyAndUpstreamTools(t *testing.T) {
	b1, pol1, reader := callerBundle("linear.list")
	b1.Workloads["proxy"] = proxyWorkload("linear", "list")
	gate := New(Options{Store: openRegistryStore(t), Bundle: b1, Policy: pol1})
	var calls atomic.Int32
	if err := gate.SetUpstreamBridge(countingBridge(&calls)); err != nil {
		t.Fatal(err)
	}

	bad, badPol, _ := callerBundle("memory.get")
	bad.Workloads["proxy"] = proxyWorkload("memory", "get")
	err := gate.Reload(bad, badPol)
	if err == nil || !strings.Contains(err.Error(), "reload refused") {
		t.Fatalf("Reload = %v, want reload refused", err)
	}
	snap := gate.snapshot()
	if snap.bundle != b1 || snap.policy != pol1 {
		t.Fatal("refused reload changed the bundle or policy")
	}
	if _, ok := gate.upstream["linear.list"]; !ok || len(gate.upstream) != 1 {
		t.Fatalf("refused reload changed the upstream tools: %v", gate.upstream)
	}
	if res, err := callAs(t, gate, reader, "linear.list", nil); err != nil || res.IsError || calls.Load() != 1 {
		t.Fatalf("previous upstream tool after refused reload: res=%+v err=%v calls=%d", res, err, calls.Load())
	}
}
