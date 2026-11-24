package contextpack

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/shadewright-labs/sablecourt/internal/domain"
	"github.com/shadewright-labs/sablecourt/internal/store"
)

const ModelTiersNamespace = "system:model-tiers"

type TierRoute struct {
	Tier     string `json:"tier"`
	Provider string `json:"provider"`
	Model    string `json:"model"`
}

var DefaultModelTiers = []TierRoute{
	{Tier: "fast", Provider: "cli:agy", Model: "gemini-3.8-flash-high"},
	{Tier: "coding", Provider: "cli:claude", Model: "claude-sonnet-5-5"},
	{Tier: "critic", Provider: "cli:claude", Model: "claude-opus-5-5"},
}

// GetModelTiers loads configured role-based model routes (fast, coding, critic) from Court Memory,
// falling back to DefaultModelTiers for any unset tier.
func GetModelTiers(ctx context.Context, st store.Store) []TierRoute {
	byTier := make(map[string]TierRoute, len(DefaultModelTiers))
	for _, d := range DefaultModelTiers {
		byTier[d.Tier] = d
	}
	if st != nil {
		if entries, err := st.SearchMemory(ctx, ModelTiersNamespace, "", 20); err == nil {
			for _, e := range entries {
				var r TierRoute
				if err := json.Unmarshal([]byte(e.Value), &r); err == nil && r.Tier != "" {
					byTier[r.Tier] = r
				}
			}
		}
	}
	order := []string{"fast", "coding", "critic"}
	out := make([]TierRoute, 0, len(order))
	for _, t := range order {
		if r, ok := byTier[t]; ok {
			out = append(out, r)
		}
	}
	return out
}

// SaveModelTiers persists role-based model routes into Court Memory.
func SaveModelTiers(ctx context.Context, st store.Store, routes []TierRoute) error {
	if st == nil {
		return nil
	}
	now := time.Now().UTC()
	for _, r := range routes {
		t := strings.ToLower(strings.TrimSpace(r.Tier))
		if t != "fast" && t != "coding" && t != "critic" {
			continue
		}
		r.Tier = t
		r.Provider = strings.TrimSpace(r.Provider)
		r.Model = strings.TrimSpace(r.Model)
		raw, err := json.Marshal(r)
		if err != nil {
			return err
		}
		if err := st.PutMemory(ctx, domain.MemoryEntry{
			Namespace: ModelTiersNamespace,
			Key:       t,
			Value:     string(raw),
			Author:    "operator",
			UpdatedAt: now,
		}); err != nil {
			return err
		}
	}
	return nil
}

// ResolveTier returns the configured provider and model for a tier ("fast", "coding", "critic").
func ResolveTier(ctx context.Context, st store.Store, tier string) (string, string) {
	t := strings.ToLower(strings.TrimSpace(tier))
	if t == "" {
		return "", ""
	}
	for _, r := range GetModelTiers(ctx, st) {
		if r.Tier == t {
			return r.Provider, r.Model
		}
	}
	return "", ""
}
