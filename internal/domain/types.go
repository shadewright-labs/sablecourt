package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math"
	"time"
)

const APIVersionV1Alpha1 = "sablecourt.io/v1alpha1"

const (
	KindWorkload = "Workload"
	KindPolicy   = "Policy"
	KindBudget   = "Budget"
	KindWorkflow = "Workflow"
	KindSchedule = "Schedule"
	KindConclave = "Conclave"
)

type WorkloadZone string

const (
	ZoneRuntime WorkloadZone = "runtime"
	ZoneSandbox WorkloadZone = "sandbox"
)

type WorkloadClass string

const (
	ClassCodingAgent             WorkloadClass = "coding-agent"
	ClassLongLivedAgent          WorkloadClass = "long-lived-agent"
	ClassDeterministicController WorkloadClass = "deterministic-controller"
)

type PolicyEffect string

const (
	EffectAllow   PolicyEffect = "allow"
	EffectApprove PolicyEffect = "approve"
	EffectDeny    PolicyEffect = "deny"
)

type TaskState string

const (
	TaskStateSubmitted     TaskState = "submitted"
	TaskStateWorking       TaskState = "working"
	TaskStateInputRequired TaskState = "input-required"
	TaskStateSuspended     TaskState = "suspended"
	TaskStateCompleted     TaskState = "completed"
	TaskStateFailed        TaskState = "failed"
	TaskStateCancelled     TaskState = "cancelled"
)

type ApprovalStatus string

const (
	ApprovalPending  ApprovalStatus = "pending"
	ApprovalApproved ApprovalStatus = "approved"
	ApprovalDenied   ApprovalStatus = "denied"
	ApprovalExpired  ApprovalStatus = "expired"
	// ApprovalConsumed marks an approval that authorized its one execution.
	ApprovalConsumed ApprovalStatus = "consumed"
)

type TypeMeta struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
}

type ObjectMeta struct {
	Name string       `json:"name"`
	Zone WorkloadZone `json:"zone,omitempty"`
}

type Workload struct {
	TypeMeta `json:",inline"`
	Metadata ObjectMeta   `json:"metadata"`
	Spec     WorkloadSpec `json:"spec"`
}

type WorkloadSpec struct {
	Class     WorkloadClass `json:"class"`
	Runtime   RuntimeConfig `json:"runtime"`
	Coding    *CodingConfig `json:"coding,omitempty"`
	AgentCard AgentCard     `json:"agentCard"`
	RoleCard  string        `json:"roleCard,omitempty"`
	Grants    GrantSet      `json:"grants"`
}

type RuntimeConfig struct {
	Type           string `json:"type"`
	Image          string `json:"image,omitempty"`
	ReadOnlyRootfs bool   `json:"readOnlyRootfs,omitempty"`
}

type CodingConfig struct {
	Harness  string `json:"harness"`
	RepoPath string `json:"repoPath"`
	Sandbox  string `json:"sandbox"`
}

type AgentCard struct {
	Description string   `json:"description"`
	Skills      []string `json:"skills,omitempty"`
}

type GrantSet struct {
	ModelGate  ModelGateGrant      `json:"modelGate"`
	MCPTools   []string            `json:"mcpTools,omitempty"`
	MCPServers []UpstreamMCPServer `json:"mcpServers,omitempty"`
	Actions    []string            `json:"actions,omitempty"`
	Repos      []string            `json:"repos,omitempty"`
	DelegateTo []string            `json:"delegateTo,omitempty"`
}

// UpstreamMCPServer declares a remote MCP Streamable HTTP server proxied through
// Sablecourt's Data & Action Gate so credentials stay inside sablecourtd's SecretStore
// and every tool call passes through Policy Engine + Approval Queue + Redactor.
type UpstreamMCPServer struct {
	Name         string   `json:"name"`
	URL          string   `json:"url"`
	SecretName   string   `json:"secretName,omitempty"`
	AuthHeader   string   `json:"authHeader,omitempty"` // default: Authorization
	AuthPrefix   string   `json:"authPrefix,omitempty"` // default: "Bearer "
	AllowedTools []string `json:"allowedTools,omitempty"`
}

type ModelGateGrant struct {
	Allowed   bool     `json:"allowed"`
	Providers []string `json:"providers,omitempty"`
	BudgetRef string   `json:"budgetRef,omitempty"`
}

