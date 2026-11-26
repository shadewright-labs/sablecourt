package model

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"math"
	"strings"

	"github.com/shadewright-labs/armory/modelusage"
	"github.com/shadewright-labs/sablecourt/internal/domain"
)

// maxUsageLineBytes bounds one SSE line or the joined data of one SSE event
// when parsing provider usage from a streamed response.
const maxUsageLineBytes = 4 << 20

// Usage holds token counts extracted from a provider reply. InputTokens is the
// uncached input count, disjoint from CacheWriteTokens and CacheReadTokens, so
// TotalInputTokens is their sum across all providers. CacheWrite1hTokens is the
// 1-hour TTL portion of CacheWriteTokens (never additive to TotalInputTokens).
type Usage struct {
	Model              string
	InputTokens        int64
	CacheWriteTokens   int64
	CacheWrite1hTokens int64
	CacheReadTokens    int64
	OutputTokens       int64
}

func satAddInt64(a, b int64) int64 {
	a = max(a, 0)
	b = max(b, 0)
	if a > math.MaxInt64-b {
		return math.MaxInt64
	}
	return a + b
}

// TotalInputTokens returns all input tokens (uncached, cache write, and cache read),
// saturating at math.MaxInt64 on overflow.
func (u Usage) TotalInputTokens() int64 {
	sum := satAddInt64(max(u.InputTokens, 0), max(u.CacheWriteTokens, 0))
	return satAddInt64(sum, max(u.CacheReadTokens, 0))
}

// TotalTokens returns all input and output tokens, saturating at math.MaxInt64 on overflow.
func (u Usage) TotalTokens() int64 {
	return satAddInt64(u.TotalInputTokens(), max(u.OutputTokens, 0))
}

// PricePerMillionTokens holds per-million-token rates in USD (equivalently,
// micro-USD per token). A zero CacheWriteUSD or CacheReadUSD means no separate
// cache rate is configured; EstimateUsageMicroUSD prices those cache tokens at
// InputUSD (never at zero) unless InputUSD itself is zero. A zero
// CacheWrite1hUSD defaults to twice InputUSD.
type PricePerMillionTokens struct {
	InputUSD        float64
	OutputUSD       float64
	CacheWriteUSD   float64
	CacheWrite1hUSD float64
	CacheReadUSD    float64
}

var defaultPricing = map[string]PricePerMillionTokens{
	// OpenAI has no separate cache write surcharge; CacheWriteUSD is omitted so
	// any reported cache_write_tokens are priced at InputUSD (never zero).
	"openai":    {InputUSD: 2.50, OutputUSD: 10.00, CacheReadUSD: 1.25},
	"anthropic": {InputUSD: 3.00, OutputUSD: 15.00, CacheWriteUSD: 3.75, CacheWrite1hUSD: 6.00, CacheReadUSD: 0.30},
	// Gemini cache rates are not in the table; cache tokens fall back to InputUSD.
	"gemini": {InputUSD: 1.25, OutputUSD: 10.00},
	"local":  {InputUSD: 0.0, OutputUSD: 0.0},
}

func resolveRate(provider, modelName string, custom map[string]PricePerMillionTokens) PricePerMillionTokens {
	rate, ok := custom[modelName]
	if !ok {
		rate, ok = custom[provider]
	}
	if !ok {
		rate, ok = defaultPricing[provider]
	}
	if !ok {
		// Conservative fallback when pricing is unknown; cache rates fall back to InputUSD.
		rate = PricePerMillionTokens{InputUSD: 5.00, OutputUSD: 15.00}
	}
	return rate
}

func effectiveRates(rate PricePerMillionTokens) (inUSD, outUSD, cacheWriteUSD, cacheWrite1hUSD, cacheReadUSD float64) {
	inUSD = max(rate.InputUSD, 0)
	outUSD = max(rate.OutputUSD, 0)
	cacheWriteUSD = rate.CacheWriteUSD
	if cacheWriteUSD <= 0 {
		// When a provider or model has no explicit cache write rate in the
		// pricing table, price cache writes at the input rate, never at zero.
		cacheWriteUSD = inUSD
	}
	cacheWrite1hUSD = rate.CacheWrite1hUSD
	if cacheWrite1hUSD <= 0 {
		// When no explicit 1-hour cache write rate is configured, default to
		// twice the input rate.
		cacheWrite1hUSD = 2 * inUSD
	}
	cacheReadUSD = rate.CacheReadUSD
	if cacheReadUSD <= 0 {
		// When a provider or model has no explicit cache read rate in the
		// pricing table, price cache reads at the input rate, never at zero.
		cacheReadUSD = inUSD
	}
	return inUSD, outUSD, cacheWriteUSD, cacheWrite1hUSD, cacheReadUSD
}

