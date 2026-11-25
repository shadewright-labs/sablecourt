package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/shadewright-labs/sablecourt/internal/gate/councilproxy"
	"github.com/shadewright-labs/sablecourt/internal/harnessimages"
	rt "github.com/shadewright-labs/sablecourt/internal/runtime"
	"github.com/shadewright-labs/sablecourt/internal/runtime/docker"
)

type fakeSession struct {
	closed bool
}

func (s *fakeSession) Workload() councilproxy.WorkloadEndpoint {
	return councilproxy.WorkloadEndpoint{Network: "net", NetworkID: "net-id", ProxyURL: "http://proxy:8888"}
}
func (s *fakeSession) Close(context.Context) error { s.closed = true; return nil }

type fakeLogin struct {
	checks                []error // consumed one per CheckCouncilHarness call
	ensureErr, proxyErr   error
	imageErr, runErr      error
	calls                 []string
	session               *fakeSession
	launch                rt.CouncilLaunch
	terminalIn            string
	terminalOut           string
	proxyConfig           councilproxy.Config
	sawOutput, sawRawEcho string
}

func (f *fakeLogin) CheckCouncilHarness(context.Context, string) error {
	f.calls = append(f.calls, "check")
	if len(f.checks) == 0 {
		return nil
	}
	e := f.checks[0]
	f.checks = f.checks[1:]
	return e
}
func (f *fakeLogin) EnsureCouncilLoginVolume(context.Context, string) error {
	f.calls = append(f.calls, "volume")
	return f.ensureErr
}
func (f *fakeLogin) ResolveLocalImage(context.Context, string) (string, error) {
	f.calls = append(f.calls, "image")
	return "sha256:" + strings.Repeat("a", 64), f.imageErr
}
func (f *fakeLogin) ProvisionCouncilProxy(_ context.Context, _ string, c councilproxy.Config) (councilproxy.LaunchSession, error) {
	f.calls = append(f.calls, "proxy")
	f.proxyConfig = c
	if f.proxyErr != nil {
		return nil, f.proxyErr
	}
	f.session = &fakeSession{}
	return f.session, nil
}
func (f *fakeLogin) RunCouncilLogin(ctx context.Context, l rt.CouncilLaunch, s func(context.Context, io.Reader, io.Writer) error, _ rt.CouncilRecorder) (rt.CouncilResult, error) {
	f.calls = append(f.calls, "run")
	f.launch = l
	if f.runErr != nil {
		return rt.CouncilResult{}, f.runErr
	}
	var sent bytes.Buffer
	err := s(ctx, strings.NewReader("visit https://example.test/login\n"), &sent)
	f.sawRawEcho = sent.String()
	return rt.CouncilResult{}, err
}

func harnessErr(f docker.HarnessFault) error {
	return &docker.HarnessError{Harness: "claude", Fault: f}
}

