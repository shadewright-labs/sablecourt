package docker

import (
	"context"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/api/types/volume"
	"github.com/moby/moby/client"
	rt "github.com/shadewright-labs/sablecourt/internal/runtime"
)

type probeEngine struct {
	councilEngine
	exit           int
	volumes        map[string]volume.Volume
	created        []client.ContainerCreateOptions
	removed        int
	volCreate      []client.VolumeCreateOptions
	createErr      error
	unlabeled      bool
	createWarnings []string
}

func (p *probeEngine) VolumeInspect(_ context.Context, name string, _ client.VolumeInspectOptions) (client.VolumeInspectResult, error) {
	if p.step == "volume backend" {
		return client.VolumeInspectResult{}, errors.New("engine down")
	}
	v, ok := p.volumes[name]
	if !ok {
		return client.VolumeInspectResult{}, missing{errors.New("no such volume")}
	}
	return client.VolumeInspectResult{Volume: v}, nil
}
func (p *probeEngine) VolumeCreate(_ context.Context, o client.VolumeCreateOptions) (client.VolumeCreateResult, error) {
	p.volCreate = append(p.volCreate, o)
	v := volume.Volume{Name: o.Name, Driver: o.Driver, Labels: o.Labels}
	if p.unlabeled {
		v.Labels = nil
	}
	p.volumes[o.Name] = v
	return client.VolumeCreateResult{Volume: v}, nil
}
func (p *probeEngine) ContainerCreate(_ context.Context, o client.ContainerCreateOptions) (client.ContainerCreateResult, error) {
	p.created = append(p.created, o)
	if p.createErr != nil {
		return client.ContainerCreateResult{}, p.createErr
	}
	return client.ContainerCreateResult{ID: "probe-id", Warnings: p.createWarnings}, nil
}
func (p *probeEngine) ContainerWait(context.Context, string, client.ContainerWaitOptions) client.ContainerWaitResult {
	r := make(chan container.WaitResponse, 1)
	r <- container.WaitResponse{StatusCode: int64(p.exit)}
	return client.ContainerWaitResult{Result: r}
}
func (p *probeEngine) ContainerInspect(context.Context, string, client.ContainerInspectOptions) (client.ContainerInspectResult, error) {
	st := &container.State{Status: "exited", ExitCode: p.exit, FinishedAt: "2026-01-01T01:01:01Z"}
	return client.ContainerInspectResult{Container: container.InspectResponse{ID: "probe-id", State: st}}, nil
}
func (p *probeEngine) ContainerRemove(context.Context, string, client.ContainerRemoveOptions) (client.ContainerRemoveResult, error) {
	p.removed++
	return client.ContainerRemoveResult{}, nil
}

func probeFixture(harness string) (*Adapter, *probeEngine) {
	p := &probeEngine{councilEngine: *newCouncilEngine(), volumes: map[string]volume.Volume{}}
	a := NewAdapter(p, "")
	name := "sablecourt-harness-" + harness
	p.volumes[name] = volume.Volume{Name: name, Driver: "local", Labels: CouncilLoginVolumeLabels(harness)}
	a.CouncilHarnesses = map[string]CouncilHarness{harness: {Image: "local-image", LoginVolume: name, LoginFiles: []string{"/home/harness/.x/cred.json"}}}
	return a, p
}

func TestCheckCouncilHarness(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(*Adapter, *probeEngine)
		fault HarnessFault
		probe bool
	}{
		{name: "login present", probe: true},
		{name: "login missing", setup: func(_ *Adapter, p *probeEngine) { p.exit = 1 }, fault: FaultLoginMissing, probe: true},
		{name: "probe tool fails", setup: func(_ *Adapter, p *probeEngine) { p.exit = 126 }, fault: FaultBackend, probe: true},
		{name: "probe create warns", setup: func(_ *Adapter, p *probeEngine) { p.createWarnings = []string{"unsupported option"} }, fault: FaultBackend, probe: true},
		{name: "probe create fails", setup: func(_ *Adapter, p *probeEngine) { p.createErr = errors.New("denied") }, fault: FaultBackend, probe: true},
		{name: "image missing", setup: func(_ *Adapter, p *probeEngine) { p.imageErr = missing{errors.New("no image")} }, fault: FaultImageMissing},
		{name: "image backend", setup: func(_ *Adapter, p *probeEngine) { p.imageErr = errors.New("engine down") }, fault: FaultBackend},
		{name: "volume missing", setup: func(_ *Adapter, p *probeEngine) { clear(p.volumes) }, fault: FaultVolumeMissing},
		{name: "volume backend", setup: func(_ *Adapter, p *probeEngine) { p.step = "volume backend" }, fault: FaultBackend},
		{name: "volume without labels", setup: func(a *Adapter, p *probeEngine) {
			v := p.volumes["sablecourt-harness-claude"]
			v.Labels = nil
			p.volumes["sablecourt-harness-claude"] = v
		}, fault: FaultVolumeInvalid},
		{name: "volume of another harness", setup: func(_ *Adapter, p *probeEngine) {
			v := p.volumes["sablecourt-harness-claude"]
			v.Labels = CouncilLoginVolumeLabels("codex")
			p.volumes["sablecourt-harness-claude"] = v
		}, fault: FaultVolumeInvalid},
		{name: "volume with options", setup: func(_ *Adapter, p *probeEngine) {
			v := p.volumes["sablecourt-harness-claude"]
			v.Options = map[string]string{"type": "nfs"}
			p.volumes["sablecourt-harness-claude"] = v
		}, fault: FaultVolumeInvalid},
		{name: "not configured", setup: func(a *Adapter, _ *probeEngine) { delete(a.CouncilHarnesses, "claude") }, fault: FaultBackend},
		{name: "no login files", setup: func(a *Adapter, _ *probeEngine) {
			c := a.CouncilHarnesses["claude"]
			c.LoginFiles = nil
			a.CouncilHarnesses["claude"] = c
		}, fault: FaultBackend},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, p := probeFixture("claude")
			if tc.setup != nil {
				tc.setup(a, p)
			}
			err := a.CheckCouncilHarness(t.Context(), "claude")
			if tc.fault == "" {
				if err != nil {
					t.Fatal(err)
				}
			} else {
				var he *HarnessError
				if !errors.As(err, &he) || he.Fault != tc.fault || he.Harness != "claude" {
					t.Fatalf("error = %v, want fault %s", err, tc.fault)
				}
			}
			if tc.probe != (len(p.created) == 1) {
				t.Fatalf("probe containers created = %d", len(p.created))
			}
			if tc.probe && p.removed != 1 && p.createErr == nil {
				t.Fatal("probe container was not removed")
			}
			if p.pulled {
				t.Fatal("an image was pulled")
			}
		})
	}
}

