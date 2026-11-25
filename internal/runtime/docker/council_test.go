package docker

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/api/types/volume"
	"github.com/moby/moby/client"
	rt "github.com/shadewright-labs/sablecourt/internal/runtime"
)

type fixtureConn struct {
	net.Conn
	body        bytes.Buffer
	eof, closed bool
	fail        string
}

func (c *fixtureConn) Write(p []byte) (int, error) {
	if c.fail == "write" {
		return 0, io.ErrClosedPipe
	}
	return c.body.Write(p)
}
func (c *fixtureConn) CloseWrite() error {
	c.eof = true
	if c.fail == "eof" {
		return io.ErrClosedPipe
	}
	return nil
}
func (c *fixtureConn) Close() error { c.closed = true; return nil }

type councilEngine struct {
	inspectEngine
	step    string
	calls   []string
	channel fixtureConn
	output  []byte
	vol     volume.Volume
	exists  bool
}

func newCouncilEngine() *councilEngine {
	f := &councilEngine{vol: volume.Volume{Name: "sablecourt-login-claude", Driver: "local", Labels: CouncilLoginVolumeLabels("claude")}, output: append(frame(1, "answer"), frame(2, "diagnostic")...)}
	f.net = network.Network{Name: DefaultNetworkName, ID: "net-id", Internal: true, Driver: "bridge"}
	f.imageID = "sha256:" + strings.Repeat("b", 64)
	return f
}
func (f *councilEngine) VolumeInspect(context.Context, string, client.VolumeInspectOptions) (client.VolumeInspectResult, error) {
	if f.step == "missing volume" {
		return client.VolumeInspectResult{}, missing{errors.New("missing")}
	}
	return client.VolumeInspectResult{Volume: f.vol}, nil
}
func (f *councilEngine) ContainerCreate(_ context.Context, o client.ContainerCreateOptions) (client.ContainerCreateResult, error) {
	f.calls = append(f.calls, "create")
	if f.exists {
		return client.ContainerCreateResult{}, errors.New("name in use")
	}
	if err := validateFakeTmpfs(o.HostConfig); err != nil {
		return client.ContainerCreateResult{}, err
	}
	f.createdOpts = o
	f.exists = true
	if f.step == "lost create" || f.step == "lost create and inspect" {
		return client.ContainerCreateResult{}, errors.New("connection lost")
	}
	return client.ContainerCreateResult{ID: "container-id"}, nil
}

// Docker 29.9.3 uses different Linux option parsers for Mounts and Tmpfs.
func validateFakeTmpfs(h *container.HostConfig) error {
	for _, m := range h.Mounts {
		if m.Type != mount.TypeTmpfs || m.TmpfsOptions == nil {
			continue
		}
		for _, opt := range m.TmpfsOptions.Options {
			if len(opt) < 1 || len(opt) > 2 || (opt[0] != "exec" && opt[0] != "noexec") {
				return fmt.Errorf("invalid structured tmpfs option %q", opt)
			}
		}
	}
	for _, options := range h.Tmpfs {
		if options == "" {
			continue
		}
		for opt := range strings.SplitSeq(options, ",") {
			switch opt {
			case "defaults", "ro", "rw", "suid", "nosuid", "dev", "nodev", "exec", "noexec",
				"sync", "async", "dirsync", "remount", "mand", "nomand", "atime", "noatime",
				"diratime", "nodiratime", "bind", "rbind", "unbindable", "runbindable", "private",
				"rprivate", "shared", "rshared", "slave", "rslave", "relatime", "norelatime", "strictatime", "nostrictatime":
				continue
			}
			key, _, hasValue := strings.Cut(opt, "=")
			switch key {
			case "size", "mode", "uid", "gid", "nr_inodes", "nr_blocks", "mpol":
				if hasValue {
					continue
				}
			}
			return fmt.Errorf("invalid legacy tmpfs option %q", opt)
		}
	}
	return nil
}

