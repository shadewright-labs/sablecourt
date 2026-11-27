package config

import (
	"bufio"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/shadewright-labs/sablecourt/internal/domain"
)

// The manifests reference (docs/reference/manifests.md) has one field table
// per kind that the loader's switch accepts, plus "Common fields" for the type and object metadata. Each table
// has the columns Field, Type, Required and Meaning. This test checks the
// first three against the manifest types and the loader:
//
//   - Field and Type: the set of field paths and their types must equal the
//     JSON field paths of the Go type of that kind (reflection).
//   - Required: for each kind there are example documents that load. The
//     test deletes the field from each example and loads it again. "yes"
//     means every deletion fails, "no" means every deletion loads, and
//     "conditional" means at least one fails and at least one loads. Every
//     field must appear in at least one example, so a deletion is never a
//     no-op for all of them.

var manifestKinds = map[string]reflect.Type{
	"Workload": reflect.TypeOf(domain.Workload{}),
	"Policy":   reflect.TypeOf(domain.Policy{}),
	"Budget":   reflect.TypeOf(domain.Budget{}),
	"Workflow": reflect.TypeOf(domain.Workflow{}),
	"Schedule": reflect.TypeOf(domain.Schedule{}),
	"Conclave": reflect.TypeOf(domain.Conclave{}),
}

// loaderKinds returns the kind names the loader accepts, read from the case
// labels of the switch on meta.Kind in loader.go, with each domain.Kind
// constant resolved from internal/domain.
func loaderKinds(t *testing.T) []string {
	t.Helper()
	fset := token.NewFileSet()
	consts := map[string]string{}
	domainDir := filepath.Join("..", "domain")
	entries, err := os.ReadDir(domainDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(domainDir, e.Name()), nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			vs, ok := n.(*ast.ValueSpec)
			if !ok {
				return true
			}
			for i, name := range vs.Names {
				if i < len(vs.Values) {
					if lit, ok := vs.Values[i].(*ast.BasicLit); ok && lit.Kind == token.STRING {
						consts[name.Name], _ = strconv.Unquote(lit.Value)
					}
				}
			}
			return true
		})
	}
	f, err := parser.ParseFile(fset, "loader.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var kinds []string
	ast.Inspect(f, func(n ast.Node) bool {
		sw, ok := n.(*ast.SwitchStmt)
		if !ok {
			return true
		}
		tag, ok := sw.Tag.(*ast.SelectorExpr)
		if !ok || tag.Sel.Name != "Kind" {
			return true
		}
		if x, ok := tag.X.(*ast.Ident); !ok || x.Name != "meta" {
			return true
		}
		for _, st := range sw.Body.List {
			for _, e := range st.(*ast.CaseClause).List {
				sel, ok := e.(*ast.SelectorExpr)
				if !ok {
					t.Errorf("loader.go: case %T on meta.Kind is not a domain constant", e)
					continue
				}
				v, ok := consts[sel.Sel.Name]
				if !ok {
					t.Errorf("loader.go: cannot resolve %s", sel.Sel.Name)
					continue
				}
				kinds = append(kinds, v)
			}
		}
		return false
	})
	if len(kinds) == 0 {
		t.Fatal("loader.go: no switch on meta.Kind found")
	}
	sort.Strings(kinds)
	return kinds
}

const commonSection = "Common fields"

func isCommon(path string) bool {
	return path == "apiVersion" || path == "kind" || strings.HasPrefix(path, "metadata.")
}

