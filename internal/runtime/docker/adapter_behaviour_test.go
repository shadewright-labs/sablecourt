package docker

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"slices"
	"strings"
	"testing"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/api/types/volume"
	"github.com/moby/moby/client"

	"github.com/shadewright-labs/sablecourt/internal/attach"
	"github.com/shadewright-labs/sablecourt/internal/domain"
	"github.com/shadewright-labs/sablecourt/internal/gate/councilproxy"
	scruntime "github.com/shadewright-labs/sablecourt/internal/runtime"
)

type pingEngine struct {
	fakeEngine
	pingErr error
	pinged  bool
}

func (p *pingEngine) Ping(context.Context, client.PingOptions) (client.PingResult, error) {
	p.pinged = true
	return client.PingResult{}, p.pingErr
}

type fullAttachEngine struct {
	fakeEngine
	attachConn   net.Conn
	attachErr    error
	startErr     error
	createErr    error
	stopErr      error
	removeErr    error
	inspectErr   error
	logsBody     io.ReadCloser
	logsErr      error
	logsCalled   chan struct{}
	networkErr   error
	createNetErr error
}

func (e *fullAttachEngine) NetworkInspect(ctx context.Context, name string, opts client.NetworkInspectOptions) (client.NetworkInspectResult, error) {
	if e.networkErr != nil {
		return client.NetworkInspectResult{}, e.networkErr
	}
	return e.fakeEngine.NetworkInspect(ctx, name, opts)
}

func (e *fullAttachEngine) NetworkCreate(ctx context.Context, name string, opts client.NetworkCreateOptions) (client.NetworkCreateResult, error) {
	if e.createNetErr != nil {
		return client.NetworkCreateResult{}, e.createNetErr
	}
	return e.fakeEngine.NetworkCreate(ctx, name, opts)
}

func (e *fullAttachEngine) ContainerCreate(ctx context.Context, opts client.ContainerCreateOptions) (client.ContainerCreateResult, error) {
	if e.createErr != nil {
		return client.ContainerCreateResult{}, e.createErr
	}
	return e.fakeEngine.ContainerCreate(ctx, opts)
}

func (e *fullAttachEngine) ContainerAttach(context.Context, string, client.ContainerAttachOptions) (client.ContainerAttachResult, error) {
	if e.attachErr != nil {
		return client.ContainerAttachResult{}, e.attachErr
	}
	return client.ContainerAttachResult{
		HijackedResponse: client.HijackedResponse{Conn: e.attachConn},
	}, nil
}

func (e *fullAttachEngine) ContainerStart(ctx context.Context, id string, opts client.ContainerStartOptions) (client.ContainerStartResult, error) {
	if e.startErr != nil {
		return client.ContainerStartResult{}, e.startErr
	}
	return e.fakeEngine.ContainerStart(ctx, id, opts)
}

func (e *fullAttachEngine) ContainerLogs(context.Context, string, client.ContainerLogsOptions) (client.ContainerLogsResult, error) {
	if e.logsCalled != nil {
		close(e.logsCalled)
	}
	if e.logsErr != nil {
		return nil, e.logsErr
	}
	return e.logsBody, nil
}

func (e *fullAttachEngine) ContainerInspect(ctx context.Context, id string, opts client.ContainerInspectOptions) (client.ContainerInspectResult, error) {
	if e.inspectErr != nil {
		return client.ContainerInspectResult{}, e.inspectErr
	}
	return e.fakeEngine.ContainerInspect(ctx, id, opts)
}

func (e *fullAttachEngine) ContainerStop(ctx context.Context, id string, opts client.ContainerStopOptions) (client.ContainerStopResult, error) {
	if e.stopErr != nil {
		return client.ContainerStopResult{}, e.stopErr
	}
	return e.fakeEngine.ContainerStop(ctx, id, opts)
}

func (e *fullAttachEngine) ContainerRemove(ctx context.Context, id string, opts client.ContainerRemoveOptions) (client.ContainerRemoveResult, error) {
	if e.removeErr != nil {
		return client.ContainerRemoveResult{}, e.removeErr
	}
	return e.fakeEngine.ContainerRemove(ctx, id, opts)
}

type stdinOnlyEngine struct {
	fakeEngine
	conn net.Conn
}