func TestCouncilEngineTmpfsOptions(t *testing.T) {
	for _, tc := range []struct {
		name       string
		structured [][]string
		legacy     string
		wantError  bool
	}{
		{"structured size and mode", nil, "", false},
		{"structured noexec", [][]string{{"noexec"}}, "", false},
		{"structured uid", [][]string{{"uid", "10001"}}, "", true},
		{"structured gid", [][]string{{"gid", "10001"}}, "", true},
		{"structured unknown flag", [][]string{{"unknown"}}, "", true},
		{"empty option", [][]string{{}}, "", true},
		{"extra values", [][]string{{"exec", "1", "2"}}, "", true},
		{"legacy uid and gid", nil, "uid=10001,gid=10001", false},
		{"legacy unknown flag", nil, "noexec,unknown", true},
		{"legacy unknown key", nil, "unknown=1", true},
		{"legacy missing value", nil, "mode", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := &container.HostConfig{Mounts: []mount.Mount{{Type: mount.TypeTmpfs, Target: "/structured",
				TmpfsOptions: &mount.TmpfsOptions{SizeBytes: 65536, Mode: 0700, Options: tc.structured}}}}
			if tc.legacy != "" {
				h.Tmpfs = map[string]string{"/scratch": tc.legacy}
			}
			f := newCouncilEngine()
			_, err := f.ContainerCreate(t.Context(), client.ContainerCreateOptions{HostConfig: h})
			if (err != nil) != tc.wantError || f.exists == tc.wantError {
				t.Fatalf("create error=%v, container present=%t", err, f.exists)
			}
		})
	}
}
func (f *councilEngine) ContainerAttach(_ context.Context, _ string, o client.ContainerAttachOptions) (client.ContainerAttachResult, error) {
	f.calls = append(f.calls, "attach")
	if !o.Stream || !o.Stdin || !o.Stdout || !o.Stderr || f.step == "attach" {
		return client.ContainerAttachResult{}, errors.New("attach failed")
	}
	return client.ContainerAttachResult{HijackedResponse: client.HijackedResponse{Conn: &f.channel, Reader: bufio.NewReader(bytes.NewReader(f.output))}}, nil
}
func (f *councilEngine) ContainerStart(context.Context, string, client.ContainerStartOptions) (client.ContainerStartResult, error) {
	f.calls = append(f.calls, "start")
	if f.step == "start" {
		return client.ContainerStartResult{}, context.DeadlineExceeded
	}
	return client.ContainerStartResult{}, nil
}
func (f *councilEngine) ContainerWait(context.Context, string, client.ContainerWaitOptions) client.ContainerWaitResult {
	result := make(chan container.WaitResponse, 1)
	result <- container.WaitResponse{StatusCode: 7}
	return client.ContainerWaitResult{Result: result}
}
func (f *councilEngine) ContainerInspect(_ context.Context, id string, _ client.ContainerInspectOptions) (client.ContainerInspectResult, error) {
	f.calls = append(f.calls, "inspect")
	if !f.exists || f.step == "inspect missing" {
		return client.ContainerInspectResult{}, missing{errors.New("missing")}
	}
	if f.step == "inspect transient" || f.step == "lost create and inspect" {
		return client.ContainerInspectResult{}, errors.New("unavailable")
	}
	state := &container.State{Status: "exited", ExitCode: 7, FinishedAt: "2026-01-01T01:01:01Z"}
	if f.step == "wrong exit" {
		state.ExitCode++
	}
	if f.step == "no timestamp" {
		state.FinishedAt = ""
	}
	return client.ContainerInspectResult{Container: container.InspectResponse{ID: "container-id", Config: f.createdOpts.Config, State: state}}, nil
}
func (f *councilEngine) ContainerRemove(ctx context.Context, id string, o client.ContainerRemoveOptions) (client.ContainerRemoveResult, error) {
	f.calls = append(f.calls, "remove")
	if ctx.Err() != nil || id != "container-id" || !o.Force || f.step == "remove" {
		return client.ContainerRemoveResult{}, errors.New("remove failed")
	}
	f.exists = false
	return client.ContainerRemoveResult{}, nil
}
func fixtureRun(t *testing.T) (*Adapter, *councilEngine, rt.CouncilLaunch) {
	t.Helper()
	f := newCouncilEngine()
	a := NewAdapter(f, "")
	a.CouncilHarnesses = map[string]CouncilHarness{"claude": {Image: "fixture", LoginVolume: f.vol.Name, ProxyURL: "http://relay:8888", DataGateURL: "http://relay:8080/gate/data"}}
	s := rt.CouncilLaunch{TaskID: "task", Workload: "writer", Harness: "claude", Model: "model", Checkout: rt.CouncilCheckout{Path: t.TempDir(), Kind: rt.CouncilReview},
		Limits: rt.CouncilLimits{NanoCPUs: 500000000, MemoryBytes: 64 << 20, PIDs: 32, WallTime: time.Second}, Prompt: "fixture question", TaskToken: "fixture-lease"}
	return a, f, s
}
func TestCouncilCreateAndEvidence(t *testing.T) {
	for _, kind := range []rt.CouncilCheckoutKind{rt.CouncilLive, rt.CouncilRef, rt.CouncilReview, rt.CouncilWriter} {
		t.Run(string(kind), func(t *testing.T) {
			a, f, s := fixtureRun(t)
			s.Checkout.Kind = kind
			s.Prompt += " password=hidden-fixture"
			f.output = append(frame(1, "answer"), frame(2, s.TaskToken)...)
			result, err := a.RunCouncil(t.Context(), s, func(_ context.Context, r rt.CouncilResult) error {
				f.calls = append(f.calls, "record")
				if !f.exists || r.Stdout != "answer" || strings.Contains(r.Stderr, s.TaskToken) || r.ExitCode != 7 || r.FinishedAt.IsZero() {
					t.Fatalf("bad evidence: %+v", r)
				}
				return nil
			})
			if err != nil || result.ContainerID != "container-id" || !slices.Equal(f.calls, []string{"create", "attach", "start", "inspect", "record", "remove"}) {
				t.Fatalf("calls=%v error=%v", f.calls, err)
			}
			opts := f.createdOpts
			c, h := opts.Config, opts.HostConfig
			if c.Image != f.imageID || c.User != "10001:10001" || c.WorkingDir != "/workspace" || c.Tty || !c.StdinOnce || !f.channel.eof || !f.channel.closed ||
				!h.ReadonlyRootfs || h.Privileged || !slices.Equal(h.CapDrop, []string{"ALL"}) || !slices.Contains(h.SecurityOpt, "no-new-privileges:true") ||
				h.NanoCPUs != s.Limits.NanoCPUs || h.Memory != s.Limits.MemoryBytes || h.MemorySwap != h.Memory || h.PidsLimit == nil || *h.PidsLimit != s.Limits.PIDs ||
				string(h.NetworkMode) != f.net.ID || len(h.ExtraHosts) > 0 || h.LogConfig.Type != "none" {
				t.Fatal("wrong isolation or limits")
			}
			assertCouncilMounts(t, h, s, f.vol.Name)
			meta, _ := json.Marshal(opts)
			for _, bad := range []string{"fixture question", s.TaskToken, "hidden-fixture", "SABLECOURT_MODEL_GATE_URL"} {
				if bytes.Contains(meta, []byte(bad)) {
					t.Fatal("sensitive create metadata")
				}
			}
			if body := f.channel.body.String(); !strings.Contains(body, "fixture question") || !strings.Contains(body, s.TaskToken) || strings.Contains(body, "hidden-fixture") {
				t.Fatal("wrong bootstrap")
			}
		})
	}
}