// fieldTypes returns the documented type name of every leaf field and every
// list of objects below t, keyed by JSON path.
func fieldTypes(t reflect.Type) map[string]string {
	out := map[string]string{}
	var walk func(t reflect.Type, prefix string)
	walk = func(t reflect.Type, prefix string) {
		for i := range t.NumField() {
			f := t.Field(i)
			tag := f.Tag.Get("json")
			name, opts, _ := strings.Cut(tag, ",")
			if name == "-" {
				continue
			}
			ft := f.Type
			if ft.Kind() == reflect.Pointer {
				ft = ft.Elem()
			}
			if f.Anonymous && (strings.Contains(opts, "inline") || name == "") {
				walk(ft, prefix)
				continue
			}
			if name == "" {
				name = f.Name
			}
			path := prefix + name
			switch ft.Kind() {
			case reflect.Struct:
				walk(ft, path+".")
			case reflect.Slice:
				el := ft.Elem()
				if el.Kind() == reflect.Struct {
					out[path] = "list of objects"
					walk(el, path+"[].")
				} else {
					out[path] = "list of " + scalarName(el) + "s"
				}
			default:
				out[path] = scalarName(ft)
			}
		}
	}
	walk(t, "")
	return out
}

func scalarName(t reflect.Type) string {
	switch t.Kind() {
	case reflect.String:
		return "string"
	case reflect.Bool:
		return "bool"
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return "integer"
	case reflect.Float32, reflect.Float64:
		return "number"
	default:
		return t.Kind().String()
	}
}

type docRow struct{ typ, required string }