type Policy struct {
	TypeMeta `json:",inline"`
	Metadata ObjectMeta `json:"metadata"`
	Spec     PolicySpec `json:"spec"`
}

type PolicySpec struct {
	DefaultEffect PolicyEffect `json:"defaultEffect"`
	Rules         []PolicyRule `json:"rules"`
}

type PolicyRule struct {
	Workload   string       `json:"workload"`
	Capability string       `json:"capability"`
	Effect     PolicyEffect `json:"effect"`
}

type Budget struct {
	TypeMeta `json:",inline"`
	Metadata ObjectMeta `json:"metadata"`
	Spec     BudgetSpec `json:"spec"`
}

type BudgetSpec struct {
	PerTask PerTaskBudget `json:"perTask"`
	Daily   DailyBudget   `json:"daily"`
}

type PerTaskBudget struct {
	MaxInputTokens  int64   `json:"maxInputTokens,omitempty"`
	MaxOutputTokens int64   `json:"maxOutputTokens,omitempty"`
	MaxCostUSD      float64 `json:"maxCostUSD,omitempty"`
}

type DailyBudget struct {
	MaxCostUSD float64 `json:"maxCostUSD,omitempty"`
}

type Workflow struct {
	TypeMeta `json:",inline"`
	Metadata ObjectMeta   `json:"metadata"`
	Spec     WorkflowSpec `json:"spec"`
}

type WorkflowSpec struct {
	Description string         `json:"description,omitempty"`
	Steps       []WorkflowStep `json:"steps"`
}

type WorkflowStep struct {
	Name       string `json:"name"`
	Workload   string `json:"workload"`
	Role       string `json:"role,omitempty"`       // author | verifier | fast
	Tier       string `json:"tier,omitempty"`       // fast | coding | critic
	MaxRetries int    `json:"maxRetries,omitempty"` // opt-in closed-loop remediation retries
	Provider   string `json:"provider,omitempty"`
	Model      string `json:"model,omitempty"`
	Prompt     string `json:"prompt,omitempty"`
}

type Schedule struct {
	TypeMeta `json:",inline"`
	Metadata ObjectMeta   `json:"metadata"`
	Spec     ScheduleSpec `json:"spec"`
}

type ScheduleSpec struct {
	Cron       string `json:"cron"`
	TargetKind string `json:"targetKind"` // workload | workflow
	TargetName string `json:"targetName"`
	Input      string `json:"input,omitempty"`
	Enabled    bool   `json:"enabled"`
}

type WorkloadRecord struct {
	Name           string
	Zone           WorkloadZone
	Class          WorkloadClass
	ManifestDigest string
	AgentCardJSON  string
	UpdatedAt      time.Time
}

type Task struct {
	ID                string
	ParentTaskID      string
	IdempotencyKey    string
	WorkloadName      string
	State             TaskState
	InputJSON         string
	ArtifactsJSON     string
	BudgetCeilingJSON string
	LastHeartbeatAt   *time.Time
	CreatedAt         time.Time
	UpdatedAt         time.Time
}

type TaskEvent struct {
	ID        int64
	TaskID    string
	FromState TaskState
	ToState   TaskState
	Reason    string
	FactJSON  string
	CreatedAt time.Time
}

type TaskMessage struct {
	ID        int64
	TaskID    string
	Role      string // user | agent | system
	Content   string
	CreatedAt time.Time
}

type Lease struct {
	TokenSHA256  string
	TaskID       string
	WorkloadName string
	ExpiresAt    time.Time
	RevokedAt    *time.Time
	CreatedAt    time.Time
}

type BudgetEntry struct {
	ID               int64
	TaskID           string
	WorkloadName     string
	Provider         string
	Model            string
	InputTokens      int64
	CacheWriteTokens int64
	CacheReadTokens  int64
	OutputTokens     int64
	CostMicroUSD     int64
	CreatedAt        time.Time
}

type BudgetTotals struct {
	InputTokens      int64
	CacheWriteTokens int64
	CacheReadTokens  int64
	OutputTokens     int64
	CostMicroUSD     int64
}