func assertCouncilMounts(t *testing.T, h *container.HostConfig, s rt.CouncilLaunch, volumeName string) {
	t.Helper()
	var want []mount.Mount
	if s.Checkout.Path != "" {
		source, err := filepath.EvalSymlinks(s.Checkout.Path)
		if err != nil {
			t.Fatal(err)
		}
		want = append(want, mount.Mount{Type: mount.TypeBind, Source: source, Target: "/workspace",
			ReadOnly: s.Checkout.Kind != rt.CouncilWriter, BindOptions: &mount.BindOptions{NonRecursive: true}})
	}
	want = append(want, mount.Mount{Type: mount.TypeVolume, Source: volumeName, Target: "/home/harness"})
	if !reflect.DeepEqual(h.Mounts, want) || len(h.Binds) != 0 {
		t.Fatalf("Mounts=%+v, Binds=%v, want only %+v", h.Mounts, h.Binds, want)
	}
	options := fmt.Sprintf("rw,noexec,nosuid,nodev,mode=0700,uid=10001,gid=10001,size=%d", s.Limits.MemoryBytes)
	if !maps.Equal(h.Tmpfs, map[string]string{"/scratch": options}) {
		t.Fatalf("Tmpfs=%v, want /scratch with %q", h.Tmpfs, options)
	}
}
func discardResult(context.Context, rt.CouncilResult) error { return nil }
func TestCouncilFailureCleanupAndRetry(t *testing.T) {
	for _, step := range []string{"lost create", "attach", "start", "write", "eof", "stream", "inspect missing", "inspect transient", "wrong exit", "no timestamp", "recorder", "remove"} {
		t.Run(step, func(t *testing.T) {
			a, f, s := fixtureRun(t)
			f.step = step
			f.channel.fail = step
			if step == "stream" {
				f.output = frame(3, "engine error")
			}
			recorded := false
			r, err := a.RunCouncil(t.Context(), s, func(context.Context, rt.CouncilResult) error {
				recorded = true
				if step == "recorder" {
					return errors.New("unavailable")
				}
				return nil
			})
			if err == nil || recorded != (step == "recorder" || step == "remove") {
				t.Fatalf("recorded=%v error=%v", recorded, err)
			}
			if step == "inspect missing" && !isMissing(err) || step == "inspect transient" && isMissing(err) {
				t.Fatal("wrong inspect error classification")
			}
			f.step = ""
			f.channel.fail = ""
			f.output = frame(1, "ok")
			if step == "remove" {
				if r.ContainerID == "" {
					t.Fatal("lost cleanup handle")
				}
				if err := a.Stop(t.Context(), rt.Handle{ExternalID: r.ContainerID}); err != nil {
					t.Fatal(err)
				}
			}
			if f.exists {
				t.Fatal("container leaked")
			}
			if _, err := a.RunCouncil(t.Context(), s, discardResult); err != nil {
				t.Fatal("retry failed", err)
			}
		})
	}
}
func TestCouncilRefusals(t *testing.T) {
	for _, step := range []string{"network missing", "network transient", "external network", "missing volume", "bind volume", "wrong volume", "image missing", "cpu", "memory", "pids", "time", "checkout", "relative", "socket", "harness", "model", "empty lease", "empty prompt", "option prompt", "unfinished key", "no recorder", "proxy credentials", "direct gate"} {
		t.Run(step, func(t *testing.T) {
			a, f, s := fixtureRun(t)
			cfg := a.CouncilHarnesses[s.Harness]
			rec := rt.CouncilRecorder(discardResult)
			switch step {
			case "network missing":
				f.netErr = missing{errors.New("missing")}
			case "network transient":
				f.netErr = errors.New("unavailable")
			case "external network":
				f.net.Internal = false
			case "missing volume":
				f.step = step
			case "bind volume":
				f.vol.Options = map[string]string{"type": "bind"}
			case "wrong volume":
				f.vol.Name = "other"
			case "image missing":
				f.imageErr = missing{errors.New("missing")}
			case "cpu":
				s.Limits.NanoCPUs = 0
			case "memory":
				s.Limits.MemoryBytes = 0
			case "pids":
				s.Limits.PIDs = -1
			case "time":
				s.Limits.WallTime = 0
			case "checkout":
				s.Checkout.Kind = "unknown"
			case "relative":
				s.Checkout.Path = "."
			case "socket":
				s.Checkout.Path = s.Checkout.Path + "/docker.sock"
			case "harness":
				s.Harness = "sh"
			case "model":
				s.Model = "--option"
			case "empty lease":
				s.TaskToken = ""
			case "empty prompt":
				s.Prompt = ""
			case "option prompt":
				s.Prompt = "--help"
			case "unfinished key":
				s.Prompt = "-----BEGIN PRIVATE KEY-----\ntext"
			case "no recorder":
				rec = nil
			case "proxy credentials":
				cfg.ProxyURL = "http://user:pass@relay"
			case "direct gate":
				cfg.DataGateURL = "http://elsewhere/gate"
			}
			a.CouncilHarnesses["claude"] = cfg
			_, err := a.RunCouncil(t.Context(), s, rec)
			if err == nil || f.createdOpts.Config != nil || f.createdNetwork.Driver != "" || f.pulled {
				t.Fatal("unsafe request proceeded")
			}
			if (step == "network missing" || step == "missing volume" || step == "image missing") && !isMissing(err) {
				t.Fatal("missing classification was lost")
			}
		})
	}
}

