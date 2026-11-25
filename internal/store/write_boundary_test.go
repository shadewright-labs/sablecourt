package store_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

var taskInsert = regexp.MustCompile(`(?i)insert\s+into\s+"?(tasks|task_messages)"?(\s|\(|$)`)

// writeBoundary names the only functions allowed to insert into each table
// and the redaction call each must make before anything else.
var writeBoundary = map[string]string{
	"sqlite/sqlite.go:SQLiteStore.CreateTask":              "RedactTaskForWrite",
	"sqlite/sqlite.go:SQLiteStore.AppendTaskMessage":       "RedactTaskMessageForWrite",
	"postgres/postgres.go:PostgresStore.CreateTask":        "RedactTaskForWrite",
	"postgres/postgres.go:PostgresStore.AppendTaskMessage": "RedactTaskMessageForWrite",
}

// TestTaskInsertsPassTheRedactionBoundary enumerates every statement in the
// module that inserts a task row or a task message. Each must sit in one of
// the boundary functions above, and each boundary function must redact its
// argument as its first statement. A new INSERT elsewhere fails this test.
func TestTaskInsertsPassTheRedactionBoundary(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	found := map[string]bool{}
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if name := d.Name(); name == ".git" || name == "node_modules" || name == "vendor" {
				return filepath.SkipDir
			}
			return nil
		}
		rel, _ := filepath.Rel(filepath.Join(root, "internal", "store"), path)
		rel = filepath.ToSlash(rel)
		switch {
		case strings.HasSuffix(path, ".sql"):
			raw, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			if taskInsert.Match(raw) {
				t.Errorf("%s inserts into a task table outside the store boundary", path)
			}
		case strings.HasSuffix(path, ".go") && !strings.HasSuffix(path, "_test.go"):
			checkGoFile(t, path, rel, found)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for name := range writeBoundary {
		if !found[name] {
			t.Errorf("boundary function %s holds no task INSERT; update the list", name)
		}
	}
}

func checkGoFile(t *testing.T, path, rel string, found map[string]bool) {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, decl := range file.Decls {
		fn, isFunc := decl.(*ast.FuncDecl)
		name := rel + ":"
		if isFunc {
			if fn.Recv != nil && len(fn.Recv.List) == 1 {
				recv := fn.Recv.List[0].Type
				if star, ok := recv.(*ast.StarExpr); ok {
					recv = star.X
				}
				if id, ok := recv.(*ast.Ident); ok {
					name += id.Name + "."
				}
			}
			name += fn.Name.Name
		}
		ast.Inspect(decl, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING || !taskInsert.MatchString(lit.Value) {
				return true
			}
			want, allowed := writeBoundary[name]
			if !isFunc || !allowed {
				t.Errorf("%s: task INSERT outside the redaction boundary (%s)", fset.Position(lit.Pos()), name)
				return true
			}
			found[name] = true
			if !redactsFirst(fn, want) {
				t.Errorf("%s does not call store.%s as its first statement", name, want)
			}
			return true
		})
	}
}

func redactsFirst(fn *ast.FuncDecl, want string) bool {
	if fn.Body == nil || len(fn.Body.List) == 0 {
		return false
	}
	assign, ok := fn.Body.List[0].(*ast.AssignStmt)
	if !ok || len(assign.Lhs) != 1 || len(assign.Rhs) != 1 || len(fn.Type.Params.List) < 2 {
		return false
	}
	call, ok := assign.Rhs[0].(*ast.CallExpr)
	if !ok || len(call.Args) != 1 {
		return false
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != want {
		return false
	}
	if pkg, ok := sel.X.(*ast.Ident); !ok || pkg.Name != "store" {
		return false
	}
	param := fn.Type.Params.List[1].Names[0].Name
	lhs, ok1 := assign.Lhs[0].(*ast.Ident)
	arg, ok2 := call.Args[0].(*ast.Ident)
	return ok1 && ok2 && lhs.Name == param && arg.Name == param
}
