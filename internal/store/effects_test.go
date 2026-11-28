package store

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/shadewright-labs/sablecourt/internal/domain"
)

func TestAuditFailureText(t *testing.T) {
	refused := RecordAudit(context.Background(), failingAppender{}, domain.AuditRecord{Action: "task.submit", Outcome: "submitted"})
	tracked := func(effects ...string) context.Context {
		ctx, _ := TrackEffects(context.Background())
		for _, e := range effects {
			NoteEffect(ctx, "%s", e)
		}
		return ctx
	}
	cases := []struct {
		name   string
		ctx    context.Context
		err    error
		want   string
		wantOK bool
	}{
		{"nil", tracked(), nil, "", false},
		{"other error", tracked("task t1 was created"), ErrConflict, "", false},
		{"no effect", tracked(), refused, "task submission refused: its audit record could not be written, nothing was changed", true},
		{"one effect", tracked("task t1 was created"), refused, "task t1 was created, but an audit record could not be written", true},
		{"effects in order", tracked("conclave c1 was opened", "conclave c1 moved to cancelled"), refused, "conclave c1 was opened; conclave c1 moved to cancelled, but an audit record could not be written", true},
		{"not tracked", context.Background(), refused, "task submission failed: an audit record could not be written, and what had already changed was not tracked", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := AuditFailureText(tc.ctx, tc.err, "task submission")
			if got != tc.want || ok != tc.wantOK {
				t.Fatalf("got %q %v, want %q %v", got, ok, tc.want, tc.wantOK)
			}
		})
	}
}

func TestTrackEffectsKeepsOneRecorderPerRequest(t *testing.T) {
	ctx, outer := TrackEffects(context.Background())
	inner, again := TrackEffects(ctx)
	NoteEffect(inner, "task %s was created", "t1")
	NoteEffectIf(inner, ErrConflict, "task %s was created", "t2")
	NoteEffectWhen(inner, false, nil, "task %s was created", "t3")
	if outer != again || !reflect.DeepEqual(outer.List(), []string{"task t1 was created"}) {
		t.Fatalf("recorders %p %p, effects %q", outer, again, outer.List())
	}
	NoteEffect(context.Background(), "no recorder")
}

// Every store method that writes notes its effect in each backend, so a
// route's audit failure answer cannot leave out a change the store made. A
// method of the store interfaces that is not listed as a read here must note
// an effect in every backend that implements it: by calling a NoteEffect
// function, or a function of the store package or of the backend that does
// (the guarded conclave paths note each change inside their transaction).
func TestStoreBackendsNoteEveryWrite(t *testing.T) {
	reads := map[string]bool{
		"Close": true, "Migrate": true, "AppendAudit": true, "ListAudit": true, "VerifyAuditChain": true,
		"ListWorkloads": true, "GetTask": true, "GetTaskByIdempotencyKey": true, "ListTasks": true,
		"ListReconcileTasks": true, "ListTaskEvents": true, "ListTaskMessages": true,
		"GetApproval": true, "ListApprovals": true, "GetActiveLeaseByTokenHash": true,
		"TaskBudgetTotals": true, "TaskLedgerModels": true, "WorkloadDailyBudgetTotals": true, "TotalBudgetTotals": true,
		"LatestCheckpoint": true, "ListProviderPrefs": true, "GetConclave": true, "ListConclaves": true,
		"ListConclaveMessages": true, "GetConclaveRelic": true, "ListConclaveRelics": true,
		"GetDevice": true, "ListDevices": true, "GetMemory": true, "SearchMemory": true,
		"CouncilByIdempotencyKey": true, "ListReconcileCouncils": true, "CouncilSnapshot": true,
		"GetLearning": true, "ListLearnings": true, "ListLearningHistory": true, "ListLearningOutcomes": true,
	}
	writes := map[string]bool{}
	for _, iface := range []reflect.Type{
		reflect.TypeFor[Store](), reflect.TypeFor[CouncilStore](), reflect.TypeFor[LearningStore](),
	} {
		for i := range iface.NumMethod() {
			if name := iface.Method(i).Name; !reads[name] {
				writes[name] = true
			}
		}
	}
	// A store function that passes a write on to a store method (the guarded
	// conclave paths) notes through that method, which is checked below.
	storeNoting := notingFuncs(t, ".", "store.", writes)
	for _, backend := range []string{"sqlite", "postgres"} {
		noting := notingFuncs(t, backend, "", storeNoting)
		implemented := map[string]bool{}
		for _, fn := range parseFuncs(t, backend) {
			if fn.Recv != nil && writes[fn.Name.Name] {
				implemented[fn.Name.Name] = true
			}
		}
		if len(implemented) == 0 {
			t.Fatalf("%s: no store methods found", backend)
		}
		for name := range implemented {
			if !noting[name] {
				t.Errorf("%s: %s writes but notes no effect", backend, name)
			}
		}
	}
}

