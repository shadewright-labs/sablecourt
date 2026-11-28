package main

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/shadewright-labs/sablecourt/internal/contextpack"
	"github.com/shadewright-labs/sablecourt/internal/domain"
	"github.com/shadewright-labs/sablecourt/internal/store"
	"github.com/shadewright-labs/sablecourt/internal/store/audittest"
)

// Every daemon route that changes Court Memory writes its audit row in the
// same transaction as the change: a row that cannot be written refuses the
// change with HTTP 500 and changes nothing, and a change that commits leaves
// its row (issue 228).
func TestMemoryRoutesAuditInSameTransaction(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name, action, method, path, body, what string
		// stored reports whether the change the route makes is in the store.
		stored func(t *testing.T, st store.Store) bool
	}{
		{"memory put", "memory.put", http.MethodPost, "/api/v1/memory", `{"namespace":"notes","key":"new","value":"v"}`, "memory write",
			func(t *testing.T, st store.Store) bool {
				_, err := st.GetMemory(ctx, "notes", "new")
				return err == nil
			}},
		{"memory delete", "memory.delete", http.MethodDelete, "/api/v1/memory/notes/old", "", "memory delete",
			func(t *testing.T, st store.Store) bool {
				_, err := st.GetMemory(ctx, "notes", "old")
				return errors.Is(err, store.ErrNotFound)
			}},
		{"model tiers save", "model_tiers.save", http.MethodPost, "/api/v1/providers/tiers", `[{"tier":"fast","provider":"local","model":"llama-3"}]`, "model tier save",
			func(t *testing.T, st store.Store) bool {
				_, err := st.GetMemory(ctx, contextpack.ModelTiersNamespace, "fast")
				return err == nil
			}},
	}
	for _, tc := range cases {
		for _, failing := range []bool{true, false} {
			name := tc.name + " commits with its row"
			if failing {
				name = tc.name + " is refused without its row"
			}
			t.Run(name, func(t *testing.T) {
				var faults *audittest.Store
				env := newAPIEnv(t, apiEnvConfig{wrap: func(in store.Store) store.Store {
					faults = audittest.Wrap(in)
					return faults
				}})
				if err := env.st.PutMemory(ctx, domain.MemoryEntry{Namespace: "notes", Key: "old", Value: "v"}); err != nil {
					t.Fatal(err)
				}
				if failing {
					faults.Fail(audittest.Action(tc.action))
				}
				rec, effects := apiCallTracked(env.h, tc.method, tc.path, tc.body)
				if failing {
					requireAuditAnswer(t, rec, tc.what, effects, nil, nil)
					if len(faults.Refused()) == 0 {
						t.Fatal("no audit write was attempted")
					}
					if tc.stored(t, env.st) {
						t.Fatal("memory changed without its audit row")
					}
					return
				}
				if rec.Code >= 300 {
					t.Fatalf("got %d %s", rec.Code, rec.Body.String())
				}
				if !tc.stored(t, env.st) {
					t.Fatal("change not stored")
				}
				rows, err := env.st.ListAudit(ctx, 50)
				if err != nil {
					t.Fatal(err)
				}
				found := false
				for _, r := range rows {
					if r.Category == "memory" && r.Action == tc.action && r.WorkloadName == "operator" {
						found = true
					}
				}
				if !found {
					t.Fatalf("no memory row %s in %+v", tc.action, rows)
				}
			})
		}
	}
}

// A delete of a missing entry answers 404 and writes no row.
func TestMemoryDeleteMissingWritesNoRow(t *testing.T) {
	env := newAPIEnv(t, apiEnvConfig{})
	before, err := env.st.ListAudit(context.Background(), 500)
	if err != nil {
		t.Fatal(err)
	}
	rec := apiCall(env.h, http.MethodDelete, "/api/v1/memory/notes/absent", "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("got %d %s, want 404", rec.Code, rec.Body.String())
	}
	after, err := env.st.ListAudit(context.Background(), 500)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range after[:len(after)-len(before)] {
		if r.Category == "memory" {
			t.Fatalf("missing entry left a row: %+v", r)
		}
	}
}
