package main

import (
	"bytes"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/shadewright-labs/sablecourt/internal/config"
	"github.com/shadewright-labs/sablecourt/internal/controlplane"
	"github.com/shadewright-labs/sablecourt/internal/domain"
	mcpgate "github.com/shadewright-labs/sablecourt/internal/gate/mcp"
	"github.com/shadewright-labs/sablecourt/internal/policy"
	"github.com/shadewright-labs/sablecourt/internal/redact"
	"github.com/shadewright-labs/sablecourt/internal/secret"
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

// A reloaded bundle naming an MCP server secret too short to redact is
// refused before anything is written or swapped: the reload returns a
// refusal naming the setting (never the value), apply never runs, and the
// store keeps the previous workloads (persist runs before the gate step, so
// the gate is not reached either). With a secret long enough the same reload is accepted,
// and the secret is registered for exact redaction before the store write,
// the gate swap and apply, so no step can see the new grants first.
func TestReloadWithShortMCPSecretKeepsPreviousState(t *testing.T) {
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

	// Shorter than redact.MinSecretLength.
	shortSecret := "s7" + "x9q"
	longSecret := synthDaemonSecret("reload", "_mcp_", "55117799")
	mem := secret.NewMemoryStore(map[string]string{"short_mcp_tok": shortSecret, "long_mcp_tok": longSecret})
	secretStore := secret.NewObservingStore(mem, registerDaemonSecret)
	gate := mcpgate.New(mcpgate.Options{Store: st, Bundle: b1, Policy: pol1})
	if err := gate.SetUpstreamBridge(mcpgate.NewUpstreamBridge(secretStore, nil)); err != nil {
		t.Fatal(err)
	}
	binding := mcpgate.RegisterBuiltinGitOps(gate, dir, nil)

	var logBuf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logBuf, nil))
	applied := false
	registeredAtPersist := false
	steps := daemonReloadSteps(ctx, logger,
		func() (*config.Bundle, *policy.Engine, error) { return loader.Load(ctx) },
		func(b *config.Bundle) error {
			registeredAtPersist = !strings.Contains(redact.String("v="+longSecret), longSecret)
			return recordWorkloads(ctx, st, b, false)
		},
		secretStore, gate, binding,
		func(*config.Bundle, *policy.Engine) { applied = true })

	m2 := func(secretName string) string {
		return `apiVersion: sablecourt.io/v1alpha1
kind: Workload
metadata: {name: alpha, zone: runtime}
spec:
  class: long-lived-agent
  runtime: {type: docker, image: "alpine:3.21"}
  grants:
    mcpTools: [ext.read]
    mcpServers:
      - name: ext
        url: "https://mcp.example.test/mcp"
        secretName: ` + secretName + `
        allowedTools: [read]
`
	}
	if err := os.WriteFile(manifests, []byte(m2("short_mcp_tok")), 0o600); err != nil {
		t.Fatal(err)
	}
	err = steps.run()
	if err == nil || !strings.HasPrefix(err.Error(), "reload refused") || !strings.Contains(err.Error(), "short_mcp_tok") {
		t.Fatalf("run = %v, want reload refused naming short_mcp_tok", err)
	}
	if strings.Contains(err.Error(), shortSecret) || strings.Contains(logBuf.String(), shortSecret) {
		t.Fatal("refusal leaked the short secret value")
	}
	if !strings.Contains(logBuf.String(), "manifest reload refused; previous bundle, policy, upstream tools and stored workloads kept") {
		t.Fatalf("log does not say the reload was refused:\n%s", logBuf.String())
	}
	if applied {
		t.Fatal("apply ran after a short MCP secret was refused")
	}
	items, err := st.ListWorkloads(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].Zone != domain.ZoneSandbox || items[0].ManifestDigest != b1.ManifestDigest {
		t.Fatalf("refused reload mutated store workloads: %+v (want sandbox alpha digest %s)", items, b1.ManifestDigest)
	}

	if err := os.WriteFile(manifests, []byte(m2("long_mcp_tok")), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := steps.run(); err != nil {
		t.Fatalf("accepted reload: %v", err)
	}
	if !applied || !registeredAtPersist {
		t.Fatalf("accepted reload: applied=%v, secret registered before persist=%v", applied, registeredAtPersist)
	}
}

// apply runs after the gate swap and cannot refuse, so a scheduler that
// fails to take the new schedules is logged, not dropped. The loader refuses
// an invalid cron expression, so only a bundle that skipped it gets here.
func TestReloadSchedulesLogsFailure(t *testing.T) {
	var logBuf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logBuf, nil))
	b := &config.Bundle{Schedules: map[string]domain.Schedule{
		"nightly": {Metadata: domain.ObjectMeta{Name: "nightly"}, Spec: domain.ScheduleSpec{Cron: "not a cron", Enabled: true}},
	}}
	s := controlplane.NewScheduler(nil)
	t.Cleanup(s.Stop)
	reloadSchedules(t.Context(), logger, s, b)
	if !strings.Contains(logBuf.String(), "schedules not reloaded") || !strings.Contains(logBuf.String(), "nightly") {
		t.Fatalf("scheduler failure not logged:\n%s", logBuf.String())
	}
	logBuf.Reset()
	reloadSchedules(t.Context(), logger, s, &config.Bundle{})
	if logBuf.Len() != 0 {
		t.Fatalf("accepted schedules logged an error:\n%s", logBuf.String())
	}
}
