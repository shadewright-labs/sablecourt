package docker

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/api/types/volume"
	"github.com/moby/moby/client"
	"github.com/shadewright-labs/sablecourt/internal/redact"
	rt "github.com/shadewright-labs/sablecourt/internal/runtime"
	"github.com/shadewright-labs/sablecourt/internal/runtime/mountguard"
)

// CouncilHarness is trusted daemon configuration for one locally built image,
// its private login volume, and optional fixed proxy endpoints.
type CouncilHarness struct {
	Image, LoginVolume    string
	ProxyURL, DataGateURL string
	// LoginFiles are the credential files whose presence in the login volume
	// means the user has logged in. Only the preflight uses them, by path, and it does not open them.
	LoginFiles []string
	// Tool is set when the image carries no vendor tool. The tool then comes
	// from its own volume, mounted read-only, and is checked before each run.
	Tool *CouncilTool
}

// CouncilLoginVolumeLabels identifies the managed volume when provisioning login state.
func CouncilLoginVolumeLabels(harness string) map[string]string {
	return map[string]string{"app.kubernetes.io/managed-by": "sablecourt", "sablecourt.io/harness": harness}
}

var councilName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,127}$`)
var modelName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._:/-]{0,127}$`)

// Bash reads two NUL-terminated values literally. Docker only stores this
// script and the public command. The CLI receives EOF on its first read.
const bootstrap = `IFS= read -r -d '' SABLECOURT_TASK_TOKEN || exit 125
IFS= read -r -d '' prompt || exit 125
` + bootstrapTail

const bootstrapTail = `SABLECOURT_TASK_TOKEN_FILE="$TMPDIR/task.lease"
(umask 077; printf '%s' "$SABLECOURT_TASK_TOKEN" > "$SABLECOURT_TASK_TOKEN_FILE") || exit 125
unset SABLECOURT_TASK_TOKEN
export SABLECOURT_TASK_TOKEN_FILE
exec "$@" "$prompt" </dev/null`

func councilCommand(s rt.CouncilLaunch, login bool, tool *CouncilTool) ([]string, error) {
	cmd, err := councilArgv(s, login)
	if err != nil || tool == nil {
		return cmd, err
	}
	// The vendor tool lives in the read-only tool volume. A guard checks it
	// before it runs; the bootstrap form reads stdin first, so the runner's
	// write of the lease and prompt never meets a closed container.
	path := councilToolDir + "/" + tool.Binary
	if login {
		cmd[0] = path
		return append([]string{"/bin/bash", "--noprofile", "--norc", "-c", toolGuardLogin, "guard"}, append(tool.guardArgs(), cmd...)...), nil
	}
	if len(cmd) < 7 || cmd[4] != bootstrap {
		return nil, errors.New("unexpected council bootstrap")
	}
	argv := append([]string{path}, cmd[7:]...)
	return append([]string{"/bin/bash", "--noprofile", "--norc", "-c", toolBootstrap, "bootstrap"}, append(tool.guardArgs(), argv...)...), nil
}

func councilArgv(s rt.CouncilLaunch, login bool) ([]string, error) {
	var cmd []string
	switch s.Harness {
	case "claude":
		cmd = []string{"/usr/local/bin/claude"}
		if login {
			return append(cmd, "auth", "login"), nil
		}
		cmd = append(cmd, "--no-session-persistence", "--output-format", "text")
		if s.Checkout.Kind.Writable() {
			cmd = append(cmd, "--permission-mode", "acceptEdits")
		} else {
			cmd = append(cmd, "--permission-mode", "plan")
		}
	case "codex":
		cmd = []string{"/usr/local/bin/codex"}
		if login {
			return append(cmd, "login", "--device-auth"), nil
		}
		// The container provides the filesystem and capability boundary.
		cmd = append(cmd, "exec", "--sandbox", "danger-full-access", "--ephemeral")
	case "agy":
		cmd = []string{"/usr/local/bin/agy"}
		if login {
			return cmd, nil
		}
		// Read-only workspace confinement is enforced by the container mount.
		cmd = append(cmd, "--output-format", "text", "--sandbox")
	default:
		return nil, errors.New("unknown council harness")
	}
	if s.Model != "" {
		if !modelName.MatchString(s.Model) {
			return nil, errors.New("invalid council model")
		}
		cmd = append(cmd, "--model", s.Model)
	}
	if s.Harness != "codex" {
		cmd = append(cmd, "-p")
	}
	return append([]string{"/bin/bash", "--noprofile", "--norc", "-c", bootstrap, "bootstrap"}, cmd...), nil
}

