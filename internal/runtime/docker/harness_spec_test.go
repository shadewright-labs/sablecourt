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

	"github.com/shadewright-labs/sablecourt/internal/harnessimages"
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
	"GIT_DIR": true, "GIT_WORK_TREE": true,
}

// linkedCheckout lays out a linked worktree the way git worktree add does:
// the checkout's .git file names a registration under the shared repository's
// worktrees directory. It returns the checkout, the shared repository's Git
// directory and a self-contained per-launch metadata directory.
func linkedCheckout(t *testing.T) (checkout, common, gitDir string) {
	t.Helper()
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	common = filepath.Join(base, "repo", ".git")
	reg := filepath.Join(common, "worktrees", "task-guard")
	checkout = filepath.Join(base, "worktrees", "task-guard")
	gitDir = checkout + ".git"
	for _, dir := range []string{filepath.Join(common, "objects"), filepath.Join(common, "refs"), reg, checkout, filepath.Join(gitDir, "objects", "pack")} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for path, body := range map[string]string{
		filepath.Join(reg, "commondir"): "../..\n",
		filepath.Join(reg, "gitdir"):    filepath.Join(checkout, ".git") + "\n",
		filepath.Join(checkout, ".git"): "gitdir: " + reg + "\n",
		filepath.Join(gitDir, "HEAD"):   "ref: refs/heads/sablecourt/task-guard\n",
	} {
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return checkout, common, gitDir
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
	// The guard resolves the runtime socket, so its directory exists, as it
	// does for any socket discovery returns.
	for _, dir := range []string{stateDir, filepath.Join(home, ".docker", "run")} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
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
		command  []string
	}{
		{name: "standalone task", kind: rt.CouncilTask, harness: "claude", writable: true},
		{name: "standalone task with tool volume", kind: rt.CouncilTask, harness: "claude", tool: true, writable: true},
		{name: "standalone verifier", kind: rt.CouncilReview, harness: "codex"},
		{name: "council live seat", kind: rt.CouncilLive, harness: "claude"},
		{name: "council ref seat", kind: rt.CouncilRef, harness: "codex"},
		{name: "council reviewer seat", kind: rt.CouncilReview, harness: "claude"},
		{name: "council writer seat", kind: rt.CouncilWriter, harness: "claude", writable: true},
		{name: "council writer seat with tool volume", kind: rt.CouncilWriter, harness: "claude", tool: true, writable: true},
		// A command step (issue 134): the same guards, no login or tool volume.
		{name: "command step", kind: rt.CouncilReview, harness: "claude", command: []string{"/bin/sh", "-c", "make check"}},
		{name: "command step with tool volume", kind: rt.CouncilReview, harness: "codex", tool: true, command: []string{"make", "check"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newCouncilEngine()
			f.vol.Labels = CouncilLoginVolumeLabels(tc.harness)
			f.net.Name = netNm
			a := testAdapter(f, "")
			a.discoverSocket = func() string { return filepath.Join(home, ".docker", "run", "docker.sock") }
			cfg := CouncilHarness{Image: "fixture", LoginVolume: f.vol.Name}
			if tc.tool {
				cfg.Tool = fixtureTool()
				tv := toolVolume(tc.harness, cfg.Tool.Volume)
				f.tool = &tv
			}
			a.CouncilHarnesses = map[string]CouncilHarness{tc.harness: cfg}
			// Every kind but live is a linked worktree and carries its own
			// metadata directory; a live checkout's .git is mounted with it.
			checkout, common, gitDir := liveDir(t), "", ""
			if tc.kind != rt.CouncilLive {
				checkout, common, gitDir = linkedCheckout(t)
			}
			if tc.kind == rt.CouncilLive {
				withWorktreeRoot(t, a)
			} else {
				openRootAt(t, a, filepath.Dir(checkout))
			}
			s := rt.CouncilLaunch{
				TaskID: "task-guard", Workload: "guard", Harness: tc.harness,
				Checkout: rt.CouncilCheckout{Path: checkout, Kind: tc.kind, GitDir: gitDir},
				Limits:   rt.CouncilLimits{NanoCPUs: 500_000_000, MemoryBytes: 64 << 20, PIDs: 32, WallTime: 1e9},
				Prompt:   prompt, TaskToken: lease,
				Network: netNm, NetworkID: f.net.ID, ProxyURL: proxy, DataGateURL: gate,
			}
			if tc.command != nil {
				s.Prompt, s.Command = "", tc.command
			}
			o, err := a.harnessContainerSpec(t.Context(), f, s, false)
			if err != nil {
				t.Fatalf("harnessContainerSpec: %v", err)
			}
			c, h := o.Config, o.HostConfig

			// Mounts: checkout, its own Git metadata for a linked worktree, login
			// volume and, for vendor tools, the read-only tool volume. Nothing else.
			src, err := filepath.EvalSymlinks(checkout)
			if err != nil {
				t.Fatal(err)
			}
			want := []mount.Mount{
				{Type: mount.TypeBind, Source: src, Target: "/workspace", ReadOnly: !tc.writable, BindOptions: &mount.BindOptions{NonRecursive: true}},
			}
			if gitDir != "" {
				gitSrc, err := filepath.EvalSymlinks(gitDir)
				if err != nil {
					t.Fatal(err)
				}
				want = append(want, mount.Mount{Type: mount.TypeBind, Source: gitSrc, Target: councilGitDir, ReadOnly: !tc.writable, BindOptions: &mount.BindOptions{NonRecursive: true}})
			}
			if tc.command == nil {
				want = append(want, mount.Mount{Type: mount.TypeVolume, Source: f.vol.Name, Target: harnessimages.StateDir})
			}
			if tc.tool && tc.command == nil {
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
			// The shared repository (its registration directory, refs and
			// object store) is never a mount source, nor inside or above one.
			if common != "" {
				commonSrc, err := filepath.EvalSymlinks(common)
				if err != nil {
					t.Fatal(err)
				}
				for i, got := range h.Mounts {
					if got.Type == mount.TypeBind && (got.Source == commonSrc || strings.HasPrefix(got.Source, commonSrc+string(filepath.Separator)) || strings.HasPrefix(commonSrc, got.Source+string(filepath.Separator))) {
						t.Fatalf("mount %d exposes the shared repository metadata: %s", i, got.Source)
					}
				}
			}
			if hasGit := slices.Contains(c.Env, "GIT_DIR="+councilGitDir) && slices.Contains(c.Env, "GIT_WORK_TREE=/workspace"); hasGit != (gitDir != "") {
				t.Fatalf("GIT_DIR and GIT_WORK_TREE must be set exactly when the launch has its own Git metadata: %v", c.Env)
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
			home := "HOME=" + harnessimages.StateDir
			if tc.command != nil {
				// No login volume to call home: the command's home is the tmpfs scratch.
				home = "HOME=/scratch"
				if !slices.Equal(c.Cmd, tc.command) || len(c.Entrypoint) == 0 || c.Entrypoint[0] != "/usr/bin/timeout" {
					t.Fatalf("command container runs %v %v, want the timeout wrapper around %v", c.Entrypoint, c.Cmd, tc.command)
				}
			}
			for _, need := range []string{"HTTP_PROXY=" + proxy, "HTTPS_PROXY=" + proxy, "SABLECOURT_DATA_GATE_URL=" + gate, home, "TMPDIR=/scratch"} {
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

// TestHarnessContainerSpecAllowsTaskWorktreeUnderStateDir keeps the state
// directory guard from refusing a task's own worktree when the database, and
// so the worktree root, lives inside the state directory.
func TestHarnessContainerSpecAllowsTaskWorktreeUnderStateDir(t *testing.T) {
	state, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", t.TempDir())
	t.Setenv("SABLECOURT_STATE_DIR", state)
	t.Setenv("DOCKER_HOST", "")
	root := filepath.Join(state, "worktrees")
	checkout := filepath.Join(root, "task-guard")
	if err := os.MkdirAll(checkout, 0o700); err != nil {
		t.Fatal(err)
	}
	f := newCouncilEngine()
	a := testAdapter(f, "")
	a.discoverSocket = func() string { return "" }
	openRootAt(t, a, root, filepath.Join(state, "sablecourt.db"), state)
	a.CouncilHarnesses = map[string]CouncilHarness{"claude": {Image: "fixture", LoginVolume: f.vol.Name, ProxyURL: "http://relay:8888", DataGateURL: "http://relay:8888/gate/mcp"}}
	s := rt.CouncilLaunch{TaskID: "task-guard", Workload: "guard", Harness: "claude", Checkout: rt.CouncilCheckout{Path: checkout, Kind: rt.CouncilTask},
		Limits: rt.CouncilLimits{NanoCPUs: 1, MemoryBytes: 1 << 20, PIDs: 1, WallTime: 1e9}, Prompt: "p", TaskToken: "lease"}
	if _, err := a.harnessContainerSpec(t.Context(), f, s, false); err != nil {
		t.Fatalf("task worktree under the state directory refused: %v", err)
	}
}
