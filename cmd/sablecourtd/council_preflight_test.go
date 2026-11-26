package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/shadewright-labs/sablecourt/internal/config"
	"github.com/shadewright-labs/sablecourt/internal/domain"
	scruntime "github.com/shadewright-labs/sablecourt/internal/runtime"
	"github.com/shadewright-labs/sablecourt/internal/runtime/docker"
	"github.com/shadewright-labs/sablecourt/internal/store"
)

func TestCouncilHarnessRefusal(t *testing.T) {
	for _, tc := range []struct {
		name    string
		err     error
		status  int
		want    []string
		notWant []string
	}{
		{"image missing", &docker.HarnessError{Harness: "codex", Fault: docker.FaultImageMissing}, http.StatusPreconditionFailed,
			[]string{"image for codex is missing", "sablectl harness build codex"}, []string{"login"}},
		{"login missing", &docker.HarnessError{Harness: "agy", Fault: docker.FaultLoginMissing}, http.StatusPreconditionFailed,
			[]string{"login for agy is missing", "sablectl harness login agy"}, []string{"build"}},
		{"volume missing", &docker.HarnessError{Harness: "claude", Fault: docker.FaultVolumeMissing}, http.StatusPreconditionFailed,
			[]string{"sablectl harness login claude"}, nil},
		{"volume without labels", &docker.HarnessError{Harness: "claude", Fault: docker.FaultVolumeInvalid}, http.StatusConflict,
			[]string{"not a local Sablecourt volume", "sablectl harness login claude"}, nil},
		{"tool missing", &docker.HarnessError{Harness: "claude", Fault: docker.FaultToolMissing}, http.StatusPreconditionFailed,
			[]string{"tool for claude is not installed", "sablectl harness install claude"}, []string{"build"}},
		{"tool wrong version", &docker.HarnessError{Harness: "agy", Fault: docker.FaultToolVersion}, http.StatusPreconditionFailed,
			[]string{"not the pinned version", "sablectl harness install agy"}, nil},
		{"tool checksum mismatch", &docker.HarnessError{Harness: "claude", Fault: docker.FaultToolChecksum}, http.StatusPreconditionFailed,
			[]string{"pinned SHA-256", "sablectl harness install claude"}, nil},
		{"tool volume without labels", &docker.HarnessError{Harness: "claude", Fault: docker.FaultToolInvalid}, http.StatusConflict,
			[]string{"not a local Sablecourt tool volume", "sablectl harness install claude"}, nil},
		{"backend", &docker.HarnessError{Harness: "claude", Fault: docker.FaultBackend, Err: errors.New("token=sk-secret-value")}, http.StatusServiceUnavailable,
			[]string{"runtime failed"}, []string{"sablectl", "secret"}},
		{"unknown error", errors.New("surprise sk-secret-value"), http.StatusServiceUnavailable,
			[]string{"preflight failed"}, []string{"sablectl", "secret"}},
		{"timeout", fmt.Errorf("probe: %w", context.DeadlineExceeded), http.StatusServiceUnavailable,
			[]string{"timed out"}, []string{"sablectl"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			role := "claude"
			he := &docker.HarnessError{}
			if errors.As(tc.err, &he) {
				role = he.Harness
			}
			status, msg := councilHarnessRefusal(role, tc.err)
			if status != tc.status {
				t.Fatalf("status = %d, want %d", status, tc.status)
			}
			for _, w := range tc.want {
				if !strings.Contains(msg, w) {
					t.Errorf("message %q lacks %q", msg, w)
				}
			}
			for _, w := range tc.notWant {
				if strings.Contains(msg, w) {
					t.Errorf("message %q must not contain %q", msg, w)
				}
			}
		})
	}
}

