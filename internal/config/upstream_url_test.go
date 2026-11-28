package config

import (
	"strings"
	"testing"
)

func upstreamURLDoc(rawURL string) string {
	return hdr + `kind: Workload
metadata: {name: proxy, zone: runtime}
spec:
  class: long-lived-agent
  runtime: {type: docker, image: "worker:0.1.0"}
  grants:
    mcpServers:
      - {name: first, url: "https://mcp.example.test/mcp", allowedTools: [get]}
      - {name: second, url: "` + rawURL + `", allowedTools: [get]}
`
}

// An upstream MCP server receives the server's secret and the tool
// arguments, so the loader refuses a URL that would send them in plain text
// off the machine. The error names the workload and the field, never the URL.
func TestLoadPathRefusesPlainHTTPUpstreamServerURL(t *testing.T) {
	cred := "hunter" + "2secret"
	field := `workload "proxy": grants.mcpServers[1].url: `
	cases := []struct {
		url     string
		wantErr string // empty: valid
	}{
		{"https://mcp.example.test/mcp", ""},
		{"http://127.0.0.1:8931/mcp", ""},
		{"http://[::1]:8931/mcp", ""},
		{"http://localhost:8931/mcp", ""},
		{"http://mcp.example.test/mcp", field + "plain http is refused"},
		{"http://u:" + cred + "@mcp.example.test/mcp", field + "plain http is refused"},
		{"http://0.0.0.0:8931/mcp", field + "plain http is refused"},
		{"ws://127.0.0.1:8931/mcp", field + "only https"},
		{"mcp.example.test/mcp", field + "the URL has no scheme"},
		{"https:///mcp", field + "the URL has no host"},
		{"", ""}, // an entry without a URL is skipped at registration
	}
	for _, tc := range cases {
		t.Run(tc.url, func(t *testing.T) {
			_, err := LoadPath(writeTempManifest(t, upstreamURLDoc(tc.url)))
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("LoadPath = %v, want valid", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("LoadPath = %v, want %q", err, tc.wantErr)
			}
			if strings.Contains(err.Error(), cred) || strings.Contains(err.Error(), "mcp.example.test") {
				t.Fatalf("LoadPath error %q carries the URL", err)
			}
		})
	}
}
