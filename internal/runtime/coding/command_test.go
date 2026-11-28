package coding

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/shadewright-labs/sablecourt/internal/domain"
	"github.com/shadewright-labs/sablecourt/internal/redact"
	scruntime "github.com/shadewright-labs/sablecourt/internal/runtime"
)

type stubRunner struct{ scruntime.CouncilRunner }

// TestValidateCommandLaunchRefusals checks every way the adapter refuses a
// command before it prepares a worktree: no container runner (host mode on
// or off), a launch that is not a pinned read-only standalone checkout, and
// a command outside the manifest's bounds.
func TestValidateCommandLaunchRefusals(t *testing.T) {
	t.Parallel()
	commit := strings.Repeat("a", 40)
	good := scruntime.CommandSpec{Name: "check", Argv: []string{"make", "check"}, Timeout: time.Minute, MaxOutputBytes: 1024}
	withContainers := &Adapter{CouncilRunner: stubRunner{}}
	for name, tc := range map[string]struct {
		adapter  *Adapter
		mutate   func(*scruntime.LaunchSpec, *scruntime.CommandSpec)
		input    launchInput
		commit   string
		readOnly bool
		reason   string
	}{
		"allowed":                   {adapter: withContainers, commit: commit, readOnly: true},
		"no container runner":       {adapter: &Adapter{}, commit: commit, readOnly: true, reason: scruntime.CommandNoContainerRuntime},
		"host mode is no fallback":  {adapter: &Adapter{HostModeUnsafe: true}, commit: commit, readOnly: true, reason: scruntime.CommandNoContainerRuntime},
		"launch not in a container": {adapter: withContainers, mutate: func(s *scruntime.LaunchSpec, _ *scruntime.CommandSpec) { s.CouncilContainer = false }, commit: commit, readOnly: true, reason: scruntime.CommandNoContainerRuntime},
		"no pinned commit":          {adapter: withContainers, readOnly: true, reason: scruntime.CommandNotAllowed},
		"writable checkout":         {adapter: withContainers, commit: commit, reason: scruntime.CommandNotAllowed},
		"council checkout":          {adapter: withContainers, input: launchInput{checkoutMode: checkoutModeLive}, commit: commit, readOnly: true, reason: scruntime.CommandNotAllowed},
		"council seat":              {adapter: withContainers, mutate: func(s *scruntime.LaunchSpec, _ *scruntime.CommandSpec) { s.CouncilDeadline = time.Now().Add(time.Hour) }, commit: commit, readOnly: true, reason: scruntime.CommandNotAllowed},
		"empty argv":                {adapter: withContainers, mutate: func(_ *scruntime.LaunchSpec, c *scruntime.CommandSpec) { c.Argv = nil }, commit: commit, readOnly: true, reason: scruntime.CommandNotAllowed},
		"option as program":         {adapter: withContainers, mutate: func(_ *scruntime.LaunchSpec, c *scruntime.CommandSpec) { c.Argv = []string{"--help"} }, commit: commit, readOnly: true, reason: scruntime.CommandNotAllowed},
		"NUL in argv":               {adapter: withContainers, mutate: func(_ *scruntime.LaunchSpec, c *scruntime.CommandSpec) { c.Argv = []string{"make", "a\x00b"} }, commit: commit, readOnly: true, reason: scruntime.CommandNotAllowed},
		"no time limit":             {adapter: withContainers, mutate: func(_ *scruntime.LaunchSpec, c *scruntime.CommandSpec) { c.Timeout = 0 }, commit: commit, readOnly: true, reason: scruntime.CommandNotAllowed},
		"time limit over ceiling": {adapter: withContainers, mutate: func(_ *scruntime.LaunchSpec, c *scruntime.CommandSpec) {
			c.Timeout = domain.MaxCommandTimeout + time.Second
		}, commit: commit, readOnly: true, reason: scruntime.CommandNotAllowed},
		"no output cap": {adapter: withContainers, mutate: func(_ *scruntime.LaunchSpec, c *scruntime.CommandSpec) { c.MaxOutputBytes = 0 }, commit: commit, readOnly: true, reason: scruntime.CommandNotAllowed},
		"output cap over ceiling": {adapter: withContainers, mutate: func(_ *scruntime.LaunchSpec, c *scruntime.CommandSpec) {
			c.MaxOutputBytes = domain.MaxCommandOutputBytes + 1
		}, commit: commit, readOnly: true, reason: scruntime.CommandNotAllowed},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			cmd := good
			cmd.Argv = append([]string(nil), good.Argv...)
			spec := scruntime.LaunchSpec{TaskID: "task_1", CouncilContainer: true, Command: &cmd}
			if tc.mutate != nil {
				tc.mutate(&spec, &cmd)
			}
			err := tc.adapter.validateCommandLaunch(spec, tc.input, tc.commit, tc.readOnly)
			if tc.reason == "" {
				if err != nil {
					t.Fatalf("allowed launch refused: %v", err)
				}
				return
			}
			var refusal *scruntime.CommandRefusal
			if !errors.As(err, &refusal) || refusal.Reason != tc.reason || refusal.Command != "check" {
				t.Fatalf("err = %v, want refusal %s", err, tc.reason)
			}
		})
	}
}

