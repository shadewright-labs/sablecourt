package main

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func runMCPServer(ctx context.Context, clientFactory func(time.Duration) (*http.Client, string)) error {
	srv := mcp.NewServer(&mcp.Implementation{
		Name:    "sablecourt",
		Version: version,
	}, nil)

	addJSONTool := func(name, desc string, schema string, fn func(ctx context.Context, rawArgs json.RawMessage) ([]byte, error)) {
		srv.AddTool(&mcp.Tool{
			Name:        name,
			Description: desc,
			InputSchema: json.RawMessage(schema),
		}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			out, err := fn(ctx, req.Params.Arguments)
			if err != nil {
				return &mcp.CallToolResult{
					IsError: true,
					Content: []mcp.Content{&mcp.TextContent{Text: err.Error()}},
				}, nil
			}
			return &mcp.CallToolResult{
				Content: []mcp.Content{&mcp.TextContent{Text: string(out)}},
			}, nil
		})
	}

	addJSONTool(
		"sablecourt_list_workloads",
		"List declared workloads registered in Sablecourt.",
		`{"type":"object","properties":{}}`,
		func(_ context.Context, _ json.RawMessage) ([]byte, error) {
			cli, base := clientFactory(5 * time.Second)
			return fetchGET(cli, base+"/api/v1/workloads")
		},
	)

	addJSONTool(
		"sablecourt_submit_task",
		"Submit a new task to a Sablecourt workload.",
		`{"type":"object","properties":{"workload":{"type":"string"},"prompt":{"type":"string"},"provider":{"type":"string"},"model":{"type":"string"}},"required":["workload","prompt"]}`,
		func(_ context.Context, raw json.RawMessage) ([]byte, error) {
			var in struct {
				Workload string `json:"workload"`
				Prompt   string `json:"prompt"`
				Provider string `json:"provider"`
				Model    string `json:"model"`
			}
			if err := json.Unmarshal(raw, &in); err != nil {
				return nil, err
			}
			payload := map[string]any{"prompt": in.Prompt}
			if in.Provider != "" {
				payload["provider"] = in.Provider
			}
			if in.Model != "" {
				payload["model"] = in.Model
			}
			inBytes, _ := json.Marshal(payload)
			cli, base := clientFactory(10 * time.Second)
			return fetchPOST(cli, base+"/api/v1/tasks", map[string]string{
				"workload":  in.Workload,
				"inputJson": string(inBytes),
			})
		},
	)

	addJSONTool(
		"sablecourt_inspect_task",
		"Inspect a task, its state transitions, and observed budget usage.",
		`{"type":"object","properties":{"task_id":{"type":"string"}},"required":["task_id"]}`,
		func(_ context.Context, raw json.RawMessage) ([]byte, error) {
			var in struct {
				TaskID string `json:"task_id"`
			}
			if err := json.Unmarshal(raw, &in); err != nil {
				return nil, err
			}
			cli, base := clientFactory(5 * time.Second)
			return fetchGET(cli, base+"/api/v1/tasks/"+in.TaskID)
		},
	)

	addJSONTool(
		"sablecourt_open_conclave",
		"Convene a multi-agent Conclave from a manifest template with an optional opening operator Decree.",
		`{"type":"object","properties":{"template":{"type":"string"},"title":{"type":"string"},"decree":{"type":"string"}},"required":["template","title"]}`,
		func(_ context.Context, raw json.RawMessage) ([]byte, error) {
			var in struct {
				Template string `json:"template"`
				Title    string `json:"title"`
				Decree   string `json:"decree"`
			}
			if err := json.Unmarshal(raw, &in); err != nil {
				return nil, err
			}
			cli, base := clientFactory(10 * time.Second)
			return fetchPOST(cli, base+"/api/v1/conclaves", map[string]string{
				"template": in.Template,
				"title":    in.Title,
				"decree":   in.Decree,
			})
		},
	)

	addJSONTool(
		"sablecourt_conclave_decree",
		"Issue an operator Decree to a Conclave (directs Vassals or resumes a Ward-paused Conclave).",
		`{"type":"object","properties":{"conclave_id":{"type":"string"},"to":{"type":"string"},"body":{"type":"string"}},"required":["conclave_id","body"]}`,
		func(_ context.Context, raw json.RawMessage) ([]byte, error) {
			var in struct {
				ConclaveID string `json:"conclave_id"`
				To         string `json:"to"`
				Body       string `json:"body"`
			}
			if err := json.Unmarshal(raw, &in); err != nil {
				return nil, err
			}
			if in.To == "" {
				in.To = "all"
			}
			cli, base := clientFactory(10 * time.Second)
			return fetchPOST(cli, base+"/api/v1/conclaves/"+in.ConclaveID+"/decree", map[string]string{
				"to":   in.To,
				"body": in.Body,
			})
		},
	)

	addJSONTool(
		"sablecourt_inspect_conclave",
		"Read a Conclave session, Vassal message log, and shared Relics.",
		`{"type":"object","properties":{"conclave_id":{"type":"string"}},"required":["conclave_id"]}`,
		func(_ context.Context, raw json.RawMessage) ([]byte, error) {
			var in struct {
				ConclaveID string `json:"conclave_id"`
			}
			if err := json.Unmarshal(raw, &in); err != nil {
				return nil, err
			}
			cli, base := clientFactory(5 * time.Second)
			return fetchGET(cli, base+"/api/v1/conclaves/"+in.ConclaveID)
		},
	)

	addJSONTool(
		"sablecourt_budget_status",
		"Inspect observed Model Gate token and USD spend.",
		`{"type":"object","properties":{}}`,
		func(_ context.Context, _ json.RawMessage) ([]byte, error) {
			cli, base := clientFactory(5 * time.Second)
			return fetchGET(cli, base+"/api/v1/budget")
		},
	)

	return srv.Run(ctx, &mcp.StdioTransport{})
}
