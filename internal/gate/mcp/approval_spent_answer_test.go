package mcpgate

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/shadewright-labs/sablecourt/internal/domain"
	"github.com/shadewright-labs/sablecourt/internal/store"
)

type evidencePutFailure struct {
	store.Store
	fail bool
}

func (s *evidencePutFailure) PutMemory(ctx context.Context, entry domain.MemoryEntry) error {
	if s.fail && entry.Namespace == prChecksNamespace {
		return errors.New("evidence store unavailable")
	}
	return s.Store.PutMemory(ctx, entry)
}

// An approved run that fails after its approval was spent, in the executor or
// in a check at run time, says in its answer that the approval is spent, so
// the caller knows to request a new one.
func TestFailureAfterSpentApprovalSaysSo(t *testing.T) {
	ctx := context.Background()
	t.Run("merge evidence cannot be stored", func(t *testing.T) {
		sha := strings.Repeat("a1", 20)
		payload := `{"owner":"shadewright-labs","repo":"sablecourt","pr_number":42,"commit_sha":"` + sha + `"}`
		var faults *evidencePutFailure
		backend := &fakePRBackend{openNumber: 42, report: passingReport(sha)}
		gate, st, queue, token := setupPRGateWithStore(t, backend, domain.EffectApprove, []string{"shadewright-labs/sablecourt"}, func(in store.Store) store.Store {
			faults = &evidencePutFailure{Store: in}
			return faults
		})
		h := gate.ActionHandler()
		openTestPR(t, h, token)
		rec := postAction(t, h, http.MethodPost, "/git.merge_pr", token, `{"payload":`+payload+`}`)
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
		faults.fail = true
		rec = postAction(t, h, http.MethodPost, "/git.merge_pr", token, `{"approvalId":"`+pending.ApprovalID+`","payload":`+payload+`}`)
		if backend.mergeCalls != 0 {
			t.Fatalf("merged %d times without stored evidence", backend.mergeCalls)
		}
		if rec.Code == http.StatusOK || !strings.Contains(rec.Body.String(), "approval "+pending.ApprovalID+" is spent, request a new one") {
			t.Fatalf("got %d %s, want an error naming the spent approval", rec.Code, rec.Body.String())
		}
	})
	t.Run("push refused at run time", func(t *testing.T) {
		branch := "sablecourt/task-push-1"
		repoDir, bareDir, commitSHA := initGitRepoWithBareRemote(t, branch)
		gate, _, queue, token := setupGitPushGate(t, repoDir, pushWorkload(repoDir, bareDir), domain.EffectApprove, nil)
		h := gate.ActionHandler()
		body := requestAndApprovePush(t, h, queue, token, `{"branch":"`+branch+`","commit_sha":"`+commitSHA+`","destination":"`+bareDir+`"}`)
		var sent struct {
			ApprovalID string `json:"approvalId"`
		}
		_ = json.Unmarshal([]byte(body), &sent)
		runGit(t, repoDir, "commit", "--allow-empty", "-m", "unreviewed")
		rec := postAction(t, h, http.MethodPost, "/git.push", token, body)
		if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "approval "+sent.ApprovalID+" is spent, request a new one") {
			t.Fatalf("got %d %s, want a 409 naming the spent approval", rec.Code, rec.Body.String())
		}
	})
}
