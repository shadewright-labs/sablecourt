package store

import (
	"context"
	"errors"
	"time"

	"github.com/shadewright-labs/sablecourt/internal/domain"
)

var (
	ErrNotFound = errors.New("not found")
	ErrConflict = errors.New("conflict")
)

type Store interface {
	Close() error
	Migrate(ctx context.Context) error

	UpsertWorkload(ctx context.Context, w domain.WorkloadRecord) error
	ListWorkloads(ctx context.Context) ([]domain.WorkloadRecord, error)

	CreateTask(ctx context.Context, t domain.Task) error
	GetTask(ctx context.Context, id string) (domain.Task, error)
	ListTasks(ctx context.Context) ([]domain.Task, error)
	ListTaskEvents(ctx context.Context, taskID string) ([]domain.TaskEvent, error)
	AppendTaskMessage(ctx context.Context, msg domain.TaskMessage) error
	ListTaskMessages(ctx context.Context, taskID string) ([]domain.TaskMessage, error)
	UpdateTaskState(ctx context.Context, taskID string, from, to domain.TaskState, reason, factJSON string) error
	UpdateTaskArtifacts(ctx context.Context, taskID, artifactsJSON string) error
	RecordHeartbeat(ctx context.Context, taskID string, at time.Time) error

	CreateApproval(ctx context.Context, a domain.Approval) error
	GetApproval(ctx context.Context, id string) (domain.Approval, error)
	ListApprovals(ctx context.Context, status domain.ApprovalStatus) ([]domain.Approval, error)
	DecideApproval(ctx context.Context, id, expectedSHA256 string, status domain.ApprovalStatus, decidedBy, proofJSON string) error

	CreateLease(ctx context.Context, lease domain.Lease) error
	GetActiveLeaseByTokenHash(ctx context.Context, tokenSHA256 string, now time.Time) (domain.Lease, error)
	RevokeTaskLeases(ctx context.Context, taskID string) error

	RecordBudgetUsage(ctx context.Context, entry domain.BudgetEntry) error
	TaskBudgetTotals(ctx context.Context, taskID string) (domain.BudgetTotals, error)
	WorkloadDailyBudgetTotals(ctx context.Context, workloadName string, dayStartUTC time.Time) (domain.BudgetTotals, error)
	TotalBudgetTotals(ctx context.Context) (domain.BudgetTotals, error)

	AppendAudit(ctx context.Context, rec domain.AuditRecord) error
	ListAudit(ctx context.Context, limit int) ([]domain.AuditRecord, error)
	VerifyAuditChain(ctx context.Context) (int, error)

	SaveCheckpoint(ctx context.Context, taskID, workloadName, refJSON string) error
	LatestCheckpoint(ctx context.Context, taskID string) (string, error)

	UpsertProviderPrefs(ctx context.Context, prefs domain.ProviderPrefs) error
	ListProviderPrefs(ctx context.Context) (map[string]domain.ProviderPrefs, error)

	CreateConclave(ctx context.Context, c domain.ConclaveSession) error
	GetConclave(ctx context.Context, id string) (domain.ConclaveSession, error)
	ListConclaves(ctx context.Context) ([]domain.ConclaveSession, error)
	UpdateConclaveState(ctx context.Context, id string, state domain.ConclaveState, wardReason string) error
	AppendConclaveMessage(ctx context.Context, msg domain.ConclaveMessage) (int64, error)
	ListConclaveMessages(ctx context.Context, conclaveID string, sinceID int64) ([]domain.ConclaveMessage, error)
	PutConclaveRelic(ctx context.Context, relic domain.ConclaveRelic) error
	GetConclaveRelic(ctx context.Context, conclaveID, name string) (domain.ConclaveRelic, error)
	ListConclaveRelics(ctx context.Context, conclaveID string) ([]domain.ConclaveRelic, error)

	CreateDevice(ctx context.Context, d domain.PairedDevice) error
	GetDevice(ctx context.Context, id string) (domain.PairedDevice, error)
	ListDevices(ctx context.Context) ([]domain.PairedDevice, error)
	RevokeDevice(ctx context.Context, id string) error
}
