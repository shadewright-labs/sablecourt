package ui

import (
	"bytes"
	"context"
	"fmt"
	"html"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/shadewright-labs/sablecourt/internal/conclave"
	"github.com/shadewright-labs/sablecourt/internal/config"
	"github.com/shadewright-labs/sablecourt/internal/domain"
	"github.com/shadewright-labs/sablecourt/internal/store"
	"github.com/shadewright-labs/sablecourt/internal/store/sqlite"
)

func newUITestStore(t *testing.T) store.Store {
	t.Helper()
	ctx := context.Background()
	st, err := sqlite.Open(filepath.Join(t.TempDir(), "sablecourt.db"))
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return st
}

func seedStudioManifests(t *testing.T, st store.Store) (string, *sync.RWMutex, *config.Bundle, func() *config.Bundle, func() error) {
	t.Helper()
	dir := t.TempDir()
	manifestFile := filepath.Join(dir, "base.yaml")
	baseYAML := `apiVersion: sablecourt.io/v1alpha1
kind: Workload
metadata:
  name: claude-worker
  zone: runtime
spec:
  class: deterministic-controller
  runtime:
    type: docker
    image: alpine:3.21
    readOnlyRootfs: true
  agentCard:
    description: First panelist workload.
  grants:
    modelGate:
      allowed: false
---
apiVersion: sablecourt.io/v1alpha1
kind: Workload
metadata:
  name: codex-worker
  zone: runtime
spec:
  class: deterministic-controller
  runtime:
    type: docker
    image: alpine:3.21
    readOnlyRootfs: true
  agentCard:
    description: Second panelist workload.
  grants:
    modelGate:
      allowed: false
---
apiVersion: sablecourt.io/v1alpha1
kind: Workload
metadata:
  name: agy-worker
  zone: runtime
spec:
  class: deterministic-controller
  runtime:
    type: docker
    image: alpine:3.21
    readOnlyRootfs: true
  agentCard:
    description: Third panelist workload.
  grants:
    modelGate:
      allowed: false
---
apiVersion: sablecourt.io/v1alpha1
kind: Workload
metadata:
  name: extra-worker
  zone: runtime
spec:
  class: deterministic-controller
  runtime:
    type: docker
    image: alpine:3.21
    readOnlyRootfs: true
  agentCard:
    description: Fourth workload for repeatable row checks.
  grants:
    modelGate:
      allowed: false
`
	if err := os.WriteFile(manifestFile, []byte(baseYAML), 0o600); err != nil {
		t.Fatalf("write base manifest: %v", err)
	}
	bundle, err := config.LoadPath(dir)
	if err != nil {
		t.Fatalf("LoadPath: %v", err)
	}
	ctx := context.Background()
	for _, w := range bundle.Workloads {
		if err := st.UpsertWorkload(ctx, domain.WorkloadRecord{
			Name:          w.Metadata.Name,
			Zone:          w.Metadata.Zone,
			Class:         w.Spec.Class,
			AgentCardJSON: "{}",
		}); err != nil {
			t.Fatalf("UpsertWorkload %s: %v", w.Metadata.Name, err)
		}
	}

	var mu sync.RWMutex
	cur := bundle
	getBundle := func() *config.Bundle {
		mu.RLock()
		defer mu.RUnlock()
		return cur
	}
	reload := func() error {
		next, err := config.LoadPath(dir)
		if err != nil {
			return err
		}
		mu.Lock()
		cur = next
		mu.Unlock()
		return nil
	}
	return dir, &mu, bundle, getBundle, reload
}