func (e *stdinOnlyEngine) ContainerAttach(context.Context, string, client.ContainerAttachOptions) (client.ContainerAttachResult, error) {
	return client.ContainerAttachResult{HijackedResponse: client.HijackedResponse{Conn: e.conn}}, nil
}

type trackingConn struct {
	fixtureConn
	closed bool
}

func (c *trackingConn) Close() error {
	c.closed = true
	return nil
}

func TestDockerAdapterPingImageNetworkAndLifecycle(t *testing.T) {
	ctx := context.Background()

	t.Run("Ping", func(t *testing.T) {
		var nilAdapter *Adapter
		if err := nilAdapter.Ping(ctx); err == nil {
			t.Fatal("nil adapter Ping succeeded")
		}
		if err := testAdapter(nil, "").Ping(ctx); err == nil {
			t.Fatal("nil client Ping succeeded")
		}
		if err := testAdapter(&fakeEngine{}, "").Ping(ctx); err != nil {
			t.Fatalf("non-pinger client Ping = %v, want nil", err)
		}
		pe := &pingEngine{}
		if err := testAdapter(pe, "").Ping(ctx); err != nil || !pe.pinged {
			t.Fatalf("pingEngine Ping = %v, pinged=%t", err, pe.pinged)
		}
		pe.pingErr = errors.New("daemon unreachable")
		if err := testAdapter(pe, "").Ping(ctx); !errors.Is(err, pe.pingErr) {
			t.Fatalf("pingEngine error = %v, want %v", err, pe.pingErr)
		}
	})

	t.Run("EnsureInternalNetwork EnsureImage ResolveLocalImage", func(t *testing.T) {
		eng := &fullAttachEngine{}
		a := testAdapter(eng, "")
		if err := a.EnsureInternalNetwork(ctx); err != nil || !eng.networkExists {
			t.Fatalf("EnsureInternalNetwork = %v, exists=%t", err, eng.networkExists)
		}
		badNet := &fullAttachEngine{createNetErr: errors.New("create denied")}
		if err := testAdapter(badNet, "").EnsureInternalNetwork(ctx); err == nil || !strings.Contains(err.Error(), "create denied") {
			t.Fatalf("failed network create = %v", err)
		}
		if err := a.EnsureImage(ctx, "fixture:1"); err != nil {
			t.Fatalf("EnsureImage = %v", err)
		}
		id, err := a.ResolveLocalImage(ctx, "fixture:1")
		if err != nil || !strings.HasPrefix(id, "sha256:") {
			t.Fatalf("ResolveLocalImage = %q, %v", id, err)
		}
		badImg := &inspectEngine{imageErr: errors.New("no image")}
		if _, err := testAdapter(badImg, "").ResolveLocalImage(ctx, "absent"); err == nil {
			t.Fatal("ResolveLocalImage accepted inspect failure")
		}
	})

	t.Run("Start env, refusal and attach streaming", func(t *testing.T) {
		eng := &fullAttachEngine{}
		a := testAdapter(eng, "")
		if _, err := a.Start(ctx, scruntime.LaunchSpec{TaskID: "t0"}); err == nil || !strings.Contains(err.Error(), "runtime.image") {
			t.Fatalf("missing image Start = %v", err)
		}

		// ContainerCreate failure propagates.
		eng.createErr = errors.New("quota exceeded")
		spec := scruntime.LaunchSpec{
			TaskID:      "t1",
			InputJSON:   `{"goal":"check"}`,
			DataGateURL: "http://host.docker.internal:7432/gate/mcp",
			Workload:    domain.Workload{Spec: domain.WorkloadSpec{Runtime: domain.RuntimeConfig{Image: "img:1"}}},
		}
		if _, err := a.Start(ctx, spec); err == nil || !strings.Contains(err.Error(), "quota exceeded") {
			t.Fatalf("create failure Start = %v", err)
		}
		eng.createErr = nil

		// Start failure closes attached stdin conn.
		conn := &trackingConn{}
		eng.attachConn = conn
		eng.startErr = errors.New("start refused")
		a.AttachHub = attach.NewHub()
		if _, err := a.Start(ctx, spec); err == nil || !conn.closed {
			t.Fatalf("start failure err=%v connClosed=%t", err, conn.closed)
		}
		eng.startErr = nil

		// Successful start with log streamer demultiplexes output into AttachHub.
		conn2 := &trackingConn{}
		eng.attachConn = conn2
		pr, pw := io.Pipe()
		eng.logsBody = pr
		eng.logsCalled = make(chan struct{})
		hub := attach.NewHub()
		a.AttachHub = hub
		if _, err := a.Start(ctx, spec); err != nil {
			t.Fatalf("Start with attach: %v", err)
		}
		history, ch, _, unsub, err := hub.Subscribe(spec.TaskID)
		if err != nil {
			t.Fatalf("Subscribe: %v", err)
		}
		defer unsub()
		<-eng.logsCalled
		_, _ = pw.Write(append(frame(1, "hello "), frame(2, "world\n")...))
		_ = pw.Close()
		var got strings.Builder
		got.WriteString(history)
		for chunk := range ch {
			got.WriteString(chunk)
		}
		if !slices.Contains(eng.createdOpts.Config.Env, "SABLECOURT_INPUT_JSON="+spec.InputJSON) ||
			!slices.Contains(eng.createdOpts.Config.Env, "SABLECOURT_DATA_GATE_URL="+spec.DataGateURL) {
			t.Fatalf("missing env vars: %v", eng.createdOpts.Config.Env)
		}
		if got.String() != "hello world\n" {
			t.Fatalf("hub output = %q, want %q", got.String(), "hello world\n")
		}

		// Engine with attach only (no logStreamer) still registers stdin in hub.
		stdinEng := &stdinOnlyEngine{conn: &trackingConn{}}
		a2 := testAdapter(stdinEng, "")
		a2.AttachHub = attach.NewHub()
		if _, err := a2.Start(ctx, spec); err != nil {
			t.Fatalf("Start stdin-only: %v", err)
		}
		if active, _, _ := a2.AttachHub.Status(spec.TaskID); !active {
			t.Fatal("stdin-only attach was not registered")
		}
		a2.AttachHub.Close(spec.TaskID)
	})

	t.Run("Observe states", func(t *testing.T) {
		for _, tc := range []struct {
			name     string
			state    *container.State
			err      error
			want     scruntime.ObservedState
			exitCode int
		}{
			{"inspect error", nil, errors.New("boom"), "", 0},
			{"nil state", nil, nil, scruntime.StateUnknown, 0},
			{"paused", &container.State{Paused: true, Running: true}, nil, scruntime.StateSuspended, 0},
			{"running", &container.State{Running: true, Status: "running"}, nil, scruntime.StateRunning, 0},
			{"exited ok", &container.State{Status: "exited", ExitCode: 0}, nil, scruntime.StateCompleted, 0},
			{"exited nonzero", &container.State{Status: "exited", ExitCode: 42}, nil, scruntime.StateFailed, 42},
			{"dead", &container.State{Status: "dead", ExitCode: 137}, nil, scruntime.StateFailed, 137},
		} {
			t.Run(tc.name, func(t *testing.T) {
				eng := &fullAttachEngine{fakeEngine: fakeEngine{inspectState: tc.state}, inspectErr: tc.err}
				obs, err := testAdapter(eng, "").Observe(ctx, scruntime.Handle{ExternalID: "c1"})
				if tc.err != nil {
					if err == nil {
						t.Fatal("expected inspect error")
					}
					return
				}
				if err != nil || obs.State != tc.want || obs.ExitCode != tc.exitCode {
					t.Fatalf("Observe = %+v, %v; want %s exit %d", obs, err, tc.want, tc.exitCode)
				}
			})
		}
	})

	t.Run("Suspend Resume Stop", func(t *testing.T) {
		eng := &fullAttachEngine{}
		a := testAdapter(eng, "")
		h := scruntime.Handle{ExternalID: "ctr-9", WorkDir: "/work"}
		cp, err := a.Suspend(ctx, h)
		if err != nil || cp.ID != "cp-ctr-9" || cp.Path != "/work" || cp.CreatedAt.IsZero() {
			t.Fatalf("Suspend = %+v, %v", cp, err)
		}
		if err := a.Resume(ctx, h, cp); err != nil || eng.startedID != "ctr-9" {
			t.Fatalf("Resume = %v, started=%q", err, eng.startedID)
		}
		eng.startErr = errors.New("cannot start")
		if err := a.Resume(ctx, h, cp); err == nil {
			t.Fatal("Resume ignored start error")
		}
		eng.stopErr = errors.New("stop failed")
		eng.removeErr = missing{errors.New("gone")}
		if err := a.Stop(ctx, h); err != nil {
			t.Fatalf("Stop with missing remove = %v, want nil", err)
		}
		eng.removeErr = errors.New("remove failed")
		if err := a.Stop(ctx, h); err == nil || !strings.Contains(err.Error(), "stop failed") || !strings.Contains(err.Error(), "remove failed") {
			t.Fatalf("Stop joined error = %v", err)
		}
	})
}

