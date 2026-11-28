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
// method of the store interfaces that is not listed as a read here must call
// NoteEffectIf or NoteEffectWhen in every backend that implements it.
func TestStoreBackendsNoteEveryWrite(t *testing.T) {
	reads := map[string]bool{
		"Close": true, "Migrate": true, "AppendAudit": true, "ListAudit": true, "VerifyAuditChain": true,
		"ListWorkloads": true, "GetTask": true, "GetTaskByIdempotencyKey": true, "ListTasks": true,
		"ListReconcileTasks": true, "ListTaskEvents": true, "ListTaskMessages": true,
		"GetApproval": true, "ListApprovals": true, "GetActiveLeaseByTokenHash": true,
		"TaskBudgetTotals": true, "WorkloadDailyBudgetTotals": true, "TotalBudgetTotals": true,
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
	for _, backend := range []string{"sqlite", "postgres"} {
		noted := map[string]bool{}
		files, err := filepath.Glob(filepath.Join(backend, "*.go"))
		if err != nil {
			t.Fatal(err)
		}
		fset := token.NewFileSet()
		implemented := map[string]bool{}
		for _, path := range files {
			if strings.HasSuffix(path, "_test.go") {
				continue
			}
			f, err := parser.ParseFile(fset, path, nil, 0)
			if err != nil {
				t.Fatal(err)
			}
			for _, decl := range f.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				if !ok || fn.Recv == nil || !writes[fn.Name.Name] {
					continue
				}
				implemented[fn.Name.Name] = true
				ast.Inspect(fn.Body, func(n ast.Node) bool {
					if sel, ok := n.(*ast.SelectorExpr); ok && (sel.Sel.Name == "NoteEffectIf" || sel.Sel.Name == "NoteEffectWhen") {
						noted[fn.Name.Name] = true
					}
					return true
				})
			}
		}
		if len(implemented) == 0 {
			t.Fatalf("%s: no store methods found", backend)
		}
		for name := range implemented {
			if !noted[name] {
				t.Errorf("%s: %s writes but notes no effect", backend, name)
			}
		}
	}
}

type failingAppender struct{}

func (failingAppender) AppendAudit(context.Context, domain.AuditRecord) error { return ErrConflict }
