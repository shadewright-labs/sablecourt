package store

import (
	"context"
	"math"
	"strings"

	"github.com/shadewright-labs/sablecourt/internal/domain"
)

func IsLearningNamespace(ns string) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(ns)), "learning:")
}

// SearchVisibleMemory applies the route's namespace policy before counting the result limit.
func SearchVisibleMemory(ctx context.Context, st Store, namespace, query string, limit int, hidden func(string) bool) ([]domain.MemoryEntry, error) {
	if hidden(namespace) {
		return []domain.MemoryEntry{}, nil
	}
	if limit <= 0 {
		limit = 50
	}
	fetchLimit := limit
	for {
		items, err := st.SearchMemory(ctx, namespace, query, fetchLimit)
		if err != nil {
			return nil, err
		}
		visible := make([]domain.MemoryEntry, 0, min(len(items), limit))
		for _, item := range items {
			if !hidden(item.Namespace) {
				visible = append(visible, item)
				if len(visible) == limit {
					return visible, nil
				}
			}
		}
		if len(items) < fetchLimit || fetchLimit == math.MaxInt {
			return visible, nil
		}
		// The store limits all matches. Fetch a larger prefix when hidden rows fill part of it.
		fetchLimit += min(fetchLimit, math.MaxInt-fetchLimit)
	}
}