func TestStudioCreateConclaveRepeatableVassals(t *testing.T) {
	t.Run("studio page renders repeatable vassal builder", func(t *testing.T) {
		st := newUITestStore(t)
		dir, _, _, getBundle, reload := seedStudioManifests(t, st)
		dash, err := NewDashboard(Options{
			Store:         st,
			GetBundle:     getBundle,
			Reload:        reload,
			ManifestsPath: dir,
		})
		if err != nil {
			t.Fatalf("NewDashboard: %v", err)
		}
		mux := http.NewServeMux()
		dash.RegisterRoutes(mux)

		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/studio", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("GET /studio status = %d", rec.Code)
		}
		body := rec.Body.String()
		for _, want := range []string{`name="vassal_role"`, `name="vassal_workload"`, `name="vassal_activation"`, `id="add-vassal-btn"`, `Vassal 3`} {
			if !strings.Contains(body, want) {
				t.Errorf("GET /studio missing %q", want)
			}
		}
	})

	cases := []struct {
		name           string
		form           url.Values
		wantErr        bool
		wantErrContain string
		wantVassals    int
		wantMaxMsgs    int
		wantMaxPP      int
	}{
		{
			name: "three vassals with explicit limits creates conclave and opens in UI",
			form: url.Values{
				"name":              {"three-way-council"},
				"description":       {"Three panelists"},
				"max_messages":      {"24"},
				"max_ping_pong":     {"3"},
				"vassal_role":       {"claude", "codex", "agy"},
				"vassal_workload":   {"claude-worker", "codex-worker", "agy-worker"},
				"vassal_activation": {"always-on", "on-mention", "on-mention"},
			},
			wantVassals: 3,
			wantMaxMsgs: 24,
			wantMaxPP:   3,
		},
		{
			name: "four vassals works via repeatable rows and defaults omitted ward limits",
			form: url.Values{
				"name":              {"four-way-council"},
				"description":       {"Four panelists"},
				"vassal_role":       {"claude", "codex", "agy", "extra"},
				"vassal_workload":   {"claude-worker", "codex-worker", "agy-worker", "extra-worker"},
				"vassal_activation": {"always-on", "on-mention", "on-mention", "on-mention"},
			},
			wantVassals: 4,
			wantMaxMsgs: 40,
			wantMaxPP:   4,
		},
		{
			name: "empty optional rows are ignored when building three vassals",
			form: url.Values{
				"name":              {"three-with-blank-row"},
				"description":       {"Blank extra row ignored"},
				"vassal_role":       {"claude", "codex", "agy", ""},
				"vassal_workload":   {"claude-worker", "codex-worker", "agy-worker", ""},
				"vassal_activation": {"always-on", "on-mention", "on-mention", "on-mention"},
			},
			wantVassals: 3,
			wantMaxMsgs: 40,
			wantMaxPP:   4,
		},
		{
			name: "empty name fails closed",
			form: url.Values{
				"name":              {"   "},
				"vassal_role":       {"claude"},
				"vassal_workload":   {"claude-worker"},
				"vassal_activation": {"always-on"},
			},
			wantErr:        true,
			wantErrContain: "conclave name is required",
		},
		{
			name: "zero max_messages fails closed",
			form: url.Values{
				"name":              {"zero-msg-council"},
				"max_messages":      {"0"},
				"max_ping_pong":     {"4"},
				"vassal_role":       {"claude"},
				"vassal_workload":   {"claude-worker"},
				"vassal_activation": {"always-on"},
			},
			wantErr:        true,
			wantErrContain: "positive integer",
		},
		{
			name: "negative max_ping_pong fails closed",
			form: url.Values{
				"name":              {"neg-pp-council"},
				"max_messages":      {"40"},
				"max_ping_pong":     {"-2"},
				"vassal_role":       {"claude"},
				"vassal_workload":   {"claude-worker"},
				"vassal_activation": {"always-on"},
			},
			wantErr:        true,
			wantErrContain: "positive integer",
		},
		{
			name: "non-numeric max_messages fails closed",
			form: url.Values{
				"name":              {"nan-msg-council"},
				"max_messages":      {"ten"},
				"max_ping_pong":     {"4"},
				"vassal_role":       {"claude"},
				"vassal_workload":   {"claude-worker"},
				"vassal_activation": {"always-on"},
			},
			wantErr:        true,
			wantErrContain: "positive integer",
		},
		{
			name: "partial vassal row fails closed",
			form: url.Values{
				"name":              {"partial-row"},
				"vassal_role":       {"claude", "codex"},
				"vassal_workload":   {"claude-worker", ""},
				"vassal_activation": {"always-on", "on-mention"},
			},
			wantErr:        true,
			wantErrContain: "role and workload",
		},
		{
			name: "mismatched vassal arrays fail closed",
			form: url.Values{
				"name":              {"mismatch-rows"},
				"vassal_role":       {"claude", "codex"},
				"vassal_workload":   {"claude-worker"},
				"vassal_activation": {"always-on", "on-mention"},
			},
			wantErr:        true,
			wantErrContain: "mismatched",
		},
		{
			name: "duplicate vassal role fails closed",
			form: url.Values{
				"name":              {"dup-role"},
				"vassal_role":       {"claude", "claude"},
				"vassal_workload":   {"claude-worker", "codex-worker"},
				"vassal_activation": {"always-on", "on-mention"},
			},
			wantErr:        true,
			wantErrContain: "duplicate vassal role",
		},
		{
			name: "reserved vassal role fails closed",
			form: url.Values{
				"name":              {"reserved-role"},
				"vassal_role":       {"@operator"},
				"vassal_workload":   {"claude-worker"},
				"vassal_activation": {"always-on"},
			},
			wantErr:        true,
			wantErrContain: "invalid vassal role",
		},
		{
			name: "invalid activation fails closed",
			form: url.Values{
				"name":              {"bad-act"},
				"vassal_role":       {"claude"},
				"vassal_workload":   {"claude-worker"},
				"vassal_activation": {"always-off"},
			},
			wantErr:        true,
			wantErrContain: "invalid vassal activation",
		},
		{
			name: "unknown workload fails closed without wedging subsequent reload",
			form: url.Values{
				"name":              {"bad-wl-council"},
				"vassal_role":       {"claude"},
				"vassal_workload":   {"does-not-exist"},
				"vassal_activation": {"always-on"},
			},
			wantErr:        true,
			wantErrContain: "does-not-exist",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := newUITestStore(t)
			dir, _, _, getBundle, reload := seedStudioManifests(t, st)
			broker := conclave.NewBroker(conclave.Options{Store: st, GetBundle: getBundle})
			dash, err := NewDashboard(Options{
				Store:         st,
				Conclave:      broker,
				GetBundle:     getBundle,
				Reload:        reload,
				ManifestsPath: dir,
			})
			if err != nil {
				t.Fatalf("NewDashboard: %v", err)
			}
			mux := http.NewServeMux()
			dash.RegisterRoutes(mux)

			req := httptest.NewRequest(http.MethodPost, "/ui/studio/conclave", strings.NewReader(tc.form.Encode()))
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, req)

			if rec.Code != http.StatusSeeOther {
				t.Fatalf("expected 303, got %d: %s", rec.Code, rec.Body.String())
			}
			loc := rec.Header().Get("Location")

			if tc.wantErr {
				if !strings.Contains(loc, "/studio?err=") || strings.Contains(loc, "error=") {
					t.Fatalf("expected /studio?err=... redirect, got %q", loc)
				}
				pageRec := httptest.NewRecorder()
				mux.ServeHTTP(pageRec, httptest.NewRequest(http.MethodGet, loc, nil))
				if pageRec.Code != http.StatusOK {
					t.Fatalf("GET %s status = %d", loc, pageRec.Code)
				}
				if !strings.Contains(pageRec.Body.String(), "banner-err") || !strings.Contains(pageRec.Body.String(), tc.wantErrContain) {
					t.Errorf("expected error banner containing %q on %s, got:\n%s", tc.wantErrContain, loc, pageRec.Body.String())
				}
				if err := reload(); err != nil {
					t.Fatalf("manifest reload wedged after failed create: %v", err)
				}
				if _, exists := getBundle().Conclaves[strings.TrimSpace(tc.form.Get("name"))]; exists {
					t.Errorf("failed create persisted conclave %q into bundle", tc.form.Get("name"))
				}
				return
			}

			if !strings.Contains(loc, "/studio?notice=") {
				t.Fatalf("expected /studio?notice=... redirect, got %q", loc)
			}
			cName := tc.form.Get("name")
			cc, ok := getBundle().Conclaves[cName]
			if !ok {
				t.Fatalf("expected conclave %q in reloaded bundle", cName)
			}
			if len(cc.Spec.Vassals) != tc.wantVassals {
				t.Fatalf("vassal count = %d, want %d (%+v)", len(cc.Spec.Vassals), tc.wantVassals, cc.Spec.Vassals)
			}
			if cc.Spec.Wards.MaxMessages != tc.wantMaxMsgs || cc.Spec.Wards.MaxPingPong != tc.wantMaxPP {
				t.Fatalf("wards = %+v, want maxMessages=%d maxPingPong=%d", cc.Spec.Wards, tc.wantMaxMsgs, tc.wantMaxPP)
			}

			// Verify /conclaves lists the template and opening it renders all vassals.
			listRec := httptest.NewRecorder()
			mux.ServeHTTP(listRec, httptest.NewRequest(http.MethodGet, "/conclaves", nil))
			wantOption := fmt.Sprintf("%s (%d vassals)", cName, tc.wantVassals)
			if !strings.Contains(listRec.Body.String(), wantOption) {
				t.Fatalf("GET /conclaves missing %q", wantOption)
			}

			openForm := url.Values{"template": {cName}, "title": {"Council Run"}}
			openReq := httptest.NewRequest(http.MethodPost, "/ui/conclaves", strings.NewReader(openForm.Encode()))
			openReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			openRec := httptest.NewRecorder()
			mux.ServeHTTP(openRec, openReq)
			if openRec.Code != http.StatusSeeOther {
				t.Fatalf("POST /ui/conclaves code = %d", openRec.Code)
			}
			detailLoc := openRec.Header().Get("Location")
			detailRec := httptest.NewRecorder()
			mux.ServeHTTP(detailRec, httptest.NewRequest(http.MethodGet, detailLoc, nil))
			if detailRec.Code != http.StatusOK {
				t.Fatalf("GET %s code = %d", detailLoc, detailRec.Code)
			}
			for _, v := range cc.Spec.Vassals {
				if !strings.Contains(detailRec.Body.String(), "@"+v.Role) {
					t.Errorf("conclave page missing vassal @%s", v.Role)
				}
			}
		})
	}
}

