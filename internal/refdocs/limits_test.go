// Package refdocs holds tests that tie the reference pages under
// docs/reference to the code.
package refdocs

import (
	"bufio"
	"errors"
	"fmt"
	"go/ast"
	"go/build"
	"go/constant"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	repoRoot   = "../.."
	modulePath = "github.com/shadewright-labs/sablecourt"
)

// value is an evaluated constant: its number and whether it is a
// time.Duration (in nanoseconds).
type value struct {
	v        constant.Value
	duration bool
}

// evaluator resolves named constants in the module's packages with the Go
// type checker, so a value is what the compiler computes: typed constants,
// implicit and explicit conversions, float32 rounding, iota and omitted
// initializers all follow the language. A package of the module is checked
// from the files the go tool selects for the current platform; any other
// package comes from the compiler's export data. Errors outside constant
// declarations are ignored: a constant that cannot be computed is reported
// when it is looked up.
type evaluator struct {
	fset *token.FileSet
	std  types.Importer
	pkgs map[string]*types.Package // absolute dir -> package, nil while checking
}

func newEvaluator() *evaluator {
	fset := token.NewFileSet()
	return &evaluator{fset: fset, std: importer.ForCompiler(fset, "gc", nil), pkgs: map[string]*types.Package{}}
}

// Import implements types.Importer.
func (e *evaluator) Import(path string) (*types.Package, error) {
	if rel, ok := strings.CutPrefix(path, modulePath+"/"); ok {
		return e.check(filepath.Join(repoRoot, filepath.FromSlash(rel)), path)
	}
	return e.std.Import(path)
}

// goFiles returns the non-test Go files of dir that the build context
// selects, so a file for another platform or behind a build tag is left out.
func goFiles(ctx build.Context, dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, ent := range entries {
		name := ent.Name()
		if ent.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		ok, err := ctx.MatchFile(dir, name)
		if err != nil {
			return nil, err
		}
		if ok {
			out = append(out, filepath.Join(dir, name))
		}
	}
	return out, nil
}

func (e *evaluator) check(dir, path string) (*types.Package, error) {
	if pkg, ok := e.pkgs[dir]; ok {
		if pkg == nil {
			return nil, fmt.Errorf("import cycle through %s", path)
		}
		return pkg, nil
	}
	e.pkgs[dir] = nil
	names, err := goFiles(build.Default, dir)
	if err != nil {
		delete(e.pkgs, dir)
		return nil, err
	}
	var files []*ast.File
	for _, name := range names {
		f, err := parser.ParseFile(e.fset, name, nil, 0)
		if err != nil {
			delete(e.pkgs, dir)
			return nil, err
		}
		files = append(files, f)
	}
	conf := types.Config{Importer: e, IgnoreFuncBodies: true, FakeImportC: true, Error: func(error) {}}
	pkg, _ := conf.Check(path, e.fset, files, nil)
	e.pkgs[dir] = pkg
	return pkg, nil
}

// resolve returns the value of constant name in the module package dir.
func (e *evaluator) resolve(dir, name string) (value, error) {
	pkg, err := e.check(filepath.Join(repoRoot, filepath.FromSlash(dir)), modulePath+"/"+dir)
	if err != nil {
		return value{}, err
	}
	return constValue(pkg, name)
}

func constValue(pkg *types.Package, name string) (value, error) {
	c, ok := pkg.Scope().Lookup(name).(*types.Const)
	if !ok {
		return value{}, fmt.Errorf("no constant %s in %s", name, pkg.Path())
	}
	if c.Val().Kind() == constant.Unknown {
		return value{}, fmt.Errorf("constant %s.%s cannot be computed", pkg.Path(), name)
	}
	return value{v: c.Val(), duration: isDuration(c.Type())}, nil
}

func isDuration(t types.Type) bool {
	n, ok := t.(*types.Named)
	return ok && n.Obj().Pkg() != nil && n.Obj().Pkg().Path() == "time" && n.Obj().Name() == "Duration"
}

var errNoFiles = errors.New("no Go files")

// evalSource type-checks one file of source as package p and returns the
// value of constant name, for tests of the evaluator itself.
func (e *evaluator) evalSource(src, name string) (value, error) {
	f, err := parser.ParseFile(e.fset, "p.go", src, 0)
	if err != nil {
		return value{}, err
	}
	conf := types.Config{Importer: e, Error: func(error) {}}
	pkg, _ := conf.Check("p", e.fset, []*ast.File{f}, nil)
	if pkg == nil {
		return value{}, errNoFiles
	}
	return constValue(pkg, name)
}

// Units allowed in the Value column, with their size in bytes or nanoseconds.
var sizeUnits = map[string]int64{"B": 1, "KiB": 1 << 10, "MiB": 1 << 20, "GiB": 1 << 30}
var durationUnits = map[string]int64{"ns": 1, "ms": 1e6, "s": 1e9, "min": 60e9, "h": 3600e9, "days": 86400e9}

