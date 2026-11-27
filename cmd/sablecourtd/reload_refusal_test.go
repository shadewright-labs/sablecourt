package main

import (
	"bytes"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/shadewright-labs/sablecourt/internal/config"
	"github.com/shadewright-labs/sablecourt/internal/domain"
	mcpgate "github.com/shadewright-labs/sablecourt/internal/gate/mcp"
	"github.com/shadewright-labs/sablecourt/internal/policy"
)

// A bundle the Data and Action Gate refuses (here an upstream server named
// like a built-in namespace, which the loader would refuse first) stops the
// whole reload: nothing after the gate step runs, the error and the log say
// the reload was refused, and the reload route answers 400 with that error.
func TestReloadRefusedByGateAppliesNothing(t *testing.T) {
	b1 := &config.Bundle{Workloads: map[string]domain.Workload{}}
	pol1 := policy.NewEngine(nil, nil)
	gate := mcpgate.New(mcpgate.Options{Bundle: b1, Policy: pol1})
	if err := gate.SetUpstreamBridge(mcpgate.NewUpstreamBridge(nil, nil)); err != nil {
		t.Fatal(err)
	}
	binding := mcpgate.RegisterBuiltinGitOps(gate, t.TempDir(), nil)

	bad := &config.Bundle{Workloads: map[string]domain.Workload{
		"proxy": {
			Metadata: domain.ObjectMeta{Name: "proxy", Zone: domain.ZoneRuntime},
			Spec: domain.WorkloadSpec{Class: domain.ClassLongLivedAgent, Grants: domain.GrantSet{
				MCPServers: []domain.UpstreamMCPServer{{Name: "memory", URL: "https://mcp.example.test/mcp", AllowedTools: []string{"get"}}},
			}},
		},
	}}
	var logBuf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logBuf, nil))
	applied := false
	steps := reloadSteps{
		load: func() (*config.Bundle, *policy.Engine, error) { return bad, policy.NewEngine(nil, nil), nil },
		gate: func(b *config.Bundle, pol *policy.Engine) error {
			return reloadGitOps(t.Context(), logger, gate, binding, b, pol)
		},
		apply:  func(*config.Bundle, *policy.Engine) { applied = true },
		logger: logger,
	}

	err := steps.run()
	if err == nil || !strings.HasPrefix(err.Error(), "reload refused") || !strings.Contains(err.Error(), `"memory.get"`) {
		t.Fatalf("run = %v, want reload refused naming memory.get", err)
	}
	if applied {
		t.Fatal("apply ran after the gate refused the bundle")
	}
	if !strings.Contains(logBuf.String(), "manifest reload refused; previous bundle, policy, upstream tools and stored workloads kept") {
		t.Fatalf("log does not say the reload was refused:\n%s", logBuf.String())
	}

	env := newAPIEnv(t, apiEnvConfig{reload: steps.run})
	rec := apiCall(env.h, http.MethodPost, "/api/v1/reload", "")
	if rec.Code != http.StatusBadRequest || !strings.HasPrefix(apiError(t, rec), "reload refused") {
		t.Fatalf("POST /api/v1/reload = %d %s", rec.Code, rec.Body.String())
	}
	if applied {
		t.Fatal("apply ran after the gate refused the bundle via the route")
	}
}

// A failed load and a refusal from a gate step without the prefix both stop
// before apply; the second is still reported as a refused reload.
func TestReloadStepsStopBeforeApply(t *testing.T) {
	for _, tc := range []struct {
		name    string
		loadErr error
		gateErr error
		want    string
	}{
		{"load fails", errors.New("manifest: bad zone"), nil, "manifest: bad zone"},
		{"gate refuses", nil, errors.New("upstream MCP tool refused"), "reload refused: upstream MCP tool refused"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			applied := false
			err := reloadSteps{
				load:  func() (*config.Bundle, *policy.Engine, error) { return &config.Bundle{}, nil, tc.loadErr },
				gate:  func(*config.Bundle, *policy.Engine) error { return tc.gateErr },
				apply: func(*config.Bundle, *policy.Engine) { applied = true },
			}.run()
			if err == nil || err.Error() != tc.want || applied {
				t.Fatalf("run = %v (applied %v), want %q and no apply", err, applied, tc.want)
			}
		})
	}
	applied := false
	if err := (reloadSteps{
		load:  func() (*config.Bundle, *policy.Engine, error) { return &config.Bundle{}, nil, nil },
		gate:  func(*config.Bundle, *policy.Engine) error { return nil },
		apply: func(*config.Bundle, *policy.Engine) { applied = true },
	}).run(); err != nil || !applied {
		t.Fatalf("accepted reload: err=%v applied=%v", err, applied)
	}
}
