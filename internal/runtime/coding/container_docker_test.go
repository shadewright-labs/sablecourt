package coding

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"io"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"
	"github.com/shadewright-labs/sablecourt/internal/domain"
	"github.com/shadewright-labs/sablecourt/internal/gate/councilproxy"
	"github.com/shadewright-labs/sablecourt/internal/harnessimages"
	scruntime "github.com/shadewright-labs/sablecourt/internal/runtime"
	"github.com/shadewright-labs/sablecourt/internal/runtime/docker"
)

const stubContainerCLI = `#!/bin/bash
set -eu
if IFS= read -r line; then exit 31; fi
[[ $(id -u) == 10001 ]]
[[ $(id -g) == 10001 ]]
grep -Eq '^CapEff:[[:space:]]*0+$' /proc/self/status
grep -Eq '^NoNewPrivs:[[:space:]]*1$' /proc/self/status
awk '$5 == "/" && $6 ~ /^ro(,|$)/ {found=1} END{exit !found}' /proc/self/mountinfo
[[ -z ${SABLECOURT_MODEL_GATE_URL+x} ]]
[[ -z ${SABLECOURT_TASK_TOKEN+x} && -r "$SABLECOURT_TASK_TOKEN_FILE" ]]
[[ -n ${SABLECOURT_DATA_GATE_URL:-} ]]
touch /scratch/probe

# Reach pinned gate via proxy with scoped task lease.
code=$(printf 'Authorization: Bearer %s\n' "$(cat "$SABLECOURT_TASK_TOKEN_FILE")" | curl -sS --max-time 5 -o /dev/null -w '%{http_code}' -H @- "${SABLECOURT_DATA_GATE_URL}")
[[ "$code" == "200" ]]

# Reject unauthenticated call to proxy gate.
unauth=$(curl -sS --max-time 5 -o /dev/null -w '%{http_code}' "${SABLECOURT_DATA_GATE_URL}")
[[ "$unauth" == "401" ]]

prompt="${!#}"
case "$prompt" in
  *MODE_LIVE*)
    grep -q "committed-v1" /workspace/committed.txt
    grep -q "uncommitted-v1" /workspace/uncommitted.txt
    if touch /workspace/write-attempt 2>/dev/null; then exit 41; fi
    printf 'live review complete\n'
    ;;
  *MODE_REF*)
    grep -q "committed-v1" /workspace/committed.txt
    if [ -e /workspace/uncommitted.txt ]; then exit 42; fi
    if touch /workspace/write-attempt 2>/dev/null; then exit 43; fi
    printf 'ref review complete\n'
    ;;
  *MODE_REVIEW*)
    grep -q "committed-v1" /workspace/committed.txt
    if touch /workspace/write-attempt 2>/dev/null; then exit 44; fi
    printf 'reviewer verdict\n'
    ;;
  *MODE_WRITER*)
    grep -q "committed-v1" /workspace/committed.txt
    printf 'implemented in container\n' > /workspace/feature.txt
    printf 'writer done\n'
    ;;
  *)
    printf 'unexpected prompt\n' >&2
    exit 45
    ;;
esac
`

