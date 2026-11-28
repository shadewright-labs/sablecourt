package docker

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"
	"github.com/shadewright-labs/sablecourt/internal/domain"
	"github.com/shadewright-labs/sablecourt/internal/operatortoken"
	"github.com/shadewright-labs/sablecourt/internal/redact"
	rt "github.com/shadewright-labs/sablecourt/internal/runtime"
	"github.com/shadewright-labs/sablecourt/internal/secret"
)

func synthDockerSecret(parts ...string) string {
	return strings.Join(parts, "")
}

func TestCouncilPromptAndStreamRedactRegisteredSecretsAllForms(t *testing.T) {
	opaque := synthDockerSecret("dock", "_council_", "opaque_556677")
	special := synthDockerSecret(`doc"k`, `\val`, `<>&/`, "council_889900")
	redact.Register(opaque, special)

	taskTok := synthDockerSecret("sc_", "task_", "abcdef0123456789abcdef0123456789")
	for _, sec := range []struct {
		name, value string
	}{
		{"opaque", opaque},
		{"special", special},
	} {
		jb, _ := json.Marshal(sec.value)
		q := strconv.Quote(sec.value)
		mid := len(sec.value) / 2
		forms := []struct {
			name    string
			payload string
			forbid  []string
		}{
			{"raw", "msg " + sec.value + " end", []string{sec.value}},
			{"quoted", "q=" + q, []string{sec.value, q[1 : len(q)-1]}},
			{"json_escaped", `{"s":` + string(jb) + `}`, []string{sec.value, string(jb[1 : len(jb)-1])}},
			{"split_crlf", "x " + sec.value[:mid] + "\r\n" + sec.value[mid:] + " y", []string{sec.value, sec.value[:mid], sec.value[mid:]}},
		}
		for _, f := range forms {
			t.Run(sec.name+"/"+f.name, func(t *testing.T) {
				prompt, err := preparePrompt(rt.CouncilLaunch{
					TaskToken: taskTok,
					Prompt:    f.payload,
				})
				if err != nil {
					t.Fatalf("preparePrompt: %v", err)
				}
				var out councilOutput
				_, _ = out.Write([]byte(f.payload))
				visible := out.visible("safe-prompt", taskTok)

				for _, check := range []struct {
					label, got string
				}{
					{"preparePrompt", prompt},
					{"councilOutput.visible", visible},
				} {
					for _, bad := range f.forbid {
						if len(bad) < redact.MinSecretLength {
							continue
						}
						if strings.Contains(check.got, bad) {
							t.Fatalf("%s leaked %q in %q", check.label, bad, check.got)
						}
					}
				}
			})
		}
	}
}