// TotalInputTokens returns all input tokens counted against input budgets:
// uncached input plus cache writes and cache reads, saturating at math.MaxInt64
// on overflow.
func (t BudgetTotals) TotalInputTokens() int64 {
	sum := satAddInt64(max(t.InputTokens, 0), max(t.CacheWriteTokens, 0))
	return satAddInt64(sum, max(t.CacheReadTokens, 0))
}

func satAddInt64(a, b int64) int64 {
	if a > math.MaxInt64-b {
		return math.MaxInt64
	}
	return a + b
}

type Approval struct {
	ID                   string
	TaskID               string
	WorkloadName         string
	Capability           string
	PayloadCanonicalJSON string
	PayloadSHA256        string
	Status               ApprovalStatus
	DecidedBy            string
	ProofJSON            string
	ExpiresAt            time.Time
	CreatedAt            time.Time
	DecidedAt            *time.Time
}

type AuditRecord struct {
	ID           int64
	Category     string
	TaskID       string
	WorkloadName string
	Action       string
	Outcome      string
	DetailJSON   string
	PrevHash     string
	RowHash      string
	CreatedAt    time.Time
}

// ComputeAuditRowHash derives a deterministic tamper-evident SHA-256 chain hash for an audit row.
func ComputeAuditRowHash(prevHash string, rec AuditRecord, createdAtRFC3339 string) string {
	if prevHash == "" {
		prevHash = "genesis"
	}
	payload := fmt.Sprintf("%s|%s|%s|%s|%s|%s|%s|%s",
		prevHash, rec.Category, rec.TaskID, rec.WorkloadName, rec.Action, rec.Outcome, rec.DetailJSON, createdAtRFC3339)
	sum := sha256.Sum256([]byte(payload))
	return hex.EncodeToString(sum[:])
}

type ProviderPrefs struct {
	ProviderID    string
	DefaultModel  string
	EnabledModels []string
	UpdatedAt     time.Time
}

type ConclaveActivation string

const (
	ActivationAlwaysOn  ConclaveActivation = "always-on"
	ActivationOnMention ConclaveActivation = "on-mention"
)

type ConclaveState string

const (
	ConclaveStateActive        ConclaveState = "active"
	ConclaveStateInputRequired ConclaveState = "input-required"
	ConclaveStateCompleted     ConclaveState = "completed"
	ConclaveStateCancelled     ConclaveState = "cancelled"
)

type Conclave struct {
	TypeMeta `json:",inline"`
	Metadata ObjectMeta   `json:"metadata"`
	Spec     ConclaveSpec `json:"spec"`
}

type ConclaveSpec struct {
	Description string           `json:"description,omitempty"`
	BudgetRef   string           `json:"budgetRef,omitempty"`
	Wards       ConclaveWards    `json:"wards,omitempty"`
	Vassals     []ConclaveVassal `json:"vassals"`
}

const ConclaveRunCeiling = 6

type ConclaveWards struct {
	MaxRuns     int `json:"maxRuns,omitempty"`
	MaxMessages int `json:"maxMessages,omitempty"`
	MaxPingPong int `json:"maxPingPong,omitempty"`
}

type ConclaveVassal struct {
	Role       string             `json:"role"`
	Workload   string             `json:"workload"`
	Activation ConclaveActivation `json:"activation,omitempty"`
	CanMessage []string           `json:"canMessage,omitempty"`
}

