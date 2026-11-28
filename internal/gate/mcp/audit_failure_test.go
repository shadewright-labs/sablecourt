package mcpgate

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
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
		name     string
		effect   domain.PolicyEffect
		approved bool
		fail     func(domain.AuditRecord) bool
		wantCode int
		wantBody string
		// wantEffects are the effects a 500 lists, {approval} standing for
		// the approval ID.
		wantEffects []string
		wantOpened  int
	}{
		{
			name: "refusal stays refused", effect: domain.EffectDeny, fail: nil,
			wantCode: 403, wantBody: "policy denied", wantOpened: 0,
		},
		{
			name: "allowed run not recorded before it starts", effect: domain.EffectAllow, fail: outcome("started"),
			wantCode: 500, wantOpened: 0,
		},
		{
			name: "approved run not recorded before it starts", effect: domain.EffectApprove, approved: true, fail: outcome("consumed"),
			wantCode: 500, wantEffects: []string{"approval {approval} was spent"}, wantOpened: 0,
		},
		{
			name: "run recorded only before it ran", effect: domain.EffectAllow, fail: outcome("allowed"),
			wantCode: 500, wantOpened: 1, wantEffects: []string{
				"pull request https://github.com/acme/widgets/pull/5 was opened from sablecourt/fix-1 to main",
				"memory entry system:opened-prs/acme/widgets#5 was stored",
				"action git.open_pr ran",
			},
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
			approvalID := ""
			if tc.approved {
				approvalID = requestAndApprove(t, tg, "git.open_pr", validOpenPR)
				body = `{"approvalId":"` + approvalID + `","payload":` + validOpenPR + `}`
			}
			faults.Fail(tc.fail)
			rec := tg.post("ops-agent", "git.open_pr", body)
			if rec.Code != tc.wantCode || !strings.Contains(rec.Body.String(), tc.wantBody) {
				t.Fatalf("got %d %s, want %d containing %q", rec.Code, rec.Body.String(), tc.wantCode, tc.wantBody)
			}
			if tc.wantCode == 500 {
				var want []string
				for _, e := range tc.wantEffects {
					want = append(want, strings.ReplaceAll(e, "{approval}", approvalID))
				}
				if got, wantText := errorText(t, rec), audittest.Answer("the action", want...); got != wantText {
					t.Fatalf("answer %q, want %q", got, wantText)
				}
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
		{name: "call not recorded before it runs", effect: domain.EffectAllow, fail: outcome("started"), wantText: audittest.Answer("the tool call"), wantCalls: 0},
		{name: "call recorded only before it ran", effect: domain.EffectAllow, fail: outcome("allowed"), wantText: audittest.Answer("the tool call", "tool probe.write ran"), wantCalls: 1},
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
// is written, so a failed record is returned as the tool's error, with the
// change recorded on the call's effects.
func TestMemoryToolAuditWriteFailures(t *testing.T) {
	cases := []struct {
		name, tool, args, wantErr string
		wantEffects               []string
	}{
		{"put", "memory.put", `{"namespace":"notes","key":"k","value":"v"}`, "audit record could not be written", []string{"memory entry notes/k was stored"}},
		{"delete", "memory.delete", `{"namespace":"notes","key":"k"}`, "audit record could not be written", []string{"memory entry notes/k was deleted"}},
		{"reserved namespace stays refused", "memory.put", `{"namespace":"system:x","key":"k","value":"v"}`, "reserved", nil},
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
			ctx, effects := store.TrackEffects(context.Background())
			_, err := g.tools[tc.tool].Handler(ctx, domain.Workload{Metadata: domain.ObjectMeta{Name: "ops-agent"}}, "task-ops-agent", json.RawMessage(tc.args))
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("got %v, want an error containing %q", err, tc.wantErr)
			}
			if !slices.Equal(effects.List(), tc.wantEffects) {
				t.Fatalf("effects %q, want %q", effects.List(), tc.wantEffects)
			}
		})
	}
}

// git.pr_checks records its evidence row before it stores the evidence, so a
// row that cannot be written leaves no evidence the audit log does not show,
// and the caller gets HTTP 500 (issue 107 row 3).
func TestPRChecksEvidenceNotStoredWithoutItsRow(t *testing.T) {
	ctx := context.Background()
	var faults *audittest.Store
	backend := &fakePRBackend{openNumber: 42, report: passingReport(strings.Repeat("a1", 20))}
	gate, st, _, token := setupPRGateWithStore(t, backend, domain.EffectAllow, []string{"shadewright-labs/sablecourt"}, func(in store.Store) store.Store {
		faults = audittest.Wrap(in)
		return faults
	})
	h := gate.ActionHandler()
	openTestPR(t, h, token)
	faults.Fail(audittest.Action("evidence:git.pr_checks"))
	rec := postAction(t, h, "POST", "/git.pr_checks", token, `{"payload":{"owner":"shadewright-labs","repo":"sablecourt","pr_number":42}}`)
	if rec.Code != 500 || errorText(t, rec) != audittest.Answer("the action") {
		t.Fatalf("got %d %s", rec.Code, rec.Body.String())
	}
	if _, err := st.GetMemory(ctx, prChecksNamespace, "shadewright-labs/sablecourt#42"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("evidence stored without its row: %v", err)
	}
}
