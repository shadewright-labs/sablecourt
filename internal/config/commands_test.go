package config

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/shadewright-labs/sablecourt/internal/domain"
)

// commandManifest is a coding workload with one allowed command and a
// workflow whose second step runs it. Each case edits one part.
func commandManifest(t *testing.T, editCmd func(map[string]any), editSteps func([]map[string]any) []map[string]any) string {
	t.Helper()
	cmd := map[string]any{"name": "check", "argv": []any{"/bin/sh", "-c", "make check"}, "timeoutSeconds": 60, "maxOutputBytes": 4096, "failureReason": "use coding-invariant"}
	if editCmd != nil {
		editCmd(cmd)
	}
	steps := []map[string]any{
		{"name": "author", "workload": "coder", "role": "author", "prompt": "p"},
		{"name": "check", "workload": "coder", "command": "check"},
	}
	if editSteps != nil {
		steps = editSteps(steps)
	}
	docs := []any{
		map[string]any{"apiVersion": domain.APIVersionV1Alpha1, "kind": "Workload", "metadata": map[string]any{"name": "coder", "zone": "runtime"},
			"spec": map[string]any{"class": "coding-agent", "runtime": map[string]any{"type": "worktree-cli"},
				"coding": map[string]any{"harness": "claude", "repoPath": "/srv/repo", "commands": []any{cmd}}}},
		map[string]any{"apiVersion": domain.APIVersionV1Alpha1, "kind": "Workload", "metadata": map[string]any{"name": "agent", "zone": "runtime"},
			"spec": map[string]any{"class": "long-lived-agent", "runtime": map[string]any{"type": "docker", "image": "example/agent:1"}}},
		map[string]any{"apiVersion": domain.APIVersionV1Alpha1, "kind": "Workload", "metadata": map[string]any{"name": "planner", "zone": "runtime"},
			"spec": map[string]any{"class": "deterministic-controller", "runtime": map[string]any{"type": "docker", "image": "example/planner:1"}}},
		map[string]any{"apiVersion": domain.APIVersionV1Alpha1, "kind": "Workflow", "metadata": map[string]any{"name": "checked"}, "spec": map[string]any{"steps": steps}},
	}
	var parts []string
	for _, d := range docs {
		raw, err := json.Marshal(d)
		if err != nil {
			t.Fatal(err)
		}
		parts = append(parts, string(raw))
	}
	return strings.Join(parts, "\n---\n")
}

// TestLoaderCommandRefusals is the loader's half of the allowed set: every
// command or command step it cannot run safely is refused at load.
func TestLoaderCommandRefusals(t *testing.T) {
	setCmd := func(key string, value any) func(map[string]any) { return func(c map[string]any) { c[key] = value } }
	setStep := func(i int, key string, value any) func([]map[string]any) []map[string]any {
		return func(s []map[string]any) []map[string]any { s[i][key] = value; return s }
	}
	// review is a model verifier on its own workload, appended after the steps.
	review := map[string]any{"name": "review", "workload": "agent", "role": "verifier", "prompt": "review"}
	steps := func(s ...map[string]any) func([]map[string]any) []map[string]any {
		return func([]map[string]any) []map[string]any { return s }
	}
	author := map[string]any{"name": "author", "workload": "coder", "role": "author", "prompt": "p"}
	check := map[string]any{"name": "check", "workload": "coder", "command": "check"}
	plan := map[string]any{"name": "plan", "workload": "planner"}
	many := make([]any, domain.MaxCommandArgs+1)
	for i := range many {
		many[i] = "make"
	}
	for name, tc := range map[string]struct {
		cmd   func(map[string]any)
		steps func([]map[string]any) []map[string]any
		want  string // empty: the manifest loads
	}{
		"allowed": {},
		"defaults": {cmd: func(c map[string]any) {
			delete(c, "timeoutSeconds")
			delete(c, "maxOutputBytes")
			delete(c, "failureReason")
		}},
		"name with uppercase":      {cmd: setCmd("name", "Check"), want: "name must match"},
		"name with a slash":        {cmd: setCmd("name", "a/b"), want: "name must match"},
		"empty name":               {cmd: setCmd("name", ""), want: "name must match"},
		"no argv":                  {cmd: setCmd("argv", []any{}), want: "argv must have"},
		"too many arguments":       {cmd: setCmd("argv", many), want: "argv must have"},
		"option as program":        {cmd: setCmd("argv", []any{"--version"}), want: "argv[0]"},
		"blank program":            {cmd: setCmd("argv", []any{"  ", "x"}), want: "argv[0]"},
		"NUL in an argument":       {cmd: setCmd("argv", []any{"make", "a\x00b"}), want: "without NUL"},
		"argument too long":        {cmd: setCmd("argv", []any{"make", strings.Repeat("a", domain.MaxCommandArgBytes+1)}), want: "without NUL"},
		"negative timeout":         {cmd: setCmd("timeoutSeconds", -1), want: "timeoutSeconds"},
		"timeout over an hour":     {cmd: setCmd("timeoutSeconds", 3601), want: "timeoutSeconds"},
		"negative output cap":      {cmd: setCmd("maxOutputBytes", -1), want: "maxOutputBytes"},
		"output cap over 1 MiB":    {cmd: setCmd("maxOutputBytes", domain.MaxCommandOutputBytes+1), want: "maxOutputBytes"},
		"multi-line reason":        {cmd: setCmd("failureReason", "a\nb"), want: "failureReason"},
		"reason too long":          {cmd: setCmd("failureReason", strings.Repeat("r", domain.MaxCommandFailureReason+1)), want: "failureReason"},
		"step names unknown":       {steps: setStep(1, "command", "deploy"), want: "not in the coding.commands"},
		"command as first step":    {steps: steps(check, author), want: "cannot be the first step"},
		"command after a verifier": {steps: steps(author, review, check), want: "must follow an author or command step"},
		"verifier after a command": {steps: steps(author, check, review)},
		// The command step's coding workload runs no model there, so it does
		// not give a model verifier anything to check.
		"model verifier after only a command": {steps: steps(plan, check, review), want: "nothing to check"},
		"non-coding workload":                 {steps: setStep(1, "workload", "agent"), want: "need a coding-agent workload"},
		"step sets a prompt":                  {steps: setStep(1, "prompt", "run make"), want: "cannot set role"},
		"step sets a role":                    {steps: setStep(1, "role", "verifier"), want: "cannot set role"},
		"step sets a tier":                    {steps: setStep(1, "tier", "coding"), want: "cannot set role"},
		"step sets a model":                   {steps: setStep(1, "model", "claude-x"), want: "cannot set role"},
		"step sets maxRetries":                {steps: setStep(1, "maxRetries", 1), want: "cannot set role"},
	} {
		t.Run(name, func(t *testing.T) {
			err := loadDocs(t, commandManifest(t, tc.cmd, tc.steps))
			if tc.want == "" {
				if err != nil {
					t.Fatalf("manifest refused: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}
}

// TestLoaderRefusesDuplicateCommand covers the one refusal that needs two entries.
func TestLoaderRefusesDuplicateCommand(t *testing.T) {
	err := validateCommands([]domain.CodingCommand{{Name: "check", Argv: []string{"make"}}, {Name: "check", Argv: []string{"make", "x"}}})
	if err == nil || !strings.Contains(err.Error(), "duplicate command") {
		t.Fatalf("err = %v", err)
	}
}