// TestBuildCommandProcessResult checks the outcome, the stored output and
// the exit the task reports for each way a command can end.
func TestBuildCommandProcessResult(t *testing.T) {
	t.Parallel()
	secret := "sk-ant-" + strings.Repeat("Z", 24)
	rec := commandRecord{Name: "check", MaxOutputBytes: 32}
	for name, tc := range map[string]struct {
		res       scruntime.CouncilResult
		outcome   string
		exit      int // the command's exit
		taskExit  int // what Observe reports
		output    string
		truncated bool
	}{
		"passed":        {res: scruntime.CouncilResult{Stdout: "ok\n"}, outcome: scruntime.CommandPassed, output: "ok"},
		"stderr joined": {res: scruntime.CouncilResult{Stdout: "out", Stderr: "warn"}, outcome: scruntime.CommandPassed, output: "out\nwarn"},
		"failed":        {res: scruntime.CouncilResult{ExitCode: 2, Stderr: "boom"}, outcome: scruntime.CommandFailed, exit: 2, taskExit: 2, output: "boom"},
		"redacted":      {res: scruntime.CouncilResult{ExitCode: 1, Stdout: "key " + secret}, outcome: scruntime.CommandFailed, exit: 1, taskExit: 1, output: "key [REDACTED"},
		// The cap keeps the last whole lines: the cut line is dropped.
		"over cap exit 0":             {res: scruntime.CouncilResult{Stdout: strings.Repeat("a", 40) + "\nTAIL"}, outcome: scruntime.CommandOutputOverCap, taskExit: -1, output: "TAIL", truncated: true},
		"over cap exit 3":             {res: scruntime.CouncilResult{ExitCode: 3, Stdout: strings.Repeat("b", 64) + "\nlast"}, outcome: scruntime.CommandOutputOverCap, exit: 3, taskExit: 3, output: "last", truncated: true},
		"over cap with no whole line": {res: scruntime.CouncilResult{Stdout: strings.Repeat("c", 64)}, outcome: scruntime.CommandOutputOverCap, taskExit: -1, truncated: true},
		// Redaction grows the output past the cap: that is a cut too.
		"redaction grows past cap":       {res: scruntime.CouncilResult{Stdout: strings.Repeat("api"+"_key=abcdef\n", 2) + "ok"}, outcome: scruntime.CommandOutputOverCap, taskExit: -1, output: "api_key=[REDACTED:secret]\nok", truncated: true},
		"runner overflow":                {res: scruntime.CouncilResult{Stdout: "[REDACTED:output limit]", OutputOverflow: true}, outcome: scruntime.CommandOutputOverCap, taskExit: -1, truncated: true},
		"runner overflow keeps the tail": {res: scruntime.CouncilResult{ExitCode: 2, Stdout: "FAIL: TestX", OutputOverflow: true}, outcome: scruntime.CommandOutputOverCap, exit: 2, taskExit: 2, output: "FAIL: TestX", truncated: true},
		"secret across streams": {
			res:     scruntime.CouncilResult{ExitCode: 1, Stdout: "a", Stderr: secret},
			outcome: scruntime.CommandFailed, exit: 1, taskExit: 1, output: "a\n[REDACTED",
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			out := buildCommandProcessResult(tc.res, rec)
			got := out.Command
			if got == nil || got.Command != "check" || got.Outcome != tc.outcome || got.ExitCode != tc.exit || out.ExitCode != tc.taskExit || got.OutputTruncated != tc.truncated {
				t.Fatalf("result = %+v (task exit %d), want outcome %s exit %d task exit %d truncated %v", got, out.ExitCode, tc.outcome, tc.exit, tc.taskExit, tc.truncated)
			}
			if !strings.HasPrefix(got.Output, tc.output) || len(got.Output) > rec.MaxOutputBytes || strings.Contains(got.Output, secret) {
				t.Fatalf("output = %q, want %q within %d bytes and redacted", got.Output, tc.output, rec.MaxOutputBytes)
			}
			if (tc.outcome == scruntime.CommandPassed) != (out.Message == "") {
				t.Fatalf("message = %q", out.Message)
			}
		})
	}
}

