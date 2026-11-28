package main

import (
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/shadewright-labs/sablecourt/internal/config"
	"github.com/shadewright-labs/sablecourt/internal/domain"
	mcpgate "github.com/shadewright-labs/sablecourt/internal/gate/mcp"
	"github.com/shadewright-labs/sablecourt/internal/policy"
	"github.com/shadewright-labs/sablecourt/internal/store/sqlite"
)

// A reload the gate refuses writes nothing to the store: the workloads, their
// zones and the manifest digest stay those of the last accepted bundle. The
// steps are wired as the daemon wires them. An accepted reload then records
// the new bundle.
func TestRefusedGateReloadLeavesStoreWorkloadsUnchanged(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()
	manifests := filepath.Join(dir, "sablecourt.yaml")
	m1 := `apiVersion: sablecourt.io/v1alpha1
kind: Workload
metadata: {name: alpha, zone: sandbox}
spec:
  class: long-lived-agent
  runtime: {type: docker, image: "alpine:3.21"}
`
	if err := os.WriteFile(manifests, []byte(m1), 0o600); err != nil {
		t.Fatal(err)
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
	b1, pol1, err := loader.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := recordWorkloads(ctx, st, b1, false); err != nil {
		t.Fatal(err)
	}
	gate := mcpgate.New(mcpgate.Options{Store: st, Bundle: b1, Policy: pol1})
	gate.RegisterTool(mcpgate.ToolDef{Name: "custom.read"})
	if err := gate.SetUpstreamBridge(mcpgate.NewUpstreamBridge(nil, nil)); err != nil {
		t.Fatal(err)
	}
	binding := mcpgate.RegisterBuiltinGitOps(gate, dir, nil)

	m2 := `apiVersion: sablecourt.io/v1alpha1
kind: Workload
metadata: {name: alpha, zone: runtime}
spec:
  class: long-lived-agent
  runtime: {type: docker, image: "alpine:3.21"}
---
apiVersion: sablecourt.io/v1alpha1
kind: Workload
metadata: {name: proxy, zone: runtime}
spec:
  class: long-lived-agent
  runtime: {type: docker, image: "alpine:3.21"}
  grants:
    mcpServers: [{name: custom, url: "https://mcp.example.test/mcp", allowedTools: [read]}]
`
	if err := os.WriteFile(manifests, []byte(m2), 0o600); err != nil {
		t.Fatal(err)
	}

	applied := false
	steps := reloadSteps{
		load:    func() (*config.Bundle, *policy.Engine, error) { return loader.Load(ctx) },
		check:   gate.CheckReload,
		persist: func(b *config.Bundle) error { return recordWorkloads(ctx, st, b, false) },
		gate: func(b *config.Bundle, pol *policy.Engine) error {
			return reloadGitOps(ctx, slog.Default(), gate, binding, b, pol)
		},
		apply: func(*config.Bundle, *policy.Engine) { applied = true },
	}
	if err := steps.run(); err == nil || !strings.HasPrefix(err.Error(), "reload refused") {
		t.Fatalf("run = %v, want reload refused", err)
	}
	if applied {
		t.Fatal("apply ran after gate refusal")
	}

	items, err := st.ListWorkloads(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].Name != "alpha" || items[0].Zone != domain.ZoneSandbox || items[0].ManifestDigest != b1.ManifestDigest {
		t.Fatalf("refused reload mutated store workloads: %+v (want only sandbox alpha digest %s)", items, b1.ManifestDigest)
	}

	// Drop the refused server: the same reload is accepted and recorded.
	if err := os.WriteFile(manifests, []byte(strings.Split(m2, "  grants:")[0]), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := steps.run(); err != nil {
		t.Fatalf("accepted reload: %v", err)
	}
	if !applied {
		t.Fatal("apply did not run for an accepted reload")
	}
	items, err = st.ListWorkloads(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 || items[0].Zone != domain.ZoneRuntime || items[0].ManifestDigest == b1.ManifestDigest {
		t.Fatalf("accepted reload not recorded: %+v", items)
	}
}

// check runs before persist, and persist before gate and apply: a refusal at
// check writes nothing, and a store error stops the swap.
func TestReloadStepsCheckBeforePersist(t *testing.T) {
	var order []string
	steps := func(checkErr, persistErr error) reloadSteps {
		order = nil
		return reloadSteps{
			load: func() (*config.Bundle, *policy.Engine, error) { return &config.Bundle{}, nil, nil },
			check: func(*config.Bundle) error {
				order = append(order, "check")
				return checkErr
			},
			persist: func(*config.Bundle) error {
				order = append(order, "persist")
				return persistErr
			},
			gate: func(*config.Bundle, *policy.Engine) error {
				order = append(order, "gate")
				return nil
			},
			apply: func(*config.Bundle, *policy.Engine) { order = append(order, "apply") },
		}
	}
	for _, tc := range []struct {
		name               string
		checkErr, storeErr error
		wantErr, wantOrder string
	}{
		{"check refuses", errors.New("upstream MCP tool refused"), nil, "reload refused: upstream MCP tool refused", "check"},
		{"store fails", nil, errors.New("disk full"), "disk full", "check persist"},
		{"accepted", nil, nil, "", "check persist gate apply"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := steps(tc.checkErr, tc.storeErr).run()
			gotErr := ""
			if err != nil {
				gotErr = err.Error()
			}
			if gotErr != tc.wantErr || strings.Join(order, " ") != tc.wantOrder {
				t.Fatalf("run = %q with steps %v, want %q with %q", gotErr, order, tc.wantErr, tc.wantOrder)
			}
		})
	}
}

// Council panel workloads are recorded without a manifest digest or agent
// card; a manifest workload of the same bundle carries both.
func TestRecordWorkloadsCouncilPanel(t *testing.T) {
	st := daemonTestStore(t)
	b, err := addCouncilPanelWorkloads(&config.Bundle{
		ManifestDigest: "d1",
		Workloads: map[string]domain.Workload{
			"alpha": {Metadata: domain.ObjectMeta{Name: "alpha", Zone: domain.ZoneSandbox}, Spec: domain.WorkloadSpec{Class: domain.ClassLongLivedAgent}},
		},
	}, "seatbelt")
	if err != nil {
		t.Fatal(err)
	}
	if err := recordWorkloads(t.Context(), st, b, true); err != nil {
		t.Fatal(err)
	}
	items, err := st.ListWorkloads(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1+len(councilPanel) {
		t.Fatalf("recorded %d workloads, want %d", len(items), 1+len(councilPanel))
	}
	for _, it := range items {
		panel := isCouncilPanelRole(it.Name)
		if panel && (it.ManifestDigest != "" || it.AgentCardJSON != "" || it.Class != domain.ClassCodingAgent) {
			t.Fatalf("panel workload recorded as %+v", it)
		}
		if !panel && (it.ManifestDigest != "d1" || it.AgentCardJSON == "") {
			t.Fatalf("manifest workload recorded as %+v", it)
		}
	}
}