type ConclaveSession struct {
	ID           string
	TemplateName string
	Title        string
	State        ConclaveState
	WardReason   string
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

type ConclaveMessage struct {
	ID         int64
	ConclaveID string
	FromRole   string
	ToRole     string
	Kind       string // proposal | question | review | blocker | decision | decree | system
	Body       string
	TaskID     string
	CreatedAt  time.Time
}

type ConclaveRelic struct {
	ConclaveID  string
	Name        string
	AuthorRole  string
	ContentType string
	Body        string
	SHA256      string
	CreatedAt   time.Time
}

type PairedDevice struct {
	ID           string
	Name         string
	Platform     string // ios | macos | linux | cli
	PublicKeyPEM string
	CreatedAt    time.Time
	RevokedAt    *time.Time
}

type MemoryEntry struct {
	Namespace string
	Key       string
	Value     string
	Author    string
	UpdatedAt time.Time
}

type LearningStatus string

const (
	LearningStatusProposed LearningStatus = "proposed"
	LearningStatusAccepted LearningStatus = "accepted"
	LearningStatusRetired  LearningStatus = "retired"
)

type LearningSourceKind string

const (
	LearningSourceOperator    LearningSourceKind = "operator"
	LearningSourceAgent       LearningSourceKind = "agent"
	LearningSourceVerifier    LearningSourceKind = "verifier"
	LearningSourceShadewright LearningSourceKind = "shadewright"
	LearningSourceTask        LearningSourceKind = "task"
	LearningSourceCouncil     LearningSourceKind = "council"
	LearningSourceIncident    LearningSourceKind = "incident"
	LearningSourceLegacy      LearningSourceKind = "legacy"
)

type LearningOutcomeKind string

const (
	LearningOutcomeSupporting    LearningOutcomeKind = "supporting"
	LearningOutcomeContradiction LearningOutcomeKind = "contradiction"
	LearningOutcomeInconclusive  LearningOutcomeKind = "inconclusive"
)

// LearningReviewTTL is the 90-day acceptance and confirmation review window from ADR-0012.
const LearningReviewTTL = 90 * 24 * time.Hour

// LearningScope identifies where a learning applies. Global scope cannot be combined
// with narrower selectors. Narrower selectors match conjunctively. Ambiguous legacy
// imports set NeedsScopeMapping and retain LegacyNamespace for operator mapping.
type LearningScope struct {
	Global            bool     `json:"global,omitempty"`
	RepoRemoteURL     string   `json:"repoRemoteURL,omitempty"`
	PathGlobs         []string `json:"pathGlobs,omitempty"`
	Cluster           string   `json:"cluster,omitempty"`
	Namespace         string   `json:"namespace,omitempty"`
	WorkloadKind      string   `json:"workloadKind,omitempty"`
	Symptom           string   `json:"symptom,omitempty"`
	LegacyNamespace   string   `json:"legacyNamespace,omitempty"`
	NeedsScopeMapping bool     `json:"needsScopeMapping,omitempty"`
}

// Learning is a versioned, immutable-statement advisory record per ADR-0012.
type Learning struct {
	ID                 string
	VersionDigest      string
	Statement          string
	Scope              LearningScope
	ScopeKind          string
	ScopeKey           string
	ScopeSpecificity   int
	SourceKind         LearningSourceKind
	SourceID           string
	EvidenceRef        string
	ProvenanceVerified bool // Evidence resolved at proposal time. Acceptance checks it again.
	Author             string
	PredecessorID      string
	Status             LearningStatus
	AcceptedBy         string
	ReviewReason       string
	LegacyNamespace    string
	LegacyKey          string
	LegacyAuthor       string
	LegacyUpdatedAt    string
	CreatedAt          time.Time
	LastConfirmedAt    *time.Time
	ExpiresAt          *time.Time
	UpdatedAt          time.Time
	InjectionCount     int64
}

// IsEligible reports whether the learning is accepted, mapped, and unexpired at now.
func (l Learning) IsEligible(now time.Time) bool {
	if l.Status != LearningStatusAccepted || l.Scope.NeedsScopeMapping || l.ExpiresAt == nil {
		return false
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	return now.Before(*l.ExpiresAt)
}

// EffectiveStatus returns LearningStatusProposed when an accepted learning is expired at now.
func (l Learning) EffectiveStatus(now time.Time) LearningStatus {
	if l.Status == LearningStatusAccepted && !l.IsEligible(now) {
		return LearningStatusProposed
	}
	return l.Status
}

// LearningHistory records every status transition and confirmation for traceability.
type LearningHistory struct {
	ID            int64
	LearningID    string
	FromStatus    LearningStatus
	ToStatus      LearningStatus
	EventKind     string
	Actor         string
	Reason        string
	EvidenceRef   string
	PredecessorID string
	SupersededBy  string
	ExpiresAt     *time.Time
	CreatedAt     time.Time
}

// LearningOutcome links post-acceptance evidence to a specific learning version.
type LearningOutcome struct {
	ID          int64
	LearningID  string
	Kind        LearningOutcomeKind
	SourceKind  LearningSourceKind
	SourceID    string
	EvidenceRef string
	Summary     string
	Actor       string
	ConfirmedBy string
	ConfirmedAt *time.Time
	CreatedAt   time.Time
}
