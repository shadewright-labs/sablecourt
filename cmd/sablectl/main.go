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
	"github.com/shadewright-labs/sablecourt/internal/secret"
)

const version = "0.1.0-dev"

func main() {
	var (
		apiURL     string
		socketPath string
		secretFile string
	)

	rootCmd := &cobra.Command{
		Use:   "sablectl",
		Short: "CLI client for the Sablecourt control plane",
	}
	rootCmd.PersistentFlags().StringVar(&apiURL, "api", "http://127.0.0.1:7432", "sablecourtd HTTP API base URL")
	rootCmd.PersistentFlags().StringVar(&socketPath, "socket", "", "Optional sablecourtd Unix domain socket path")
	rootCmd.PersistentFlags().StringVar(&secretFile, "secret-file", "", "Optional 0600 JSON secret file instead of OS Keychain")

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
			fmt.Fprintf(cmd.OutOrStdout(), "valid: workloads=%d policies=%d budgets=%d digest=%s\n",
				len(bundle.Workloads), len(bundle.Policies), len(bundle.Budgets), bundle.ManifestDigest[:12])
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
			return printGET(cmd.OutOrStdout(), apiURL+"/api/v1/workloads")
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
			return printGET(cmd.OutOrStdout(), apiURL+"/api/v1/tasks")
		},
	})
	var taskInput, taskIdem string
	submitCmd := &cobra.Command{
		Use:   "submit <workload>",
		Short: "Submit a new task to sablecourtd",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return printPOST(cmd.OutOrStdout(), apiURL+"/api/v1/tasks", map[string]string{
				"workload":       args[0],
				"idempotencyKey": taskIdem,
				"inputJson":      taskInput,
			})
		},
	}
	submitCmd.Flags().StringVar(&taskInput, "input", "{}", "JSON task input")
	submitCmd.Flags().StringVar(&taskIdem, "idempotency-key", "", "Optional idempotency key")
	taskCmd.AddCommand(submitCmd)
	taskCmd.AddCommand(&cobra.Command{
		Use:   "inspect <task-id>",
		Short: "Inspect a task, its state transitions, and budget usage",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cli, base := httpClient(5 * time.Second)
			return doGET(cli, cmd.OutOrStdout(), base+"/api/v1/tasks/"+args[0])
		},
	})
	taskCmd.AddCommand(&cobra.Command{
		Use:   "cancel <task-id>",
		Short: "Cancel an active task and revoke its gate leases",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cli, base := httpClient(5 * time.Second)
			return doPOST(cli, cmd.OutOrStdout(), base+"/api/v1/tasks/"+args[0]+"/cancel", map[string]string{})
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
			return doGET(cli, cmd.OutOrStdout(), base+"/api/v1/budget")
		},
	})

	applyCmd := &cobra.Command{
		Use:   "apply",
		Short: "Reload Git-tracked manifests in a running sablecourtd instance",
		RunE: func(cmd *cobra.Command, args []string) error {
			cli, base := httpClient(10 * time.Second)
			return doPOST(cli, cmd.OutOrStdout(), base+"/api/v1/reload", map[string]string{})
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
			return printGET(cmd.OutOrStdout(), apiURL+"/api/v1/audit")
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
			return printGET(cmd.OutOrStdout(), apiURL+"/api/v1/approvals?status=pending")
		},
	})

	var approveDigest string
	approveCmd := &cobra.Command{
		Use:   "approve <approval-id>",
		Short: "Approve a pending request (triggers out-of-band confirmation on daemon)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return printPOST(cmd.OutOrStdout(), apiURL+"/api/v1/approvals/"+args[0]+"/approve", map[string]string{
				"expectedSha256": approveDigest,
			})
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
			return printPOST(cmd.OutOrStdout(), apiURL+"/api/v1/approvals/"+args[0]+"/deny", map[string]string{
				"reason": denyReason,
			})
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
			fmt.Fprintf(cmd.OutOrStdout(), "stored secret %q\n", args[0])
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
				fmt.Fprintf(cmd.OutOrStdout(), "%s: missing\n", args[0])
				return nil
			}
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s: present\n", args[0])
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
			fmt.Fprintf(cmd.OutOrStdout(), "deleted secret %q\n", args[0])
			return nil
		},
	}
	secretCmd.AddCommand(secretSetCmd, secretCheckCmd, secretDeleteCmd)

	versionCmd := &cobra.Command{
		Use:   "version",
		Short: "Print sablectl version",
		Run: func(cmd *cobra.Command, args []string) {
			fmt.Fprintln(cmd.OutOrStdout(), version)
		},
	}

	rootCmd.AddCommand(validateCmd, applyCmd, workloadCmd, taskCmd, budgetCmd, approvalCmd, auditCmd, secretCmd, versionCmd)

	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func printGET(out io.Writer, url string) error {
	return doGET(&http.Client{Timeout: 5 * time.Second}, out, url)
}

func doGET(client *http.Client, out io.Writer, url string) error {
	resp, err := client.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode >= 400 {
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(body))
	}
	_, err = out.Write(body)
	return err
}

func printPOST(out io.Writer, url string, payload any) error {
	return doPOST(&http.Client{Timeout: 65 * time.Second}, out, url, payload)
}

func doPOST(client *http.Client, out io.Writer, url string, payload any) error {
	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	resp, err := client.Post(url, "application/json", bytes.NewReader(raw))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode >= 400 {
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(body))
	}
	_, err = out.Write(body)
	return err
}
