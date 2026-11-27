package coding

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/shadewright-labs/sablecourt/internal/attach"
	scruntime "github.com/shadewright-labs/sablecourt/internal/runtime"
)

func TestAdapterLaunchAndLifecycleRefusals(t *testing.T) {
	ctx := context.Background()
	t.Run("SupportsCouncilContainers", func(t *testing.T) {
		var nilAdapter *Adapter
		if nilAdapter.SupportsCouncilContainers() {
			t.Error("nil adapter reported container support")
		}
		a := NewAdapter(t.TempDir())
		if a.SupportsCouncilContainers() {
			t.Error("adapter without runner reported container support")
		}
		a.CouncilRunner = newFakeCouncilRunner()
		if !a.SupportsCouncilContainers() {
			t.Error("adapter with runner did not report container support")
		}
	})

	t.Run("Suspend and Resume refuse", func(t *testing.T) {
		a := NewAdapter(t.TempDir())
		if _, err := a.Suspend(ctx, scruntime.Handle{}); err == nil || !strings.Contains(err.Error(), "suspend") {
			t.Fatalf("Suspend = %v, want suspend refusal", err)
		}
		if err := a.Resume(ctx, scruntime.Handle{}, scruntime.CheckpointRef{}); err == nil || !strings.Contains(err.Error(), "resume") {
			t.Fatalf("Resume = %v, want resume refusal", err)
		}
	})

	t.Run("Start refusals", func(t *testing.T) {
		cases := []struct {
			name   string
			mutate func(*Adapter, *scruntime.LaunchSpec)
			commit string
			want   string
		}{
			{
				name:   "missing coding config",
				mutate: func(_ *Adapter, s *scruntime.LaunchSpec) { s.Workload.Spec.Coding = nil },
				want:   "coding config is required",
			},
			{
				name:   "unsupported harness",
				mutate: func(_ *Adapter, s *scruntime.LaunchSpec) { s.Workload.Spec.Coding.Harness = "other" },
				want:   "unsupported coding harness",
			},
			{
				name:   "negative wall time",
				mutate: func(a *Adapter, _ *scruntime.LaunchSpec) { a.WallTime = -time.Second },
				want:   "wall time must be positive",
			},
			{
				name:   "StartAtCommit rejects short hash",
				commit: "abc123",
				want:   "full commit ID",
			},
			{
				name: "StartAtCommit rejects checkoutMode",
				mutate: func(_ *Adapter, s *scruntime.LaunchSpec) {
					s.InputJSON = `{"checkoutMode":"live","prompt":"inspect"}`
				},
				commit: strings.Repeat("a", 40),
				want:   "cannot be combined with checkoutMode",
			},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				a, spec := codingFixture(t)
				a.DryRun = true
				if tc.mutate != nil {
					tc.mutate(a, &spec)
				}
				var err error
				if tc.commit != "" {
					_, err = a.StartAtCommit(ctx, spec, tc.commit, false)
				} else {
					_, err = a.Start(ctx, spec)
				}
				if err == nil || !strings.Contains(err.Error(), tc.want) {
					t.Fatalf("error = %v, want %q", err, tc.want)
				}
			})
		}
	})

	t.Run("sanitizeSegment", func(t *testing.T) {
		for _, tc := range []struct{ in, want string }{
			{"", "task"},
			{"abc-1_Z", "abc-1_Z"},
			{"../a b/c", "---a-b-c"},
		} {
			if got := sanitizeSegment(tc.in); got != tc.want {
				t.Errorf("sanitizeSegment(%q) = %q, want %q", tc.in, got, tc.want)
			}
		}
	})

	t.Run("orphan container stop cleans resources", func(t *testing.T) {
		proxy := newFakeProxyDocker()
		proxy.networks["net-1"] = true
		runner := newFakeCouncilRunner()
		runner.containers["cid-orphan"] = &fakeContainerState{id: "cid-orphan", exists: true, running: true}
		a := NewAdapter(t.TempDir())
		a.CouncilRunner = runner
		a.CouncilProxy = proxy
		h := scruntime.Handle{
			ExternalID:    "task-orphan",
			ContainerID:   "cid-orphan",
			ContainerName: "sablecourt-council-task-orphan",
			CreateAttempt: "att-1",
			ProxyName:     "px-1",
			NetworkName:   "net-1",
		}
		if err := a.Stop(ctx, h); err != nil {
			t.Fatalf("Stop orphan container: %v", err)
		}
		if len(proxy.removedProxies) != 2 || proxy.removedProxies[0] != "px-1-probe" || proxy.removedProxies[1] != "px-1" {
			t.Errorf("removed proxies = %v, want [px-1-probe px-1]", proxy.removedProxies)
		}
		if len(proxy.removedNetworks) != 1 || proxy.removedNetworks[0] != "net-1" {
			t.Errorf("removed networks = %v, want [net-1]", proxy.removedNetworks)
		}
	})
}