var noteFuncs = map[string]bool{"NoteEffect": true, "NoteEffectIf": true, "NoteEffectWhen": true, "NoteEffectRows": true}

func parseFuncs(t *testing.T, dir string) []*ast.FuncDecl {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(dir, "*.go"))
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	var out []*ast.FuncDecl
	for _, path := range files {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range f.Decls {
			if fn, ok := decl.(*ast.FuncDecl); ok && fn.Body != nil {
				out = append(out, fn)
			}
		}
	}
	return out
}

// notingFuncs returns the names, each with prefix, of the functions in dir
// that note an effect, directly or through another function of dir or one
// named in outer (keys "store.Name", or store method names for dir ".").
func notingFuncs(t *testing.T, dir, prefix string, outer map[string]bool) map[string]bool {
	t.Helper()
	funcs := parseFuncs(t, dir)
	noting := map[string]bool{}
	for changed := true; changed; {
		changed = false
		for _, fn := range funcs {
			name := prefix + fn.Name.Name
			if noting[name] {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				var callee string
				switch f := call.Fun.(type) {
				case *ast.Ident:
					callee = f.Name
				case *ast.SelectorExpr:
					callee = f.Sel.Name
					if pkg, ok := f.X.(*ast.Ident); ok && pkg.Name == "store" {
						if noteFuncs[callee] || outer["store."+callee] {
							noting[name] = true
						}
						return true
					}
				}
				if noteFuncs[callee] || noting[prefix+callee] || dir == "." && outer[callee] {
					noting[name] = true
				}
				return true
			})
			changed = changed || noting[name]
		}
	}
	return noting
}

// Every store transaction commits through Commit, which releases the effects
// it held only once the commit succeeds. A bare tx.Commit in a store package
// would let an effect noted inside its transaction stay held, or reach the
// recorder before the change is durable.
func TestStoreCommitsOnlyThroughCommit(t *testing.T) {
	for _, dir := range []string{".", "sqlite", "postgres"} {
		for _, fn := range parseFuncs(t, dir) {
			if dir == "." && fn.Recv == nil && fn.Name.Name == "Commit" {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok || sel.Sel.Name != "Commit" || len(call.Args) != 0 {
					return true
				}
				t.Errorf("%s: %s commits a transaction without store.Commit", dir, fn.Name.Name)
				return true
			})
		}
	}
}

type failingAppender struct{}

func (failingAppender) AppendAudit(context.Context, domain.AuditRecord) error { return ErrConflict }

// A store function that commits a transaction and notes its effect in a
// deferred call returns success only after the commit: a nil error returned
// before it (a replay that changed nothing) would note an effect for a write
// that did not happen.
func TestDeferredEffectsFollowCommit(t *testing.T) {
	for _, dir := range []string{".", "sqlite", "postgres"} {
		for _, fn := range parseFuncs(t, dir) {
			deferred := false
			commit := token.NoPos
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				switch n := n.(type) {
				case *ast.DeferStmt:
					ast.Inspect(n, func(m ast.Node) bool {
						if call, ok := m.(*ast.CallExpr); ok && calleeName(call) == "NoteEffectIf" {
							deferred = true
						}
						return true
					})
				case *ast.CallExpr:
					if calleeName(n) == "Commit" && len(n.Args) == 2 && (commit == token.NoPos || n.Pos() < commit) {
						commit = n.Pos()
					}
				}
				return true
			})
			if !deferred || commit == token.NoPos {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				if _, ok := n.(*ast.FuncLit); ok {
					return false
				}
				ret, ok := n.(*ast.ReturnStmt)
				if !ok || len(ret.Results) == 0 || ret.Pos() > commit {
					return true
				}
				if id, ok := ret.Results[len(ret.Results)-1].(*ast.Ident); ok && id.Name == "nil" {
					t.Errorf("%s: %s returns success before its commit while a deferred call notes an effect", dir, fn.Name.Name)
				}
				return true
			})
		}
	}
}

func calleeName(call *ast.CallExpr) string {
	switch f := call.Fun.(type) {
	case *ast.Ident:
		return f.Name
	case *ast.SelectorExpr:
		return f.Sel.Name
	}
	return ""
}
