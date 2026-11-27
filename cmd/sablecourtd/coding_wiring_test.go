package main

import (
	"bytes"
	"net"

	"errors"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/shadewright-labs/sablecourt/internal/attach"
	"github.com/shadewright-labs/sablecourt/internal/domain"
	"github.com/shadewright-labs/sablecourt/internal/harnessimages"
	scruntime "github.com/shadewright-labs/sablecourt/internal/runtime"
	"github.com/shadewright-labs/sablecourt/internal/runtime/coding"
	"github.com/shadewright-labs/sablecourt/internal/runtime/docker"
)

func TestCodingAdapterWiresPersistentState(t *testing.T) {
	for _, retention := range []time.Duration{0, 72 * time.Hour} {
		t.Run(retention.String(), func(t *testing.T) {
			state := t.TempDir()
			hub := attach.NewHub()
			adapter := newCodingAdapter(hub, filepath.Join(state, "sablecourt.db"), retention)
			if adapter.WorktreeRoot != filepath.Join(state, "worktrees") || adapter.BranchRetention != retention || adapter.AttachHub != hub {
				t.Fatalf("coding adapter root = %q, retention = %s, attach hub wired = %t",
					adapter.WorktreeRoot, adapter.BranchRetention, adapter.AttachHub == hub)
			}
		})
	}
}

// TestRegisterCodingAdapter checks host mode wiring. Outside Citadel a coding
// adapter is always registered so a host worker started before an upgrade can
// still be observed and stopped; without the opt-in (or without a working
// sandbox) that adapter refuses every new launch (ADR-0011 amendment of
// 2026-11-27, design note "Migration").
func TestRegisterCodingAdapter(t *testing.T) {
	sandboxErr := errors.New("no working coding sandbox on linux")
	for _, tc := range []struct {
		name         string
		isCitadel    bool
		unsafeHost   bool
		checkSandbox func() error
		wantAdapter  bool
		wantHost     bool
		wantLog      string
	}{
		{
			name:         "default configuration registers a recovery-only adapter",
			checkSandbox: func() error { return nil },
			wantAdapter:  true,
		},
		{
			name:         "explicit opt-in with working sandbox turns host mode on and logs the warning",
			unsafeHost:   true,
			checkSandbox: func() error { return nil },
			wantAdapter:  true,
			wantHost:     true,
			wantLog:      "outside invariants 2 and 3",
		},
		{
			name:         "opt-in without working sandbox stays recovery-only and logs reason",
			unsafeHost:   true,
			checkSandbox: func() error { return sandboxErr },
			wantAdapter:  true,
			wantLog:      sandboxErr.Error(),
		},
		{
			name:        "opt-in with nil sandbox checker stays recovery-only",
			unsafeHost:  true,
			wantAdapter: true,
			wantLog:     "sandbox availability check unavailable",
		},
		{
			name:         "citadel registers no coding adapter even with opt-in",
			isCitadel:    true,
			unsafeHost:   true,
			checkSandbox: func() error { return nil },
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			state := t.TempDir()
			hub := attach.NewHub()
			var logBuf bytes.Buffer
			logger := slog.New(slog.NewJSONHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelInfo}))
			adapters := map[domain.WorkloadClass]scruntime.Adapter{}

			registerCodingAdapter(adapters, tc.isCitadel, hub, filepath.Join(state, "sablecourt.db"), 24*time.Hour, tc.unsafeHost, tc.checkSandbox, logger)

			raw, ok := adapters[domain.ClassCodingAgent]
			if ok != tc.wantAdapter {
				t.Fatalf("registered = %t, want %t (log: %s)", ok, tc.wantAdapter, logBuf.String())
			}
			if tc.wantLog != "" && !strings.Contains(logBuf.String(), tc.wantLog) {
				t.Fatalf("log = %q, want substring %q", logBuf.String(), tc.wantLog)
			}
			if !ok {
				return
			}
			adapter, ok := raw.(*coding.Adapter)
			if !ok {
				t.Fatalf("adapter type = %T, want *coding.Adapter", raw)
			}
			if adapter.WorktreeRoot != filepath.Join(state, "worktrees") || adapter.BranchRetention != 24*time.Hour || adapter.AttachHub != hub ||
				adapter.HostModeUnsafe != tc.wantHost || adapter.Logger != logger || adapter.SupportsCouncilContainers() {
				t.Fatalf("unexpected adapter wiring: root=%q retention=%s hub=%t host=%t", adapter.WorktreeRoot, adapter.BranchRetention, adapter.AttachHub == hub, adapter.HostModeUnsafe)
			}
			wantMode := "unavailable"
			if tc.wantHost {
				wantMode = "host-unsafe"
			}
			if got := codingRuntime(adapters, tc.unsafeHost); got.Mode != wantMode || got.HostModeOptIn != tc.unsafeHost {
				t.Fatalf("coding runtime = %+v, want %s", got, wantMode)
			}
			if tc.wantHost {
				return
			}
			// Recovery only: a new launch is refused before any worktree or process exists.
			repo := t.TempDir()
			_, err := adapter.Start(t.Context(), scruntime.LaunchSpec{
				TaskID: "task-after-upgrade", TaskToken: "sc_task_" + "afterupgrade", InputJSON: `{"prompt":"edit"}`,
				Workload: domain.Workload{Spec: domain.WorkloadSpec{Class: domain.ClassCodingAgent, Coding: &domain.CodingConfig{Harness: "claude", RepoPath: repo, Sandbox: "seatbelt"}}},
			})
			if !errors.Is(err, coding.ErrHostModeOff) {
				t.Fatalf("Start on a recovery-only adapter = %v, want ErrHostModeOff", err)
			}
			if entries, _ := filepath.Glob(filepath.Join(state, "worktrees", "*")); len(entries) != 0 {
				t.Fatalf("refused launch left state: %v", entries)
			}
		})
	}
}