func TestParseLaunchInputRefusalsAndResolveHarness(t *testing.T) {
	keyHeader := "key material:\n-----BEGIN " + "RSA PRIVATE KEY-----\nabc"
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{"trailing JSON data", `{"prompt":"ok"} {}`, "unexpected data"},
		{"invalid live conclaveId with control char", `{"checkoutMode":"live","conclaveId":"ccl\u0007","prompt":"ok"}`, "invalid coding conclaveId"},
		{"invalid live role", `{"checkoutMode":"live","role":"@admin","prompt":"ok"}`, "invalid coding role"},
		{"invalid ref role", `{"checkoutMode":"ref","checkoutRef":"HEAD","conclaveId":"c1","role":"operator","prompt":"ok"}`, "invalid coding role"},
		{"unfinished private key in prompt", mustMarshalInput(t, map[string]any{"prompt": keyHeader}), "unfinished private key"},
		{"unfinished private key in context", mustMarshalInput(t, map[string]any{"context_packet": keyHeader}), "unfinished private key"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := parseLaunchInput(tc.raw); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("parseLaunchInput(%s) = %v, want %q", tc.name, err, tc.want)
			}
		})
	}

	t.Run("resolveHarness rejects relative configured path", func(t *testing.T) {
		if _, err := resolveHarness("claude", "bin/claude"); err == nil || !strings.Contains(err.Error(), "must be absolute") {
			t.Fatalf("resolveHarness relative = %v", err)
		}
		_, err := resolveHarness("claude", filepath.Join(t.TempDir(), "missing-bin"))
		var missing *scruntime.MissingBinaryError
		if !errors.As(err, &missing) {
			t.Fatalf("resolveHarness missing = %v, want MissingBinaryError", err)
		}
	})
}

