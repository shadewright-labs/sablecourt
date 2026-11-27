package docker

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/api/types/volume"
	"github.com/moby/moby/client"
	rt "github.com/shadewright-labs/sablecourt/internal/runtime"
)

// CouncilTool is trusted daemon configuration for a vendor tool that the
// image does not carry. Its volume holds only the tool and a VERSION file.
// Harness containers mount it read-only; only the install container writes it.
type CouncilTool struct {
	Volume  string
	Binary  string
	Version string
	SHA256  string
	// InstallCommand is shown when the tool is missing or not the pinned one.
	// The runtime never chooses user guidance itself.
	InstallCommand string
}

// councilToolDir is where the tool volume is mounted. The image creates the
// directory owned by the harness user, so a new volume takes that owner.
const councilToolDir = "/opt/harness-tool"

var (
	toolBinaryName = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)
	toolVersion    = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+$`)
	toolDigest     = regexp.MustCompile(`^[0-9a-f]{64}$`)
	toolCommand    = regexp.MustCompile(`^[a-z][a-z0-9 -]{0,127}$`)
)

func (t *CouncilTool) guardArgs() []string {
	return []string{councilToolDir + "/" + t.Binary, t.Version, t.SHA256, t.InstallCommand}
}

// CouncilToolVolumeLabels identifies the tool volume of a harness. They differ
// from the login labels, so neither volume validates as the other.
func CouncilToolVolumeLabels(harness string) map[string]string {
	return map[string]string{"app.kubernetes.io/managed-by": "sablecourt", "sablecourt.io/harness-tool": harness}
}

func (a *Adapter) validateCouncilTool(harness string, cfg CouncilHarness) error {
	t := cfg.Tool
	if t == nil {
		return nil
	}
	if !councilName.MatchString(t.Volume) || !strings.HasPrefix(t.Volume, "sablecourt-") || !toolBinaryName.MatchString(t.Binary) ||
		!toolVersion.MatchString(t.Version) || !toolDigest.MatchString(t.SHA256) || !toolCommand.MatchString(t.InstallCommand) {
		return errors.New("invalid council tool configuration")
	}
	for name, other := range a.CouncilHarnesses {
		if t.Volume == other.LoginVolume || (name != harness && other.Tool != nil && other.Tool.Volume == t.Volume) {
			return errors.New("council tool volume must be separate from every login volume and from other tools")
		}
	}
	return nil
}

func inspectCouncilToolVolume(ctx context.Context, engine councilEngineAPI, harness, name string) error {
	vol, err := engine.VolumeInspect(ctx, name, client.VolumeInspectOptions{})
	if err != nil {
		return fmt.Errorf("inspect council tool volume: %w", err)
	}
	return validateCouncilToolVolume(vol.Volume, harness, name)
}

func validateCouncilToolVolume(vol volume.Volume, harness, name string) error {
	if vol.Name != name || vol.Driver != "local" || len(vol.Options) != 0 {
		return errors.New("invalid council tool volume")
	}
	want := CouncilToolVolumeLabels(harness)
	for k, v := range want {
		if vol.Labels[k] != v {
			return errors.New("tool volume does not belong to this council harness")
		}
	}
	// Labels cannot change after creation and only Sablecourt creates this
	// volume, so any other label (a login label included) means another
	// creator. Docker adds no labels to a volume created through its API.
	if len(vol.Labels) != len(want) {
		return errors.New("tool volume has other labels than the tool volume labels")
	}
	return nil
}

// toolCheck sets tool_fault from $t (path), $v (version) and $d (SHA-256):
// 0 when the tool is the pinned one, 3 when missing, 4 when another version is
// installed, 5 when the bytes differ from the pin. It never runs the tool.
// VERSION must be a regular file, not a symlink, holding exactly the version
// and one newline: reading two bytes more than that shows any extra content,
// and the trailing x keeps command substitution from dropping the newline.
const toolCheck = `vf=${t%/*}/VERSION
if ! test -f "$t" || test -L "$t" || ! test -x "$t"; then tool_fault=3
elif ! test -f "$vf" || test -L "$vf" || test "$(head -c $((${#v} + 2)) "$vf" 2>/dev/null; echo x)" != "$v"$'\n'x; then tool_fault=4
elif test "$(sha256sum < "$t" | head -c 64)" != "$d"; then tool_fault=5
else tool_fault=0; fi
`

// toolRefusal prints the fault and the command that fixes it, then exits 125.
const toolRefusal = `case $tool_fault in
0) ;;
3) printf 'council tool %s is not installed: run %s\n' "${t##*/}" "$h" >&2; exit 125 ;;
4) printf 'council tool %s is not the pinned version %s: run %s\n' "${t##*/}" "$v" "$h" >&2; exit 125 ;;
*) printf 'council tool %s does not match its pinned SHA-256: run %s\n' "${t##*/}" "$h" >&2; exit 125 ;;
esac
`

const toolGuardArgs = `t=$1 v=$2 d=$3 h=$4
shift 4
`

// toolBootstrap is the bootstrap with the tool check between reading stdin
// and writing the lease file, so a refused run never stores the lease.
const toolBootstrap = toolGuardArgs + `IFS= read -r -d '' SABLECOURT_TASK_TOKEN || exit 125
IFS= read -r -d '' prompt || exit 125
` + toolCheck + toolRefusal + bootstrapTail

const toolGuardLogin = toolGuardArgs + toolCheck + toolRefusal + `exec "$@"`

// toolProbeScript checks the tool and then the login files. Exit 1 means no
// login, 3 to 5 are the tool faults of toolCheck.
const toolProbeScript = `t=$1 v=$2 d=$3
shift 3
` + toolCheck + `test "$tool_fault" = 0 || exit "$tool_fault"
` + loginProbeScript

// toolInstallScript writes stdin to a temporary name in the tool volume, checks
// the bytes against the pin, and renames them into place, then VERSION. The
// temporary names are unique to the run, so an install running at the same
// time cannot write the file this run checked before it is renamed. A
// destination that exists as anything but a regular file fails the install,
// because mv would move the file inside a directory; after the renames both
// final paths are checked again.
const toolInstallScript = `set -eu
dir=` + councilToolDir + ` b=$1 v=$2 d=$3
umask 022
for f in "$dir/$b" "$dir/VERSION"; do
  if test -L "$f" || { test -e "$f" && ! test -f "$f"; }; then
    echo "$f exists and is not a regular file: remove the tool volume and install again" >&2
    exit 4
  fi
