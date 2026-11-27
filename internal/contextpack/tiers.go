package contextpack

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/shadewright-labs/sablecourt/internal/domain"
	"github.com/shadewright-labs/sablecourt/internal/modelfamily"
	"github.com/shadewright-labs/sablecourt/internal/store"
)

const ModelTiersNamespace = "system:model-tiers"

// TierRoute maps a tier to a provider and model. Family is an operator's
// model family for a model the built-in rules of internal/modelfamily do not
// recognize; a family that disagrees with those rules is refused on save.
type TierRoute struct {
	Tier     string `json:"tier"`
	Provider string `json:"provider"`
	Model    string `json:"model"`
	Family   string `json:"family,omitempty"`
}

// DefaultModelTiers are the shipped routes. The critic route runs a model
// family (OpenAI) other than the coding route's (Anthropic), so a verifier
// step on the critic tier can follow an author on the coding tier.
var DefaultModelTiers = []TierRoute{
	{Tier: "fast", Provider: "cli:agy", Model: "gemini-3.8-flash-high"},
	{Tier: "coding", Provider: "cli:claude", Model: "claude-sonnet-5-5"},
	{Tier: "critic", Provider: "cli:codex", Model: "gpt-6.2-astra"},
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

// SaveModelTiers persists role-based model routes into Court Memory. A route
// whose family is malformed or disagrees with the built-in family of its
// model refuses the whole save before anything is written.
func SaveModelTiers(ctx context.Context, st store.Store, routes []TierRoute) error {
	if st == nil {
		return nil
	}
	clean := make([]TierRoute, 0, len(routes))
	for _, r := range routes {
		t := strings.ToLower(strings.TrimSpace(r.Tier))
		if t != "fast" && t != "coding" && t != "critic" {
			continue
		}
		r.Tier = t
		r.Provider = strings.TrimSpace(r.Provider)
		r.Model = strings.TrimSpace(r.Model)
		family, err := modelfamily.NormalizeOverride(r.Family)
		if err != nil {
			return fmt.Errorf("tier %s: %w", t, err)
		}
		r.Family = family
		if family != "" {
			if _, err := modelfamily.Classify(r.Model, family); err != nil {
				return fmt.Errorf("tier %s: %w", t, err)
			}
		}
		clean = append(clean, r)
	}
	now := time.Now().UTC()
	for _, r := range clean {
		t := r.Tier
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

// IsRouteRefusal reports whether an error from SaveModelTiers refused a route
// the caller sent, as opposed to a store failure.
func IsRouteRefusal(err error) bool { return modelfamily.ReasonOf(err) != "" }

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
