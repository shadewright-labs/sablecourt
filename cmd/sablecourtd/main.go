package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/spf13/cobra"

	"github.com/shadewright-labs/sablecourt/internal/a2a"
	"github.com/shadewright-labs/sablecourt/internal/approval"
	"github.com/shadewright-labs/sablecourt/internal/attach"
	"github.com/shadewright-labs/sablecourt/internal/conclave"
	"github.com/shadewright-labs/sablecourt/internal/config"
	"github.com/shadewright-labs/sablecourt/internal/contextpack"
	"github.com/shadewright-labs/sablecourt/internal/controlplane"
	"github.com/shadewright-labs/sablecourt/internal/domain"
	"github.com/shadewright-labs/sablecourt/internal/gate/councilproxy"
	mcpgate "github.com/shadewright-labs/sablecourt/internal/gate/mcp"
	"github.com/shadewright-labs/sablecourt/internal/gate/model"
	"github.com/shadewright-labs/sablecourt/internal/harnessimages"
	"github.com/shadewright-labs/sablecourt/internal/integrations"
	"github.com/shadewright-labs/sablecourt/internal/notify"
	"github.com/shadewright-labs/sablecourt/internal/operatortoken"
	"github.com/shadewright-labs/sablecourt/internal/policy"
	"github.com/shadewright-labs/sablecourt/internal/providers"
	"github.com/shadewright-labs/sablecourt/internal/redact"
	scruntime "github.com/shadewright-labs/sablecourt/internal/runtime"
	"github.com/shadewright-labs/sablecourt/internal/runtime/coding"
	"github.com/shadewright-labs/sablecourt/internal/runtime/controller"
	"github.com/shadewright-labs/sablecourt/internal/runtime/docker"
	k8sruntime "github.com/shadewright-labs/sablecourt/internal/runtime/k8s"
	"github.com/shadewright-labs/sablecourt/internal/secret"
	"github.com/shadewright-labs/sablecourt/internal/store"
	"github.com/shadewright-labs/sablecourt/internal/store/postgres"
	"github.com/shadewright-labs/sablecourt/internal/store/sqlite"
	"github.com/shadewright-labs/sablecourt/internal/telemetry"
	"github.com/shadewright-labs/sablecourt/internal/ui"
	"github.com/shadewright-labs/sablecourt/internal/voice"
	"github.com/shadewright-labs/sablecourt/internal/webhook"
)

func main() {
	rootCmd := newRootCmd()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := rootCmd.ExecuteContext(ctx); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1) //nolint:gocritic // stop only releases the signal handler, which exiting releases too
	}
}

// newRootCmd builds the daemon command and its flags. The CLI reference is
// generated from it.
func newRootCmd() *cobra.Command {
	var (
		dbPath               string
		postgresDSN          string
		manifestsPath        string
		listenAddr           string
		socketPath           string
		gateHost             string
		secretFile           string
		repoRoot             string
		runtimeBackend       string
		k8sNamespace         string
		k8sRuntimeClass      string
		kubeconfigPath       string
		manifestsGitRepo     string
		manifestsGitBranch   string
		manifestsGitInterval time.Duration
		maxConcurrent        int
		mode                 string
		modelGatewayURL      string
		allowedHosts         []string
		incidentTemplate     string
		branchRetention      time.Duration
	)

	rootCmd := &cobra.Command{
		Use:   "sablecourtd",
		Short: "Sablecourt local-first and enterprise Kubernetes control plane daemon",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runDaemon(cmd.Context(), daemonConfig{
				dbPath:               dbPath,
				postgresDSN:          postgresDSN,
				manifestsPath:        manifestsPath,
				listenAddr:           listenAddr,
				socketPath:           socketPath,
				gateHost:             gateHost,
				secretFile:           secretFile,
				repoRoot:             repoRoot,
				runtimeBackend:       runtimeBackend,
				k8sNamespace:         k8sNamespace,
				k8sRuntimeClass:      k8sRuntimeClass,
				kubeconfigPath:       kubeconfigPath,
				manifestsGitRepo:     manifestsGitRepo,
				manifestsGitBranch:   manifestsGitBranch,
				manifestsGitInterval: manifestsGitInterval,
				maxConcurrent:        maxConcurrent,
				mode:                 mode,
				modelGatewayURL:      modelGatewayURL,
				allowedHosts:         allowedHosts,
				incidentTemplate:     incidentTemplate,
				branchRetention:      branchRetention,
			})
		},
	}

	rootCmd.Flags().StringVar(&mode, "mode", "sanctum", "Operating mode: sanctum (local macOS/Ubuntu) or citadel (Kubernetes)")
	rootCmd.Flags().StringVar(&modelGatewayURL, "model-gateway-url", "", "Optional upstream AI gateway URL (e.g. agentgateway or Agent Router)")
	rootCmd.Flags().StringVar(&dbPath, "db", "sablecourt.db", "SQLite database path")
	rootCmd.Flags().StringVar(&postgresDSN, "postgres-dsn", "", "Optional Postgres DSN (or set SABLECOURT_POSTGRES_DSN) instead of SQLite")
	rootCmd.Flags().StringVar(&manifestsPath, "manifests", "", "Path to Git-tracked YAML manifests file or directory")
	rootCmd.Flags().StringVar(&manifestsGitRepo, "manifests-git-repo", "", "Optional Git repository URL to sync manifests from automatically")
	rootCmd.Flags().StringVar(&manifestsGitBranch, "manifests-git-branch", "main", "Branch for --manifests-git-repo")
	rootCmd.Flags().DurationVar(&manifestsGitInterval, "manifests-git-interval", 60*time.Second, "Poll interval for --manifests-git-repo")
	rootCmd.Flags().DurationVar(&branchRetention, "coding-branch-retention", 0, "Age before merged coding branches can be pruned (0 disables pruning)")
	rootCmd.Flags().IntVar(&maxConcurrent, "max-concurrent", 4, "Maximum number of concurrently running tasks (0 = unlimited)")
	rootCmd.Flags().StringVar(&listenAddr, "listen", "127.0.0.1:7432", "HTTP listen address")
	rootCmd.Flags().StringSliceVar(&allowedHosts, "allow-host", nil, "Additional allowed HTTP host or host:port (repeatable, bare hosts use the listen port)")
	rootCmd.Flags().StringVar(&incidentTemplate, "incident-conclave-template", "", "Default Conclave template for new Shadewright incidents")
	rootCmd.Flags().StringVar(&socketPath, "socket", "", "Optional Unix domain socket path for local CLI access (0600)")
	rootCmd.Flags().StringVar(&gateHost, "gate-host", "", "Optional host:port injected into containers for gates (defaults to --listen)")
	rootCmd.Flags().StringVar(&secretFile, "secret-file", "", "Optional 0600 JSON secret file fallback path")
	rootCmd.Flags().StringVar(&repoRoot, "repo-root", ".", "Git repository root for GitOps typed actions")
	rootCmd.Flags().StringVar(&runtimeBackend, "runtime", "docker", "Container runtime backend: docker or k8s")
	rootCmd.Flags().StringVar(&k8sNamespace, "k8s-namespace", "default", "Kubernetes namespace when --runtime=k8s")
	rootCmd.Flags().StringVar(&k8sRuntimeClass, "k8s-runtime-class", "", "Optional Kubernetes RuntimeClassName (e.g. gvisor, kata)")
	rootCmd.Flags().StringVar(&kubeconfigPath, "kubeconfig", "", "Optional kubeconfig path when --runtime=k8s")
	rootCmd.Flags().Bool("operator-auth-required", true, "Require operator authentication")
	_ = rootCmd.Flags().MarkDeprecated("operator-auth-required", "operator authentication is always required")
	return rootCmd
}

type daemonConfig struct {
	dbPath               string
	postgresDSN          string
	manifestsPath        string
	listenAddr           string
	socketPath           string
	gateHost             string
	secretFile           string
	repoRoot             string
	runtimeBackend       string
	k8sNamespace         string
	k8sRuntimeClass      string
	kubeconfigPath       string
	manifestsGitRepo     string
	manifestsGitBranch   string
	manifestsGitInterval time.Duration
	maxConcurrent        int
	mode                 string
	modelGatewayURL      string
	allowedHosts         []string
	incidentTemplate     string
	branchRetention      time.Duration
	openStore            func() (store.Store, error)
	logger               *slog.Logger
}

func newCodingAdapter(hub *attach.Hub, dbPath string, retention time.Duration) *coding.Adapter {
	adapter := coding.NewAdapter(filepath.Join(filepath.Dir(dbPath), "worktrees"))
	adapter.BranchRetention = retention
	adapter.AttachHub = hub
	return adapter
}

func registerCodingAdapter(adapters map[domain.WorkloadClass]scruntime.Adapter, isCitadel bool, hub *attach.Hub, dbPath string, retention time.Duration, checkSandbox func() error, logger *slog.Logger) {
	if isCitadel {
		return
	}
	if checkSandbox == nil {
		if logger != nil {
			logger.Warn("host coding adapter disabled: sandbox availability check unavailable")
		}
		return
	}
	if err := checkSandbox(); err != nil {
		if logger != nil {
			logger.Warn("host coding adapter disabled: no working sandbox on this platform", "error", err)
		}
		return
	}
	adapters[domain.ClassCodingAgent] = newCodingAdapter(hub, dbPath, retention)
}