// TestUnsafeHostHarnessesFlagDefaultsOff pins the flag name and its default:
// host mode is never on unless the flag is passed.
func TestUnsafeHostHarnessesFlagDefaultsOff(t *testing.T) {
	f := newRootCmd().Flags().Lookup(unsafeHostHarnessesFlag)
	if f == nil || f.DefValue != "false" || !strings.Contains(f.Usage, "UNSAFE") || !strings.Contains(f.Usage, "outside invariants 2 and 3") {
		t.Fatalf("flag = %+v, want an UNSAFE opt-in that defaults to false", f)
	}
}

type fakeWiringEngine struct {
	docker.EngineClient
}

func TestWireCouncilContainers(t *testing.T) {
	state := t.TempDir()
	hub := attach.NewHub()
	da, err := docker.NewAdapter(fakeWiringEngine{}, "")
	if err != nil {
		t.Fatal(err)
	}
	adapters := map[domain.WorkloadClass]scruntime.Adapter{}

	// Even when host sandbox is unavailable, Sanctum registers coding adapter for container councils.
	wireCouncilContainers(adapters, false, da, hub, filepath.Join(state, "sablecourt.db"), time.Hour, net.JoinHostPort(net.IPv6loopback.String(), "7432"))

	ca, ok := adapters[domain.ClassCodingAgent].(*coding.Adapter)
	if !ok || ca == nil {
		t.Fatalf("expected *coding.Adapter registered, got %T", adapters[domain.ClassCodingAgent])
	}
	if !ca.SupportsCouncilContainers() || ca.CouncilRunner != da || ca.CouncilProxy != da || ca.CouncilProxyImage != harnessimages.ProxyImage() || ca.DaemonListen != net.JoinHostPort(net.IPv6loopback.String(), "7432") {
		t.Fatalf("unexpected council container wiring: runner=%v proxy=%v img=%q listen=%q", ca.CouncilRunner == da, ca.CouncilProxy == da, ca.CouncilProxyImage, ca.DaemonListen)
	}
	for _, role := range []string{"claude", "codex", "agy"} {
		def, err := harnessimages.Lookup(role)
		if err != nil {
			t.Fatal(err)
		}
		got, ok := da.CouncilHarnesses[role]
		if !ok || got.Image != def.Image || got.LoginVolume != def.Volume {
			t.Fatalf("harness %s config = %+v, want image=%q volume=%q", role, got, def.Image, def.Volume)
		}
		if (def.Tool == nil) != (got.Tool == nil) {
			t.Fatalf("harness %s tool = %+v", role, got.Tool)
		}
		if got.Tool != nil && (got.Tool.Volume != def.Tool.Volume || got.Tool.SHA256 != def.Tool.SHA256 || got.Tool.Version != def.Tool.Version ||
			got.Tool.InstallCommand != harnessimages.InstallCommand(role) || got.Tool.Volume == got.LoginVolume) {
			t.Fatalf("harness %s tool = %+v", role, got.Tool)
		}
	}

	if got := codingRuntime(adapters, false); got.Mode != "container" || got.HostModeOptIn {
		t.Fatalf("coding runtime = %+v, want container", got)
	}
	if ca.HostModeUnsafe {
		t.Fatal("container wiring must not turn host mode on")
	}

	// With the opt-in, containers still win: the host adapter gains the runner.
	hostAdapters := map[domain.WorkloadClass]scruntime.Adapter{}
	registerCodingAdapter(hostAdapters, false, hub, filepath.Join(state, "sablecourt.db"), time.Hour, true, func() error { return nil }, nil)
	wireCouncilContainers(hostAdapters, false, da, hub, filepath.Join(state, "sablecourt.db"), time.Hour, net.JoinHostPort(net.IPv6loopback.String(), "7432"))
	if got := codingRuntime(hostAdapters, true); got.Mode != "container" || !got.HostModeOptIn {
		t.Fatalf("coding runtime with opt-in and containers = %+v, want container", got)
	}

	// Citadel must not wire local council containers.
	citadelAdapters := map[domain.WorkloadClass]scruntime.Adapter{}
	wireCouncilContainers(citadelAdapters, true, da, hub, filepath.Join(state, "sablecourt.db"), time.Hour, net.JoinHostPort(net.IPv6loopback.String(), "7432"))
	if citadelAdapters[domain.ClassCodingAgent] != nil {
		t.Fatal("citadel unexpectedly registered council coding adapter")
	}
}
