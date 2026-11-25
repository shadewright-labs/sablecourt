package telemetry

import (
	"bufio"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/shadewright-labs/sablecourt/internal/store/sqlite"
)

// TestMetricsReferenceListsEveryMetric fails when the metric names and types
// in docs/reference/metrics.md differ from the "# TYPE" lines that
// GET /metrics writes.
func TestMetricsReferenceListsEveryMetric(t *testing.T) {
	ctx := context.Background()
	st, err := sqlite.Open(filepath.Join(t.TempDir(), "sablecourt.db"))
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	MetricsHandler(st).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	code := map[string]string{}
	for _, line := range strings.Split(rec.Body.String(), "\n") {
		if rest, ok := strings.CutPrefix(line, "# TYPE "); ok {
			name, typ, _ := strings.Cut(rest, " ")
			code[name] = typ
		}
	}
	if len(code) == 0 {
		t.Fatal("no metrics written")
	}

	f, err := os.Open(filepath.Join("..", "..", "docs", "reference", "metrics.md"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	doc := map[string]string{}
	inTable := false
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if !strings.HasPrefix(line, "|") {
			inTable = false
			continue
		}
		cells := strings.Split(strings.Trim(line, "|"), "|")
		for i := range cells {
			cells[i] = strings.TrimSpace(cells[i])
		}
		switch {
		case cells[0] == "Name":
			inTable = true
		case !inTable || strings.HasPrefix(cells[0], "---") || len(cells) < 2:
		default:
			name := strings.Trim(cells[0], "`")
			if _, dup := doc[name]; dup {
				t.Errorf("metric %s listed twice in metrics.md", name)
			}
			doc[name] = cells[1]
		}
	}
	for name, typ := range code {
		switch got, ok := doc[name]; {
		case !ok:
			t.Errorf("metric %s (%s) missing from metrics.md", name, typ)
		case got != typ:
			t.Errorf("metric %s has type %q in metrics.md, the handler writes %q", name, got, typ)
		}
	}
	for name := range doc {
		if _, ok := code[name]; !ok {
			t.Errorf("metric %s is in metrics.md but the handler does not write it", name)
		}
	}
}