func TestRecoveryInvalidHandleAndFailedExit(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name                             string
		handle                           scruntime.Handle
		state                            *container.State
		wantAlive, wantExited, wantError bool
	}{
		{"wrong adapter name", scruntime.Handle{AdapterName: "coding", ExternalID: "c1"}, nil, false, false, true},
		{"empty external ID", scruntime.Handle{AdapterName: "docker", ExternalID: ""}, nil, false, false, true},
		{"failed exit", scruntime.Handle{AdapterName: "docker", ExternalID: "c1"}, &container.State{Status: "exited", ExitCode: 7, FinishedAt: "2026-01-02T03:04:05Z"}, false, true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := testAdapter(&recoveryEngine{id: "c1", state: tc.state}, "")
			alive, err := a.HandleAlive(ctx, tc.handle)
			if alive != tc.wantAlive || (err != nil) != tc.wantError {
				t.Fatalf("HandleAlive = %t, %v", alive, err)
			}
			exited, err := a.HandleExited(ctx, tc.handle)
			if exited != tc.wantExited || (err != nil) != tc.wantError {
				t.Fatalf("HandleExited = %t, %v", exited, err)
			}
		})
	}
}

type shortWriter struct {
	n   int
	err error
}

func (w shortWriter) Write([]byte) (int, error) { return w.n, w.err }