func proxyEndpoint(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "http" || !councilName.MatchString(u.Hostname()) || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New("invalid council proxy endpoint")
	}
	return u, nil
}

// councilWorkspace builds the checkout bind mount and, for a linked worktree,
// the bind mount of its per-launch Git metadata directory. Which host
// directories may be mounted is decided by mountguard alone: a checkout the
// daemon made must be reached through the worktree root handle with no
// symlink component, and a live checkout is checked by file identity against
// the protected paths and runtime sockets.
func councilWorkspace(s rt.CouncilCheckout, sockets []string, guard *mountguard.Guard) ([]mount.Mount, error) {
	switch s.Kind {
	case rt.CouncilLive, rt.CouncilRef, rt.CouncilReview, rt.CouncilWriter, rt.CouncilTask:
	default:
		return nil, errors.New("invalid council checkout kind")
	}
	live := s.Kind == rt.CouncilLive
	if live && s.GitDir != "" {
		return nil, errors.New("a live checkout carries its own Git metadata")
	}
	path, err := guard.MountSource(s.Path, live, sockets)
	if err != nil {
		return nil, err
	}
	mounts := []mount.Mount{{Type: mount.TypeBind, Source: path, Target: "/workspace", ReadOnly: !s.Kind.Writable(), BindOptions: &mount.BindOptions{NonRecursive: true}}}
	if s.GitDir == "" {
		return mounts, nil
	}
	gitDir, err := guard.MountSource(s.GitDir, false, sockets)
	if err != nil {
		return nil, err
	}
	if overlap, err := mountguard.Overlap(path, gitDir); err != nil || overlap {
		return nil, errors.New("checkout Git metadata overlaps the checkout")
	}
	// Only a self-contained metadata directory is mounted. A linked worktree
	// registration (commondir), an alternates file or a repository with
	// registered worktrees would reach the shared refs and objects.
	for _, name := range []string{"commondir", "worktrees", filepath.Join("objects", "info", "alternates")} {
		if _, err := guard.Root.Lstat(gitDir + string(filepath.Separator) + name); !errors.Is(err, os.ErrNotExist) {
			return nil, errors.New("checkout Git metadata is not self-contained")
		}
	}
	return append(mounts, mount.Mount{Type: mount.TypeBind, Source: gitDir, Target: councilGitDir, ReadOnly: !s.Kind.Writable(), BindOptions: &mount.BindOptions{NonRecursive: true}}), nil
}

// councilGitDir is where a launch's own Git metadata directory is mounted.
// GIT_DIR and GIT_WORK_TREE point Git in the container at it and /workspace.
const councilGitDir = "/workspace-git"

func preparePrompt(s rt.CouncilLaunch) (string, error) {
	if s.TaskToken == "" || len(s.TaskToken) > 4096 || strings.ContainsAny(s.TaskToken, "\x00\r\n") ||
		len(s.Prompt) > 64<<10 || strings.TrimSpace(s.Prompt) == "" || strings.HasPrefix(strings.TrimSpace(s.Prompt), "-") || strings.ContainsRune(s.Prompt, 0) {
		return "", errors.New("invalid council bootstrap input")
	}
	prompt := redact.String(strings.ReplaceAll(s.Prompt, s.TaskToken, "[REDACTED]"))
	if privateKeyStart.MatchString(prompt) || len(prompt) > 64<<10 {
		return "", errors.New("unsafe council prompt")
	}
	return prompt, nil
}

