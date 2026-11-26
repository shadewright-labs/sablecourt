package ui

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/shadewright-labs/sablecourt/internal/contextpack"
	"github.com/shadewright-labs/sablecourt/internal/domain"
	"github.com/shadewright-labs/sablecourt/internal/providers"
	"github.com/shadewright-labs/sablecourt/internal/store"
	"github.com/shadewright-labs/sablecourt/internal/store/sqlite"
)

func TestDashboardRendersFactsAndServesBrandAssets(t *testing.T) {
	ctx := context.Background()
	st, err := sqlite.Open(filepath.Join(t.TempDir(), "sablecourt.db"))
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	_ = st.UpsertWorkload(ctx, domain.WorkloadRecord{
		Name: "triage-agent", Zone: domain.ZoneRuntime, Class: domain.ClassLongLivedAgent,
		ManifestDigest: "abc123def456", AgentCardJSON: "{}",
	})

	dash, err := NewDashboard(Options{Store: st})
	if err != nil {
		t.Fatalf("NewDashboard: %v", err)
	}

	mux := http.NewServeMux()
	dash.RegisterRoutes(mux)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "/assets/logo-topbar.png") || !strings.Contains(body, "/assets/sablecourt.css") || !strings.Contains(body, "/assets/shadewright-labs-mark.svg") || !strings.Contains(body, "triage-agent") {
		t.Fatalf("expected branded dashboard HTML, got:\n%s", body)
	}

	favRec := httptest.NewRecorder()
	mux.ServeHTTP(favRec, httptest.NewRequest(http.MethodGet, "/favicon.ico", nil))
	if favRec.Code != http.StatusOK || favRec.Header().Get("Content-Type") != "image/x-icon" || favRec.Body.Len() == 0 {
		t.Fatalf("expected ICO favicon, got code=%d ct=%q len=%d", favRec.Code, favRec.Header().Get("Content-Type"), favRec.Body.Len())
	}

	appleRec := httptest.NewRecorder()
	mux.ServeHTTP(appleRec, httptest.NewRequest(http.MethodGet, "/apple-touch-icon.png", nil))
	if appleRec.Code != http.StatusOK || appleRec.Header().Get("Content-Type") != "image/png" || appleRec.Body.Len() == 0 {
		t.Fatalf("expected apple-touch-icon PNG, got code=%d ct=%q len=%d", appleRec.Code, appleRec.Header().Get("Content-Type"), appleRec.Body.Len())
	}

	logoRec := httptest.NewRecorder()
	mux.ServeHTTP(logoRec, httptest.NewRequest(http.MethodGet, "/assets/logo-horizontal-dark.png", nil))
	if logoRec.Code != http.StatusOK || logoRec.Body.Len() == 0 {
		t.Fatalf("expected embedded logo asset, got code=%d len=%d", logoRec.Code, logoRec.Body.Len())
	}
}