func TestStreamsErrorAndOverflowPaths(t *testing.T) {
	if err := copyDockerStreams(bytes.NewReader(frame(1, "abc")), shortWriter{err: errors.New("disk full")}, io.Discard); err == nil || !strings.Contains(err.Error(), "disk full") {
		t.Fatalf("writer error = %v", err)
	}
	if err := copyDockerStreams(bytes.NewReader(frame(1, "abc")), shortWriter{n: 1}, io.Discard); !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("short write = %v, want io.ErrShortWrite", err)
	}
	var out councilOutput
	_, _ = out.Write([]byte(strings.Repeat("a", maxCouncilOutput+1)))
	n, err := out.Write([]byte("more after overflow"))
	if err != nil || n != len("more after overflow") || out.visible("", "") != outputLimitMarker {
		t.Fatalf("overflow write n=%d err=%v visible=%q", n, err, out.visible("", ""))
	}
}

func TestHarnessErrorAndPreflightRefusals(t *testing.T) {
	tok := "sk-" + strings.Repeat("x", 28)
	withErr := &HarnessError{Harness: "claude", Fault: FaultBackend, Err: errors.New("denied " + tok)}
	if !errors.Is(withErr, withErr.Err) {
		t.Fatal("Unwrap did not match inner error")
	}
	if msg := withErr.Error(); strings.Contains(msg, tok) || !strings.Contains(msg, "council harness claude: backend:") {
		t.Fatalf("HarnessError.Error() = %q", msg)
	}
	withoutErr := &HarnessError{Harness: "codex", Fault: FaultImageMissing}
	if got := withoutErr.Error(); got != "council harness codex: image-missing" {
		t.Fatalf("HarnessError without Err = %q", got)
	}
	if !IsNotFound(missing{errors.New("gone")}) || IsNotFound(errors.New("other")) {
		t.Fatal("IsNotFound mismatch")
	}

	// Engine missing councilEngineAPI refuses CheckCouncilHarness and EnsureCouncilLoginVolume.
	plain := testAdapter(&fakeEngine{}, "")
	plain.CouncilHarnesses = map[string]CouncilHarness{
		"claude": {Image: "img", LoginVolume: "sablecourt-claude", LoginFiles: []string{"/data/harness/c"}},
	}
	var he *HarnessError
	if err := plain.CheckCouncilHarness(context.Background(), "claude"); !errors.As(err, &he) || he.Fault != FaultBackend {
		t.Fatalf("CheckCouncilHarness without councilEngineAPI = %v", err)
	}
	if err := plain.EnsureCouncilLoginVolume(context.Background(), "claude"); !errors.As(err, &he) || he.Fault != FaultBackend {
		t.Fatalf("EnsureCouncilLoginVolume without councilEngineAPI = %v", err)
	}
	if err := plain.EnsureCouncilLoginVolume(context.Background(), "unknown"); !errors.As(err, &he) || he.Fault != FaultBackend {
		t.Fatalf("EnsureCouncilLoginVolume unknown = %v", err)
	}

	// Engine with councilEngineAPI but without volumeCreator refuses creation.
	noCreate := &missingVolumeEngine{councilEngine: newCouncilEngine()}
	aNoCreate := testAdapter(noCreate, "")
	aNoCreate.CouncilHarnesses = map[string]CouncilHarness{"claude": {Image: "img", LoginVolume: "sablecourt-claude"}}
	if err := aNoCreate.EnsureCouncilLoginVolume(context.Background(), "claude"); !errors.As(err, &he) || he.Fault != FaultBackend {
		t.Fatalf("EnsureCouncilLoginVolume without volumeCreator = %v", err)
	}
}