func TestSessionOutputRedactionAndSuppression(t *testing.T) {
	t.Run("private key block and credential continuation", func(t *testing.T) {
		var fileBuf strings.Builder
		tail := &diagnosticTail{}
		hub := attach.NewHub()
		hub.Register("t-out", nil)
		t.Cleanup(func() { hub.Close("t-out") })
		history, subCh, closed, unsub, err := hub.Subscribe("t-out")
		if err != nil || history != "" || closed {
			t.Fatalf("Subscribe = %q, closed=%t, err=%v", history, closed, err)
		}
		t.Cleanup(unsub)

		out := &sessionOutput{file: &fileBuf, tail: tail, hub: hub, taskID: "t-out"}
		tok := "sk-" + strings.Repeat("a", 28)
		beginKey := "-----BEGIN " + "PRIVATE KEY-----"
		endKey := "-----END " + "PRIVATE KEY-----"
		chunks := []string{
			"before " + beginKey + "\n",
			"secret-body-line\n",
			endKey + " after\n",
			"Authorization:\n",
			"Bearer\n",
			tok + "\n",
			"safe tail\n",
		}
		for _, c := range chunks {
			if _, err := out.Write([]byte(c)); err != nil {
				t.Fatalf("Write(%q): %v", c, err)
			}
		}
		if err := out.Flush(); err != nil {
			t.Fatalf("Flush: %v", err)
		}
		hub.Close("t-out")
		var hubBuf strings.Builder
		for chunk := range subCh {
			hubBuf.WriteString(chunk)
		}
		for _, target := range []struct {
			name string
			got  string
		}{
			{"file", fileBuf.String()},
			{"diagnostic tail", tail.text},
			{"attach hub", hubBuf.String()},
		} {
			for _, forbidden := range []string{"secret-body-line", tok} {
				if strings.Contains(target.got, forbidden) {
					t.Errorf("%s leaked %q: %q", target.name, forbidden, target.got)
				}
			}
			if !strings.Contains(target.got, "[REDACTED:private_key]") || !strings.Contains(target.got, "safe tail\n") {
				t.Errorf("unexpected sanitized %s output: %q", target.name, target.got)
			}
		}
	})

	t.Run("oversized line suppresses subsequent writes", func(t *testing.T) {
		var fileBuf strings.Builder
		out := &sessionOutput{file: &fileBuf}
		huge := strings.Repeat("a", maxSessionLine+10)
		if _, err := out.Write([]byte(huge)); err != nil {
			t.Fatal(err)
		}
		if _, err := out.Write([]byte("after limit\n")); err != nil {
			t.Fatal(err)
		}
		if got := fileBuf.String(); got != "[REDACTED:output limit]\n" {
			t.Fatalf("output = %q, want single suppression marker", got)
		}
	})

	t.Run("harnessResponse and diagnosticTail bounds", func(t *testing.T) {
		secretVal := "sk-" + strings.Repeat("b", 28)
		var r harnessResponse
		_, _ = r.Write([]byte("hello " + secretVal))
		if got := r.text(); strings.Contains(got, secretVal) || !strings.Contains(got, "hello ") {
			t.Errorf("harnessResponse did not redact: %q", got)
		}
		var unfinished harnessResponse
		_, _ = unfinished.Write([]byte("-----BEGIN " + "PRIVATE KEY-----\nbody"))
		if got := unfinished.text(); got != "" {
			t.Errorf("unfinished private key returned %q, want empty", got)
		}
		var oversized harnessResponse
		_, _ = oversized.Write([]byte(strings.Repeat("x", 64*1024+1)))
		if got := oversized.text(); got != "" {
			t.Errorf("oversized response returned %d bytes, want empty", len(got))
		}

		var d diagnosticTail
		d.add(strings.Repeat("a", maxDiagnosticTail+50) + "TAIL")
		if len(d.text) != maxDiagnosticTail || !strings.HasSuffix(d.text, "TAIL") {
			t.Errorf("single long line tail len=%d suffix ok=%t", len(d.text), strings.HasSuffix(d.text, "TAIL"))
		}
		d.add("END")
		if len(d.text) != maxDiagnosticTail || !strings.HasSuffix(d.text, "TAILEND") {
			t.Errorf("appended tail len=%d suffix ok=%t", len(d.text), strings.HasSuffix(d.text, "TAILEND"))
		}
	})
}

