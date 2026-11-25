package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/shadewright-labs/sablecourt/internal/gatehttp"
)

// demo-agent is a minimal workload demonstrating Sablecourt's Three Doors gates:
// it holds zero raw API keys and communicates exclusively via SABLECOURT_TASK_TOKEN.
func main() {
	taskID := os.Getenv("SABLECOURT_TASK_ID")
	workload := os.Getenv("SABLECOURT_WORKLOAD")
	token := os.Getenv("SABLECOURT_TASK_TOKEN")
	actionGateBase := strings.TrimSuffix(os.Getenv("SABLECOURT_DATA_GATE_URL"), "/mcp") + "/actions"

	fmt.Printf("demo-agent starting: workload=%s task=%s\n", workload, taskID)
	if token == "" {
		fmt.Fprintln(os.Stderr, "SABLECOURT_TASK_TOKEN is not set")
		os.Exit(1)
	}

	payload := map[string]any{
		"payload": map[string]any{
			"owner": "shadewright-labs",
			"repo":  "sablecourt",
			"head":  "sablecourt/" + taskID,
			"base":  "main",
			"title": "Demo proposal from " + workload,
			"body":  "Submitted via Sablecourt Action Gate",
		},
	}

	code, body, err := postJSON(actionGateBase+"/git.open_pr", token, payload)
	if err != nil {
		fmt.Fprintf(os.Stderr, "action call failed: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("initial git.open_pr -> HTTP %d: %s\n", code, strings.TrimSpace(string(body)))

	if code == http.StatusAccepted {
		var pending struct {
			ApprovalID    string `json:"approval_id"`
			PayloadSHA256 string `json:"payload_sha256"`
		}
		_ = json.Unmarshal(body, &pending)
		fmt.Printf("waiting for human approval: id=%s sha256=%s\n", pending.ApprovalID, pending.PayloadSHA256)

		// Poll up to 60s for operator approval, then replay with exact pinned payload.
		payload["approvalId"] = pending.ApprovalID
		deadline := time.Now().Add(60 * time.Second)
		for time.Now().Before(deadline) {
			time.Sleep(3 * time.Second)
			retryCode, retryBody, err := postJSON(actionGateBase+"/git.open_pr", token, payload)
			if err != nil {
				continue
			}
			if retryCode == http.StatusOK {
				fmt.Printf("approved and executed -> HTTP 200: %s\n", strings.TrimSpace(string(retryBody)))
				return
			}
		}
		fmt.Println("approval window elapsed; exiting cleanly")
	}
}

func postJSON(url, token string, v any) (int, []byte, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return 0, nil, err
	}
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(raw)) // #nosec G704 -- main appends the fixed action path to the data gate URL injected by runDaemon.
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	client := gatehttp.NewClient(nil, 10*time.Second)
	resp, err := client.Do(req) // #nosec G704 -- main supplies the injected data gate host and gatehttp.NewClient refuses redirects.
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	return resp.StatusCode, body, err
}