// documentedFields reads the field tables of the page, keyed by the "## "
// section they sit in.
func documentedFields(t *testing.T, page string) map[string]map[string]docRow {
	t.Helper()
	f, err := os.Open(page) // #nosec G304 -- fixed repository path
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	out := map[string]map[string]docRow{}
	section := ""
	inTable := false
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if s, ok := strings.CutPrefix(line, "## "); ok {
			section = s
			continue
		}
		if !strings.HasPrefix(line, "|") {
			inTable = false
			continue
		}
		cells := strings.Split(strings.Trim(line, "|"), "|")
		for i := range cells {
			cells[i] = strings.TrimSpace(cells[i])
		}
		switch {
		case cells[0] == "Field":
			if len(cells) != 4 || cells[1] != "Type" || cells[2] != "Required" || cells[3] != "Meaning" {
				t.Errorf("section %q: field table header must be Field | Type | Required | Meaning, got %q", section, line)
			}
			inTable = true
		case !inTable || strings.HasPrefix(cells[0], "---"):
		default:
			if len(cells) != 4 {
				t.Errorf("section %q: row %q must have 4 cells", section, line)
				continue
			}
			path := strings.Trim(cells[0], "`")
			if out[section] == nil {
				out[section] = map[string]docRow{}
			}
			if _, dup := out[section][path]; dup {
				t.Errorf("section %q: field %s listed twice", section, path)
			}
			out[section][path] = docRow{typ: cells[1], required: cells[2]}
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// Supporting documents that every example loads next to. Nothing in them
// refers to the document under test, which is always named "subject".
var supportDocs = []string{
	`{"apiVersion":"sablecourt.io/v1alpha1","kind":"Budget","metadata":{"name":"sup-budget"},"spec":{"perTask":{"maxCostUSD":1}}}`,
	`{"apiVersion":"sablecourt.io/v1alpha1","kind":"Workload","metadata":{"name":"sup-runtime","zone":"runtime"},"spec":{"class":"long-lived-agent","runtime":{"type":"docker","image":"example/agent:1"}}}`,
	`{"apiVersion":"sablecourt.io/v1alpha1","kind":"Workload","metadata":{"name":"sup-coder","zone":"runtime"},"spec":{"class":"coding-agent","runtime":{"type":"worktree-cli"},"coding":{"harness":"claude","repoPath":"/srv/repo","sandbox":"seatbelt","commands":[{"name":"test","argv":["make","test"]}]}}}`,
}

var exampleDocs = map[string][]string{
	"Workload": {
		`{"apiVersion":"sablecourt.io/v1alpha1","kind":"Workload","metadata":{"name":"subject","zone":"runtime"},"spec":{
			"class":"long-lived-agent",
			"runtime":{"type":"docker","image":"example/agent:1","readOnlyRootfs":true},
			"agentCard":{"description":"d","skills":["s"]},
			"roleCard":"r",
			"grants":{
				"modelGate":{"allowed":true,"providers":["openai"],"budgetRef":"sup-budget"},
				"mcpTools":["memory.get"],
				"mcpServers":[{"name":"up","url":"https://mcp.example.test/mcp","secretName":"up_token","authHeader":"Authorization","authPrefix":"Bearer ","allowedTools":["search"]}],
				"actions":["git.open_pr"],
				"repos":["shadewright-labs/sablecourt"],
				"delegateTo":["sup-runtime"]}}}`,
		`{"apiVersion":"sablecourt.io/v1alpha1","kind":"Workload","metadata":{"name":"subject","zone":"sandbox"},"spec":{
			"class":"coding-agent",
			"runtime":{"type":"worktree-cli"},
			"coding":{"harness":"codex","repoPath":"/srv/repo","sandbox":"seatbelt",
				"commands":[{"name":"test","argv":["make","test"],"timeoutSeconds":600,"maxOutputBytes":65536,"failureReason":"r"}]}}}`,
	},
	"Policy": {
		`{"apiVersion":"sablecourt.io/v1alpha1","kind":"Policy","metadata":{"name":"subject"},"spec":{"defaultEffect":"deny","rules":[{"workload":"*","capability":"mcp:memory.get","effect":"allow"}]}}`,
	},
	// At least one limit must be positive, so each limit is the only one in
	// one example: deleting it there fails, deleting it elsewhere loads.
	"Budget": {
		`{"apiVersion":"sablecourt.io/v1alpha1","kind":"Budget","metadata":{"name":"subject"},"spec":{"perTask":{"maxInputTokens":10,"maxOutputTokens":10,"maxCostUSD":1},"daily":{"maxCostUSD":5}}}`,
		`{"apiVersion":"sablecourt.io/v1alpha1","kind":"Budget","metadata":{"name":"subject"},"spec":{"perTask":{"maxInputTokens":10}}}`,
		`{"apiVersion":"sablecourt.io/v1alpha1","kind":"Budget","metadata":{"name":"subject"},"spec":{"perTask":{"maxOutputTokens":10}}}`,
		`{"apiVersion":"sablecourt.io/v1alpha1","kind":"Budget","metadata":{"name":"subject"},"spec":{"perTask":{"maxCostUSD":1}}}`,
		`{"apiVersion":"sablecourt.io/v1alpha1","kind":"Budget","metadata":{"name":"subject"},"spec":{"daily":{"maxCostUSD":5}}}`,
	},
	"Workflow": {
		`{"apiVersion":"sablecourt.io/v1alpha1","kind":"Workflow","metadata":{"name":"subject","zone":"runtime"},"spec":{"description":"d","steps":[
			{"name":"author","workload":"sup-runtime","role":"author","provider":"openai","model":"gpt-x","prompt":"p","maxRetries":1}]}}`,
		`{"apiVersion":"sablecourt.io/v1alpha1","kind":"Workflow","metadata":{"name":"subject"},"spec":{"steps":[
			{"name":"code","workload":"sup-coder","tier":"coding"},
			{"name":"check","workload":"sup-coder","command":"test"}]}}`,
	},
	"Schedule": {
		`{"apiVersion":"sablecourt.io/v1alpha1","kind":"Schedule","metadata":{"name":"subject"},"spec":{"cron":"@hourly","targetKind":"workload","targetName":"sup-runtime","input":"{}","enabled":true}}`,
	},
	"Conclave": {
		`{"apiVersion":"sablecourt.io/v1alpha1","kind":"Conclave","metadata":{"name":"subject"},"spec":{"description":"d","budgetRef":"sup-budget",
			"wards":{"maxRuns":2,"maxMessages":4,"maxPingPong":2},
			"vassals":[{"role":"lead","workload":"sup-runtime","activation":"always-on","canMessage":["operator"]}]}}`,
	},
}

// deletePath removes the field at path from doc. A "[]" segment means the
// first element of a list that has the rest of the path, so a field set only
// on a later step (a command step cannot be the first) is found. It reports
// whether the field was present.
func deletePath(doc map[string]any, path string) bool {
	name, rest, nested := strings.Cut(path, ".")
	field, isList := strings.CutSuffix(name, "[]")
	next, ok := doc[field]
	if !ok {
		return false
	}
	if !nested {
		delete(doc, field)
		return true
	}
	if !isList {
		m, ok := next.(map[string]any)
		return ok && deletePath(m, rest)
	}
	list, ok := next.([]any)
	if !ok {
		return false
	}
	for _, item := range list {
		if m, ok := item.(map[string]any); ok && deletePath(m, rest) {
			return true
		}
	}
	return false
}

func loadDocs(t *testing.T, subject string) error {
	t.Helper()
	file := filepath.Join(t.TempDir(), "m.yaml")
	body := strings.Join(append(append([]string{}, supportDocs...), subject), "\n---\n")
	if err := os.WriteFile(file, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := LoadPath(file)
	return err
}

// requiredness deletes path from each example and classifies the outcome.
func requiredness(t *testing.T, examples []string, path string) (string, bool) {
	t.Helper()
	fails, loads, present := 0, 0, false
	for _, ex := range examples {
		var doc map[string]any
		if err := json.Unmarshal([]byte(ex), &doc); err != nil {
			t.Fatalf("example is not JSON: %v", err)
		}
		if deletePath(doc, path) {
			present = true
		}
		b, _ := json.Marshal(doc)
		if loadDocs(t, string(b)) != nil {
			fails++
		} else {
			loads++
		}
	}
	switch {
	case loads == 0:
		return "yes", present
	case fails == 0:
		return "no", present
	default:
		return "conditional", present
	}
}

func TestManifestReferenceMatchesTypesAndLoader(t *testing.T) {
	for kind, examples := range exampleDocs {
		for i, ex := range examples {
			if err := loadDocs(t, ex); err != nil {
				t.Fatalf("%s example %d does not load: %v", kind, i, err)
			}
		}
	}

	kinds := loaderKinds(t)
	if got := sortedKeys(manifestKinds); strings.Join(got, ",") != strings.Join(kinds, ",") {
		t.Fatalf("the loader accepts the kinds %v, manifestKinds lists %v; add the new kind to manifestKinds and exampleDocs", kinds, got)
	}

	doc := documentedFields(t, filepath.Join("..", "..", "docs", "reference", "manifests.md"))
	var allExamples []string
	common := map[string]string{}
	for _, kind := range sortedKeys(manifestKinds) {
		allExamples = append(allExamples, exampleDocs[kind]...)
		want := map[string]string{}
		for path, typ := range fieldTypes(manifestKinds[kind]) {
			if isCommon(path) {
				common[path] = typ
				continue
			}
			want[path] = typ
		}
		checkSection(t, kind, doc[kind], want, exampleDocs[kind])
	}
	checkSection(t, commonSection, doc[commonSection], common, allExamples)
}

func checkSection(t *testing.T, section string, got map[string]docRow, want map[string]string, examples []string) {
	t.Helper()
	if len(got) == 0 {
		t.Errorf("manifests.md has no field table in section %q", section)
	}
	var printed []string
	for _, path := range sortedKeys(want) {
		req, present := requiredness(t, examples, path)
		printed = append(printed, fmt.Sprintf("| `%s` | %s | %s |", path, want[path], req))
		if !present {
			t.Errorf("%s: no example sets %s; add one to exampleDocs", section, path)
		}
		row, ok := got[path]
		if !ok {
			t.Errorf("%s: field %s (%s) missing from manifests.md", section, path, want[path])
			continue
		}
		if row.typ != want[path] {
			t.Errorf("%s: field %s has type %q in manifests.md, the code says %q", section, path, row.typ, want[path])
		}
		if row.required != req {
			t.Errorf("%s: field %s has Required %q in manifests.md, the loader says %q", section, path, row.required, req)
		}
	}
	for path := range got {
		if _, ok := want[path]; !ok {
			t.Errorf("%s: field %s is in manifests.md but not in the manifest type", section, path)
		}
	}
	if t.Failed() {
		t.Logf("%s rows from the code:\n%s", section, strings.Join(printed, "\n"))
	}
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