// parseValue reads "<number>" or "<number> <unit>".
func parseValue(cell string) (num *big.Rat, unit string, err error) {
	fields := strings.Fields(cell)
	if len(fields) == 0 || len(fields) > 2 {
		return nil, "", fmt.Errorf("value %q must be a number and an optional unit", cell)
	}
	num, ok := new(big.Rat).SetString(strings.ReplaceAll(fields[0], ",", ""))
	if !ok {
		return nil, "", fmt.Errorf("value %q does not start with a number", cell)
	}
	if len(fields) == 2 {
		unit = fields[1]
	}
	return num, unit, nil
}

func ratOf(v constant.Value) (*big.Rat, bool) {
	switch v.Kind() {
	case constant.Int:
		n, ok := new(big.Int).SetString(v.ExactString(), 10)
		if !ok {
			return nil, false
		}
		return new(big.Rat).SetInt(n), true
	case constant.Float:
		r, ok := new(big.Rat).SetString(v.ExactString())
		return r, ok
	}
	return nil, false
}

type limitRow struct {
	line         int
	constantRef  string
	value        string
	sectionTitle string
}

func documentedLimits(t *testing.T, page string) []limitRow {
	t.Helper()
	f, err := os.Open(page) // #nosec G304 -- fixed repository path
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	var rows []limitRow
	// pos is the line's place in the current table: 0 outside a table, 1
	// for the header, 2 for the delimiter row, 3 for data rows.
	// A table without its delimiter row is reported once and skipped.
	section, pos, n, broken := "", 0, 0, false
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		n++
		line := strings.TrimSpace(sc.Text())
		if s, ok := strings.CutPrefix(line, "## "); ok {
			section = s
		}
		if !strings.HasPrefix(line, "|") {
			pos, broken = 0, false
			continue
		}
		if broken {
			continue
		}
		if pos < 3 {
			pos++
		}
		cells := strings.Split(strings.Trim(line, "|"), "|")
		for i := range cells {
			cells[i] = strings.TrimSpace(cells[i])
		}
		switch pos {
		case 1:
			if len(cells) != 3 || cells[0] != "Limit" || cells[1] != "Constant" || cells[2] != "Value" {
				t.Errorf("limits.md:%d: table header must be Limit | Constant | Value", n)
			}
		case 2:
			if !isDelimiterRow(cells, 3) {
				t.Errorf("limits.md:%d: the header must be followed by a delimiter row of 3 cells, or the table does not render", n)
				broken = true
			}
		default:
			if len(cells) != 3 {
				t.Errorf("limits.md:%d: row must have 3 cells", n)
				continue
			}
			rows = append(rows, limitRow{line: n, constantRef: strings.Trim(cells[1], "`"), value: cells[2], sectionTitle: section})
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return rows
}

// isDelimiterRow reports a Markdown table delimiter row of width cells,
// such as "| --- | :---: |".
func isDelimiterRow(cells []string, width int) bool {
	if len(cells) != width {
		return false
	}
	for _, c := range cells {
		c = strings.TrimSuffix(strings.TrimPrefix(c, ":"), ":")
		if len(c) < 3 || strings.Trim(c, "-") != "" {
			return false
		}
	}
	return true
}

// TestLimitsReferenceMatchesConstants checks that docs/reference/limits.md
// has exactly one row for each constant in limitConstants, resolves the
// constant named in every row and compares it with the Value column:
// the number, and the unit (a time unit needs a time.Duration constant, a
// size unit or no unit needs a plain number).
func TestLimitsReferenceMatchesConstants(t *testing.T) {
	rows := documentedLimits(t, filepath.Join(repoRoot, "docs", "reference", "limits.md"))
	if len(rows) < 30 {
		t.Fatalf("only %d limit rows parsed", len(rows))
	}
	onPage := map[string]int{}
	for _, row := range rows {
		onPage[row.constantRef]++
	}
	declared := map[string]bool{}
	for _, c := range limitConstants {
		declared[c] = true
		if onPage[c] == 0 {
			t.Errorf("limits.md has no row for %s", c)
		}
	}
	for c, n := range onPage {
		if n > 1 {
			t.Errorf("limits.md lists %s %d times", c, n)
		}
		if !declared[c] {
			t.Errorf("limits.md lists %s, which is not in limitConstants", c)
		}
	}
	e := newEvaluator()
	for _, row := range rows {
		dot := strings.LastIndex(row.constantRef, ".")
		if dot < 0 {
			t.Errorf("limits.md:%d: constant %q must be <package dir>.<Name>", row.line, row.constantRef)
			continue
		}
		got, err := e.resolve(row.constantRef[:dot], row.constantRef[dot+1:])
		if err != nil {
			t.Errorf("limits.md:%d: %v", row.line, err)
			continue
		}
		num, unit, err := parseValue(row.value)
		if err != nil {
			t.Errorf("limits.md:%d: %v", row.line, err)
			continue
		}
		var scale int64 = 1
		switch {
		case unit == "":
			if got.duration {
				t.Errorf("limits.md:%d: %s is a duration, the value needs a time unit", row.line, row.constantRef)
				continue
			}
		case sizeUnits[unit] != 0:
			if got.duration {
				t.Errorf("limits.md:%d: %s is a duration, not a size", row.line, row.constantRef)
				continue
			}
			scale = sizeUnits[unit]
		case durationUnits[unit] != 0:
			if !got.duration {
				t.Errorf("limits.md:%d: %s is not a time.Duration, the value cannot use %q", row.line, row.constantRef, unit)
				continue
			}
			scale = durationUnits[unit]
		default:
			t.Errorf("limits.md:%d: unknown unit %q", row.line, unit)
			continue
		}
		want := new(big.Rat).Mul(num, new(big.Rat).SetInt64(scale))
		have, ok := ratOf(got.v)
		if !ok {
			t.Errorf("limits.md:%d: %s is not a number", row.line, row.constantRef)
			continue
		}
		if want.Cmp(have) != 0 {
			t.Errorf("limits.md:%d: %s is %s in the code, the page says %s", row.line, row.constantRef, have.RatString(), row.value)
		}
	}
}

// TestEvaluatorFollowsGo checks constant forms whose value depends on Go's
// typing rules, each with the value the compiler gives.
func TestEvaluatorFollowsGo(t *testing.T) {
	for _, tc := range []struct {
		name, src, want string
		duration        bool
	}{
		{"conversion then division", "const c = int(1025.0) / 2", "512", false},
		{"float conversion", "const c = float64(3) / 2", "3/2", false},
		{"untyped operand takes the typed side", "const c = int64(7) / 2.0", "3", false},
		{"untyped float division", "const c = 7.0 / 2", "7/2", false},
		{"declared type", "const base float64 = 3\nconst c = base / 2", "3/2", false},
		{"omitted initializer", "const (\n\tbase = 4\n\tc\n)", "4", false},
		{"iota", "const (\n\ta = 1 << (10 * iota)\n\tb\n\tc\n)", "1048576", false},
		{"float32 rounding", "const c = float32(16777217)", "16777216", false},
		{"duration to int64", "import \"time\"\nconst c = int64(time.Second)", "1000000000", false},
		{"duration conversion", "import \"time\"\nconst c = time.Duration(1025.0) / 2", "512", true},
		{"duration constant", "import \"time\"\nconst c = 30 * time.Second", "30000000000", true},
		{"float shift count", "const c = 1 << 10.0", "1024", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := newEvaluator().evalSource("package p\n"+tc.src+"\n", "c")
			if err != nil {
				t.Fatal(err)
			}
			if got.v.ExactString() != tc.want || got.duration != tc.duration {
				t.Errorf("c = %s (duration %v), want %s (duration %v)", got.v.ExactString(), got.duration, tc.want, tc.duration)
			}
		})
	}
	if _, err := newEvaluator().evalSource("package p\nconst c = int(1.5)\n", "c"); err == nil {
		t.Error("int(1.5) evaluated; want an error, as Go refuses it")
	}
}

