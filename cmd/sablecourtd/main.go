package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/shadewright-labs/sablecourt/internal/approval"
	"github.com/shadewright-labs/sablecourt/internal/config"
	"github.com/shadewright-labs/sablecourt/internal/controlplane"
	"github.com/shadewright-labs/sablecourt/internal/domain"
	mcpgate "github.com/shadewright-labs/sablecourt/internal/gate/mcp"
	"github.com/shadewright-labs/sablecourt/internal/gate/model"
	"github.com/shadewright-labs/sablecourt/internal/policy"
	"github.com/shadewright-labs/sablecourt/internal/providers"
	scruntime "github.com/shadewright-labs/sablecourt/internal/runtime"
	"github.com/shadewright-labs/sablecourt/internal/runtime/coding"
	"github.com/shadewright-labs/sablecourt/internal/runtime/controller"
	"github.com/shadewright-labs/sablecourt/internal/runtime/docker"
	"github.com/shadewright-labs/sablecourt/internal/secret"
	"github.com/shadewright-labs/sablecourt/internal/store"
	"github.com/shadewright-labs/sablecourt/internal/store/sqlite"
	"github.com/shadewright-labs/sablecourt/internal/telemetry"
	"github.com/shadewright-labs/sablecourt/internal/ui"
)

func main() {
	var (
		dbPath        string
		manifestsPath string
		listenAddr    string
		socketPath    string
		gateHost      string
		secretFile    string
		repoRoot      string
	)

	rootCmd := &cobra.Command{
		Use:   "sablecourtd",
		Short: "Sablecourt local-first control plane daemon",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runDaemon(cmd.Context(), dbPath, manifestsPath, listenAddr, socketPath, gateHost, secretFile, repoRoot)
		},
	}

	rootCmd.Flags().StringVar(&dbPath, "db", "sablecourt.db", "SQLite database path")
	rootCmd.Flags().StringVar(&manifestsPath, "manifests", "", "Path to Git-tracked YAML manifests file or directory")
	rootCmd.Flags().StringVar(&listenAddr, "listen", "127.0.0.1:7432", "HTTP listen address")
	rootCmd.Flags().StringVar(&socketPath, "socket", "", "Optional Unix domain socket path for local CLI access (0600)")
	rootCmd.Flags().StringVar(&gateHost, "gate-host", "", "Optional host:port injected into containers for gates (defaults to --listen)")
	rootCmd.Flags().StringVar(&secretFile, "secret-file", "", "Optional 0600 JSON secret file fallback path")
	rootCmd.Flags().StringVar(&repoRoot, "repo-root", ".", "Git repository root for GitOps typed actions")

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := rootCmd.ExecuteContext(ctx); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func runDaemon(ctx context.Context, dbPath, manifestsPath, listenAddr, socketPath, gateHost, secretFile, repoRoot string) error {
	logger := telemetry.NewLogger()

	st, err := sqlite.Open(dbPath)
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
		logger.Info("loaded manifests", "workloads", len(bundle.Workloads), "policies", len(bundle.Policies), "budgets", len(bundle.Budgets))
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

	adapters := map[domain.WorkloadClass]scruntime.Adapter{
		domain.ClassCodingAgent: coding.NewAdapter(os.TempDir()),
	}
	if dockerAdapter, err := docker.NewFromEnv(); err == nil {
		adapters[domain.ClassLongLivedAgent] = dockerAdapter
		adapters[domain.ClassDeterministicController] = controller.NewAdapter(dockerAdapter)
	} else {
		logger.Warn("docker runtime unavailable", "error", err)
	}

	if gateHost == "" {
		gateHost = listenAddr
	}
	reconciler := controlplane.NewReconciler(controlplane.Options{
		Store:        st,
		Bundle:       bundle,
		Policy:       policyEng,
		Adapters:     adapters,
		ModelGateURL: "http://" + gateHost + "/gate/model",
		DataGateURL:  "http://" + gateHost + "/gate/mcp",
	})

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
		if b != nil {
			logger.Info("reloaded manifests", "workloads", len(b.Workloads), "digest", b.ManifestDigest[:12])
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
		Secrets: secretStore,
	})

	getBundle := func() *config.Bundle {
		bundleMu.RLock()
		defer bundleMu.RUnlock()
		return bundle
	}

	dash, err := ui.NewDashboard(ui.Options{
		Store:         st,
		Approvals:     approvalQueue,
		Reconciler:    reconciler,
		Providers:     provReg,
		GetBundle:     getBundle,
		Reload:        reloadFn,
		ManifestsPath: manifestsPath,
	})
	if err != nil {
		return fmt.Errorf("init web ui: %w", err)
	}

	mux := newHandler(st, getBundle, reloadFn, modelGate, dataActionGate, approvalQueue, reconciler, provReg, dash)
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

func newHandler(st store.Store, getBundle func() *config.Bundle, reloadFn func() error, modelGate *model.Gate, dataActionGate *mcpgate.Gate, approvals *approval.Queue, reconciler *controlplane.Reconciler, provReg *providers.Registry, dash *ui.Dashboard) http.Handler {
	mux := http.NewServeMux()
	if dash != nil {
		dash.RegisterRoutes(mux)
	}
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
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
	if modelGate != nil {
		mux.Handle("/gate/model/", http.StripPrefix("/gate/model", modelGate.Handler()))
	}
	if dataActionGate != nil {
		mux.Handle("/gate/mcp", dataActionGate.MCPHandler())
		mux.Handle("/gate/actions/", http.StripPrefix("/gate/actions", dataActionGate.ActionHandler()))
	}
	return mux
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
