package model

import (
	"encoding/json"

	"github.com/shadewright-labs/sablecourt/internal/domain"
	"github.com/shadewright-labs/sablecourt/internal/store"
)

var errInvalidChildBudgetCeiling = store.ErrInvalidChildBudgetCeiling

// estimateRequestReserve derives conservative pre-flight token and cost reservation bounds
// from a provider request body while preserving a 1-unit minimum per dimension.
func estimateRequestReserve(provider, modelName string, body []byte, pricing map[string]PricePerMillionTokens) domain.BudgetTotals {
	inTokens := int64(1)
	outTokens := int64(1)

	var payload map[string]any
	if len(body) > 0 && json.Unmarshal(body, &payload) == nil && payload != nil {
		var textBytes int64
		for _, field := range []string{"messages", "input", "contents", "system", "systemInstruction", "prompt"} {
			if val, ok := payload[field]; ok {
				textBytes += countPromptTextBytes(val)
			}
		}
		if est := textBytes / 4; est > inTokens {
			inTokens = est
		}
		if requested := extractRequestedOutputTokens(payload); requested > outTokens {
			outTokens = requested
		}
	}

	costMicro := EstimateMicroUSD(provider, modelName, inTokens, outTokens, pricing)
	if costMicro < 1 {
		costMicro = 1
	}
	return domain.BudgetTotals{
		InputTokens:  inTokens,
		OutputTokens: outTokens,
		CostMicroUSD: costMicro,
	}
}

func countPromptTextBytes(v any) int64 {
	switch x := v.(type) {
	case string:
		return int64(len(x))
	case []any:
		var total int64
		for _, item := range x {
			total += countPromptTextBytes(item)
		}
		return total
	case map[string]any:
		var total int64
		for k, item := range x {
			switch k {
			case "role", "type", "name", "id", "tool_call_id", "mimeType", "media_type":
				continue
			}
			total += countPromptTextBytes(item)
		}
		return total
	default:
		return 0
	}
}

func extractRequestedOutputTokens(payload map[string]any) int64 {
	for _, field := range []string{"max_tokens", "max_completion_tokens", "max_output_tokens"} {
		if n, ok := positiveJSONInt64(payload[field]); ok {
			return n
		}
	}
	if genCfg, ok := payload["generationConfig"].(map[string]any); ok {
		if n, ok := positiveJSONInt64(genCfg["maxOutputTokens"]); ok {
			return n
		}
	}
	return 0
}

func positiveJSONInt64(v any) (int64, bool) {
	f, ok := v.(float64)
	if !ok || f <= 0 {
		return 0, false
	}
	return int64(f), true
}