func wireCouncilContainers(adapters map[domain.WorkloadClass]scruntime.Adapter, isCitadel bool, dockerAdapter *docker.Adapter, hub *attach.Hub, dbPath string, retention time.Duration, listenAddr string) {
	if isCitadel || dockerAdapter == nil {
		return
	}
	harnesses := make(map[string]docker.CouncilHarness, len(councilPanel))
	for _, role := range councilPanel {
		def, lookupErr := harnessimages.Lookup(role)
		if lookupErr != nil {
			continue
		}
		h := docker.CouncilHarness{
			Image:       def.Image,
			LoginVolume: def.Volume,
			LoginFiles:  def.LoginFiles,
		}
		if t := def.Tool; t != nil {
			// The image carries no vendor tool: it comes from the read-only tool volume.
			h.Tool = &docker.CouncilTool{Volume: t.Volume, Binary: t.Binary, Version: t.Version, SHA256: t.SHA256, InstallCommand: harnessimages.InstallCommand(role)}
		}
		harnesses[role] = h
	}
	dockerAdapter.CouncilHarnesses = harnesses
	codingAdapter, ok := adapters[domain.ClassCodingAgent].(*coding.Adapter)
	if !ok || codingAdapter == nil {
		codingAdapter = newCodingAdapter(hub, dbPath, retention)
		adapters[domain.ClassCodingAgent] = codingAdapter
	}
	codingAdapter.CouncilRunner = dockerAdapter
	codingAdapter.CouncilProxy = dockerAdapter
	codingAdapter.CouncilProxyImage = harnessimages.ProxyImage()
	codingAdapter.DaemonListen = listenAddr
}

