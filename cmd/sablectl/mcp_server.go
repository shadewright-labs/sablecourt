package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/shadewright-labs/sablecourt/internal/domain"
)

const maxMCPArgsBytes = 256 << 10

func runMCPServer(ctx context.Context, clientFactory func(time.Duration) (*http.Client, string)) error {
	srv := newMCPServer(clientFactory)
	return srv.Run(ctx, &mcp.StdioTransport{})
}

func newMCPServer(clientFactory func(time.Duration) (*http.Client, string)) *mcp.Server {
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
		func(ctx context.Context, _ json.RawMessage) ([]byte, error) {
			cli, base := clientFactory(5 * time.Second)
			return fetchGETContext(ctx, cli, base+"/api/v1/workloads")
		},
	)

	addJSONTool(
		"sablecourt_submit_task",
		"Submit a new task to a Sablecourt workload.",
		`{"type":"object","properties":{"workload":{"type":"string"},"prompt":{"type":"string"},"provider":{"type":"string"},"model":{"type":"string"}},"required":["workload","prompt"]}`,
		func(ctx context.Context, raw json.RawMessage) ([]byte, error) {
			var in struct {
				Workload string `json:"workload"`
				Prompt   string `json:"prompt"`
				Provider string `json:"provider"`
				Model    string `json:"model"`
			}
			if err := decodeMCPArgs(raw, &in); err != nil {
				return nil, err
			}
			if strings.TrimSpace(in.Workload) == "" {
				return nil, errors.New("workload is required")
			}
			if strings.TrimSpace(in.Prompt) == "" {
				return nil, errors.New("prompt is required")
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
			return fetchPOSTContext(ctx, cli, base+"/api/v1/tasks", map[string]string{
				"workload":  strings.TrimSpace(in.Workload),
				"inputJson": string(inBytes),
			})
		},
	)

	addJSONTool(
		"sablecourt_inspect_task",
		"Inspect a task, its state transitions, and observed budget usage.",
		`{"type":"object","properties":{"task_id":{"type":"string"}},"required":["task_id"]}`,
		func(ctx context.Context, raw json.RawMessage) ([]byte, error) {
			var in struct {
				TaskID string `json:"task_id"`
			}
			if err := decodeMCPArgs(raw, &in); err != nil {
				return nil, err
			}
			escapedID, err := escapeMCPPathID(in.TaskID, "task_id")
			if err != nil {
				return nil, err
			}
			cli, base := clientFactory(5 * time.Second)
			return fetchGETContext(ctx, cli, base+"/api/v1/tasks/"+escapedID)
		},
	)

	addJSONTool(
		"sablecourt_open_conclave",
		"Convene a multi-agent Conclave from a manifest template with an optional opening operator Decree.",
		`{"type":"object","properties":{"template":{"type":"string"},"title":{"type":"string"},"decree":{"type":"string"}},"required":["template","title"]}`,
		func(ctx context.Context, raw json.RawMessage) ([]byte, error) {
			var in struct {
				Template string `json:"template"`
				Title    string `json:"title"`
				Decree   string `json:"decree"`
			}
			if err := decodeMCPArgs(raw, &in); err != nil {
				return nil, err
			}
			if strings.TrimSpace(in.Template) == "" {
				return nil, errors.New("template is required")
			}
			if strings.TrimSpace(in.Title) == "" {
				return nil, errors.New("title is required")
			}
			cli, base := clientFactory(10 * time.Second)
			return fetchPOSTContext(ctx, cli, base+"/api/v1/conclaves", map[string]string{
				"template": strings.TrimSpace(in.Template),
				"title":    strings.TrimSpace(in.Title),
				"decree":   in.Decree,
			})
		},
	)

	addJSONTool(
		"sablecourt_conclave_decree",
		"Issue an operator Decree to a Conclave (directs Vassals or resumes a Ward-paused Conclave).",
		`{"type":"object","properties":{"conclave_id":{"type":"string"},"to":{"type":"string"},"body":{"type":"string"}},"required":["conclave_id","body"]}`,
		func(ctx context.Context, raw json.RawMessage) ([]byte, error) {
			var in struct {
				ConclaveID string `json:"conclave_id"`
				To         string `json:"to"`
				Body       string `json:"body"`
			}
			if err := decodeMCPArgs(raw, &in); err != nil {
				return nil, err
			}
			escapedID, err := escapeMCPPathID(in.ConclaveID, "conclave_id")
			if err != nil {
				return nil, err
			}
			body := strings.TrimSpace(in.Body)
			if body == "" {
				return nil, errors.New("body is required")
			}
			if len(body) > maxCouncilQuestionBytes {
				return nil, errors.New("body exceeds 64 KiB")
			}
			to := strings.TrimSpace(in.To)
			if to == "" {
				to = "all"
			}
			cli, base := clientFactory(10 * time.Second)
			return fetchPOSTContext(ctx, cli, base+"/api/v1/conclaves/"+escapedID+"/decree", map[string]string{
				"to":   to,
				"body": body,
			})
		},
	)

	addJSONTool(
		"sablecourt_inspect_conclave",
		"Read a Conclave session, Vassal message log, and shared Relics.",
		`{"type":"object","properties":{"conclave_id":{"type":"string"}},"required":["conclave_id"]}`,
		func(ctx context.Context, raw json.RawMessage) ([]byte, error) {
			var in struct {
				ConclaveID string `json:"conclave_id"`
			}
			if err := decodeMCPArgs(raw, &in); err != nil {
				return nil, err
			}
			escapedID, err := escapeMCPPathID(in.ConclaveID, "conclave_id")
			if err != nil {
				return nil, err
			}
			cli, base := clientFactory(5 * time.Second)
			return fetchGETContext(ctx, cli, base+"/api/v1/conclaves/"+escapedID)
		},
	)

	addJSONTool(
		"sablecourt_budget_status",
		"Inspect observed Model Gate token and USD spend.",
		`{"type":"object","properties":{}}`,
		func(ctx context.Context, _ json.RawMessage) ([]byte, error) {
			cli, base := clientFactory(5 * time.Second)
			return fetchGETContext(ctx, cli, base+"/api/v1/budget")
		},
	)

	addJSONTool(
		"sablecourt_council_convene",
		"Convene a bounded multi-agent council.",
		`{"type":"object","properties":{"question":{"type":"string"},"repo":{"type":"string"},"ref":{"type":"string"},"live":{"type":"boolean"},"rounds":{"type":"integer"},"with":{"type":"array","items":{"type":"string"}},"sequential":{"type":"boolean"}},"required":["question"]}`,
		func(ctx context.Context, raw json.RawMessage) ([]byte, error) {
			var in struct {
				Question   string   `json:"question"`
				Repo       string   `json:"repo"`
				Ref        *string  `json:"ref"`
				Live       bool     `json:"live"`
				Rounds     *int     `json:"rounds"`
				With       []string `json:"with"`
				Sequential bool     `json:"sequential"`
			}
			if err := decodeMCPArgs(raw, &in); err != nil {
				return nil, err
			}
			question, err := validateCouncilQuestion(in.Question)
			if err != nil {
				return nil, err
			}
			refSet := in.Ref != nil
			rawRef := ""
			if refSet {
				rawRef = *in.Ref
			}
			ref, err := validateCheckoutMode(refSet, rawRef, in.Live)
			if err != nil {
				return nil, err
			}
			rounds := defaultCouncilRounds
			if in.Rounds != nil {
				rounds = *in.Rounds
			}
			if rounds <= 0 {
				return nil, errors.New("rounds must be a positive integer")
			}
			harnesses := append([]string(nil), defaultCouncilHarnesses...)
			if in.With != nil {
				harnesses, err = validateCouncilHarnessSlice(in.With)
				if err != nil {
					return nil, err
				}
			}
			repo := strings.TrimSpace(in.Repo)
			if repo != "" && hasControlChars(repo) {
				return nil, errors.New("invalid repo path")
			}
			payload := buildCouncilOpenPayload(question, repo, ref, in.Live, rounds, harnesses, in.Sequential)
			cli, base := clientFactory(15 * time.Second)
			return postCouncilOpen(ctx, cli, base, payload)
		},
	)

	addJSONTool(
		"sablecourt_council_read",
		"Read a council session, participant messages, and Relics (or a named Relic) as readable text.",
		`{"type":"object","properties":{"conclave_id":{"type":"string"},"since_id":{"type":"integer"},"relic":{"type":"string"}},"required":["conclave_id"]}`,
		func(ctx context.Context, raw json.RawMessage) ([]byte, error) {
			var in struct {
				ConclaveID string `json:"conclave_id"`
				SinceID    int64  `json:"since_id"`
				Relic      string `json:"relic"`
			}
			if err := decodeMCPArgs(raw, &in); err != nil {
				return nil, err
			}
			escapedID, err := escapeMCPPathID(in.ConclaveID, "conclave_id")
			if err != nil {
				return nil, err
			}
			if in.SinceID < 0 {
				return nil, errors.New("since_id must be non-negative")
			}
			cli, base := clientFactory(10 * time.Second)
			if strings.TrimSpace(in.Relic) != "" {
				escapedRelic, err := escapeMCPPathID(in.Relic, "relic")
				if err != nil {
					return nil, err
				}
				body, err := fetchGETContext(ctx, cli, base+"/api/v1/conclaves/"+escapedID+"/relics/"+escapedRelic)
				if err != nil {
					return nil, err
				}
				var r domain.ConclaveRelic
				if err := json.Unmarshal(body, &r); err != nil {
					return nil, err
				}
				return []byte(formatCouncilRelicText(r)), nil
			}
			body, err := fetchGETContext(ctx, cli, base+"/api/v1/conclaves/"+escapedID)
			if err != nil {
				return nil, err
			}
			var detail councilDetailResponse
			if err := json.Unmarshal(body, &detail); err != nil {
				return nil, err
			}
			return []byte(formatCouncilReadText(detail, in.SinceID)), nil
		},
	)

	addJSONTool(
		"sablecourt_council_say",
		"Send an operator Decree to a council or close/cancel the council.",
		`{"type":"object","properties":{"conclave_id":{"type":"string"},"body":{"type":"string"},"to":{"type":"string"},"close":{"type":"boolean"},"state":{"type":"string","enum":["completed","cancelled"]}},"required":["conclave_id"]}`,
		func(ctx context.Context, raw json.RawMessage) ([]byte, error) {
			var in struct {
				ConclaveID string `json:"conclave_id"`
				Body       string `json:"body"`
				To         string `json:"to"`
				Close      *bool  `json:"close"`
				State      string `json:"state"`
			}
			if err := decodeMCPArgs(raw, &in); err != nil {
				return nil, err
			}
			escapedID, err := escapeMCPPathID(in.ConclaveID, "conclave_id")
			if err != nil {
				return nil, err
			}
			conclaveID := strings.TrimSpace(in.ConclaveID)
			body := strings.TrimSpace(in.Body)
			if len(body) > maxCouncilQuestionBytes {
				return nil, errors.New("body exceeds 64 KiB")
			}
			state := strings.TrimSpace(in.State)
			switch state {
			case "", string(domain.ConclaveStateCompleted), string(domain.ConclaveStateCancelled):
			default:
				return nil, fmt.Errorf("invalid close state %q (expected completed or cancelled)", state)
			}
			shouldClose := in.Close != nil && *in.Close
			if state != "" {
				if in.Close != nil && !*in.Close {
					return nil, errors.New("state cannot be set when close is false")
				}
				shouldClose = true
			}
			if shouldClose && state == "" {
				state = string(domain.ConclaveStateCompleted)
			}
			if body == "" && !shouldClose {
				return nil, errors.New("body or close is required")
			}
			if body != "" && shouldClose {
				return nil, errors.New("body cannot be combined with close or state; send the decree and close in separate calls")
			}

			cli, base := clientFactory(10 * time.Second)
			if body != "" {
				to := strings.TrimSpace(in.To)
				if to == "" {
					to = "all"
				}
				msgRaw, err := fetchPOSTContext(ctx, cli, base+"/api/v1/conclaves/"+escapedID+"/decree", map[string]string{
					"to":   to,
					"body": body,
				})
				if err != nil {
					return nil, err
				}
				var msg domain.ConclaveMessage
				_ = json.Unmarshal(msgRaw, &msg)
				return []byte(fmt.Sprintf("decree #%d sent to @%s", msg.ID, to)), nil
			}
			if _, err := closeConclaveAndStopTasks(ctx, clientFactory, base, conclaveID, domain.ConclaveState(state)); err != nil {
				return nil, err
			}
			return []byte(fmt.Sprintf("council %s closed (%s)", conclaveID, state)), nil
		},
	)

	return srv
}

