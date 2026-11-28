package config

import (
	"strings"
	"testing"
)

func upstreamServerDoc(serverName string) string {
	return hdr + `kind: Workload
metadata: {name: proxy, zone: runtime}
spec:
  class: long-lived-agent
  runtime: {type: docker, image: "worker:0.1.0"}
  grants:
    mcpServers: [{name: "` + serverName + `", url: "https://mcp.example.test/mcp", secretName: up_token, allowedTools: [get]}]
`
}

// An upstream MCP server is exposed as "<name>.<tool>". A name that equals a
// built-in tool namespace would let the upstream server answer a call meant
// for a built-in tool, so the loader refuses it.
func TestLoadPathRefusesUpstreamServerWithBuiltinNamespace(t *testing.T) {
	cases := []struct {
		server  string
		wantErr string // empty: valid
	}{
		{"memory", `mcpServers name "memory" is a built-in tool namespace`},
		{"conclave", `mcpServers name "conclave" is a built-in tool namespace`},
		{"sablecourt", `mcpServers name "sablecourt" is a built-in tool namespace`},
		{"git", `mcpServers name "git" is a built-in tool namespace`},
		{"sandbox", `mcpServers name "sandbox" is a built-in tool namespace`},
		{"memory.v2", `mcpServers name "memory.v2" is a built-in tool namespace`},
		{"Memory", `mcpServers name "Memory" is a built-in tool namespace`},
		{" memory", `mcpServers name " memory" is a built-in tool namespace`},
		// Neighbours that only share a prefix stay valid.
		{"memoryx", ""},
		{"linear", ""},
		{"conclaves", ""},
	}
	for _, tc := range cases {
		t.Run(tc.server, func(t *testing.T) {
			_, err := LoadPath(writeTempManifest(t, upstreamServerDoc(tc.server)))
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("LoadPath = %v, want valid", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("LoadPath = %v, want %q", err, tc.wantErr)
			}
		})
	}
}
