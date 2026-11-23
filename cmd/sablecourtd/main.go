package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/shadewright-labs/sablecourt/internal/a2a"
	"github.com/shadewright-labs/sablecourt/internal/approval"
	"github.com/shadewright-labs/sablecourt/internal/attach"
	"github.com/shadewright-labs/sablecourt/internal/conclave"
	"github.com/shadewright-labs/sablecourt/internal/config"
	"github.com/shadewright-labs/sablecourt/internal/controlplane"
	"github.com/shadewright-labs/sablecourt/internal/domain"
	mcpgate "github.com/shadewright-labs/sablecourt/internal/gate/mcp"
	"github.com/shadewright-labs/sablecourt/internal/gate/model"
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
	"github.com/shadewright-labs/sablecourt/internal/webhook"
)

func main() {
	var (
		dbPath          string
		postgresDSN     string
		manifestsPath   string
		listenAddr       string
		socketPath       string
		gateHost         string
		secretFile       string
		repoRoot         string
		runtimeBackend   string
		k8sNamespace         string
		k8sRuntimeClass      string
		kubeconfigPath       string
		operatorAuthRequired bool
	)

	rootCmd := &cobra.Command{
		Use:   "sablecourtd",
		Short: "Sablecourt local-first control plane daemon",
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
				operatorAuthRequired: operatorAuthRequired,
			})
		},
	}

	rootCmd.Flags().StringVar(&dbPath, "db", "sablecourt.db", "SQLite database path")
	rootCmd.Flags().StringVar(&postgresDSN, "postgres-dsn", "", "Optional Postgres DSN (or set SABLECOURT_POSTGRES_DSN) instead of SQLite")
	rootCmd.Flags().StringVar(&manifestsPath, "manifests", "", "Path to Git-tracked YAML manifests file or directory")
	rootCmd.Flags().StringVar(&listenAddr, "listen", "127.0.0.1:7432", "HTTP listen address")
	rootCmd.Flags().StringVar(&socketPath, "socket", "", "Optional Unix domain socket path for local CLI access (0600)")
	rootCmd.Flags().StringVar(&gateHost, "gate-host", "", "Optional host:port injected into containers for gates (defaults to --listen)")
	rootCmd.Flags().StringVar(&secretFile, "secret-file", "", "Optional 0600 JSON secret file fallback path")
	rootCmd.Flags().StringVar(&repoRoot, "repo-root", ".", "Git repository root for GitOps typed actions")
	rootCmd.Flags().StringVar(&runtimeBackend, "runtime", "docker", "Container runtime backend: docker or k8s")
	rootCmd.Flags().StringVar(&k8sNamespace, "k8s-namespace", "default", "Kubernetes namespace when --runtime=k8s")
	rootCmd.Flags().StringVar(&k8sRuntimeClass, "k8s-runtime-class", "", "Optional Kubernetes RuntimeClassName (e.g. gvisor, kata)")
	rootCmd.Flags().StringVar(&kubeconfigPath, "kubeconfig", "", "Optional kubeconfig path when --runtime=k8s")
	rootCmd.Flags().BoolVar(&operatorAuthRequired, "operator-auth-required", false, "Require Authorization: Bearer <operator_bearer_token> on operator UI and API routes")

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := rootCmd.ExecuteContext(ctx); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
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
	operatorAuthRequired bool
}