// TestCommandArtifactWithoutRunnerOutput checks the record of a command
// whose output was never captured: a timeout, a stop or a restart.
func TestCommandArtifactWithoutRunnerOutput(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		result  *processResult
		outcome string
	}{
		"timeout": {result: &processResult{ExitCode: -1, TimedOut: true}, outcome: scruntime.CommandTimedOut},
		"stopped": {result: &processResult{ExitCode: -1, Message: "coding task was stopped"}, outcome: scruntime.CommandFailed},
		"restart": {result: &processResult{ExitCode: 1}, outcome: scruntime.CommandFailed},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			art, ok := commandArtifact(&worktreeState{Command: &commandRecord{Name: "check", MaxOutputBytes: 10}, Result: tc.result})
			var res scruntime.CommandResult
			if !ok || art.Name != scruntime.CommandResultArtifact || json.Unmarshal([]byte(art.Text), &res) != nil ||
				res.Outcome != tc.outcome || res.Command != "check" || res.Output != "" {
				t.Fatalf("artifact = %+v, %v", art, ok)
			}
		})
	}
	if _, ok := commandArtifact(&worktreeState{Result: &processResult{}}); ok {
		t.Fatal("a harness task got a command artifact")
	}
}

// TestBuildCommandProcessResultTailIsRedactionStable checks the cap cut: a
// cut that leaves a secret-shaped fragment is redacted and cut again, so the
// stored output stays within the cap and redacting it again changes nothing.
func TestBuildCommandProcessResultTailIsRedactionStable(t *testing.T) {
	t.Parallel()
	// A word character glued to the front hides each token from its rule
	// until the cut removes it.
	github := "gh" + "p_" + strings.Repeat("G", 24)
	aws := "AK" + "IA" + strings.Repeat("W", 16)
	for name, tc := range map[string]struct {
		stdout string
		cap    int
		gone   string
	}{
		"cut exposes a GitHub token": {stdout: "x" + github, cap: len(github), gone: github},
		"cut exposes an AWS key":     {stdout: strings.Repeat("y", 50) + "x" + aws, cap: len(aws), gone: aws},
		"plain tail":                 {stdout: strings.Repeat("z", 40), cap: 8},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			out := buildCommandProcessResult(scruntime.CouncilResult{Stdout: tc.stdout}, commandRecord{Name: "check", MaxOutputBytes: tc.cap})
			got := out.Command.Output
			if len(got) > tc.cap || redact.String(got) != got || (tc.gone != "" && strings.Contains(got, tc.gone)) || !out.Command.OutputTruncated {
				t.Fatalf("output = %q (cap %d, truncated %v), want a redaction-stable tail without %q", got, tc.cap, out.Command.OutputTruncated, tc.gone)
			}
		})
	}
}

// TestCommandArtifactInvalidUTF8StaysWithinCap checks that invalid UTF-8 is
// made valid before the cap: JSON encoding would otherwise turn each invalid
// byte into U+FFFD (three bytes) and the stored output would pass the cap.
func TestCommandArtifactInvalidUTF8StaysWithinCap(t *testing.T) {
	t.Parallel()
	bad := strings.Repeat("\xff", 64)
	for name, tc := range map[string]struct {
		stdout    string
		truncated bool
	}{
		"only invalid bytes":         {stdout: bad},
		"invalid bytes between text": {stdout: "a" + bad + "b" + bad + "c"},
		"invalid bytes over the cap": {stdout: strings.Repeat("z", 70) + bad, truncated: true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			rec := commandRecord{Name: "check", MaxOutputBytes: 64}
			out := buildCommandProcessResult(scruntime.CouncilResult{Stdout: tc.stdout}, rec)
			art, ok := commandArtifact(&worktreeState{Command: &rec, Result: out})
			var res scruntime.CommandResult
			if !ok || json.Unmarshal([]byte(art.Text), &res) != nil {
				t.Fatalf("artifact = %+v, %v", art, ok)
			}
			if len(res.Output) > rec.MaxOutputBytes || !utf8.ValidString(out.Command.Output) || res.Output != out.Command.Output || res.OutputTruncated != tc.truncated {
				t.Fatalf("stored output %q (%d bytes, truncated %v), want valid UTF-8 within %d bytes, truncated %v", res.Output, len(res.Output), res.OutputTruncated, rec.MaxOutputBytes, tc.truncated)
			}
		})
	}
}