func microUSDFromFloat(micro float64) int64 {
	if micro <= 0 || math.IsNaN(micro) {
		return 0
	}
	ceiled := math.Ceil(micro)
	if ceiled >= float64(math.MaxInt64) {
		return math.MaxInt64
	}
	return int64(ceiled)
}

// EstimateMicroUSD computes the cost in micro-USD for uncached input and output tokens.
func EstimateMicroUSD(provider, modelName string, inputTokens, outputTokens int64, custom map[string]PricePerMillionTokens) int64 {
	return EstimateUsageMicroUSD(provider, modelName, Usage{
		InputTokens:  inputTokens,
		OutputTokens: outputTokens,
	}, custom)
}

// EstimateUsageMicroUSD computes the cost in micro-USD for uncached input,
// cache write (splitting 1-hour writes capped at total cache writes), cache
// read, and output tokens.
func EstimateUsageMicroUSD(provider, modelName string, usage Usage, custom map[string]PricePerMillionTokens) int64 {
	inUSD, outUSD, cacheWriteUSD, cacheWrite1hUSD, cacheReadUSD := effectiveRates(resolveRate(provider, modelName, custom))
	in := float64(max(usage.InputTokens, 0))
	cwTotal := max(usage.CacheWriteTokens, 0)
	cw1h := min(max(usage.CacheWrite1hTokens, 0), cwTotal)
	cw5m := cwTotal - cw1h
	cr := float64(max(usage.CacheReadTokens, 0))
	out := float64(max(usage.OutputTokens, 0))
	// Rates are USD per 1M tokens, which equals micro-USD per token.
	return microUSDFromFloat(in*inUSD + float64(cw5m)*cacheWriteUSD + float64(cw1h)*cacheWrite1hUSD + cr*cacheReadUSD + out*outUSD)
}

// estimateReserveMicroUSD prices a pre-call reservation without assuming any
// cache read discount, using the highest applicable input rate (including the
// 1-hour cache write rate for Anthropic or when explicitly configured) so
// providers with a cache write surcharge never reserve less than a
// cache-creating call can cost.
func estimateReserveMicroUSD(provider, modelName string, inputTokens, outputTokens int64, custom map[string]PricePerMillionTokens) int64 {
	rate := resolveRate(provider, modelName, custom)
	inUSD, outUSD, cacheWriteUSD, cacheWrite1hUSD, _ := effectiveRates(rate)
	reserveInUSD := max(inUSD, cacheWriteUSD)
	if provider == "anthropic" || rate.CacheWrite1hUSD > 0 {
		reserveInUSD = max(reserveInUSD, cacheWrite1hUSD)
	}
	in := float64(max(inputTokens, 0))
	out := float64(max(outputTokens, 0))
	return microUSDFromFloat(in*reserveInUSD + out*outUSD)
}

type usageOutcome int

const (
	usageOutcomeNone usageOutcome = iota
	usageOutcomeFinal
	usageOutcomeTrailing
	usageOutcomePartial
)

type parsedUsage struct {
	Usage   Usage
	Outcome usageOutcome
	Err     error
}

func familyForRoute(provider, upstreamPath string) (modelusage.Family, bool) {
	switch provider {
	case "anthropic":
		return modelusage.Anthropic, true
	case "openai", "local":
		if upstreamPath == "/v1/responses" {
			return modelusage.OpenAIResponses, true
		}
		return modelusage.OpenAIChat, true
	default:
		return 0, false
	}
}

func cacheWrite1hTokens(mu modelusage.Usage, cw int64) int64 {
	if cw <= 0 {
		return 0
	}
	// Cap each group's 1-hour lifetime count at its own parent cache-write count
	// so a lifetime field without a parent count adds nothing.
	top := min(max(mu.CacheCreation1hInputTokens, 0), max(mu.CacheCreationInputTokens, 0))
	comp := min(max(mu.CompactionCacheCreation1hInputTokens, 0), max(mu.CompactionCacheCreationInputTokens, 0))
	adv := min(max(mu.AdvisorCacheCreation1hInputTokens, 0), max(mu.AdvisorCacheCreationInputTokens, 0))
	return min(satAddInt64(satAddInt64(top, comp), adv), cw)
}

