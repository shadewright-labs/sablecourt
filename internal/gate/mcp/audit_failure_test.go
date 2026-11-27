package mcpgate

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/shadewright-labs/sablecourt/internal/config"
	"github.com/shadewright-labs/sablecourt/internal/domain"
	"github.com/shadewright-labs/sablecourt/internal/policy"
	"github.com/shadewright-labs/sablecourt/internal/store"
	"github.com/shadewright-labs/sablecourt/internal/store/audittest"
)

func outcome(name string) func(domain.AuditRecord) bool {
	return func(rec domain.AuditRecord) bool { return rec.Outcome == name }
}

// An action whose audit record cannot be written fails closed (issue 107
// row 3). Each row is one class of audit write on the action path: a refusal
// stays a refusal; a record that must precede the side effect stops it; a
// record of a side effect that already ran is reported to the caller.
func TestActionAuditWriteFailures(t *testing.T) {
	cases := []struct {
		name       string
		effect     domain.PolicyEffect
		approved   bool
		fail       func(domain.AuditRecord) bool
		wantCode   int
		wantBody   string
		wantOpened int
	}{
		{
			name: "refusal stays refused", effect: domain.EffectDeny, fail: nil,
			wantCode: 403, wantBody: "policy denied", wantOpened: 0,
		},
		{
			name: "allowed run not recorded before it starts", effect: domain.EffectAllow, fail: outcome("started"),
			wantCode: 500, wantBody: "the action did not run", wantOpened: 0,
		},
		{
			name: "approved run not recorded before it starts", effect: domain.EffectApprove, approved: true, fail: outcome("consumed"),
			wantCode: 500, wantBody: "did not run", wantOpened: 0,
		},
		{
			name: "run recorded only before it ran", effect: domain.EffectAllow, fail: outcome("allowed"),
			wantCode: 500, wantBody: "the action ran, but its audit record could not be written", wantOpened: 1,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var faults *audittest.Store
			tg := setupTypedGate(t, typedGateOptions{effect: tc.effect, wrapStore: func(in store.Store) store.Store {
				faults = audittest.Wrap(in)
				return faults
			}})
			body := `{"payload":` + validOpenPR + `}`
			if tc.approved {
				body = `{"approvalId":"` + requestAndApprove(t, tg, "git.open_pr", validOpenPR) + `","payload":` + validOpenPR + `}`
			}
			faults.Fail(tc.fail)
			rec := tg.post("ops-agent", "git.open_pr", body)
			if rec.Code != tc.wantCode || !strings.Contains(rec.Body.String(), tc.wantBody) {
				t.Fatalf("got %d %s, want %d containing %q", rec.Code, rec.Body.String(), tc.wantCode, tc.wantBody)
			}
			if n := len(tg.pr.calls()); n != tc.wantOpened {
				t.Fatalf("pull request opened %d times, want %d", n, tc.wantOpened)
			}
			if len(faults.Refused()) == 0 {
				t.Fatal("no audit write was attempted")
			}
		})
	}
}

// An MCP tool call follows the same classes: a denial stays denied, a call
// whose record cannot be written before it runs never reaches the tool, and
// a call that ran but cannot be recorded returns an error, not its output.
func TestToolAuditWriteFailures(t *testing.T) {
	cases := []struct {
		name      string
		effect    domain.PolicyEffect
		fail      func(domain.AuditRecord) bool
		wantText  string
		wantCalls int
	}{
		{name: "denial stays denied", effect: domain.EffectDeny, wantText: "policy denied", wantCalls: 0},
		{name: "call not recorded before it runs", effect: domain.EffectAllow, fail: outcome("started"), wantText: "tool call refused: its audit record could not be written", wantCalls: 0},
		{name: "call recorded only before it ran", effect: domain.EffectAllow, fail: outcome("allowed"), wantText: "the tool ran, but its audit record could not be written", wantCalls: 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tg := setupTypedGate(t, typedGateOptions{effect: domain.EffectDeny})
			faults := audittest.Wrap(tg.st)
			wl := domain.Workload{
				Metadata: domain.ObjectMeta{Name: "ops-agent", Zone: domain.ZoneRuntime},
				Spec:     domain.WorkloadSpec{Class: domain.ClassLongLivedAgent, Grants: domain.GrantSet{MCPTools: []string{"probe.write"}}},
			}
			pol := domain.Policy{Metadata: domain.ObjectMeta{Name: "p"}, Spec: domain.PolicySpec{DefaultEffect: domain.EffectDeny, Rules: []domain.PolicyRule{
				{Workload: "*", Capability: "mcp:*", Effect: tc.effect},
			}}}
			g := New(Options{Store: faults, Bundle: &config.Bundle{Workloads: map[string]domain.Workload{"ops-agent": wl}}, Policy: policy.NewEngine([]domain.Policy{pol}, nil)})
			calls := 0
			def := ToolDef{Name: "probe.write", Handler: func(context.Context, domain.Workload, string, json.RawMessage) (string, error) {
				calls++
				return "secret-free output", nil
			}}
			g.RegisterTool(def)
			faults.Fail(tc.fail)
			res, err := g.invokeTool(context.Background(), domain.Lease{TaskID: "task-ops-agent", WorkloadName: "ops-agent"}, wl, def, json.RawMessage(`{}`))
			if err != nil {
				t.Fatal(err)
			}
			text := res.Content[0].(*mcp.TextContent).Text
			if !res.IsError || !strings.Contains(text, tc.wantText) {
				t.Fatalf("got error=%v %q, want an error containing %q", res.IsError, text, tc.wantText)
			}
			if calls != tc.wantCalls {
				t.Fatalf("tool ran %d times, want %d", calls, tc.wantCalls)
			}
			if len(faults.Refused()) == 0 {
				t.Fatal("no audit write was attempted")
			}
		})
	}
}

// memory.put and memory.delete change Court Memory before their own record
// is written, so a failed record is returned as the tool's error.
func TestMemoryToolAuditWriteFailures(t *testing.T) {
	cases := []struct {
		name, tool, args, wantErr string
	}{
		{"put", "memory.put", `{"namespace":"notes","key":"k","value":"v"}`, "memory entry stored, but audit record could not be written"},
		{"delete", "memory.delete", `{"namespace":"notes","key":"k"}`, "memory entry deleted, but audit record could not be written"},
		{"reserved namespace stays refused", "memory.put", `{"namespace":"system:x","key":"k","value":"v"}`, "reserved"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tg := setupTypedGate(t, typedGateOptions{effect: domain.EffectDeny})
			faults := audittest.Wrap(tg.st)
			g := New(Options{Store: faults})
			RegisterBuiltinMemory(g, faults)
			if err := tg.st.PutMemory(context.Background(), domain.MemoryEntry{Namespace: "notes", Key: "k", Value: "v", Author: "seed"}); err != nil {
				t.Fatal(err)
			}
			faults.Fail(nil)
			_, err := g.tools[tc.tool].Handler(context.Background(), domain.Workload{Metadata: domain.ObjectMeta{Name: "ops-agent"}}, "task-ops-agent", json.RawMessage(tc.args))
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("got %v, want an error containing %q", err, tc.wantErr)
			}
		})
	}
}
