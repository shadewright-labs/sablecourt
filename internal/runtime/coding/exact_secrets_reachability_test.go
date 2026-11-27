package coding

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/shadewright-labs/sablecourt/internal/attach"
	"github.com/shadewright-labs/sablecourt/internal/domain"
	"github.com/shadewright-labs/sablecourt/internal/operatortoken"
	"github.com/shadewright-labs/sablecourt/internal/redact"
	scruntime "github.com/shadewright-labs/sablecourt/internal/runtime"
	"github.com/shadewright-labs/sablecourt/internal/secret"
)

func synthCodingSecret(parts ...string) string {
	return strings.Join(parts, "")
}

func TestSessionOutputRedactsExactSecretsAllForms(t *testing.T) {
	reg := redact.NewRegistry()
	opaque := synthCodingSecret("coding", "_sess_", "opaque_991122")
	special := synthCodingSecret(`cod"ing`, `\val`, `<>&/`, "778899")
	reg.Register(opaque, special)

	for _, sec := range []struct {
		name, value string
	}{
		{"opaque", opaque},
		{"special", special},
	} {
		jb, _ := json.Marshal(sec.value)
		q := strconv.Quote(sec.value)
		mid := len(sec.value) / 2
		forms := []struct {
			name    string
			payload string
			forbid  []string
		}{
			{"raw", "line " + sec.value + "\n", []string{sec.value}},
			{"quoted", "q=" + q + "\n", []string{sec.value, q[1 : len(q)-1]}},
			{"json_escaped", `{"k":` + string(jb) + "}\n", []string{sec.value, string(jb[1 : len(jb)-1])}},
			{"split_crlf", "a " + sec.value[:mid] + "\r\n" + sec.value[mid:] + " b\n", []string{sec.value, sec.value[:mid], sec.value[mid:]}},
		}
		for _, f := range forms {
			t.Run(sec.name+"/"+f.name, func(t *testing.T) {
				var fileBuf, archBuf strings.Builder
				hub := attach.NewHubWithRegistry(reg)
				hub.Register("task-out", nil)
				out := &sessionOutput{
					file:    &fileBuf,
					archive: &boundedLog{file: &archBuf},
					hub:     hub,
					taskID:  "task-out",
					known:   reg.NewStream(),
				}
				cut := len(f.payload) / 2
				if _, err := out.Write([]byte(f.payload[:cut])); err != nil {
					t.Fatalf("Write part 1: %v", err)
				}
				if _, err := out.Write([]byte(f.payload[cut:])); err != nil {
					t.Fatalf("Write part 2: %v", err)
				}
				if err := out.Flush(); err != nil {
					t.Fatalf("Flush: %v", err)
				}
				hub.Close("task-out")
				hist, _, _, unsub, err := hub.Subscribe("task-out")
				if err != nil {
					t.Fatalf("Subscribe: %v", err)
				}
				unsub()

				for _, dst := range []struct {
					label, text string
				}{
					{"file", fileBuf.String()},
					{"archive", archBuf.String()},
					{"hub", hist},
				} {
					for _, bad := range f.forbid {
						if len(bad) < redact.MinSecretLength {
							continue
						}
						if strings.Contains(dst.text, bad) {
							t.Fatalf("%s leaked %q in %q", dst.label, bad, dst.text)
						}
					}
				}
			})
		}
	}
}