func TestLoginProbeIsolation(t *testing.T) {
	a, p := probeFixture("claude")
	if err := a.CheckCouncilHarness(t.Context(), "claude"); err != nil {
		t.Fatal(err)
	}
	o := p.created[0]
	h, c := o.HostConfig, o.Config
	if string(h.NetworkMode) != "none" || !h.ReadonlyRootfs || h.Privileged || !slices.Equal(h.CapDrop, []string{"ALL"}) ||
		!slices.Contains(h.SecurityOpt, "no-new-privileges:true") || c.User != "10001:10001" || c.Image != p.imageID {
		t.Fatalf("probe is not isolated: %+v %+v", h, c)
	}
	want := []mount.Mount{{Type: mount.TypeVolume, Source: "sablecourt-harness-claude", Target: "/home/harness", ReadOnly: true}}
	if len(h.Mounts) != 1 || h.Mounts[0] != want[0] || len(h.Binds) != 0 {
		t.Fatalf("mounts = %+v", h.Mounts)
	}
	for _, e := range c.Env {
		if !slices.Contains(councilBaseEnv, e) {
			t.Fatalf("unexpected env %q", e)
		}
	}
	if !slices.Contains(c.Cmd, "/home/harness/.x/cred.json") || strings.Contains(strings.Join(c.Cmd, " "), "cat ") {
		t.Fatalf("probe command = %v", c.Cmd)
	}
}

func TestEnsureCouncilLoginVolume(t *testing.T) {
	t.Run("creates with labels", func(t *testing.T) {
		a, p := probeFixture("claude")
		clear(p.volumes)
		if err := a.EnsureCouncilLoginVolume(t.Context(), "claude"); err != nil {
			t.Fatal(err)
		}
		if len(p.volCreate) != 1 || p.volCreate[0].Driver != "local" || p.volCreate[0].Labels["sablecourt.io/harness"] != "claude" {
			t.Fatalf("create = %+v", p.volCreate)
		}
	})
	t.Run("keeps a labelled volume", func(t *testing.T) {
		a, p := probeFixture("claude")
		if err := a.EnsureCouncilLoginVolume(t.Context(), "claude"); err != nil || len(p.volCreate) != 0 {
			t.Fatalf("err=%v creates=%d", err, len(p.volCreate))
		}
	})
	t.Run("refuses a volume without labels", func(t *testing.T) {
		a, p := probeFixture("claude")
		p.volumes["sablecourt-harness-claude"] = volume.Volume{Name: "sablecourt-harness-claude", Driver: "local"}
		var he *HarnessError
		if err := a.EnsureCouncilLoginVolume(t.Context(), "claude"); !errors.As(err, &he) || he.Fault != FaultVolumeInvalid || len(p.volCreate) != 0 {
			t.Fatalf("err=%v", err)
		}
	})
	t.Run("refuses a volume replaced during creation", func(t *testing.T) {
		a, p := probeFixture("claude")
		clear(p.volumes)
		p.unlabeled = true
		var he *HarnessError
		if err := a.EnsureCouncilLoginVolume(t.Context(), "claude"); !errors.As(err, &he) || he.Fault != FaultVolumeInvalid {
			t.Fatalf("err=%v", err)
		}
	})
	t.Run("backend error", func(t *testing.T) {
		a, p := probeFixture("claude")
		p.step = "volume backend"
		var he *HarnessError
		if err := a.EnsureCouncilLoginVolume(t.Context(), "claude"); !errors.As(err, &he) || he.Fault != FaultBackend {
			t.Fatalf("err=%v", err)
		}
	})
}

// A login session has no gate: the endpoint of a login-mode proxy carries none.
func TestCouncilLoginWithoutGate(t *testing.T) {
	a, f, s := fixtureRun(t)
	cfg := a.CouncilHarnesses["claude"]
	cfg.DataGateURL = ""
	a.CouncilHarnesses["claude"] = cfg
	s.ProxyURL, s.DataGateURL = "http://relay:8888", ""
	s.Checkout, s.Prompt, s.TaskToken, s.Model = rt.CouncilCheckout{}, "", "", ""
	_, err := a.RunCouncilLogin(t.Context(), s, func(context.Context, io.Reader, io.Writer) error { return nil }, discardResult)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range f.createdOpts.Config.Env {
		if strings.HasPrefix(e, "SABLECOURT_DATA_GATE_URL") {
			t.Fatal("login carries a gate URL")
		}
	}
	if !slices.Contains(f.createdOpts.Config.Env, "NO_PROXY=relay") {
		t.Fatalf("env = %v", f.createdOpts.Config.Env)
	}
}