func usageFromModelUsage(mu modelusage.Usage) (Usage, error) {
	cw := max(mu.CacheWriteTokens(), 0)
	cr := max(mu.CacheReadTokens(), 0)
	uncached := max(mu.UncachedInputTokens(), 0)
	totalIn := max(mu.TotalInputTokens(), 0)
	out := max(mu.TotalOutputTokens(), 0)
	unclassIn := max(mu.UnclassifiedIterationInputTokens, 0)
	unclassOut := max(mu.UnclassifiedIterationOutputTokens, 0)
	reportedTotal := max(mu.ReportedTotalTokens, 0)

	if totalIn > maxReservedTokens || out > maxReservedTokens ||
		unclassIn > maxReservedTokens || unclassOut > maxReservedTokens ||
		reportedTotal > maxReservedTokens ||
		cw > totalIn || cr > totalIn-cw {
		return Usage{}, modelusage.ErrOutOfRange
	}
	// Add unclassified Anthropic iteration input to cache write (1-hour TTL)
	// and output to output. An unknown iteration entry may carry 1-hour cache
	// writes, so pricing its input at the dearest input rate is fail-closed
	// while keeping TotalInputTokens unchanged.
	if unclassIn > maxReservedTokens-totalIn {
		return Usage{}, modelusage.ErrOutOfRange
	}
	cw1h := cacheWrite1hTokens(mu, cw)
	cw += unclassIn
	cw1h += unclassIn
	totalIn += unclassIn

	if unclassOut > maxReservedTokens-out {
		return Usage{}, modelusage.ErrOutOfRange
	}
	out += unclassOut

	if totalIn > maxReservedTokens-out {
		return Usage{}, modelusage.ErrOutOfRange
	}
	parts := totalIn + out
	// When the provider reports total_tokens above the sum of input and output
	// parts, charge the difference as output tokens (the dearer side) so the
	// charged total is never below the provider total.
	if reportedTotal > parts {
		out += reportedTotal - parts
	}

	return Usage{
		InputTokens:        uncached,
		CacheWriteTokens:   cw,
		CacheWrite1hTokens: cw1h,
		CacheReadTokens:    cr,
		OutputTokens:       out,
	}, nil
}

func partialUsageFromModelUsage(mu modelusage.Usage) (Usage, error) {
	// Per modelusage.PartialError, UncachedInputTokens is not a lower bound
	// on an incomplete stream. Price the partial by its sums: keep cache
	// writes (which may carry a surcharge) and bill remaining prompt tokens
	// at the uncached input rate with no cache-read discount.
	baseIn := max(mu.TotalInputTokens(), 0)
	out := max(mu.TotalOutputTokens(), 0)
	unclassIn := max(mu.UnclassifiedIterationInputTokens, 0)
	unclassOut := max(mu.UnclassifiedIterationOutputTokens, 0)
	reportedTotal := max(mu.ReportedTotalTokens, 0)

	if baseIn > maxReservedTokens || out > maxReservedTokens ||
		unclassIn > maxReservedTokens || unclassOut > maxReservedTokens ||
		reportedTotal > maxReservedTokens {
		return Usage{}, modelusage.ErrOutOfRange
	}
	cacheWrite := min(max(mu.CacheWriteTokens(), 0), baseIn)
	cacheWrite1h := cacheWrite1hTokens(mu, cacheWrite)

	// Add unclassified Anthropic iteration input to cache write (1-hour TTL)
	// and output to output; an unknown entry may carry 1-hour cache writes,
	// so billing its input at the dearest input rate is the fail-closed direction.
	if unclassIn > maxReservedTokens-baseIn {
		return Usage{}, modelusage.ErrOutOfRange
	}
	cacheWrite += unclassIn
	cacheWrite1h += unclassIn
	totalIn := baseIn + unclassIn

	if unclassOut > maxReservedTokens-out {
		return Usage{}, modelusage.ErrOutOfRange
	}
	out += unclassOut

	if totalIn > maxReservedTokens-out {
		return Usage{}, modelusage.ErrOutOfRange
	}
	parts := totalIn + out
	// Charge any excess of ReportedTotalTokens over the parts as output tokens
	// (the dearer side) so partial settlement never falls below the provider total.
	if reportedTotal > parts {
		out += reportedTotal - parts
	}

	return Usage{
		InputTokens:        totalIn - cacheWrite,
		CacheWriteTokens:   cacheWrite,
		CacheWrite1hTokens: cacheWrite1h,
		OutputTokens:       out,
	}, nil
}

