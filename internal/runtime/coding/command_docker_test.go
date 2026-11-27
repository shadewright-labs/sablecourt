package coding

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"net/http"
	"net/netip"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/containerd/errdefs"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"
	"github.com/shadewright-labs/sablecourt/internal/domain"
	"github.com/shadewright-labs/sablecourt/internal/gate/councilproxy"
	"github.com/shadewright-labs/sablecourt/internal/harnessimages"
	scruntime "github.com/shadewright-labs/sablecourt/internal/runtime"
	"github.com/shadewright-labs/sablecourt/internal/runtime/docker"
)

// commandProbe runs inside the command container. Each guard of the
// decision table exits with its own code when it does not hold, so a failure
// names the broken guard. It prints the checked-out commit and a fake token.
const commandProbe = `set -u
if IFS= read -r line; then exit 31; fi
[[ $(id -u) == 10001 && $(id -g) == 10001 ]] || exit 32
grep -Eq '^CapEff:[[:space:]]*0+$' /proc/self/status || exit 33
grep -Eq '^NoNewPrivs:[[:space:]]*1$' /proc/self/status || exit 34
awk '$5 == "/" && $6 ~ /^ro(,|$)/ {f=1} END{exit !f}' /proc/self/mountinfo || exit 35
awk '$5 == "/workspace" && $6 ~ /^ro(,|$)/ {f=1} END{exit !f}' /proc/self/mountinfo || exit 36
if awk '$5 == "/home/harness" {f=1} END{exit !f}' /proc/self/mountinfo; then exit 37; fi
[[ $HOME == /scratch ]] || exit 38
[[ -z ${SABLECOURT_TASK_TOKEN+x} && -z ${SABLECOURT_TASK_TOKEN_FILE+x} && -z ${SABLECOURT_MODEL_GATE_URL+x} ]] || exit 39
if touch /workspace/write-attempt 2>/dev/null; then exit 40; fi
if curl -sS --max-time 5 -o /dev/null https://api.anthropic.com/ 2>/dev/null; then exit 41; fi
code=$(curl -sS --max-time 5 -o /dev/null -w '%{http_code}' "$SABLECOURT_DATA_GATE_URL")
[[ $code == 401 ]] || exit 42
git -C /workspace rev-parse HEAD || exit 43
printf 'token ghp_%s\n' "$(printf 'R%.0s' $(seq 36))"
exit "$1"
`

// buildCurrentProxyImage layers the proxy compiled from this checkout onto
// the local proxy image, so the test runs the command profile without
// retagging the operator's images.
func buildCurrentProxyImage(t *testing.T, cli *client.Client, baseID string) string {
	t.Helper()
	binary, err := harnessimages.LocalProxyCompiler{Source: filepath.Join("..", "..", "..")}.Compile(t.Context())
	if err != nil {
		t.Skipf("cannot compile the council proxy: %v", err)
	}
	stage, err := cli.ContainerCreate(t.Context(), client.ContainerCreateOptions{Config: &container.Config{Image: baseID}, HostConfig: &container.HostConfig{NetworkMode: "none"}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, _ = cli.ContainerRemove(ctx, stage.ID, client.ContainerRemoveOptions{Force: true})
	})
	var payload bytes.Buffer
	tw := tar.NewWriter(&payload)
	if err := tw.WriteHeader(&tar.Header{Name: "council-proxy", Mode: 0o755, Size: int64(len(binary))}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(binary); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := cli.CopyToContainer(t.Context(), stage.ID, client.CopyToContainerOptions{DestinationPath: "/usr/local/bin", Content: &payload}); err != nil {
		t.Fatal(err)
	}
	img, err := cli.ContainerCommit(t.Context(), stage.ID, client.ContainerCommitOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, _ = cli.ImageRemove(ctx, img.ID, client.ImageRemoveOptions{Force: true})
	})
	return img.ID
}