func TestCouncilLoginTerminal(t *testing.T) {
	for _, harness := range []string{"claude", "codex", "agy"} {
		t.Run(harness, func(t *testing.T) {
			a, f, s := fixtureRun(t)
			cfg := a.CouncilHarnesses["claude"]
			delete(a.CouncilHarnesses, "claude")
			f.vol.Name = "sablecourt-login-" + harness
			cfg.LoginVolume = f.vol.Name
			f.vol.Labels = CouncilLoginVolumeLabels(harness)
			a.CouncilHarnesses[harness] = cfg
			s.Harness = harness
			s.Checkout = rt.CouncilCheckout{}
			s.Limits.MemoryBytes = 32 << 20
			s.Model = ""
			s.Prompt = ""
			s.TaskToken = ""
			f.output = []byte("login challenge")
			result, err := a.RunCouncilLogin(t.Context(), s, func(_ context.Context, r io.Reader, w io.Writer) error {
				data, err := io.ReadAll(r)
				if err != nil || string(data) != "login challenge" {
					t.Fatal("TTY output")
				}
				_, err = io.WriteString(w, "login reply")
				return err
			}, discardResult)
			if err != nil || !f.createdOpts.Config.Tty || !f.createdOpts.Config.OpenStdin || !f.channel.closed || f.channel.body.String() != "login reply" ||
				result.Stdout != "" || result.Stderr != "" || f.exists || f.createdOpts.HostConfig.PidsLimit == nil || !slices.Contains(f.createdOpts.Config.Entrypoint, "/usr/bin/timeout") {
				t.Fatal("login failed", err)
			}
			assertCouncilMounts(t, f.createdOpts.HostConfig, s, f.vol.Name)
			if harness == "codex" && !slices.Contains(f.createdOpts.Config.Cmd, "--device-auth") {
				t.Fatal("missing device flow")
			}
		})
	}
}