// harnessContainerSpec is the only function that builds a harness container:
// standalone coding tasks (checkout kind task, or review when read-only),
// council seats and login sessions all get their create options here, so they
// share one set of guards. Only explicit mounts (checkout, login volume, the
// read-only tool volume, tmpfs scratch), an allowlisted environment, the
// per-launch internal network behind the proxy, a non-root user, a read-only
// root file system, all capabilities dropped and no-new-privileges.
func (a *Adapter) harnessContainerSpec(ctx context.Context, engine councilEngineAPI, s rt.CouncilLaunch, login bool) (client.ContainerCreateOptions, error) {
	var o client.ContainerCreateOptions
	cfg, ok := a.CouncilHarnesses[s.Harness]
	if !ok || cfg.Image == "" || !councilName.MatchString(cfg.LoginVolume) || !strings.HasPrefix(cfg.LoginVolume, "sablecourt-") {
		return o, errors.New("missing council image or login volume configuration")
	}
	for name, other := range a.CouncilHarnesses {
		if name != s.Harness && cfg.LoginVolume == other.LoginVolume {
			return o, errors.New("council harnesses require separate login volumes")
		}
	}
	if err := a.validateCouncilTool(s.Harness, cfg); err != nil {
		return o, err
	}
	if err := a.remoteEngine(); err != nil {
		return o, err
	}
	if !councilName.MatchString(s.TaskID) || !councilName.MatchString(s.Workload) {
		return o, errors.New("invalid council task or workload name")
	}
	l := s.Limits
	if l.NanoCPUs <= 0 || l.MemoryBytes <= 0 || l.PIDs <= 0 || l.WallTime <= 0 {
		return o, errors.New("council requires positive CPU, memory, PID and wall-time limits")
	}
	cmd, err := councilCommand(s, login, cfg.Tool)
	if err != nil {
		return o, err
	}
	proxyRaw := cfg.ProxyURL
	if s.ProxyURL != "" {
		proxyRaw = s.ProxyURL
	}
	gateRaw := cfg.DataGateURL
	if s.DataGateURL != "" {
		gateRaw = s.DataGateURL
	}
	proxy, err := proxyEndpoint(proxyRaw)
	if err != nil || (proxy.Path != "" && proxy.Path != "/") {
		return o, errors.New("invalid council forward proxy")
	}
	// A login session reaches only the provider through the proxy and has no gate.
	gate := proxy
	if !login {
		gate, err = proxyEndpoint(gateRaw)
		if err != nil || gate.Hostname() != proxy.Hostname() {
			return o, errors.New("gate must use the pinned council proxy")
		}
	}
	env := append(slices.Clone(councilBaseEnv),
		"HTTP_PROXY="+proxyRaw, "HTTPS_PROXY="+proxyRaw,
		"http_proxy="+proxyRaw, "https_proxy="+proxyRaw,
		"NO_PROXY="+gate.Hostname(), "no_proxy="+gate.Hostname(),
	)
	if s.Harness == "claude" {
		// Claude Code's documented switch that blocks every update, background
		// and manual. A new version arrives only as a new pin in a release.
		env = append(env, "DISABLE_UPDATES=1")
	}
	var mounts []mount.Mount
	wd := "/scratch"
	if !login {
		ms, err := councilWorkspace(s.Checkout, a.dockerSockets(), a.Mounts)
		if err != nil {
			return o, err
		}
		mounts = append(mounts, ms...)
		m := ms[0]
		wd = m.Target
		env = append(env, "SABLECOURT_MANAGED=1", "SABLECOURT_TASK_ID="+s.TaskID, "SABLECOURT_WORKLOAD="+s.Workload, "SABLECOURT_DATA_GATE_URL="+gateRaw)
		if len(ms) > 1 {
			// The checkout's .git file names a host path; Git here uses the
			// launch's own metadata, mounted beside the checkout.
			env = append(env, "GIT_DIR="+councilGitDir, "GIT_WORK_TREE="+m.Target)
		}
		if m.ReadOnly {
			env = append(env, "GIT_OPTIONAL_LOCKS=0")
		}
	}
	netName := a.NetworkName
	if s.Network != "" {
		if !councilName.MatchString(s.Network) {
			return o, errors.New("invalid council network name")
		}
		netName = s.Network
	}
	networkID, err := a.internalNetworkNamed(ctx, netName, false)
	if err != nil {
		return o, fmt.Errorf("inspect council network: %w", err)
	}
	if s.NetworkID != "" && s.NetworkID != networkID {
		return o, errors.New("council harness network was replaced")
	}
	imageID, err := a.inspectCouncilHarness(ctx, engine, s.Harness, cfg)
	if err != nil {
		return o, err
	}
	mounts = append(mounts, mount.Mount{Type: mount.TypeVolume, Source: cfg.LoginVolume, Target: "/home/harness"})
	if cfg.Tool != nil {
		// Read-only, and NoCopy so the engine never fills it from the image.
		mounts = append(mounts, mount.Mount{Type: mount.TypeVolume, Source: cfg.Tool.Volume, Target: councilToolDir, ReadOnly: true, VolumeOptions: &mount.VolumeOptions{NoCopy: true}})
	}
	// timeout also bounds the workload if the daemon exits mid-run.
	seconds := strconv.FormatFloat(l.WallTime.Seconds(), 'f', 9, 64)
	o.Name = "sablecourt-council-" + s.TaskID
	o.Config = &container.Config{
		Image:        imageID,
		User:         "10001:10001",
		WorkingDir:   wd,
		Env:          env,
		Entrypoint:   []string{"/usr/bin/timeout", "--foreground", "--signal=KILL", seconds},
		Cmd:          cmd,
		OpenStdin:    true,
		StdinOnce:    true,
		AttachStdin:  true,
		AttachStdout: true,
		AttachStderr: true,
		Tty:          login,
		Healthcheck:  &container.HealthConfig{Test: []string{"NONE"}},
		Labels:       map[string]string{"sablecourt.io/task-id": s.TaskID},
	}
	o.HostConfig = &container.HostConfig{
		NetworkMode:    container.NetworkMode(networkID),
		ReadonlyRootfs: true,
		CapDrop:        []string{"ALL"},
		SecurityOpt:    []string{"no-new-privileges:true"},
		LogConfig:      container.LogConfig{Type: "none"},
		Mounts:         mounts,
		// Docker accepts uid and gid in the tmpfs option string.
		Tmpfs: map[string]string{"/scratch": fmt.Sprintf("rw,noexec,nosuid,nodev,mode=0700,uid=10001,gid=10001,size=%d", l.MemoryBytes)},
		Resources: container.Resources{
			NanoCPUs:   l.NanoCPUs,
			Memory:     l.MemoryBytes,
			MemorySwap: l.MemoryBytes,
			PidsLimit:  &l.PIDs,
		},
	}
	return o, nil
}

