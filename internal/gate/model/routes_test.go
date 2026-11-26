package model

import "testing"

func TestRouteProvider(t *testing.T) {
	for _, tc := range []struct {
		path, provider, upstream string
		ok                       bool
	}{
		{"/openai/v1/chat/completions", "openai", "/v1/chat/completions", true},
		{"/openai/v1/responses", "openai", "/v1/responses", true},
		{"/local/v1/chat/completions", "local", "/v1/chat/completions", true},
		{"/anthropic/v1/messages", "anthropic", "/v1/messages", true},
		{"/gemini/v1beta/models/m:generateContent", "gemini", "/v1beta/models/m:generateContent", true},
		{"/gemini/v1beta/models/m:streamGenerateContent", "gemini", "/v1beta/models/m:streamGenerateContent", true},
		{"/openai/v1/messages", "", "", false},
		{"/anthropic/v1/chat/completions", "", "", false},
		{"/openaix/v1/chat/completions", "", "", false},
		{"/gemini/v1beta/models/m:countTokens", "", "", false},
		{"/mistral/v1/chat/completions", "", "", false},
		{"/", "", "", false},
	} {
		t.Run(tc.path, func(t *testing.T) {
			p, up, ok := routeProvider(tc.path)
			if p != tc.provider || up != tc.upstream || ok != tc.ok {
				t.Fatalf("routeProvider(%q) = %q, %q, %v; want %q, %q, %v", tc.path, p, up, ok, tc.provider, tc.upstream, tc.ok)
			}
		})
	}
}

// Every provider the gate dispatches has a default upstream, so a provider
// added to routes alone is not served with a 502.
func TestEveryRoutedProviderHasAnUpstream(t *testing.T) {
	ups := DefaultUpstreams()
	for _, r := range routes {
		if _, ok := ups[r.provider]; !ok {
			t.Errorf("provider %s is routed but has no default upstream", r.provider)
		}
		if p, _, ok := routeProvider("/" + r.provider + r.path); !ok || p != r.provider {
			t.Errorf("route /%s%s is listed but not dispatched", r.provider, r.path)
		}
	}
}
