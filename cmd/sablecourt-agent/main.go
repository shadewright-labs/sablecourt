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
)

type taskInput struct {
	Prompt   string `json:"prompt"`
	Input    string `json:"input"`
	Provider string `json:"provider"`
	Model    string `json:"model"`
}

// sablecourt-agent is a lightweight container runner that executes a single LLM turn
// exclusively through Sablecourt's Model Gate and posts the reply back to the task thread.
func main() {
	taskID := os.Getenv("SABLECOURT_TASK_ID")
	workload := os.Getenv("SABLECOURT_WORKLOAD")
	token := os.Getenv("SABLECOURT_TASK_TOKEN")
	modelGate := strings.TrimRight(os.Getenv("SABLECOURT_MODEL_GATE_URL"), "/")
	dataGate := strings.TrimRight(os.Getenv("SABLECOURT_DATA_GATE_URL"), "/")
	gateRoot := strings.TrimSuffix(dataGate, "/gate/mcp")

	if token == "" || modelGate == "" {
		fmt.Fprintln(os.Stderr, "sablecourt-agent requires SABLECOURT_TASK_TOKEN and SABLECOURT_MODEL_GATE_URL")
		os.Exit(1)
	}

	var in taskInput
	rawIn := strings.TrimSpace(os.Getenv("SABLECOURT_INPUT_JSON"))
	if rawIn != "" {
		if err := json.Unmarshal([]byte(rawIn), &in); err != nil {
			in.Prompt = rawIn
		}
	}
	prompt := strings.TrimSpace(in.Prompt)
	if in.Input != "" {
		if prompt != "" {
			prompt += "\n\nContext:\n" + in.Input
		} else {
			prompt = in.Input
		}
	}
	if prompt == "" {
		prompt = fmt.Sprintf("Hello from Sablecourt task %s (%s). Report readiness briefly.", taskID, workload)
	}

	provider := strings.ToLower(strings.TrimSpace(in.Provider))
	if provider == "" {
		provider = "anthropic"
	}
	model := strings.TrimSpace(in.Model)
	if model == "" {
		model = defaultModelFor(provider)
	}

	reply, err := callModelGate(modelGate, token, provider, model, prompt)
	if err != nil {
		if gateRoot != "" {
			_ = postThreadMessage(gateRoot+"/gate/task/messages", token, "system", fmt.Sprintf("Model Gate call (%s/%s) failed: %v", provider, model, err))
		}
		fmt.Fprintf(os.Stderr, "model gate error: %v\n", err)
		os.Exit(1)
	}

	fmt.Println(reply)
	if gateRoot != "" {
		if err := postThreadMessage(gateRoot+"/gate/task/messages", token, "agent", reply); err != nil {
			fmt.Fprintf(os.Stderr, "warning: failed to post reply to thread: %v\n", err)
		}
	}
}

func defaultModelFor(provider string) string {
	switch provider {
	case "openai":
		return "gpt-5-mini"
	case "gemini":
		return "gemini-2.5-flash"
	case "local":
		return "qwen2.5-coder:32b"
	default:
		return "claude-sonnet-4-5"
	}
}

func callModelGate(baseURL, token, provider, model, prompt string) (string, error) {
	cli := &http.Client{Timeout: 90 * time.Second}
	var endpoint string
	var body []byte
	var err error

	switch provider {
	case "openai", "local":
		endpoint = baseURL + "/" + provider + "/v1/chat/completions"
		body, err = json.Marshal(map[string]any{
			"model": model,
			"messages": []map[string]string{
				{"role": "user", "content": prompt},
			},
		})
	case "anthropic":
		endpoint = baseURL + "/anthropic/v1/messages"
		body, err = json.Marshal(map[string]any{
			"model":      model,
			"max_tokens": 1024,
			"messages": []map[string]string{
				{"role": "user", "content": prompt},
			},
		})
	case "gemini":
		endpoint = fmt.Sprintf("%s/gemini/v1beta/models/%s:generateContent", baseURL, model)
		body, err = json.Marshal(map[string]any{
			"contents": []map[string]any{
				{"parts": []map[string]string{{"text": prompt}}},
			},
		})
	default:
		return "", fmt.Errorf("unsupported provider %q", provider)
	}
	if err != nil {
		return "", err
	}

	req, err := http.NewRequest(http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := cli.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return "", err
	}
	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(respBody)))
	}
	return extractText(provider, respBody), nil
}

func extractText(provider string, body []byte) string {
	switch provider {
	case "openai", "local":
		var out struct {
			Choices []struct {
				Message struct {
					Content string `json:"content"`
				} `json:"message"`
			} `json:"choices"`
		}
		if json.Unmarshal(body, &out) == nil && len(out.Choices) > 0 {
			return out.Choices[0].Message.Content
		}
	case "anthropic":
		var out struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		}
		if json.Unmarshal(body, &out) == nil && len(out.Content) > 0 {
			return out.Content[0].Text
		}
	case "gemini":
		var out struct {
			Candidates []struct {
				Content struct {
					Parts []struct {
						Text string `json:"text"`
					} `json:"parts"`
				} `json:"content"`
			} `json:"candidates"`
		}
		if json.Unmarshal(body, &out) == nil && len(out.Candidates) > 0 && len(out.Candidates[0].Content.Parts) > 0 {
			return out.Candidates[0].Content.Parts[0].Text
		}
	}
	return strings.TrimSpace(string(body))
}

func postThreadMessage(url, token, role, content string) error {
	raw, _ := json.Marshal(map[string]string{"role": role, "content": content})
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return nil
}
