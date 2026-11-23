package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/shadewright-labs/sablecourt/internal/config"
)

const version = "0.1.0-dev"

func main() {
	var apiURL string

	rootCmd := &cobra.Command{
		Use:   "sablectl",
		Short: "CLI client for the Sablecourt control plane",
	}
	rootCmd.PersistentFlags().StringVar(&apiURL, "api", "http://127.0.0.1:7432", "sablecourtd HTTP API base URL")

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

	versionCmd := &cobra.Command{
		Use:   "version",
		Short: "Print sablectl version",
		Run: func(cmd *cobra.Command, args []string) {
			fmt.Fprintln(cmd.OutOrStdout(), version)
		},
	}

	rootCmd.AddCommand(validateCmd, workloadCmd, taskCmd, approvalCmd, auditCmd, versionCmd)

	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func printGET(out io.Writer, url string) error {
	client := &http.Client{Timeout: 5 * time.Second}
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
	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	client := &http.Client{Timeout: 65 * time.Second}
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
