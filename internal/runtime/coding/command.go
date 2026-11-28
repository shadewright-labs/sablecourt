package coding

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/shadewright-labs/sablecourt/internal/domain"
	"github.com/shadewright-labs/sablecourt/internal/redact"
	scruntime "github.com/shadewright-labs/sablecourt/internal/runtime"
)

// commandRecord is kept in the task state so that every path that records a
// result (the runner, a timeout, a restart, a stop) knows the task ran a
// command and how much of its output may be stored.
type commandRecord struct {
	Name           string
	MaxOutputBytes int
}

// validateCommandLaunch is the adapter's check of a command step (issue 134).
// The command runs only in a harness container, on a read-only checkout
// pinned to a commit, never as a host process: without a container runner
// there is no fallback, whatever host mode says.
func (a *Adapter) validateCommandLaunch(spec scruntime.LaunchSpec, input launchInput, commit string, readOnly bool) error {
	c := spec.Command
	if !spec.CouncilContainer || !a.SupportsCouncilContainers() {
		return &scruntime.CommandRefusal{Command: c.Name, Reason: scruntime.CommandNoContainerRuntime}
	}
	if commit == "" || !readOnly || input.checkoutMode != "" || !spec.CouncilDeadline.IsZero() ||
		domain.ValidCommandArgv(c.Argv) != nil ||
		c.Timeout <= 0 || c.Timeout > domain.MaxCommandTimeout ||
		c.MaxOutputBytes <= 0 || c.MaxOutputBytes > domain.MaxCommandOutputBytes {
		return &scruntime.CommandRefusal{Command: c.Name, Reason: scruntime.CommandNotAllowed}
	}
	return nil
}

// buildCommandProcessResult turns a finished command container into a task
// result. The runner already redacted each stream; the joined output is made
// valid UTF-8 (each run of invalid bytes becomes one U+FFFD, so JSON encoding
// cannot grow it later), then redacted and capped by redact.Tail, the same
// function the runner uses. Output over the cap, or a stream over the
// runner's limit, fails the step even when the command exited 0, and only
// the last whole lines within the cap are kept.
func buildCommandProcessResult(res scruntime.CouncilResult, rec commandRecord) *processResult {
	var parts []string
	for _, s := range []string{res.Stdout, res.Stderr} {
		if s = strings.TrimSpace(s); s != "" {
			parts = append(parts, s)
		}
	}
	output, cut := redact.Tail(strings.ToValidUTF8(strings.Join(parts, "\n"), "\uFFFD"), 0, rec.MaxOutputBytes, nil)
	result := &scruntime.CommandResult{Command: rec.Name, ExitCode: res.ExitCode, Output: output}
	result.OutputTruncated = res.OutputOverflow || cut
	out := &processResult{ExitCode: res.ExitCode, Command: result}
	switch {
	case result.OutputTruncated:
		result.Outcome = scruntime.CommandOutputOverCap
		out.Message = fmt.Sprintf("command %q output exceeded its cap of %d bytes", rec.Name, rec.MaxOutputBytes)
		if out.ExitCode == 0 {
			out.ExitCode = -1
		}
	case res.ExitCode != 0:
		result.Outcome = scruntime.CommandFailed
		out.Message = fmt.Sprintf("command %q exited with status %d", rec.Name, res.ExitCode)
	default:
		result.Outcome = scruntime.CommandPassed
	}
	return out
}

// commandArtifact is the durable record of a command step. A result recorded
// without the runner's output (a timeout, a stop, or a daemon restart while
// the command ran) is a failure with no output.
func commandArtifact(state *worktreeState) (scruntime.Artifact, bool) {
	if state.Command == nil || state.Result == nil {
		return scruntime.Artifact{}, false
	}
	result := state.Result.Command
	if result == nil {
		outcome := scruntime.CommandFailed
		if state.Result.TimedOut {
			outcome = scruntime.CommandTimedOut
		}
		result = &scruntime.CommandResult{Command: state.Command.Name, Outcome: outcome, ExitCode: state.Result.ExitCode}
	}
	raw, err := json.Marshal(result)
	if err != nil {
		return scruntime.Artifact{}, false
	}
	return scruntime.Artifact{Name: scruntime.CommandResultArtifact, ContentType: "application/json", Text: string(raw)}, true
}