func TestShebangAndRecordedExitValidation(t *testing.T) {
	t.Run("shebangInterpreter and checkWrappedTarget", func(t *testing.T) {
		dir := t.TempDir()
		cases := []struct {
			name    string
			content string
			want    string
			ok      bool
		}{
			{"standard sh", "#!/bin/sh -e\necho hi\n", "/bin/sh", true},
			{"leading spaces", "#!  \t/usr/bin/env bash\n", "/usr/bin/env", true},
			{"no shebang", "echo hi\n", "", false},
			{"blank shebang", "#!   \n", "", false},
			{"over 512 bytes no newline", "#!/" + strings.Repeat("a", 600), "", false},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				p := filepath.Join(dir, sanitizeSegment(tc.name))
				if err := os.WriteFile(p, []byte(tc.content), 0o700); err != nil {
					t.Fatal(err)
				}
				got, ok := shebangInterpreter(p)
				if ok != tc.ok || got != tc.want {
					t.Fatalf("shebangInterpreter = %q, %t; want %q, %t", got, ok, tc.want, tc.ok)
				}
			})
		}
		if runtime.GOOS == "darwin" {
			p := filepath.Join(dir, "darwin-hash")
			if err := os.WriteFile(p, []byte("#!/bin/sh#comment\n"), 0o700); err != nil {
				t.Fatal(err)
			}
			if got, ok := shebangInterpreter(p); !ok || got != "/bin/sh" {
				t.Fatalf("darwin hash shebang = %q, %t", got, ok)
			}
		}
		if err := checkWrappedTarget(filepath.Join(dir, "absent"), dir); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("missing target = %v, want ErrNotExist", err)
		}
		badScript := filepath.Join(dir, "bad-interp")
		if err := os.WriteFile(badScript, []byte("#!/nonexistent/interp\n"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := checkWrappedTarget(badScript, dir); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("missing interpreter = %v, want ErrNotExist", err)
		}
		goodScript := filepath.Join(dir, "good-interp")
		if err := os.WriteFile(goodScript, []byte("#!/bin/sh\n"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := checkWrappedTarget(goodScript, dir); err != nil {
			t.Fatalf("valid interpreter = %v", err)
		}
		if err := checkWrappedTarget(dir, dir); err != nil {
			t.Fatalf("directory target = %v", err)
		}
	})

	t.Run("recordedExitAt", func(t *testing.T) {
		now := time.Now().UTC().Truncate(time.Second)
		h := scruntime.Handle{PID: 42, ProcessStartTime: now}
		code0, code2, codeBad := 0, 2, 300
		tok := "sk-" + strings.Repeat("c", 28)

		cases := []struct {
			name     string
			exitBody string
			stdout   string
			stderr   string
			wantNil  bool
			wantErr  string
			wantCode int
			wantResp string
			wantMsg  string
		}{
			{name: "no exit file", wantNil: true},
			{name: "invalid JSON", exitBody: "{", wantErr: "invalid coding exit record"},
			{name: "exit code out of range", exitBody: mustMarshal(t, exitRecord{workerIdentity: workerIdentity{42, now}, Code: &codeBad}), wantErr: "invalid coding exit record"},
			{name: "PID mismatch", exitBody: mustMarshal(t, exitRecord{workerIdentity: workerIdentity{99, now}, Code: &code0}), wantErr: "invalid coding exit record"},
			{name: "oversized exit file", exitBody: strings.Repeat(" ", 600), wantErr: "oversized"},
			{name: "clean exit redacts stdout", exitBody: mustMarshal(t, exitRecord{workerIdentity: workerIdentity{42, now}, Code: &code0}), stdout: "answer " + tok, stderr: "", wantCode: 0, wantResp: "answer "},
			{name: "nonzero exit redacts stderr", exitBody: mustMarshal(t, exitRecord{workerIdentity: workerIdentity{42, now}, Code: &code2}), stderr: "boom " + tok, wantCode: 2, wantMsg: "boom "},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				base := filepath.Join(t.TempDir(), "task")
				runDir := base + ".run"
				if err := os.MkdirAll(runDir, 0o700); err != nil {
					t.Fatal(err)
				}
				if tc.exitBody != "" {
					if err := os.WriteFile(filepath.Join(runDir, "exit"), []byte(tc.exitBody), 0o600); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(filepath.Join(runDir, "stderr"), []byte(tc.stderr), 0o600); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(filepath.Join(runDir, "stdout"), []byte(tc.stdout), 0o600); err != nil {
						t.Fatal(err)
					}
				}
				res, err := recordedExitAt(base, h)
				if tc.wantErr != "" {
					if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
						t.Fatalf("recordedExitAt = %+v, %v; want error %q", res, err, tc.wantErr)
					}
					return
				}
				if err != nil {
					t.Fatalf("recordedExitAt unexpected error: %v", err)
				}
				if tc.wantNil {
					if res != nil {
						t.Fatalf("got %+v, want nil", res)
					}
					return
				}
				if res == nil || res.ExitCode != tc.wantCode {
					t.Fatalf("res = %+v, want code %d", res, tc.wantCode)
				}
				if strings.Contains(res.Response, tok) || strings.Contains(res.Message, tok) {
					t.Errorf("secret leaked in result: %+v", res)
				}
				if tc.wantResp != "" && !strings.Contains(res.Response, tc.wantResp) {
					t.Errorf("response = %q, want %q", res.Response, tc.wantResp)
				}
				if tc.wantMsg != "" && !strings.Contains(res.Message, tc.wantMsg) {
					t.Errorf("message = %q, want %q", res.Message, tc.wantMsg)
				}
			})
		}
	})
}