func TestConclaveRelicUploadFormAndHandler(t *testing.T) {
	t.Run("conclave detail page renders Relic upload form", func(t *testing.T) {
		st := newUITestStore(t)
		now := time.Now().UTC()
		if err := st.CreateConclave(context.Background(), domain.ConclaveSession{
			ID: "cc-form", Title: "Relic Room", State: domain.ConclaveStateActive, CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatal(err)
		}
		dash, err := NewDashboard(Options{Store: st, Conclave: conclave.NewBroker(conclave.Options{Store: st})})
		if err != nil {
			t.Fatal(err)
		}
		mux := http.NewServeMux()
		dash.RegisterRoutes(mux)

		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/conclaves/cc-form", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("GET /conclaves/cc-form status = %d", rec.Code)
		}
		body := rec.Body.String()
		for _, want := range []string{
			`action="/ui/conclaves/cc-form/relics"`,
			`name="name"`,
			`name="content"`,
			`name="file"`,
			`name="content_type"`,
		} {
			if !strings.Contains(body, want) {
				t.Errorf("conclave page missing %q", want)
			}
		}
	})

	type uploadRequest struct {
		multipart   bool
		name        string
		content     string
		contentType string
		fileName    string
		fileBody    []byte
		rawCT       string
		rawBody     string
	}

	syntheticToken := "sc_task_" + strings.Repeat("0", 24)
	cases := []struct {
		name           string
		sessionState   domain.ConclaveState
		nilBroker      bool
		req            uploadRequest
		wantErr        bool
		wantErrContain string
		wantName       string
		wantCT         string
		wantBody       string
		secretName     bool
	}{
		{
			name:         "urlencoded relic upload succeeds and displays on conclave page",
			sessionState: domain.ConclaveStateActive,
			req: uploadRequest{
				name:        "notes.md",
				content:     "Line one\nLine two",
				contentType: "text/markdown",
			},
			wantName: "notes.md",
			wantCT:   "text/markdown",
			wantBody: "Line one\nLine two",
		},
		{
			name:         "multipart text upload succeeds",
			sessionState: domain.ConclaveStateActive,
			req: uploadRequest{
				multipart:   true,
				name:        "diff.patch",
				content:     "--- a/foo.go\n+++ b/foo.go",
				contentType: "text/x-diff",
			},
			wantName: "diff.patch",
			wantCT:   "text/x-diff",
			wantBody: "--- a/foo.go\n+++ b/foo.go",
		},
		{
			name:         "multipart file upload falls back to filename",
			sessionState: domain.ConclaveStateActive,
			req: uploadRequest{
				multipart:   true,
				fileName:    "evidence.log",
				fileBody:    []byte("stack trace ok"),
				contentType: "text/plain",
			},
			wantName: "evidence.log",
			wantCT:   "text/plain",
			wantBody: "stack trace ok",
		},
		{
			name:         "secret in filename rejected",
			sessionState: domain.ConclaveStateActive,
			req: uploadRequest{
				multipart: true,
				fileName:  syntheticToken + ".log",
				fileBody:  []byte("review notes"),
			},
			wantErr:        true,
			wantErrContain: "invalid relic name",
			secretName:     true,
		},
		{
			name:         "secret in explicit name rejected",
			sessionState: domain.ConclaveStateActive,
			req: uploadRequest{
				name:    syntheticToken + ".log",
				content: "review notes",
			},
			wantErr:        true,
			wantErrContain: "invalid relic name",
			secretName:     true,
		},
		{
			name:         "empty name and content shows error on conclave page",
			sessionState: domain.ConclaveStateActive,
			req: uploadRequest{
				multipart: true,
			},
			wantErr:        true,
			wantErrContain: "relic name and content are required",
		},
		{
			name:         "closed conclave upload shows error on conclave page",
			sessionState: domain.ConclaveStateCompleted,
			req: uploadRequest{
				name:    "late.txt",
				content: "too late",
			},
			wantErr:        true,
			wantErrContain: "conclave is closed",
		},
		{
			name:         "paused conclave upload shows error on conclave page",
			sessionState: domain.ConclaveStateInputRequired,
			req: uploadRequest{
				name:    "paused.txt",
				content: "wait",
			},
			wantErr:        true,
			wantErrContain: "conclave paused",
		},
		{
			name:         "nil broker shows error on conclave page",
			sessionState: domain.ConclaveStateActive,
			nilBroker:    true,
			req: uploadRequest{
				name:    "disabled.txt",
				content: "disabled",
			},
			wantErr:        true,
			wantErrContain: "conclave disabled",
		},
		{
			name:         "path traversal relic name rejected",
			sessionState: domain.ConclaveStateActive,
			req: uploadRequest{
				name:    "../secret.txt",
				content: "bad path",
			},
			wantErr:        true,
			wantErrContain: "invalid relic name",
		},
		{
			name:         "invalid UTF-8 binary file rejected",
			sessionState: domain.ConclaveStateActive,
			req: uploadRequest{
				multipart: true,
				name:      "bin.dat",
				fileName:  "bin.dat",
				fileBody:  []byte{0xff, 0xfe, 0x00, 0x01},
			},
			wantErr:        true,
			wantErrContain: "UTF-8",
		},
		{
			name:         "both file and content rejected",
			sessionState: domain.ConclaveStateActive,
			req: uploadRequest{
				multipart: true,
				name:      "both.txt",
				content:   "text",
				fileName:  "both.txt",
				fileBody:  []byte("file"),
			},
			wantErr:        true,
			wantErrContain: "not both",
		},
		{
			name:         "oversized relic upload rejected",
			sessionState: domain.ConclaveStateActive,
			req: uploadRequest{
				multipart: true,
				name:      "huge.txt",
				fileName:  "huge.txt",
				fileBody:  bytes.Repeat([]byte("a"), (1<<20)+64),
			},
			wantErr:        true,
			wantErrContain: "maximum size",
		},
		{
			name:         "unsupported content type rejected",
			sessionState: domain.ConclaveStateActive,
			req: uploadRequest{
				name:        "raw.bin",
				content:     "hello",
				contentType: "application/octet-stream",
			},
			wantErr:        true,
			wantErrContain: "unsupported relic content type",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := newUITestStore(t)
			now := time.Now().UTC()
			if err := st.CreateConclave(context.Background(), domain.ConclaveSession{
				ID: "cc-1", Title: "Upload Test", State: tc.sessionState, CreatedAt: now, UpdatedAt: now,
			}); err != nil {
				t.Fatal(err)
			}
			var broker *conclave.Broker
			if !tc.nilBroker {
				broker = conclave.NewBroker(conclave.Options{Store: st})
			}
			dash, err := NewDashboard(Options{Store: st, Conclave: broker})
			if err != nil {
				t.Fatal(err)
			}
			mux := http.NewServeMux()
			dash.RegisterRoutes(mux)

			var reqBody io.Reader
			var ctHeader string
			if tc.req.rawCT != "" {
				reqBody = strings.NewReader(tc.req.rawBody)
				ctHeader = tc.req.rawCT
			} else if tc.req.multipart {
				var buf bytes.Buffer
				mw := multipart.NewWriter(&buf)
				if tc.req.name != "" {
					_ = mw.WriteField("name", tc.req.name)
				}
				if tc.req.content != "" {
					_ = mw.WriteField("content", tc.req.content)
				}
				if tc.req.contentType != "" {
					_ = mw.WriteField("content_type", tc.req.contentType)
				}
				if tc.req.fileName != "" {
					h := make(textproto.MIMEHeader)
					h.Set("Content-Disposition", fmt.Sprintf(`form-data; name="file"; filename=%q`, tc.req.fileName))
					h.Set("Content-Type", "application/octet-stream")
					fw, err := mw.CreatePart(h)
					if err != nil {
						t.Fatal(err)
					}
					_, _ = fw.Write(tc.req.fileBody)
				}
				_ = mw.Close()
				reqBody = &buf
				ctHeader = mw.FormDataContentType()
			} else {
				form := url.Values{}
				if tc.req.name != "" {
					form.Set("name", tc.req.name)
				}
				if tc.req.content != "" {
					form.Set("content", tc.req.content)
				}
				if tc.req.contentType != "" {
					form.Set("content_type", tc.req.contentType)
				}
				reqBody = strings.NewReader(form.Encode())
				ctHeader = "application/x-www-form-urlencoded"
			}

			req := httptest.NewRequest(http.MethodPost, "/ui/conclaves/cc-1/relics", reqBody)
			req.Header.Set("Content-Type", ctHeader)
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, req)

			if rec.Code != http.StatusSeeOther {
				t.Fatalf("expected 303, got %d: %s", rec.Code, rec.Body.String())
			}
			loc := rec.Header().Get("Location")

			if tc.secretName {
				if strings.Contains(loc, syntheticToken) {
					t.Error("redirect contains a task token")
				}
				relics, err := st.ListConclaveRelics(context.Background(), "cc-1")
				if err != nil || len(relics) != 0 {
					t.Errorf("secret name upload stored %d relics, err = %v", len(relics), err)
				}
				audit, err := st.ListAudit(context.Background(), 10)
				if err != nil {
					t.Fatal(err)
				}
				for _, record := range audit {
					if strings.Contains(record.DetailJSON, syntheticToken) {
						t.Error("audit record contains a task token")
					}
				}
				if t.Failed() {
					return
				}
			}
			if tc.wantErr {
				if !strings.Contains(loc, "/conclaves/cc-1?err=") || strings.Contains(loc, "error=") {
					t.Fatalf("expected ?err= redirect, got %q", loc)
				}
				pageRec := httptest.NewRecorder()
				mux.ServeHTTP(pageRec, httptest.NewRequest(http.MethodGet, loc, nil))
				if pageRec.Code != http.StatusOK {
					t.Fatalf("GET %s status = %d", loc, pageRec.Code)
				}
				pageBody := pageRec.Body.String()
				if !strings.Contains(pageBody, "banner-err") || !strings.Contains(pageBody, tc.wantErrContain) {
					t.Errorf("conclave page did not show error banner with %q at %q; body:\n%s", tc.wantErrContain, loc, pageBody)
				}
				relics, err := st.ListConclaveRelics(context.Background(), "cc-1")
				if err != nil || len(relics) != 0 {
					t.Errorf("expected 0 stored relics on failure, got %+v (err=%v)", relics, err)
				}
				return
			}

			if !strings.Contains(loc, "/conclaves/cc-1?notice=") {
				t.Fatalf("expected notice redirect, got %q", loc)
			}
			relic, err := st.GetConclaveRelic(context.Background(), "cc-1", tc.wantName)
			if err != nil {
				t.Fatalf("GetConclaveRelic: %v", err)
			}
			if relic.ContentType != tc.wantCT || relic.Body != tc.wantBody || relic.AuthorRole != "operator" {
				t.Fatalf("stored relic = %+v, want ct=%q body=%q author=operator", relic, tc.wantCT, tc.wantBody)
			}

			pageRec := httptest.NewRecorder()
			mux.ServeHTTP(pageRec, httptest.NewRequest(http.MethodGet, loc, nil))
			if pageRec.Code != http.StatusOK {
				t.Fatalf("GET %s status = %d", loc, pageRec.Code)
			}
			pageBody := pageRec.Body.String()
			unescaped := html.UnescapeString(pageBody)
			if !strings.Contains(pageBody, "banner-ok") || !strings.Contains(unescaped, tc.wantName) || !strings.Contains(unescaped, tc.wantBody) {
				t.Errorf("conclave page missing notice or relic %q", tc.wantName)
			}
		})
	}
}

