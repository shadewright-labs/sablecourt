package docker

import (
	"os"
	"path/filepath"
	"testing"

	rt "github.com/shadewright-labs/sablecourt/internal/runtime"
)

// hostedEngine gives the fake engine the DaemonHost method the real client has.
type hostedEngine struct {
	*councilEngine
	host string
}

func (h hostedEngine) DaemonHost() string { return h.host }

// TestHarnessContainerSpecRefusesSocketsTheClientDials checks that the socket
// guard reads the endpoint the way the Moby client does: ParseHostURL for
// DOCKER_HOST (everything after unix:// is a literal path, so #, ? and %20
// stay in it), and the client's own resolved host when DOCKER_HOST is unset.
func TestHarnessContainerSpecRefusesSocketsTheClientDials(t *testing.T) {
	cases := []struct {
		name      string
		dir       string
		sockPath  string
		envHost   func(sock string) string
		clientURL func(sock string) string
	}{
		{name: "client host only", dir: "client", sockPath: "custom/path.sock", clientURL: func(s string) string { return "unix://" + s }},
		{name: "env hash in path", dir: "a#b", sockPath: "d.sock", envHost: func(s string) string { return "unix://" + s }},
		{name: "env question mark in path", dir: "a?b", sockPath: "d.sock", envHost: func(s string) string { return "unix://" + s }},
		{name: "env percent in path", dir: "a%20b", sockPath: "d.sock", envHost: func(s string) string { return "unix://" + s }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			checkout := filepath.Join(t.TempDir(), tc.dir)
			t.Setenv("HOME", t.TempDir())
			t.Setenv("SABLECOURT_STATE_DIR", filepath.Join(t.TempDir(), "state"))
			t.Setenv("DOCKER_HOST", "")
			sock := filepath.Join(checkout, tc.sockPath)
			if err := os.MkdirAll(filepath.Dir(sock), 0o700); err != nil {
				t.Fatal(err)
			}
			f := newCouncilEngine()
			engine := hostedEngine{councilEngine: f}
			if tc.envHost != nil {
				t.Setenv("DOCKER_HOST", tc.envHost(sock))
			}
			if tc.clientURL != nil {
				engine.host = tc.clientURL(sock)
			}
			a := NewAdapter(engine, "")
			a.discoverSocket = func() string { return "" }
			a.CouncilHarnesses = map[string]CouncilHarness{"claude": {Image: "fixture", LoginVolume: f.vol.Name, ProxyURL: "http://relay:8888", DataGateURL: "http://relay:8888/gate/mcp"}}
			s := rt.CouncilLaunch{TaskID: "task-sock", Workload: "guard", Harness: "claude", Checkout: rt.CouncilCheckout{Path: checkout, Kind: rt.CouncilTask},
				Limits: rt.CouncilLimits{NanoCPUs: 1, MemoryBytes: 1 << 20, PIDs: 1, WallTime: 1e9}, Prompt: "p", TaskToken: "lease"}
			if _, err := a.harnessContainerSpec(t.Context(), engine, s, false); err == nil {
				t.Fatalf("checkout holding the socket %q was accepted", sock)
			}
		})
	}
}
