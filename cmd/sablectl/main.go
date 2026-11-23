package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/shadewright-labs/sablecourt/internal/config"
	"github.com/shadewright-labs/sablecourt/internal/domain"
	"github.com/shadewright-labs/sablecourt/internal/providers"
	"github.com/shadewright-labs/sablecourt/internal/secret"
)

const version = "0.1.0-dev"

func main() {
	var (
		apiURL     string
		socketPath string
		secretFile string
		jsonOut    bool
	)

	rootCmd := &cobra.Command{
		Use:   "sablectl",
		Short: "CLI client for the Sablecourt control plane",
	}
	rootCmd.PersistentFlags().StringVar(&apiURL, "api", "http://127.0.0.1:7432", "sablecourtd HTTP API base URL")
	rootCmd.PersistentFlags().StringVar(&socketPath, "socket", "", "Optional sablecourtd Unix domain socket path")
	rootCmd.PersistentFlags().StringVar(&secretFile, "secret-file", "", "Optional 0600 JSON secret file instead of OS Keychain")
	rootCmd.PersistentFlags().BoolVar(&jsonOut, "json", false, "Output raw JSON instead of styled tables")

	httpClient := func(timeout time.Duration) (*http.Client, string) {
		if socketPath != "" {
			return &http.Client{
				Timeout: timeout,
				Transport: &http.Transport{
					DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
						return (&net.Dialer{}).DialContext(ctx, "unix", socketPath)
					},
				},
			}, "http://unix"
		}
		return &http.Client{Timeout: timeout}, strings.TrimRight(apiURL, "/")
	}

	validateCmd := &cobra.Command{
		Use:   "validate <path>",
		Short: "Validate Git-tracked Sablecourt YAML manifests",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			bundle, err := config.LoadPath(args[0])
			if err != nil {
				return err
			}
			if jsonOut {
				return writeJSONOut(cmd.OutOrStdout(), map[string]any{
					"valid":     true,
					"workloads": len(bundle.Workloads),
					"policies":  len(bundle.Policies),
					"budgets":   len(bundle.Budgets),
					"workflows": len(bundle.Workflows),
					"digest":    bundle.ManifestDigest,
				})
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s workloads=%d policies=%d budgets=%d workflows=%d digest=%s\n",
				stJade.Render("✓ valid"),
				len(bundle.Workloads), len(bundle.Policies), len(bundle.Budgets), len(bundle.Workflows), bundle.ManifestDigest[:12])
			return nil
		},
	}

	workloadCmd := &cobra.Command{
		Use:   "workload",
		Short: "Inspect registered workloads",
	}
	workloadCmd.AddCommand(&cobra.Command{
		Use:   "list",
		Short: "List registered workloads from sablecourtd",
		RunE: func(cmd *cobra.Command, args []string) error {
			cli, base := httpClient(5 * time.Second)
			body, err := fetchGET(cli, base+"/api/v1/workloads")
			if err != nil {
				return err
			}
			if jsonOut {
				_, err = cmd.OutOrStdout().Write(body)
				return err
			}
			var items []domain.WorkloadRecord
			if err := json.Unmarshal(body, &items); err != nil {
				return err
			}
			renderWorkloads(cmd.OutOrStdout(), items)
			return nil
		},
	})

	taskCmd := &cobra.Command{
		Use:   "task",
		Short: "Submit and inspect tasks",
	}
	taskCmd.AddCommand(&cobra.Command{
		Use:   "list",
		Short: "List tasks from sablecourtd",
		RunE: func(cmd *cobra.Command, args []string) error {
			cli, base := httpClient(5 * time.Second)
			body, err := fetchGET(cli, base+"/api/v1/tasks")
			if err != nil {
				return err
			}
			if jsonOut {
				_, err = cmd.OutOrStdout().Write(body)
				return err
			}
			var items []domain.Task
			if err := json.Unmarshal(body, &items); err != nil {
				return err
			}
			renderTasks(cmd.OutOrStdout(), items)
			return nil
		},
	})

	var taskInput, taskIdem, taskProvider, taskModel string
	submitCmd := &cobra.Command{
		Use:   "submit <workload>",
		Short: "Submit a new task to sablecourtd",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			payload := make(map[string]any)
			if strings.TrimSpace(taskInput) != "" {
				if err := json.Unmarshal([]byte(taskInput), &payload); err != nil {
					payload = map[string]any{"prompt": taskInput}
				}
			}
			if taskProvider != "" {
				payload["provider"] = taskProvider
			}
			if taskModel != "" {
				payload["model"] = taskModel
			}
			rawInput, _ := json.Marshal(payload)
			cli, base := httpClient(10 * time.Second)
			body, err := fetchPOST(cli, base+"/api/v1/tasks", map[string]string{
				"workload":       args[0],
				"idempotencyKey": taskIdem,
				"inputJson":      string(rawInput),
			})
			if err != nil {
				return err
			}
			if jsonOut {
				_, err = cmd.OutOrStdout().Write(body)
				return err
			}
			var t domain.Task
			if err := json.Unmarshal(body, &t); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s submitted task %s for %s\n", stJade.Render("✓"), stTitle.Render(t.ID), t.WorkloadName)
			return nil
		},
	}
	submitCmd.Flags().StringVar(&taskInput, "input", "{}", "JSON task input or plain prompt text")
	submitCmd.Flags().StringVar(&taskIdem, "idempotency-key", "", "Optional idempotency key")
	submitCmd.Flags().StringVar(&taskProvider, "provider", "", "Optional LLM provider (openai, anthropic, gemini, local)")
	submitCmd.Flags().StringVar(&taskModel, "model", "", "Optional model ID")
	taskCmd.AddCommand(submitCmd)

	taskCmd.AddCommand(&cobra.Command{
		Use:   "inspect <task-id>",
		Short: "Inspect a task, its state transitions, and budget usage",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cli, base := httpClient(5 * time.Second)
			body, err := fetchGET(cli, base+"/api/v1/tasks/"+args[0])
			if err != nil {
				return err
			}
			if jsonOut {
				_, err = cmd.OutOrStdout().Write(body)
				return err
			}
			var resp struct {
				Task   domain.Task         `json:"task"`
				Events []domain.TaskEvent  `json:"events"`
				Budget domain.BudgetTotals `json:"budget"`
			}
			if err := json.Unmarshal(body, &resp); err != nil {
				return err
			}
			renderTaskDetail(cmd.OutOrStdout(), resp.Task, resp.Events, resp.Budget)
			return nil
		},
	})
	taskCmd.AddCommand(&cobra.Command{
		Use:   "cancel <task-id>",
		Short: "Cancel an active task and revoke its gate leases",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cli, base := httpClient(5 * time.Second)
			body, err := fetchPOST(cli, base+"/api/v1/tasks/"+args[0]+"/cancel", map[string]string{})
			if err != nil {
				return err
			}
			if jsonOut {
				_, err = cmd.OutOrStdout().Write(body)
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s cancelled task %s\n", stAmber.Render("✓"), args[0])
			return nil
		},
	})
	taskCmd.AddCommand(&cobra.Command{
		Use:   "suspend <task-id>",
		Short: "Checkpoint and suspend a working task",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cli, base := httpClient(10 * time.Second)
			body, err := fetchPOST(cli, base+"/api/v1/tasks/"+args[0]+"/suspend", map[string]string{})
			if err != nil {
				return err
			}
			if jsonOut {
				_, err = cmd.OutOrStdout().Write(body)
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s suspended task %s\n", stAmber.Render("⏸"), args[0])
			return nil
		},
	})
	taskCmd.AddCommand(&cobra.Command{
		Use:   "resume <task-id>",
		Short: "Restore checkpoint and resume a suspended task",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cli, base := httpClient(10 * time.Second)
			body, err := fetchPOST(cli, base+"/api/v1/tasks/"+args[0]+"/resume", map[string]string{})
			if err != nil {
				return err
			}
			if jsonOut {
				_, err = cmd.OutOrStdout().Write(body)
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s resumed task %s\n", stJade.Render("▶"), args[0])
			return nil
		},
	})

	budgetCmd := &cobra.Command{
		Use:   "budget",
		Short: "Inspect observed model budget usage",
	}
	budgetCmd.AddCommand(&cobra.Command{
		Use:   "status",
		Short: "Show aggregate token and micro-USD usage",
		RunE: func(cmd *cobra.Command, args []string) error {
			cli, base := httpClient(5 * time.Second)
			body, err := fetchGET(cli, base+"/api/v1/budget")
			if err != nil {
				return err
			}
			if jsonOut {
				_, err = cmd.OutOrStdout().Write(body)
				return err
			}
			var totals domain.BudgetTotals
			if err := json.Unmarshal(body, &totals); err != nil {
				return err
			}
			renderBudget(cmd.OutOrStdout(), totals)
			return nil
		},
	})

	applyCmd := &cobra.Command{
		Use:   "apply",
		Short: "Reload Git-tracked manifests in a running sablecourtd instance",
		RunE: func(cmd *cobra.Command, args []string) error {
			cli, base := httpClient(10 * time.Second)
			body, err := fetchPOST(cli, base+"/api/v1/reload", map[string]string{})
			if err != nil {
				return err
			}
			if jsonOut {
				_, err = cmd.OutOrStdout().Write(body)
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), stJade.Render("✓ manifests reloaded"))
			return nil
		},
	}

	auditCmd := &cobra.Command{
		Use:   "audit",
		Short: "Inspect audit log",
	}
	auditCmd.AddCommand(&cobra.Command{
		Use:   "tail",
		Short: "Show recent audit events from sablecourtd",
		RunE: func(cmd *cobra.Command, args []string) error {
			cli, base := httpClient(5 * time.Second)
			body, err := fetchGET(cli, base+"/api/v1/audit")
			if err != nil {
				return err
			}
			if jsonOut {
				_, err = cmd.OutOrStdout().Write(body)
				return err
			}
			var items []domain.AuditRecord
			if err := json.Unmarshal(body, &items); err != nil {
				return err
			}
			renderAudit(cmd.OutOrStdout(), items)
			return nil
		},
	})

	approvalCmd := &cobra.Command{
		Use:   "approval",
		Short: "Inspect and decide runtime approval requests",
	}
	approvalCmd.AddCommand(&cobra.Command{
		Use:   "list",
		Short: "List pending approvals from sablecourtd",
		RunE: func(cmd *cobra.Command, args []string) error {
			cli, base := httpClient(5 * time.Second)
			body, err := fetchGET(cli, base+"/api/v1/approvals?status=pending")
			if err != nil {
				return err
			}
			if jsonOut {
				_, err = cmd.OutOrStdout().Write(body)
				return err
			}
			var items []domain.Approval
			if err := json.Unmarshal(body, &items); err != nil {
				return err
			}
			renderApprovals(cmd.OutOrStdout(), items)
			return nil
		},
	})

	var approveDigest string
	approveCmd := &cobra.Command{
		Use:   "approve <approval-id>",
		Short: "Approve a pending request (triggers out-of-band confirmation on daemon)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cli, base := httpClient(65 * time.Second)
			body, err := fetchPOST(cli, base+"/api/v1/approvals/"+args[0]+"/approve", map[string]string{
				"expectedSha256": approveDigest,
			})
			if err != nil {
				return err
			}
			if jsonOut {
				_, err = cmd.OutOrStdout().Write(body)
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s approved %s\n", stJade.Render("✓"), args[0])
			return nil
		},
	}
	approveCmd.Flags().StringVar(&approveDigest, "digest", "", "Expected SHA-256 digest of the canonical payload")
	approvalCmd.AddCommand(approveCmd)

	var denyReason string
	denyCmd := &cobra.Command{
		Use:   "deny <approval-id>",
		Short: "Deny a pending approval request",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cli, base := httpClient(10 * time.Second)
			body, err := fetchPOST(cli, base+"/api/v1/approvals/"+args[0]+"/deny", map[string]string{
				"reason": denyReason,
			})
			if err != nil {
				return err
			}
			if jsonOut {
				_, err = cmd.OutOrStdout().Write(body)
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s denied %s\n", stRed.Render("✕"), args[0])
			return nil
		},
	}
	denyCmd.Flags().StringVar(&denyReason, "reason", "", "Reason for denial")
	approvalCmd.AddCommand(denyCmd)

	secretCmd := &cobra.Command{
		Use:   "secret",
		Short: "Manage upstream gate credentials in OS Keychain or 0600 file",
	}
	newSecretStore := func() secret.SecretStore {
		if secretFile != "" {
			return secret.NewFileStore(secretFile)
		}
		return secret.NewKeyringStore("sablecourt")
	}

	var secretValue string
	secretSetCmd := &cobra.Command{
		Use:   "set <name>",
		Short: "Store a secret (reads from stdin if --value is omitted)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			val := strings.TrimSpace(secretValue)
			if val == "" {
				fmt.Fprintf(cmd.ErrOrStderr(), "Enter value for %s: ", args[0])
				line, err := bufio.NewReader(cmd.InOrStdin()).ReadString('\n')
				if err != nil && !errors.Is(err, io.EOF) {
					return err
				}
				val = strings.TrimSpace(line)
			}
			if val == "" {
				return errors.New("secret value cannot be empty")
			}
			if err := newSecretStore().Set(context.Background(), args[0], val); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s stored secret %q\n", stJade.Render("✓"), args[0])
			return nil
		},
	}
	secretSetCmd.Flags().StringVar(&secretValue, "value", "", "Secret value (prefer stdin pipe over shell args)")

	secretCheckCmd := &cobra.Command{
		Use:   "check <name>",
		Short: "Check whether a secret exists without printing its value",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			_, err := newSecretStore().Get(context.Background(), args[0])
			if errors.Is(err, secret.ErrSecretNotFound) {
				fmt.Fprintf(cmd.OutOrStdout(), "%s %s\n", stMuted.Render("○ missing"), args[0])
				return nil
			}
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s %s\n", stJade.Render("● present"), args[0])
			return nil
		},
	}

	secretDeleteCmd := &cobra.Command{
		Use:   "delete <name>",
		Short: "Delete a stored secret",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := newSecretStore().Delete(context.Background(), args[0]); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s deleted secret %q\n", stAmber.Render("✓"), args[0])
			return nil
		},
	}
	secretCmd.AddCommand(secretSetCmd, secretCheckCmd, secretDeleteCmd)

	providerCmd := &cobra.Command{
		Use:   "provider",
		Short: "Inspect connected LLM providers and sync available models",
	}
	providerCmd.AddCommand(&cobra.Command{
		Use:   "list",
		Short: "List provider connection status and cached models",
		RunE: func(cmd *cobra.Command, args []string) error {
			cli, base := httpClient(5 * time.Second)
			body, err := fetchGET(cli, base+"/api/v1/providers")
			if err != nil {
				return err
			}
			if jsonOut {
				_, err = cmd.OutOrStdout().Write(body)
				return err
			}
			var items []providers.ProviderStatus
			if err := json.Unmarshal(body, &items); err != nil {
				return err
			}
			renderProviders(cmd.OutOrStdout(), items)
			return nil
		},
	})
	providerCmd.AddCommand(&cobra.Command{
		Use:   "sync <provider-id>",
		Short: "Query upstream /models endpoint and refresh cached model list",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cli, base := httpClient(15 * time.Second)
			body, err := fetchPOST(cli, base+"/api/v1/providers/"+args[0]+"/sync", map[string]string{})
			if err != nil {
				return err
			}
			if jsonOut {
				_, err = cmd.OutOrStdout().Write(body)
				return err
			}
			var models []providers.ModelInfo
			if err := json.Unmarshal(body, &models); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s synced %d models from %s\n", stJade.Render("✓"), len(models), args[0])
			return nil
		},
	})

	versionCmd := &cobra.Command{
		Use:   "version",
		Short: "Print sablectl version",
		Run: func(cmd *cobra.Command, args []string) {
			fmt.Fprintln(cmd.OutOrStdout(), stTitle.Render("sablectl")+" "+stJade.Render(version))
		},
	}

	promoteCmd := &cobra.Command{
		Use:   "promote <workload>",
		Short: "Create a GitOps promotion branch to graduate a sandbox workload to runtime",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cli, base := httpClient(10 * time.Second)
			body, err := fetchPOST(cli, base+"/api/v1/promote/"+args[0], map[string]string{})
			if err != nil {
				return err
			}
			if jsonOut {
				_, err = cmd.OutOrStdout().Write(body)
				return err
			}
			var res struct {
				Branch string `json:"branch"`
				Next   string `json:"next"`
			}
			_ = json.Unmarshal(body, &res)
			fmt.Fprintf(cmd.OutOrStdout(), "%s created promotion branch %s\n%s\n", stJade.Render("✓"), stTitle.Render(res.Branch), stMuted.Render(res.Next))
			return nil
		},
	}

	authCmd := &cobra.Command{
		Use:   "auth",
		Short: "Authenticate external providers via OAuth flows",
	}
	var ghClientID string
	authGitHubCmd := &cobra.Command{
		Use:   "github",
		Short: "Authenticate GitHub via OAuth Device Flow and store github_token in SecretStore",
		RunE: func(cmd *cobra.Command, args []string) error {
			if ghClientID == "" {
				ghClientID = os.Getenv("SABLECOURT_GITHUB_CLIENT_ID")
			}
			flow := providers.NewGitHubDeviceFlow(ghClientID)
			dc, err := flow.Start(context.Background(), "repo")
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Open %s and enter code: %s\nWaiting for authorization...\n",
				stJade.Render(dc.VerificationURI), stTitle.Render(dc.UserCode))
			tok, err := flow.PollToken(context.Background(), dc.DeviceCode, time.Duration(dc.Interval)*time.Second)
			if err != nil {
				return err
			}
			if err := newSecretStore().Set(context.Background(), "github_token", tok); err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), stJade.Render("✓ stored github_token in SecretStore"))
			return nil
		},
	}
	authGitHubCmd.Flags().StringVar(&ghClientID, "client-id", "", "GitHub OAuth App client_id (or set SABLECOURT_GITHUB_CLIENT_ID)")
	authCmd.AddCommand(authGitHubCmd)

	rootCmd.AddCommand(validateCmd, applyCmd, promoteCmd, authCmd, workloadCmd, taskCmd, budgetCmd, approvalCmd, auditCmd, providerCmd, secretCmd, versionCmd)

	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, stRed.Render("error: ")+err.Error())
		os.Exit(1)
	}
}

func fetchGET(client *http.Client, url string) ([]byte, error) {
	resp, err := client.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return body, nil
}

func fetchPOST(client *http.Client, url string, payload any) ([]byte, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	resp, err := client.Post(url, "application/json", bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return body, nil
}

func writeJSONOut(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}
