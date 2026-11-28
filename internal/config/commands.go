package config

import (
	"errors"
	"fmt"
	"regexp"
	"unicode/utf8"

	"github.com/shadewright-labs/sablecourt/internal/domain"
)

var commandName = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

// validateCommands checks a coding workload's allowed command set. The argv
// is operator configuration: it runs as given, with no shell unless the
// operator names one, in the harness container of the workload.
func validateCommands(cmds []domain.CodingCommand) error {
	seen := make(map[string]bool, len(cmds))
	for i, c := range cmds {
		if !commandName.MatchString(c.Name) {
			return fmt.Errorf("coding.commands[%d]: name must match %s", i, commandName)
		}
		if seen[c.Name] {
			return fmt.Errorf("coding.commands[%d]: duplicate command %q", i, c.Name)
		}
		seen[c.Name] = true
		if err := domain.ValidCommandArgv(c.Argv); err != nil {
			return fmt.Errorf("coding.commands[%d] %q: %w", i, c.Name, err)
		}
		if c.TimeoutSeconds < 0 || c.TimeoutSeconds > int(domain.MaxCommandTimeout.Seconds()) {
			return fmt.Errorf("coding.commands[%d] %q: timeoutSeconds must be between 1 and %d", i, c.Name, int(domain.MaxCommandTimeout.Seconds()))
		}
		if c.MaxOutputBytes < 0 || c.MaxOutputBytes > domain.MaxCommandOutputBytes {
			return fmt.Errorf("coding.commands[%d] %q: maxOutputBytes must be between 1 and %d", i, c.Name, domain.MaxCommandOutputBytes)
		}
		if len(c.FailureReason) > domain.MaxCommandFailureReason || !utf8.ValidString(c.FailureReason) || hasControl(c.FailureReason) {
			return fmt.Errorf("coding.commands[%d] %q: failureReason must be at most %d bytes of text on one line", i, c.Name, domain.MaxCommandFailureReason)
		}
	}
	return nil
}

func hasControl(s string) bool {
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return true
		}
	}
	return false
}

// validateCommandStep checks a workflow step that runs a command. It needs an
// earlier author or command step whose commit it checks, a coding workload
// that allows the command, and none of the fields that only a harness step uses.
func validateCommandStep(steps []domain.WorkflowStep, index int, wl domain.Workload) error {
	step := steps[index]
	if index == 0 {
		return errors.New("a command step cannot be the first step: it runs on the commit of the step before it")
	}
	if steps[index-1].Role == "verifier" {
		return errors.New("a command step must follow an author or command step, not a verifier")
	}
	if wl.Spec.Class != domain.ClassCodingAgent || wl.Spec.Coding == nil {
		return fmt.Errorf("command steps need a %s workload", domain.ClassCodingAgent)
	}
	if _, ok := wl.Spec.Coding.Command(step.Command); !ok {
		return fmt.Errorf("command %q is not in the coding.commands of workload %q", step.Command, wl.Metadata.Name)
	}
	if step.Role != "" || step.Tier != "" || step.Provider != "" || step.Model != "" || step.Prompt != "" || step.MaxRetries != 0 {
		return errors.New("a command step cannot set role, tier, provider, model, prompt or maxRetries")
	}
	return nil
}