// TestCommandStepDocker runs command steps through the real Docker adapter
// and council proxy (issue 134): the command runs as uid 10001 with no
// capabilities, a read-only root and checkout, no login volume, no task
// lease and nothing on stdin; the proxy refuses every provider and the gate
// refuses a caller without the lease. The result is recorded redacted, and
// the container, proxy and network are removed.
func TestCommandStepDocker(t *testing.T) {
	da, err := docker.NewFromEnv()
	if err != nil {
		t.Skipf("Docker client unavailable: %v", err)
	}
	t.Cleanup(func() { _ = da.Close() })
	cli, err := client.New(client.WithHost(da.DaemonHost()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cli.Close() })
	pingCtx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	err = da.Ping(pingCtx)
	cancel()
	if err != nil {
		t.Skipf("Docker engine unavailable: %v", err)
	}
	baseProxyID, err := da.ResolveLocalImage(t.Context(), harnessimages.ProxyImage())
	if err != nil {
		t.Skipf("local council proxy image unavailable (run sablectl harness build): %v", err)
	}
	proxyImageID := buildCurrentProxyImage(t, cli, baseProxyID)
	stubImageID := buildStubHarnessImage(t, cli)

	listen := netip.AddrPortFrom(netip.AddrFrom4([4]byte{127, 0, 0, 1}), 7432).String()
	wantAuthority, err := councilproxy.GateAuthority(listen)
	if err != nil {
		t.Fatal(err)
	}
	gateHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Host != wantAuthority || !councilproxy.AllowsGateRequest(r) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	// The login volume is named but never created: a command must not need it.
	da.CouncilHarnesses = map[string]docker.CouncilHarness{"claude": {Image: stubImageID, LoginVolume: "sablecourt-test-unused-" + strings.ToLower(rand.Text()[:8])}}

	repo := initTempGitRepo(t)
	testWrite(t, repo, "committed.txt", "committed-v1\n", 0o644)
	testGit(t, repo, "add", "committed.txt")
	testGit(t, repo, "commit", "-m", "v1")
	commit := strings.TrimSpace(testGit(t, repo, "rev-parse", "HEAD"))

	adapter := NewAdapter(filepath.Join(t.TempDir(), "worktrees"))
	adapter.CouncilRunner = da
	wireMountGuard(t, adapter, da)
	adapter.CouncilProxy = da
	adapter.CouncilProxyImage = proxyImageID
	adapter.DaemonListen = listen
	adapter.CouncilGateHandler = gateHandler
	adapter.CouncilSocketRoot = integrationSocketRoot(t)
	adapter.CouncilLimits = scruntime.CouncilLimits{NanoCPUs: 500_000_000, MemoryBytes: 128 << 20, PIDs: 64, WallTime: 30 * time.Second}

	for _, tc := range []struct {
		name, exit string
		state      scruntime.ObservedState
		outcome    string
		exitCode   int
	}{
		{name: "passed", exit: "0", state: scruntime.StateCompleted, outcome: scruntime.CommandPassed},
		{name: "failed", exit: "3", state: scruntime.StateFailed, outcome: scruntime.CommandFailed, exitCode: 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			taskID := "task_cmd_" + tc.name + "_" + strings.ToLower(rand.Text()[:6])
			spec := scruntime.LaunchSpec{
				TaskID:           taskID,
				Workload:         domain.Workload{Metadata: domain.ObjectMeta{Name: "coder", Zone: domain.ZoneRuntime}, Spec: domain.WorkloadSpec{Class: domain.ClassCodingAgent, Coding: &domain.CodingConfig{Harness: "claude", RepoPath: repo, Sandbox: "container"}}},
				InputJSON:        `{"command":"check"}`,
				TaskToken:        "sc_task_" + rand.Text(),
				CouncilContainer: true,
				Command: &scruntime.CommandSpec{Name: "check", Argv: []string{"/bin/bash", "--noprofile", "--norc", "-c", commandProbe, "check", tc.exit},
					Timeout: 30 * time.Second, MaxOutputBytes: domain.DefaultCommandOutputBytes},
			}
			h, err := adapter.StartAtCommit(t.Context(), spec, commit, true)
			if isContainerHandle(h) {
				t.Cleanup(func() {
					ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
					defer cancel()
					_ = adapter.Stop(ctx, h)
				})
			}
			if err != nil {
				t.Fatalf("StartAtCommit: %v", err)
			}
			taskBase, err := adapter.taskBase(taskID)
			if err != nil {
				t.Fatal(err)
			}
			<-adapter.containerProc(taskBase).done
			obs, err := adapter.Observe(t.Context(), h)
			if err != nil {
				t.Fatal(err)
			}
			arts, err := adapter.CollectArtifacts(t.Context(), h)
			if err != nil {
				t.Fatal(err)
			}
			var res scruntime.CommandResult
			for _, a := range arts {
				if a.Name == scruntime.CommandResultArtifact {
					if err := json.Unmarshal([]byte(a.Text), &res); err != nil {
						t.Fatal(err)
					}
				}
				if a.Name == "session-log" || a.Name == "final-message" {
					t.Fatalf("a command step stored a %s artifact", a.Name)
				}
			}
			if obs.State != tc.state || res.Outcome != tc.outcome || res.ExitCode != tc.exitCode {
				t.Fatalf("obs %+v, result %+v; want %s with outcome %s exit %d", obs, res, tc.state, tc.outcome, tc.exitCode)
			}
			if !strings.Contains(res.Output, commit) || strings.Contains(res.Output, "ghp_RRRR") || !strings.Contains(res.Output, "token [REDACTED") {
				t.Fatalf("output = %q, want the pinned commit and a redacted token", res.Output)
			}
			for kind, name := range map[string]string{"container": h.ContainerID, "proxy": h.ProxyName} {
				if _, err := cli.ContainerInspect(t.Context(), name, client.ContainerInspectOptions{}); !errdefs.IsNotFound(err) {
					t.Fatalf("command %s %s is not removed: %v", kind, name, err)
				}
			}
			if _, err := cli.NetworkInspect(t.Context(), h.NetworkName, client.NetworkInspectOptions{}); !errdefs.IsNotFound(err) {
				t.Fatalf("command network %s is not removed: %v", h.NetworkName, err)
			}
		})
	}
}
