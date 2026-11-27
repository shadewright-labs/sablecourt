package main

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDoctorReportsDaemonState(t *testing.T) {
	cases := []struct {
		name    string
		daemon  *cliDaemon
		want    []string
		notWant []string
	}{
		{
			name: "healthy daemon with providers and voice",
			daemon: newCLIDaemon().
				on("GET /healthz", http.StatusOK, "ok").
				on("GET /api/v1/audit/verify", http.StatusOK, `{"valid":true,"verifiedRows":7}`).
				on("GET /api/v1/providers", http.StatusOK, `[{"id":"openai","connected":true},{"id":"gemini","connected":false},{"id":"local","connected":true}]`).
				on("GET /api/v1/voice/status", http.StatusOK, `{"sttUrl":"local-stt","ttsBackend":"say"}`),
			want:    []string{"healthy", "audit SHA-256 chain: valid (7 rows)", "connected providers: openai, local", "voice: stt=local-stt tts=say"},
			notWant: []string{"gemini", "unreachable"},
		},
		{
			name: "no connected providers",
			daemon: newCLIDaemon().
				on("GET /healthz", http.StatusOK, "ok").
				on("GET /api/v1/providers", http.StatusOK, `[{"id":"openai","connected":false}]`),
			want: []string{"healthy", "connected providers: none configured"},
		},
		{
			name: "audit chain reported invalid with 200",
			daemon: newCLIDaemon().
				on("GET /healthz", http.StatusOK, "ok").
				on("GET /api/v1/audit/verify", http.StatusOK, `{"valid":false,"verifiedRows":2}`),
			want:    []string{"audit SHA-256 chain: BROKEN"},
			notWant: []string{"chain: valid"},
		},
		{
			name:    "unreachable daemon",
			daemon:  newCLIDaemon().on("GET /healthz", http.StatusServiceUnavailable, "starting"),
			want:    []string{"unreachable (HTTP 503: starting)"},
			notWant: []string{"healthy", "audit"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("PATH", t.TempDir())
			out, _, err := runCLI(t, tc.daemon, "", "doctor")
			if err != nil {
				t.Fatalf("doctor: %v", err)
			}
			for _, w := range tc.want {
				if !strings.Contains(out, w) {
					t.Errorf("output %q does not contain %q", out, w)
				}
			}
			for _, w := range tc.notWant {
				if strings.Contains(out, w) {
					t.Errorf("output %q contains %q", out, w)
				}
			}
			if !strings.Contains(out, "tool docker") || !strings.Contains(out, "not on PATH") {
				t.Errorf("tool check missing from output %q", out)
			}
		})
	}
}

func TestDoctorFlagsBrokenAuditChainFromDaemon(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	d := newCLIDaemon().
		on("GET /healthz", http.StatusOK, "ok").
		on("GET /api/v1/audit/verify", http.StatusConflict, `{"valid":false,"verifiedRows":3,"error":"row 4 hash mismatch"}`)
	out, _, err := runCLI(t, d, "", "doctor")
	if err != nil {
		t.Fatalf("doctor: %v", err)
	}
	if !strings.Contains(out, "audit SHA-256 chain: BROKEN") {
		t.Fatalf("doctor output %q does not report the broken audit chain", out)
	}
}

func TestDoctorFindsToolsOnPath(t *testing.T) {
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "docker"), []byte("#!/bin/sh\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	out, _, err := runCLI(t, newCLIDaemon(), "", "doctor")
	if err != nil {
		t.Fatalf("doctor: %v", err)
	}
	if !strings.Contains(out, filepath.Join(bin, "docker")) {
		t.Errorf("docker path missing from %q", out)
	}
	if !strings.Contains(out, "unreachable") {
		t.Errorf("daemon state missing from %q", out)
	}
}

func TestInitRefusesToOverwriteWithoutForce(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "codex"), []byte("#!/bin/sh\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	out := filepath.Join(dir, "sablecourt.yaml")
	if err := os.WriteFile(out, []byte("keep me"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, _, err := runCLI(t, newCLIDaemon(), "", "init", "--out", out)
	if err == nil || !strings.Contains(err.Error(), "already exists (pass --force to overwrite)") {
		t.Fatalf("err = %v, want overwrite refusal", err)
	}
	if raw, _ := os.ReadFile(out); string(raw) != "keep me" {
		t.Fatalf("refused init changed the file: %q", raw)
	}
	stdout, _, err := runCLI(t, newCLIDaemon(), "", "init", "--out", out, "--force")
	if err != nil {
		t.Fatalf("init --force: %v", err)
	}
	if !strings.Contains(stdout, "harness=codex") {
		t.Errorf("output %q does not name the harness found on PATH", stdout)
	}
	raw, _ := os.ReadFile(out)
	if !strings.Contains(string(raw), "harness: codex") || !strings.Contains(string(raw), "defaultEffect: deny") {
		t.Errorf("manifest missing harness or deny default:\n%s", raw)
	}
	info, _ := os.Stat(out)
	if info.Mode().Perm() != 0o600 {
		t.Errorf("manifest mode = %o, want 0600", info.Mode().Perm())
	}
}

func TestTaskDiffPrintsGitDiffArtifact(t *testing.T) {
	diff := "--- a/x\n+++ b/x\n@@ -1 +1 @@\n-old\n+new\n context"
	arts, _ := json.Marshal([]map[string]string{{"name": "log", "text": "noise"}, {"name": "git-diff", "contentType": "text/x-diff", "text": diff}})
	task, _ := json.Marshal(map[string]any{"task": map[string]string{"ID": "t1", "ArtifactsJSON": string(arts)}})
	t.Run("plain", func(t *testing.T) {
		d := newCLIDaemon().on("GET /api/v1/tasks/t1", http.StatusOK, string(task))
		out, _, err := runCLI(t, d, "", "task", "diff", "t1")
		if err != nil {
			t.Fatalf("diff: %v", err)
		}
		for _, line := range strings.Split(diff, "\n") {
			if !strings.Contains(out, line+"\n") {
				t.Errorf("output %q missing line %q", out, line)
			}
		}
		if strings.Contains(out, "noise") {
			t.Errorf("printed a non-diff artifact: %q", out)
		}
	})
	t.Run("json", func(t *testing.T) {
		d := newCLIDaemon().on("GET /api/v1/tasks/t1", http.StatusOK, string(task))
		out, _, err := runCLI(t, d, "", "--json", "task", "diff", "t1")
		if err != nil {
			t.Fatalf("diff --json: %v", err)
		}
		var a struct {
			Name, Text string
		}
		if err := json.Unmarshal([]byte(out), &a); err != nil || a.Name != "git-diff" || a.Text != diff {
			t.Fatalf("diff --json = %q (%v)", out, err)
		}
	})
	t.Run("no diff artifact", func(t *testing.T) {
		d := newCLIDaemon().on("GET /api/v1/tasks/t1", http.StatusOK, `{"task":{"ID":"t1","ArtifactsJSON":"not json"}}`)
		out, _, err := runCLI(t, d, "", "task", "diff", "t1")
		if err != nil || !strings.Contains(out, "No git-diff artifact recorded for task t1") {
			t.Fatalf("out=%q err=%v", out, err)
		}
	})
}
