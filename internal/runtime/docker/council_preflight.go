package docker

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/client"
	"github.com/shadewright-labs/sablecourt/internal/redact"
	rt "github.com/shadewright-labs/sablecourt/internal/runtime"
)

// HarnessFault names why a council harness cannot launch. Callers choose the
// message and the command to show, so the runtime never prints user guidance.
type HarnessFault string

const (
	// FaultImageMissing means the local image does not exist. Images are never pulled.
	FaultImageMissing HarnessFault = "image-missing"
	// FaultVolumeMissing means the login volume does not exist yet.
	FaultVolumeMissing HarnessFault = "volume-missing"
	// FaultVolumeInvalid means the volume exists but is not the labelled local volume of this harness.
	FaultVolumeInvalid HarnessFault = "volume-invalid"
	// FaultLoginMissing means the volume holds no credential file for the harness.
	FaultLoginMissing HarnessFault = "login-missing"
	// FaultBackend means Docker or the harness configuration failed, so nothing is known about the login.
	FaultBackend HarnessFault = "backend"
)

// HarnessError is the typed result of a harness preflight.
type HarnessError struct {
	Harness string
	Fault   HarnessFault
	Err     error
}

func (e *HarnessError) Error() string {
	if e.Err == nil {
		return "council harness " + e.Harness + ": " + string(e.Fault)
	}
	return "council harness " + e.Harness + ": " + string(e.Fault) + ": " + redact.String(e.Err.Error())
}

func (e *HarnessError) Unwrap() error { return e.Err }

const (
	loginProbeTimeout = 20 * time.Second
	// The probe only runs test, so it needs almost nothing.
	loginProbeMemory = int64(32 << 20)
	loginProbePIDs   = int64(16)
	// Exit status of the probe script when no credential file exists.
	loginProbeAbsent = 1
)

// The script reads only file names and sizes: test -s never opens the file.
const loginProbeScript = `for f in "$@"; do if test -s "$f"; then exit 0; fi; done; exit 1`

type volumeCreator interface {
	VolumeCreate(context.Context, client.VolumeCreateOptions) (client.VolumeCreateResult, error)
}

// CheckCouncilHarness verifies, with no pull and no network, that the harness
// image exists locally, that its login volume is the labelled local volume, and
// that the volume holds a credential file. Every failure is a *HarnessError.
func (a *Adapter) CheckCouncilHarness(ctx context.Context, harness string) error {
	fault := func(f HarnessFault, err error) error { return &HarnessError{Harness: harness, Fault: f, Err: err} }
	cfg, ok := a.CouncilHarnesses[harness]
	if !ok || cfg.Image == "" || len(cfg.LoginFiles) == 0 || !councilName.MatchString(cfg.LoginVolume) {
		return fault(FaultBackend, errors.New("harness is not configured"))
	}
	engine, ok := a.Client.(councilEngineAPI)
	if !ok {
		return fault(FaultBackend, errors.New("Docker cannot verify a login probe"))
	}
	imageID, err := a.councilImageID(ctx, cfg.Image)
	if err != nil {
		if isMissing(err) {
			return fault(FaultImageMissing, nil)
		}
		return fault(FaultBackend, err)
	}
	vol, err := engine.VolumeInspect(ctx, cfg.LoginVolume, client.VolumeInspectOptions{})
	if err != nil {
		if isMissing(err) {
			return fault(FaultVolumeMissing, nil)
		}
		return fault(FaultBackend, err)
	}
	if err := validateCouncilLoginVolume(vol.Volume, harness, cfg.LoginVolume); err != nil {
		return fault(FaultVolumeInvalid, err)
	}
	present, err := a.probeLogin(ctx, engine, imageID, cfg)
	if err != nil {
		return fault(FaultBackend, err)
	}
	if !present {
		return fault(FaultLoginMissing, nil)
	}
	return nil
}