func TestUIErrorQueryParameterConsistency(t *testing.T) {
	cases := []struct {
		name           string
		method         string
		target         string
		form           url.Values
		wantRedirectTo string
		wantErrText    string
	}{
		{
			name:           "save learning error renders on conclave page via err",
			method:         http.MethodPost,
			target:         "/ui/conclaves/cc-1/learning",
			form:           url.Values{"scope": {"global"}, "key": {"conclave-msg-1"}, "rule": {"   "}},
			wantRedirectTo: "/conclaves/cc-1?err=",
			wantErrText:    "learning rule is required",
		},
		{
			name:           "upload relic error renders on conclave page via err",
			method:         http.MethodPost,
			target:         "/ui/conclaves/cc-1/relics",
			form:           url.Values{"name": {""}, "content": {""}},
			wantRedirectTo: "/conclaves/cc-1?err=",
			wantErrText:    "relic name and content are required",
		},
		{
			name:           "post decree error renders on conclave page via err",
			method:         http.MethodPost,
			target:         "/ui/conclaves/cc-1/decree",
			form:           url.Values{"to": {"all"}, "body": {"go"}},
			wantRedirectTo: "/conclaves/cc-1?err=",
			wantErrText:    "conclave broker unavailable",
		},
		{
			name:           "close conclave error renders on conclave page via err",
			method:         http.MethodPost,
			target:         "/ui/conclaves/cc-1/close",
			form:           url.Values{"state": {"completed"}},
			wantRedirectTo: "/conclaves/cc-1?err=",
			wantErrText:    "conclave broker unavailable",
		},
		{
			name:           "open conclave error renders on conclaves page via err",
			method:         http.MethodPost,
			target:         "/ui/conclaves",
			form:           url.Values{"template": {"missing"}, "title": {"x"}},
			wantRedirectTo: "/conclaves?err=",
			wantErrText:    "conclave broker unavailable",
		},
		{
			name:           "run schedule error renders on schedules page via err",
			method:         http.MethodPost,
			target:         "/ui/schedules/nightly/run",
			form:           url.Values{},
			wantRedirectTo: "/schedules?err=",
			wantErrText:    "scheduler unavailable",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := newUITestStore(t)
			now := time.Now().UTC()
			if err := st.CreateConclave(context.Background(), domain.ConclaveSession{
				ID: "cc-1", Title: "Err Check", State: domain.ConclaveStateActive, CreatedAt: now, UpdatedAt: now,
			}); err != nil {
				t.Fatal(err)
			}
			dash, err := NewDashboard(Options{
				Store:    st,
				Conclave: conclave.NewBroker(conclave.Options{Store: st}),
			})
			if err != nil {
				t.Fatal(err)
			}
			// For unavailable broker checks, exercise a dashboard without broker.
			if tc.wantErrText == "conclave broker unavailable" {
				dash, err = NewDashboard(Options{Store: st})
				if err != nil {
					t.Fatal(err)
				}
			}
			mux := http.NewServeMux()
			dash.RegisterRoutes(mux)

			req := httptest.NewRequest(tc.method, tc.target, strings.NewReader(tc.form.Encode()))
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, req)

			if rec.Code != http.StatusSeeOther {
				t.Fatalf("expected 303, got %d", rec.Code)
			}
			loc := rec.Header().Get("Location")
			if !strings.HasPrefix(loc, tc.wantRedirectTo) || strings.Contains(loc, "error=") {
				t.Fatalf("Location = %q, want prefix %q without error=", loc, tc.wantRedirectTo)
			}

			pageRec := httptest.NewRecorder()
			mux.ServeHTTP(pageRec, httptest.NewRequest(http.MethodGet, loc, nil))
			if pageRec.Code != http.StatusOK {
				t.Fatalf("GET %s status = %d", loc, pageRec.Code)
			}
			if !strings.Contains(pageRec.Body.String(), tc.wantErrText) {
				t.Errorf("page %s did not render error %q; body:\n%s", loc, tc.wantErrText, pageRec.Body.String())
			}
		})
	}
}

