package docker

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/shadewright-labs/sablecourt/internal/domain"
	rt "github.com/shadewright-labs/sablecourt/internal/runtime"
)

// TestHarnessContainerSpecRefusesBadCommand checks that the one container
// spec function refuses a command launch it cannot build safely, before any
// engine call creates a container.
func TestHarnessContainerSpecRefusesBadCommand(t *testing.T) {
	long := strings.Repeat("a", domain.MaxCommandArgBytes+1)
	many := make([]string, domain.MaxCommandArgs+1)
	for i := range many {
		many[i] = "make"
	}
	// The same launch with a good argv builds, so each refusal below comes from its one change.
	a, f, s := fixtureRun(t)
	s.Prompt, s.Model, s.Command = "", "", []string{"make", "check"}
	if _, err := a.harnessContainerSpec(t.Context(), f, s, false); err != nil {
		t.Fatalf("good command launch refused: %v", err)
	}
	for name, tc := range map[string]struct {
		mutate func(*rt.CouncilLaunch)
		login  bool
	}{
		"empty argv":        {mutate: func(s *rt.CouncilLaunch) { s.Command = []string{} }},
		"empty program":     {mutate: func(s *rt.CouncilLaunch) { s.Command = []string{" "} }},
		"option as program": {mutate: func(s *rt.CouncilLaunch) { s.Command = []string{"-c", "id"} }},
		"NUL in argument":   {mutate: func(s *rt.CouncilLaunch) { s.Command = []string{"make", "a\x00b"} }},
		"invalid UTF-8":     {mutate: func(s *rt.CouncilLaunch) { s.Command = []string{"make", "\xff"} }},
		"argument too long": {mutate: func(s *rt.CouncilLaunch) { s.Command = []string{"make", long} }},
		"too many args":     {mutate: func(s *rt.CouncilLaunch) { s.Command = many }},
		"with a prompt":     {mutate: func(s *rt.CouncilLaunch) { s.Prompt = "run this" }},
		"with a model":      {mutate: func(s *rt.CouncilLaunch) { s.Model = "model" }},
		"as a login":        {login: true},
		"no wall time":      {mutate: func(s *rt.CouncilLaunch) { s.Limits.WallTime = 0 }},
		"unknown harness":   {mutate: func(s *rt.CouncilLaunch) { s.Harness = "other" }},
		"writable checkout": {mutate: func(s *rt.CouncilLaunch) { s.Checkout.Kind = rt.CouncilTask }},
		"live checkout":     {mutate: func(s *rt.CouncilLaunch) { s.Checkout.Kind = rt.CouncilLive; s.Checkout.GitDir = "" }},
	} {
		t.Run(name, func(t *testing.T) {
			a, f, s := fixtureRun(t)
			s.Prompt, s.Model, s.Command = "", "", []string{"make", "check"}
			if tc.mutate != nil {
				tc.mutate(&s)
			}
			if _, err := a.harnessContainerSpec(t.Context(), f, s, tc.login); err == nil {
				t.Fatal("bad command launch accepted")
			}
			if len(f.calls) != 0 {
				t.Fatalf("engine calls %v before the refusal", f.calls)
			}
		})
	}
}

// TestRunCommandSendsNothingOnStdin checks that a command container reads
// EOF at once: neither the task lease nor a prompt reaches it, and its
// output comes back redacted with the exit code.
func TestRunCommandSendsNothingOnStdin(t *testing.T) {
	a, f, s := fixtureRun(t)
	s.Prompt, s.Model, s.Command = "", "", []string{"make", "check"}
	secret := "ghp_" + strings.Repeat("R", 36)
	f.output = append(frame(1, "tests ran "+secret), frame(2, "1 failure")...)
	var got rt.CouncilResult
	_, err := a.RunCouncil(t.Context(), s, func(_ context.Context, r rt.CouncilResult) error {
		got = r
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if f.channel.body.Len() != 0 || !f.channel.eof {
		t.Fatalf("stdin got %q (eof %v), want nothing and EOF", f.channel.body.String(), f.channel.eof)
	}
	if got.ExitCode != 7 || strings.Contains(got.Stdout, secret) || !strings.HasPrefix(got.Stdout, "tests ran ") || got.Stderr != "1 failure" || got.OutputOverflow {
		t.Fatalf("result = %+v", got)
	}
	if !slices.Equal(f.createdOpts.Config.Cmd, s.Command) {
		t.Fatalf("container command = %v", f.createdOpts.Config.Cmd)
	}
	for _, m := range f.createdOpts.HostConfig.Mounts {
		if m.Source == f.vol.Name {
			t.Fatal("a command container mounted the harness login volume")
		}
	}
}

// TestRunCommandOutputOverflow checks that a stream past the runner's limit
// is reported as an overflow, so the step fails instead of storing it.
func TestRunCommandOutputOverflow(t *testing.T) {
	a, f, s := fixtureRun(t)
	s.Prompt, s.Model, s.Command = "", "", []string{"make", "check"}
	f.output = frame(1, strings.Repeat("y", maxCouncilOutput+1))
	var got rt.CouncilResult
	if _, err := a.RunCouncil(t.Context(), s, func(_ context.Context, r rt.CouncilResult) error { got = r; return nil }); err != nil {
		t.Fatal(err)
	}
	if !got.OutputOverflow || len(got.Stdout) > len(outputLimitMarker) {
		t.Fatalf("overflow = %v, stdout %d bytes", got.OutputOverflow, len(got.Stdout))
	}
}
