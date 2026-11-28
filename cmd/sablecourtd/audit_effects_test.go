package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/shadewright-labs/sablecourt/internal/store"
	"github.com/shadewright-labs/sablecourt/internal/store/audittest"
)

// apiCallTracked is apiCall with an effects recorder on the request; it
// returns the effects the request recorded.
func apiCallTracked(h http.Handler, method, path, body string) (*httptest.ResponseRecorder, []string) {
	req := operatorRequest(method, path, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	ctx, effects := store.TrackEffects(req.Context())
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req.WithContext(ctx))
	return rec, effects.List()
}

// taskIDs returns the IDs of the tasks in st.
func taskIDs(t *testing.T, st store.Store) map[string]bool {
	t.Helper()
	tasks, err := st.ListTasks(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]bool{}
	for _, task := range tasks {
		ids[task.ID] = true
	}
	return ids
}

// newTaskID returns the one task in st that before does not hold, or "".
func newTaskID(t *testing.T, st store.Store, before map[string]bool) string {
	t.Helper()
	for id := range taskIDs(t, st) {
		if !before[id] {
			return id
		}
	}
	return ""
}

// requireAuditAnswer checks that rec is a 500 whose error lists exactly the
// effects the request recorded (got), and that those are want with each key
// of vars replaced by its value.
func requireAuditAnswer(t *testing.T, rec *httptest.ResponseRecorder, what string, got, want []string, vars map[string]string) {
	t.Helper()
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("got %d %s, want 500", rec.Code, rec.Body.String())
	}
	if msg := apiError(t, rec); msg != audittest.Answer(what, got...) {
		t.Fatalf("answer %q does not list the recorded effects %q", msg, got)
	}
	var expanded []string
	for _, w := range want {
		for k, v := range vars {
			w = strings.ReplaceAll(w, k, v)
		}
		expanded = append(expanded, w)
	}
	if !slices.Equal(got, expanded) {
		t.Fatalf("effects %q, want %q", got, expanded)
	}
}
