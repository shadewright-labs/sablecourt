package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
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
	scruntime "github.com/shadewright-labs/sablecourt/internal/runtime"
	"github.com/shadewright-labs/sablecourt/internal/runtime/coding"
	"github.com/shadewright-labs/sablecourt/internal/runtime/controller"
	"github.com/shadewright-labs/sablecourt/internal/runtime/docker"
	"github.com/shadewright-labs/sablecourt/internal/secret"
	"github.com/shadewright-labs/sablecourt/internal/store"
	"github.com/shadewright-labs/sablecourt/internal/store/sqlite"
	"github.com/shadewright-labs/sablecourt/internal/ui"
)

func main() {
	var (
		dbPath        string
		manifestsPath string
		listenAddr    string
		secretFile    string
	)

	rootCmd := &cobra.Command{
		Use:   "sablecourtd",
		Short: "Sablecourt local-first control plane daemon",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runDaemon(cmd.Context(), dbPath, manifestsPath, listenAddr, secretFile)
		},
	}

	rootCmd.Flags().StringVar(&dbPath, "db", "sablecourt.db", "SQLite database path")
	rootCmd.Flags().StringVar(&manifestsPath, "manifests", "", "Path to Git-tracked YAML manifests file or directory")
	rootCmd.Flags().StringVar(&listenAddr, "listen", "127.0.0.1:7432", "HTTP listen address")
	rootCmd.Flags().StringVar(&secretFile, "secret-file", "", "Optional 0600 JSON secret file fallback path")

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := rootCmd.ExecuteContext(ctx); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func runDaemon(ctx context.Context, dbPath, manifestsPath, listenAddr, secretFile string) error {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	st, err := sqlite.Open(dbPath)
	if err != nil {
		return err
	}
	defer st.Close()

	if err := st.Migrate(ctx); err != nil {
		return err
	}

	var bundle *config.Bundle
	if manifestsPath != "" {
		bundle, err = config.LoadPath(manifestsPath)
		if err != nil {
			return fmt.Errorf("load manifests: %w", err)
		}
		for _, w := range bundle.Workloads {
			cardBytes, err := json.Marshal(w.Spec.AgentCard)
			if err != nil {
				return err
			}
			if err := st.UpsertWorkload(ctx, domain.WorkloadRecord{
				Name:           w.Metadata.Name,
				Zone:           w.Metadata.Zone,
				Class:          w.Spec.Class,
				ManifestDigest: bundle.ManifestDigest,
				AgentCardJSON:  string(cardBytes),
			}); err != nil {
				return err
			}
		}
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

	var policies []domain.Policy
	if bundle != nil {
		for _, p := range bundle.Policies {
			policies = append(policies, p)
		}
	}
	breaker := policy.NewCircuitBreaker(3, 2*time.Minute)
	policyEng := policy.NewEngine(policies, breaker)
	approvalQueue := approval.NewQueue(st, &approval.MacOSDialogApprover{})
	dataActionGate := mcpgate.New(mcpgate.Options{
		Store:     st,
		Bundle:    bundle,
		Policy:    policyEng,
		Breaker:   breaker,
		Approvals: approvalQueue,
	})

	adapters := map[domain.WorkloadClass]scruntime.Adapter{
		domain.ClassCodingAgent: coding.NewAdapter(os.TempDir()),
	}
	if dockerAdapter, err := docker.NewFromEnv(); err == nil {
		adapters[domain.ClassLongLivedAgent] = dockerAdapter
		adapters[domain.ClassDeterministicController] = controller.NewAdapter(dockerAdapter)
	} else {
		logger.Warn("docker runtime unavailable", "error", err)
	}

	reconciler := controlplane.NewReconciler(controlplane.Options{
		Store:        st,
		Bundle:       bundle,
		Policy:       policyEng,
		Adapters:     adapters,
		ModelGateURL: "http://" + listenAddr + "/gate/model",
		DataGateURL:  "http://" + listenAddr + "/gate/mcp",
	})

	go func() {
		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if bundle != nil {
					_ = reconciler.ReconcileOnce(ctx)
				}
			}
		}
	}()

	dash, err := ui.NewDashboard(st)
	if err != nil {
		return fmt.Errorf("init web ui: %w", err)
	}

	mux := newHandler(st, modelGate, dataActionGate, approvalQueue, reconciler, dash)
	srv := &http.Server{
		Addr:              listenAddr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
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
		return srv.Shutdown(shutdownCtx)
	case err := <-errCh:
		return err
	}
}

func newHandler(st store.Store, modelGate *model.Gate, dataActionGate *mcpgate.Gate, approvals *approval.Queue, reconciler *controlplane.Reconciler, dash http.Handler) http.Handler {
	mux := http.NewServeMux()
	if dash != nil {
		mux.Handle("GET /{$}", dash)
	}
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
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

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