func (a *Adapter) inspectCouncilHarness(ctx context.Context, engine councilEngineAPI, harness string, cfg CouncilHarness) (string, error) {
	id, err := a.councilImageID(ctx, cfg.Image)
	if err != nil {
		return "", err
	}
	if err := inspectCouncilLoginVolume(ctx, engine, harness, cfg.LoginVolume); err != nil {
		return "", err
	}
	if cfg.Tool != nil {
		if err := inspectCouncilToolVolume(ctx, engine, harness, cfg.Tool.Volume); err != nil {
			return "", err
		}
	}
	return id, nil
}

var councilBaseEnv = []string{"PATH=/usr/local/bin:/usr/bin:/bin", "HOME=/home/harness", "TMPDIR=/scratch"}

func (a *Adapter) councilImageID(ctx context.Context, ref string) (string, error) {
	image, err := a.localImage(ctx, ref)
	if err != nil {
		return "", err
	}
	if image.Config == nil || len(image.Config.Volumes) != 0 {
		return "", errors.New("missing image config or unexpected image volumes")
	}
	// Docker merges image defaults at create. Only inherit variables that the
	// launch configuration explicitly replaces.
	for _, entry := range image.Config.Env {
		key, _, ok := strings.Cut(entry, "=")
		if !ok || (key != "HOME" && key != "PATH" && key != "TMPDIR") {
			return "", errors.New("council image has unexpected environment defaults")
		}
	}
	return image.ID, nil
}

func inspectCouncilLoginVolume(ctx context.Context, engine councilEngineAPI, harness, name string) error {
	vol, err := engine.VolumeInspect(ctx, name, client.VolumeInspectOptions{})
	if err != nil {
		return fmt.Errorf("inspect council login volume: %w", err)
	}
	return validateCouncilLoginVolume(vol.Volume, harness, name)
}

func validateCouncilLoginVolume(vol volume.Volume, harness, name string) error {
	if vol.Name != name || vol.Driver != "local" || len(vol.Options) != 0 {
		return errors.New("invalid council login volume")
	}
	for k, v := range CouncilLoginVolumeLabels(harness) {
		if vol.Labels[k] != v {
			return errors.New("login volume does not belong to this council harness")
		}
	}
	return nil
}
