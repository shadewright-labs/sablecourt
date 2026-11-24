package ui

import (
	"bytes"
	"context"
	"html/template"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/shadewright-labs/sablecourt/internal/conclave"
	"github.com/shadewright-labs/sablecourt/internal/domain"
)

func TestStudioVassalRowsBrowser(t *testing.T) {
	runBrowserCheck(t, "studio", func() string {
		st := newUITestStore(t)
		_, _, _, getBundle, _ := seedStudioManifests(t, st)
		return renderBrowserPage(t, Options{Store: st, GetBundle: getBundle}, "/studio")
	})
}

func TestConclaveHeartbeatAfterRelicDraftBrowser(t *testing.T) {
	runBrowserCheck(t, "conclave", func() string {
		st := newUITestStore(t)
		now := time.Now().UTC()
		if err := st.CreateConclave(t.Context(), domain.ConclaveSession{
			ID: "cc-browser", Title: "Browser Test", State: domain.ConclaveStateActive, CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatal(err)
		}
		page := renderBrowserPage(t, Options{Store: st, Conclave: conclave.NewBroker(conclave.Options{Store: st})}, "/conclaves/cc-browser")
		return strings.Replace(page, "<head>", `<head><script>
			window.EventSource = class extends EventTarget {
				constructor() { super(); window.testStream = this; }
			};
		</script>`, 1)
	})
}

func renderBrowserPage(t *testing.T, opts Options, path string) string {
	t.Helper()
	dash, err := NewDashboard(opts)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	dash.RegisterRoutes(mux)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s returned %d", path, rec.Code)
	}
	return rec.Body.String()
}

func runBrowserCheck(t *testing.T, name string, page func() string) {
	t.Helper()
	// Set the executable for headless runs, or export fixtures to open in a browser.
	chrome := os.Getenv("SABLECOURT_UI_CHROME")
	dir := os.Getenv("SABLECOURT_UI_BROWSER_FIXTURES")
	if chrome == "" && dir == "" {
		t.Skip("set SABLECOURT_UI_CHROME to the Chrome or Chromium executable")
	}
	if dir == "" {
		dir = t.TempDir()
	}
	tmpl, err := template.ParseFiles("testdata/browser.html")
	if err != nil {
		t.Fatal(err)
	}
	var fixture bytes.Buffer
	if err := tmpl.Execute(&fixture, struct{ Name, Page string }{name, page()}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, name+".html")
	if err := os.WriteFile(path, fixture.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	if chrome == "" {
		t.Skipf("open exported browser fixture %s", path)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, chrome, "--headless", "--no-first-run", "--disable-background-networking",
		"--user-data-dir="+t.TempDir(), "--virtual-time-budget=2500", "--dump-dom", path)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("run headless browser: %v", err)
	}
	if !bytes.Contains(out, []byte(`<p id="result">PASS</p>`)) {
		for line := range strings.Lines(string(out)) {
			if strings.Contains(line, `<p id="result">`) {
				t.Fatal(strings.TrimSpace(line))
			}
		}
		t.Fatal("browser result is missing")
	}
}