// EnsureCouncilLoginVolume creates the labelled login volume when it does not
// exist and refuses a volume that exists without the labels. Only the login
// command calls it: the daemon never creates a volume.
func (a *Adapter) EnsureCouncilLoginVolume(ctx context.Context, harness string) error {
	fault := func(f HarnessFault, err error) error { return &HarnessError{Harness: harness, Fault: f, Err: err} }
	cfg, ok := a.CouncilHarnesses[harness]
	if !ok || !councilName.MatchString(cfg.LoginVolume) {
		return fault(FaultBackend, errors.New("harness is not configured"))
	}
	engine, ok := a.Client.(councilEngineAPI)
	if !ok {
		return fault(FaultBackend, errors.New("Docker cannot inspect volumes"))
	}
	vol, err := engine.VolumeInspect(ctx, cfg.LoginVolume, client.VolumeInspectOptions{})
	if err != nil {
		if !isMissing(err) {
			return fault(FaultBackend, err)
		}
		creator, ok := a.Client.(volumeCreator)
		if !ok {
			return fault(FaultBackend, errors.New("Docker cannot create volumes"))
		}
		if _, err := creator.VolumeCreate(ctx, client.VolumeCreateOptions{
			Name: cfg.LoginVolume, Driver: "local", Labels: CouncilLoginVolumeLabels(harness),
		}); err != nil {
			return fault(FaultBackend, fmt.Errorf("create login volume: %w", err))
		}
		// A concurrent creator without our labels must not be trusted.
		if vol, err = engine.VolumeInspect(ctx, cfg.LoginVolume, client.VolumeInspectOptions{}); err != nil {
			return fault(FaultBackend, err)
		}
	}
	if err := validateCouncilLoginVolume(vol.Volume, harness, cfg.LoginVolume); err != nil {
		return fault(FaultVolumeInvalid, err)
	}
	return nil
}

// probeLogin runs one short container from the pinned image: no network, no
// capabilities, read-only root, the volume mounted read-only, no environment
// beyond the base. It reports whether any credential file is non-empty.
func (a *Adapter) probeLogin(parent context.Context, engine councilEngineAPI, imageID string, cfg CouncilHarness) (bool, error) {
	ctx, cancel := context.WithTimeout(parent, loginProbeTimeout)
	defer cancel()
	pids := loginProbePIDs
	cmd := append([]string{"-c", loginProbeScript, "probe"}, cfg.LoginFiles...)
	created, err := a.Client.ContainerCreate(ctx, client.ContainerCreateOptions{
		Name: "sablecourt-login-probe-" + rand.Text(),
		Config: &container.Config{
			Image:       imageID,
			User:        "10001:10001",
			Entrypoint:  []string{"/bin/bash", "--noprofile", "--norc"},
			Cmd:         cmd,
			Env:         append([]string(nil), councilBaseEnv...),
			WorkingDir:  "/",
			Labels:      map[string]string{"sablecourt.io/login-probe": "1"},
			Healthcheck: &container.HealthConfig{Test: []string{"NONE"}},
		},
		HostConfig: &container.HostConfig{
			NetworkMode:    "none",
			ReadonlyRootfs: true,
			CapDrop:        []string{"ALL"},
			SecurityOpt:    []string{"no-new-privileges:true"},
			LogConfig:      container.LogConfig{Type: "none"},
			Mounts:         []mount.Mount{{Type: mount.TypeVolume, Source: cfg.LoginVolume, Target: "/home/harness", ReadOnly: true}},
			Resources:      container.Resources{Memory: loginProbeMemory, MemorySwap: loginProbeMemory, PidsLimit: &pids},
		},
	})
	if err != nil {
		return false, fmt.Errorf("create login probe: %w", err)
	}
	if created.ID == "" {
		return false, errors.New("unverified login probe creation")
	}
	defer func() {
		cleanup, done := context.WithTimeout(context.WithoutCancel(parent), 10*time.Second)
		defer done()
		_, _ = a.Client.ContainerRemove(cleanup, created.ID, client.ContainerRemoveOptions{Force: true})
	}()
	if len(created.Warnings) > 0 {
		return false, errors.New("Docker reported a warning applying the login probe configuration")
	}
	if _, err := a.Client.ContainerStart(ctx, created.ID, client.ContainerStartOptions{}); err != nil {
		return false, fmt.Errorf("start login probe: %w", err)
	}
	result := rt.CouncilResult{ContainerID: created.ID}
	if err := a.councilExit(ctx, engine, &result); err != nil {
		return false, err
	}
	switch result.ExitCode {
	case 0:
		return true, nil
	case loginProbeAbsent:
		return false, nil
	default:
		return false, fmt.Errorf("login probe exited with status %d", result.ExitCode)
	}
}

// IsNotFound reports whether err says Docker has no such image or volume.
func IsNotFound(err error) bool { return isMissing(err) }