func TestConclaveSaveLearningCreatesProposalWithoutInjecting(t *testing.T) {
	cases := []struct {
		name           string
		scope          string
		key            string
		rule           string
		wantProposalNS string
		wantTargetNS   string
		wantErr        bool
		source         string
	}{
		{
			name:           "global scope creates proposed learning",
			scope:          "global",
			key:            "conclave-msg-1",
			rule:           "Always run go test -race before merge",
			wantProposalNS: "proposed:learning:global",
			wantTargetNS:   "learning:global",
		},
		{
			name:           "empty scope defaults to proposed learning global",
			scope:          "",
			key:            "conclave-msg-1",
			rule:           "Keep PRs small",
			wantProposalNS: "proposed:learning:global",
			wantTargetNS:   "learning:global",
		},
		{
			name:           "repo scope creates proposed repo learning",
			scope:          "repo:sablecourt",
			key:            "conclave-msg-1",
			rule:           "No em dashes in docs",
			wantProposalNS: "proposed:learning:repo:sablecourt",
			wantTargetNS:   "learning:repo:sablecourt",
		},
		{
			name:           "path scope creates proposed path learning",
			scope:          "path:internal/gate/**",
			key:            "conclave-msg-1",
			rule:           "Always fail closed on policy errors",
			wantProposalNS: "proposed:learning:path:internal/gate/**",
			wantTargetNS:   "learning:path:internal/gate/**",
		},
		{
			name:    "empty rule fails closed",
			scope:   "global",
			key:     "conclave-msg-1",
			rule:    "   ",
			wantErr: true,
		},
		{
			name:    "missing message ID fails closed",
			scope:   "global",
			key:     "manual-rule",
			rule:    "Keep PRs small",
			wantErr: true,
		},
		{
			name:    "nonexistent message fails closed",
			scope:   "global",
			key:     "manual-rule",
			rule:    "Keep PRs small",
			wantErr: true,
			source:  "nonexistent",
		},
		{
			name:    "message from another conclave fails closed",
			scope:   "global",
			key:     "manual-rule",
			rule:    "Keep PRs small",
			wantErr: true,
			source:  "other conclave",
		},
		{
			name:    "message lookup failure fails closed",
			scope:   "global",
			key:     "conclave-msg-1",
			rule:    "Keep PRs small",
			wantErr: true,
			source:  "lookup failure",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			st, err := sqlite.Open(filepath.Join(t.TempDir(), "sablecourt.db"))
			if err != nil {
				t.Fatalf("open sqlite: %v", err)
			}
			t.Cleanup(func() { _ = st.Close() })
			if err := st.Migrate(ctx); err != nil {
				t.Fatalf("migrate: %v", err)
			}

			now := time.Now().UTC()
			session := domain.ConclaveSession{
				ID:           "cc-1",
				TemplateName: "arch-review",
				Title:        "Security Review",
				State:        domain.ConclaveStateActive,
				CreatedAt:    now,
				UpdatedAt:    now,
			}
			if err := st.CreateConclave(ctx, session); err != nil {
				t.Fatalf("CreateConclave: %v", err)
			}
			msgID, err := st.AppendConclaveMessage(ctx, domain.ConclaveMessage{
				ConclaveID: "cc-1",
				FromRole:   "coder",
				ToRole:     "all",
				Kind:       "proposal",
				Body:       "Original conclave message body from coder",
				CreatedAt:  now,
			})
			if err != nil {
				t.Fatalf("AppendConclaveMessage: %v", err)
			}

			dashStore := store.Store(st)
			if tc.source == "lookup failure" {
				dashStore = conclaveMessageLookupError{Store: st}
			}
			dash, err := NewDashboard(Options{Store: dashStore})
			if err != nil {
				t.Fatalf("NewDashboard: %v", err)
			}
			mux := http.NewServeMux()
			dash.RegisterRoutes(mux)

			form := url.Values{}
			form.Set("scope", tc.scope)
			if tc.key == "conclave-"+"msg-1" {
				form.Set("key", "conclave-msg-"+strconv.FormatInt(msgID, 10))
			} else {
				form.Set("key", tc.key)
			}
			form.Set("rule", tc.rule)
			switch tc.source {
			case "nonexistent":
				form.Set("message_id", strconv.FormatInt(msgID+1, 10))
			case "other conclave":
				session.ID = "cc-2"
				if err := st.CreateConclave(ctx, session); err != nil {
					t.Fatalf("create other conclave: %v", err)
				}
				otherID, err := st.AppendConclaveMessage(ctx, domain.ConclaveMessage{
					ConclaveID: session.ID,
					FromRole:   "coder",
					ToRole:     "all",
					Kind:       "proposal",
					Body:       "Other message",
					CreatedAt:  now,
				})
				if err != nil {
					t.Fatalf("append other message: %v", err)
				}
				form.Set("message_id", strconv.FormatInt(otherID, 10))
			}

			req := httptest.NewRequest(http.MethodPost, "/ui/conclaves/cc-1/learning", strings.NewReader(form.Encode()))
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, req)

			if rec.Code != http.StatusSeeOther {
				t.Fatalf("expected 303 SeeOther, got %d: %s", rec.Code, rec.Body.String())
			}

			// Critical invariant: "Save as learning" must NEVER inject anything into MatchLearnings until accepted.
			matched := contextpack.MatchLearnings(ctx, st, "sablecourt", []string{"internal/gate/mcp/memory.go"}, 20)
			if len(matched) != 0 {
				t.Fatalf("expected 0 injected learnings before acceptance, got %+v", matched)
			}

			if tc.wantErr {
				loc := rec.Header().Get("Location")
				if !strings.Contains(loc, "err=") || strings.Contains(loc, "error=") {
					t.Errorf("expected ?err= error redirect, got Location=%q", loc)
				}
				if _, err := st.GetMemory(ctx, "proposed:learning:global", form.Get("key")); !errors.Is(err, store.ErrNotFound) {
					t.Errorf("expected no proposal after rejection, got %v", err)
				}
				audits, err := st.ListAudit(ctx, 50)
				if err != nil {
					t.Fatalf("ListAudit: %v", err)
				}
				for _, rec := range audits {
					if rec.Action == "learning.propose" && rec.Outcome == "proposed" {
						t.Errorf("unexpected successful proposal audit after rejection")
					}
				}
				return
			}

			expectedKey := form.Get("key")
			entry, err := st.GetMemory(ctx, tc.wantProposalNS, expectedKey)
			if err != nil {
				t.Fatalf("expected proposed learning at %s/%s, got err=%v", tc.wantProposalNS, expectedKey, err)
			}

			var doc struct {
				Status          string `json:"status"`
				TargetNamespace string `json:"target_namespace"`
				Rule            string `json:"rule"`
				Source          struct {
					ConclaveID  string `json:"conclave_id"`
					MessageID   int64  `json:"message_id"`
					FromRole    string `json:"from_role"`
					MessageBody string `json:"message_body"`
				} `json:"source"`
			}
			if err := json.Unmarshal([]byte(entry.Value), &doc); err != nil {
				t.Fatalf("expected JSON proposal payload, got err=%v value=%q", err, entry.Value)
			}
			if doc.Status != "proposed" {
				t.Fatalf("status = %q, want proposed", doc.Status)
			}
			if doc.TargetNamespace != tc.wantTargetNS {
				t.Fatalf("target_namespace = %q, want %q", doc.TargetNamespace, tc.wantTargetNS)
			}
			if doc.Source.ConclaveID != "cc-1" || doc.Source.MessageID != msgID || doc.Source.FromRole != "coder" || doc.Source.MessageBody != "Original conclave message body from coder" {
				t.Fatalf("unexpected source message metadata: %+v", doc.Source)
			}
			if doc.Rule != strings.TrimSpace(tc.rule) {
				t.Fatalf("rule = %q, want %q", doc.Rule, strings.TrimSpace(tc.rule))
			}

			// Verify /memory renders the proposed learning and its source message.
			memRec := httptest.NewRecorder()
			mux.ServeHTTP(memRec, httptest.NewRequest(http.MethodGet, "/memory", nil))
			if memRec.Code != http.StatusOK {
				t.Fatalf("GET /memory returned %d", memRec.Code)
			}
			body := memRec.Body.String()
			if !strings.Contains(body, tc.wantProposalNS) || !strings.Contains(body, "Original conclave message body from coder") {
				t.Fatalf("expected /memory page to display proposed namespace %q and source message", tc.wantProposalNS)
			}
		})
	}
}