func runDaemon(ctx context.Context, cfg daemonConfig) (runErr error) {
	mode := strings.ToLower(strings.TrimSpace(cfg.mode))
	if mode == "" || mode == "sanctum" || mode == "local" {
		if envMode := strings.ToLower(strings.TrimSpace(os.Getenv("SABLECOURT_MODE"))); envMode != "" {
			mode = envMode
		}
	}
	isCitadel := mode == "citadel" || mode == "enterprise"
	if isCitadel {
		if cfg.runtimeBackend == "" || cfg.runtimeBackend == "docker" {
			cfg.runtimeBackend = "k8s"
		}
		if cfg.listenAddr == "127.0.0.1:7432" {
			cfg.listenAddr = "0.0.0.0:7432"
		}
	}
	if cfg.modelGatewayURL == "" {
		cfg.modelGatewayURL = strings.TrimSpace(os.Getenv("SABLECOURT_MODEL_GATEWAY_URL"))
	}

	dbPath := cfg.dbPath
	manifestsPath := cfg.manifestsPath
	listenAddr := cfg.listenAddr
	socketPath := cfg.socketPath
	gateHost := cfg.containerGateHost()
	secretFile := cfg.secretFile
	repoRoot := cfg.repoRoot

	dir, err := operatortoken.StateDir()
	if err != nil {
		return fmt.Errorf("resolve operator state directory: %w", err)
	}
	operatorToken, stateLock, err := initDaemonState(dir)
	if err != nil {
		return err
	}
	defer stateLock.Close() //nolint:errcheck // lock handle released at exit; the lock holds no data
	logger := cfg.logger
	if logger == nil {
		logger = telemetry.NewLogger()
	}

	var gitSyncer *config.GitSyncer
	if cfg.manifestsGitRepo != "" {
		syncDir := manifestsPath
		if syncDir == "" {
			syncDir = filepath.Join(os.TempDir(), "sablecourt-git-manifests")
		}
		gitSyncer = config.NewGitSyncer(cfg.manifestsGitRepo, cfg.manifestsGitBranch, syncDir)
		if _, sha, err := gitSyncer.SyncOnce(ctx); err != nil {
			logger.Warn("initial manifests git sync failed", "repo", cfg.manifestsGitRepo, "error", err)
		} else {
			logger.Info("synced manifests from git", "repo", cfg.manifestsGitRepo, "sha", sha[:12])
			manifestsPath = syncDir
		}
	}

	if manifestsPath == "" {
		manifestsPath = discoverManifestsPath()
	}

	pgDSN := cfg.postgresDSN
	if pgDSN == "" {
		pgDSN = os.Getenv("SABLECOURT_POSTGRES_DSN")
	}
	var st store.Store
	if cfg.openStore != nil {
		st, err = cfg.openStore()
	} else if pgDSN != "" {
		st, err = postgres.Open(pgDSN)
		if err == nil {
			logger.Info("using postgres store")
		}
	} else {
		st, err = sqlite.Open(dbPath)
	}
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := st.Close(); closeErr != nil {
			safeClose := sanitizeStoreCloseError(closeErr)
			if runErr != nil {
				logger.Error("close store failed after daemon error", "error", safeClose, "daemon_error", sanitizeStoreCloseError(runErr))
				return
			}
			logger.Error("close store failed", "error", safeClose)
			runErr = &redactedStoreCloseError{msg: "close store: " + safeClose, err: closeErr}
		}
	}()

	if err := st.Migrate(ctx); err != nil {
		return err
	}

	attachHub := attach.NewHub()
	adapters := map[domain.WorkloadClass]scruntime.Adapter{}
	registerCodingAdapter(adapters, isCitadel, attachHub, dbPath, cfg.branchRetention, func() error {
		return coding.CheckSandbox(ctx)
	}, logger)
	var councilDocker *docker.Adapter
	if cfg.runtimeBackend != "k8s" {
		if da, err := docker.NewFromEnv(); err == nil {
			da.SetAttachHub(ctx, attachHub)
			adapters[domain.ClassLongLivedAgent] = da
			adapters[domain.ClassDeterministicController] = controller.NewAdapter(da)
			if da.CouncilRuntimeConfigured() {
				wireCouncilContainers(adapters, isCitadel, da, attachHub, dbPath, cfg.branchRetention, listenAddr)
				if !isCitadel {
					councilDocker = da
				}
			}
		} else {
			return fmt.Errorf("configure Docker runtime: %w", err)
		}
	}

	var (
		bundleMu sync.RWMutex
		bundle   *config.Bundle
	)
	policies := newPolicyLoader(st, manifestsPath)
	loadAndSync := func() (*config.Bundle, *policy.Engine, error) {
		b, pol, err := policies.Load(ctx)
		if err != nil {
			return nil, nil, err
		}
		if !isCitadel && adapters[domain.ClassCodingAgent] != nil {
			var err error
			b, err = ensureCouncilPanelWorkloads(ctx, st, b, councilSandbox(councilDocker != nil))
			if err != nil {
				return nil, nil, fmt.Errorf("register council panel workloads: %w", err)
			}
		}
		return b, pol, nil
	}

	var policyEng *policy.Engine
	bundle, policyEng, err = loadAndSync()
	if err != nil {
		return err
	}
	if manifestsPath != "" && bundle != nil {
		logger.Info("loaded manifests", "path", manifestsPath, "workloads", len(bundle.Workloads), "policies", len(bundle.Policies), "budgets", len(bundle.Budgets), "workflows", len(bundle.Workflows))
	} else {
		logger.Warn("no manifests loaded; pass --manifests=<path> or create ~/.config/sablecourt/manifests")
	}

	stores := []secret.SecretStore{secret.NewKeyringStore("sablecourt")}
	if secretFile != "" {
		stores = append(stores, secret.NewFileStore(secretFile))
	}
	secretStore := &secret.ChainStore{Stores: stores}

	modelGate := model.New(model.Options{
		Store:      gateLeaseStore{st},
		Secrets:    secretStore,
		Bundle:     bundle,
		GatewayURL: cfg.modelGatewayURL,
	})
	if cfg.modelGatewayURL != "" {
		logger.Info("model gate routing through external AI gateway", "url", cfg.modelGatewayURL)
	}

	breaker := policies.Breaker()
	notifier := notify.New(notify.Options{Secrets: secretStore})
	var oobApprover approval.Approver
	if !isCitadel {
		oobApprover = &approval.MacOSDialogApprover{}
	}
	approvalQueue := approval.NewQueue(st, oobApprover)
	approvalQueue.SetOnRequested(func(apr domain.Approval) {
		go func() {
			_ = notifier.Send(context.Background(), notify.Event{
				Type:   "approval.requested",
				Title:  fmt.Sprintf("Approval required: %s (%s)", apr.Capability, apr.WorkloadName),
				Detail: fmt.Sprintf("Approval %s for task %s awaits confirmation (sha256=%s).", apr.ID, apr.TaskID, apr.PayloadSHA256[:12]),
				Path:   "/tasks/" + apr.TaskID,
			})
		}()
	})
	dataActionGate := mcpgate.New(mcpgate.Options{
		Store:     gateLeaseStore{st},
		Bundle:    bundle,
		Policy:    policyEng,
		Breaker:   breaker,
		Approvals: approvalQueue,
	})
	// The repository git.open_pr may target is read from the repo root's origin
	// once, here. A reload only compares the origin with it (reloadGitOps);
	// changing the repository needs a restart.
	repoBinding := mcpgate.RegisterBuiltinGitOps(dataActionGate, repoRoot, &mcpgate.GitHubPROpener{Secrets: secretStore})
	if pinned, err := repoBinding.Pinned(); err != nil {
		logger.Warn("git.open_pr has no target repository; every call is refused until a restart", "error", err)
	} else {
		logger.Info("git.open_pr pinned to the repo root's origin", "repo", pinned)
	}
	mcpgate.RegisterBuiltinMemory(dataActionGate, st)
	mcpBridge := mcpgate.NewUpstreamBridge(secretStore, nil)
	registerUpstreamMCP := func(b *config.Bundle) {
		if b == nil {
			return
		}
		for _, w := range b.Workloads {
			if len(w.Spec.Grants.MCPServers) > 0 {
				dataActionGate.RegisterUpstreamTools(w.Spec.Grants.MCPServers, mcpBridge)
			}
		}
	}
	registerUpstreamMCP(bundle)

	if isCitadel {
		logger.Info("citadel mode active: host coding-agent disabled, fail-closed operator auth enforced")
		if pgDSN == "" {
			logger.Warn("citadel mode running on SQLite; set SABLECOURT_POSTGRES_DSN for production HA")
		}
	}
	if cfg.runtimeBackend == "k8s" {
		if k8sAdapter, err := k8sruntime.NewFromConfig(cfg.kubeconfigPath, cfg.k8sNamespace, cfg.k8sRuntimeClass); err == nil {
			k8sAdapter.SetAttachHub(ctx, attachHub)
			adapters[domain.ClassLongLivedAgent] = k8sAdapter
			adapters[domain.ClassDeterministicController] = controller.NewAdapter(k8sAdapter)
			logger.Info("using kubernetes runtime adapter", "namespace", cfg.k8sNamespace, "runtimeClass", cfg.k8sRuntimeClass)
		} else {
			logger.Warn("kubernetes runtime unavailable", "error", err)
		}
	}

	reconciler := controlplane.NewReconciler(controlplane.Options{
		Store:        st,
		Bundle:       bundle,
		Policy:       policyEng,
		Adapters:     adapters,
		ModelGateURL: "http://" + gateHost + "/gate/model",
		DataGateURL:  "http://" + gateHost + "/gate/mcp",
		// Host coding agents reach the gate over loopback, not a container alias.
		HostDataGateURL: cfg.hostGateURL(),
		MaxConcurrent:   cfg.maxConcurrent,
		OnTaskFailed: func(task domain.Task, reason string) {
			go func() {
				_ = notifier.Send(context.Background(), notify.Event{
					Type:   "task.failed",
					Title:  fmt.Sprintf("Task failed: %s (%s)", task.WorkloadName, task.ID),
					Detail: reason,
					Path:   "/tasks/" + task.ID,
				})
			}()
		},
	})
	getBundle := func() *config.Bundle {
		bundleMu.RLock()
		defer bundleMu.RUnlock()
		return bundle
	}

	conclaveBroker := conclave.NewBroker(conclave.Options{
		Store:      st,
		Reconciler: reconciler,
		GetBundle:  getBundle,
		Notifier:   notifier,
	})
	conclaveBroker.RegisterMCPTools(dataActionGate)

	scheduler := controlplane.NewScheduler(reconciler)
	// One reload at a time: the reload route, the dashboard and the manifests
	// git sync can call it together, and the gate and the reconciler must end
	// with the same bundle and policy.
	reloadFn := serialized(func() error {
		b, pol, err := loadAndSync()
		if err != nil {
			return err
		}
		bundleMu.Lock()
		bundle = b
		bundleMu.Unlock()
		modelGate.SetBundle(b)
		reloadGitOps(ctx, logger, dataActionGate, repoBinding, b, pol)
		registerUpstreamMCP(b)
		reconciler.Reload(b, pol)
		_ = scheduler.Reload(ctx, b)
		if b != nil {
			logger.Info("reloaded manifests", "workloads", len(b.Workloads), "schedules", len(b.Schedules), "digest", shortDigest(b.ManifestDigest))
		}
		return nil
	})

	provReg := providers.NewRegistry(providers.Options{
		Store:   st,
		Secrets: secretStore,
	})

	dash, err := ui.NewDashboard(ui.Options{
		Store:         st,
		Approvals:     approvalQueue,
		Reconciler:    reconciler,
		Scheduler:     scheduler,
		Conclave:      conclaveBroker,
		Providers:     provReg,
		GetBundle:     getBundle,
		Reload:        reloadFn,
		ManifestsPath: manifestsPath,
	})
	if err != nil {
		return fmt.Errorf("init web ui: %w", err)
	}

	privateDirs := []string{dir}
	if dbPath != "" {
		if absDB, err := filepath.Abs(dbPath); err == nil {
			privateDirs = append(privateDirs, absDB, filepath.Join(filepath.Dir(absDB), "worktrees"))
		}
	}
	if secretFile != "" {
		if absSecret, err := filepath.Abs(secretFile); err == nil {
			privateDirs = append(privateDirs, absSecret)
		}
	}

	hc :=
		handlerConfig{
			incidentTemplate:    cfg.incidentTemplate,
			operatorToken:       operatorToken,
			councilsDisabled:    isCitadel,
			councilPrivateDirs:  privateDirs,
			checkCouncilRuntime: councilRuntimeChecker(getBundle, adapters, councilSandbox(councilDocker != nil)),
		}
	if councilDocker != nil {
		hc.preflightCouncilHarness = councilDocker.CheckCouncilHarness
	}
	mux := newHandler(st, secretStore, getBundle, reloadFn, modelGate, dataActionGate, approvalQueue, reconciler, scheduler, conclaveBroker, attachHub, provReg, dash, hc)
	if ca, ok := adapters[domain.ClassCodingAgent].(*coding.Adapter); ok && ca.SupportsCouncilContainers() {
		ca.CouncilGateHandler, err = cfg.councilGateHandler(mux)
		if err != nil {
			return err
		}
		ca.CouncilSocketRoot = councilproxy.SocketRoot(dir)
	}

	if err := reconcileDaemonPass(ctx, reconciler.Recover, conclaveBroker); err != nil {
		logger.Warn("startup reconciliation completed with errors", "error", store.RedactCouncilAnswer(err.Error()))
	}
	if err := scheduler.Reload(ctx, bundle); err != nil {
		return err
	}
	defer scheduler.Stop()

	if gitSyncer != nil {
		interval := cfg.manifestsGitInterval
		if interval <= 0 {
			interval = 60 * time.Second
		}
		go func() {
			ticker := time.NewTicker(interval)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					changed, sha, err := gitSyncer.SyncOnce(ctx)
					if err != nil {
						logger.Warn("manifests git sync failed", "error", err)
						continue
					}
					if changed {
						if err := reloadFn(); err != nil {
							logger.Warn("reload after git sync failed", "sha", sha[:12], "error", err)
						} else {
							logger.Info("reloaded manifests from git update", "sha", sha[:12])
						}
					}
				}
			}
		}()
	}

	go func() {
		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				bundleMu.RLock()
				hasBundle := bundle != nil
				bundleMu.RUnlock()
				taskPass := reconciler.Recover
				if hasBundle {
					taskPass = reconciler.ReconcileOnce
				}
				if err := reconcileDaemonPass(ctx, taskPass, conclaveBroker); err != nil {
					logger.Warn("daemon reconciliation completed with errors", "error", store.RedactCouncilAnswer(err.Error()))
				}
			}
		}
	}()

	unixMux, err := cfg.hostHandler(mux, true)
	if err != nil {
		return err
	}
	mux, err = cfg.hostHandler(mux, false)
	if err != nil {
		return err
	}
	srv := newDaemonServer(mux)
	tcpLn, err := net.Listen("tcp", listenAddr)
	if err != nil {
		return fmt.Errorf("listen %q: %w", listenAddr, err)
	}
	limitedTCPLn := councilproxy.LimitListener(tcpLn, maxListenerConns)

	var unixSrv *http.Server
	if socketPath != "" {
		if err := os.MkdirAll(filepath.Dir(socketPath), 0o700); err != nil {
			_ = limitedTCPLn.Close()
			return fmt.Errorf("mkdir socket dir: %w", err)
		}
		_ = os.Remove(socketPath)
		uln, err := net.Listen("unix", socketPath)
		if err != nil {
			_ = limitedTCPLn.Close()
			return fmt.Errorf("listen unix socket %q: %w", socketPath, err)
		}
		_ = os.Chmod(socketPath, 0o600)
		defer os.Remove(socketPath)
		unixSrv = newDaemonServer(unixMux)
		go func() {
			logger.Info("sablecourtd unix socket listening", "path", socketPath)
			_ = unixSrv.Serve(councilproxy.LimitListener(uln, maxListenerConns))
		}()
	}

	errCh := make(chan error, 1)
	go func() {
		logger.Info("sablecourtd listening", "addr", listenAddr)
		if err := srv.Serve(limitedTCPLn); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
		close(errCh)
	}()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if unixSrv != nil {
			_ = unixSrv.Shutdown(shutdownCtx)
		}
		return srv.Shutdown(shutdownCtx)
	case err := <-errCh:
		return err
	}
}

