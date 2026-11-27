package redact

import (
	"regexp"
	"sort"
)

type rule struct {
	category string
	re       *regexp.Regexp
	replace  string
}

// Go's regexp package implements RE2, guaranteeing linear O(n) scan time with no backtracking.
const sensitiveField = `[a-z0-9_-]*(?:api[_-]?key|secret|password|passwd|access[_-]?token|operator[_-]?token)`

var rules = []rule{
	{
		category: "private_key",
		re:       regexp.MustCompile(`(?s)-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----.*?-----END [A-Z0-9 ]*PRIVATE KEY-----`),
		replace:  "[REDACTED:private_key]",
	},
	{
		category: "sablecourt_token",
		re:       regexp.MustCompile(`\bsc_task_[0-9a-fA-F]{16,}\b|\bop_[0-9a-fA-F]{64}\b`),
		replace:  "[REDACTED:sablecourt_token]",
	},
	{
		category: "anthropic_key",
		re:       regexp.MustCompile(`\bsk-ant-[A-Za-z0-9_-]{16,}\b`),
		replace:  "[REDACTED:anthropic_key]",
	},
	{
		category: "openai_key",
		re:       regexp.MustCompile(`\bsk-(?:proj-)?[A-Za-z0-9_-]{20,}\b`),
		replace:  "[REDACTED:openai_key]",
	},
	{
		category: "github_token",
		re:       regexp.MustCompile(`\b(?:ghp|gho|ghu|ghs|ghr)_[A-Za-z0-9]{20,}\b|\bgithub_pat_[A-Za-z0-9_]{20,}\b`),
		replace:  "[REDACTED:github_token]",
	},
	{
		category: "google_api_key",
		re:       regexp.MustCompile(`\bAIza[0-9A-Za-z_-]{20,}\b`),
		replace:  "[REDACTED:google_api_key]",
	},
	{
		category: "aws_access_key",
		re:       regexp.MustCompile(`\b(?:AKIA|ASIA)[0-9A-Z]{16}\b`),
		replace:  "[REDACTED:aws_access_key]",
	},
	{
		category: "bearer_header",
		re:       regexp.MustCompile(`(?i)(authorization\s*:\s*bearer\s+)[A-Za-z0-9._~+/=-]{8,}`),
		replace:  "${1}[REDACTED:bearer_token]",
	},
	{
		category: "generic_secret_kv",
		re:       regexp.MustCompile(`(?i)("` + sensitiveField + `"\s*:\s*")(?:[^"\\]|\\.)*(")`),
		replace:  "${1}[REDACTED:secret]${2}",
	},
	{
		category: "generic_secret_kv",
		re:       regexp.MustCompile(`(?i)\b(` + sensitiveField + `)\b(\s*[:=]\s*["']?)([^\s"',;]{6,})`),
		replace:  "${1}${2}[REDACTED:secret]",
	},
}

type Result struct {
	Text       string
	Hits       int
	Categories []string
}

// Text redacts registered daemon secrets by exact match first, then applies
// pattern rules deterministically in linear time.
func Text(input string) Result {
	return defaultRegistry.RedactText(input)
}

func redactPatterns(input string) Result {
	if input == "" {
		return Result{}
	}
	out := input
	totalHits := 0
	seen := make(map[string]struct{})

	for _, r := range rules {
		matches := r.re.FindAllStringIndex(out, -1)
		if len(matches) == 0 {
			continue
		}
		totalHits += len(matches)
		seen[r.category] = struct{}{}
		out = r.re.ReplaceAllString(out, r.replace)
	}

	cats := make([]string, 0, len(seen))
	for c := range seen {
		cats = append(cats, c)
	}
	sort.Strings(cats)

	return Result{
		Text:       out,
		Hits:       totalHits,
		Categories: cats,
	}
}

// String is a convenience wrapper returning only the redacted string.
func String(input string) string {
	return defaultRegistry.RedactString(input)
}

// ContainsSensitive reports whether input matches any registered secret or redaction rule.
func ContainsSensitive(input string) bool {
	return defaultRegistry.ContainsSensitive(input)
}
