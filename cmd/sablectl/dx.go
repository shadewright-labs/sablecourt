package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/shadewright-labs/sablecourt/internal/domain"
	"github.com/shadewright-labs/sablecourt/internal/providers"
	scruntime "github.com/shadewright-labs/sablecourt/internal/runtime"
)

func buildDXCommands(httpClient func(time.Duration) (*http.Client, string), jsonOut *bool) (*cobra.Command, *cobra.Command, *cobra.Command, *cobra.Command) {
	doctorCmd := &cobra.Command{
		Use:   "doctor",
		Short: "Check daemon health, audit chain, runtimes, coding harnesses, providers, and voice backends",
		RunE: func(cmd *cobra.Command, args []string) error {
			out := cmd.OutOrStdout()
			cli, base := httpClient(4 * time.Second)
			okMark := stJade.Render("✓")
			warnMark := stAmber.Render("!")
			errMark := stRed.Render("✕")

			fmt.Fprintln(out, stTitle.Render("Sablecourt doctor"))
			if _, err := fetchGET(cli, base+"/healthz"); err != nil {
				fmt.Fprintf(out, "  %s sablecourtd (%s): unreachable (%v)\n", errMark, base, err)
			} else {
				fmt.Fprintf(out, "  %s sablecourtd (%s): healthy\n", okMark, base)
				// The daemon answers a broken chain with HTTP 409 and the verify result as the body.
				body, err := fetchGET(cli, base+"/api/v1/audit/verify")
				var statusErr *httpStatusError
				if errors.As(err, &statusErr) && statusErr.StatusCode == http.StatusConflict {
					body, err = []byte(statusErr.Body), nil
				}
				if err == nil {
					var res struct {
						Valid        bool `json:"valid"`
						VerifiedRows int  `json:"verifiedRows"`
					}
					_ = json.Unmarshal(body, &res)
					if res.Valid {
						fmt.Fprintf(out, "  %s audit SHA-256 chain: valid (%d rows)\n", okMark, res.VerifiedRows)
					} else {
						fmt.Fprintf(out, "  %s audit SHA-256 chain: BROKEN\n", errMark)
					}
				}
				if body, err := fetchGET(cli, base+"/api/v1/providers"); err == nil {
					var provs []providers.ProviderStatus
					_ = json.Unmarshal(body, &provs)
					var connected []string
					for _, p := range provs {
						if p.Connected {
							connected = append(connected, p.ID)
						}
					}
					if len(connected) > 0 {
						fmt.Fprintf(out, "  %s connected providers: %s\n", okMark, strings.Join(connected, ", "))
					} else {
						fmt.Fprintf(out, "  %s connected providers: none configured\n", warnMark)
					}
				}
				if body, err := fetchGET(cli, base+"/api/v1/voice/status"); err == nil {
					var vs struct {
						STTURL     string `json:"sttUrl"`
						TTSBackend string `json:"ttsBackend"`
					}
					_ = json.Unmarshal(body, &vs)
					fmt.Fprintf(out, "  %s voice: stt=%s tts=%s\n", okMark, vs.STTURL, vs.TTSBackend)
				}
			}

			for _, bin := range []string{"docker", "kubectl", "ollama", "claude", "codex", "agy", "whisper-server"} {
				if p, err := exec.LookPath(bin); err == nil {
					fmt.Fprintf(out, "  %s tool %-14s %s\n", okMark, bin, stMuted.Render(p))
				} else {
					fmt.Fprintf(out, "  %s tool %-14s not on PATH\n", warnMark, bin)
				}
			}
			return nil
		},
	}

	var initOut string
	var initForce bool
	initCmd := &cobra.Command{
		Use:   "init",
		Short: "Generate a starter sablecourt.yaml manifest tailored to the current repository",
		RunE: func(cmd *cobra.Command, args []string) error {
			if !initForce {
				if _, err := os.Stat(initOut); err == nil {
					return fmt.Errorf("%s already exists (pass --force to overwrite)", initOut)
				}
			}
			cwd, err := os.Getwd()
			if err != nil {
				return err
			}
			harness := "claude"
			for _, h := range []string{"claude", "codex", "agy"} {
				if _, err := exec.LookPath(h); err == nil {
					harness = h
					break
				}
			}
			repoName := filepath.Base(cwd)
			content := fmt.Sprintf(`apiVersion: sablecourt.io/v1alpha1
kind: Budget
metadata:
  name: %s-daily
spec:
  perTask:
    maxInputTokens: 200000
    maxOutputTokens: 40000
    maxCostUSD: 2.50
  daily:
    maxCostUSD: 15.00
---
apiVersion: sablecourt.io/v1alpha1
kind: Policy
metadata:
  name: %s-policy
spec:
  defaultEffect: deny
  rules:
    - workload: coder
      capability: mcp:memory.*
      effect: allow
    - workload: coder
      capability: mcp:conclave.*
      effect: allow
    - workload: reviewer
      capability: mcp:conclave.*
      effect: allow
    - workload: "*"
      capability: policy:*
      effect: deny
---
apiVersion: sablecourt.io/v1alpha1
kind: Workload
metadata:
  name: coder
  zone: runtime
spec:
  class: coding-agent
  runtime:
    type: worktree-cli
  coding:
    harness: %s
    repoPath: %s
    sandbox: seatbelt
  agentCard:
    description: Edits code in an isolated Git worktree for %s.
    skills: [coding, tests]
  grants:
    modelGate:
      allowed: true
      providers: [anthropic, openai, gemini, local]
      budgetRef: %s-daily
    mcpTools: [memory.get, memory.put, memory.search, conclave.post, conclave.read, conclave.relic_put, conclave.relic_get]
---
apiVersion: sablecourt.io/v1alpha1
kind: Workload
metadata:
  name: reviewer
  zone: runtime
spec:
  class: long-lived-agent
  runtime:
    type: docker
    image: sablecourt-agent:0.1.0
    readOnlyRootfs: true
  agentCard:
    description: Reviews diffs and Relics inside Conclaves.
    skills: [code-review]
  grants:
    modelGate:
      allowed: true
      providers: [anthropic, openai, gemini, local]
      budgetRef: %s-daily
    mcpTools: [memory.get, memory.search, conclave.post, conclave.read, conclave.relic_get]
---
apiVersion: sablecourt.io/v1alpha1
kind: Conclave
metadata:
  name: %s-conclave
spec:
  description: Multi-agent Conclave for %s.
  budgetRef: %s-daily
  wards:
    maxMessages: 40
    maxPingPong: 4
  vassals:
    - role: coder
      workload: coder
      activation: always-on
      canMessage: [reviewer, operator]
    - role: reviewer
      workload: reviewer
      activation: on-mention
      canMessage: [coder, operator]
`, repoName, repoName, harness, cwd, repoName, repoName, repoName, repoName, repoName, repoName)

			if err := os.WriteFile(initOut, []byte(content), 0o600); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s wrote %s (harness=%s, repo=%s)\n", stJade.Render("✓"), initOut, harness, cwd)
			return nil
		},
	}
	initCmd.Flags().StringVar(&initOut, "out", "sablecourt.yaml", "Output manifest file path")
	initCmd.Flags().BoolVar(&initForce, "force", false, "Overwrite existing manifest file")

	taskDiffCmd := &cobra.Command{
		Use:   "diff <task-id>",
		Short: "Print the colorized git-diff artifact recorded for a coding task",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cli, base := httpClient(5 * time.Second)
			body, err := fetchGET(cli, base+"/api/v1/tasks/"+url.PathEscape(args[0]))
			if err != nil {
				return err
			}
			var resp struct {
				Task domain.Task `json:"task"`
			}
			if err := json.Unmarshal(body, &resp); err != nil {
				return err
			}
			var arts []scruntime.Artifact
			_ = json.Unmarshal([]byte(resp.Task.ArtifactsJSON), &arts)
			for _, a := range arts {
				if a.Name == "git-diff" {
					if *jsonOut {
						return writeJSONOut(cmd.OutOrStdout(), a)
					}
					printColorDiff(cmd.OutOrStdout(), a.Text)
					return nil
				}
			}
			fmt.Fprintln(cmd.OutOrStdout(), stMuted.Render("No git-diff artifact recorded for task "+args[0]))
			return nil
		},
	}

	memoryCmd := &cobra.Command{
		Use:   "memory",
		Short: "Manage persistent Court Memory shared across tasks and Conclaves",
	}
	var memNS, memQuery string
	memListCmd := &cobra.Command{
		Use:   "list",
		Short: "List or search Court Memory entries",
		RunE: func(cmd *cobra.Command, args []string) error {
			cli, base := httpClient(5 * time.Second)
			u := base + "/api/v1/memory?namespace=" + url.QueryEscape(memNS) + "&q=" + url.QueryEscape(memQuery)
			body, err := fetchGET(cli, u)
			if err != nil {
				return err
			}
			if *jsonOut {
				_, err = cmd.OutOrStdout().Write(body)
				return err
			}
			var items []domain.MemoryEntry
			if err := json.Unmarshal(body, &items); err != nil {
				return err
			}
			for _, m := range items {
				fmt.Fprintf(cmd.OutOrStdout(), "%s/%s  %s\n", stMuted.Render(m.Namespace), stTitle.Render(m.Key), m.Value)
			}
			return nil
		},
	}
	memListCmd.Flags().StringVar(&memNS, "namespace", "", "Optional memory namespace filter")
	memListCmd.Flags().StringVar(&memQuery, "q", "", "Optional substring search")

	memSetCmd := &cobra.Command{
		Use:   "set <namespace> <key> <value>",
		Short: "Store a redacted Court Memory entry",
		Args:  cobra.ExactArgs(3),
		RunE: func(cmd *cobra.Command, args []string) error {
			cli, base := httpClient(5 * time.Second)
			body, err := fetchPOST(cli, base+"/api/v1/memory", map[string]string{
				"namespace": args[0],
				"key":       args[1],
				"value":     args[2],
			})
			if err != nil {
				return err
			}
			if *jsonOut {
				_, err = cmd.OutOrStdout().Write(body)
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s saved memory %s/%s\n", stJade.Render("✓"), args[0], args[1])
			return nil
		},
	}

	memDelCmd := &cobra.Command{
		Use:   "delete <namespace> <key>",
		Short: "Delete a Court Memory entry",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			cli, base := httpClient(5 * time.Second)
			req, err := http.NewRequest(http.MethodDelete, base+"/api/v1/memory/"+url.PathEscape(args[0])+"/"+url.PathEscape(args[1]), nil)
			if err != nil {
				return err
			}
			resp, err := cli.Do(req)
			if err != nil {
				return err
			}
			defer resp.Body.Close()
			if resp.StatusCode >= 400 {
				b, _ := io.ReadAll(resp.Body)
				return fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s deleted memory %s/%s\n", stAmber.Render("✓"), args[0], args[1])
			return nil
		},
	}
	memoryCmd.AddCommand(memListCmd, memSetCmd, memDelCmd)

	return doctorCmd, initCmd, taskDiffCmd, memoryCmd
}

func printColorDiff(w io.Writer, diff string) {
	for _, line := range strings.Split(diff, "\n") {
		switch {
		case strings.HasPrefix(line, "+++") || strings.HasPrefix(line, "---") || strings.HasPrefix(line, "@@"):
			fmt.Fprintln(w, stAmber.Render(line))
		case strings.HasPrefix(line, "+"):
			fmt.Fprintln(w, stJade.Render(line))
		case strings.HasPrefix(line, "-"):
			fmt.Fprintln(w, stRed.Render(line))
		default:
			fmt.Fprintln(w, line)
		}
	}
}