func decodeMCPArgs(raw json.RawMessage, dst any) error {
	if len(raw) > maxMCPArgsBytes {
		return errors.New("tool arguments exceed 256 KiB")
	}
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return errors.New("tool arguments object is required")
	}
	if trimmed[0] != '{' {
		return errors.New("tool arguments must be a JSON object")
	}
	return json.Unmarshal(trimmed, dst)
}

func escapeMCPPathID(raw, field string) (string, error) {
	id := strings.TrimSpace(raw)
	if id == "" {
		return "", fmt.Errorf("%s is required", field)
	}
	if hasControlChars(id) {
		return "", fmt.Errorf("invalid %s", field)
	}
	return url.PathEscape(id), nil
}

func formatCouncilRelicText(r domain.ConclaveRelic) string {
	sha := r.SHA256
	if len(sha) > 12 {
		sha = sha[:12]
	}
	return fmt.Sprintf("Relic %s (by @%s, %s, sha256=%s):\n%s\n",
		r.Name, r.AuthorRole, r.ContentType, sha, strings.TrimSpace(r.Body))
}

func formatCouncilReadText(detail councilDetailResponse, sinceID int64) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Council %s (%q) state=%s template=%s\n",
		detail.Conclave.ID, detail.Conclave.Title, detail.Conclave.State, detail.Conclave.TemplateName)
	if detail.Conclave.WardReason != "" {
		fmt.Fprintf(&b, "Ward: %q\n", detail.Conclave.WardReason)
	}
	b.WriteString("\nMessages:\n")
	msgCount := 0
	for _, m := range detail.Messages {
		if m.ID <= sinceID {
			continue
		}
		msgCount++
		roundTag := ""
		if m.Round > 0 {
			roundTag = fmt.Sprintf(" round=%d", m.Round)
		}
		fmt.Fprintf(&b, "  #%d%s @%s -> @%s (%s): %q\n", m.ID, roundTag, m.FromRole, m.ToRole, m.Kind, strings.TrimSpace(m.Body))
	}
	if msgCount == 0 {
		b.WriteString("  (none)\n")
	}
	if len(detail.Relics) > 0 {
		b.WriteString("\nRelics:\n")
		for _, r := range detail.Relics {
			sha := r.SHA256
			if len(sha) > 12 {
				sha = sha[:12]
			}
			fmt.Fprintf(&b, "  - %s (by @%s, %s, sha256=%s)\n", r.Name, r.AuthorRole, r.ContentType, sha)
		}
	}
	if digest, ok := findDigestRelic(detail.Relics); ok {
		fmt.Fprintf(&b, "\nDigest (%s):\n%s\n", digest.Name, strings.TrimSpace(digest.Body))
	} else {
		b.WriteString("\nDigest: no digest recorded\n")
	}
	return b.String()
}