func TestHarnessLogin(t *testing.T) {
	for _, tc := range []struct {
		name      string
		role      string
		fake      *fakeLogin
		wantErr   string
		wantCalls string
		notErr    string
	}{
		{name: "new volume", role: "claude", fake: &fakeLogin{checks: []error{harnessErr(docker.FaultVolumeMissing), nil}}, wantCalls: "check image volume proxy run check"},
		{name: "login absent then stored", role: "codex", fake: &fakeLogin{checks: []error{harnessErr(docker.FaultLoginMissing), nil}}, wantCalls: "check image volume proxy run check"},
		{name: "already logged in", role: "agy", fake: &fakeLogin{}, wantCalls: "check image volume proxy run check"},
		{name: "image missing", role: "claude", fake: &fakeLogin{checks: []error{harnessErr(docker.FaultImageMissing)}}, wantErr: "sablectl harness build claude", wantCalls: "check"},
		{name: "unlabelled volume", role: "claude", fake: &fakeLogin{checks: []error{harnessErr(docker.FaultVolumeInvalid)}}, wantErr: "not a local Sablecourt volume", wantCalls: "check"},
		{name: "engine failure", role: "claude", fake: &fakeLogin{checks: []error{&docker.HarnessError{Harness: "claude", Fault: docker.FaultBackend, Err: errors.New("sk-ant-secret")}}}, wantErr: "unavailable", notErr: "secret", wantCalls: "check"},
		{name: "proxy image missing", role: "codex", fake: &fakeLogin{checks: []error{harnessErr(docker.FaultLoginMissing)}, imageErr: errNotFound{}}, wantErr: "sablectl harness build codex", wantCalls: "check image"},
		{name: "volume create fails", role: "claude", fake: &fakeLogin{checks: []error{harnessErr(docker.FaultVolumeMissing)}, ensureErr: errors.New("denied")}, wantErr: "prepare login volume", wantCalls: "check image volume"},
		{name: "proxy fails", role: "claude", fake: &fakeLogin{checks: []error{harnessErr(docker.FaultVolumeMissing)}, proxyErr: errors.New("no network")}, wantErr: "start login proxy", wantCalls: "check image volume proxy"},
		{name: "session fails", role: "claude", fake: &fakeLogin{checks: []error{harnessErr(docker.FaultVolumeMissing)}, runErr: errors.New("create failed")}, wantErr: "login session for claude failed", wantCalls: "check image volume proxy run"},
		{name: "no credential after session", role: "claude", fake: &fakeLogin{checks: []error{harnessErr(docker.FaultVolumeMissing), harnessErr(docker.FaultLoginMissing)}}, wantErr: "sablectl harness login claude", wantCalls: "check image volume proxy run check"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			def, _ := harnessimages.Lookup(tc.role)
			old := newLoginRuntime
			var got harnessimages.Definition
			newLoginRuntime = func(d harnessimages.Definition) (loginRuntime, error) { got = d; return tc.fake, nil }
			t.Cleanup(func() { newLoginRuntime = old })
			var out, errOut bytes.Buffer
			err := runHarnessLogin(t.Context(), tc.role, strings.NewReader("code123\n"), &out, &errOut)
			if got.Image != def.Image || got.Volume != def.Volume {
				t.Fatalf("runtime configured for %+v", got)
			}
			if strings.Join(tc.fake.calls, " ") != tc.wantCalls {
				t.Fatalf("calls = %v, want %s", tc.fake.calls, tc.wantCalls)
			}
			if tc.wantErr == "" {
				if err != nil {
					t.Fatal(err)
				}
				if !strings.Contains(out.String(), "https://example.test/login") || !strings.Contains(out.String(), def.Volume) {
					t.Fatalf("output = %q", out.String())
				}
				l := tc.fake.launch
				if l.Harness != tc.role || l.ProxyURL != "http://proxy:8888" || l.Network != "net" || l.NetworkID != "net-id" ||
					l.Prompt != "" || l.TaskToken != "" || l.Checkout != (rt.CouncilCheckout{}) || l.DataGateURL != "" {
					t.Fatalf("launch = %+v", l)
				}
				if tc.fake.proxyConfig.LeaseSHA256 != "" || tc.fake.proxyConfig.Harness != tc.role {
					t.Fatalf("proxy must be login-only: %+v", tc.fake.proxyConfig)
				}
			} else {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error = %v, want %q", err, tc.wantErr)
				}
				if tc.notErr != "" && strings.Contains(err.Error(), tc.notErr) {
					t.Fatalf("error leaks %q: %v", tc.notErr, err)
				}
			}
			if tc.fake.session != nil && !tc.fake.session.closed {
				t.Fatal("login proxy was not removed")
			}
		})
	}
}

type errNotFound struct{}

func (errNotFound) Error() string { return "no such image" }
func (errNotFound) NotFound()     {}

func TestHarnessLoginCommandRegistered(t *testing.T) {
	root := newRootCmd()
	cmd, _, err := root.Find([]string{"harness", "login"})
	if err != nil || cmd.Name() != "login" {
		t.Fatalf("harness login is not registered: %v", err)
	}
	for _, tc := range []struct {
		args []string
		bad  bool
	}{{[]string{"claude"}, false}, {[]string{"codex"}, false}, {[]string{"agy"}, false}, {[]string{"proxy"}, true}, {nil, true}, {[]string{"claude", "codex"}, true}} {
		if err := cmd.ValidateArgs(tc.args); (err != nil) != tc.bad {
			t.Errorf("args %v: error %v", tc.args, err)
		}
	}
}

type signalWriter struct {
	buf  bytes.Buffer
	seen chan struct{}
}

func (w *signalWriter) Write(p []byte) (int, error) {
	n, err := w.buf.Write(p)
	close(w.seen)
	return n, err
}

func TestTerminalSessionForwardsBothWays(t *testing.T) {
	pr, pw := io.Pipe()
	w := &signalWriter{seen: make(chan struct{})}
	var out bytes.Buffer
	done := make(chan error, 1)
	go func() {
		done <- terminalSession(strings.NewReader("code123\n"), &out)(t.Context(), pr, w)
	}()
	if _, err := io.WriteString(pw, "open the link\n"); err != nil {
		t.Fatal(err)
	}
	<-w.seen // the typed code reached the container before it exits
	pw.Close()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if w.buf.String() != "code123\n" || out.String() != "open the link\n" {
		t.Fatalf("container got %q, terminal got %q", w.buf.String(), out.String())
	}
}
