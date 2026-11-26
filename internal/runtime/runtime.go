package runtime

import (
	"context"
	"fmt"
	"time"

	"github.com/shadewright-labs/sablecourt/internal/domain"
)

type LaunchSpec struct {
	TaskID           string
	Workload         domain.Workload
	WorkDir          string
	InputJSON        string
	ModelGateURL     string
	DataGateURL      string
	TaskToken        string
	ModelGateToken   string
	CouncilContainer bool
	CouncilDeadline  time.Time
	CreateAttempt    string
}

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
}

type CouncilCheckoutKind string

const (
	CouncilLive   CouncilCheckoutKind = "live"
	CouncilRef    CouncilCheckoutKind = "ref"
	CouncilReview CouncilCheckoutKind = "review"
	CouncilWriter CouncilCheckoutKind = "assigned-writer"
)

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
	OnCreated         func(context.Context, CouncilIdentity) error
	OnStarted         func(context.Context, CouncilIdentity) error
}

// CouncilResult identifies the exact container for evidence and cleanup retries.
type CouncilResult struct {
	ContainerName, CreateAttempt string
	ContainerID                  string
	FinishedAt                   time.Time
	ExitCode                     int
	Stdout, Stderr               string
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