func buildStubHarnessImage(t *testing.T, cli *client.Client) string {
	t.Helper()
	var baseID string
	for _, ref := range []string{harnessimages.ProxyImage(), "sablecourt-spike-base:local"} {
		img, err := cli.ImageInspect(t.Context(), ref)
		if err == nil && img.ID != "" {
			baseID = img.ID
			break
		}
	}
	if baseID == "" {
		t.Skip("local Debian harness base with curl and bash unavailable; build with 'sablectl harness build claude'")
	}
	stage, err := cli.ContainerCreate(t.Context(), client.ContainerCreateOptions{
		Config:     &container.Config{Image: baseID, Cmd: []string{"/bin/true"}},
		HostConfig: &container.HostConfig{NetworkMode: "none"},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, _ = cli.ContainerRemove(ctx, stage.ID, client.ContainerRemoveOptions{Force: true, RemoveVolumes: true})
	})
	var payload bytes.Buffer
	tw := tar.NewWriter(&payload)
	for _, name := range []string{"claude", "codex", "agy"} {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(stubContainerCLI))}); err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(tw, stubContainerCLI); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := cli.CopyToContainer(t.Context(), stage.ID, client.CopyToContainerOptions{
		DestinationPath: "/usr/local/bin",
		Content:         &payload,
	}); err != nil {
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

func TestCouncilContainerDockerMountModesAndDiff(t *testing.T) {
	da, err := docker.NewFromEnv()
	if err != nil {
		t.Skipf("Docker client unavailable: %v", err)
	}
	cli := da.Client.(*client.Client)
	t.Cleanup(func() { _ = cli.Close() })
	pingCtx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	err = da.Ping(pingCtx)
	cancel()
	if err != nil {
		t.Skipf("Docker engine unavailable: %v", err)
	}

	proxyImageID, err := da.ResolveLocalImage(t.Context(), harnessimages.ProxyImage())
	if err != nil {
		t.Skipf("local council proxy image unavailable (run sablectl harness build): %v", err)
	}
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
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer sc_task_") {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	// Create disposable managed login volumes for all three harnesses.
	da.CouncilHarnesses = make(map[string]docker.CouncilHarness, 3)
	for _, role := range []string{"claude", "codex", "agy"} {
		volName := "sablecourt-test-" + role + "-" + strings.ToLower(rand.Text()[:8])
		if _, err := cli.VolumeCreate(t.Context(), client.VolumeCreateOptions{
			Name:   volName,
			Driver: "local",
			Labels: docker.CouncilLoginVolumeLabels(role),
		}); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			_, _ = cli.VolumeRemove(ctx, volName, client.VolumeRemoveOptions{Force: true})
		})
		da.CouncilHarnesses[role] = docker.CouncilHarness{
			Image:       stubImageID,
			LoginVolume: volName,
		}
	}

	// Prepare a Git repo with committed v1 and uncommitted file.
	repo := initTempGitRepo(t)
	testWrite(t, repo, "committed.txt", "committed-v1\n", 0o644)
	testGit(t, repo, "add", "committed.txt")
	testGit(t, repo, "commit", "-m", "v1")
	v1 := strings.TrimSpace(testGit(t, repo, "rev-parse", "HEAD"))
	testWrite(t, repo, "uncommitted.txt", "uncommitted-v1\n", 0o644)

	wtRoot := filepath.Join(t.TempDir(), "worktrees")
	adapter := NewAdapter(wtRoot)
	adapter.CouncilRunner = da
	adapter.CouncilProxy = da
	adapter.CouncilProxyImage = proxyImageID
	adapter.DaemonListen = listen
	adapter.CouncilGateHandler = gateHandler
	adapter.CouncilSocketRoot = integrationSocketRoot(t)
	adapter.CouncilLimits = scruntime.CouncilLimits{
		NanoCPUs:    500_000_000,
		MemoryBytes: 128 << 20,
		PIDs:        64,
		WallTime:    30 * time.Second,
	}

	runTurn := func(t *testing.T, taskID, role string, payload map[string]string) []scruntime.Artifact {
		t.Helper()
		raw, err := json.Marshal(payload)
		if err != nil {
			t.Fatal(err)
		}
		token := "sc_task_" + rand.Text()
		spec := scruntime.LaunchSpec{
			TaskID:           taskID,
			Workload:         domain.Workload{Metadata: domain.ObjectMeta{Name: role, Zone: domain.ZoneSandbox}, Spec: domain.WorkloadSpec{Class: domain.ClassCodingAgent, Coding: &domain.CodingConfig{Harness: role, RepoPath: repo, Sandbox: "seatbelt"}}},
			InputJSON:        string(raw),
			TaskToken:        token,
			CouncilContainer: true,
			CouncilDeadline:  time.Now().Add(30 * time.Second),
		}
		h, err := adapter.Start(t.Context(), spec)
		if err != nil {
			t.Fatalf("Start(%s): %v", taskID, err)
		}
		t.Cleanup(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			_ = adapter.Stop(ctx, h)
		})

		taskBase, err := adapter.taskBase(taskID)
		if err != nil {
			t.Fatal(err)
		}
		<-adapter.containerProc(taskBase).done
		obs, err := adapter.Observe(t.Context(), h)
		if err != nil {
			t.Fatal(err)
		}
		if obs.State != scruntime.StateCompleted {
			t.Fatalf("task %s obs = %+v, want completed", taskID, obs)
		}
		arts, err := adapter.CollectArtifacts(t.Context(), h)
		if err != nil {
			t.Fatalf("CollectArtifacts(%s): %v", taskID, err)
		}
		// Verify container, proxy, and internal network are all removed.
		if _, err := cli.ContainerInspect(t.Context(), h.ContainerID, client.ContainerInspectOptions{}); err == nil {
			t.Fatalf("council container %s survived completion", h.ContainerID)
		}
		if _, err := cli.ContainerInspect(t.Context(), h.ProxyName, client.ContainerInspectOptions{}); err == nil {
			t.Fatalf("council proxy %s survived completion", h.ProxyName)
		}
		if _, err := cli.NetworkInspect(t.Context(), h.NetworkName, client.NetworkInspectOptions{}); err == nil {
			t.Fatalf("council network %s survived completion", h.NetworkName)
		}
		return arts
	}

	artifactByName := func(arts []scruntime.Artifact, name string) string {
		for _, a := range arts {
			if a.Name == name {
				return strings.TrimSpace(a.Text)
			}
		}
		return ""
	}

	t.Run("live", func(t *testing.T) {
		arts := runTurn(t, "task_docker_live_"+strings.ToLower(rand.Text()[:6]), "claude", map[string]string{
			"conclaveId":     "ccl_docker_live",
			"role":           "claude",
			"context_packet": "MODE_LIVE check",
			"checkoutMode":   checkoutModeLive,
		})
		if got := artifactByName(arts, "final-message"); got != "live review complete" {
			t.Fatalf("final-message = %q, want %q", got, "live review complete")
		}
		if diff := artifactByName(arts, "git-diff"); diff != "" {
			t.Fatalf("unexpected git-diff in live mode: %q", diff)
		}
	})

	// Advance HEAD to v2 before testing ref mode against pinned v1.
	testWrite(t, repo, "committed.txt", "committed-v2\n", 0o644)
	testGit(t, repo, "add", "committed.txt")
	testGit(t, repo, "commit", "-m", "v2")

	t.Run("ref", func(t *testing.T) {
		arts := runTurn(t, "task_docker_ref_"+strings.ToLower(rand.Text()[:6]), "codex", map[string]string{
			"conclaveId":     "ccl_docker_ref",
			"role":           "codex",
			"context_packet": "MODE_REF check",
			"checkoutMode":   checkoutModeRef,
			"checkoutRef":    v1,
		})
		if got := artifactByName(arts, "final-message"); got != "ref review complete" {
			t.Fatalf("final-message = %q, want %q", got, "ref review complete")
		}
		if diff := artifactByName(arts, "git-diff"); diff != "" {
			t.Fatalf("unexpected git-diff in ref mode: %q", diff)
		}
		if err := adapter.CleanupCouncil(t.Context(), "ccl_docker_ref"); err != nil {
			t.Fatalf("CleanupCouncil(ref): %v", err)
		}
	})

	t.Run("writer", func(t *testing.T) {
		// Reviewer turn on pinned base commit (read-only).
		revArts := runTurn(t, "task_docker_rev_"+strings.ToLower(rand.Text()[:6]), "agy", map[string]string{
			"conclaveId":     "ccl_docker_writer",
			"role":           "agy",
			"context_packet": "MODE_REVIEW check",
			"checkoutMode":   checkoutModeWriter,
			"checkoutRef":    v1,
			"checkoutWriter": "claude",
		})
		if got := artifactByName(revArts, "final-message"); got != "reviewer verdict" {
			t.Fatalf("reviewer final-message = %q, want %q", got, "reviewer verdict")
		}
		if diff := artifactByName(revArts, "git-diff"); diff != "" {
			t.Fatalf("unexpected git-diff for reviewer: %q", diff)
		}

		// Writer turn on assigned writer worktree (read-write) + host-side diff collection.
		wArts := runTurn(t, "task_docker_w_"+strings.ToLower(rand.Text()[:6]), "claude", map[string]string{
			"conclaveId":     "ccl_docker_writer",
			"role":           "claude",
			"context_packet": "MODE_WRITER edit",
			"checkoutMode":   checkoutModeWriter,
			"checkoutRef":    v1,
			"checkoutWriter": "claude",
		})
		if got := artifactByName(wArts, "final-message"); got != "writer done" {
			t.Fatalf("writer final-message = %q, want %q", got, "writer done")
		}
		diff := artifactByName(wArts, "git-diff")
		if !strings.Contains(diff, "feature.txt") || !strings.Contains(diff, "implemented in container") {
			t.Fatalf("writer git-diff missing container edit: %q", diff)
		}
		commit := artifactByName(wArts, "git-commit")
		if commit == "" || commit == v1 {
			t.Fatalf("writer git-commit = %q, expected new commit advancing past %s", commit, v1)
		}
		if got := testGit(t, repo, "show", commit+":feature.txt"); strings.TrimSpace(got) != "implemented in container" {
			t.Fatalf("host git show feature.txt = %q", got)
		}
		if err := adapter.CleanupCouncil(t.Context(), "ccl_docker_writer"); err != nil {
			t.Fatalf("CleanupCouncil(writer): %v", err)
		}
		// Ensure writer worktree directory was cleaned up while branch remains in Git.
		if _, err := os.Stat(filepath.Join(wtRoot, ".councils", councilKey("ccl_docker_writer"))); !os.IsNotExist(err) {
			t.Fatalf("council worktree directory still exists after cleanup: %v", err)
		}
	})
}

