package store_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/shadewright-labs/sablecourt/internal/domain"
	"github.com/shadewright-labs/sablecourt/internal/store"
)

// searchStore returns a prefix of its rows, like a store that applies LIMIT before the route policy.
type searchStore struct {
	store.Store
	rows   []domain.MemoryEntry
	limits []int
	err    error
}

func (s *searchStore) SearchMemory(_ context.Context, _, _ string, limit int) ([]domain.MemoryEntry, error) {
	s.limits = append(s.limits, limit)
	if s.err != nil {
		return nil, s.err
	}
	return s.rows[:min(limit, len(s.rows))], nil
}

func memoryRows(namespaces ...string) []domain.MemoryEntry {
	out := make([]domain.MemoryEntry, len(namespaces))
	for i, ns := range namespaces {
		out[i] = domain.MemoryEntry{Namespace: ns, Key: fmt.Sprint("k", i)}
	}
	return out
}

func keys(entries []domain.MemoryEntry) string {
	var out []string
	for _, e := range entries {
		out = append(out, e.Key)
	}
	return strings.Join(out, ",")
}

func TestSearchVisibleMemory(t *testing.T) {
	hidden := func(ns string) bool { return store.IsLearningNamespace(ns) }
	for _, tc := range []struct {
		name      string
		namespace string
		rows      []domain.MemoryEntry
		limit     int
		want      string
		fetches   []int
	}{
		{name: "hidden namespace is never queried", namespace: " Learning:ops", rows: memoryRows("a"), limit: 5, want: "", fetches: nil},
		{name: "no hidden rows", rows: memoryRows("a", "b", "c"), limit: 2, want: "k0,k1", fetches: []int{2}},
		{name: "short result", rows: memoryRows("a"), limit: 3, want: "k0", fetches: []int{3}},
		{name: "default limit", rows: memoryRows("a"), limit: 0, want: "k0", fetches: []int{50}},
		{name: "negative limit uses default", rows: memoryRows("a"), limit: -4, want: "k0", fetches: []int{50}},
		{name: "hidden rows widen the fetch",
			rows:  memoryRows("learning:x", "learning:y", "a", "learning:z", "b", "c"),
			limit: 2, want: "k2,k4", fetches: []int{2, 4, 8}},
		{name: "all hidden", rows: memoryRows("learning:a", "LEARNING:b", "learning:c"), limit: 2, want: "", fetches: []int{2, 4}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := &searchStore{rows: tc.rows}
			got, err := store.SearchVisibleMemory(t.Context(), st, tc.namespace, "q", tc.limit, hidden)
			if err != nil {
				t.Fatal(err)
			}
			if got == nil || keys(got) != tc.want {
				t.Fatalf("SearchVisibleMemory = %v (%q), want %q", got, keys(got), tc.want)
			}
			for _, e := range got {
				if hidden(e.Namespace) {
					t.Fatalf("hidden row returned: %+v", e)
				}
			}
			if fmt.Sprint(st.limits) != fmt.Sprint(tc.fetches) {
				t.Fatalf("fetch limits = %v, want %v", st.limits, tc.fetches)
			}
		})
	}

	errSearch := errors.New("search failed")
	if got, err := store.SearchVisibleMemory(t.Context(), &searchStore{err: errSearch}, "", "q", 1, hidden); !errors.Is(err, errSearch) || got != nil {
		t.Fatalf("store error = %v, %v", got, err)
	}
}