// A failed create followed by a failed inspect has no container ID to return.
func TestCouncilAmbiguousCleanup(t *testing.T) {
	for _, outcome := range []string{"original", "gone", "replacement", "unavailable"} {
		t.Run(outcome, func(t *testing.T) {
			a, f, s := fixtureRun(t)
			f.step = "lost create and inspect"
			r, err := a.RunCouncil(t.Context(), s, discardResult)
			if err == nil || !f.exists {
				t.Fatal("fixture did not lose both acknowledgments")
			}
			cleaner, ok := any(a).(interface {
				CleanupCouncil(context.Context, rt.CouncilResult) error
			})
			if !ok {
				t.Fatal("cannot retry ambiguous cleanup")
			}
			f.step = ""
			switch outcome {
			case "gone":
				f.exists = false
			case "replacement":
				f.createdOpts.Config.Labels[attemptLabel] = "another-attempt"
			case "unavailable":
				f.step = "inspect transient"
			}
			err = cleaner.CleanupCouncil(t.Context(), r)
			if (err != nil) != (outcome == "unavailable") || f.exists != (outcome == "replacement" || outcome == "unavailable") {
				t.Fatalf("container present=%t error=%v", f.exists, err)
			}
		})
	}
}

type configuredImageEngine struct {
	*councilEngine
	config string
}