func TestCouncilOpenContainerPreflight(t *testing.T) {
	repo, bin, token := councilTestInputs(t)
	body := fmt.Sprintf(`{"question":"Review this","repo":%s,"ref":"HEAD"}`, strconv.Quote(repo))
	// No host CLI is needed in container mode.
	for _, role := range councilPanel {
		if err := os.Remove(bin + "/" + role); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		name    string
		failing string
		fault   docker.HarnessFault
		status  int
		message string
	}{
		{name: "all present", status: http.StatusCreated},
		{name: "image missing", failing: "codex", fault: docker.FaultImageMissing, status: 412, message: "sablectl harness build codex"},
		{name: "login missing", failing: "agy", fault: docker.FaultLoginMissing, status: 412, message: "sablectl harness login agy"},
		{name: "no volume", failing: "claude", fault: docker.FaultVolumeMissing, status: 412, message: "sablectl harness login claude"},
		{name: "unlabelled volume", failing: "claude", fault: docker.FaultVolumeInvalid, status: 409, message: "not a local Sablecourt volume"},
		{name: "engine down", failing: "claude", fault: docker.FaultBackend, status: 503, message: "runtime failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := daemonTestStore(t)
			var checked []string
			opts := councilTestOptions(token)
			opts.checkCouncilSandbox = func(context.Context) error {
				t.Error("host sandbox check ran in container mode")
				return errors.New("unexpected")
			}
			opts.preflightCouncilHarness = func(_ context.Context, role string) error {
				checked = append(checked, role)
				if role == tc.failing {
					return &docker.HarnessError{Harness: role, Fault: tc.fault}
				}
				return nil
			}
			opts.checkCouncilRuntime = func(context.Context, store.CouncilCheckout) error {
				if tc.failing != "" {
					t.Error("refused request reached runtime admission")
				}
				return nil
			}
			h := councilTestHandler(st, nil, opts)
			resp := councilTestRequest(h, http.MethodPost, "/api/v1/councils", token, body)
			if resp.Code != tc.status || !strings.Contains(resp.Body.String(), tc.message) {
				t.Fatalf("POST = %d %s, want %d containing %q", resp.Code, resp.Body, tc.status, tc.message)
			}
			if tc.failing != "" {
				councilAssertEmpty(t, st)
				if strings.Contains(resp.Body.String(), "secret") {
					t.Fatal("refusal leaked detail")
				}
			} else if len(checked) != len(councilPanel) {
				t.Fatalf("checked %v, want every role", checked)
			}
		})
	}
}

func TestCouncilPanelWorkloadsFollowMode(t *testing.T) {
	for _, tc := range []struct {
		container      bool
		kind, mismatch string
	}{{true, "container", "seatbelt"}, {false, "seatbelt", "container"}} {
		t.Run(tc.kind, func(t *testing.T) {
			if councilSandbox(tc.container) != tc.kind {
				t.Fatalf("councilSandbox(%v) = %q", tc.container, councilSandbox(tc.container))
			}
			bundle, err := ensureCouncilPanelWorkloads(t.Context(), daemonTestStore(t), nil, tc.kind)
			if err != nil {
				t.Fatal(err)
			}
			adapters := map[domain.WorkloadClass]scruntime.Adapter{domain.ClassCodingAgent: &councilTestAdapter{}}
			get := func() *config.Bundle { return bundle }
			if err := councilRuntimeChecker(get, adapters, tc.kind)(t.Context(), store.CouncilCheckout{}); err != nil {
				t.Fatal(err)
			}
			if err := councilRuntimeChecker(get, adapters, tc.mismatch)(t.Context(), store.CouncilCheckout{}); err == nil {
				t.Fatal("a workload of the other mode passed the check")
			}
		})
	}
}

// The checkout deadline must not cut short the checks that follow it: a slow
// but successful harness preflight still has to reach admission.
func TestCouncilOpenAdmissionOutlivesCheckoutDeadline(t *testing.T) {
	repo, _, token := councilTestInputs(t)
	body := fmt.Sprintf(`{"question":"Review this","repo":%s,"ref":"HEAD"}`, strconv.Quote(repo))
	st := daemonTestStore(t)
	opts := councilTestOptions(token)
	opts.onCouncilCheckout = func(expire context.CancelFunc) { expire() }
	opts.preflightCouncilHarness = func(ctx context.Context, _ string) error { return ctx.Err() }
	opts.checkCouncilRuntime = func(ctx context.Context, _ store.CouncilCheckout) error { return ctx.Err() }
	resp := councilTestRequest(councilTestHandler(st, nil, opts), http.MethodPost, "/api/v1/councils", token, body)
	if resp.Code != http.StatusCreated {
		t.Fatalf("POST = %d %s", resp.Code, resp.Body)
	}
}
