package docker

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/moby/moby/api/types/mount"
	rt "github.com/shadewright-labs/sablecourt/internal/runtime"
)

// harnessEnvAllowlist is every variable a harness container may receive.
// Anything else in Config.Env fails the guard test, whichever kind of
// workload the container is for.
var harnessEnvAllowlist = map[string]bool{
	"PATH": true, "HOME": true, "TMPDIR": true,
	"HTTP_PROXY": true, "HTTPS_PROXY": true, "http_proxy": true, "https_proxy": true,
	"NO_PROXY": true, "no_proxy": true, "DISABLE_UPDATES": true,
	"SABLECOURT_MANAGED": true, "SABLECOURT_TASK_ID": true, "SABLECOURT_WORKLOAD": true,
	"SABLECOURT_DATA_GATE_URL": true, "GIT_OPTIONAL_LOCKS": true,
}

// TestHarnessContainerSpecGuardsForStandaloneAndCouncil runs a standalone
// coding task and every council seat kind through the one container spec
// function and applies the same guards to each (issue 181).
func TestHarnessContainerSpecGuardsForStandaloneAndCouncil(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	stateDir := filepath.Join(home, ".local", "share", "sablecourt")
	t.Setenv("SABLECOURT_STATE_DIR", "")
	t.Setenv("DOCKER_HOST", "")
	// Daemon-held secrets, each a distinct canary, seeded where a careless
	// spec could pick them up: the daemon environment and the state directory.
	canaries := map[string]string{
		"operator token":     "sc_op_" + "canaryoperator0001",
		"anthropic key":      "sk-ant-" + "canaryprovider0002",
		"openai key":         "sk-" + "canaryprovider0003",
		"gemini key":         "AIza" + "canaryprovider0004",
		"upstream mcp":       "mcp-" + "canaryupstream0005",
		"webhook secret":     "whsec_" + "canarywebhook0006",
		"another task lease": "sc_task_" + "canaryotherlease07",
	}
	for env, name := range map[string]string{
		"SABLECOURT_OPERATOR_TOKEN": "operator token", "ANTHROPIC_API_KEY": "anthropic key", "OPENAI_API_KEY": "openai key",
		"GEMINI_API_KEY": "gemini key", "SABLECOURT_MCP_UPSTREAM_TOKEN": "upstream mcp", "SABLECOURT_WEBHOOK_SECRET": "webhook secret",
		"SABLECOURT_TASK_TOKEN": "another task lease",
	} {
		t.Setenv(env, canaries[name])
	}
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stateDir, "operator.token"), []byte(canaries["operator token"]), 0o600); err != nil {
		t.Fatal(err)
	}
	const (
		lease  = "sc_task_" + "fixturelease0123456789"
		prompt = "fixture prompt for the harness"
		proxy  = "http://sablecourt-proxy-launch:8888"
		gate   = proxy + "/gate/mcp"
		netNm  = "sablecourt-net-launch"
	)
	for _, tc := range []struct {
		name     string
		kind     rt.CouncilCheckoutKind
		harness  string
		tool     bool
		writable bool
	}{
		{name: "standalone task", kind: rt.CouncilTask, harness: "claude", writable: true},
		{name: "standalone task with tool volume", kind: rt.CouncilTask, harness: "claude", tool: true, writable: true},
		{name: "standalone verifier", kind: rt.CouncilReview, harness: "codex"},
		{name: "council live seat", kind: rt.CouncilLive, harness: "claude"},
		{name: "council ref seat", kind: rt.CouncilRef, harness: "codex"},
		{name: "council reviewer seat", kind: rt.CouncilReview, harness: "claude"},
		{name: "council writer seat", kind: rt.CouncilWriter, harness: "claude", writable: true},
		{name: "council writer seat with tool volume", kind: rt.CouncilWriter, harness: "claude", tool: true, writable: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newCouncilEngine()
			f.vol.Labels = CouncilLoginVolumeLabels(tc.harness)
			f.net.Name = netNm
			a := NewAdapter(f, "")
			a.discoverSocket = func() string { return filepath.Join(home, ".docker", "run", "docker.sock") }
			cfg := CouncilHarness{Image: "fixture", LoginVolume: f.vol.Name}
			if tc.tool {
				cfg.Tool = fixtureTool()
				tv := toolVolume(tc.harness, cfg.Tool.Volume)
				f.tool = &tv
			}
			a.CouncilHarnesses = map[string]CouncilHarness{tc.harness: cfg}
			checkout := t.TempDir()
			s := rt.CouncilLaunch{
				TaskID: "task-guard", Workload: "guard", Harness: tc.harness,
				Checkout: rt.CouncilCheckout{Path: checkout, Kind: tc.kind},
				Limits:   rt.CouncilLimits{NanoCPUs: 500_000_000, MemoryBytes: 64 << 20, PIDs: 32, WallTime: 1e9},
				Prompt:   prompt, TaskToken: lease,
				Network: netNm, NetworkID: f.net.ID, ProxyURL: proxy, DataGateURL: gate,
			}
			o, err := a.harnessContainerSpec(t.Context(), f, s, false)
			if err != nil {
				t.Fatalf("harnessContainerSpec: %v", err)
			}
			c, h := o.Config, o.HostConfig

			// Mounts: checkout, login volume and, for vendor tools, the read-only tool volume. Nothing else.
			src, err := filepath.EvalSymlinks(checkout)
			if err != nil {
				t.Fatal(err)
			}
			want := []mount.Mount{
				{Type: mount.TypeBind, Source: src, Target: "/workspace", ReadOnly: !tc.writable, BindOptions: &mount.BindOptions{NonRecursive: true}},
				{Type: mount.TypeVolume, Source: f.vol.Name, Target: "/home/harness"},
			}
			if tc.tool {
				want = append(want, mount.Mount{Type: mount.TypeVolume, Source: cfg.Tool.Volume, Target: councilToolDir, ReadOnly: true, VolumeOptions: &mount.VolumeOptions{NoCopy: true}})
			}
			if len(h.Mounts) != len(want) || len(h.Binds) != 0 || len(h.VolumesFrom) != 0 || len(c.Volumes) != 0 {
				t.Fatalf("mounts = %+v binds = %v, want only %+v", h.Mounts, h.Binds, want)
			}
			for i := range want {
				got := h.Mounts[i]
				if !reflect.DeepEqual(got, want[i]) {
					t.Fatalf("mount %d = %+v, want %+v", i, got, want[i])
				}
				if got.Type == mount.TypeBind && (strings.HasPrefix(home, got.Source+string(filepath.Separator)) || strings.HasPrefix(got.Source, stateDir)) {
					t.Fatalf("mount %d exposes the home directory or daemon state: %s", i, got.Source)
				}
			}
			if len(h.Tmpfs) != 1 || h.Tmpfs["/scratch"] == "" || !strings.Contains(h.Tmpfs["/scratch"], "noexec") {
				t.Fatalf("tmpfs = %v, want only a noexec /scratch", h.Tmpfs)
			}

			// Environment: allowlist only, the proxy as the only route out, no lease and no model gate.
			for _, kv := range c.Env {
				key, _, _ := strings.Cut(kv, "=")
				if !harnessEnvAllowlist[key] {
					t.Fatalf("variable %q is not on the harness allowlist", key)
				}
			}
			for _, need := range []string{"HTTP_PROXY=" + proxy, "HTTPS_PROXY=" + proxy, "SABLECOURT_DATA_GATE_URL=" + gate, "HOME=/home/harness", "TMPDIR=/scratch"} {
				if !slices.Contains(c.Env, need) {
					t.Fatalf("env %v lacks %q", c.Env, need)
				}
			}
			if slices.Contains(c.Env, "GIT_OPTIONAL_LOCKS=0") == tc.writable {
				t.Fatalf("GIT_OPTIONAL_LOCKS must be set exactly for read-only checkouts: %v", c.Env)
			}

			// Network: the per-launch internal network only.
			if string(h.NetworkMode) != f.net.ID || len(h.ExtraHosts) != 0 || len(h.DNS) != 0 || len(h.PortBindings) != 0 || h.PublishAllPorts || o.NetworkingConfig != nil {
				t.Fatalf("network = %q extraHosts = %v ports = %v, want only the per-launch network", h.NetworkMode, h.ExtraHosts, h.PortBindings)
			}

			// User, root file system, capabilities and namespaces.
			if c.User != "10001:10001" || !h.ReadonlyRootfs || h.Privileged || len(h.CapAdd) != 0 || !slices.Equal(h.CapDrop, []string{"ALL"}) ||
				!slices.Contains(h.SecurityOpt, "no-new-privileges:true") || len(h.Devices) != 0 ||
				h.PidMode != "" || h.IpcMode != "" || h.UTSMode != "" || h.UsernsMode != "" || h.CgroupnsMode != "" {
				t.Fatalf("isolation guard failed: user=%q ro=%t priv=%t capAdd=%v capDrop=%v secopt=%v", c.User, h.ReadonlyRootfs, h.Privileged, h.CapAdd, h.CapDrop, h.SecurityOpt)
			}
			if h.Memory != s.Limits.MemoryBytes || h.NanoCPUs != s.Limits.NanoCPUs || h.PidsLimit == nil || *h.PidsLimit != s.Limits.PIDs || h.LogConfig.Type != "none" {
				t.Fatal("resource limits or logging guard failed")
			}

			// The lease and the prompt travel on stdin, never in stored create options.
			meta, err := json.Marshal(o)
			if err != nil {
				t.Fatal(err)
			}
			for _, secret := range []string{lease, prompt, "SABLECOURT_TASK_TOKEN=", "SABLECOURT_MODEL_GATE_URL"} {
				if strings.Contains(string(meta), secret) {
					t.Fatalf("create options carry %q", secret)
				}
			}
			for name, canary := range canaries {
				if strings.Contains(string(meta), canary) {
					t.Fatalf("create options carry the daemon's %s", name)
				}
			}
			// Labels carry the task ID only; nothing else may ride along.
			if !reflect.DeepEqual(c.Labels, map[string]string{"sablecourt.io/task-id": s.TaskID}) {
				t.Fatalf("labels = %v, want only the task ID", c.Labels)
			}
		})
	}
}

