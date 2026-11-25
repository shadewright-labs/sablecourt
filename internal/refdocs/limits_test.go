// Package refdocs holds tests that tie the reference pages under
// docs/reference to the code.
package refdocs

import (
	"bufio"
	"fmt"
	"go/ast"
	"go/constant"
	"go/parser"
	"go/token"
	"math/big"
	"os"
	"path/filepath"
	"strconv"
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

// evaluator resolves named constants in the module's packages from source.
// It understands literals, arithmetic, shifts, parentheses, references to
// other constants (in the same package or another package of the module),
// the time unit constants, conversions such as int64(x) or
// time.Duration(x), and len of a string constant.
type evaluator struct {
	fset  *token.FileSet
	pkgs  map[string]map[string]*constDecl // package dir -> name -> decl
	cache map[string]value
}

type constDecl struct {
	file    *ast.File
	typ     ast.Expr
	value   ast.Expr
	pkgDir  string
	pending bool
}

func newEvaluator() *evaluator {
	return &evaluator{fset: token.NewFileSet(), pkgs: map[string]map[string]*constDecl{}, cache: map[string]value{}}
}

func (e *evaluator) load(dir string) (map[string]*constDecl, error) {
	if decls, ok := e.pkgs[dir]; ok {
		return decls, nil
	}
	entries, err := os.ReadDir(filepath.Join(repoRoot, dir))
	if err != nil {
		return nil, err
	}
	decls := map[string]*constDecl{}
	for _, ent := range entries {
		name := ent.Name()
		if ent.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(e.fset, filepath.Join(repoRoot, dir, name), nil, 0)
		if err != nil {
			return nil, err
		}
		for _, d := range f.Decls {
			g, ok := d.(*ast.GenDecl)
			if !ok || g.Tok != token.CONST {
				continue
			}
			for _, s := range g.Specs {
				vs := s.(*ast.ValueSpec)
				for i, n := range vs.Names {
					if i < len(vs.Values) {
						decls[n.Name] = &constDecl{file: f, typ: vs.Type, value: vs.Values[i], pkgDir: dir}
					}
				}
			}
		}
	}
	e.pkgs[dir] = decls
	return decls, nil
}

func (e *evaluator) resolve(dir, name string) (value, error) {
	key := dir + "." + name
	if v, ok := e.cache[key]; ok {
		return v, nil
	}
	decls, err := e.load(dir)
	if err != nil {
		return value{}, err
	}
	d, ok := decls[name]
	if !ok {
		return value{}, fmt.Errorf("no constant %s in %s", name, dir)
	}
	if d.pending {
		return value{}, fmt.Errorf("constant %s refers to itself", key)
	}
	d.pending = true
	defer func() { d.pending = false }()
	v, err := e.eval(d, d.value)
	if err != nil {
		return value{}, fmt.Errorf("%s: %w", key, err)
	}
	if isTimeDuration(d.typ) {
		v.duration = true
	}
	e.cache[key] = v
	return v, nil
}

func isTimeDuration(x ast.Expr) bool {
	sel, ok := x.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	pkg, ok := sel.X.(*ast.Ident)
	return ok && pkg.Name == "time" && sel.Sel.Name == "Duration"
}

var timeUnits = map[string]int64{
	"Nanosecond": 1, "Microsecond": 1e3, "Millisecond": 1e6,
	"Second": 1e9, "Minute": 60e9, "Hour": 3600e9,
}

var conversions = map[string]bool{
	"int": true, "int8": true, "int16": true, "int32": true, "int64": true,
	"uint": true, "uint8": true, "uint16": true, "uint32": true, "uint64": true,
	"float32": true, "float64": true,
}

func (e *evaluator) eval(d *constDecl, x ast.Expr) (value, error) {
	switch x := x.(type) {
	case *ast.BasicLit:
		return value{v: constant.MakeFromLiteral(x.Value, x.Kind, 0)}, nil
	case *ast.ParenExpr:
		return e.eval(d, x.X)
	case *ast.Ident:
		return e.resolve(d.pkgDir, x.Name)
	case *ast.SelectorExpr:
		pkg, ok := x.X.(*ast.Ident)
		if !ok {
			return value{}, fmt.Errorf("unsupported selector %T", x.X)
		}
		if pkg.Name == "time" {
			if n, ok := timeUnits[x.Sel.Name]; ok {
				return value{v: constant.MakeInt64(n), duration: true}, nil
			}
		}
		dir, err := importDir(d.file, pkg.Name)
		if err != nil {
			return value{}, err
		}
		return e.resolve(dir, x.Sel.Name)
	case *ast.UnaryExpr:
		v, err := e.eval(d, x.X)
		if err != nil {
			return value{}, err
		}
		return value{v: constant.UnaryOp(x.Op, v.v, 0), duration: v.duration}, nil
	case *ast.BinaryExpr:
		l, err := e.eval(d, x.X)
		if err != nil {
			return value{}, err
		}
		r, err := e.eval(d, x.Y)
		if err != nil {
			return value{}, err
		}
		var v constant.Value
		switch x.Op {
		case token.SHL, token.SHR:
			s, ok := constant.Uint64Val(r.v)
			if !ok {
				return value{}, fmt.Errorf("bad shift count")
			}
			v = constant.Shift(l.v, x.Op, uint(s))
		case token.QUO:
			if l.v.Kind() == constant.Int && r.v.Kind() == constant.Int {
				v = constant.BinaryOp(l.v, token.QUO_ASSIGN, r.v)
			} else {
				v = constant.BinaryOp(l.v, token.QUO, r.v)
			}
		case token.ADD, token.SUB, token.MUL, token.REM:
			v = constant.BinaryOp(l.v, x.Op, r.v)
		default:
			return value{}, fmt.Errorf("unsupported operator %s", x.Op)
		}
		return value{v: v, duration: l.duration || r.duration}, nil
	case *ast.CallExpr:
		if len(x.Args) != 1 {
			return value{}, fmt.Errorf("unsupported call")
		}
		if fn, ok := x.Fun.(*ast.Ident); ok && fn.Name == "len" {
			if lit, ok := x.Args[0].(*ast.BasicLit); ok && lit.Kind == token.STRING {
				s, err := strconv.Unquote(lit.Value)
				if err != nil {
					return value{}, err
				}
				return value{v: constant.MakeInt64(int64(len(s)))}, nil
			}
			if id, ok := x.Args[0].(*ast.Ident); ok {
				decls, _ := e.load(d.pkgDir)
				if sd, ok := decls[id.Name]; ok {
					if lit, ok := sd.value.(*ast.BasicLit); ok && lit.Kind == token.STRING {
						s, err := strconv.Unquote(lit.Value)
						if err != nil {
							return value{}, err
						}
						return value{v: constant.MakeInt64(int64(len(s)))}, nil
					}
				}
			}
			return value{}, fmt.Errorf("unsupported len argument")
		}
		arg, err := e.eval(d, x.Args[0])
		if err != nil {
			return value{}, err
		}
		switch fn := x.Fun.(type) {
		case *ast.Ident:
			if conversions[fn.Name] {
				return arg, nil
			}
		case *ast.SelectorExpr:
			if isTimeDuration(fn) {
				arg.duration = true
				return arg, nil
			}
		}
		return value{}, fmt.Errorf("unsupported call")
	}
	return value{}, fmt.Errorf("unsupported expression %T", x)
}

// importDir maps a package name used in file to its directory in the module.
func importDir(f *ast.File, name string) (string, error) {
	for _, imp := range f.Imports {
		path, _ := strconv.Unquote(imp.Path.Value)
		local := path[strings.LastIndex(path, "/")+1:]
		if imp.Name != nil {
			local = imp.Name.Name
		}
		if local != name {
			continue
		}
		rel, ok := strings.CutPrefix(path, modulePath+"/")
		if !ok {
			return "", fmt.Errorf("package %s is outside the module", path)
		}
		return rel, nil
	}
	return "", fmt.Errorf("no import for %s", name)
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
	section, inTable, n := "", false, 0
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		n++
		line := strings.TrimSpace(sc.Text())
		if s, ok := strings.CutPrefix(line, "## "); ok {
			section = s
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
		case !inTable:
			if len(cells) != 3 || cells[0] != "Limit" || cells[1] != "Constant" || cells[2] != "Value" {
				t.Errorf("limits.md:%d: table header must be Limit | Constant | Value", n)
			}
			inTable = true
		case strings.HasPrefix(cells[0], "---"):
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
