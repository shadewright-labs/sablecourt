package contextpack

import (
	"context"
	"fmt"
	"path"
	"strings"
	"time"

	"github.com/shadewright-labs/sablecourt/internal/domain"
	"github.com/shadewright-labs/sablecourt/internal/redact"
	"github.com/shadewright-labs/sablecourt/internal/store"
)

const (
	LearningNamespaceGlobal     = "learning:global"
	LearningNamespaceRepoPrefix = "learning:repo:"
	LearningNamespacePathPrefix = "learning:path:"
)

// NormalizeLearningNamespace converts user-facing scopes ("global", "repo:foo", "path:internal/**")
// into reserved Court Memory learning namespaces.
func NormalizeLearningNamespace(scope string) string {
	s := strings.TrimSpace(scope)
	if s == "" || s == "global" || s == LearningNamespaceGlobal {
		return LearningNamespaceGlobal
	}
	if strings.HasPrefix(s, "learning:") {
		return s
	}
	if strings.HasPrefix(s, "repo:") {
		return LearningNamespaceRepoPrefix + strings.TrimSpace(strings.TrimPrefix(s, "repo:"))
	}
	if strings.HasPrefix(s, "path:") {
		return LearningNamespacePathPrefix + strings.TrimSpace(strings.TrimPrefix(s, "path:"))
	}
	return "learning:" + s
}

// PutLearning redacts and persists a scoped learning entry in Court Memory.
func PutLearning(ctx context.Context, st store.Store, scope, key, rule, author string) (domain.MemoryEntry, error) {
	if st == nil {
		return domain.MemoryEntry{}, fmt.Errorf("store is unavailable")
	}
	ns := NormalizeLearningNamespace(scope)
	k := strings.TrimSpace(key)
	v := strings.TrimSpace(rule)
	if k == "" || v == "" {
		return domain.MemoryEntry{}, fmt.Errorf("learning key and rule are required")
	}
	if strings.TrimSpace(author) == "" {
		author = "operator"
	}
	entry := domain.MemoryEntry{
		Namespace: ns,
		Key:       k,
		Value:     redact.String(v),
		Author:    author,
		UpdatedAt: time.Now().UTC(),
	}
	if err := st.PutMemory(ctx, entry); err != nil {
		return domain.MemoryEntry{}, err
	}
	return entry, nil
}

// MatchLearnings returns global, repo-scoped, and path-glob-scoped learnings relevant to a task.
func MatchLearnings(ctx context.Context, st store.Store, repoName string, changedPaths []string, limit int) []domain.MemoryEntry {
	if st == nil {
		return nil
	}
	if limit <= 0 {
		limit = 20
	}
	entries, err := st.SearchMemory(ctx, "", "", 200)
	if err != nil {
		return nil
	}
	repoNS := ""
	if strings.TrimSpace(repoName) != "" {
		repoNS = LearningNamespaceRepoPrefix + strings.TrimSpace(repoName)
	}

	var matched []domain.MemoryEntry
	for _, e := range entries {
		if !strings.HasPrefix(e.Namespace, "learning:") {
			continue
		}
		include := false
		switch {
		case e.Namespace == LearningNamespaceGlobal:
			include = true
		case repoNS != "" && e.Namespace == repoNS:
			include = true
		case strings.HasPrefix(e.Namespace, LearningNamespacePathPrefix):
			glob := strings.TrimPrefix(e.Namespace, LearningNamespacePathPrefix)
			if matchAnyPath(glob, changedPaths) {
				include = true
			}
		}
		if include {
			matched = append(matched, e)
			if len(matched) >= limit {
				break
			}
		}
	}
	return matched
}

func matchAnyPath(glob string, paths []string) bool {
	glob = strings.TrimSpace(glob)
	if glob == "" {
		return false
	}
	if len(paths) == 0 {
		return false
	}
	// Support "**" recursive prefix patterns like "internal/gate/**".
	if strings.HasSuffix(glob, "/**") {
		prefix := strings.TrimSuffix(glob, "/**")
		for _, p := range paths {
			p = strings.TrimPrefix(strings.TrimSpace(p), "./")
			if p == prefix || strings.HasPrefix(p, prefix+"/") {
				return true
			}
		}
		return false
	}
	for _, p := range paths {
		p = strings.TrimPrefix(strings.TrimSpace(p), "./")
		if ok, _ := path.Match(glob, p); ok {
			return true
		}
		if ok, _ := path.Match(glob, path.Base(p)); ok {
			return true
		}
	}
	return false
}
