package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/shadewright-labs/sablecourt/internal/domain"
)

type LaunchSpec struct {
	TaskID         string
	Workload       domain.Workload
	WorkDir        string
	InputJSON      string
	ModelGateURL   string
	DataGateURL    string
	TaskToken      string
	ModelGateToken string
	// CouncilContainer asks the coding adapter for a harness container. The
	// reconciler sets it for every coding task (standalone or council seat)
	// when the adapter has a container runner; the name predates standalone use.
	CouncilContainer bool
	CouncilDeadline  time.Time // Set only for council seats.
	CreateAttempt    string
	// Command, when set, runs this operator-declared command in a harness
	// container instead of the workload's harness. The control plane resolves
	// it from the manifest; task input never supplies it.
	Command *CommandSpec
}

// CommandSpec is one entry of a coding workload's allowed command set.
type CommandSpec struct {
	Name           string
	Argv           []string
	Timeout        time.Duration
	MaxOutputBytes int
	// AuthorBase is the commit the run's first author step started from, as
	// the daemon recorded it (issue 214). The adapter checks that it is an
	// ancestor of the pinned commit and names it in the container's Git
	// metadata as AuthorBaseRef.
	AuthorBase string
}

// AuthorBaseRef is the ref a command container's Git metadata holds for the
// run's first author base.
const AuthorBaseRef = "refs/sablecourt/author-base"

// GitBaseArtifact names the artifact in which a coding task reports the
// commit its checkout started from.
const GitBaseArtifact = "git-base"

// Reasons a command step is refused before its command runs.
const (
	CommandNotAllowed         = "command_not_allowed"
	CommandNoContainerRuntime = "no_container_runtime"
	CommandContainerRefused   = "container_spec_refused"
	// CommandAuthorBaseUnavailable: the run has no recorded author base, or
	// it is not an ancestor of the commit the step checks.
	CommandAuthorBaseUnavailable = "author_base_unavailable"
)

// Outcomes of a command that ran.
const (
	CommandPassed        = "passed"
	CommandFailed        = "failed"
	CommandTimedOut      = "timeout"
	CommandOutputOverCap = "output_over_cap"
)

// CommandRefusal is the error of a command step refused before its command
// ran. Its text names only the command and a fixed reason.
type CommandRefusal struct {
	Command string
	Reason  string
}

func (e *CommandRefusal) Error() string {
	return fmt.Sprintf("command %q refused: %s", e.Command, e.Reason)
}

// CommandResult is the durable record of a command step, stored redacted and
// capped as the task's command-result artifact.
type CommandResult struct {
	Command         string `json:"command"`
	Outcome         string `json:"outcome"`
	ExitCode        int    `json:"exit_code"`
	Output          string `json:"output"`
	OutputTruncated bool   `json:"output_truncated,omitempty"`
}

// CommandResultArtifact names the artifact that carries a CommandResult.
const CommandResultArtifact = "command-result"

type Handle struct {
	AdapterName      string
	ExternalID       string
	WorkDir          string
	Branch           string
	PID              int
	ProcessStartTime time.Time
	LogPath          string
	ContainerName    string
	CreateAttempt    string
	ContainerID      string
	ProxyName        string
	NetworkName      string
}

type ObservedState string

const (
	StateRunning   ObservedState = "running"
	StateSuspended ObservedState = "suspended"
	StateCompleted ObservedState = "completed"
	StateFailed    ObservedState = "failed"
	StateUnknown   ObservedState = "unknown"
)

// ExitCodeLogClose is recorded by a runtime wrapper when a harness exited 0
// (so its side effects already occurred) but closing or finalizing its output
// log failed. Normal POSIX exits are 0 to 255 and signalled, timed-out,
// stopped, or output-capture failures use -1, so a harness cannot return -2.
const ExitCodeLogClose = -2

type Observation struct {
	State          ObservedState
	ExitCode       int
	ObservedAt     time.Time
	Message        string
	LogCloseFailed bool
}

type CheckpointRef struct {
	ID        string    `json:"id"`
	Path      string    `json:"path"`
	CreatedAt time.Time `json:"createdAt"`
}

type Artifact struct {
	Name        string `json:"name"`
	ContentType string `json:"contentType"`
	URI         string `json:"uri,omitempty"`
	Text        string `json:"text,omitempty"`
}

type Adapter interface {
	Start(ctx context.Context, spec LaunchSpec) (Handle, error)
	Observe(ctx context.Context, handle Handle) (Observation, error)
	Suspend(ctx context.Context, handle Handle) (CheckpointRef, error)
	Resume(ctx context.Context, handle Handle, cp CheckpointRef) error
	Stop(ctx context.Context, handle Handle) error
	CollectArtifacts(ctx context.Context, handle Handle) ([]Artifact, error)
}

// ErrCodingRuntimeUnavailable means no coding task can start: no container
// runtime is configured and host mode is off. Refusals wrap it so the
// reconciler can record why a task failed.
var ErrCodingRuntimeUnavailable = errors.New("coding runtime unavailable")

// MissingBinaryError reports that a workload executable could not be found at Path.
type MissingBinaryError struct {
	Path string
	Err  error
}

