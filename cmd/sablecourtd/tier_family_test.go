package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestAPIProviderTiersFamily(t *testing.T) {
	cases := []struct {
		name     string
		body     string
		wantCode int
		wantErr  string
	}{
		{"family for an unknown model", `[{"tier":"fast","provider":"local","model":"llama-3","family":"meta"}]`, http.StatusOK, ""},
		{"agreeing family", `[{"tier":"critic","provider":"cli:codex","model":"gpt-6.2-astra","family":"openai"}]`, http.StatusOK, ""},
		{"family disagrees with the prefix", `[{"tier":"critic","provider":"cli:codex","model":"gpt-6.2-astra","family":"anthropic"}]`, http.StatusBadRequest, "model_family_override_conflict"},
		{"malformed family", `[{"tier":"fast","provider":"local","model":"llama-3","family":"a b"}]`, http.StatusBadRequest, "model_family_override_invalid"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newAPIEnv(t, apiEnvConfig{})
			rec := apiCall(env.h, http.MethodPost, "/api/v1/providers/tiers", tc.body)
			if rec.Code != tc.wantCode {
				t.Fatalf("POST tiers = %d %s, want %d", rec.Code, rec.Body.String(), tc.wantCode)
			}
			if tc.wantErr != "" && !strings.Contains(apiError(t, rec), tc.wantErr) {
				t.Fatalf("error %q lacks %q", rec.Body.String(), tc.wantErr)
			}
			if tc.wantErr != "" {
				return
			}
			var tiers []struct{ Tier, Family string }
			if err := json.Unmarshal(rec.Body.Bytes(), &tiers); err != nil {
				t.Fatal(err)
			}
			var in []struct{ Tier, Family string }
			if err := json.Unmarshal([]byte(tc.body), &in); err != nil {
				t.Fatal(err)
			}
			for _, tr := range tiers {
				if tr.Tier == in[0].Tier && tr.Family != in[0].Family {
					t.Fatalf("stored family %q, want %q", tr.Family, in[0].Family)
				}
			}
		})
	}
}