func parseUsage(provider, upstreamPath string, body []byte, contentType string) parsedUsage {
	isSSE := strings.Contains(strings.ToLower(contentType), "text/event-stream")
	if family, ok := familyForRoute(provider, upstreamPath); ok {
		lim := modelusage.Limits{
			MaxBytes:     maxCapturedResponseBytes,
			MaxLineBytes: maxUsageLineBytes,
			MaxTokens:    maxReservedTokens,
		}
		modelName := extractResponseModel(provider, body, isSSE)
		if isSSE {
			mu, err := modelusage.ParseStream(family, bytes.NewReader(body), lim)
			if err == nil {
				u, uerr := usageFromModelUsage(mu)
				if uerr != nil {
					return parsedUsage{Usage: Usage{Model: modelName}, Outcome: usageOutcomeNone, Err: uerr}
				}
				u.Model = modelName
				return parsedUsage{Usage: u, Outcome: usageOutcomeFinal}
			}
			if _, ok := errors.AsType[*modelusage.TrailingError](err); ok {
				u, uerr := usageFromModelUsage(mu)
				if uerr != nil {
					return parsedUsage{Usage: Usage{Model: modelName}, Outcome: usageOutcomeNone, Err: uerr}
				}
				u.Model = modelName
				return parsedUsage{Usage: u, Outcome: usageOutcomeTrailing, Err: err}
			}
			if _, ok := errors.AsType[*modelusage.PartialError](err); ok {
				u, uerr := partialUsageFromModelUsage(mu)
				if uerr != nil {
					return parsedUsage{Usage: Usage{Model: modelName}, Outcome: usageOutcomeNone, Err: uerr}
				}
				u.Model = modelName
				return parsedUsage{Usage: u, Outcome: usageOutcomePartial, Err: err}
			}
			return parsedUsage{Usage: Usage{Model: modelName}, Outcome: usageOutcomeNone, Err: err}
		}

		mu, err := modelusage.ParseJSON(family, bytes.NewReader(body), lim)
		if err != nil {
			return parsedUsage{Usage: Usage{Model: modelName}, Outcome: usageOutcomeNone, Err: err}
		}
		u, uerr := usageFromModelUsage(mu)
		if uerr != nil {
			return parsedUsage{Usage: Usage{Model: modelName}, Outcome: usageOutcomeNone, Err: uerr}
		}
		u.Model = modelName
		return parsedUsage{Usage: u, Outcome: usageOutcomeFinal}
	}

	if provider == "gemini" {
		return parseGeminiUsage(body, isSSE)
	}
	return parsedUsage{Outcome: usageOutcomeNone}
}

