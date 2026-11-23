package model

import (
	"bufio"
	"bytes"
	"encoding/json"
	"strings"
)

type Usage struct {
	Model        string
	InputTokens  int64
	OutputTokens int64
}

// PricePerMillionTokens holds input and output rates in micro-USD per 1M tokens.
// 1 USD = 1_000_000 micro-USD.
type PricePerMillionTokens struct {
	InputUSD  float64
	OutputUSD float64
}

var defaultPricing = map[string]PricePerMillionTokens{
	"openai":    {InputUSD: 2.50, OutputUSD: 10.00},
	"anthropic": {InputUSD: 3.00, OutputUSD: 15.00},
	"gemini":    {InputUSD: 1.25, OutputUSD: 10.00},
	"local":     {InputUSD: 0.0, OutputUSD: 0.0},
}

func EstimateMicroUSD(provider, modelName string, inputTokens, outputTokens int64, custom map[string]PricePerMillionTokens) int64 {
	rate, ok := custom[modelName]
	if !ok {
		rate, ok = custom[provider]
	}
	if !ok {
		rate, ok = defaultPricing[provider]
	}
	if !ok {
		// Conservative fallback when pricing is unknown.
		rate = PricePerMillionTokens{InputUSD: 5.00, OutputUSD: 15.00}
	}
	costUSD := (float64(inputTokens)*rate.InputUSD + float64(outputTokens)*rate.OutputUSD) / 1_000_000.0
	return int64(costUSD*1_000_000.0 + 0.5)
}

func parseUsage(provider string, body []byte, contentType string) Usage {
	isSSE := strings.Contains(strings.ToLower(contentType), "text/event-stream")
	if isSSE {
		return parseSSEUsage(provider, body)
	}
	return parseJSONUsage(provider, body)
}

func parseJSONUsage(provider string, body []byte) Usage {
	switch provider {
	case "openai", "local":
		var resp struct {
			Model string `json:"model"`
			Usage struct {
				PromptTokens     int64 `json:"prompt_tokens"`
				CompletionTokens int64 `json:"completion_tokens"`
				InputTokens      int64 `json:"input_tokens"`
				OutputTokens     int64 `json:"output_tokens"`
			} `json:"usage"`
		}
		if err := json.Unmarshal(body, &resp); err != nil {
			return Usage{}
		}
		in := resp.Usage.PromptTokens
		if in == 0 {
			in = resp.Usage.InputTokens
		}
		out := resp.Usage.CompletionTokens
		if out == 0 {
			out = resp.Usage.OutputTokens
		}
		return Usage{Model: resp.Model, InputTokens: in, OutputTokens: out}

	case "anthropic":
		var resp struct {
			Model string `json:"model"`
			Usage struct {
				InputTokens  int64 `json:"input_tokens"`
				OutputTokens int64 `json:"output_tokens"`
			} `json:"usage"`
		}
		if err := json.Unmarshal(body, &resp); err != nil {
			return Usage{}
		}
		return Usage{Model: resp.Model, InputTokens: resp.Usage.InputTokens, OutputTokens: resp.Usage.OutputTokens}

	case "gemini":
		var resp struct {
			ModelVersion  string `json:"modelVersion"`
			UsageMetadata struct {
				PromptTokenCount     int64 `json:"promptTokenCount"`
				CandidatesTokenCount int64 `json:"candidatesTokenCount"`
			} `json:"usageMetadata"`
		}
		if err := json.Unmarshal(body, &resp); err != nil {
			return Usage{}
		}
		return Usage{
			Model:        resp.ModelVersion,
			InputTokens:  resp.UsageMetadata.PromptTokenCount,
			OutputTokens: resp.UsageMetadata.CandidatesTokenCount,
		}
	}
	return Usage{}
}

func parseSSEUsage(provider string, body []byte) Usage {
	var acc Usage
	scanner := bufio.NewScanner(bytes.NewReader(body))
	buf := make([]byte, 0, 64*1024)
	scanner.Buffer(buf, 4*1024*1024)

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" || payload == "[DONE]" {
			continue
		}

		switch provider {
		case "openai", "local":
			u := parseJSONUsage(provider, []byte(payload))
			if u.Model != "" {
				acc.Model = u.Model
			}
			if u.InputTokens > 0 {
				acc.InputTokens = u.InputTokens
			}
			if u.OutputTokens > 0 {
				acc.OutputTokens = u.OutputTokens
			}
		case "anthropic":
			var evt struct {
				Type    string `json:"type"`
				Message struct {
					Model string `json:"model"`
					Usage struct {
						InputTokens  int64 `json:"input_tokens"`
						OutputTokens int64 `json:"output_tokens"`
					} `json:"usage"`
				} `json:"message"`
				Usage struct {
					InputTokens  int64 `json:"input_tokens"`
					OutputTokens int64 `json:"output_tokens"`
				} `json:"usage"`
			}
			if err := json.Unmarshal([]byte(payload), &evt); err != nil {
				continue
			}
			if evt.Message.Model != "" {
				acc.Model = evt.Message.Model
			}
			if evt.Message.Usage.InputTokens > 0 {
				acc.InputTokens = evt.Message.Usage.InputTokens
			}
			if evt.Message.Usage.OutputTokens > 0 {
				acc.OutputTokens = evt.Message.Usage.OutputTokens
			}
			if evt.Usage.InputTokens > 0 {
				acc.InputTokens = evt.Usage.InputTokens
			}
			if evt.Usage.OutputTokens > 0 {
				acc.OutputTokens = evt.Usage.OutputTokens
			}
		case "gemini":
			u := parseJSONUsage("gemini", []byte(payload))
			if u.Model != "" {
				acc.Model = u.Model
			}
			if u.InputTokens > 0 {
				acc.InputTokens = u.InputTokens
			}
			if u.OutputTokens > 0 {
				acc.OutputTokens = u.OutputTokens
			}
		}
	}
	return acc
}