func (f configuredImageEngine) ImageInspect(ctx context.Context, ref string, opts ...client.ImageInspectOption) (client.ImageInspectResult, error) {
	r, err := f.councilEngine.ImageInspect(ctx, ref, opts...)
	if err != nil {
		return r, err
	}
	data := []byte(`{"Config":` + f.config + `}`)
	err = json.Unmarshal(data, &r)
	return r, err
}

func TestCouncilInheritedConfig(t *testing.T) {
	for _, config := range []string{`null`, `{"Volumes":{"/extra":{}}}`, `{"Env":["SABLECOURT_MODEL_GATE_URL=unexpected"]}`, `{"Env":["BASH_ENV=/workspace/startup"]}`, `{"Env":["API_KEY=private-fixture"]}`} {
		t.Run(config, func(t *testing.T) {
			a, f, s := fixtureRun(t)
			a.Client = configuredImageEngine{f, config}
			_, err := a.RunCouncil(t.Context(), s, discardResult)
			if err == nil || f.exists || len(f.calls) != 0 {
				t.Fatal("unverified image config reached create")
			}
		})
	}
}

// The peer never reads or writes. Cancellation must unblock either direction.
type stalledConn struct {
	net.Conn
	closes atomic.Int32
}

func (c *stalledConn) CloseWrite() error { return nil }
func (c *stalledConn) Close() error      { c.closes.Add(1); return c.Conn.Close() }

type stalledEngine struct {
	*councilEngine
	conn *stalledConn
}

func (f *stalledEngine) ContainerAttach(context.Context, string, client.ContainerAttachOptions) (client.ContainerAttachResult, error) {
	return client.ContainerAttachResult{HijackedResponse: client.HijackedResponse{Conn: f.conn, Reader: bufio.NewReader(f.conn)}}, nil
}

func TestCouncilDeadlineClosesAttachment(t *testing.T) {
	for _, mode := range []string{"bootstrap", "output", "login"} {
		t.Run(mode, func(t *testing.T) {
			a, f, s := fixtureRun(t)
			local, peer := net.Pipe()
			defer peer.Close()
			conn := &stalledConn{Conn: local}
			a.Client = &stalledEngine{f, conn}
			s.Limits.WallTime = 200 * time.Millisecond
			if mode == "output" {
				go func() { _, _ = io.Copy(io.Discard, peer) }()
			}
			var err error
			if mode == "login" {
				s.Checkout = rt.CouncilCheckout{}
				s.Model = ""
				s.TaskToken = ""
				s.Prompt = ""
				_, err = a.RunCouncilLogin(t.Context(), s, func(_ context.Context, r io.Reader, _ io.Writer) error { _, err := io.Copy(io.Discard, r); return err }, discardResult)
			} else {
				_, err = a.RunCouncil(t.Context(), s, discardResult)
			}
			if !errors.Is(err, context.DeadlineExceeded) || f.exists || conn.closes.Load() != 1 {
				t.Fatalf("present=%t close count=%d err=%v", f.exists, conn.closes.Load(), err)
			}
		})
	}
}

func TestCouncilLoginVolumeIdentity(t *testing.T) {
	for _, labels := range []map[string]string{nil, {"app.kubernetes.io/managed-by": "another-service", "sablecourt.io/harness": "claude"}, {"app.kubernetes.io/managed-by": "sablecourt", "sablecourt.io/harness": "agy"}} {
		a, f, s := fixtureRun(t)
		f.vol.Labels = labels
		_, err := a.RunCouncil(t.Context(), s, discardResult)
		if err == nil || len(f.calls) != 0 {
			t.Errorf("mounted volume with labels %v", labels)
		}
	}
}
