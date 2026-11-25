package telemetry

import (
	"bufio"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/shadewright-labs/sablecourt/internal/domain"
	"github.com/shadewright-labs/sablecourt/internal/store/sqlite"
)

// TestMetricsReferenceListsEveryMetric fails when the metric names, types or
// label names in docs/reference/metrics.md differ from what GET /metrics
// writes. The store holds a task, a Conclave and an audit row, so every
// labelled series has a sample.
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
	now := time.Now().UTC()
	if err := st.UpsertWorkload(ctx, domain.WorkloadRecord{Name: "w", Zone: domain.ZoneRuntime, Class: domain.ClassLongLivedAgent, AgentCardJSON: "{}"}); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateTask(ctx, domain.Task{ID: "t", IdempotencyKey: "t", WorkloadName: "w", State: domain.TaskStateWorking, InputJSON: "{}", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateConclave(ctx, domain.ConclaveSession{ID: "c", TemplateName: "room", Title: "room", State: domain.ConclaveStateActive, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := st.AppendAudit(ctx, domain.AuditRecord{Category: "test", Action: "test", Outcome: "allowed", DetailJSON: "{}", CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	MetricsHandler(st).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	code := map[string]string{}
	codeLabels := map[string]map[string]bool{}
	sample := regexp.MustCompile(`^([a-z_]+)(?:\{(.*)\})? `)
	labelName := regexp.MustCompile(`([a-z_]+)="`)
	for _, line := range strings.Split(rec.Body.String(), "\n") {
		if rest, ok := strings.CutPrefix(line, "# TYPE "); ok {
			name, typ, _ := strings.Cut(rest, " ")
			code[name] = typ
			continue
		}
		m := sample.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		if codeLabels[m[1]] == nil {
			codeLabels[m[1]] = map[string]bool{}
		}
		for _, l := range labelName.FindAllStringSubmatch(m[2], -1) {
			codeLabels[m[1]][l[1]] = true
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
	docLabels := map[string]string{}
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
			if len(cells) > 2 {
				docLabels[name] = labelsCell(cells[2])
			}
		}
	}
	for name, typ := range code {
		switch got, ok := doc[name]; {
		case !ok:
			t.Errorf("metric %s (%s) missing from metrics.md", name, typ)
		case got != typ:
			t.Errorf("metric %s has type %q in metrics.md, the handler writes %q", name, got, typ)
		}
		labels, ok := codeLabels[name]
		if !ok {
			t.Errorf("metric %s has no sample; seed the store so its labels show", name)
			continue
		}
		if want := joinLabels(labels); docLabels[name] != want {
			t.Errorf("metric %s has labels %q in metrics.md, the handler writes %q", name, docLabels[name], want)
		}
	}
	for name := range doc {
		if _, ok := code[name]; !ok {
			t.Errorf("metric %s is in metrics.md but the handler does not write it", name)
		}
	}
}

// labelsCell reads the label names of a Labels cell: the names in backticks
// before any parenthesis, or "none".
func labelsCell(cell string) string {
	before, _, _ := strings.Cut(cell, "(")
	if strings.TrimSpace(before) == "none" {
		return "none"
	}
	names := map[string]bool{}
	for _, m := range regexp.MustCompile("`([a-z_]+)`").FindAllStringSubmatch(before, -1) {
		names[m[1]] = true
	}
	return joinLabels(names)
}

func joinLabels(names map[string]bool) string {
	if len(names) == 0 {
		return "none"
	}
	out := make([]string, 0, len(names))
	for n := range names {
		out = append(out, n)
	}
	sort.Strings(out)
	return strings.Join(out, ", ")
}