// settleUsageForEntry turns parsed response usage and the pre-call reservation
// into fail-closed token counts and micro-USD cost for ledger settlement.
func settleUsageForEntry(activeProvider, reqModel string, parsed parsedUsage, reserve domain.BudgetTotals, pricing map[string]PricePerMillionTokens) (Usage, int64) {
	u := parsed.Usage
	switch parsed.Outcome {
	case usageOutcomeFinal, usageOutcomeTrailing:
		u.InputTokens = max(u.InputTokens, 0)
		u.CacheWriteTokens = max(u.CacheWriteTokens, 0)
		u.CacheWrite1hTokens = min(max(u.CacheWrite1hTokens, 0), u.CacheWriteTokens)
		u.CacheReadTokens = max(u.CacheReadTokens, 0)
		u.OutputTokens = max(u.OutputTokens, 0)
		if u.TotalTokens() == 0 {
			// The response carried all-zero usage. Charge at least the admitted
			// reserve estimate so a 2xx call is never free or below admission.
			u.InputTokens = reserve.TotalInputTokens()
			u.CacheWriteTokens = 0
			u.CacheWrite1hTokens = 0
			u.CacheReadTokens = 0
			u.OutputTokens = reserve.OutputTokens
			costMicro := max(reserve.CostMicroUSD, estimateReserveMicroUSD(activeProvider, reqModel, u.InputTokens, u.OutputTokens, pricing), 0)
			return u, costMicro
		}
		// Price with the admitted request model, the identity the reservation was
		// sized for, so a reported model name cannot settle above the reserve.
		costMicro := max(EstimateUsageMicroUSD(activeProvider, reqModel, u, pricing), 0)
		return u, costMicro

	case usageOutcomePartial:
		// Partial stream usage is a lower bound (or contradictory on
		// ErrInconsistent), never final. Fail closed: never settle below the
		// reserved tokens or admitted reserve cost, and never apply cache-read discounts.
		totalIn := max(u.TotalInputTokens(), reserve.TotalInputTokens())
		cacheWrite := min(max(u.CacheWriteTokens, 0), totalIn)
		cacheWrite1h := min(max(u.CacheWrite1hTokens, 0), cacheWrite)
		u.InputTokens = totalIn - cacheWrite
		u.CacheWriteTokens = cacheWrite
		u.CacheWrite1hTokens = cacheWrite1h
		u.CacheReadTokens = 0
		u.OutputTokens = max(u.OutputTokens, reserve.OutputTokens)
		costMicro := max(
			EstimateUsageMicroUSD(activeProvider, reqModel, u, pricing),
			estimateReserveMicroUSD(activeProvider, reqModel, reserve.TotalInputTokens(), reserve.OutputTokens, pricing),
			reserve.CostMicroUSD,
			0,
		)
		return u, costMicro

	default:
		// Usage could not be read (ErrNoUsage, ErrMalformed, ErrOutOfRange,
		// ErrTruncated, or LimitError). Settle at least the admitted reserve
		// estimate so the call is never free or below admission.
		u.InputTokens = reserve.TotalInputTokens()
		u.CacheWriteTokens = 0
		u.CacheWrite1hTokens = 0
		u.CacheReadTokens = 0
		u.OutputTokens = reserve.OutputTokens
		costMicro := max(reserve.CostMicroUSD, estimateReserveMicroUSD(activeProvider, reqModel, u.InputTokens, u.OutputTokens, pricing), 0)
		return u, costMicro
	}
}

func parseGeminiUsage(body []byte, isSSE bool) parsedUsage {
	if len(body) > maxCapturedResponseBytes {
		return parsedUsage{Outcome: usageOutcomeNone, Err: modelusage.ErrTooLarge}
	}
	if !isSSE {
		u, ok, err := parseGeminiJSONChecked(body)
		if err != nil {
			return parsedUsage{Usage: u, Outcome: usageOutcomeNone, Err: err}
		}
		if !ok {
			return parsedUsage{Usage: u, Outcome: usageOutcomeNone}
		}
		return parsedUsage{Usage: u, Outcome: usageOutcomeFinal}
	}
	var acc Usage
	var seen bool
	scanner := bufio.NewScanner(bytes.NewReader(body))
	scanner.Buffer(make([]byte, 0, 64*1024), maxUsageLineBytes)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" || payload == "[DONE]" {
			continue
		}
		u, ok, err := parseGeminiJSONChecked([]byte(payload))
		if err != nil {
			if errors.Is(err, modelusage.ErrOutOfRange) {
				return parsedUsage{Usage: Usage{Model: acc.Model}, Outcome: usageOutcomeNone, Err: err}
			}
			if seen {
				return parsedUsage{Usage: acc, Outcome: usageOutcomePartial, Err: err}
			}
			return parsedUsage{Usage: acc, Outcome: usageOutcomeNone, Err: err}
		}
		if u.Model != "" {
			acc.Model = u.Model
		}
		if !ok {
			continue
		}
		if u.TotalInputTokens() > 0 {
			acc.InputTokens = u.InputTokens
			acc.CacheReadTokens = u.CacheReadTokens
			seen = true
		}
		if u.OutputTokens > 0 {
			acc.OutputTokens = u.OutputTokens
			seen = true
		}
		if acc.TotalInputTokens() > maxReservedTokens-acc.OutputTokens {
			return parsedUsage{Usage: Usage{Model: acc.Model}, Outcome: usageOutcomeNone, Err: modelusage.ErrOutOfRange}
		}
	}
	if err := scanner.Err(); err != nil {
		if seen {
			return parsedUsage{Usage: acc, Outcome: usageOutcomePartial, Err: err}
		}
		return parsedUsage{Usage: acc, Outcome: usageOutcomeNone, Err: err}
	}
	if !seen {
		return parsedUsage{Usage: acc, Outcome: usageOutcomeNone}
	}
	return parsedUsage{Usage: acc, Outcome: usageOutcomeFinal}
}