type missingVolumeEngine struct {
	*councilEngine
}

func (e *missingVolumeEngine) VolumeInspect(context.Context, string, client.VolumeInspectOptions) (client.VolumeInspectResult, error) {
	return client.VolumeInspectResult{}, missing{errors.New("missing")}
}

func TestInspectAndCleanupCouncilRefusals(t *testing.T) {
	ctx := context.Background()
	t.Run("InspectCouncil cases", func(t *testing.T) {
		for _, tc := range []struct {
			name     string
			id       scruntime.CouncilIdentity
			resp     container.InspectResponse
			err      error
			wantErr  string
			exists   bool
			running  bool
			exited   bool
			exitCode int
		}{
			{name: "missing identity", id: scruntime.CouncilIdentity{}, wantErr: "missing council container identity"},
			{name: "not found", id: scruntime.CouncilIdentity{ContainerID: "c1"}, err: missing{errors.New("gone")}, exists: false},
			{name: "inspect backend error", id: scruntime.CouncilIdentity{ContainerID: "c1"}, err: errors.New("engine error"), wantErr: "engine error"},
			{name: "incomplete evidence nil config", id: scruntime.CouncilIdentity{ContainerID: "c1"}, resp: container.InspectResponse{ID: "c1"}, wantErr: "incomplete council container inspect evidence"},
			{name: "ID mismatch", id: scruntime.CouncilIdentity{ContainerID: "c1"}, resp: container.InspectResponse{ID: "c2", Config: &container.Config{}}, exists: false},
			{name: "attempt mismatch", id: scruntime.CouncilIdentity{ContainerName: "n1", CreateAttempt: "a1"}, resp: container.InspectResponse{ID: "c1", Config: &container.Config{Labels: map[string]string{attemptLabel: "a2"}}}, exists: false},
			{name: "nil state", id: scruntime.CouncilIdentity{ContainerID: "c1"}, resp: container.InspectResponse{ID: "c1", Config: &container.Config{}}, exists: true},
			{name: "running", id: scruntime.CouncilIdentity{ContainerID: "c1"}, resp: container.InspectResponse{ID: "c1", Config: &container.Config{}, State: &container.State{Running: true, Status: "running"}}, exists: true, running: true},
			{name: "exited", id: scruntime.CouncilIdentity{ContainerID: "c1"}, resp: container.InspectResponse{ID: "c1", Config: &container.Config{}, State: &container.State{Status: "exited", ExitCode: 5, FinishedAt: "2026-01-01T00:00:00Z"}}, exists: true, exited: true, exitCode: 5},
		} {
			t.Run(tc.name, func(t *testing.T) {
				eng := &inspectOnlyCouncilEngine{resp: tc.resp, err: tc.err}
				got, err := testAdapter(eng, "").InspectCouncil(ctx, tc.id)
				if tc.wantErr != "" {
					if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
						t.Fatalf("InspectCouncil = %+v, %v; want %q", got, err, tc.wantErr)
					}
					return
				}
				if err != nil || got.Exists != tc.exists || got.Running != tc.running || got.Exited != tc.exited || got.ExitCode != tc.exitCode {
					t.Fatalf("InspectCouncil = %+v, %v", got, err)
				}
			})
		}
	})

	t.Run("CleanupCouncil refusals", func(t *testing.T) {
		a := testAdapter(&inspectOnlyCouncilEngine{}, "")
		if err := a.CleanupCouncil(ctx, scruntime.CouncilResult{}); err == nil || !strings.Contains(err.Error(), "missing council create identity") {
			t.Fatalf("missing identity cleanup = %v", err)
		}
		engIncomplete := &inspectOnlyCouncilEngine{resp: container.InspectResponse{ID: "c1"}}
		if err := testAdapter(engIncomplete, "").CleanupCouncil(ctx, scruntime.CouncilResult{ContainerName: "n", CreateAttempt: "a"}); err == nil || !strings.Contains(err.Error(), "incomplete council create evidence") {
			t.Fatalf("incomplete cleanup = %v", err)
		}
		engGone := &inspectOnlyCouncilEngine{removeErr: missing{errors.New("gone")}}
		if err := testAdapter(engGone, "").CleanupCouncil(ctx, scruntime.CouncilResult{ContainerID: "c1"}); err != nil {
			t.Fatalf("remove not found = %v, want nil", err)
		}
	})
}

