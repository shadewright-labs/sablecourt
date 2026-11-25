package model

import (
	"encoding/json"
	"strconv"
	"strings"

	"github.com/shadewright-labs/sablecourt/internal/domain"
	"github.com/shadewright-labs/sablecourt/internal/store"
)

var errInvalidChildBudgetCeiling = store.ErrInvalidChildBudgetCeiling

// estimateRequestReserve derives conservative pre-flight token and cost reservation bounds
// from a provider request body while preserving a 1-unit minimum per dimension. When
// fallbackProviders are supplied, cost uses the largest estimate across primary and
// fallback providers so a fallback dispatch cannot settle above the reserved cost.
func estimateRequestReserve(provider, modelName string, body []byte, pricing map[string]PricePerMillionTokens, fallbackProviders ...string) domain.BudgetTotals {
	inTokens := int64(1)
	outTokens := int64(1)

	var payload map[string]any
	if len(body) > 0 && json.Unmarshal(body, &payload) == nil && payload != nil {
		var textBytes int64
		for _, field := range []string{"messages", "input", "contents", "system", "systemInstruction", "prompt", "tools", "functions"} {
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
		outTokens = scaleOutputTokensByChoices(outTokens, payload)
	}

	costMicro := EstimateMicroUSD(provider, modelName, inTokens, outTokens, pricing)
	for _, fb := range fallbackProviders {
		if fbCost := EstimateMicroUSD(fb, modelName, inTokens, outTokens, pricing); fbCost > costMicro {
			costMicro = fbCost
		}
	}
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
			total += int64(len(k)) + countPromptTextBytes(item)
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
	if n, ok := geminiConfigInt(payload, "maxOutputTokens", "max_output_tokens"); ok {
		return n
	}
	return 0
}

// scaleOutputTokensByChoices multiplies the output reservation by the requested number
// of Chat Completions choices or Gemini candidates, because the provider bills output for every choice.
func scaleOutputTokensByChoices(outTokens int64, payload map[string]any) int64 {
	choices := int64(1)
	if n, ok := positiveJSONInt64(payload["n"]); ok {
		choices = n
	}
	if c, ok := geminiConfigInt(payload, "candidateCount", "candidate_count"); ok && c > choices {
		choices = c
	}
	if choices <= 1 {
		return outTokens
	}
	if outTokens > maxReservedTokens/choices {
		return maxReservedTokens
	}
	return outTokens * choices
}

// maxReservedTokens caps a single reservation dimension so that float to integer
// conversions and cost estimates cannot overflow.
const maxReservedTokens = int64(1) << 40

func positiveJSONInt64(v any) (int64, bool) {
	f, ok := v.(float64)
	if !ok || f <= 0 {
		return 0, false
	}
	if f >= float64(maxReservedTokens) {
		return maxReservedTokens, true
	}
	return int64(f), true
}

// geminiConfigInt reads a Gemini generation limit from either spelling of the config
// object and either spelling of the field, accepting JSON numbers or decimal strings,
// and returns the largest valid value so the reservation is never understated.
func geminiConfigInt(payload map[string]any, names ...string) (int64, bool) {
	var best int64
	var found bool
	for _, cfgKey := range []string{"generationConfig", "generation_config"} {
		cfg, ok := payload[cfgKey].(map[string]any)
		if !ok {
			continue
		}
		for _, name := range names {
			v := cfg[name]
			if str, isStr := v.(string); isStr {
				f, err := strconv.ParseFloat(strings.TrimSpace(str), 64)
				if err != nil {
					continue
				}
				v = f
			}
			if n, ok := positiveJSONInt64(v); ok && n > best {
				best, found = n, true
			}
		}
	}
	return best, found
}
