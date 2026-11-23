package ui

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/shadewright-labs/sablecourt/internal/domain"
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
	if !strings.Contains(body, "/assets/logo-horizontal-dark.png") || !strings.Contains(body, "/assets/favicon.png") || !strings.Contains(body, "triage-agent") {
		t.Fatalf("expected branded dashboard HTML, got:\n%s", body)
	}

	favRec := httptest.NewRecorder()
	mux.ServeHTTP(favRec, httptest.NewRequest(http.MethodGet, "/favicon.ico", nil))
	if favRec.Code != http.StatusOK || favRec.Header().Get("Content-Type") != "image/png" || favRec.Body.Len() == 0 {
		t.Fatalf("expected PNG favicon, got code=%d ct=%q len=%d", favRec.Code, favRec.Header().Get("Content-Type"), favRec.Body.Len())
	}

	logoRec := httptest.NewRecorder()
	mux.ServeHTTP(logoRec, httptest.NewRequest(http.MethodGet, "/assets/logo-horizontal-dark.png", nil))
	if logoRec.Code != http.StatusOK || logoRec.Body.Len() == 0 {
		t.Fatalf("expected embedded logo asset, got code=%d len=%d", logoRec.Code, logoRec.Body.Len())
	}
}