done
tool=$(mktemp "$dir/.tool.XXXXXXXX")
ver=$(mktemp "$dir/.version.XXXXXXXX")
trap 'rm -f "$tool" "$ver"' EXIT
cat > "$tool"
if test "$(sha256sum < "$tool" | head -c 64)" != "$d"; then
  echo 'the bytes written to the tool volume do not match the pinned SHA-256' >&2
  exit 3
fi
chmod 0555 "$tool"
printf '%s\n' "$v" > "$ver"
chmod 0644 "$ver"
mv -f "$tool" "$dir/$b"
mv -f "$ver" "$dir/VERSION"
if test -L "$dir/$b" || ! test -f "$dir/$b" || test "$(sha256sum < "$dir/$b" | head -c 64)" != "$d" ||
  test -L "$dir/VERSION" || ! test -f "$dir/VERSION" || test "$(head -c $((${#v} + 2)) "$dir/VERSION"; echo x)" != "$v"$'\n'x; then
  echo 'the installed tool or VERSION is not a regular file with the pinned content' >&2
  exit 4
fi`

const (
	toolInstallTimeout = 10 * time.Minute
	toolInstallMemory  = int64(256 << 20)
	toolInstallPIDs    = int64(32)
)

// InstallCouncilTool streams a binary that the caller has already checked
// into the harness's tool volume. The install container uses the harness
// image, runs as the harness user with no network, no capabilities, a
// read-only root, no environment beyond the base and no other mount: it sees
// no login and no secret, and can write only the tool volume. It checks the
// SHA-256 again before the binary replaces the old one.
func (a *Adapter) InstallCouncilTool(parent context.Context, harness string, binary io.Reader) (err error) {
	cfg, ok := a.CouncilHarnesses[harness]
	if !ok || cfg.Tool == nil || cfg.Image == "" {
		return errors.New("council harness has no tool to install")
	}
	if err := a.validateCouncilTool(harness, cfg); err != nil {
		return err
	}
	if binary == nil {
		return errors.New("missing tool binary")
	}
	engine, ok := a.Client.(councilEngineAPI)
	if !ok {
		return errors.New("Docker cannot attach to the install container")
	}
	ctx, cancel := context.WithTimeout(parent, toolInstallTimeout)
	defer cancel()
	imageID, err := a.councilImageID(ctx, cfg.Image)
	if err != nil {
		return fmt.Errorf("resolve harness image: %w", err)
	}
	if err := a.ensureCouncilToolVolume(ctx, engine, harness, cfg.Tool.Volume); err != nil {
		return err
	}
	pids := toolInstallPIDs
	created, err := a.createContainer(ctx, client.ContainerCreateOptions{
		Name: "sablecourt-tool-install-" + harness + "-" + strings.ToLower(rand.Text()[:10]),
		Config: &container.Config{
			Image:       imageID,
			User:        "10001:10001",
			Entrypoint:  []string{"/bin/bash", "--noprofile", "--norc"},
			Cmd:         []string{"-c", toolInstallScript, "install", cfg.Tool.Binary, cfg.Tool.Version, cfg.Tool.SHA256},
			Env:         append([]string(nil), councilBaseEnv...),
			WorkingDir:  "/",
			OpenStdin:   true,
			StdinOnce:   true,
			AttachStdin: true, AttachStdout: true, AttachStderr: true,
			Labels:      map[string]string{"sablecourt.io/tool-install": harness},
			Healthcheck: &container.HealthConfig{Test: []string{"NONE"}},
		},
		HostConfig: &container.HostConfig{
			NetworkMode:    "none",
			ReadonlyRootfs: true,
			CapDrop:        []string{"ALL"},
			SecurityOpt:    []string{"no-new-privileges:true"},
			LogConfig:      container.LogConfig{Type: "none"},
			Mounts:         []mount.Mount{{Type: mount.TypeVolume, Source: cfg.Tool.Volume, Target: councilToolDir}},
			Resources:      container.Resources{Memory: toolInstallMemory, MemorySwap: toolInstallMemory, PidsLimit: &pids},
		},
	})
	if err != nil {
		return fmt.Errorf("create tool install container: %w", err)
	}
	if created.ID == "" {
		return errors.New("Docker returned no container ID")
	}
	defer func() {
		cleanup, done := context.WithTimeout(context.WithoutCancel(parent), 10*time.Second)
		defer done()
		if _, rerr := a.Client.ContainerRemove(cleanup, created.ID, client.ContainerRemoveOptions{Force: true}); rerr != nil && !isMissing(rerr) {
			err = errors.Join(err, fmt.Errorf("remove tool install container: %w", rerr))
		}
	}()
	if len(created.Warnings) > 0 {
		return errors.New("Docker reported a warning applying the tool install configuration")
	}
	// The mount names the volume: check again that the name still refers to
	// the labelled tool volume now that the container pins it.
	if err := inspectCouncilToolVolume(ctx, engine, harness, cfg.Tool.Volume); err != nil {
		return err
	}
	stream, err := engine.ContainerAttach(ctx, created.ID, client.ContainerAttachOptions{Stream: true, Stdin: true, Stdout: true, Stderr: true})
	if err != nil {
		return fmt.Errorf("attach tool install container: %w", err)
	}
	if stream.Conn == nil || stream.Reader == nil {
		return errors.New("missing tool install attachment")
	}
	closeStream := sync.OnceFunc(stream.Close)
	defer closeStream()
	stopClose := context.AfterFunc(ctx, closeStream)
	defer stopClose()
	halfClose, ok := stream.Conn.(client.CloseWriter)
	if !ok {
		return errors.New("tool install requires half-close support")
	}
	if _, err := a.Client.ContainerStart(ctx, created.ID, client.ContainerStartOptions{}); err != nil {
		return fmt.Errorf("start tool install container: %w", err)
	}
	var stdout, stderr councilOutput
	drained := make(chan error, 1)
	go func() { drained <- copyDockerStreams(stream.Reader, &stdout, &stderr) }()
	if _, err := io.Copy(stream.Conn, binary); err != nil {
		return errors.Join(errors.New("stream tool into the install container"), ctx.Err())
	}
	if err := halfClose.CloseWrite(); err != nil {
		return fmt.Errorf("close tool install input: %w", err)
	}
	if err := <-drained; err != nil {
		return errors.Join(errors.New("tool install output failed"), ctx.Err())
	}
	result := rt.CouncilResult{ContainerID: created.ID}
	if err := a.councilExit(ctx, engine, &result); err != nil {
		return err
	}
	if result.ExitCode != 0 {
		return fmt.Errorf("tool install exited with status %d: %s", result.ExitCode, strings.TrimSpace(stderr.visible("", "")))
	}
	return nil
}

// ensureCouncilToolVolume creates the labelled tool volume when it does not
// exist and refuses one that exists without the labels.
func (a *Adapter) ensureCouncilToolVolume(ctx context.Context, engine councilEngineAPI, harness, name string) error {
	vol, err := engine.VolumeInspect(ctx, name, client.VolumeInspectOptions{})
	if err != nil {
		if !isMissing(err) {
			return fmt.Errorf("inspect council tool volume: %w", err)
		}
		creator, ok := a.Client.(volumeCreator)
		if !ok {
			return errors.New("Docker cannot create volumes")
		}
		if _, err := creator.VolumeCreate(ctx, client.VolumeCreateOptions{Name: name, Driver: "local", Labels: CouncilToolVolumeLabels(harness)}); err != nil {
			return fmt.Errorf("create tool volume: %w", err)
		}
		if vol, err = engine.VolumeInspect(ctx, name, client.VolumeInspectOptions{}); err != nil {
			return fmt.Errorf("inspect council tool volume: %w", err)
		}
	}
	if err := validateCouncilToolVolume(vol.Volume, harness, name); err != nil {
		return &HarnessError{Harness: harness, Fault: FaultToolInvalid, Err: err}
	}
	return nil
}
