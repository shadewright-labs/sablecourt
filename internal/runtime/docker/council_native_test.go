package docker

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"
	rt "github.com/shadewright-labs/sablecourt/internal/runtime"
)

// This probe runs as the CLI under the production bootstrap and resource policy.
const nativeProbe = `#!/bin/bash
set -eu
if IFS= read -r line; then exit 31; fi
[[ $(id -u) == 10001 ]]
[[ $(id -g) == 10001 ]]
grep -Eq '^CapEff:[[:space:]]*0+$' /proc/self/status
grep -Eq '^NoNewPrivs:[[:space:]]*1$' /proc/self/status
awk '$5 == "/" && $6 ~ /^ro(,|$)/ {found=1} END{exit !found}' /proc/self/mountinfo
[[ $(cat /sys/fs/cgroup/memory.max) == 67108864 ]]
[[ $(cat /sys/fs/cgroup/memory.swap.max) == 0 ]]
[[ $(cat /sys/fs/cgroup/pids.max) == 32 ]]
read -r quota period </sys/fs/cgroup/cpu.max
(( quota * 2 == period ))
[[ -z ${SABLECOURT_MODEL_GATE_URL+x} ]]
[[ $(stat -c '%u:%g:%a' /scratch) == 10001:10001:700 ]]
awk '$2 == "/scratch" && $3 == "tmpfs" {
  opts = "," $4 ","
  found = opts ~ /,noexec,/ && opts ~ /,nosuid,/ && opts ~ /,nodev,/
} END {exit !found}' /proc/mounts
read -r block_size blocks < <(stat -f -c '%S %b' /scratch)
(( block_size * blocks == 67108864 ))
touch /scratch/probe
case "$SABLECOURT_WORKLOAD" in
  writer) touch /workspace/probe ;;
  review) if touch /workspace/probe 2>/dev/null; then exit 32; fi ;;
esac
printf 'clean answer\n'
printf '%s\n' "${!#}" "$(cat "$SABLECOURT_TASK_TOKEN_FILE")" >&2
exit 7
`

func cleanupNative(t *testing.T, fn func(context.Context) error) {
	t.Helper()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := fn(ctx); err != nil && !isMissing(err) {
			t.Errorf("Docker fixture cleanup: %v", err)
		}
	})
}

const defaultCouncilBaseImage = "debian:bookworm-slim@sha256:e67eeead736ff7bbe258ef088bff683635c561b1fcfefad0806c1df4ab49ce0e"

func nativeFixtureImage(t *testing.T, cli *client.Client) string {
	t.Helper()
	ref := defaultCouncilBaseImage
	if envRef := os.Getenv("SABLECOURT_TEST_COUNCIL_BASE_IMAGE"); envRef != "" {
		ref = envRef
	}
	image, err := cli.ImageInspect(t.Context(), ref)
	if isMissing(err) {
		pullCtx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
		defer cancel()
		resp, pullErr := cli.ImagePull(pullCtx, ref, client.ImagePullOptions{})
		if pullErr != nil {
			t.Fatalf("pull council test base image %q: %v", ref, pullErr)
		}
		if waitErr := resp.Wait(pullCtx); waitErr != nil {
			t.Fatalf("wait for council test base image %q pull: %v", ref, waitErr)
		}
		image, err = cli.ImageInspect(t.Context(), ref)
	}
	if err != nil {
		t.Fatalf("inspect council test base image %q: %v", ref, err)
	}
	id := image.ID
	// Install a test CLI in a disposable local image. The staging container never starts.
	stage, err := cli.ContainerCreate(t.Context(), client.ContainerCreateOptions{Config: &container.Config{Image: id, Cmd: []string{"/bin/true"}}, HostConfig: &container.HostConfig{NetworkMode: "none"}})
	if err != nil {
		t.Fatal(err)
	}
	cleanupNative(t, func(ctx context.Context) error {
		_, err := cli.ContainerRemove(ctx, stage.ID, client.ContainerRemoveOptions{Force: true, RemoveVolumes: true})
		return err
	})
	var payload bytes.Buffer
	w := tar.NewWriter(&payload)
	if err := w.WriteHeader(&tar.Header{Name: "claude", Mode: 0755, Size: int64(len(nativeProbe))}); err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(w, nativeProbe); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err = cli.CopyToContainer(t.Context(), stage.ID, client.CopyToContainerOptions{DestinationPath: "/usr/local/bin", Content: &payload}); err != nil {
		t.Fatal(err)
	}
	img, err := cli.ContainerCommit(t.Context(), stage.ID, client.ContainerCommitOptions{})
	if err != nil {
		t.Fatal(err)
	}
	cleanupNative(t, func(ctx context.Context) error {
		_, err := cli.ImageRemove(ctx, img.ID, client.ImageRemoveOptions{})
		return err
	})
	return img.ID
}

