package model

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"math"
	"strconv"

	"github.com/shadewright-labs/sablecourt/internal/domain"
)

const maxChildBudgetCeilingBytes = 4 << 10

var errInvalidChildBudgetCeiling = errors.New("invalid_child_budget_ceiling")

func childBudgetReason(raw string, totals domain.BudgetTotals) (string, error) {
	if len(raw) > maxChildBudgetCeilingBytes {
		return "", errInvalidChildBudgetCeiling
	}
	var ceiling struct {
		Input  jsontext.Value `json:"maxInputTokens"`
		Output jsontext.Value `json:"maxOutputTokens"`
		Cost   jsontext.Value `json:"maxCostUSD"`
	}
	if err := json.Unmarshal([]byte(raw), &ceiling, json.RejectUnknownMembers(true)); err != nil {
		return "", errInvalidChildBudgetCeiling
	}
	if ceiling.Input == nil && ceiling.Output == nil && ceiling.Cost == nil {
		return "", errInvalidChildBudgetCeiling
	}

	var reason string
	for _, limit := range []struct {
		value  jsontext.Value
		used   int64
		reason string
		usd    bool
	}{
		{value: ceiling.Input, used: totals.InputTokens, reason: "child_input_tokens"},
		{value: ceiling.Output, used: totals.OutputTokens, reason: "child_output_tokens"},
		{value: ceiling.Cost, used: totals.CostMicroUSD, reason: "child_cost_usd", usd: true},
	} {
		if limit.value == nil {
			continue
		}
		maximum, err := childBudgetLimit(limit.value, limit.usd)
		if err != nil {
			return "", err
		}
		if reason == "" && limit.used >= maximum {
			reason = limit.reason
		}
	}
	return reason, nil
}

func childBudgetLimit(value jsontext.Value, usd bool) (int64, error) {
	if value.Kind() != '0' {
		return 0, errInvalidChildBudgetCeiling
	}
	if usd {
		cost, err := strconv.ParseFloat(string(value), 64)
		microUSD := cost * 1_000_000
		if err != nil || microUSD < 0 || microUSD >= math.MaxInt64 {
			return 0, errInvalidChildBudgetCeiling
		}
		return int64(microUSD), nil
	}
	tokens, err := strconv.ParseInt(string(value), 10, 64)
	if err != nil || tokens < 0 {
		return 0, errInvalidChildBudgetCeiling
	}
	return tokens, nil
}
