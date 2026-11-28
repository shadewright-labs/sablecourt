package mcpgate

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/shadewright-labs/sablecourt/internal/approval"
	"github.com/shadewright-labs/sablecourt/internal/domain"
	"github.com/shadewright-labs/sablecourt/internal/policy"
	"github.com/shadewright-labs/sablecourt/internal/redact"
	"github.com/shadewright-labs/sablecourt/internal/store"
)

// createApprovalEcho fails CreateApproval with an error that repeats a value,
// as a database error that quotes the row it refused would.
type createApprovalEcho struct {
	store.Store
	echo string
}

func (s createApprovalEcho) CreateApproval(context.Context, domain.Approval) error {
	return fmt.Errorf("insert approval: duplicate key near %s", s.echo)
}

// The error of a failed approval request reaches the workload only through
// the redactor, on the MCP tool path and on the action path (issue 186).
func TestApprovalRequestErrorIsRedacted(t *testing.T) {
	held := "approval_held_" + strings.Repeat("k9", 12)
	redact.Register(held)
	shaped := "ghp_" + strings.Repeat("Z4x", 12)
	for _, secret := range []string{held, shaped} {
		for _, path := range []string{"mcp tool", "action"} {
			t.Run(path+"/"+secret[:4], func(t *testing.T) {
				gate, st, _, token := setupMCPGateTest(t)
				bundle := gate.snapshot().bundle
				wl := bundle.Workloads["triage-agent"]
				wl.Spec.Grants.MCPTools = append(wl.Spec.Grants.MCPTools, "github.comment")
				wl.Spec.Grants.Actions = append(wl.Spec.Grants.Actions, "test.echo")
				bundle.Workloads["triage-agent"] = wl
				pol := bundle.Policies["default"]
				pol.Spec.Rules = append(pol.Spec.Rules,
					domain.PolicyRule{Workload: "triage-agent", Capability: "mcp:github.comment", Effect: domain.EffectApprove},
					domain.PolicyRule{Workload: "triage-agent", Capability: "action:test.echo", Effect: domain.EffectApprove},
				)
				mustReload(t, gate, bundle, policy.NewEngine([]domain.Policy{pol}, nil))
				gate.RegisterAction("test.echo", func(context.Context, domain.Workload, string, json.RawMessage) (any, error) {
					t.Fatal("action ran without an approval")
					return nil, nil
				})
				gate.approvals = approval.NewQueue(createApprovalEcho{Store: st, echo: secret}, autoApprover{})

				var answer string
				switch path {
				case "mcp tool":
					def := ToolDef{Name: "github.comment", Handler: func(context.Context, domain.Workload, string, json.RawMessage) (string, error) {
						t.Fatal("tool ran without an approval")
						return "", nil
					}}
					res, err := gate.invokeTool(context.Background(), domain.Lease{TaskID: "task-1", WorkloadName: "triage-agent"}, wl, def, json.RawMessage(`{"body":"hi"}`))
					if err != nil {
						t.Fatal(err)
					}
					if !res.IsError {
						t.Fatalf("result is not an error: %+v", res)
					}
					answer = toolText(t, res)
				case "action":
					rec := postAction(t, gate.ActionHandler(), "POST", "/test.echo", token, `{"payload":{"body":"hi"}}`)
					if rec.Code != 500 {
						t.Fatalf("got %d %s", rec.Code, rec.Body.String())
					}
					answer = rec.Body.String()
				}
				if strings.Contains(answer, secret) {
					t.Fatalf("answer repeats the secret: %s", answer)
				}
				if !strings.Contains(answer, "duplicate key near") {
					t.Fatalf("answer lost the error: %s", answer)
				}
			})
		}
	}
}