func TestCouncilNativeDocker(t *testing.T) {
	a, err := NewFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	cli := a.Client.(*client.Client)
	t.Cleanup(func() { _ = cli.Close() })
	probe, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	_, err = cli.Ping(probe, client.PingOptions{})
	cancel()
	if err != nil {
		t.Skipf("Docker engine unavailable: %v", err)
	}

	id := nativeFixtureImage(t, cli)
	name := "sablecourt-test-" + strings.ToLower(rand.Text())
	net, err := cli.NetworkCreate(t.Context(), name, client.NetworkCreateOptions{Internal: true, Driver: "bridge"})
	if err != nil {
		t.Fatal(err)
	}
	cleanupNative(t, func(ctx context.Context) error {
		_, err := cli.NetworkRemove(ctx, net.ID, client.NetworkRemoveOptions{})
		return err
	})
	vol, err := cli.VolumeCreate(t.Context(), client.VolumeCreateOptions{Name: name, Driver: "local", Labels: CouncilLoginVolumeLabels("claude")})
	if err != nil {
		t.Fatal(err)
	}
	cleanupNative(t, func(ctx context.Context) error {
		_, err := cli.VolumeRemove(ctx, vol.Volume.Name, client.VolumeRemoveOptions{})
		return err
	})
	a.NetworkName = name
	a.CouncilHarnesses = map[string]CouncilHarness{"claude": {Image: id, LoginVolume: name, ProxyURL: "http://relay:8888", DataGateURL: "http://relay:8080/gate/data"}}
	for _, kind := range []rt.CouncilCheckoutKind{rt.CouncilWriter, rt.CouncilReview} {
		t.Run(string(kind), func(t *testing.T) {
			path := t.TempDir()
			if err := os.Chmod(path, 0777); err != nil {
				t.Fatal(err)
			}
			s := rt.CouncilLaunch{TaskID: name, Harness: "claude", Checkout: rt.CouncilCheckout{Path: path, Kind: kind}, Workload: "review",
				Limits: rt.CouncilLimits{NanoCPUs: 500000000, MemoryBytes: 64 << 20, PIDs: 32, WallTime: 20 * time.Second}, Prompt: "question " + rand.Text(), TaskToken: rand.Text()}
			if kind == rt.CouncilWriter {
				s.Workload = "writer"
			}
			r, err := a.RunCouncil(t.Context(), s, func(ctx context.Context, r rt.CouncilResult) error {
				meta, err := cli.ContainerInspect(ctx, r.ContainerID, client.ContainerInspectOptions{})
				if err != nil {
					return err
				}
				raw, err := json.Marshal(meta.Container)
				if err != nil {
					return err
				}
				if bytes.Contains(raw, []byte(s.TaskToken)) || bytes.Contains(raw, []byte(s.Prompt)) {
					return errors.New("sensitive Docker inspect metadata")
				}
				if meta.Container.HostConfig.LogConfig.Type != "none" || meta.Container.LogPath != "" {
					return errors.New("persistent Docker logs enabled")
				}
				logs, err := cli.ContainerLogs(ctx, r.ContainerID, client.ContainerLogsOptions{ShowStdout: true, ShowStderr: true})
				switch {
				case err == nil:
					defer logs.Close()
					raw, err = io.ReadAll(io.LimitReader(logs, 1024))
					if err != nil || len(raw) != 0 {
						return errors.New("Docker retained output")
					}
				case !strings.Contains(err.Error(), "does not support reading"):
					return fmt.Errorf("check Docker logs: %w", err)
				}
				return nil
			})
			if err != nil || r.ExitCode != 7 || r.FinishedAt.IsZero() || r.Stdout != "clean answer\n" || r.Stderr != "[REDACTED]\n[REDACTED]\n" {
				t.Fatalf("Docker evidence: exit=%d stdout=%q stderr=%q error=%v", r.ExitCode, r.Stdout, r.Stderr, err)
			}
			meta, err := cli.ContainerInspect(t.Context(), r.ContainerID, client.ContainerInspectOptions{})
			switch {
			case err == nil:
				t.Fatalf("expected inspect of %q after cleanup to return not found (HTTP 404), got container %q", r.ContainerID, meta.Container.ID)
			case !isMissing(err):
				t.Fatalf("expected inspect of %q after cleanup to return not found (HTTP 404), got error %v", r.ContainerID, err)
			}
		})
	}
	t.Run("detached wall limit", func(t *testing.T) {
		// Leave bootstrap waiting for input and disconnect. PID 1 must enforce the deadline.
		s := rt.CouncilLaunch{TaskID: name, Workload: "review", Harness: "claude", Checkout: rt.CouncilCheckout{Kind: rt.CouncilReview, Path: t.TempDir()}, Limits: rt.CouncilLimits{WallTime: time.Second, MemoryBytes: 64 << 20, NanoCPUs: 500000000, PIDs: 32}}
		o, err := a.harnessContainerSpec(t.Context(), cli, s, false)
		if err != nil {
			t.Fatal(err)
		}
		c, err := cli.ContainerCreate(t.Context(), o)
		if err != nil {
			t.Fatal(err)
		}
		cleanupNative(t, func(ctx context.Context) error {
			_, err := cli.ContainerRemove(ctx, c.ID, client.ContainerRemoveOptions{Force: true, RemoveVolumes: true})
			return err
		})
		if _, err := cli.ContainerStart(t.Context(), c.ID, client.ContainerStartOptions{}); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancel()
		wait := cli.ContainerWait(ctx, c.ID, client.ContainerWaitOptions{})
		select {
		case <-ctx.Done():
			t.Fatal("detached container outlived its wall limit")
		case err := <-wait.Error:
			t.Fatal(err)
		case result := <-wait.Result:
			if result.Error != nil {
				t.Fatalf("expected watchdog exit 137, Docker wait reported %q", result.Error.Message)
			}
			if result.StatusCode != 137 {
				t.Fatalf("expected watchdog exit 137, got %d", result.StatusCode)
			}
		}
	})
}
