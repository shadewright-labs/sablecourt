package coding

import (
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"

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
// result. The runner already redacted each stream; the joined output is
// redacted again, then capped. Output over the cap fails the step even when
// the command exited 0, and only the last cap bytes are kept.
func buildCommandProcessResult(res scruntime.CouncilResult, rec commandRecord) *processResult {
	var parts []string
	for _, s := range []string{res.Stdout, res.Stderr} {
		if s = strings.TrimSpace(s); s != "" {
			parts = append(parts, s)
		}
	}
	output := redact.String(strings.Join(parts, "\n"))
	result := &scruntime.CommandResult{Command: rec.Name, ExitCode: res.ExitCode, Output: output}
	switch {
	case res.OutputOverflow:
		result.Output, result.OutputTruncated = "", true
	case len(output) > rec.MaxOutputBytes:
		result.Output, result.OutputTruncated = redactedTail(output, rec.MaxOutputBytes), true
	}
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

// redactedTail returns at most maxBytes from the end of s that redaction
// leaves unchanged. A cut can leave a fragment that a rule matches, and its
// replacement can be longer than the fragment, so the tail is redacted and cut
// again until it is stable; a tail that does not settle is dropped. Redacting
// the stored output again therefore never changes it or grows it past the cap.
func redactedTail(s string, maxBytes int) string {
	tail := utf8Tail(s, maxBytes)
	for range 3 {
		r := redact.String(tail)
		if r == tail {
			return tail
		}
		tail = utf8Tail(r, maxBytes)
	}
	return ""
}

// utf8Tail returns at most maxBytes from the end of s, starting on a rune.
func utf8Tail(s string, maxBytes int) string {
	if len(s) <= maxBytes {
		return s
	}
	cut := len(s) - maxBytes
	for cut < len(s) && !utf8.RuneStart(s[cut]) {
		cut++
	}
	return s[cut:]
}
