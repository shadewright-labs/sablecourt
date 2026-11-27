package ui

import (
	"context"
	"net/url"
	"strings"
	"testing"

	"github.com/shadewright-labs/sablecourt/internal/contextpack"
)

// The /providers form shows each route's family and posts it back, so saving
// the form keeps a family the operator set.
func TestProviderTiersFormKeepsFamily(t *testing.T) {
	ctx := context.Background()
	st := newUITestStore(t)
	if err := contextpack.SaveModelTiers(ctx, st, []contextpack.TierRoute{{Tier: "fast", Provider: "local", Model: "llama-3", Family: "meta"}}); err != nil {
		t.Fatal(err)
	}
	mux := newFormMux(t, Options{Store: st})
	body := getPage(t, mux, "/providers").Body.String()
	if !strings.Contains(body, `name="tier_fast_family"`) || !strings.Contains(body, `value="meta"`) {
		t.Fatal("providers page does not show the fast route's family")
	}

	cases := []struct {
		name       string
		family     string
		wantErr    string
		wantFamily string
	}{
		{"family kept", "meta", "", "meta"},
		{"family changed", "Local", "", "local"},
		{"family cleared", "", "", ""},
		{"disagreeing family refused", "anthropic", "model_family_override_conflict", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			model := "llama-3"
			if tc.wantErr != "" {
				model = "gpt-6.2-astra"
			}
			path, notice, errMsg := redirectTarget(t, postForm(t, mux, "/ui/providers/tiers", url.Values{
				"tier_fast_provider": {"local"}, "tier_fast_model": {model}, "tier_fast_family": {tc.family},
				"tier_coding_provider": {"cli:claude"}, "tier_coding_model": {"claude-sonnet-5-5"},
				"tier_critic_provider": {"cli:codex"}, "tier_critic_model": {"gpt-6.2-astra"},
			}))
			if tc.wantErr != "" {
				if path != "/providers" || !strings.Contains(errMsg, tc.wantErr) {
					t.Fatalf("%s notice=%q err=%q, want error %q", path, notice, errMsg, tc.wantErr)
				}
				return
			}
			if path != "/providers" || errMsg != "" || notice == "" {
				t.Fatalf("%s notice=%q err=%q", path, notice, errMsg)
			}
			for _, r := range contextpack.GetModelTiers(ctx, st) {
				if r.Tier == "fast" && (r.Model != "llama-3" || r.Family != tc.wantFamily) {
					t.Fatalf("stored %+v, want family %q", r, tc.wantFamily)
				}
			}
		})
	}
}
