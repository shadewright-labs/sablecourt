package mcpgate

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/shadewright-labs/sablecourt/internal/domain"
	"github.com/shadewright-labs/sablecourt/internal/store"
	"github.com/shadewright-labs/sablecourt/internal/store/audittest"
)

// postActionTracked posts like postAction with an effects recorder on the
// request and returns the effects the request recorded.
func postActionTracked(t *testing.T, h http.Handler, path, token, body string) (*httptest.ResponseRecorder, []string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "http://sablecourt.local"+path, strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	ctx, effects := store.TrackEffects(req.Context())
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req.WithContext(ctx))
	return rec, effects.List()
}

// requireAuditAnswer checks that rec is a 500 whose text lists exactly the
// effects the request recorded, and that those are want, with each key of
// vars replaced by its value.
func requireAuditAnswer(t *testing.T, rec *httptest.ResponseRecorder, what string, got, want []string, vars map[string]string) {
	t.Helper()
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("got %d %s, want 500", rec.Code, rec.Body.String())
	}
	if text := errorText(t, rec); text != audittest.Answer(what, got...) {
		t.Fatalf("answer %q does not list the recorded effects %q", text, got)
	}
	var expanded []string
	for _, w := range want {
		for k, v := range vars {
			w = strings.ReplaceAll(w, k, v)
		}
		expanded = append(expanded, w)
	}
	if !slices.Equal(got, expanded) {
		t.Fatalf("effects %q, want %q", got, expanded)
	}
}

// A git.push whose allowed row cannot be written after the push lists the
// spent approval, the pushed commit, its provenance record and the run.
func TestPushAuditFailureListsThePush(t *testing.T) {
	branch := "sablecourt/task-push-1"
	repoDir, bareDir, commitSHA := initGitRepoWithBareRemote(t, branch)
	gate, st, queue, token := setupGitPushGate(t, repoDir, pushWorkload(repoDir, bareDir), domain.EffectApprove, nil)
	h := gate.ActionHandler()
	payload := `{"branch":"` + branch + `","commit_sha":"` + commitSHA + `","destination":"` + bareDir + `"}`
	body := requestAndApprovePush(t, h, queue, token, payload)
	path, err := st.MainFile(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Exec(`CREATE TRIGGER fail_allowed_audit BEFORE INSERT ON audit_log
		WHEN NEW.outcome = 'allowed' BEGIN SELECT RAISE(ABORT, 'audit unavailable'); END`); err != nil {
		t.Fatal(err)
	}
	var approvalID struct {
		ApprovalID string `json:"approvalId"`
	}
	if err := json.Unmarshal([]byte(body), &approvalID); err != nil {
		t.Fatal(err)
	}
	rec, effects := postActionTracked(t, h, "/git.push", token, body)
	if len(effects) != 4 || !strings.HasPrefix(effects[2], "memory entry ") {
		t.Fatalf("effects %q", effects)
	}
	requireAuditAnswer(t, rec, "the action", effects, []string{
		"approval {approval} was spent",
		"commit {commit} was pushed to branch {branch}",
		effects[2], // the provenance record, keyed by task and commit
		"action git.push ran",
	}, map[string]string{"{approval}": approvalID.ApprovalID, "{commit}": commitSHA, "{branch}": branch})
	if !remoteHasBranch(bareDir, branch) {
		t.Fatal("branch not pushed")
	}
}
