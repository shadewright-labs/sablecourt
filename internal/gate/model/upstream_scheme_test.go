package model

import (
	"strings"
	"testing"
)

// TestJoinUpstreamURLRefusesPlainHTTP is the request-time half of the rule
// the daemon applies to --model-gateway-url at start: the gate never builds a
// provider or gateway request that would carry a key or a prompt in plain
// text off the machine, whatever base URL reached it.
func TestJoinUpstreamURLRefusesPlainHTTP(t *testing.T) {
	cases := []struct {
		base string
		want string // "" means accepted
	}{
		{"https://api.provider.example.test", ""},
		{"https://gateway.example.test:8443/route", ""},
		{"http://127.0.0.1:11434", ""},
		{"http://[::1]:3000", ""},
		{"http://localhost:3000", ""},
		{"http://gateway.example.test:3000", "plain http is refused"},
		{"http://agentgateway.sablecourt.svc:3000", "plain http is refused"},
		{"http://10.0.0.7:3000", "plain http is refused"},
		{"ftp://gateway.example.test", "only https"},
		{"gateway.example.test:3000", "only https"},
		{"https://", "no host"},
	}
	for _, tc := range cases {
		got, err := joinUpstreamURL(tc.base, "/v1/messages", "")
		if tc.want == "" {
			if err != nil || !strings.HasSuffix(got, "/v1/messages") {
				t.Errorf("joinUpstreamURL(%q) = %q, %v; want accepted", tc.base, got, err)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("joinUpstreamURL(%q) = %q, %v; want %q", tc.base, got, err, tc.want)
		}
	}
}