func mustMarshal(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestContainerHelpersAndValidation(t *testing.T) {
	t.Run("matchesContainerHandle", func(t *testing.T) {
		id := &containerIdentity{ContainerID: "cid-1", ContainerName: "name-1", CreateAttempt: "att-1"}
		for _, tc := range []struct {
			name string
			c    *containerIdentity
			h    scruntime.Handle
			want bool
		}{
			{"nil identity", nil, scruntime.Handle{ContainerID: "cid-1"}, false},
			{"matching ID", id, scruntime.Handle{ContainerID: "cid-1"}, true},
			{"mismatched ID", id, scruntime.Handle{ContainerID: "cid-2"}, false},
			{"matching name and attempt", id, scruntime.Handle{ContainerName: "name-1", CreateAttempt: "att-1"}, true},
			{"name only without attempt", id, scruntime.Handle{ContainerName: "name-1"}, false},
			{"mismatched attempt", id, scruntime.Handle{ContainerName: "name-1", CreateAttempt: "att-2"}, false},
		} {
			t.Run(tc.name, func(t *testing.T) {
				if got := matchesContainerHandle(tc.c, tc.h); got != tc.want {
					t.Fatalf("matchesContainerHandle = %t, want %t", got, tc.want)
				}
			})
		}
	})

	t.Run("councilCheckoutKind", func(t *testing.T) {
		for _, tc := range []struct {
			mode     string
			readOnly bool
			want     scruntime.CouncilCheckoutKind
			wantErr  bool
		}{
			{checkoutModeLive, true, scruntime.CouncilLive, false},
			{checkoutModeRef, true, scruntime.CouncilRef, false},
			{checkoutModeWriter, true, scruntime.CouncilReview, false},
			{checkoutModeWriter, false, scruntime.CouncilWriter, false},
			{"other", false, "", true},
		} {
			got, err := councilCheckoutKind(tc.mode, tc.readOnly)
			if (err != nil) != tc.wantErr || got != tc.want {
				t.Fatalf("councilCheckoutKind(%q,%t) = %q, %v", tc.mode, tc.readOnly, got, err)
			}
		}
	})

	t.Run("councilLimits and resolveProxyImageID", func(t *testing.T) {
		a := NewAdapter(t.TempDir())
		a.WallTime = 5 * time.Minute
		lim, err := a.councilLimits(time.Now().Add(30 * time.Second))
		if err != nil || lim.NanoCPUs != defaultCouncilNanoCPUs || lim.MemoryBytes != defaultCouncilMemoryBytes || lim.PIDs != defaultCouncilPIDs || lim.WallTime > 31*time.Second {
			t.Fatalf("councilLimits = %+v, %v", lim, err)
		}
		if _, err := a.councilLimits(time.Now().Add(-time.Second)); err == nil || !strings.Contains(err.Error(), "deadline reached") {
			t.Fatalf("past deadline = %v", err)
		}
		if _, err := a.resolveProxyImageID(context.Background()); err == nil {
			t.Fatal("accepted empty proxy image")
		}
		a.CouncilProxyImage = "repo:tag"
		if _, err := a.resolveProxyImageID(context.Background()); err == nil || !strings.Contains(err.Error(), "local image ID resolution") {
			t.Fatalf("unresolved tag = %v", err)
		}
		a.CouncilProxyImage = "sha256:" + strings.Repeat("d", 64)
		if got, err := a.resolveProxyImageID(context.Background()); err != nil || got != a.CouncilProxyImage {
			t.Fatalf("digest proxy image = %q, %v", got, err)
		}
	})

	t.Run("validateCouncilContainerLaunch", func(t *testing.T) {
		validAdapter := func() *Adapter {
			a := NewAdapter(t.TempDir())
			a.CouncilRunner = newFakeCouncilRunner()
			a.CouncilProxy = newFakeProxyDocker()
			a.CouncilProxyImage = "sha256:" + strings.Repeat("e", 64)
			a.DaemonListen = "[::1]:7432"
			return a
		}
		validSpec := scruntime.LaunchSpec{TaskToken: "sc_task_1", CouncilDeadline: time.Now().Add(time.Minute)}
		validInput := launchInput{checkoutMode: checkoutModeLive}

		for _, tc := range []struct {
			name   string
			mutate func(*Adapter, *scruntime.LaunchSpec, *launchInput)
			want   string
		}{
			{"missing runner", func(a *Adapter, _ *scruntime.LaunchSpec, _ *launchInput) { a.CouncilRunner = nil }, "runner unavailable"},
			{"missing proxy", func(a *Adapter, _ *scruntime.LaunchSpec, _ *launchInput) { a.CouncilProxy = nil }, "proxy engine unavailable"},
			{"missing proxy image", func(a *Adapter, _ *scruntime.LaunchSpec, _ *launchInput) { a.CouncilProxyImage = " " }, "proxy image is required"},
			{"missing listen addr", func(a *Adapter, _ *scruntime.LaunchSpec, _ *launchInput) { a.DaemonListen = "" }, "listen address is required"},
			{"missing task token", func(_ *Adapter, s *scruntime.LaunchSpec, _ *launchInput) { s.TaskToken = "" }, "scoped task lease"},
			{"missing checkout mode", func(_ *Adapter, _ *scruntime.LaunchSpec, in *launchInput) { in.checkoutMode = "" }, "explicit council checkout mode"},
			{"council checkout without a council seat", func(_ *Adapter, s *scruntime.LaunchSpec, _ *launchInput) {
				s.CouncilDeadline = time.Time{}
			}, "council checkout mode requires a council seat"},
			{"writer checkout without a council seat", func(_ *Adapter, s *scruntime.LaunchSpec, in *launchInput) {
				s.CouncilDeadline = time.Time{}
				in.checkoutMode = checkoutModeWriter
			}, "council checkout mode requires a council seat"},
			{"expired deadline", func(_ *Adapter, s *scruntime.LaunchSpec, _ *launchInput) {
				s.CouncilDeadline = time.Now().Add(-time.Second)
			}, "deadline reached"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				a := validAdapter()
				s, in := validSpec, validInput
				tc.mutate(a, &s, &in)
				if err := a.validateCouncilContainerLaunch(s, in); err == nil || !strings.Contains(err.Error(), tc.want) {
					t.Fatalf("validateCouncilContainerLaunch = %v, want %q", err, tc.want)
				}
			})
		}
	})

	t.Run("buildContainerProcessResult and writeContainerLog", func(t *testing.T) {
		tok := "sk-" + strings.Repeat("d", 28)
		okRes := buildContainerProcessResult(scruntime.CouncilResult{ExitCode: 0, Stdout: "done " + tok, Stderr: "warn"})
		if okRes.ExitCode != 0 || strings.Contains(okRes.Response, tok) || !strings.Contains(okRes.Response, "done ") || okRes.Message != "warn" {
			t.Fatalf("okRes = %+v", okRes)
		}
		failRes := buildContainerProcessResult(scruntime.CouncilResult{
			ExitCode: 7,
			Stdout:   "ignored on failure",
			Stderr:   strings.Repeat("x", maxDiagnosticTail+100) + "TAIL",
		})
		if failRes.ExitCode != 7 || failRes.Response != "" || !strings.Contains(failRes.Message, "status 7") || !strings.HasSuffix(failRes.Message, "TAIL") {
			t.Fatalf("failRes = %+v", failRes)
		}

		base := filepath.Join(t.TempDir(), "task")
		if err := writeContainerLog(base, scruntime.CouncilResult{}); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(base + ".log"); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("empty result created log file: %v", err)
		}
		if err := writeContainerLog(base, scruntime.CouncilResult{Stdout: "out " + tok, Stderr: "err"}); err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(base + ".log")
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(data), tok) || !strings.Contains(string(data), "out ") || !strings.Contains(string(data), "\nerr\n") {
			t.Fatalf("log content = %q", data)
		}
	})
}
