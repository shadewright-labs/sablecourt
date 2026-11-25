package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
)

type harnessDocker struct {
	calls  int
	failAt int
}

func (d *harnessDocker) Run(_ context.Context, _ []string, input io.Reader) error {
	d.calls++
	_, _ = io.Copy(io.Discard, input)
	if d.failAt == d.calls {
		return errors.New("engine unavailable")
	}
	return nil
}

type harnessCompiler struct{}

func (harnessCompiler) Compile(context.Context) ([]byte, error) {
	return []byte("proxy executable"), nil
}

func TestHarnessBuildExecution(t *testing.T) {
	for _, tc := range []struct {
		name, role    string
		failAt, calls int
		wantError     bool
	}{
		{name: "claude", role: "claude", calls: 2},
		{name: "codex", role: "codex", calls: 2},
		{name: "agy", role: "agy", calls: 2},
		{name: "unknown", role: "unknown", wantError: true},
		{name: "proxy fails", role: "claude", failAt: 1, calls: 1, wantError: true},
		{name: "role fails", role: "codex", failAt: 2, calls: 2, wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			docker := &harnessDocker{failAt: tc.failAt}
			cmd := buildHarnessCmdWithCompiler(docker, harnessCompiler{})
			var out bytes.Buffer
			cmd.SetOut(&out)
			cmd.SetErr(io.Discard)
			cmd.SetArgs([]string{"build", tc.role})
			err := cmd.ExecuteContext(context.Background())
			if (err != nil) != tc.wantError || docker.calls != tc.calls {
				t.Fatalf("error %v, calls %d", err, docker.calls)
			}
			if strings.Contains(out.String(), "Built ") == tc.wantError {
				t.Fatalf("incorrect success output: %s", out.String())
			}
		})
	}
}

func TestHarnessBuildCommandRoles(t *testing.T) {
	for _, tc := range []struct {
		name      string
		args      []string
		wantError string
	}{
		{name: "claude", args: []string{"claude"}},
		{name: "codex", args: []string{"codex"}},
		{name: "agy", args: []string{"agy"}},
		{name: "unknown", args: []string{"unknown"}, wantError: "unknown harness role"},
		{name: "proxy is not a role", args: []string{"proxy"}, wantError: "unknown harness role"},
		{name: "missing", wantError: "accepts 1 arg"},
		{name: "extra", args: []string{"claude", "codex"}, wantError: "accepts 1 arg"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := newRootCmd()
			cmd, _, err := root.Find([]string{"harness", "build"})
			if err != nil || cmd.Name() != "build" {
				t.Fatalf("harness build is not registered: %v", err)
			}
			err = cmd.ValidateArgs(tc.args)
			if tc.wantError == "" && err != nil {
				t.Fatal(err)
			}
			if tc.wantError != "" && (err == nil || !strings.Contains(err.Error(), tc.wantError)) {
				t.Fatalf("error = %v, want %q", err, tc.wantError)
			}
		})
	}
}