// TestHarnessContainerSpecRefusesUnsafeCheckouts applies the checkout guard to
// a standalone task and a council seat alike. Each row removes one guard's
// reason to exist, so the table fails if that check is removed.
func TestHarnessContainerSpecRefusesUnsafeCheckouts(t *testing.T) {
	cases := []string{
		"checkout contains home", "checkout contains runtime socket", "checkout contains DOCKER_HOST socket",
		"checkout is the state dir", "checkout contains the state dir", "checkout is the default state dir",
		"checkout is a protected path", "checkout contains a protected path",
		"leaf symlink", "dot-dot path", "unknown kind",
	}
	for _, kind := range []rt.CouncilCheckoutKind{rt.CouncilTask, rt.CouncilWriter, rt.CouncilLive} {
		for _, tc := range cases {
			t.Run(string(kind)+"/"+tc, func(t *testing.T) {
				checkout := t.TempDir()
				t.Setenv("HOME", t.TempDir())
				t.Setenv("SABLECOURT_STATE_DIR", filepath.Join(t.TempDir(), "state"))
				t.Setenv("DOCKER_HOST", "")
				f := newCouncilEngine()
				a := NewAdapter(f, "")
				a.discoverSocket = func() string { return "" }
				a.CouncilHarnesses = map[string]CouncilHarness{"claude": {Image: "fixture", LoginVolume: f.vol.Name, ProxyURL: "http://relay:8888", DataGateURL: "http://relay:8888/gate/mcp"}}
				s := rt.CouncilLaunch{TaskID: "task-guard", Workload: "guard", Harness: "claude", Checkout: rt.CouncilCheckout{Path: checkout, Kind: kind},
					Limits: rt.CouncilLimits{NanoCPUs: 1, MemoryBytes: 1 << 20, PIDs: 1, WallTime: 1e9}, Prompt: "p", TaskToken: "lease"}
				mkdir := func(p string) string {
					if err := os.MkdirAll(p, 0o700); err != nil {
						t.Fatal(err)
					}
					return p
				}
				switch tc {
				case "checkout contains home":
					t.Setenv("HOME", mkdir(filepath.Join(checkout, "home")))
				case "checkout contains runtime socket":
					a.discoverSocket = func() string { return filepath.Join(checkout, "docker.sock") }
				case "checkout contains DOCKER_HOST socket":
					t.Setenv("DOCKER_HOST", "unix://"+filepath.Join(checkout, "run", "engine.sock"))
				case "checkout is the state dir":
					t.Setenv("SABLECOURT_STATE_DIR", checkout)
				case "checkout contains the state dir":
					t.Setenv("SABLECOURT_STATE_DIR", mkdir(filepath.Join(checkout, "state")))
				case "checkout is the default state dir":
					home := t.TempDir()
					t.Setenv("HOME", home)
					t.Setenv("SABLECOURT_STATE_DIR", "")
					s.Checkout.Path = mkdir(filepath.Join(home, ".local", "share", "sablecourt"))
				case "checkout is a protected path":
					a.ProtectedPaths = []string{checkout}
				case "checkout contains a protected path":
					a.ProtectedPaths = []string{filepath.Join(mkdir(filepath.Join(checkout, "db")), "sablecourt.db")}
				case "leaf symlink":
					link := filepath.Join(t.TempDir(), "link")
					if err := os.Symlink(checkout, link); err != nil {
						t.Fatal(err)
					}
					s.Checkout.Path = link
				case "dot-dot path":
					s.Checkout.Path = filepath.Join(checkout, "sub") + string(filepath.Separator) + ".."
					mkdir(filepath.Join(checkout, "sub"))
				case "unknown kind":
					s.Checkout.Kind = "host"
				}
				if _, err := a.harnessContainerSpec(t.Context(), f, s, false); err == nil {
					t.Fatal("unsafe checkout accepted")
				}
			})
		}
	}
}