type inspectOnlyCouncilEngine struct {
	fakeEngine
	resp      container.InspectResponse
	err       error
	removeErr error
}

func (e *inspectOnlyCouncilEngine) ContainerInspect(context.Context, string, client.ContainerInspectOptions) (client.ContainerInspectResult, error) {
	return client.ContainerInspectResult{Container: e.resp}, e.err
}

func (e *inspectOnlyCouncilEngine) ContainerRemove(context.Context, string, client.ContainerRemoveOptions) (client.ContainerRemoveResult, error) {
	return client.ContainerRemoveResult{}, e.removeErr
}

func TestCouncilConfigAndProxyRefusals(t *testing.T) {
	ctx := context.Background()

	t.Run("RunCouncilLogin rejects invalid session inputs", func(t *testing.T) {
		a, _, s := fixtureRun(t)
		s.Checkout, s.Prompt, s.TaskToken, s.Model = scruntime.CouncilCheckout{}, "", "", ""
		noop := func(context.Context, io.Reader, io.Writer) error { return nil }
		for _, tc := range []struct {
			name   string
			sess   func(context.Context, io.Reader, io.Writer) error
			mutate func(*scruntime.CouncilLaunch)
		}{
			{"nil session", nil, nil},
			{"non-empty prompt", noop, func(l *scruntime.CouncilLaunch) { l.Prompt = "hi" }},
			{"non-empty token", noop, func(l *scruntime.CouncilLaunch) { l.TaskToken = "tok" }},
			{"non-empty model", noop, func(l *scruntime.CouncilLaunch) { l.Model = "m" }},
		} {
			t.Run(tc.name, func(t *testing.T) {
				launch := s
				if tc.mutate != nil {
					tc.mutate(&launch)
				}
				if _, err := a.RunCouncilLogin(ctx, launch, tc.sess, discardResult); err == nil || !strings.Contains(err.Error(), "invalid council login session") {
					t.Fatalf("RunCouncilLogin = %v", err)
				}
			})
		}
	})

	t.Run("councilCommand and preparePrompt refusals", func(t *testing.T) {
		if _, err := councilCommand(scruntime.CouncilLaunch{Harness: "unknown"}, false, nil); err == nil {
			t.Fatal("accepted unknown harness")
		}
		badPrompt := "here is -----BEGIN " + "PRIVATE KEY-----\nsecret"
		if _, err := preparePrompt(scruntime.CouncilLaunch{TaskToken: "tok", Prompt: badPrompt}); err == nil || !strings.Contains(err.Error(), "unsafe council prompt") {
			t.Fatalf("preparePrompt private key = %v", err)
		}
	})

	t.Run("harnessContainerSpec shared volume, invalid names and replaced network", func(t *testing.T) {
		a, eng, s := fixtureRun(t)
		a.CouncilHarnesses["codex"] = CouncilHarness{Image: "other", LoginVolume: a.CouncilHarnesses["claude"].LoginVolume}
		if _, err := a.harnessContainerSpec(ctx, eng, s, false); err == nil || !strings.Contains(err.Error(), "separate login volumes") {
			t.Fatalf("shared login volume = %v", err)
		}
		delete(a.CouncilHarnesses, "codex")

		badTask := s
		badTask.TaskID = "../bad"
		if _, err := a.harnessContainerSpec(ctx, eng, badTask, false); err == nil || !strings.Contains(err.Error(), "invalid council task or workload name") {
			t.Fatalf("bad task ID = %v", err)
		}

		badNet := s
		badNet.Network = "bad net"
		if _, err := a.harnessContainerSpec(ctx, eng, badNet, false); err == nil || !strings.Contains(err.Error(), "invalid council network name") {
			t.Fatalf("bad network name = %v", err)
		}

		replacedNet := s
		replacedNet.NetworkID = "wrong-net-id"
		if _, err := a.harnessContainerSpec(ctx, eng, replacedNet, false); err == nil || !strings.Contains(err.Error(), "network was replaced") {
			t.Fatalf("replaced network = %v", err)
		}
	})

	t.Run("proxy adapter capability and name refusals", func(t *testing.T) {
		plain := testAdapter(&fakeEngine{}, "")
		if err := plain.ProbeGateSocket(ctx, councilproxy.GateSocketProbe{Name: "p"}); err == nil {
			t.Fatal("ProbeGateSocket succeeded without councilEngineAPI")
		}
		if _, err := plain.CreateInternalNetwork(ctx, councilproxy.InternalNetworkSpec{Name: "bad name"}); err == nil {
			t.Fatal("CreateInternalNetwork accepted bad name")
		}
		if _, err := plain.CreateInternalNetwork(ctx, councilproxy.InternalNetworkSpec{Name: "ok-net"}); err == nil {
			t.Fatal("CreateInternalNetwork succeeded without ServerVersion")
		}
		if err := plain.CreateProxy(ctx, councilproxy.ProxyContainer{Name: "bad name", InternalNetwork: "ok"}); err == nil {
			t.Fatal("CreateProxy accepted bad name")
		}
		if err := plain.ConnectProxyEgress(ctx, "px"); err == nil {
			t.Fatal("ConnectProxyEgress succeeded without proxyEngineAPI")
		}
		if err := plain.StartProxy(ctx, "px"); err == nil {
			t.Fatal("StartProxy succeeded without proxyExecAPI")
		}
		if err := plain.RemoveProxy(ctx, ""); err != nil {
			t.Fatalf("RemoveProxy empty = %v", err)
		}
		if err := plain.RemoveInternalNetwork(ctx, ""); err != nil {
			t.Fatalf("RemoveInternalNetwork empty = %v", err)
		}
		if err := plain.RemoveInternalNetwork(ctx, "net"); err == nil {
			t.Fatal("RemoveInternalNetwork succeeded without proxyEngineAPI")
		}
	})

	t.Run("ProvisionCouncilProxy error returns nil session", func(t *testing.T) {
		plain := testAdapter(&fakeEngine{}, "")
		sess, err := plain.ProvisionCouncilProxy(ctx, "sha256:"+strings.Repeat("a", 64), councilproxy.Config{})
		if err == nil || sess != nil {
			t.Fatalf("ProvisionCouncilProxy = %v, %v; want nil session and error", sess, err)
		}
	})

	t.Run("probeLogin failure branches", func(t *testing.T) {
		a, p := probeFixture("claude")
		p.volumes["sablecourt-harness-claude"] = volume.Volume{Name: "sablecourt-harness-claude", Driver: "local", Labels: CouncilLoginVolumeLabels("claude")}
		// Trigger non-ready wait error in councilExit via error channel.
		we := &waitErrorEngine{councilEngine: newCouncilEngine(), sendErr: true}
		aWe := testAdapter(we, "")
		aWe.CouncilHarnesses = a.CouncilHarnesses
		res := scruntime.CouncilResult{ContainerID: "cid"}
		if err := aWe.councilExit(ctx, we, &res); err == nil || !strings.Contains(err.Error(), "could not verify council exit") {
			t.Fatalf("councilExit error channel = %v", err)
		}
		we.sendErr = false
		we.closeResult = true
		if err := aWe.councilExit(ctx, we, &res); err == nil || !strings.Contains(err.Error(), "missing council wait status") {
			t.Fatalf("councilExit closed channel = %v", err)
		}
	})

	t.Run("StartProxy refuses stopped container and nil reader", func(t *testing.T) {
		eng := &nilReaderProxyEngine{readinessEngine: &readinessEngine{fakeProxyEngine: newFakeProxyEngine()}}
		eng.containers["proxy"] = container.InspectResponse{ID: "cid", Config: &container.Config{}, State: &container.State{Running: true}}
		a := testAdapter(eng, "")
		if err := a.StartProxy(ctx, "proxy"); err == nil || !strings.Contains(err.Error(), "missing council proxy authentication challenge") {
			t.Fatalf("nil reader StartProxy = %v", err)
		}
		eng.notRunning = true
		if err := a.StartProxy(ctx, "proxy"); err == nil || !strings.Contains(err.Error(), "council proxy is not running") {
			t.Fatalf("stopped container StartProxy = %v", err)
		}
		// proxyProbeOutcome rejects oversized streams.
		huge := bufio.NewReader(bytes.NewReader(frame(1, strings.Repeat("4", 300))))
		if got := proxyProbeOutcome(huge); got != probeInvalid {
			t.Fatalf("oversized probe outcome = %v, want probeInvalid", got)
		}
	})

	t.Run("CreateProxy warnings and empty ID", func(t *testing.T) {
		eng := &warnProxyEngine{fakeProxyEngine: newFakeProxyEngine(), emptyID: true}
		a := testAdapter(eng, "")
		info, err := a.CreateInternalNetwork(ctx, councilproxy.InternalNetworkSpec{Name: "pnet"})
		if err != nil {
			t.Fatal(err)
		}
		spec := councilproxy.ProxyContainer{Name: "p1", ImageID: eng.imageID, InternalNetwork: "pnet", InternalNetworkID: info.ID}
		if err := a.CreateProxy(ctx, spec); err == nil || !strings.Contains(err.Error(), "no proxy container ID") {
			t.Fatalf("empty ID CreateProxy = %v", err)
		}
		eng.emptyID = false
		eng.warnings = []string{"warn"}
		if err := a.CreateProxy(ctx, spec); err == nil || !strings.Contains(err.Error(), "reported a warning") {
			t.Fatalf("warnings CreateProxy = %v", err)
		}
		// Mismatched image ID from inspect.
		eng.warnings = nil
		eng.forceImageID = "sha256:" + strings.Repeat("c", 64)
		spec.ImageID = "sha256:" + strings.Repeat("b", 64)
		if err := a.CreateProxy(ctx, spec); err == nil || !strings.Contains(err.Error(), "image ID mismatch") {
			t.Fatalf("image mismatch CreateProxy = %v", err)
		}
	})
}

