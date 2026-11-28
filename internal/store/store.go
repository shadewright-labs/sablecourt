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
	ErrExpired  = errors.New("expired")
)

type Store interface {
	Close() error
	Migrate(ctx context.Context) error

	UpsertWorkload(ctx context.Context, w domain.WorkloadRecord) error
	ListWorkloads(ctx context.Context) ([]domain.WorkloadRecord, error)

	CreateTask(ctx context.Context, t domain.Task) error
	GetTask(ctx context.Context, id string) (domain.Task, error)
	GetTaskByIdempotencyKey(ctx context.Context, key string) (domain.Task, error)
	ListTasks(ctx context.Context) ([]domain.Task, error)
	ClaimRecovery(ctx context.Context, taskID, previous, owner string) (bool, error)
	ListReconcileTasks(ctx context.Context, handleIDs []string) ([]domain.Task, error)
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
	// ConsumeApproval spends an approval: one UPDATE moves the row from
	// approved to consumed only where it belongs to taskID and capability, pins
	// payloadSHA256 and expires later than the database's own clock, read
	// inside that UPDATE once the lock on the row is held, so after any wait
	// for it. Exactly one affected row means the caller may act. A
	// refusal changes nothing: a matching approved row (which can only have
	// failed the expiry) returns ErrExpired, a missing row ErrNotFound, and any
	// other mismatch, or a row that is not approved, ErrConflict. A consumed row
	// never becomes usable again.
	ConsumeApproval(ctx context.Context, id, taskID, capability, payloadSHA256 string) error

	CreateLease(ctx context.Context, lease domain.Lease) error
	GetActiveLeaseByTokenHash(ctx context.Context, tokenSHA256 string, now time.Time) (domain.Lease, error)
	RenewTaskLeases(ctx context.Context, taskID string, now, expiresAt time.Time) error
	RevokeTaskLeases(ctx context.Context, taskID string) error

	RecordBudgetUsage(ctx context.Context, entry domain.BudgetEntry) error
	ReserveBudget(ctx context.Context, req BudgetReservationRequest) (BudgetReservationResult, error)
	SettleBudgetReservation(ctx context.Context, reservationID string, actual *domain.BudgetEntry) error
	TaskBudgetTotals(ctx context.Context, taskID string) (domain.BudgetTotals, error)
	WorkloadDailyBudgetTotals(ctx context.Context, workloadName string, dayStartUTC time.Time) (domain.BudgetTotals, error)
	TotalBudgetTotals(ctx context.Context) (domain.BudgetTotals, error)

	AppendAudit(ctx context.Context, rec domain.AuditRecord) error
	ListAudit(ctx context.Context, limit int) ([]domain.AuditRecord, error)
	// VerifyAuditChain walks audit_log in ID order through domain.AuditChainVerifier
	// and returns the verified head; a non-nil anchor must match the chain.
	VerifyAuditChain(ctx context.Context, anchor *domain.AuditAnchor) (domain.AuditChainHead, error)

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
	FinishConclaveWake(ctx context.Context, messageID int64) error
	PutConclaveRelic(ctx context.Context, relic domain.ConclaveRelic) error
	GetConclaveRelic(ctx context.Context, conclaveID, name string) (domain.ConclaveRelic, error)
	ListConclaveRelics(ctx context.Context, conclaveID string) ([]domain.ConclaveRelic, error)

	CreateDevice(ctx context.Context, d domain.PairedDevice) error
	GetDevice(ctx context.Context, id string) (domain.PairedDevice, error)
	ListDevices(ctx context.Context) ([]domain.PairedDevice, error)
	RevokeDevice(ctx context.Context, id string) error

	PutMemory(ctx context.Context, entry domain.MemoryEntry) error
	GetMemory(ctx context.Context, namespace, key string) (domain.MemoryEntry, error)
	SearchMemory(ctx context.Context, namespace, query string, limit int) ([]domain.MemoryEntry, error)
	DeleteMemory(ctx context.Context, namespace, key string) error
}

// ConsumeRefusal names why ConsumeApproval updated no row, from the row as
// read afterwards. It only reads: a refusal never changes the approval.
func ConsumeRefusal(a domain.Approval, taskID, capability, payloadSHA256 string) error {
	if a.Status == domain.ApprovalApproved && a.TaskID == taskID && a.Capability == capability && a.PayloadSHA256 == payloadSHA256 {
		return ErrExpired
	}
	return ErrConflict
}