func TestContainerAndCouncilWorkloadsCannotReachDaemonSecrets(t *testing.T) {
	ctx := context.Background()

	stateDir := filepath.Join(t.TempDir(), "daemon-state")
	t.Setenv("SABLECOURT_STATE_DIR", stateDir)
	t.Setenv("DOCKER_HOST", "")
	opToken, err := operatortoken.LoadOrCreate(stateDir)
	if err != nil {
		t.Fatalf("operatortoken.LoadOrCreate: %v", err)
	}

	providerKey := synthDockerSecret("prov", "_docker_", "reach_112233")
	mcpSecret := synthDockerSecret("upmcp", "_docker_", "reach_445566")
	webhookSecret := synthDockerSecret("whsec", "_docker_", "reach_778899")
	pgPassword := synthDockerSecret("pgpw", "_docker_", "reach_334455")
	otherTaskToken := synthDockerSecret("sc_", "task_", "00112233445566778899aabbccddeeff")

	secretFile := filepath.Join(stateDir, "secrets.json")
	fs := secret.NewFileStore(secretFile)
	if err := fs.Set(ctx, "openai_api_key", providerKey); err != nil {
		t.Fatalf("FileStore.Set: %v", err)
	}

	t.Setenv("OPENAI_API_KEY", providerKey)
	t.Setenv("ANTHROPIC_API_KEY", providerKey)
	t.Setenv("GITHUB_TOKEN", mcpSecret)
	t.Setenv("SABLECOURT_OPERATOR_TOKEN", opToken)
	t.Setenv("SABLECOURT_POSTGRES_DSN", "postgres://svc:"+pgPassword+"@127.0.0.1:5432/sablecourt")
	t.Setenv("SABLECOURT_NOTIFY_WEBHOOK_URL", "https://hooks.example.invalid/"+webhookSecret)

	daemonSecrets := []string{opToken, providerKey, mcpSecret, webhookSecret, pgPassword, otherTaskToken}
	redact.Register(daemonSecrets...)

	assertCreateOptsClean := func(t *testing.T, label string, opts client.ContainerCreateOptions, ownTokenAllowedInEnv bool, ownToken string) {
		t.Helper()
		var hay strings.Builder
		if opts.Config != nil {
			hay.WriteString(strings.Join(opts.Config.Env, "\n"))
			hay.WriteString("\n")
			hay.WriteString(strings.Join(opts.Config.Cmd, "\n"))
			hay.WriteString("\n")
			hay.WriteString(strings.Join(opts.Config.Entrypoint, "\n"))
			for k, v := range opts.Config.Labels {
				hay.WriteString("\n" + k + "=" + v)
			}
		}
		if opts.HostConfig != nil {
			hay.WriteString("\n" + strings.Join(opts.HostConfig.Binds, "\n"))
			for _, m := range opts.HostConfig.Mounts {
				hay.WriteString("\n" + m.Source + ":" + m.Target)
				if strings.HasPrefix(m.Source, stateDir) || m.Source == secretFile {
					t.Fatalf("%s mounted daemon state path %q", label, m.Source)
				}
			}
			for k, v := range opts.HostConfig.Tmpfs {
				hay.WriteString("\n" + k + "=" + v)
			}
			for _, bind := range opts.HostConfig.Binds {
				if strings.HasPrefix(bind, stateDir) || strings.HasPrefix(bind, secretFile) {
					t.Fatalf("%s bound daemon state path %q", label, bind)
				}
			}
		}
		combined := hay.String()
		for _, sec := range daemonSecrets {
			if strings.Contains(combined, sec) {
				t.Fatalf("%s container spec exposed daemon-held secret", label)
			}
		}
		if !ownTokenAllowedInEnv && ownToken != "" && strings.Contains(combined, ownToken) {
			t.Fatalf("%s container spec exposed its task token outside stdin", label)
		}
	}

	t.Run("container_workload_start", func(t *testing.T) {
		eng := newCouncilEngine()
		eng.net = network.Network{Name: DefaultNetworkName, ID: "net-id", Internal: true, Driver: "bridge"}
		eng.imageID = "sha256:" + strings.Repeat("a", 64)
		adapter := testAdapter(stdinAttachEngine{eng}, DefaultNetworkName)
		ownTok := synthDockerSecret("sc_", "task_", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
		_, err := adapter.Start(ctx, rt.LaunchSpec{
			TaskID:         "task-cont-reach",
			InputJSON:      `{"prompt":"do work"}`,
			WorkDir:        t.TempDir(),
			ModelGateURL:   "http://127.0.0.1:7432/gate/model",
			ModelGateToken: ownTok,
			DataGateURL:    "http://127.0.0.1:7432/gate/mcp",
			TaskToken:      ownTok,
			Workload: domain.Workload{
				Metadata: domain.ObjectMeta{Name: "agent-cont", Zone: domain.ZoneRuntime},
				Spec: domain.WorkloadSpec{
					Class:   domain.ClassLongLivedAgent,
					Runtime: domain.RuntimeConfig{Type: "docker", Image: "sablecourt-agent:0.1.0", ReadOnlyRootfs: true},
				},
			},
		})
		if err != nil {
			t.Fatalf("Adapter.Start: %v", err)
		}
		assertCreateOptsClean(t, "container workload", eng.createdOpts, true, ownTok)
	})

	t.Run("council_member_container", func(t *testing.T) {
		eng := newCouncilEngine()
		adapter := testAdapter(eng, DefaultNetworkName)
		root := withWorktreeRoot(t, adapter)
		adapter.CouncilHarnesses = map[string]CouncilHarness{
			"claude": {
				Image:       "sablecourt-harness-claude:local",
				LoginVolume: "sablecourt-login-claude",
				ProxyURL:    "http://proxy-1:3128",
				DataGateURL: "http://proxy-1:3128/gate/mcp",
			},
		}
		checkoutDir := mkdirAll(t, filepath.Join(root, "council-reach-1"))
		ownTok := synthDockerSecret("sc_", "task_", "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")
		spec := rt.CouncilLaunch{
			TaskID:    "council-reach-1",
			Workload:  "council-claude",
			Harness:   "claude",
			TaskToken: ownTok,
			Prompt:    "review " + providerKey + " and " + opToken,
			Checkout:  rt.CouncilCheckout{Kind: rt.CouncilRef, Path: checkoutDir},
			Limits: rt.CouncilLimits{
				NanoCPUs:    1_000_000_000,
				MemoryBytes: 512 << 20,
				PIDs:        64,
				WallTime:    time.Minute,
			},
		}
		opts, err := adapter.harnessContainerSpec(ctx, eng, spec, false)
		if err != nil {
			t.Fatalf("harnessContainerSpec: %v", err)
		}
		assertCreateOptsClean(t, "council container", opts, false, ownTok)
		if opts.HostConfig == nil || !opts.HostConfig.ReadonlyRootfs || opts.HostConfig.NetworkMode != container.NetworkMode("net-id") {
			t.Fatalf("council container missing hardened rootfs or internal network: %+v", opts.HostConfig)
		}

		// Prompt delivered over stdin must also redact daemon secrets.
		prompt, err := preparePrompt(spec)
		if err != nil {
			t.Fatalf("preparePrompt: %v", err)
		}
		for _, sec := range daemonSecrets {
			if strings.Contains(prompt, sec) {
				t.Fatalf("council stdin prompt leaked daemon-held secret")
			}
		}
	})
}
