package main

import (
	"bytes"
	"errors"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/shadewright-labs/sablecourt/internal/attach"
	"github.com/shadewright-labs/sablecourt/internal/domain"
	scruntime "github.com/shadewright-labs/sablecourt/internal/runtime"
	"github.com/shadewright-labs/sablecourt/internal/runtime/coding"
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

func TestRegisterCodingAdapter(t *testing.T) {
	sandboxErr := errors.New("no working coding sandbox on linux")
	for _, tc := range []struct {
		name           string
		isCitadel      bool
		checkSandbox   func() error
		wantRegistered bool
		wantLog        string
	}{
		{
			name:           "sanctum with working sandbox registers adapter",
			isCitadel:      false,
			checkSandbox:   func() error { return nil },
			wantRegistered: true,
		},
		{
			name:           "sanctum without working sandbox fails closed and logs reason",
			isCitadel:      false,
			checkSandbox:   func() error { return sandboxErr },
			wantRegistered: false,
			wantLog:        sandboxErr.Error(),
		},
		{
			name:           "sanctum with nil sandbox checker fails closed",
			isCitadel:      false,
			checkSandbox:   nil,
			wantRegistered: false,
			wantLog:        "sandbox availability check unavailable",
		},
		{
			name:           "citadel keeps host coding adapter unregistered",
			isCitadel:      true,
			checkSandbox:   func() error { return nil },
			wantRegistered: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			state := t.TempDir()
			hub := attach.NewHub()
			var logBuf bytes.Buffer
			logger := slog.New(slog.NewJSONHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelInfo}))
			adapters := map[domain.WorkloadClass]scruntime.Adapter{}

			registerCodingAdapter(adapters, tc.isCitadel, hub, filepath.Join(state, "sablecourt.db"), 24*time.Hour, tc.checkSandbox, logger)

			raw, ok := adapters[domain.ClassCodingAgent]
			if ok != tc.wantRegistered {
				t.Fatalf("registered = %t, want %t (log: %s)", ok, tc.wantRegistered, logBuf.String())
			}
			if tc.wantRegistered {
				adapter, ok := raw.(*coding.Adapter)
				if !ok {
					t.Fatalf("adapter type = %T, want *coding.Adapter", raw)
				}
				if adapter.WorktreeRoot != filepath.Join(state, "worktrees") || adapter.BranchRetention != 24*time.Hour || adapter.AttachHub != hub {
					t.Fatalf("unexpected adapter wiring: root=%q retention=%s hub=%t", adapter.WorktreeRoot, adapter.BranchRetention, adapter.AttachHub == hub)
				}
				if logBuf.Len() != 0 {
					t.Fatalf("unexpected log output when sandbox works: %s", logBuf.String())
				}
				return
			}
			if tc.wantLog != "" && !strings.Contains(logBuf.String(), tc.wantLog) {
				t.Fatalf("log = %q, want substring %q", logBuf.String(), tc.wantLog)
			}
		})
	}
}