func TestStudioConcurrentManifestAppendAndSingleFileRollback(t *testing.T) {
	t.Run("concurrent studio creates do not overwrite each other", func(t *testing.T) {
		st := newUITestStore(t)
		dir, _, _, getBundle, reload := seedStudioManifests(t, st)
		dash, err := NewDashboard(Options{
			Store:         st,
			GetBundle:     getBundle,
			Reload:        reload,
			ManifestsPath: dir,
		})
		if err != nil {
			t.Fatal(err)
		}
		mux := http.NewServeMux()
		dash.RegisterRoutes(mux)

		var wg sync.WaitGroup
		errs := make(chan error, 2)
		for _, cname := range []string{"council-alpha", "council-beta"} {
			wg.Add(1)
			go func(name string) {
				defer wg.Done()
				form := url.Values{
					"name":              {name},
					"description":       {"concurrent council"},
					"vassal_role":       {"claude", "codex", "agy"},
					"vassal_workload":   {"claude-worker", "codex-worker", "agy-worker"},
					"vassal_activation": {"always-on", "on-mention", "on-mention"},
				}
				req := httptest.NewRequest(http.MethodPost, "/ui/studio/conclave", strings.NewReader(form.Encode()))
				req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
				rec := httptest.NewRecorder()
				mux.ServeHTTP(rec, req)
				if rec.Code != http.StatusSeeOther || !strings.Contains(rec.Header().Get("Location"), "?notice=") {
					errs <- fmt.Errorf("%s unexpected response code=%d loc=%q", name, rec.Code, rec.Header().Get("Location"))
				}
			}(cname)
		}
		wg.Wait()
		close(errs)
		for e := range errs {
			t.Fatal(e)
		}
		b := getBundle()
		for _, cname := range []string{"council-alpha", "council-beta"} {
			cc, ok := b.Conclaves[cname]
			if !ok || len(cc.Spec.Vassals) != 3 {
				t.Fatalf("expected 3-vassal conclave %q in bundle, got ok=%v cc=%+v", cname, ok, cc)
			}
		}
	})

	t.Run("single manifest file rolls back on reload error", func(t *testing.T) {
		st := newUITestStore(t)
		dir, _, _, _, _ := seedStudioManifests(t, st)
		singleFile := filepath.Join(dir, "base.yaml")
		origRaw, err := os.ReadFile(singleFile)
		if err != nil {
			t.Fatal(err)
		}

		var mu sync.RWMutex
		cur, err := config.LoadPath(singleFile)
		if err != nil {
			t.Fatal(err)
		}
		getBundle := func() *config.Bundle {
			mu.RLock()
			defer mu.RUnlock()
			return cur
		}
		reload := func() error {
			next, err := config.LoadPath(singleFile)
			if err != nil {
				return err
			}
			mu.Lock()
			cur = next
			mu.Unlock()
			return nil
		}

		dash, err := NewDashboard(Options{
			Store:         st,
			GetBundle:     getBundle,
			Reload:        reload,
			ManifestsPath: singleFile,
		})
		if err != nil {
			t.Fatal(err)
		}
		mux := http.NewServeMux()
		dash.RegisterRoutes(mux)

		badForm := url.Values{
			"name":              {"bad-council"},
			"vassal_role":       {"claude"},
			"vassal_workload":   {"missing-workload"},
			"vassal_activation": {"always-on"},
		}
		badReq := httptest.NewRequest(http.MethodPost, "/ui/studio/conclave", strings.NewReader(badForm.Encode()))
		badReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		badRec := httptest.NewRecorder()
		mux.ServeHTTP(badRec, badReq)
		if !strings.Contains(badRec.Header().Get("Location"), "/studio?err=") {
			t.Fatalf("expected error redirect, got %q", badRec.Header().Get("Location"))
		}

		afterBadRaw, err := os.ReadFile(singleFile)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(afterBadRaw, origRaw) {
			t.Fatalf("single manifest file not rolled back on failed reload")
		}

		goodForm := url.Values{
			"name":              {"good-council"},
			"vassal_role":       {"claude", "codex", "agy"},
			"vassal_workload":   {"claude-worker", "codex-worker", "agy-worker"},
			"vassal_activation": {"always-on", "on-mention", "on-mention"},
		}
		goodReq := httptest.NewRequest(http.MethodPost, "/ui/studio/conclave", strings.NewReader(goodForm.Encode()))
		goodReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		goodRec := httptest.NewRecorder()
		mux.ServeHTTP(goodRec, goodReq)
		if !strings.Contains(goodRec.Header().Get("Location"), "/studio?notice=") {
			t.Fatalf("expected retry after rollback to succeed, got %q", goodRec.Header().Get("Location"))
		}
	})
}
