package refdocs

import (
	"bufio"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/shadewright-labs/sablecourt/internal/gate/councilproxy"
	"github.com/shadewright-labs/sablecourt/internal/operatortoken"
)

// pageTable is one Markdown table of a page: the "## " section it sits in,
// its header cells and its rows.
type pageTable struct {
	section string
	header  []string
	rows    [][]string
}

// readTables reads the tables of a page and the text of each section. A row
// whose cell count differs from its header fails the test and is left out,
// so later lookups can index any column of the header.
func readTables(t *testing.T, page string) (tables []pageTable, text map[string]string) {
	t.Helper()
	f, err := os.Open(page) // #nosec G304 -- fixed repository path
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	tables, text, problems, err := parseTables(f)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range problems {
		t.Errorf("%s: %s", filepath.Base(page), p)
	}
	return tables, text
}

func parseTables(r io.Reader) (tables []pageTable, text map[string]string, problems []string, err error) {
	text = map[string]string{}
	section := ""
	var cur *pageTable
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if s, ok := strings.CutPrefix(line, "## "); ok {
			section = s
		}
		if !strings.HasPrefix(line, "|") {
			cur = nil
			text[section] += line + "\n"
			continue
		}
		cells := strings.Split(strings.Trim(line, "|"), "|")
		for i := range cells {
			cells[i] = strings.TrimSpace(cells[i])
		}
		switch {
		case cur == nil:
			tables = append(tables, pageTable{section: section, header: cells})
			cur = &tables[len(tables)-1]
		case strings.HasPrefix(cells[0], "---"):
		case len(cells) != len(cur.header):
			problems = append(problems, fmt.Sprintf("%s: row %q has %d cells, its header has %d", section, line, len(cells), len(cur.header)))
		default:
			cur.rows = append(cur.rows, cells)
		}
	}
	return tables, text, problems, sc.Err()
}

// backticked returns the names in backticks in a cell.
func backticked(cell string) []string {
	var out []string
	for _, m := range regexp.MustCompile("`([^`]+)`").FindAllStringSubmatch(cell, -1) {
		out = append(out, m[1])
	}
	return out
}

var configPage = filepath.Join(repoRoot, "docs", "reference", "configuration.md")

// unobservedStateEntries are state directory entries that the test cannot
// see after LoadOrCreate and LockStateDir return, with the source text that
// names each one.
var unobservedStateEntries = map[string]struct{ file, source string }{
	"operator.token.pending": {"internal/operatortoken/token.go", `const pending = Filename + ".pending"`},
}

// TestConfigurationStateDirectory fails when the State directory section of
// docs/reference/configuration.md differs from what the code does: the
// default directory below the home directory, the directory mode, and the
// name and mode of each entry the daemon creates there.
func TestConfigurationStateDirectory(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("SABLECOURT_STATE_DIR", "")
	dir, err := operatortoken.StateDir()
	if err != nil {
		t.Fatal(err)
	}
	rel, err := filepath.Rel(home, dir)
	if err != nil {
		t.Fatal(err)
	}
	tables, text := readTables(t, configPage)
	intro := text["State directory"]
	if want := "otherwise `" + rel + "` under the home directory"; !strings.Contains(intro, want) {
		t.Errorf("State directory: the default is %s under the home directory; the page must say %q", rel, want)
	}
	if !strings.Contains(intro, "`SABLECOURT_STATE_DIR` if set") {
		t.Error("State directory: the page must say the directory is `SABLECOURT_STATE_DIR` if set")
	}

	if _, err := operatortoken.LoadOrCreate(dir); err != nil {
		t.Fatal(err)
	}
	lock, err := operatortoken.LockStateDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lock.Close() }()
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if want := fmt.Sprintf("It is created with mode %04o.", info.Mode().Perm()); !strings.Contains(intro, want) {
		t.Errorf("State directory: the page must say %q", want)
	}

	rows := map[string]string{}
	for _, tb := range tables {
		if tb.section != "State directory" {
			continue
		}
		for _, r := range tb.rows {
			for _, name := range backticked(r[0]) {
				rows[name] = strings.Join(r[1:], " | ")
			}
		}
	}
	accounted := map[string]bool{}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		fi, err := e.Info()
		if err != nil {
			t.Fatal(err)
		}
		accounted[e.Name()] = true
		row, ok := rows[e.Name()]
		if !ok {
			t.Errorf("State directory: the daemon creates %s; it is missing from the table", e.Name())
			continue
		}
		if want := fmt.Sprintf("mode %04o", fi.Mode().Perm()); !strings.Contains(row, want) {
			t.Errorf("State directory: %s is created with %s; its row must say so", e.Name(), want)
		}
	}
	gates := filepath.Base(councilproxy.SocketRoot(dir)) + "/"
	accounted[gates] = true
	if _, ok := rows[gates]; !ok {
		t.Errorf("State directory: council gate sockets live in %s; it is missing from the table", gates)
	}
	for name, src := range unobservedStateEntries {
		body, err := os.ReadFile(filepath.Join(repoRoot, src.file)) // #nosec G304 -- fixed repository path
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(body), src.source) {
			t.Errorf("%s no longer contains %q; update unobservedStateEntries", src.file, src.source)
			continue
		}
		accounted[name] = true
	}
	for name := range rows {
		if !accounted[name] {
			t.Errorf("State directory: %s is in the table, but the code does not create it", name)
		}
	}
}