type conclaveMessageLookupError struct {
	store.Store
}

func (conclaveMessageLookupError) ListConclaveMessages(context.Context, string, int64) ([]domain.ConclaveMessage, error) {
	return nil, errors.New("message lookup failed")
}

func TestTaskMessageRedirect(t *testing.T) {
	mux := http.NewServeMux()
	dash, err := NewDashboard(Options{Store: newUITestStore(t)})
	if err != nil {
		t.Fatal(err)
	}
	dash.RegisterRoutes(mux)
	for _, id := range []string{"task", "two/parts", "task?view", "task#section", `../\` + httptest.NewRequest(http.MethodGet, "/", nil).Host} {
		t.Run(id, func(t *testing.T) {
			segment := url.PathEscape(id)
			r := httptest.NewRequest(http.MethodPost, "/ui/tasks/"+segment+"/messages", nil)
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, r)
			if loc := w.Header().Get("Location"); w.Code != http.StatusSeeOther || loc != "/tasks/"+segment {
				t.Fatalf("redirect = %d %q, want a single task path segment", w.Code, loc)
			}
		})
	}
}

func TestModelScriptEscaping(t *testing.T) {
	const model = `model</script>&"`
	reg := providers.NewRegistry(providers.Options{})
	if err := reg.SavePrefs(t.Context(), "local", model, []string{model}); err != nil {
		t.Fatal(err)
	}
	dash, err := NewDashboard(Options{Store: newUITestStore(t), Providers: reg})
	if err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	dash.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))
	for _, name := range []string{"modelsByProvider", "defaultsByProv"} {
		t.Run(name, func(t *testing.T) {
			_, after, ok := strings.Cut(w.Body.String(), "const "+name+" = ")
			declaration, _, _ := strings.Cut(after, ";")
			if !ok || !strings.Contains(declaration, `model\u003c/script\u003e\u0026`) || strings.Contains(declaration, "</script>") {
				t.Fatalf("unexpected script value for %s: %s", name, declaration)
			}
		})
	}
}
