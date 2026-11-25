package conclave

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/shadewright-labs/sablecourt/internal/controlplane"
	"github.com/shadewright-labs/sablecourt/internal/domain"
	rt "github.com/shadewright-labs/sablecourt/internal/runtime"
	"github.com/shadewright-labs/sablecourt/internal/runtime/coding"
	"github.com/shadewright-labs/sablecourt/internal/store"
)

func TestLiveCouncilStubCLI(t *testing.T) {
	for _, tc := range []struct {
		name       string
		sequential bool
	}{{"parallel", false}, {"sequential", true}} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			repo := t.TempDir()
			for _, args := range [][]string{
				{"init", "-q"},
				{"-c", "user.name=Fixture", "-c", "user.email=fixture", "-c", "commit.gpgsign=false", "-c", "core.hooksPath=", "commit", "-qm", "base", "--allow-empty"},
			} {
				cmd := exec.CommandContext(ctx, "git", args...)
				cmd.Dir = repo
				if out, err := cmd.CombinedOutput(); err != nil {
					t.Fatal(err, string(out))
				}
			}
			if err := os.WriteFile(filepath.Join(repo, "fixture"), []byte("uncommitted\n"), 0600); err != nil {
				t.Fatal(err)
			}
			const script = `#!/bin/sh
read -r value < fixture || exit 1
[ "$value" = uncommitted ] || exit 2
if (echo changed > fixture) 2>/dev/null; then exit 3; fi
echo "Saw $value in a read-only checkout"
`
			harness := filepath.Join(t.TempDir(), "claude")
			if err := os.WriteFile(harness, []byte(script), 0700); err != nil {
				t.Fatal(err)
			}
			f := driverRoomWithCheckout(t, tc.sequential, 6, 6, store.CouncilCheckout{Source: repo})
			w := f.bundle.Workloads["worker"]
			w.Spec.Class = domain.ClassCodingAgent
			w.Spec.Coding = &domain.CodingConfig{Harness: "claude", RepoPath: repo, Sandbox: "seatbelt"}
			f.bundle.Workloads["worker"] = w
			state := t.TempDir()
			t.Setenv("SABLECOURT_STATE_DIR", state)
			a := coding.NewAdapter(filepath.Join(state, "worktrees"))
			a.HarnessPaths = map[string]string{"claude": harness}
			rec := controlplane.NewReconciler(controlplane.Options{Store: f.Store, Bundle: f.bundle, Adapters: map[domain.WorkloadClass]rt.Adapter{w.Spec.Class: a}})
			f.Broker.reconciler = rec
			t.Cleanup(func() {
				ctx := context.Background()
				s, err := f.CouncilSnapshot(ctx, "room")
				if err == nil && !councilClosed(&s) {
					if err := f.Broker.Close(ctx, "room", domain.ConclaveStateCancelled); err != nil {
						t.Error(err)
					}
				}
			})
			for deadline := time.Now().Add(time.Minute); time.Now().Before(deadline); {
				f.pass(t)
				if err := rec.ReconcileOnce(ctx); err != nil {
					if probe := coding.CheckSandbox(ctx); probe != nil {
						t.Fatalf("%v (Seatbelt probe: %v)", err, probe)
					}
					t.Fatal(err)
				}
				s, err := f.CouncilSnapshot(ctx, "room")
				if err != nil {
					t.Fatal(err)
				}
				if councilClosed(&s) {
					if len(s.Turns) != 6 || s.Session.WardReason != "round_cap" {
						t.Fatalf("closed with %d turns: %s", len(s.Turns), s.Session.WardReason)
					}
					for _, turn := range s.Turns {
						if turn.Answer == nil || !strings.Contains(turn.Answer.Body, "Saw uncommitted in a read-only checkout") {
							t.Fatalf("missing checkout evidence for %s in round %d", turn.Role, turn.Round)
						}
					}
					return
				}
				time.Sleep(10 * time.Millisecond)
			}
			t.Fatal("council did not finish")
		})
	}
}