// envRead is one environment variable read in the code.
type envRead struct {
	name      string
	forwarded bool // read only to copy into the environment of a child process
	at        string
}

// envScanDirs are the source trees of sablecourtd, sablectl and the packages
// they use. cmd/sablecourt-agent runs inside a workload; what it reads is in
// the "Environment passed to workloads" table. internal/clidoc/write is the
// docs generator that make docs runs, and is skipped.
var envScanDirs = []string{"cmd/sablecourtd", "cmd/sablectl", "internal"}

// environSites are the calls of os.Environ, which pass the whole
// environment on. The page names each one.
var environSites = map[string]bool{"internal/runtime/coding/git.go": true}

// scanEnvReads finds every os.Getenv and os.LookupEnv call. The name is a
// string literal, a range variable over a literal list, or a parameter of
// the enclosing function, resolved from the literal arguments of its calls
// in the same package.
func scanEnvReads(t *testing.T) ([]envRead, map[string]bool) {
	t.Helper()
	fset := token.NewFileSet()
	var reads []envRead
	environ := map[string]bool{}
	pkgs := map[string][]*ast.File{}
	for _, root := range envScanDirs {
		err := filepath.WalkDir(filepath.Join(repoRoot, root), func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() && (d.Name() == "testdata" || filepath.ToSlash(path) == repoRoot+"/internal/clidoc/write") {
				return filepath.SkipDir
			}
			if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			f, err := parser.ParseFile(fset, path, nil, 0)
			if err != nil {
				return err
			}
			pkgs[filepath.Dir(path)] = append(pkgs[filepath.Dir(path)], f)
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	for dir, files := range pkgs {
		for _, f := range files {
			var stack []ast.Node
			ast.Inspect(f, func(n ast.Node) bool {
				if n == nil {
					stack = stack[:len(stack)-1]
					return true
				}
				stack = append(stack, n)
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				pkg, ok := sel.X.(*ast.Ident)
				if !ok || pkg.Name != "os" {
					return true
				}
				pos := fset.Position(call.Pos())
				relFile, _ := filepath.Rel(repoRoot, pos.Filename)
				at := fmt.Sprintf("%s:%d", relFile, pos.Line)
				switch sel.Sel.Name {
				case "Environ":
					environ[relFile] = true
					return true
				case "Getenv", "LookupEnv":
				default:
					return true
				}
				names, forwarded, err := resolveEnvName(call.Args[0], stack, files)
				if err != nil {
					t.Errorf("%s: %v", at, err)
					return true
				}
				if !forwarded && len(stack) >= 2 {
					if bin, ok := stack[len(stack)-2].(*ast.BinaryExpr); ok && bin.Op == token.ADD {
						if lit, ok := bin.X.(*ast.BasicLit); ok && strings.HasSuffix(lit.Value, `="`) {
							forwarded = true
						}
					}
				}
				for _, name := range names {
					reads = append(reads, envRead{name: name, forwarded: forwarded, at: at})
				}
				return true
			})
		}
		_ = dir
	}
	return reads, environ
}

func stringLit(x ast.Expr) (string, bool) {
	lit, ok := x.(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return "", false
	}
	s, err := strconv.Unquote(lit.Value)
	return s, err == nil
}

func resolveEnvName(arg ast.Expr, stack []ast.Node, files []*ast.File) (names []string, forwarded bool, err error) {
	if s, ok := stringLit(arg); ok {
		return []string{s}, false, nil
	}
	id, ok := arg.(*ast.Ident)
	if !ok {
		return nil, false, fmt.Errorf("cannot resolve the variable name %T", arg)
	}
	for i := len(stack) - 1; i >= 0; i-- {
		switch n := stack[i].(type) {
		case *ast.RangeStmt:
			if v, ok := n.Value.(*ast.Ident); ok && v.Name == id.Name {
				list, ok := n.X.(*ast.CompositeLit)
				if !ok {
					return nil, false, fmt.Errorf("range over %s is not a literal list", id.Name)
				}
				for _, el := range list.Elts {
					s, ok := stringLit(el)
					if !ok {
						return nil, false, fmt.Errorf("range list of %s holds a non-literal", id.Name)
					}
					names = append(names, s)
				}
				// The loop copies each name into a child's environment.
				ast.Inspect(n.Body, func(m ast.Node) bool {
					if lit, ok := m.(*ast.BasicLit); ok && lit.Value == `"="` {
						forwarded = true
					}
					return true
				})
				return names, forwarded, nil
			}
		case *ast.FuncDecl:
			idx := -1
			k := 0
			for _, field := range n.Type.Params.List {
				for _, nm := range field.Names {
					if nm.Name == id.Name {
						idx = k
					}
					k++
				}
			}
			if idx < 0 {
				return nil, false, fmt.Errorf("%s is neither a literal, a range variable nor a parameter", id.Name)
			}
			for _, f := range files {
				ast.Inspect(f, func(m ast.Node) bool {
					call, ok := m.(*ast.CallExpr)
					if !ok || len(call.Args) <= idx {
						return true
					}
					var fn string
					switch c := call.Fun.(type) {
					case *ast.Ident:
						fn = c.Name
					case *ast.SelectorExpr:
						fn = c.Sel.Name
					}
					if fn != n.Name.Name {
						return true
					}
					s, ok := stringLit(call.Args[idx])
					if !ok {
						err = fmt.Errorf("a call of %s passes a non-literal %s", n.Name.Name, id.Name)
					} else if s != "" {
						names = append(names, s)
					}
					return true
				})
			}
			if err == nil && len(names) == 0 {
				err = fmt.Errorf("no call of %s passes a literal %s", n.Name.Name, id.Name)
			}
			return names, false, err
		}
	}
	return nil, false, fmt.Errorf("cannot resolve %s", id.Name)
}

// TestConfigurationEnvironmentVariables fails when the environment variable
// names on docs/reference/configuration.md differ from the names the code
// reads. A name read to configure the program must be in the Environment
// variables table or the Environment variable column of the voice table; a
// name read only to copy into a child process must be in the pass-through
// table. Both directions are checked. No flag takes its default from the
// environment through a binding library: every fallback is an os.Getenv
// call, so the scan sees it.
func TestConfigurationEnvironmentVariables(t *testing.T) {
	reads, environ := scanEnvReads(t)
	if len(reads) < 10 {
		t.Fatalf("only %d environment reads found; the scan is broken", len(reads))
	}
	code := map[bool]map[string]string{false: {}, true: {}}
	for _, r := range reads {
		code[r.forwarded][r.name] = r.at
	}
	for file := range environ {
		if !environSites[file] {
			t.Errorf("%s passes the whole environment on with os.Environ; name it on the page and in environSites", file)
		}
	}
	for file := range environSites {
		if !environ[file] {
			t.Errorf("%s no longer calls os.Environ; update environSites and the page", file)
		}
	}

	tables, text := readTables(t, configPage)
	doc := map[bool]map[string]bool{false: {}, true: {}}
	for _, tb := range tables {
		col, forwarded := -1, false
		switch {
		case tb.section == "Environment variables" && len(tb.header) > 1 && tb.header[0] == "Variable" && tb.header[1] == "Read by":
			col = 0
		case tb.section == "Environment variables" && len(tb.header) > 1 && tb.header[0] == "Variable" && tb.header[1] == "Passed by":
			col, forwarded = 0, true
		default:
			for i, h := range tb.header {
				if h == "Environment variable" {
					col = i
				}
			}
		}
		if col < 0 {
			continue
		}
		for _, r := range tb.rows {
			for _, name := range backticked(r[col]) {
				doc[forwarded][name] = true
			}
		}
	}
	for file := range environSites {
		if !strings.Contains(text["Environment variables"], "`"+file+"`") {
			t.Errorf("Environment variables: the page must name %s, which passes the environment on", file)
		}
	}
	for _, forwarded := range []bool{false, true} {
		kind := "read"
		if forwarded {
			kind = "passed to a child process"
		}
		var names []string
		for name := range code[forwarded] {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			if !doc[forwarded][name] {
				t.Errorf("%s is %s at %s; it is missing from the page", name, kind, code[forwarded][name])
			}
		}
		for name := range doc[forwarded] {
			if _, ok := code[forwarded][name]; !ok {
				t.Errorf("%s is on the page as %s, but no code does that", name, kind)
			}
		}
	}
}

// TestParseTablesRowWidth checks that a row whose cell count differs from
// its header is reported and left out instead of being indexed later.
func TestParseTablesRowWidth(t *testing.T) {
	for _, tc := range []struct {
		name     string
		page     string
		rows     int
		problems int
	}{
		{"full rows", "## S\n\n| A | B |\n| --- | --- |\n| `x` | `y` |\n", 1, 0},
		{"short row", "## S\n\n| A | B |\n| --- | --- |\n| `x` |\n| `y` | `z` |\n", 1, 1},
		{"long row", "## S\n\n| A | B |\n| --- | --- |\n| `x` | `y` | `z` |\n", 0, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tables, _, problems, err := parseTables(strings.NewReader(tc.page))
			if err != nil {
				t.Fatal(err)
			}
			if len(tables) != 1 || len(tables[0].rows) != tc.rows || len(problems) != tc.problems {
				t.Fatalf("tables %v, problems %q; want one table with %d rows and %d problems", tables, problems, tc.rows, tc.problems)
			}
			for _, r := range tables[0].rows {
				_ = r[len(tables[0].header)-1]
			}
		})
	}
}