func (e *MissingBinaryError) Error() string {
	if e.Err != nil {
		return e.Err.Error()
	}
	return fmt.Sprintf("executable %q not found", e.Path)
}

func (e *MissingBinaryError) Unwrap() error { return e.Err }

// CouncilCheckout refers to the directory already selected and verified by the coding adapter.
type CouncilCheckout struct {
	Path string
	Kind CouncilCheckoutKind
	// GitDir, when set, is a self-contained Git metadata directory built for
	// this launch. A linked worktree's .git file points at its registration
	// in the shared repository, which a harness container never mounts, so
	// the container gets this directory as its Git metadata instead.
	GitDir string
}

type CouncilCheckoutKind string

const (
	CouncilLive   CouncilCheckoutKind = "live"
	CouncilRef    CouncilCheckoutKind = "ref"
	CouncilReview CouncilCheckoutKind = "review"
	CouncilWriter CouncilCheckoutKind = "assigned-writer"
	// CouncilTask is a standalone coding task's own writable worktree. It runs
	// in the same harness container, with the same guards, as a council seat.
	CouncilTask CouncilCheckoutKind = "task"
)

// Writable reports whether the harness may write to the checkout.
func (k CouncilCheckoutKind) Writable() bool {
	return k == CouncilWriter || k == CouncilTask
}

type CouncilLimits struct {
	NanoCPUs    int64
	MemoryBytes int64
	PIDs        int64
	WallTime    time.Duration
}

// CouncilIdentity binds a council container to its durable name and create attempt.
type CouncilIdentity struct {
	ContainerName string
	CreateAttempt string
	ContainerID   string
}

type CouncilLaunch struct {
	TaskID, Workload  string
	Harness, Model    string
	Checkout          CouncilCheckout
	Limits            CouncilLimits
	Prompt, TaskToken string
	Network           string
	NetworkID         string
	ProxyURL          string
	DataGateURL       string
	CreateAttempt     string
	// Command replaces the harness CLI with this argv. Such a container gets
	// no login volume, no tool volume, no task lease and no prompt.
	Command   []string
	OnCreated func(context.Context, CouncilIdentity) error
	OnStarted func(context.Context, CouncilIdentity) error
}

// CouncilResult identifies the exact container for evidence and cleanup retries.
type CouncilResult struct {
	ContainerName, CreateAttempt string
	ContainerID                  string
	FinishedAt                   time.Time
	ExitCode                     int
	Stdout, Stderr               string
	// OutputOverflow is set when a stream was cut: it passed the runner's
	// output limit, or its redaction grew past it. A council stream is then replaced by a marker; a command stream keeps
	// its redacted tail, starting on a whole line.
	OutputOverflow bool
}

// CouncilInspection reports verified Docker container state for recovery and polling.
type CouncilInspection struct {
	Exists      bool
	Running     bool
	Exited      bool
	ExitCode    int
	FinishedAt  time.Time
	ContainerID string
}

type CouncilRecorder func(context.Context, CouncilResult) error

// CouncilRunner runs to completion inside the coding adapter's process lifecycle.
// The recorder persists the result before the container is removed.
type CouncilRunner interface {
	RunCouncil(context.Context, CouncilLaunch, CouncilRecorder) (CouncilResult, error)
}

// CouncilInspector verifies and removes council containers across daemon restarts.
type CouncilInspector interface {
	InspectCouncil(context.Context, CouncilIdentity) (CouncilInspection, error)
	CleanupCouncil(context.Context, CouncilResult) error
}

// LocalImageResolver resolves a local image reference to its immutable sha256 ID without pulling.
type LocalImageResolver interface {
	ResolveLocalImage(context.Context, string) (string, error)
}

// ErrInputNotDelivered: the runtime could not hand the task input to the
// workload (stdin attach or write failed). The task fails with reason
// input_not_delivered instead of running without its input.
var ErrInputNotDelivered = errors.New("task input not delivered")

// InputStdinEnv tells a Docker or Kubernetes workload that its task input is
// the first line of its stdin. The variable holds only "1", never the input:
// environment values show in docker inspect, pod specs and process listings.
const InputStdinEnv = "SABLECOURT_INPUT_STDIN"

// StdinInputLine frames task input for delivery on a workload's stdin: one
// line of JSON ending in a newline. JSON input is compacted, so its strings
// keep their escaped line breaks and the frame stays one line. Input that is
// not JSON is sent as a JSON string. Empty input yields an empty frame.
func StdinInputLine(input string) (string, error) {
	if input == "" {
		return "", nil
	}
	var buf bytes.Buffer
	if json.Valid([]byte(input)) {
		if err := json.Compact(&buf, []byte(input)); err != nil {
			return "", fmt.Errorf("frame task input: %w", ErrInputNotDelivered)
		}
	} else {
		raw, err := json.Marshal(input)
		if err != nil {
			return "", fmt.Errorf("frame task input: %w", ErrInputNotDelivered)
		}
		buf.Write(raw)
	}
	if bytes.ContainsAny(buf.Bytes(), "\r\n") {
		return "", fmt.Errorf("frame task input: %w", ErrInputNotDelivered)
	}
	buf.WriteByte('\n')
	return buf.String(), nil
}