// TestGoFilesFollowsBuildConstraints checks that a file for another
// platform does not supply a constant.
func TestGoFilesFollowsBuildConstraints(t *testing.T) {
	dir := t.TempDir()
	for name, body := range map[string]string{
		"limits_linux.go":   "package p\n\nconst limit = 100\n",
		"limits_windows.go": "package p\n\nconst limit = 200\n",
		"limits_test.go":    "package p\n\nconst other = 1\n",
		"tagged.go":         "//go:build sometag\n\npackage p\n\nconst tagged = 1\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	ctx := build.Default
	ctx.GOOS = "linux"
	got, err := goFiles(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || filepath.Base(got[0]) != "limits_linux.go" {
		t.Errorf("goFiles for linux = %v, want only limits_linux.go", got)
	}
}

func TestIsDelimiterRow(t *testing.T) {
	for _, tc := range []struct {
		cells []string
		want  bool
	}{
		{[]string{"---", ":---:", "---:"}, true},
		{[]string{"---", "---"}, false},
		{[]string{"`a`", "`b`", "1"}, false},
		{[]string{"--", "---", "---"}, false},
	} {
		if got := isDelimiterRow(tc.cells, 3); got != tc.want {
			t.Errorf("isDelimiterRow(%q) = %v, want %v", tc.cells, got, tc.want)
		}
	}
}