type waitErrorEngine struct {
	*councilEngine
	sendErr     bool
	closeResult bool
}

func (e *waitErrorEngine) ContainerWait(context.Context, string, client.ContainerWaitOptions) client.ContainerWaitResult {
	errCh := make(chan error, 1)
	resCh := make(chan container.WaitResponse, 1)
	if e.sendErr {
		errCh <- errors.New("wait broken")
	}
	if e.closeResult {
		close(resCh)
	}
	return client.ContainerWaitResult{Result: resCh, Error: errCh}
}

type nilReaderProxyEngine struct {
	*readinessEngine
	notRunning bool
}

func (e *nilReaderProxyEngine) ContainerStart(context.Context, string, client.ContainerStartOptions) (client.ContainerStartResult, error) {
	if e.notRunning {
		c := e.containers["proxy"]
		c.State = &container.State{Running: false, Status: "exited"}
		e.containers["proxy"] = c
		return client.ContainerStartResult{}, nil
	}
	return e.fakeProxyEngine.ContainerStart(context.Background(), "proxy", client.ContainerStartOptions{})
}

func (e *nilReaderProxyEngine) ExecAttach(context.Context, string, client.ExecAttachOptions) (client.ExecAttachResult, error) {
	return client.ExecAttachResult{HijackedResponse: client.HijackedResponse{Conn: &fixtureConn{}, Reader: nil}}, nil
}

type warnProxyEngine struct {
	*fakeProxyEngine
	emptyID      bool
	warnings     []string
	forceImageID string
}

func (e *warnProxyEngine) ImageInspect(ctx context.Context, ref string, opts ...client.ImageInspectOption) (client.ImageInspectResult, error) {
	r, err := e.fakeProxyEngine.ImageInspect(ctx, ref, opts...)
	if e.forceImageID != "" {
		r.ID = e.forceImageID
	}
	return r, err
}

func (e *warnProxyEngine) ContainerCreate(_ context.Context, opts client.ContainerCreateOptions) (client.ContainerCreateResult, error) {
	if e.emptyID {
		return client.ContainerCreateResult{}, nil
	}
	return client.ContainerCreateResult{ID: "cid-" + opts.Name, Warnings: e.warnings}, nil
}

var _ network.Network