func TestHostWorkloadsCannotReachDaemonSecrets(t *testing.T) {
	ctx := context.Background()

	// Create daemon state dir with operator.token, secrets.json, and sablecourt.db.
	stateDir := filepath.Join(t.TempDir(), "daemon-state")
	t.Setenv("SABLECOURT_STATE_DIR", stateDir)
	opToken, err := operatortoken.LoadOrCreate(stateDir)
	if err != nil {
		t.Fatalf("operatortoken.LoadOrCreate: %v", err)
	}

	providerKey := synthCodingSecret("prov", "_host_", "reach_secret_112233")
	mcpSecret := synthCodingSecret("upmcp", "_host_", "reach_secret_445566")
	webhookSecret := synthCodingSecret("whsec", "_host_", "reach_secret_778899")
	pgPassword := synthCodingSecret("pgpw", "_host_", "reach_secret_334455")
	otherTaskToken := synthCodingSecret("sc_", "task_", "fedcba9876543210fedcba9876543210")

	secretFile := filepath.Join(stateDir, "secrets.json")
	fs := secret.NewFileStore(secretFile)
	for k, v := range map[string]string{
		"openai_api_key":        providerKey,
		"mcp_upstream_secret":   mcpSecret,
		"github_webhook_secret": webhookSecret,
	} {
		if err := fs.Set(ctx, k, v); err != nil {
			t.Fatalf("FileStore.Set %s: %v", k, err)
		}
	}
	dbPath := filepath.Join(stateDir, "sablecourt.db")
	testWrite(t, stateDir, "sablecourt.db", "daemon-db-"+webhookSecret+"\n", 0o600)

	// Populate daemon process environment with raw secrets to confirm strippedEnv never inherits them.
	t.Setenv("OPENAI_API_KEY", providerKey)
	t.Setenv("ANTHROPIC_API_KEY", providerKey)
	t.Setenv("GITHUB_TOKEN", mcpSecret)
	t.Setenv("SABLECOURT_OPERATOR_TOKEN", opToken)
	t.Setenv("SABLECOURT_POSTGRES_DSN", "postgres://svc:"+pgPassword+"@127.0.0.1:5432/sablecourt")
	t.Setenv("SABLECOURT_NOTIFY_WEBHOOK_URL", "https://hooks.example.invalid/"+webhookSecret)

	daemonSecrets := []string{opToken, providerKey, mcpSecret, webhookSecret, pgPassword, otherTaskToken}
	redact.Register(daemonSecrets...)

	repo := initTempGitRepo(t)
	headCommit := strings.TrimSpace(testGit(t, repo, "rev-parse", "HEAD"))
	worktreeRoot := filepath.Join(t.TempDir(), "worktrees")

	assertNoDaemonSecret := func(t *testing.T, label, text string) {
		t.Helper()
		for _, sec := range daemonSecrets {
			if strings.Contains(text, sec) {
				t.Fatalf("%s exposed daemon-held secret", label)
			}
		}
	}

	// Build a fake harness binary path for buildCommand inspection.
	harnessDir := t.TempDir()
	harnessPath := filepath.Join(harnessDir, "claude")
	testWrite(t, harnessDir, "claude", "#!/bin/sh\nexit 0\n", 0o700)

	modes := []struct {
		name      string
		inputJSON string
		readOnly  bool
	}{
		{
			name:      "coding_task_host",
			inputJSON: fmt.Sprintf(`{"prompt":"fix bug %s","context_packet":"ctx %s"}`, providerKey, opToken),
			readOnly:  false,
		},
		{
			name:      "council_member_host_live",
			inputJSON: fmt.Sprintf(`{"prompt":"review %s","conclaveId":"ccl-reach-1","role":"claude","checkoutMode":"live"}`, mcpSecret),
			readOnly:  true,
		},
		{
			name:      "council_member_host_ref",
			inputJSON: fmt.Sprintf(`{"prompt":"review %s","conclaveId":"ccl-reach-2","role":"claude","checkoutMode":"ref","checkoutRef":%q}`, webhookSecret, headCommit),
			readOnly:  true,
		},
		{
			name:      "council_member_host_writer",
			inputJSON: fmt.Sprintf(`{"prompt":"edit %s","conclaveId":"ccl-reach-3","role":"claude","checkoutMode":"writer","checkoutWriter":"claude"}`, pgPassword),
			readOnly:  false,
		},
	}

	sandboxMode := "none"
	if runtime.GOOS == "darwin" {
		sandboxMode = "seatbelt"
	}

	for _, tc := range modes {
		t.Run(tc.name, func(t *testing.T) {
			adapter := NewAdapter(worktreeRoot)
			adapter.DryRun = true
			adapter.HarnessPaths = map[string]string{"claude": harnessPath}
			spec := scruntime.LaunchSpec{
				TaskID:    "reach-" + tc.name,
				InputJSON: tc.inputJSON,
				TaskToken: synthCodingSecret("sc_", "task_", "0123456789abcdef0123456789abcdef"),
				Workload: domain.Workload{
					Metadata: domain.ObjectMeta{Name: "coder-" + tc.name, Zone: domain.ZoneRuntime},
					Spec: domain.WorkloadSpec{
						Class: domain.ClassCodingAgent,
						Coding: &domain.CodingConfig{
							RepoPath: repo,
							Harness:  "claude",
							Sandbox:  sandboxMode,
						},
					},
				},
			}
			handle, err := adapter.Start(ctx, spec)
			if err != nil {
				t.Fatalf("DryRun Start: %v", err)
			}
			t.Cleanup(func() { _ = adapter.Stop(context.Background(), handle) })

			input, err := parseLaunchInput(spec.InputJSON)
			if err != nil {
				t.Fatalf("parseLaunchInput: %v", err)
			}
			scratch := t.TempDir()
			taskBase := handle.WorkDir
			if handle.LogPath != "" {
				taskBase = strings.TrimSuffix(handle.LogPath, ".log")
			}
			state, err := readWorktreeState(taskBase + stateSuffix)
			if err != nil {
				t.Fatalf("readWorktreeState: %v", err)
			}
			cmd, profile, err := adapter.buildCommand(spec, input, handle.WorkDir, scratch, state, tc.readOnly)
			if err != nil {
				t.Fatalf("buildCommand: %v", err)
			}

			// 1. Check built environment and argv.
			assertNoDaemonSecret(t, "cmd.Env", strings.Join(cmd.Env, "\n"))
			assertNoDaemonSecret(t, "cmd.Args", strings.Join(cmd.Args, "\n"))

			// 2. Check Seatbelt profile does not grant access to stateDir, operator.token, secrets.json, or db.
			if profile != "" {
				resolvedState, _ := resolvedPath(stateDir)
				for _, p := range []string{stateDir, resolvedState, secretFile, dbPath} {
					if p != "" && strings.Contains(profile, fmt.Sprintf("(subpath %q)", p)) {
						t.Fatalf("seatbelt profile granted subpath access to daemon state %q", p)
					}
					if p != "" && strings.Contains(profile, fmt.Sprintf("(literal %q)", p)) && strings.Contains(profile, "file-read*") {
						if strings.Contains(profile, fmt.Sprintf("file-read* file-test-existence (literal %q)", p)) {
							t.Fatalf("seatbelt profile granted file-read access to %q", p)
						}
					}
				}
			}

			// 3. Check every file created inside workdir and scratch.
			for _, dir := range []string{handle.WorkDir, scratch} {
				err := filepath.WalkDir(dir, func(path string, d os.DirEntry, walkErr error) error {
					if walkErr != nil || d.IsDir() || d.Name() == ".git" {
						if d != nil && d.IsDir() && d.Name() == ".git" {
							return filepath.SkipDir
						}
						return nil
					}
					data, readErr := os.ReadFile(path)
					if readErr != nil {
						return nil
					}
					assertNoDaemonSecret(t, "file "+filepath.Base(path), string(data))
					return nil
				})
				if err != nil {
					t.Fatalf("WalkDir %s: %v", dir, err)
				}
			}
		})
	}

	// If native Seatbelt works in this environment, start a real coding task and
	// verify live env and denied reads of operator.token, secrets.json, and db.
	if err := CheckSandbox(ctx); err == nil {
		t.Run("coding_task_host_live_seatbelt", func(t *testing.T) {
			tokenPath := filepath.Join(stateDir, operatortoken.Filename)
			// The session log is redacted, so the probe checks its own
			// environment for each secret instead of printing it.
			var envChecks strings.Builder
			for i, sec := range daemonSecrets {
				fmt.Fprintf(&envChecks, "if /usr/bin/env | /usr/bin/grep -qF -- '%s'; then printf 'ENV_LEAK:%d\\n'; exit 42; fi\n", sec, i)
			}
			probeScript := fmt.Sprintf(`#!/bin/sh
set -u
%s
for target in %q %q %q; do
  if /bin/cat "$target" >/dev/null 2>&1; then
    printf 'READABLE:%%s\n' "$target"
    exit 41
  fi
done
printf 'REACHABILITY_OK\n'
`, envChecks.String(), tokenPath, secretFile, dbPath)
			liveHarness := filepath.Join(t.TempDir(), "claude")
			if err := os.WriteFile(liveHarness, []byte(probeScript), 0o700); err != nil {
				t.Fatalf("write live harness: %v", err)
			}
			adapter := NewAdapter(worktreeRoot)
			adapter.HarnessPaths = map[string]string{"claude": liveHarness}
			spec := scruntime.LaunchSpec{
				TaskID:    "reach-live-host",
				InputJSON: `{"prompt":"check reachability"}`,
				TaskToken: synthCodingSecret("sc_", "task_", "11223344556677881122334455667788"),
				Workload: domain.Workload{
					Metadata: domain.ObjectMeta{Name: "coder-live", Zone: domain.ZoneRuntime},
					Spec: domain.WorkloadSpec{
						Class: domain.ClassCodingAgent,
						Coding: &domain.CodingConfig{
							RepoPath: repo,
							Harness:  "claude",
							Sandbox:  "seatbelt",
						},
					},
				},
			}
			handle, err := adapter.Start(ctx, spec)
			if err != nil {
				t.Fatalf("Start live seatbelt coding task: %v", err)
			}
			t.Cleanup(func() { _ = adapter.Stop(context.Background(), handle) })
			waitUntil(t, func() bool { return outputClosed(adapter.process(handle.WorkDir).done) })
			logBytes, err := os.ReadFile(handle.WorkDir + ".log")
			if err != nil {
				t.Fatalf("read task log: %v", err)
			}
			obs, err := adapter.Observe(ctx, handle)
			if err != nil || obs.State != scruntime.StateCompleted {
				t.Fatalf("live probe task state = %+v, err = %v\nlog:\n%s", obs, err, logBytes)
			}
			logText := string(logBytes)
			if !strings.Contains(logText, "REACHABILITY_OK") {
				t.Fatalf("live probe did not confirm reachability isolation:\n%s", logText)
			}
			assertNoDaemonSecret(t, "live task output/env", logText)
		})
	}
}