func parseGeminiJSON(body []byte) (Usage, bool) {
	u, ok, err := parseGeminiJSONChecked(body)
	if err != nil {
		return Usage{Model: u.Model}, false
	}
	return u, ok
}

// integerOverflow reports whether a decode failed because an integer literal does not fit the field, so a count
// above int64 takes the same path as any other out-of-range count. Fractions, exponents and other type
// mismatches stay malformed.
func integerOverflow(err error) bool {
	var typeErr *json.UnmarshalTypeError
	if !errors.As(err, &typeErr) {
		return false
	}
	digits, ok := strings.CutPrefix(typeErr.Value, "number ")
	if !ok {
		return false
	}
	digits = strings.TrimPrefix(digits, "-")
	if digits == "" {
		return false
	}
	for _, c := range digits {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

func parseGeminiJSONChecked(body []byte) (Usage, bool, error) {
	var resp struct {
		ModelVersion  string `json:"modelVersion"`
		UsageMetadata struct {
			PromptTokenCount        int64 `json:"promptTokenCount"`
			CachedContentTokenCount int64 `json:"cachedContentTokenCount"`
			CandidatesTokenCount    int64 `json:"candidatesTokenCount"`
			// Thinking tokens are reported apart from the candidates and are billed as output.
			ThoughtsTokenCount int64 `json:"thoughtsTokenCount"`
		} `json:"usageMetadata"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		if integerOverflow(err) {
			return Usage{}, false, modelusage.ErrOutOfRange
		}
		return Usage{}, false, modelusage.ErrMalformed
	}
	meta := resp.UsageMetadata
	if meta.PromptTokenCount > maxReservedTokens ||
		meta.CachedContentTokenCount > maxReservedTokens ||
		meta.CandidatesTokenCount > maxReservedTokens ||
		meta.ThoughtsTokenCount > maxReservedTokens {
		return Usage{Model: resp.ModelVersion}, false, modelusage.ErrOutOfRange
	}
	prompt := max(meta.PromptTokenCount, 0)
	cached := max(meta.CachedContentTokenCount, 0)
	candidates := max(meta.CandidatesTokenCount, 0)
	thoughts := max(meta.ThoughtsTokenCount, 0)
	if cached > prompt {
		return Usage{Model: resp.ModelVersion}, false, modelusage.ErrOutOfRange
	}
	if candidates > maxReservedTokens-thoughts {
		return Usage{Model: resp.ModelVersion}, false, modelusage.ErrOutOfRange
	}
	output := candidates + thoughts
	if prompt > maxReservedTokens-output {
		return Usage{Model: resp.ModelVersion}, false, modelusage.ErrOutOfRange
	}
	if prompt <= 0 && output <= 0 {
		return Usage{Model: resp.ModelVersion}, false, nil
	}
	return Usage{
		Model:           resp.ModelVersion,
		InputTokens:     prompt - cached,
		CacheReadTokens: cached,
		OutputTokens:    output,
	}, true, nil
}

func extractResponseModel(provider string, body []byte, isSSE bool) string {
	if len(body) > maxCapturedResponseBytes {
		body = body[:maxCapturedResponseBytes]
	}
	if !isSSE {
		return extractJSONModel(body)
	}
	scanner := bufio.NewScanner(bytes.NewReader(body))
	scanner.Buffer(make([]byte, 0, 64*1024), maxUsageLineBytes)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" || payload == "[DONE]" {
			continue
		}
		if m := extractJSONModel([]byte(payload)); m != "" {
			return m
		}
	}
	_ = provider
	return ""
}

func extractJSONModel(data []byte) string {
	var env struct {
		Model        string `json:"model"`
		ModelVersion string `json:"modelVersion"`
		Message      struct {
			Model string `json:"model"`
		} `json:"message"`
		Response struct {
			Model string `json:"model"`
		} `json:"response"`
	}
	if err := json.Unmarshal(data, &env); err != nil {
		return ""
	}
	switch {
	case env.Model != "":
		return env.Model
	case env.Message.Model != "":
		return env.Message.Model
	case env.Response.Model != "":
		return env.Response.Model
	default:
		return env.ModelVersion
	}
}
