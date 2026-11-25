package redact

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
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
		re:       regexp.MustCompile(`(?i)(authorization["']?\s*:\s*["']?bearer\s+)[A-Za-z0-9._~+/=-]{8,}`),
		replace:  "${1}[REDACTED:bearer_token]",
	},
	{
		// Any other scheme: the upstream MCP gate sends whatever prefix a
		// server is configured with (Basic, Token, ApiKey, ...). The value
		// class excludes "[" so a Bearer value already redacted above is not
		// matched again.
		category: "authorization_header",
		re:       regexp.MustCompile(`(?i)(authorization["']?\s*[:=]\s*["']?[A-Za-z][A-Za-z0-9_-]*\s+)[A-Za-z0-9._~+/=-]{8,}`),
		replace:  "${1}[REDACTED:authorization]",
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

const privateKeyMarker = "[REDACTED:private_key]"

var privateKeyHeader = regexp.MustCompile(`-----BEGIN ([A-Z0-9 ]*PRIVATE KEY)-----`)

type Result struct {
	Text       string
	Hits       int
	Categories []string
}

// Text redacts sensitive credentials deterministically in linear time.
func Text(input string) Result {
	if input == "" {
		return Result{}
	}
	seen := make(map[string]struct{})
	// Key blocks go first: an assignment rule could otherwise consume the
	// start of a header and leave the key body behind.
	out, totalHits := redactKeyBlocks(input)
	if totalHits > 0 {
		seen["private_key"] = struct{}{}
	}

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

// redactKeyBlocks replaces each private key block with a marker. A block ends
// only at the footer with its own label; a header without one, because the
// footer was cut off, never written or carries another label, withholds
// everything from the header on and the marker records how many bytes.
func redactKeyBlocks(input string) (string, int) {
	if !strings.Contains(input, "-----BEGIN ") {
		return input, 0
	}
	var b strings.Builder
	hits := 0
	for offset := 0; ; {
		loc := privateKeyHeader.FindStringSubmatchIndex(input[offset:])
		if loc == nil {
			if hits == 0 {
				return input, 0
			}
			b.WriteString(input[offset:])
			return b.String(), hits
		}
		start, end := offset+loc[0], offset+loc[1]
		footer := "-----END " + input[offset+loc[2]:offset+loc[3]] + "-----"
		b.WriteString(input[offset:start])
		hits++
		i := strings.Index(input[end:], footer)
		if i < 0 {
			fmt.Fprintf(&b, "[REDACTED:private_key; %d bytes withheld]", len(input)-start)
			return b.String(), hits
		}
		b.WriteString(privateKeyMarker)
		offset = end + i + len(footer)
	}
}

// String is a convenience wrapper returning only the redacted string.
func String(input string) string {
	return Text(input).Text
}

// ContainsSensitive reports whether input matches any redaction rule.
func ContainsSensitive(input string) bool {
	res := Text(input)
	return res.Hits > 0 && strings.Contains(res.Text, "[REDACTED:")
}
