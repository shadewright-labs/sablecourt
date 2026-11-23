package domain

import "time"

const APIVersionV1Alpha1 = "sablecourt.io/v1alpha1"

const (
	KindWorkload = "Workload"
	KindPolicy   = "Policy"
	KindBudget   = "Budget"
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
	ModelGate  ModelGateGrant `json:"modelGate"`
	MCPTools   []string       `json:"mcpTools,omitempty"`
	Actions    []string       `json:"actions,omitempty"`
	DelegateTo []string       `json:"delegateTo,omitempty"`
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

type BudgetEntry struct {
	ID           int64
	TaskID       string
	WorkloadName string
	Provider     string
	Model        string
	InputTokens  int64
	OutputTokens int64
	CostMicroUSD int64
	CreatedAt    time.Time
}

type BudgetTotals struct {
	InputTokens  int64
	OutputTokens int64
	CostMicroUSD int64
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
	CreatedAt    time.Time
}