// TestHarnessContainerSpecAllowsTaskWorktreeUnderStateDir keeps the state
// directory guard from refusing a task's own worktree when the database, and
// so the worktree root, lives inside the state directory.
func TestHarnessContainerSpecAllowsTaskWorktreeUnderStateDir(t *testing.T) {
	state := t.TempDir()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("SABLECOURT_STATE_DIR", state)
	t.Setenv("DOCKER_HOST", "")
	root := filepath.Join(state, "worktrees")
	checkout := filepath.Join(root, "task-guard")
	if err := os.MkdirAll(checkout, 0o700); err != nil {
		t.Fatal(err)
	}
	f := newCouncilEngine()
	a := NewAdapter(f, "")
	a.discoverSocket = func() string { return "" }
	a.ProtectedPaths = []string{filepath.Join(state, "sablecourt.db"), root}
	a.CouncilHarnesses = map[string]CouncilHarness{"claude": {Image: "fixture", LoginVolume: f.vol.Name, ProxyURL: "http://relay:8888", DataGateURL: "http://relay:8888/gate/mcp"}}
	s := rt.CouncilLaunch{TaskID: "task-guard", Workload: "guard", Harness: "claude", Checkout: rt.CouncilCheckout{Path: checkout, Kind: rt.CouncilTask},
		Limits: rt.CouncilLimits{NanoCPUs: 1, MemoryBytes: 1 << 20, PIDs: 1, WallTime: 1e9}, Prompt: "p", TaskToken: "lease"}
	if _, err := a.harnessContainerSpec(t.Context(), f, s, false); err != nil {
		t.Fatalf("task worktree under the state directory refused: %v", err)
	}
}