func TestCouncilContainerAuthenticatedCodexTool(t *testing.T) {
	if os.Getenv("SABLECOURT_TEST_AUTHENTICATED_CODEX") != "1" {
		t.Skip("requires a local image, a logged-in Codex volume and provider access")
	}
	da, err := docker.NewFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = da.Client.(*client.Client).Close() })
	def, err := harnessimages.Lookup("codex")
	if err != nil {
		t.Fatal(err)
	}
	da.CouncilHarnesses = map[string]docker.CouncilHarness{"codex": {Image: def.Image, LoginVolume: def.Volume}}
	repo := initTempGitRepo(t)
	if err := os.Chmod(repo, 0755); err != nil {
		t.Fatal(err)
	}
	// The expected answer is available only by reading the mounted checkout.
	challenge := rand.Text()
	testWrite(t, repo, "challenge.txt", challenge, 0644)
	a := NewAdapter(filepath.Join(t.TempDir(), "worktrees"))
	a.CouncilRunner = da
	a.CouncilProxy = da
	a.CouncilProxyImage = harnessimages.ProxyImage()
	a.DaemonListen = netip.AddrPortFrom(netip.AddrFrom4([4]byte{127, 0, 0, 1}), 7432).String()
	a.CouncilGateHandler = http.NotFoundHandler()
	a.CouncilSocketRoot = integrationSocketRoot(t)
	spec := scruntime.LaunchSpec{
		TaskID: "codex-tool-" + strings.ToLower(rand.Text()), TaskToken: "sc_task_" + rand.Text(),
		CouncilContainer: true, CouncilDeadline: time.Now().Add(3 * time.Minute),
		Workload:  domain.Workload{Metadata: domain.ObjectMeta{Name: "codex"}, Spec: domain.WorkloadSpec{Coding: &domain.CodingConfig{Harness: "codex", RepoPath: repo}}},
		InputJSON: `{"conclaveId":"ccl_tool","role":"codex","checkoutMode":"live","context_packet":"Use a shell tool to read challenge.txt in the checkout. Reply with its contents."}`,
	}
	h, err := a.Start(t.Context(), spec)
	if isContainerHandle(h) {
		t.Cleanup(func() {
			if err := a.Stop(context.Background(), h); err != nil {
				t.Error(err)
			}
		})
	}
	if err != nil {
		t.Fatal(err)
	}
	taskBase, err := a.taskBase(h.ExternalID)
	if err != nil {
		t.Fatal(err)
	}
	<-a.containerProc(taskBase).done
	obs, err := a.Observe(t.Context(), h)
	if err != nil || obs.State != scruntime.StateCompleted {
		t.Fatalf("Codex turn state %s, error %v", obs.State, err)
	}
	arts, err := a.CollectArtifacts(t.Context(), h)
	if err != nil {
		t.Fatal(err)
	}
	for _, art := range arts {
		if art.Name == "final-message" && strings.Contains(art.Text, challenge) {
			return
		}
	}
	t.Fatal("Codex did not return the contents of the challenge file")
}

func integrationSocketRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "g")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(dir); err != nil {
			t.Error(err)
		}
	})
	return dir
}