// daemonIdleTimeout releases a capped listener slot held by an idle keep-alive
// connection. It does not cut a response that is still streaming.
const daemonIdleTimeout = 60 * time.Second

// daemonReadTimeout bounds reading a whole request, including the body that
// net/http drains after a handler rejects a request without reading it, so an
// unauthenticated stalled upload cannot hold a capped listener slot. It is a
// variable so tests can shorten it. No write timeout is set, so streaming
// responses are not cut.
var daemonReadTimeout = 60 * time.Second

// maxListenerConns caps open connections on each listener (TCP and Unix socket).
const maxListenerConns = 256

// maxStoreCloseErrorBytes bounds redacted store-close errors in daemon logs and exit output.
const maxStoreCloseErrorBytes = 1024

type redactedStoreCloseError struct {
	msg string
	err error
}

func (e *redactedStoreCloseError) Error() string { return e.msg }
func (e *redactedStoreCloseError) Unwrap() error { return e.err }

func sanitizeStoreCloseError(err error) string {
	s := strings.TrimSpace(store.RedactLearningText(err.Error()))
	if len(s) <= maxStoreCloseErrorBytes {
		return s
	}
	n := maxStoreCloseErrorBytes
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

func newDaemonServer(handler http.Handler) *http.Server {
	return &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       daemonReadTimeout,
		IdleTimeout:       daemonIdleTimeout,
	}
}

func initDaemonState(dir string) (string, io.Closer, error) {
	stateLock, err := operatortoken.LockStateDir(dir)
	if err != nil {
		return "", nil, fmt.Errorf("lock operator state directory: %w", err)
	}
	operatorToken, err := operatortoken.LoadOrCreate(dir)
	if err != nil {
		_ = stateLock.Close()
		return "", nil, fmt.Errorf("initialize operator credential: %w", err)
	}
	if err := councilproxy.SweepStaleSockets(councilproxy.SocketRoot(dir)); err != nil {
		_ = stateLock.Close()
		return "", nil, fmt.Errorf("sweep stale council gate sockets: %w", err)
	}
	return operatorToken, stateLock, nil
}

func shortDigest(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	return s
}

func reconcileDaemonPass(ctx context.Context, reconcileTasks func(context.Context) error, broker *conclave.Broker) error {
	taskErr := reconcileTasks(ctx)
	return errors.Join(taskErr, broker.ReconcileCouncils(ctx, taskErr == nil))
}

type handlerConfig struct {
	incidentTemplate    string
	operatorToken       string
	councilsDisabled    bool
	councilPrivateDirs  []string
	checkCouncilSandbox func(context.Context) error
	checkCouncilRuntime func(context.Context, store.CouncilCheckout) error
	// routeSink, when set, is told every route pattern the handler registers.
	// The HTTP API reference test uses it; routing does not depend on it.
	routeSink func(pattern string)
	// preflightCouncilHarness is set in container mode. It replaces the host
	// executable and sandbox checks with image, volume and login checks.
	preflightCouncilHarness func(context.Context, string) error
	// onCouncilCheckout is a test hook that runs once the checkout is verified.
	onCouncilCheckout func(expire context.CancelFunc)
}

