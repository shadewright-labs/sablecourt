package mcpgate

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/shadewright-labs/sablecourt/internal/config"
	"github.com/shadewright-labs/sablecourt/internal/domain"
	"github.com/shadewright-labs/sablecourt/internal/policy"
	"github.com/shadewright-labs/sablecourt/internal/store"
	"github.com/shadewright-labs/sablecourt/internal/store/audittest"
)

// git.merge_pr meets a failed audit write at each of its steps. Every one
// answers HTTP 500 and says what already happened (issue 107 row 3): before
// an approval exists nothing changed; once the approval is spent, the answer
// says so and that the merge did not run; after the merge, that it ran.
func TestMergePRAuditFailuresSayWhatHappened(t *testing.T) {
	ctx := context.Background()
	sha := strings.Repeat("a1", 20)
	payload := `{"owner":"shadewright-labs","repo":"sablecourt","pr_number":42,"commit_sha":"` + sha + `"}`
	approvalRow := func(name string) func(domain.AuditRecord) bool {
		return func(rec domain.AuditRecord) bool { return rec.Category == "approval" && rec.Outcome == name }
	}
	cases := []struct {
		name         string
		approved     bool
		fail         func(domain.AuditRecord) bool
		wantBody     []string
		wantMerges   int
		wantEvidence bool
	}{
		{
			name: "precheck evidence row", fail: audittest.Action("evidence:git.pr_checks"),
			wantBody: []string{"refused: its audit record could not be written, nothing was changed"},
		},
		{
			name: "approval request row", fail: approvalRow("pending"), wantEvidence: true,
			wantBody: []string{"approval request refused: its audit record could not be written, nothing was changed"},
		},
		{
			name: "approval use row", approved: true, fail: approvalRow("consumed"),
			wantBody: []string{"approval is spent", "did not run"},
		},
		{
			name: "run evidence row after the approval is spent", approved: true, fail: audittest.Action("evidence:git.pr_checks"),
			wantBody: []string{"was spent and the action did not run, but an audit record could not be written"},
		},
		{
			name: "allowed row after the merge", approved: true, fail: outcome("allowed"), wantMerges: 1, wantEvidence: true,
			wantBody: []string{"the action ran", "do not repeat it"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var faults *audittest.Store
			backend := &fakePRBackend{openNumber: 42, report: passingReport(sha)}
			gate, st, queue, token := setupPRGateWithStore(t, backend, domain.EffectApprove, []string{"shadewright-labs/sablecourt"}, func(in store.Store) store.Store {
				faults = audittest.Wrap(in)
				return faults
			})
			h := gate.ActionHandler()
			openTestPR(t, h, token)
			body := `{"payload":` + payload + `}`
			if tc.approved {
				rec := postAction(t, h, http.MethodPost, "/git.merge_pr", token, body)
				if rec.Code != http.StatusAccepted {
					t.Fatalf("request approval: %d %s", rec.Code, rec.Body.String())
				}
				var pending struct {
					ApprovalID    string `json:"approval_id"`
					PayloadSHA256 string `json:"payload_sha256"`
				}
				_ = json.Unmarshal(rec.Body.Bytes(), &pending)
				if _, err := queue.Approve(ctx, pending.ApprovalID, pending.PayloadSHA256, "operator"); err != nil {
					t.Fatal(err)
				}
				if err := st.DeleteMemory(ctx, prChecksNamespace, "shadewright-labs/sablecourt#42"); err != nil {
					t.Fatal(err)
				}
				body = `{"approvalId":"` + pending.ApprovalID + `","payload":` + payload + `}`
			}
			faults.Fail(tc.fail)
			rec := postAction(t, h, http.MethodPost, "/git.merge_pr", token, body)
			if rec.Code != http.StatusInternalServerError {
				t.Fatalf("got %d %s, want 500", rec.Code, rec.Body.String())
			}
			for _, want := range tc.wantBody {
				if !strings.Contains(rec.Body.String(), want) {
					t.Fatalf("body %s, want it to contain %q", rec.Body.String(), want)
				}
			}
			if backend.mergeCalls != tc.wantMerges {
				t.Fatalf("MergePR called %d times, want %d", backend.mergeCalls, tc.wantMerges)
			}
			_, err := st.GetMemory(ctx, prChecksNamespace, "shadewright-labs/sablecourt#42")
			if stored := err == nil; stored != tc.wantEvidence {
				t.Fatalf("evidence stored=%v (err %v), want %v", stored, err, tc.wantEvidence)
			}
			if !tc.wantEvidence && !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("evidence lookup: %v", err)
			}
			if len(faults.Refused()) == 0 {
				t.Fatal("no audit write was attempted")
			}
		})
	}
}

// An MCP tool whose executor meets a failed audit write says what already
// happened, through the same text as an action.
func TestToolAuditFailureSaysWhatHappened(t *testing.T) {
	refused := store.RecordAudit(context.Background(), nil, domain.AuditRecord{})
	cases := []struct {
		name     string
		err      error
		wantText string
	}{
		{"refused before its effect", refused, "probe.write refused: its audit record could not be written, nothing was changed"},
		{"after its effect", store.AfterEffect("memory entry stored", refused), "memory entry stored, but an audit record could not be written"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tg := setupTypedGate(t, typedGateOptions{effect: domain.EffectDeny})
			wl := domain.Workload{
				Metadata: domain.ObjectMeta{Name: "ops-agent", Zone: domain.ZoneRuntime},
				Spec:     domain.WorkloadSpec{Class: domain.ClassLongLivedAgent, Grants: domain.GrantSet{MCPTools: []string{"probe.write"}}},
			}
			pol := domain.Policy{Metadata: domain.ObjectMeta{Name: "p"}, Spec: domain.PolicySpec{DefaultEffect: domain.EffectDeny, Rules: []domain.PolicyRule{
				{Workload: "*", Capability: "mcp:*", Effect: domain.EffectAllow},
			}}}
			g := New(Options{Store: tg.st, Bundle: &config.Bundle{Workloads: map[string]domain.Workload{"ops-agent": wl}}, Policy: policy.NewEngine([]domain.Policy{pol}, nil)})
			def := ToolDef{Name: "probe.write", Handler: func(context.Context, domain.Workload, string, json.RawMessage) (string, error) {
				return "", tc.err
			}}
			g.RegisterTool(def)
			res, err := g.invokeTool(context.Background(), domain.Lease{TaskID: "task-ops-agent", WorkloadName: "ops-agent"}, wl, def, json.RawMessage(`{}`))
			if err != nil {
				t.Fatal(err)
			}
			text := res.Content[0].(*mcp.TextContent).Text
			if !res.IsError || !strings.Contains(text, tc.wantText) {
				t.Fatalf("got error=%v %q, want %q", res.IsError, text, tc.wantText)
			}
		})
	}
}
