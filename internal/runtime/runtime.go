package runtime

import (
	"context"
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
}

type Handle struct {
	AdapterName      string
	ExternalID       string
	WorkDir          string
	Branch           string
	PID              int
	ProcessStartTime time.Time
	LogPath          string
}

type ObservedState string

const (
	StateRunning   ObservedState = "running"
	StateSuspended ObservedState = "suspended"
	StateCompleted ObservedState = "completed"
	StateFailed    ObservedState = "failed"
	StateUnknown   ObservedState = "unknown"
)

type Observation struct {
	State      ObservedState
	ExitCode   int
	ObservedAt time.Time
	Message    string
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