func newHandler(st store.Store, secrets secret.SecretStore, getBundle func() *config.Bundle, reloadFn func() error, modelGate *model.Gate, dataActionGate *mcpgate.Gate, approvals *approval.Queue, reconciler *controlplane.Reconciler, scheduler *controlplane.Scheduler, conclaveBroker *conclave.Broker, attachHub *attach.Hub, provReg *providers.Registry, dash *ui.Dashboard, options ...handlerConfig) http.Handler {
	var cfg handlerConfig
	if len(options) > 0 {
		cfg = options[0]
	}
	serveMux := http.NewServeMux()
	mux := recordingMux{serveMux, cfg.routeSink}
	if dash != nil {
		dash.RegisterRoutes(mux)
	}
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.Handle("GET /metrics", telemetry.MetricsHandler(st))
	mux.Handle("POST /api/v1/hooks/github", webhook.GitHubHandler(webhook.GitHubOptions{
		Secrets:        secrets,
		Reconciler:     reconciler,
		TargetWorkload: "triage-agent",
	}))
	mux.Handle("POST /a2a", a2a.BrokerHandler(a2a.Options{
		Store:      st,
		Reconciler: reconciler,
		Secrets:    secrets,
	}))
	mux.HandleFunc("GET /api/v1/providers", func(w http.ResponseWriter, r *http.Request) {
		if provReg == nil {
			writeJSON(w, http.StatusOK, []any{})
			return
		}
		writeJSON(w, http.StatusOK, provReg.List(r.Context()))
	})
	mux.HandleFunc("GET /api/v1/providers/tiers", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, contextpack.GetModelTiers(r.Context(), st))
	})
	mux.HandleFunc("POST /api/v1/providers/tiers", func(w http.ResponseWriter, r *http.Request) {
		var routes []contextpack.TierRoute
		if err := json.NewDecoder(r.Body).Decode(&routes); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json: " + err.Error()})
			return
		}
		if err := contextpack.SaveModelTiers(r.Context(), st, routes); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, contextpack.GetModelTiers(r.Context(), st))
	})
	mux.HandleFunc("GET /api/v1/learnings", func(w http.ResponseWriter, r *http.Request) {
		repo := strings.TrimSpace(r.URL.Query().Get("repo"))
		var paths []string
		if p := strings.TrimSpace(r.URL.Query().Get("path")); p != "" {
			paths = strings.Split(p, ",")
		}
		writeJSON(w, http.StatusOK, contextpack.MatchLearnings(r.Context(), st, repo, paths, 100))
	})
	mux.HandleFunc("POST /api/v1/learnings", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusGone, map[string]string{"error": "legacy learning publication is disabled"})
	})
	mux.HandleFunc("POST /api/v1/providers/{id}/sync", func(w http.ResponseWriter, r *http.Request) {
		if provReg == nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "provider registry unavailable"})
			return
		}
		models, err := provReg.SyncModels(r.Context(), r.PathValue("id"))
		if err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, models)
	})
	mux.HandleFunc("GET /.well-known/agent-card.json", func(w http.ResponseWriter, r *http.Request) {
		var b *config.Bundle
		if getBundle != nil {
			b = getBundle()
		}
		writeJSON(w, http.StatusOK, buildAgentCard(b))
	})
	mux.HandleFunc("POST /api/v1/reload", func(w http.ResponseWriter, r *http.Request) {
		if reloadFn == nil {
			writeJSON(w, http.StatusNotImplemented, map[string]string{"error": "reload not configured"})
			return
		}
		if err := reloadFn(); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "reloaded"})
	})
	mux.HandleFunc("GET /api/v1/budget", func(w http.ResponseWriter, r *http.Request) {
		totals, err := st.TotalBudgetTotals(r.Context())
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, totals)
	})
	mux.HandleFunc("GET /api/v1/tasks/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		task, err := st.GetTask(r.Context(), id)
		if err != nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "task not found"})
			return
		}
		events, _ := st.ListTaskEvents(r.Context(), id)
		budget, _ := st.TaskBudgetTotals(r.Context(), id)
		writeJSON(w, http.StatusOK, map[string]any{
			"task":   task,
			"events": events,
			"budget": budget,
		})
	})
	mux.HandleFunc("POST /api/v1/tasks/{id}/cancel", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if err := reconciler.CancelTask(r.Context(), id, "api_cancelled"); err != nil {
			writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "cancelled", "id": id})
	})
	mux.HandleFunc("POST /api/v1/tasks/{id}/retry", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		withHandoff := r.URL.Query().Get("handoff") == "true" || r.URL.Query().Get("handoff") == "1"
		retried, err := reconciler.RetryTaskWithHandoff(r.Context(), id, withHandoff, "api_retry")
		if err != nil {
			status := http.StatusBadRequest
			if errors.Is(err, store.ErrNotFound) {
				status = http.StatusNotFound
			}
			writeJSON(w, status, map[string]string{"error": err.Error()})
			return
		}
		_ = st.AppendAudit(r.Context(), domain.AuditRecord{
			Category:     "task",
			TaskID:       retried.ID,
			WorkloadName: retried.WorkloadName,
			Action:       "task.retry",
			Outcome:      "submitted",
			DetailJSON:   fmt.Sprintf(`{"retried_from":%q,"handoff":%v}`, id, withHandoff),
			CreatedAt:    time.Now().UTC(),
		})
		writeJSON(w, http.StatusCreated, retried)
	})
	if conclaveBroker != nil {
		integrations.NewShadewright(integrations.ShadewrightOptions{
			Store:           st,
			Secrets:         secrets,
			GetBundle:       getBundle,
			Broker:          conclaveBroker,
			Reconciler:      reconciler,
			DefaultTemplate: cfg.incidentTemplate,
		}).RegisterRoutes(mux)
	}
	mux.HandleFunc("POST /api/v1/tasks/{id}/suspend", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if err := reconciler.SuspendTask(r.Context(), id, "api_suspended"); err != nil {
			writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "suspended", "id": id})
	})
	mux.HandleFunc("POST /api/v1/tasks/{id}/resume", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if err := reconciler.ResumeTask(r.Context(), id); err != nil {
			writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "working", "id": id})
	})
	if attachHub != nil {
		mux.HandleFunc("GET /api/v1/tasks/{id}/scry", func(w http.ResponseWriter, r *http.Request) {
			id := r.PathValue("id")
			hist, ch, closed, unsub, err := attachHub.Subscribe(id)
			if err != nil {
				writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
				return
			}
			defer unsub()

			flusher, ok := w.(http.Flusher)
			if !ok {
				http.Error(w, "streaming unsupported", http.StatusInternalServerError)
				return
			}
			_ = st.AppendAudit(r.Context(), domain.AuditRecord{
				Category:  "attach",
				TaskID:    id,
				Action:    "task.scry",
				Outcome:   "attached",
				CreatedAt: time.Now().UTC(),
			})
			w.Header().Set("Content-Type", "text/event-stream")
			w.Header().Set("Cache-Control", "no-cache")
			w.Header().Set("Connection", "keep-alive")

			if hist != "" {
				raw, _ := json.Marshal(hist)
				_, _ = fmt.Fprintf(w, "event: history\ndata: %s\n\n", raw)
				flusher.Flush()
			}
			if closed {
				_, _ = fmt.Fprintf(w, "event: closed\ndata: {}\n\n")
				flusher.Flush()
				return
			}
			for {
				select {
				case <-r.Context().Done():
					return
				case chunk, open := <-ch:
					if !open {
						_, _ = fmt.Fprintf(w, "event: closed\ndata: {}\n\n")
						flusher.Flush()
						return
					}
					raw, _ := json.Marshal(chunk)
					_, _ = fmt.Fprintf(w, "event: chunk\ndata: %s\n\n", raw)
					flusher.Flush()
				}
			}
		})
		mux.HandleFunc("POST /api/v1/tasks/{id}/takeover", func(w http.ResponseWriter, r *http.Request) {
			id := r.PathValue("id")
			var body struct {
				Operator string `json:"operator"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body.Operator == "" {
				body.Operator = "operator"
			}
			if err := attachHub.AcquireTakeover(id, body.Operator); err != nil {
				code := http.StatusConflict
				if errors.Is(err, attach.ErrSessionNotFound) {
					code = http.StatusNotFound
				}
				writeJSON(w, code, map[string]string{"error": err.Error()})
				return
			}
			_ = st.RecordHeartbeat(r.Context(), id, time.Now().UTC())
			_ = st.AppendAudit(r.Context(), domain.AuditRecord{
				Category:   "attach",
				TaskID:     id,
				Action:     "task.takeover",
				Outcome:    "acquired",
				DetailJSON: fmt.Sprintf(`{"operator":%q}`, body.Operator),
				CreatedAt:  time.Now().UTC(),
			})
			writeJSON(w, http.StatusOK, map[string]string{"status": "takeover", "id": id, "operator": body.Operator})
		})
		mux.HandleFunc("POST /api/v1/tasks/{id}/stdin", func(w http.ResponseWriter, r *http.Request) {
			id := r.PathValue("id")
			var body struct {
				Operator string `json:"operator"`
				Data     string `json:"data"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
				return
			}
			n, err := attachHub.SendInput(id, body.Operator, []byte(body.Data))
			if err != nil {
				code := http.StatusConflict
				if errors.Is(err, attach.ErrSessionNotFound) {
					code = http.StatusNotFound
				}
				writeJSON(w, code, map[string]string{"error": err.Error()})
				return
			}
			_ = st.RecordHeartbeat(r.Context(), id, time.Now().UTC())
			writeJSON(w, http.StatusOK, map[string]any{"status": "written", "bytes": n})
		})
		mux.HandleFunc("POST /api/v1/tasks/{id}/detach", func(w http.ResponseWriter, r *http.Request) {
			id := r.PathValue("id")
			var body struct {
				Operator string `json:"operator"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			attachHub.ReleaseTakeover(id, body.Operator)
			_ = st.AppendAudit(r.Context(), domain.AuditRecord{
				Category:   "attach",
				TaskID:     id,
				Action:     "task.detach",
				Outcome:    "released",
				DetailJSON: fmt.Sprintf(`{"operator":%q}`, body.Operator),
				CreatedAt:  time.Now().UTC(),
			})
			writeJSON(w, http.StatusOK, map[string]string{"status": "detached", "id": id})
		})
	}
	mux.HandleFunc("GET /api/v1/stream", func(w http.ResponseWriter, r *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "streaming unsupported", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")

		emit := func() {
			tasks, _ := st.ListTasks(r.Context())
			approvals, _ := st.ListApprovals(r.Context(), domain.ApprovalPending)
			audits, _ := st.ListAudit(r.Context(), 1)
			var lastAudit int64
			if len(audits) > 0 {
				lastAudit = audits[0].ID
			}
			var lastTaskUpd string
			if len(tasks) > 0 {
				lastTaskUpd = tasks[0].UpdatedAt.Format(time.RFC3339Nano)
			}
			fp := fmt.Sprintf("%d:%d:%d:%s", len(tasks), len(approvals), lastAudit, lastTaskUpd)
			_, _ = fmt.Fprintf(w, "event: state\ndata: %s\n\n", fp)
			flusher.Flush()
		}

		emit()
		ticker := time.NewTicker(3 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-r.Context().Done():
				return
			case <-ticker.C:
				emit()
			}
		}
	})
	voiceSvc := voice.New(voice.Options{Secrets: secrets})
	mux.HandleFunc("GET /api/v1/voice/status", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, voiceSvc.Status(r.Context()))
	})
	mux.HandleFunc("POST /api/v1/voice/transcribe", func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, voiceTranscribeMaxBytes)
		audioBytes, filename, lang, err := readUploadedAudio(r)
		if err != nil {
			var tooLarge *http.MaxBytesError
			if errors.As(err, &tooLarge) {
				writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "audio payload too large"})
				return
			}
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		text, err := voiceSvc.Transcribe(r.Context(), audioBytes, filename, lang)
		if err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
			return
		}
		_ = st.AppendAudit(r.Context(), domain.AuditRecord{
			Category:   "voice",
			Action:     "voice.transcribe",
			Outcome:    "ok",
			DetailJSON: fmt.Sprintf(`{"bytes":%d,"chars":%d}`, len(audioBytes), len(text)),
			CreatedAt:  time.Now().UTC(),
		})
		writeJSON(w, http.StatusOK, map[string]string{"text": text})
	})
	mux.HandleFunc("POST /api/v1/voice/speak", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Text  string `json:"text"`
			Voice string `json:"voice"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			var tooLarge *http.MaxBytesError
			if errors.As(err, &tooLarge) {
				writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "voice request too large"})
				return
			}
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		audioBytes, ct, err := voiceSvc.Speak(r.Context(), body.Text, body.Voice)
		if err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
			return
		}
		w.Header().Set("Content-Type", ct)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(audioBytes)
	})
	if conclaveBroker != nil {
		mux.HandleFunc("POST /api/v1/conclaves/{id}/voice-decree", func(w http.ResponseWriter, r *http.Request) {
			id := r.PathValue("id")
			audioBytes, filename, lang, err := readUploadedAudio(r)
			if err != nil {
				var tooLarge *http.MaxBytesError
				if errors.As(err, &tooLarge) {
					writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "audio payload too large"})
					return
				}
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
				return
			}
			text, err := voiceSvc.Transcribe(r.Context(), audioBytes, filename, lang)
			if err != nil {
				writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
				return
			}
			to := strings.TrimSpace(r.URL.Query().Get("to"))
			if to == "" && r.MultipartForm != nil {
				if vals := r.MultipartForm.Value["to"]; len(vals) > 0 {
					to = strings.TrimSpace(vals[0])
				}
			}
			if to == "" {
				to = "all"
			}
			msg, err := conclaveBroker.PostMessage(r.Context(), id, "operator", to, "decree", text, "")
			if err != nil {
				writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
				return
			}
			writeJSON(w, http.StatusCreated, map[string]any{"text": text, "message": msg})
		})
	}
	mux.HandleFunc("POST /api/v1/notify/test", func(w http.ResponseWriter, r *http.Request) {
		n := notify.New(notify.Options{Secrets: secrets})
		if err := n.Send(r.Context(), notify.Event{
			Type:   "notify.test",
			Title:  "Test notification from Sablecourt",
			Detail: "Outbound notifications are configured and reachable.",
			Path:   "/",
		}); err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "sent"})
	})
	mux.HandleFunc("GET /api/v1/observer/snapshot", func(w http.ResponseWriter, r *http.Request) {
		tasks, _ := st.ListTasks(r.Context())
		conclaves, _ := st.ListConclaves(r.Context())
		pendingApprovals, _ := st.ListApprovals(r.Context(), domain.ApprovalPending)
		budget, _ := st.TotalBudgetTotals(r.Context())

		type taskSummary struct {
			ID        string           `json:"id"`
			Workload  string           `json:"workload"`
			State     domain.TaskState `json:"state"`
			UpdatedAt time.Time        `json:"updatedAt"`
		}
		var activeTasks []taskSummary
		for _, t := range tasks {
			if t.State == domain.TaskStateWorking || t.State == domain.TaskStateSubmitted || t.State == domain.TaskStateInputRequired || t.State == domain.TaskStateSuspended {
				activeTasks = append(activeTasks, taskSummary{
					ID:        t.ID,
					Workload:  t.WorkloadName,
					State:     t.State,
					UpdatedAt: t.UpdatedAt,
				})
			}
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"source":           "sablecourt",
			"observedAt":       time.Now().UTC(),
			"activeTasks":      activeTasks,
			"conclaves":        conclaves,
			"pendingApprovals": len(pendingApprovals),
			"budget":           budget,
		})
	})
	mux.HandleFunc("GET /api/v1/observer/stream", func(w http.ResponseWriter, r *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "streaming unsupported", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		emit := func() {
			tasks, _ := st.ListTasks(r.Context())
			conclaves, _ := st.ListConclaves(r.Context())
			pending, _ := st.ListApprovals(r.Context(), domain.ApprovalPending)
			fp := fmt.Sprintf("%d:%d:%d", len(tasks), len(conclaves), len(pending))
			_, _ = fmt.Fprintf(w, "event: observer\ndata: %s\n\n", fp)
			flusher.Flush()
		}
		emit()
		ticker := time.NewTicker(3 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-r.Context().Done():
				return
			case <-ticker.C:
				emit()
			}
		}
	})
	mux.HandleFunc("GET /api/v1/memory", func(w http.ResponseWriter, r *http.Request) {
		ns := strings.TrimSpace(r.URL.Query().Get("namespace"))
		q := strings.TrimSpace(r.URL.Query().Get("q"))
		items, err := store.SearchVisibleMemory(r.Context(), st, ns, q, 100, store.IsLearningNamespace)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, items)
	})
	mux.HandleFunc("POST /api/v1/memory", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Namespace string `json:"namespace"`
			Key       string `json:"key"`
			Value     string `json:"value"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		if store.IsLearningNamespace(body.Namespace) {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "learning namespace is reserved"})
			return
		}
		body.Key = strings.TrimSpace(body.Key)
		if body.Key == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "key is required"})
			return
		}
		if strings.TrimSpace(body.Namespace) == "" {
			body.Namespace = "default"
		}
		entry := domain.MemoryEntry{
			Namespace: strings.TrimSpace(body.Namespace),
			Key:       body.Key,
			Value:     redact.String(strings.TrimSpace(body.Value)),
			Author:    "operator",
			UpdatedAt: time.Now().UTC(),
		}
		if err := st.PutMemory(r.Context(), entry); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusCreated, entry)
	})
	mux.HandleFunc("DELETE /api/v1/memory/{namespace}/{key}", func(w http.ResponseWriter, r *http.Request) {
		if store.IsLearningNamespace(r.PathValue("namespace")) {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "learning namespace is reserved"})
			return
		}
		if err := st.DeleteMemory(r.Context(), r.PathValue("namespace"), r.PathValue("key")); err != nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
	})
	mux.HandleFunc("GET /api/v1/workloads", func(w http.ResponseWriter, r *http.Request) {
		items, err := st.ListWorkloads(r.Context())
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, items)
	})
	mux.HandleFunc("GET /api/v1/tasks", func(w http.ResponseWriter, r *http.Request) {
		items, err := st.ListTasks(r.Context())
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, items)
	})
	mux.HandleFunc("POST /api/v1/tasks", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Workload       string `json:"workload"`
			IdempotencyKey string `json:"idempotencyKey"`
			InputJSON      string `json:"inputJson"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		task, err := reconciler.SubmitTask(r.Context(), body.Workload, body.IdempotencyKey, body.InputJSON)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusCreated, task)
	})
	mux.HandleFunc("GET /api/v1/audit", func(w http.ResponseWriter, r *http.Request) {
		items, err := st.ListAudit(r.Context(), 50)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, items)
	})
	mux.HandleFunc("GET /api/v1/audit/verify", func(w http.ResponseWriter, r *http.Request) {
		n, err := st.VerifyAuditChain(r.Context())
		if err != nil {
			writeJSON(w, http.StatusConflict, map[string]any{"valid": false, "verifiedRows": n, "error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"valid": true, "verifiedRows": n})
	})
	mux.HandleFunc("GET /api/v1/audit/export", func(w http.ResponseWriter, r *http.Request) {
		items, err := st.ListAudit(r.Context(), 10000)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		w.Header().Set("Content-Type", "application/x-ndjson")
		w.WriteHeader(http.StatusOK)
		enc := json.NewEncoder(w)
		for i := len(items) - 1; i >= 0; i-- {
			_ = enc.Encode(items[i])
		}
	})
	mux.HandleFunc("GET /api/v1/schedules", func(w http.ResponseWriter, r *http.Request) {
		var list []domain.Schedule
		if getBundle != nil {
			if b := getBundle(); b != nil {
				for _, sc := range b.Schedules {
					list = append(list, sc)
				}
			}
		}
		writeJSON(w, http.StatusOK, list)
	})
	mux.HandleFunc("POST /api/v1/schedules/{name}/run", func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")
		if scheduler == nil || getBundle == nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "scheduler unavailable"})
			return
		}
		b := getBundle()
		if b == nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "no manifest bundle loaded"})
			return
		}
		sc, ok := b.Schedules[name]
		if !ok {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "schedule not found"})
			return
		}
		if err := scheduler.TriggerNow(r.Context(), sc); err != nil {
			writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "triggered", "schedule": name})
	})
	mux.HandleFunc("GET /api/v1/approvals", func(w http.ResponseWriter, r *http.Request) {
		status := domain.ApprovalStatus(r.URL.Query().Get("status"))
		items, err := st.ListApprovals(r.Context(), status)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, items)
	})
	mux.HandleFunc("POST /api/v1/approvals/{id}/approve", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			ExpectedSHA256 string `json:"expectedSha256"`
			DecidedBy      string `json:"decidedBy"`
			DeviceID       string `json:"deviceId"`
			Signature      string `json:"signature"`
		}
		// An empty body leaves the pin empty and the queue refuses it (ErrDigestRequired, once an approver is configured); a malformed one is a 400 so a pin is not dropped silently.
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil && !errors.Is(err, io.EOF) {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid approval body"})
			return
		}
		if body.DeviceID != "" && body.Signature != "" {
			apr, err := approvals.ApproveWithSignature(r.Context(), r.PathValue("id"), body.ExpectedSHA256, body.DeviceID, body.Signature)
			if err != nil {
				writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
				return
			}
			writeJSON(w, http.StatusOK, apr)
			return
		}
		if body.DecidedBy == "" {
			body.DecidedBy = "operator"
		}
		apr, err := approvals.Approve(r.Context(), r.PathValue("id"), body.ExpectedSHA256, body.DecidedBy)
		if err != nil {
			writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, apr)
	})
	pairingUnavailable := func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "device enrollment requires independent confirmation"})
	}
	mux.HandleFunc("POST /api/v1/devices/pair/start", pairingUnavailable)
	mux.HandleFunc("POST /api/v1/devices/pair/complete", pairingUnavailable)
	mux.HandleFunc("GET /api/v1/devices", func(w http.ResponseWriter, r *http.Request) {
		items, err := st.ListDevices(r.Context())
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, items)
	})
	mux.HandleFunc("POST /api/v1/devices/{id}/revoke", func(w http.ResponseWriter, r *http.Request) {
		if err := st.RevokeDevice(r.Context(), r.PathValue("id")); err != nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "revoked", "id": r.PathValue("id")})
	})
	mux.HandleFunc("POST /api/v1/approvals/{id}/deny", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			DecidedBy string `json:"decidedBy"`
			Reason    string `json:"reason"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body.DecidedBy == "" {
			body.DecidedBy = "operator"
		}
		apr, err := approvals.Deny(r.Context(), r.PathValue("id"), body.DecidedBy, body.Reason)
		if err != nil {
			writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, apr)
	})
	mux.HandleFunc("GET /gate/task/messages", func(w http.ResponseWriter, r *http.Request) {
		lease, ok := authenticateGateLease(w, r, st)
		if !ok {
			return
		}
		msgs, err := st.ListTaskMessages(r.Context(), lease.TaskID)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, msgs)
	})
	mux.HandleFunc("POST /gate/task/messages", func(w http.ResponseWriter, r *http.Request) {
		lease, ok := authenticateGateLease(w, r, st)
		if !ok {
			return
		}
		var body struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || strings.TrimSpace(body.Content) == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "content is required"})
			return
		}
		if strings.EqualFold(strings.TrimSpace(body.Role), "system") {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "system role is reserved"})
			return
		}
		if err := st.AppendTaskMessage(r.Context(), domain.TaskMessage{
			TaskID:    lease.TaskID,
			Role:      "agent",
			Content:   redact.String(body.Content),
			CreatedAt: time.Now().UTC(),
		}); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusCreated, map[string]string{"status": "recorded", "taskId": lease.TaskID})
	})
	mux.HandleFunc("POST /api/v1/promote/{name}", func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")
		var b *config.Bundle
		if getBundle != nil {
			b = getBundle()
		}
		if b == nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "no manifest bundle loaded"})
			return
		}
		wl, ok := b.Workloads[name]
		if !ok {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "workload not found"})
			return
		}
		if wl.Metadata.Zone != domain.ZoneSandbox {
			writeJSON(w, http.StatusConflict, map[string]string{"error": "workload is already in zone " + string(wl.Metadata.Zone)})
			return
		}
		branch := "sablecourt/promote-" + name
		cmd := exec.CommandContext(r.Context(), "git", "branch", "--", branch) // #nosec G702 -- newHandler matches a manifest workload and passes its prefixed name as one argument after -- to git branch, without a shell.
		if out, err := cmd.CombinedOutput(); err != nil {
			writeJSON(w, http.StatusConflict, map[string]string{"error": "git branch failed: " + strings.TrimSpace(string(out))})
			return
		}
		_ = st.AppendAudit(r.Context(), domain.AuditRecord{
			Category:     "gitops",
			WorkloadName: name,
			Action:       "sandbox.propose_promotion",
			Outcome:      "branch_created",
			DetailJSON:   fmt.Sprintf(`{"branch":%q}`, branch),
			CreatedAt:    time.Now().UTC(),
		})
		writeJSON(w, http.StatusOK, map[string]string{
			"status":   "branch_created",
			"workload": name,
			"branch":   branch,
			"next":     "switch zone to runtime on " + branch + " and open a PR for review",
		})
	})
	if modelGate != nil {
		mux.Handle("/gate/model/", http.StripPrefix("/gate/model", modelGate.Handler()))
	}
	if dataActionGate != nil {
		mux.Handle("/gate/mcp", dataActionGate.MCPHandler())
		mux.Handle("/gate/actions/", http.StripPrefix("/gate/actions", dataActionGate.ActionHandler()))
	}
	if conclaveBroker != nil {
		mux.HandleFunc("POST /api/v1/councils", councilOpen(st, cfg))
		mux.HandleFunc("GET /api/v1/conclaves", func(w http.ResponseWriter, r *http.Request) {
			items, err := st.ListConclaves(r.Context())
			if err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
				return
			}
			writeJSON(w, http.StatusOK, items)
		})
		mux.HandleFunc("POST /api/v1/conclaves", func(w http.ResponseWriter, r *http.Request) {
			var body struct {
				Template string `json:"template"`
				Title    string `json:"title"`
				Decree   string `json:"decree"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
				return
			}
			sess, err := conclaveBroker.Open(r.Context(), body.Template, body.Title, body.Decree)
			if err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
				return
			}
			writeJSON(w, http.StatusCreated, sess)
		})
		mux.HandleFunc("GET /api/v1/conclaves/{id}", councilDetail(st, func(w http.ResponseWriter, r *http.Request) {
			id := r.PathValue("id")
			sess, err := st.GetConclave(r.Context(), id)
			if err != nil {
				writeJSON(w, http.StatusNotFound, map[string]string{"error": "conclave not found"})
				return
			}
			msgs, _ := st.ListConclaveMessages(r.Context(), id, 0)
			relics, _ := st.ListConclaveRelics(r.Context(), id)
			writeJSON(w, http.StatusOK, map[string]any{
				"conclave": sess,
				"messages": msgs,
				"relics":   relics,
			})
		}))
		mux.HandleFunc("GET /api/v1/conclaves/{id}/stream", func(w http.ResponseWriter, r *http.Request) {
			id := r.PathValue("id")
			flusher, ok := w.(http.Flusher)
			if !ok {
				http.Error(w, "streaming unsupported", http.StatusInternalServerError)
				return
			}
			w.Header().Set("Content-Type", "text/event-stream")
			w.Header().Set("Cache-Control", "no-cache")
			w.Header().Set("Connection", "keep-alive")

			emit := func() bool {
				sess, err := st.GetConclave(r.Context(), id)
				if err != nil {
					return false
				}
				msgs, _ := st.ListConclaveMessages(r.Context(), id, 0)
				relics, _ := st.ListConclaveRelics(r.Context(), id)
				var lastID int64
				if len(msgs) > 0 {
					lastID = msgs[len(msgs)-1].ID
				}
				fp := fmt.Sprintf("%s:%d:%d:%d", sess.State, len(msgs), lastID, len(relics))
				_, _ = fmt.Fprintf(w, "event: state\ndata: %s\n\n", fp) // #nosec G705 -- Broker.Close restricts requested states to lifecycle constants. newHandler streams that state and integer counters as text/event-stream.
				flusher.Flush()
				return true
			}
			if !emit() {
				return
			}
			ticker := time.NewTicker(2 * time.Second)
			defer ticker.Stop()
			for {
				select {
				case <-r.Context().Done():
					return
				case <-ticker.C:
					if !emit() {
						return
					}
				}
			}
		})
		mux.HandleFunc("POST /api/v1/conclaves/{id}/decree", func(w http.ResponseWriter, r *http.Request) {
			var body struct {
				To   string `json:"to"`
				Body string `json:"body"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
				return
			}
			msg, err := conclaveBroker.PostMessage(r.Context(), r.PathValue("id"), "operator", body.To, "decree", body.Body, "")
			if err != nil {
				writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
				return
			}
			writeJSON(w, http.StatusCreated, msg)
		})
		mux.HandleFunc("POST /api/v1/conclaves/{id}/close", func(w http.ResponseWriter, r *http.Request) {
			var body struct {
				State string `json:"state"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			if err := conclaveBroker.Close(r.Context(), r.PathValue("id"), domain.ConclaveState(body.State)); err != nil {
				writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
				return
			}
			writeJSON(w, http.StatusOK, map[string]string{"status": "closed"})
		})
		mux.HandleFunc("POST /api/v1/conclaves/{id}/relics", func(w http.ResponseWriter, r *http.Request) {
			var body struct {
				Name        string `json:"name"`
				ContentType string `json:"contentType"`
				Body        string `json:"body"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
				return
			}
			relic, err := conclaveBroker.PutRelic(r.Context(), r.PathValue("id"), "operator", body.Name, body.ContentType, body.Body)
			if err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
				return
			}
			writeJSON(w, http.StatusCreated, relic)
		})
		mux.HandleFunc("GET /api/v1/conclaves/{id}/relics/{name}", func(w http.ResponseWriter, r *http.Request) {
			relic, err := st.GetConclaveRelic(r.Context(), r.PathValue("id"), r.PathValue("name"))
			if err != nil {
				writeJSON(w, http.StatusNotFound, map[string]string{"error": "relic not found"})
				return
			}
			writeJSON(w, http.StatusOK, relic)
		})
		mux.HandleFunc("POST /gate/conclave/{id}/messages", func(w http.ResponseWriter, r *http.Request) {
			lease, ok := authenticateGateLease(w, r, st)
			if !ok {
				return
			}
			role, err := conclaveBroker.ResolveRoleForTask(r.Context(), r.PathValue("id"), lease.WorkloadName, lease.TaskID)
			if err != nil {
				writeJSON(w, http.StatusForbidden, map[string]string{"error": err.Error()})
				return
			}
			var body struct {
				To   string `json:"to"`
				Body string `json:"body"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
				return
			}
			msg, err := conclaveBroker.PostMessage(r.Context(), r.PathValue("id"), role, body.To, "proposal", body.Body, lease.TaskID)
			if err != nil {
				code := http.StatusConflict
				if errors.Is(err, conclave.ErrMessageDenied) {
					code = http.StatusForbidden
				}
				writeJSON(w, code, map[string]string{"error": err.Error()})
				return
			}
			writeJSON(w, http.StatusCreated, msg)
		})
		mux.HandleFunc("GET /gate/conclave/{id}/messages", func(w http.ResponseWriter, r *http.Request) {
			lease, ok := authenticateGateLease(w, r, st)
			if !ok {
				return
			}
			var since int64
			if raw := r.URL.Query().Get("since"); raw != "" {
				var err error
				since, err = strconv.ParseInt(raw, 10, 64)
				if err != nil || since < 0 {
					writeJSON(w, http.StatusBadRequest, map[string]string{"error": "since must be a non-negative integer"})
					return
				}
			}
			msgs, err := conclaveBroker.ReadMessagesForTask(r.Context(), r.PathValue("id"), lease.WorkloadName, lease.TaskID, since)
			if errors.Is(err, conclave.ErrMessageDenied) {
				writeJSON(w, http.StatusForbidden, map[string]string{"error": err.Error()})
				return
			}
			if err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
				return
			}
			writeJSON(w, http.StatusOK, msgs)
		})
		mux.HandleFunc("POST /gate/conclave/{id}/relics", func(w http.ResponseWriter, r *http.Request) {
			lease, ok := authenticateGateLease(w, r, st)
			if !ok {
				return
			}
			role, err := conclaveBroker.ResolveRoleForTask(r.Context(), r.PathValue("id"), lease.WorkloadName, lease.TaskID)
			if err != nil {
				writeJSON(w, http.StatusForbidden, map[string]string{"error": err.Error()})
				return
			}
			var body struct {
				Name        string `json:"name"`
				ContentType string `json:"contentType"`
				Body        string `json:"body"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
				return
			}
			relic, err := conclaveBroker.PutRelic(r.Context(), r.PathValue("id"), role, body.Name, body.ContentType, body.Body)
			if err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
				return
			}
			writeJSON(w, http.StatusCreated, relic)
		})
		mux.HandleFunc("GET /gate/conclave/{id}/relics/{name}", func(w http.ResponseWriter, r *http.Request) {
			lease, ok := authenticateGateLease(w, r, st)
			if !ok {
				return
			}
			relic, err := conclaveBroker.ReadRelicForTask(r.Context(), r.PathValue("id"), lease.WorkloadName, lease.TaskID, r.PathValue("name"))
			if errors.Is(err, conclave.ErrMessageDenied) {
				writeJSON(w, http.StatusForbidden, map[string]string{"error": err.Error()})
				return
			}
			if errors.Is(err, store.ErrNotFound) {
				writeJSON(w, http.StatusNotFound, map[string]string{"error": "relic not found"})
				return
			}
			if err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
				return
			}
			writeJSON(w, http.StatusOK, relic)
		})
	}
	auth := newOperatorAuth(cfg.operatorToken)
	auth.onRoute = cfg.routeSink
	mux.HandleFunc("GET /login", operatorLoginPage)
	mux.HandleFunc("POST /login", auth.login)
	return securityHeadersMiddleware(auth.requireOperatorAuth(serveMux, guardOperatorRequest(serveMux), secrets))
}

// Gate requests can arrive after a terminal transition and before lease cleanup.
type gateLeaseStore struct{ store.Store }

func (s gateLeaseStore) GetActiveLeaseByTokenHash(ctx context.Context, hash string, now time.Time) (domain.Lease, error) {
	lease, err := s.Store.GetActiveLeaseByTokenHash(ctx, hash, now)
	if err != nil {
		return domain.Lease{}, err
	}
	task, err := s.GetTask(ctx, lease.TaskID)
	if err != nil {
		return domain.Lease{}, err
	}
	if task.WorkloadName != lease.WorkloadName || (task.State != domain.TaskStateWorking && task.State != domain.TaskStateInputRequired) {
		return domain.Lease{}, store.ErrNotFound
	}
	return lease, nil
}

func authenticateGateLease(w http.ResponseWriter, r *http.Request, st store.Store) (domain.Lease, bool) {
	auth := r.Header.Get("Authorization")
	if !strings.HasPrefix(auth, "Bearer ") {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "bearer task token required"})
		return domain.Lease{}, false
	}
	tok := strings.TrimSpace(strings.TrimPrefix(auth, "Bearer "))
	lease, err := (gateLeaseStore{st}).GetActiveLeaseByTokenHash(r.Context(), model.HashToken(tok), time.Now().UTC())
	if err != nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid or expired task token"})
		return domain.Lease{}, false
	}
	return lease, true
}

func discoverManifestsPath() string {
	candidates := []string{
		"manifests",
		filepath.Join("examples", "local-demo.yaml"),
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		candidates = append(candidates, filepath.Join(home, ".config", "sablecourt", "manifests"))
	}
	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil {
			return c
		}
	}
	return ""
}

func buildAgentCard(bundle *config.Bundle) map[string]any {
	type skill struct {
		ID          string   `json:"id"`
		Name        string   `json:"name"`
		Description string   `json:"description"`
		Tags        []string `json:"tags,omitempty"`
	}
	var skills []skill
	if bundle != nil {
		for _, w := range bundle.Workloads {
			// Sandbox workloads are never advertised externally on the broker Agent Card.
			if w.Metadata.Zone == domain.ZoneSandbox {
				continue
			}
			skills = append(skills, skill{
				ID:          w.Metadata.Name,
				Name:        w.Metadata.Name,
				Description: w.Spec.AgentCard.Description,
				Tags:        w.Spec.AgentCard.Skills,
			})
		}
	}
	return map[string]any{
		"name":        "Sablecourt Control Plane",
		"description": "Policy-governed broker for heterogeneous Shadewright Labs workloads.",
		"version":     "0.1.0",
		"capabilities": map[string]bool{
			"streaming":       true,
			"stateTransition": true,
			"humanInTheLoop":  true,
		},
		"skills": skills,
	}
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func readUploadedAudio(r *http.Request) ([]byte, string, string, error) {
	ct := r.Header.Get("Content-Type")
	if strings.HasPrefix(ct, "multipart/form-data") {
		if err := r.ParseMultipartForm(voiceTranscribeMaxBytes); err != nil { // #nosec G120 -- newHandler caps transcribe at 24 MiB and guardOperatorRequest caps voice-decree at 2 MiB with MaxBytesReader.
			return nil, "", "", fmt.Errorf("parse multipart audio: %w", err)
		}
		f, hdr, err := r.FormFile("file")
		if err != nil {
			return nil, "", "", fmt.Errorf("missing audio form field 'file': %w", err)
		}
		defer f.Close() //nolint:errcheck // uploaded form file, read only
		data, err := io.ReadAll(f)
		if err != nil {
			return nil, "", "", err
		}
		lang := strings.TrimSpace(r.FormValue("language"))
		return data, hdr.Filename, lang, nil
	}
	data, err := io.ReadAll(r.Body)
	if err != nil {
		return nil, "", "", err
	}
	return data, "audio.webm", strings.TrimSpace(r.URL.Query().Get("language")), nil
}

// policyLoader reads the manifests, records the declared workloads and builds
// the policy engine for each load.
type policyLoader struct {
	st            store.Store
	manifestsPath string
	gateBreaker   *policy.CircuitBreaker
}

func newPolicyLoader(st store.Store, manifestsPath string) *policyLoader {
	return &policyLoader{st: st, manifestsPath: manifestsPath, gateBreaker: policy.NewCircuitBreaker(3, 2*time.Minute)}
}

// Breaker returns the circuit breaker the action gate records outcomes on.
func (l *policyLoader) Breaker() *policy.CircuitBreaker {
	return l.gateBreaker
}

func (l *policyLoader) Load(ctx context.Context) (*config.Bundle, *policy.Engine, error) {
	if l.manifestsPath == "" {
		return nil, policy.NewEngine(nil, nil), nil
	}
	loaded, err := config.LoadPath(l.manifestsPath)
	if err != nil {
		return nil, nil, fmt.Errorf("load manifests: %w", err)
	}
	for _, w := range loaded.Workloads {
		cardBytes, err := json.Marshal(w.Spec.AgentCard)
		if err != nil {
			return nil, nil, err
		}
		if err := l.st.UpsertWorkload(ctx, domain.WorkloadRecord{
			Name:           w.Metadata.Name,
			Zone:           w.Metadata.Zone,
			Class:          w.Spec.Class,
			ManifestDigest: loaded.ManifestDigest,
			AgentCardJSON:  string(cardBytes),
		}); err != nil {
			return nil, nil, err
		}
	}
	var policies []domain.Policy
	for _, p := range loaded.Policies {
		policies = append(policies, p)
	}
	breaker := policy.NewCircuitBreaker(3, 2*time.Minute)
	return loaded, policy.NewEngine(policies, breaker), nil
}

// serialized returns fn guarded by one mutex, so calls never overlap and
// each runs to the end before the next starts.
func serialized(fn func() error) func() error {
	var mu sync.Mutex
	return func() error {
		mu.Lock()
		defer mu.Unlock()
		return fn()
	}
}

// reloadGitOps applies a reloaded bundle and policy to the action gate and
// checks the repo root's origin against the repository pinned at start. Every
// reload trigger (POST /api/v1/reload, the dashboard and the manifests git
// sync) runs it through the daemon's one reload function. It never re-pins: a
// changed or unreadable origin keeps git.open_pr refused, is logged here, and
// Recheck writes it to the audit log once per change of the observed origin.
func reloadGitOps(ctx context.Context, logger *slog.Logger, gate *mcpgate.Gate, binding *mcpgate.RepoBinding, b *config.Bundle, pol *policy.Engine) {
	gate.Reload(b, pol)
	if err := binding.Recheck(ctx); err != nil {
		logger.Warn("git.open_pr refused after reload", "error", err)
	}
}
