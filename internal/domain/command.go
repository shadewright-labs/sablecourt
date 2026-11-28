package domain

import (
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

// CodingCommand is one command a workflow command step may run, inside a
// harness container, against the checkout of the step before it.
type CodingCommand struct {
	Name           string   `json:"name"`
	Argv           []string `json:"argv"`
	TimeoutSeconds int      `json:"timeoutSeconds,omitempty"`
	MaxOutputBytes int      `json:"maxOutputBytes,omitempty"`
	// FailureReason is recorded on the workflow run when the command fails.
	FailureReason string `json:"failureReason,omitempty"`
}

// Limits of a command step. The output ceiling matches the largest output a
// harness container run keeps (internal/runtime/docker maxCouncilOutput).
const (
	DefaultCommandTimeout     = 10 * time.Minute
	MaxCommandTimeout         = time.Hour
	DefaultCommandOutputBytes = 64 << 10
	MaxCommandOutputBytes     = 1 << 20
	MaxCommandArgs            = 64
	MaxCommandArgBytes        = 4096
	MaxCommandFailureReason   = 512
)

// Command returns the allowed command with this name.
func (c *CodingConfig) Command(name string) (CodingCommand, bool) {
	if c == nil || name == "" {
		return CodingCommand{}, false
	}
	for _, cmd := range c.Commands {
		if cmd.Name == name {
			return cmd, true
		}
	}
	return CodingCommand{}, false
}

// Timeout returns the command's time limit, the default when unset.
func (c CodingCommand) Timeout() time.Duration {
	if c.TimeoutSeconds <= 0 {
		return DefaultCommandTimeout
	}
	return time.Duration(c.TimeoutSeconds) * time.Second
}

// OutputCap returns the most output bytes the command may produce.
func (c CodingCommand) OutputCap() int {
	if c.MaxOutputBytes <= 0 {
		return DefaultCommandOutputBytes
	}
	return c.MaxOutputBytes
}

// ValidCommandArgv is the argv check shared by the manifest loader and the
// harness container spec: 1 to MaxCommandArgs entries of valid UTF-8 without
// NUL, each at most MaxCommandArgBytes, and a program name that is not empty
// and is not an option.
func ValidCommandArgv(argv []string) error {
	if len(argv) == 0 || len(argv) > MaxCommandArgs {
		return fmt.Errorf("argv must have 1 to %d entries", MaxCommandArgs)
	}
	if strings.TrimSpace(argv[0]) == "" || strings.HasPrefix(argv[0], "-") {
		return errors.New("argv[0] must name a program")
	}
	for _, arg := range argv {
		if len(arg) > MaxCommandArgBytes || !utf8.ValidString(arg) || strings.ContainsRune(arg, 0) {
			return fmt.Errorf("each argv entry must be valid UTF-8 without NUL, at most %d bytes", MaxCommandArgBytes)
		}
	}
	return nil
}