func runDaemon(ctx context.Context, cfg daemonConfig) error {
	dbPath := cfg.dbPath
	manifestsPath := cfg.manifestsPath
	listenAddr := cfg.listenAddr
	socketPath := cfg.socketPath
	gateHost := cfg.gateHost
	secretFile := cfg.secretFile
	repoRoot := cfg.repoRoot

	logger := telemetry.NewLogger()

	if manifestsPath == "" {
		manifestsPath = discoverManifestsPath()
	}

	pgDSN := cfg.postgresDSN
	if pgDSN == "" {
		pgDSN = os.Getenv("SABLECOURT_POSTGRES_DSN")
	}
	var (
		st  store.Store
		err error
	)
	if pgDSN != "" {
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
	defer st.Close()

	if err := st.Migrate(ctx); err != nil {
		return err
	}

	var (
		bundleMu sync.RWMutex
		bundle   *config.Bundle
	)
	loadAndSync := func() (*config.Bundle, *policy.Engine, error) {
		if manifestsPath == "" {
			return nil, policy.NewEngine(nil, nil), nil
		}
		b, err := config.LoadPath(manifestsPath)
		if err != nil {
			return nil, nil, fmt.Errorf("load manifests: %w", err)
		}
		for _, w := range b.Workloads {
			cardBytes, err := json.Marshal(w.Spec.AgentCard)
			if err != nil {
				return nil, nil, err
			}
			if err := st.UpsertWorkload(ctx, domain.WorkloadRecord{
				Name:           w.Metadata.Name,
				Zone:           w.Metadata.Zone,
				Class:          w.Spec.Class,
				ManifestDigest: b.ManifestDigest,
				AgentCardJSON:  string(cardBytes),
			}); err != nil {
				return nil, nil, err
			}
		}
		var policies []domain.Policy
		for _, p := range b.Policies {
			policies = append(policies, p)
		}
		breaker := policy.NewCircuitBreaker(3, 2*time.Minute)
		return b, policy.NewEngine(policies, breaker), nil
	}

	var policyEng *policy.Engine
	bundle, policyEng, err = loadAndSync()
	if err != nil {
		return err
	}
	if bundle != nil {
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
		Store:   st,
		Secrets: secretStore,
		Bundle:  bundle,
	})

	breaker := policy.NewCircuitBreaker(3, 2*time.Minute)
	approvalQueue := approval.NewQueue(st, &approval.MacOSDialogApprover{})
	dataActionGate := mcpgate.New(mcpgate.Options{
		Store:     st,
		Bundle:    bundle,
		Policy:    policyEng,
		Breaker:   breaker,
		Approvals: approvalQueue,
	})
	mcpgate.RegisterBuiltinGitOps(dataActionGate, repoRoot, &mcpgate.GitHubPROpener{Secrets: secretStore})

	attachHub := attach.NewHub()
	codingAdapter := coding.NewAdapter(os.TempDir())
	codingAdapter.AttachHub = attachHub
	adapters := map[domain.WorkloadClass]scruntime.Adapter{
		domain.ClassCodingAgent: codingAdapter,
	}
	switch cfg.runtimeBackend {
	case "k8s":
		if k8sAdapter, err := k8sruntime.NewFromConfig(cfg.kubeconfigPath, cfg.k8sNamespace, cfg.k8sRuntimeClass); err == nil {
			k8sAdapter.SetAttachHub(attachHub)
			adapters[domain.ClassLongLivedAgent] = k8sAdapter
			adapters[domain.ClassDeterministicController] = controller.NewAdapter(k8sAdapter)
			logger.Info("using kubernetes runtime adapter", "namespace", cfg.k8sNamespace, "runtimeClass", cfg.k8sRuntimeClass)
		} else {
			logger.Warn("kubernetes runtime unavailable", "error", err)
		}
	default:
		if dockerAdapter, err := docker.NewFromEnv(); err == nil {
			dockerAdapter.AttachHub = attachHub
			adapters[domain.ClassLongLivedAgent] = dockerAdapter
			adapters[domain.ClassDeterministicController] = controller.NewAdapter(dockerAdapter)
		} else {
			logger.Warn("docker runtime unavailable", "error", err)
		}
	}

	if gateHost == "" {
		if _, port, err := net.SplitHostPort(listenAddr); err == nil && strings.HasPrefix(listenAddr, "127.0.0.1:") {
			gateHost = "host.docker.internal:" + port
		} else {
			gateHost = listenAddr
		}
	}
	reconciler := controlplane.NewReconciler(controlplane.Options{
		Store:        st,
		Bundle:       bundle,
		Policy:       policyEng,
		Adapters:     adapters,
		ModelGateURL: "http://" + gateHost + "/gate/model",
		DataGateURL:  "http://" + gateHost + "/gate/mcp",
	})
	scheduler := controlplane.NewScheduler(reconciler)
	if err := scheduler.Reload(ctx, bundle); err != nil {
		return err
	}
	defer scheduler.Stop()

	reloadFn := func() error {
		b, pol, err := loadAndSync()
		if err != nil {
			return err
		}
		bundleMu.Lock()
		bundle = b
		bundleMu.Unlock()
		modelGate.SetBundle(b)
		dataActionGate.Reload(b, pol)
		reconciler.Reload(b, pol)
		_ = scheduler.Reload(ctx, b)
		if b != nil {
			logger.Info("reloaded manifests", "workloads", len(b.Workloads), "schedules", len(b.Schedules), "digest", b.ManifestDigest[:12])
		}
		return nil
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
				if hasBundle {
					_ = reconciler.ReconcileOnce(ctx)
				}
			}
		}
	}()

	provReg := providers.NewRegistry(providers.Options{
		Store:   st,
		Secrets: secretStore,
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
	})
	conclaveBroker.RegisterMCPTools(dataActionGate)

	dash, err := ui.NewDashboard(ui.Options{
		Store:         st,
		Approvals:     approvalQueue,
		Reconciler:    reconciler,
		Conclave:      conclaveBroker,
		Providers:     provReg,
		GetBundle:     getBundle,
		Reload:        reloadFn,
		ManifestsPath: manifestsPath,
	})
	if err != nil {
		return fmt.Errorf("init web ui: %w", err)
	}

	mux := newHandler(st, secretStore, getBundle, reloadFn, modelGate, dataActionGate, approvalQueue, reconciler, conclaveBroker, attachHub, provReg, dash)
	if cfg.operatorAuthRequired {
		logger.Info("operator bearer authentication enabled on UI and API routes")
		mux = requireOperatorAuth(mux, secretStore)
	}
	srv := &http.Server{
		Addr:              listenAddr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	var unixSrv *http.Server
	if socketPath != "" {
		if err := os.MkdirAll(filepath.Dir(socketPath), 0o700); err != nil {
			return fmt.Errorf("mkdir socket dir: %w", err)
		}
		_ = os.Remove(socketPath)
		uln, err := net.Listen("unix", socketPath)
		if err != nil {
			return fmt.Errorf("listen unix socket %q: %w", socketPath, err)
		}
		_ = os.Chmod(socketPath, 0o600)
		defer os.Remove(socketPath)
		unixSrv = &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
		go func() {
			logger.Info("sablecourtd unix socket listening", "path", socketPath)
			_ = unixSrv.Serve(uln)
		}()
	}

	errCh := make(chan error, 1)
	go func() {
		logger.Info("sablecourtd listening", "addr", listenAddr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
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

func newHandler(st store.Store, secrets secret.SecretStore, getBundle func() *config.Bundle, reloadFn func() error, modelGate *model.Gate, dataActionGate *mcpgate.Gate, approvals *approval.Queue, reconciler *controlplane.Reconciler, conclaveBroker *conclave.Broker, attachHub *attach.Hub, provReg *providers.Registry, dash *ui.Dashboard) http.Handler {
	mux := http.NewServeMux()
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
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
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
		auth := r.Header.Get("Authorization")
		if !strings.HasPrefix(auth, "Bearer ") {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "bearer task token required"})
			return
		}
		tok := strings.TrimSpace(strings.TrimPrefix(auth, "Bearer "))
		lease, err := st.GetActiveLeaseByTokenHash(r.Context(), model.HashToken(tok), time.Now().UTC())
		if err != nil {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid or expired task token"})
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
		auth := r.Header.Get("Authorization")
		if !strings.HasPrefix(auth, "Bearer ") {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "bearer task token required"})
			return
		}
		tok := strings.TrimSpace(strings.TrimPrefix(auth, "Bearer "))
		lease, err := st.GetActiveLeaseByTokenHash(r.Context(), model.HashToken(tok), time.Now().UTC())
		if err != nil {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid or expired task token"})
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
		role := body.Role
		if role != "agent" && role != "system" {
			role = "agent"
		}
		if err := st.AppendTaskMessage(r.Context(), domain.TaskMessage{
			TaskID:    lease.TaskID,
			Role:      role,
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
		cmd := exec.CommandContext(r.Context(), "git", "branch", branch)
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
		mux.HandleFunc("GET /api/v1/conclaves/{id}", func(w http.ResponseWriter, r *http.Request) {
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
		mux.HandleFunc("POST /gate/conclave/{id}/messages", func(w http.ResponseWriter, r *http.Request) {
			lease, ok := authenticateGateLease(w, r, st)
			if !ok {
				return
			}
			role, err := conclaveBroker.ResolveRoleForWorkload(r.Context(), r.PathValue("id"), lease.WorkloadName)
			if err != nil {
				writeJSON(w, http.StatusForbidden, map[string]string{"error": err.Error()})
				return
			}
			var body struct {
				To   string `json:"to"`
				Kind string `json:"kind"`
				Body string `json:"body"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
				return
			}
			msg, err := conclaveBroker.PostMessage(r.Context(), r.PathValue("id"), role, body.To, body.Kind, body.Body, lease.TaskID)
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
			if _, err := conclaveBroker.ResolveRoleForWorkload(r.Context(), r.PathValue("id"), lease.WorkloadName); err != nil {
				writeJSON(w, http.StatusForbidden, map[string]string{"error": err.Error()})
				return
			}
			var since int64
			_, _ = fmt.Sscanf(r.URL.Query().Get("since"), "%d", &since)
			msgs, err := st.ListConclaveMessages(r.Context(), r.PathValue("id"), since)
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
			role, err := conclaveBroker.ResolveRoleForWorkload(r.Context(), r.PathValue("id"), lease.WorkloadName)
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
			if _, err := conclaveBroker.ResolveRoleForWorkload(r.Context(), r.PathValue("id"), lease.WorkloadName); err != nil {
				writeJSON(w, http.StatusForbidden, map[string]string{"error": err.Error()})
				return
			}
			relic, err := st.GetConclaveRelic(r.Context(), r.PathValue("id"), r.PathValue("name"))
			if err != nil {
				writeJSON(w, http.StatusNotFound, map[string]string{"error": "relic not found"})
				return
			}
			writeJSON(w, http.StatusOK, relic)
		})
	}
	return mux
}

func authenticateGateLease(w http.ResponseWriter, r *http.Request, st store.Store) (domain.Lease, bool) {
	auth := r.Header.Get("Authorization")
	if !strings.HasPrefix(auth, "Bearer ") {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "bearer task token required"})
		return domain.Lease{}, false
	}
	tok := strings.TrimSpace(strings.TrimPrefix(auth, "Bearer "))
	lease, err := st.GetActiveLeaseByTokenHash(r.Context(), model.HashToken(tok), time.Now().UTC())
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
			"streaming":         true,
			"stateTransition":   true,
			"humanInTheLoop":    true,
		},
		"skills": skills,
	}
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func requireOperatorAuth(next http.Handler, secrets secret.SecretStore) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		if p == "/healthz" ||
			p == "/.well-known/agent-card.json" ||
			p == "/favicon.ico" ||
			p == "/apple-touch-icon.png" ||
			strings.HasPrefix(p, "/assets/") ||
			strings.HasPrefix(p, "/gate/") ||
			p == "/api/v1/hooks/github" ||
			p == "/a2a" {
			next.ServeHTTP(w, r)
			return
		}
		if secrets == nil {
			w.Header().Set("WWW-Authenticate", `Bearer realm="sablecourt"`)
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "operator_bearer_token not configured"})
			return
		}
		want, err := secrets.Get(r.Context(), "operator_bearer_token")
		if err != nil || strings.TrimSpace(want) == "" {
			w.Header().Set("WWW-Authenticate", `Bearer realm="sablecourt"`)
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "operator_bearer_token not configured"})
			return
		}
		auth := r.Header.Get("Authorization")
		if !strings.HasPrefix(auth, "Bearer ") {
			w.Header().Set("WWW-Authenticate", `Bearer realm="sablecourt"`)
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "operator bearer token required"})
			return
		}
		got := strings.TrimSpace(strings.TrimPrefix(auth, "Bearer "))
		if subtle.ConstantTimeCompare([]byte(got), []byte(strings.TrimSpace(want))) != 1 {
			w.Header().Set("WWW-Authenticate", `Bearer realm="sablecourt"`)
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid operator bearer token"})
			return
		}
		next.ServeHTTP(w, r)
	})
}
