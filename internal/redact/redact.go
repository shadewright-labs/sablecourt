package redact

import (
	"encoding/base64"
	"regexp"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

type span struct {
	start int
	end   int
}

type scanStats struct {
	scannerSteps int
	bytesScanned int
}

type rule struct {
	category string
	marker   string
	mayMatch func(string) bool
	re       *regexp.Regexp
	group    int
	find     func(string, *scanStats) []span
}

// Go's regexp package implements RE2, guaranteeing linear O(n) scan time with no backtracking.
const sensitiveField = `(?:` +
	`[a-z0-9_-]*(?:api[_-]?keys?|secret(?:[_-]?access)?[_-]?keys?|private[_-]?keys?|access[_-]?keys?|signing[_-]?keys?|secrets?|passwords?|passwds?|passphrases?|credentials?|token)` +
	`|(?:[a-z0-9_-]*[_-])?(?:pwd|pass)` +
	`|tokens` +
	`)`

const kvSep = `[ \t]*(?::=|=>|[:=])[ \t]*(?:![^\s"']+[ \t]+)?`

var (
	privateKeyHeader = regexp.MustCompile(`-----BEGIN ([A-Z0-9 ]*PRIVATE KEY[A-Z0-9 ]*)-----`)
	privateKeyFooter = regexp.MustCompile(`-----END ([A-Z0-9 ]*PRIVATE KEY[A-Z0-9 ]*)-----`)

	// Matches a YAML mapping header for a sensitive key with no inline scalar value
	// (nested mapping like "secret:", anchor like "password: &pw", or block scalar like "secret: |").
	yamlSensitiveHeaderRe = regexp.MustCompile(
		`(?i)^([ \t\x{00a0}]*(?:-[ \t\x{00a0}]+)?)["']?` + sensitiveField + `["']?[ \t\x{00a0}]*:[ \t\x{00a0}]*(?:&[A-Za-z0-9_-]+[ \t\x{00a0}]*)?(?:([|>](?:[+-][1-9]?|[1-9][+-]?)?))?(?:[ \t\x{00a0}]+#.*)?[ \t\x{00a0}]*$`,
	)
	yamlLogPrefixedHeaderRe = regexp.MustCompile(
		`(?i)^(?:[0-9]{4}-[0-9]{2}-[0-9]{2}[T ][0-9:.Z+-]*|\[[^\]]+\]|(?:TRACE|DEBUG|INFO|WARN|WARNING|ERROR|FATAL))[ \t]+.*[ \t]["']?` + sensitiveField + `["']?[ \t]*:[ \t]*(?:&[A-Za-z0-9_-]+[ \t]*)?(?:([|>](?:[+-][1-9]?|[1-9][+-]?)?))?(?:[ \t]+#.*)?[ \t]*$`,
	)
	yamlKeyLineRe        = regexp.MustCompile(`^([ \t\x{00a0}]*(?:-[ \t\x{00a0}]+)?([^:#\[\r\n]+?)[ \t\x{00a0}]*:[ \t\x{00a0}]*)(.*)$`)
	yamlBlockIndicatorRe = regexp.MustCompile(`^[|>](?:[+-][1-9]?|[1-9][+-]?)?(?:[ \t\x{00a0}]+#.*)?[ \t\x{00a0}]*$`)
	yamlListItemRe       = regexp.MustCompile(`^([ \t\x{00a0}]*-[ \t\x{00a0}]+)(.+)$`)

	bearerAuthHeaderRe = regexp.MustCompile(`(?i)\b(?:proxy-)?authorization["']?[\s\x{00a0}]*:[\s\x{00a0}\["']*bearer[\s\x{00a0}]+([^\s\x{00a0}"'\[\]}),;<>]{8,})`)
	basicAuthHeaderRe  = regexp.MustCompile(`(?i)\b(?:proxy-)?authorization["']?[\s\x{00a0}]*:[\s\x{00a0}\["']*basic[\s\x{00a0}]+([^\s\x{00a0}"'\[\]}),;<>]{4,})`)
	otherAuthHeaderRe  = regexp.MustCompile(`(?i)\b(?:proxy-)?authorization(["']?)[\s\x{00a0}]*:([\s\x{00a0}\["']*)([a-z][a-z0-9_-]{1,31})[\s\x{00a0}]+`)
	cookieHeaderRe     = regexp.MustCompile(`(?i)\b(set-)?cookie(["']?)[ \t\x{00a0}]*:[ \t\x{00a0}]*(\[(?:[\s\x{00a0}]*["'])?|["']?)`)
	awsAccessKeyRe     = regexp.MustCompile(`(?:^|[^A-Z0-9])((?:AKIA|ASIA)[0-9A-Z]{16})`)
	curlUserRe         = regexp.MustCompile(`(?i)\bcurl\b[^\r\n;|&]*[ \t](?:-u|--user)(?:[ \t]+|=)(?:"([^"\r\n]*:[^"\r\n]+)"|'([^'\r\n]*:[^'\r\n]+)'|([^\s"'<>]*:[^\s"'<>]+))`)
	flowStartRe        = regexp.MustCompile(`(?i)(?:["']` + sensitiveField + `["']|\b` + sensitiveField + `\b)[ \t]*(?::=|=>|[:=])[ \t]*([{\[])`)
	kvKeyRe            = regexp.MustCompile(`(?i)(?:\\?"(` + sensitiveField + `)\\?"|'(` + sensitiveField + `)'|\b(` + sensitiveField + `)\b)(` + kvSep + `)`)
)

var basicAuthStopwords = map[string]struct{}{
	"auth":           {},
	"authentication": {},
	"credentials":    {},
	"denied":         {},
	"disabled":       {},
	"enabled":        {},
	"error":          {},
	"failed":         {},
	"false":          {},
	"header":         {},
	"invalid":        {},
	"missing":        {},
	"mode":           {},
	"none":           {},
	"null":           {},
	"only":           {},
	"required":       {},
	"scheme":         {},
	"true":           {},
	"unsupported":    {},
}

var authSchemeStopwords = map[string]struct{}{
	"are":      {},
	"check":    {},
	"denied":   {},
	"disabled": {},
	"enabled":  {},
	"error":    {},
	"failed":   {},
	"false":    {},
	"for":      {},
	"from":     {},
	"header":   {},
	"invalid":  {},
	"is":       {},
	"missing":  {},
	"mode":     {},
	"none":     {},
	"not":      {},
	"null":     {},
	"ok":       {},
	"only":     {},
	"policy":   {},
	"required": {},
	"true":     {},
	"type":     {},
	"using":    {},
	"via":      {},
	"was":      {},
	"with":     {},
}

var multiParamAuthSchemes = map[string]struct{}{
	"aws4-hmac-sha256": {},
	"digest":           {},
	"hoba":             {},
	"mutual":           {},
	"oauth":            {},
	"oauth2":           {},
	"scram-sha-256":    {},
	"vapid":            {},
}

var yamlMetadataKeys = map[string]struct{}{
	"account":            {},
	"alg":                {},
	"algorithm":          {},
	"apiversion":         {},
	"audience":           {},
	"backend":            {},
	"cluster":            {},
	"configmapname":      {},
	"context":            {},
	"count":              {},
	"defaultmode":        {},
	"deprecated":         {},
	"description":        {},
	"disabled":           {},
	"driver":             {},
	"enabled":            {},
	"endpoint":           {},
	"engine":             {},
	"env":                {},
	"environment":        {},
	"expiresin":          {},
	"expiry":             {},
	"file":               {},
	"filename":           {},
	"format":             {},
	"host":               {},
	"issuer":             {},
	"keyref":             {},
	"kind":               {},
	"length":             {},
	"maximum":            {},
	"maxlength":          {},
	"minimum":            {},
	"minlength":          {},
	"mode":               {},
	"mountpath":          {},
	"name":               {},
	"namespace":          {},
	"nullable":           {},
	"optional":           {},
	"path":               {},
	"pattern":            {},
	"port":               {},
	"profile":            {},
	"project":            {},
	"protocol":           {},
	"provider":           {},
	"readonly":           {},
	"ref":                {},
	"region":             {},
	"required":           {},
	"role":               {},
	"rolearn":            {},
	"scheme":             {},
	"scope":              {},
	"scopes":             {},
	"secretkeyref":       {},
	"secretname":         {},
	"secretref":          {},
	"serviceaccount":     {},
	"serviceaccountname": {},
	"size":               {},
	"source":             {},
	"subpath":            {},
	"summary":            {},
	"timeout":            {},
	"title":              {},
	"ttl":                {},
	"type":               {},
	"version":            {},
	"writeonly":          {},
}

var unquotedKVProseWords = map[string]struct{}{
	"active":      {},
	"boolean":     {},
	"clean":       {},
	"default":     {},
	"denied":      {},
	"disabled":    {},
	"empty":       {},
	"enabled":     {},
	"error":       {},
	"example":     {},
	"exists":      {},
	"expired":     {},
	"failed":      {},
	"false":       {},
	"format":      {},
	"found":       {},
	"header":      {},
	"integer":     {},
	"interface":   {},
	"invalid":     {},
	"length":      {},
	"mismatch":    {},
	"missing":     {},
	"none":        {},
	"null":        {},
	"number":      {},
	"object":      {},
	"optional":    {},
	"placeholder": {},
	"present":     {},
	"ready":       {},
	"required":    {},
	"reset":       {},
	"revoked":     {},
	"signature":   {},
	"status":      {},
	"string":      {},
	"struct":      {},
	"true":        {},
	"undefined":   {},
	"unknown":     {},
	"unset":       {},
	"valid":       {},
}

var rules = []rule{
	{
		category: "private_key",
		marker:   "[REDACTED:private_key]",
		mayMatch: func(s string) bool { return strings.Contains(s, "-----BEGIN ") },
		find:     findPrivateKeySpans,
	},
	{
		category: "sablecourt_token",
		marker:   "[REDACTED:sablecourt_token]",
		mayMatch: func(s string) bool { return strings.Contains(s, "sc_task_") || strings.Contains(s, "op_") },
		re:       regexp.MustCompile(`(?:^|[^A-Za-z0-9])(sc_task_[0-9a-fA-F]{16,}|op_[0-9a-fA-F]{64})`),
		group:    1,
	},
	{
		category: "anthropic_key",
		marker:   "[REDACTED:anthropic_key]",
		mayMatch: func(s string) bool { return strings.Contains(s, "sk-ant-") },
		re:       regexp.MustCompile(`(?:^|[^A-Za-z0-9])(sk-ant-[A-Za-z0-9_-]{16,})`),
		group:    1,
	},
	{
		category: "openai_key",
		marker:   "[REDACTED:openai_key]",
		mayMatch: func(s string) bool { return strings.Contains(s, "sk-") },
		re:       regexp.MustCompile(`(?:^|[^A-Za-z0-9])(sk-(?:proj-)?[A-Za-z0-9_-]{20,})`),
		group:    1,
	},
	{
		category: "stripe_key",
		marker:   "[REDACTED:stripe_key]",
		mayMatch: func(s string) bool { return strings.Contains(s, "sk_live_") || strings.Contains(s, "rk_live_") },
		re:       regexp.MustCompile(`(?:^|[^A-Za-z0-9])((?:sk|rk)_live_[0-9A-Za-z]{20,})`),
		group:    1,
	},
	{
		category: "slack_token",
		marker:   "[REDACTED:slack_token]",
		mayMatch: func(s string) bool { return strings.Contains(s, "xox") },
		re:       regexp.MustCompile(`(?:^|[^A-Za-z0-9])(xox[baprs]-[0-9A-Za-z-]{10,})`),
		group:    1,
	},
	{
		category: "github_token",
		marker:   "[REDACTED:github_token]",
		mayMatch: func(s string) bool {
			return strings.Contains(s, "ghp_") ||
				strings.Contains(s, "gho_") ||
				strings.Contains(s, "ghu_") ||
				strings.Contains(s, "ghs_") ||
				strings.Contains(s, "ghr_") ||
				strings.Contains(s, "github_pat_")
		},
		re: regexp.MustCompile(`(?:ghp|gho|ghu|ghs|ghr)_(?:[A-Za-z0-9]{20,}|[A-Za-z0-9]{1,19}\r?\n[A-Za-z0-9]{10,})|github_pat_(?:[A-Za-z0-9_]{20,}|[A-Za-z0-9_]{1,19}\r?\n[A-Za-z0-9_]{10,})`),
	},
	{
		category: "google_api_key",
		marker:   "[REDACTED:google_api_key]",
		mayMatch: func(s string) bool { return strings.Contains(s, "AIza") },
		re:       regexp.MustCompile(`(?:^|[^A-Za-z0-9])(AIza[0-9A-Za-z_-]{20,})`),
		group:    1,
	},
	{
		category: "aws_access_key",
		marker:   "[REDACTED:aws_access_key]",
		mayMatch: func(s string) bool { return strings.Contains(s, "AKIA") || strings.Contains(s, "ASIA") },
		find:     findAWSAccessKeySpans,
	},
	{
		category: "url_credential",
		marker:   "[REDACTED:url_credential]",
		mayMatch: func(s string) bool {
			return strings.IndexByte(s, '@') >= 0 && (strings.Contains(s, "://") || strings.Contains(s, `:\/\/`))
		},
		find: findURLCredentialSpans,
	},
	{
		category: "bearer_header",
		marker:   "[REDACTED:bearer_token]",
		mayMatch: func(s string) bool { return strings.IndexByte(s, ':') >= 0 },
		find:     findBearerHeaderSpans,
	},
	{
		category: "basic_auth_header",
		marker:   "[REDACTED:basic_auth]",
		mayMatch: func(s string) bool { return strings.IndexByte(s, ':') >= 0 },
		find:     findBasicAuthHeaderSpans,
	},
	{
		category: "auth_header",
		marker:   "[REDACTED:auth_header]",
		mayMatch: func(s string) bool { return strings.IndexByte(s, ':') >= 0 },
		find:     findOtherAuthHeaderSpans,
	},
	{
		category: "jwt",
		marker:   "[REDACTED:jwt]",
		mayMatch: func(s string) bool { return strings.Contains(s, "eyJ") && strings.IndexByte(s, '.') >= 0 },
		find:     findJWTSpans,
	},
	{
		category: "cookie_header",
		marker:   "[REDACTED:cookie]",
		mayMatch: func(s string) bool { return strings.IndexByte(s, ':') >= 0 && strings.IndexByte(s, '=') >= 0 },
		find:     findCookieHeaderSpans,
	},
	{
		category: "generic_secret_kv",
		marker:   "[REDACTED:secret]",
		mayMatch: func(s string) bool {
			return strings.IndexByte(s, ':') >= 0 &&
				(strings.IndexByte(s, '\n') >= 0 || strings.IndexByte(s, '\r') >= 0 ||
					strings.Contains(s, "\u2028") || strings.Contains(s, `\n`) ||
					strings.Contains(s, `\u000a`) || strings.Contains(s, `\u000A`))
		},
		find: findNestedYAMLSecretSpans,
	},
	{
		category: "generic_secret_kv",
		marker:   "[REDACTED:secret]",
		mayMatch: func(s string) bool {
			return (strings.IndexByte(s, '{') >= 0 || strings.IndexByte(s, '[') >= 0) &&
				(strings.IndexByte(s, ':') >= 0 || strings.IndexByte(s, '=') >= 0)
		},
		find: findFlowCollectionSecretSpans,
	},
	{
		category: "generic_secret_kv",
		marker:   "[REDACTED:secret]",
		mayMatch: func(s string) bool { return strings.IndexByte(s, '<') >= 0 && strings.Contains(s, "</") },
		re:       regexp.MustCompile(`(?i)<` + sensitiveField + `(?:\s[^<>]*)?>([^<\r\n]+)</` + sensitiveField + `>`),
		group:    1,
	},
	{
		category: "generic_secret_kv",
		marker:   "[REDACTED:secret]",
		mayMatch: func(s string) bool {
			return strings.IndexByte(s, ':') >= 0 && (strings.Contains(s, "curl") || strings.Contains(s, "CURL"))
		},
		find: findCurlUserSpans,
	},
	{
		category: "generic_secret_kv",
		marker:   "[REDACTED:secret]",
		mayMatch: func(s string) bool { return strings.Contains(s, "--") && strings.IndexByte(s, '"') >= 0 },
		re:       regexp.MustCompile(`(?i)(?:^|[\s(])--` + sensitiveField + `[ \t]+"((?:[^"\\\r\n]|\\.)+)"`),
		group:    1,
	},
	{
		category: "generic_secret_kv",
		marker:   "[REDACTED:secret]",
		mayMatch: func(s string) bool { return strings.Contains(s, "--") && strings.IndexByte(s, '\'') >= 0 },
		re:       regexp.MustCompile(`(?i)(?:^|[\s(])--` + sensitiveField + `[ \t]+'((?:[^'\\\r\n]|\\.)+)'`),
		group:    1,
	},
	{
		category: "generic_secret_kv",
		marker:   "[REDACTED:secret]",
		mayMatch: func(s string) bool { return strings.Contains(s, "--") },
		re:       regexp.MustCompile(`(?i)(?:^|[\s(])--` + sensitiveField + `[ \t]+([^\s"',;<>\[({-][^\s"',;<>]{5,})`),
		group:    1,
	},
	{
		category: "generic_secret_kv",
		marker:   "[REDACTED:secret]",
		mayMatch: func(s string) bool { return strings.IndexByte(s, ':') >= 0 || strings.IndexByte(s, '=') >= 0 },
		find:     findKVSpans,
	},
}

type Result struct {
	Text       string
	Hits       int
	Categories []string
}

// Text redacts sensitive credentials deterministically in linear time.
func Text(input string) Result {
	res, _ := textWithStats(input)
	return res
}

func textWithStats(input string) (Result, scanStats) {
	var stats scanStats
	if input == "" {
		return Result{}, stats
	}
	out := input
	totalHits := 0
	seen := make(map[string]struct{})

	hasNoise := mayContainNoise(out, &stats)
	var clean string
	var pos []int
	cleanReady := false

	for _, r := range rules {
		var hits int
		if hasNoise {
			if !cleanReady {
				clean, pos = stripNoise(out, &stats)
				cleanReady = true
			}
			if len(clean) < len(out) {
				out, hits = applySpans(out, r.findSpans(clean, &stats), pos, r.marker, &stats)
				if hits > 0 {
					totalHits += hits
					seen[r.category] = struct{}{}
					hasNoise = mayContainNoise(out, &stats)
					cleanReady = false
				}
			} else {
				hasNoise = false
			}
		}
		out, hits = applySpans(out, r.findSpans(out, &stats), nil, r.marker, &stats)
		if hits > 0 {
			totalHits += hits
			seen[r.category] = struct{}{}
			if hasNoise {
				hasNoise = mayContainNoise(out, &stats)
				cleanReady = false
			}
		}
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
	}, stats
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

func (r rule) findSpans(s string, stats *scanStats) []span {
	if r.mayMatch != nil && !r.mayMatch(s) {
		return nil
	}
	if r.find != nil {
		return r.find(s, stats)
	}
	if stats != nil {
		stats.bytesScanned += len(s)
	}
	if r.group == 0 {
		locs := r.re.FindAllStringIndex(s, -1)
		if len(locs) == 0 {
			return nil
		}
		spans := make([]span, len(locs))
		for i, loc := range locs {
			spans[i] = span{start: loc[0], end: loc[1]}
		}
		return spans
	}
	locs := r.re.FindAllStringSubmatchIndex(s, -1)
	if len(locs) == 0 {
		return nil
	}
	spans := make([]span, 0, len(locs))
	gStart := 2 * r.group
	gEnd := gStart + 1
	for _, loc := range locs {
		if loc[gStart] >= 0 && loc[gEnd] > loc[gStart] {
			spans = append(spans, span{start: loc[gStart], end: loc[gEnd]})
		}
	}
	return spans
}

func applySpans(out string, spans []span, pos []int, marker string, stats *scanStats) (string, int) {
	if len(spans) == 0 {
		return out, 0
	}
	merged := make([]span, 0, len(spans))
	for _, sp := range spans {
		if stats != nil {
			stats.scannerSteps++
		}
		origStart, origEnd := sp.start, sp.end
		if pos != nil {
			origStart = pos[sp.start]
			origEnd = pos[sp.end-1] + 1
		}
		if origStart >= origEnd {
			continue
		}
		if isMarker(out[origStart:origEnd]) {
			continue
		}
		if n := len(merged); n > 0 && origStart < merged[n-1].end {
			if origEnd > merged[n-1].end {
				merged[n-1].end = origEnd
			}
			continue
		}
		merged = append(merged, span{start: origStart, end: origEnd})
	}
	if len(merged) == 0 {
		return out, 0
	}
	var b strings.Builder
	b.Grow(len(out))
	cursor := 0
	for _, sp := range merged {
		b.WriteString(out[cursor:sp.start])
		b.WriteString(marker)
		cursor = sp.end
	}
	b.WriteString(out[cursor:])
	return b.String(), len(merged)
}

func isMarker(s string) bool {
	return matchMarkerPrefix(s, 0) == len(s) && len(s) > 0
}

// matchMarkerPrefix returns the end index if s[pos:] begins with "[REDACTED]" or
// "[REDACTED:<category>]", otherwise -1.
func matchMarkerPrefix(s string, pos int) int {
	if pos < 0 || pos >= len(s) || s[pos] != '[' {
		return -1
	}
	if strings.HasPrefix(s[pos:], "[REDACTED]") {
		return pos + len("[REDACTED]")
	}
	const prefix = "[REDACTED:"
	if !strings.HasPrefix(s[pos:], prefix) {
		return -1
	}
	i := pos + len(prefix)
	catStart := i
	for i < len(s) && s[i] != ']' {
		b := s[i]
		if (b < 'a' || b > 'z') && (b < '0' || b > '9') && b != '_' {
			return -1
		}
		i++
	}
	if i == catStart || i >= len(s) || s[i] != ']' {
		return -1
	}
	return i + 1
}

func mayContainNoise(s string, stats *scanStats) bool {
	for i := range len(s) {
		if stats != nil {
			stats.scannerSteps++
		}
		b := s[i]
		if b == 0 || b == 0x1b || b >= 0x80 {
			return true
		}
	}
	return false
}

// stripNoise removes invalid UTF-8 bytes, NUL bytes, literal U+FFFD, Unicode format
// runes (zero-width spaces, soft hyphen, BOM), and ANSI CSI escape sequences while
// preserving a byte-position map back to the original string.
func stripNoise(s string, stats *scanStats) (string, []int) {
	var b strings.Builder
	b.Grow(len(s))
	pos := make([]int, 0, len(s))
	for i := 0; i < len(s); {
		if stats != nil {
			stats.scannerSteps++
		}
		ch := s[i]
		if ch == 0 {
			i++
			continue
		}
		if ch == 0x1b && i+1 < len(s) && s[i+1] == '[' {
			j := i + 2
			for j < len(s) && s[j] >= 0x30 && s[j] <= 0x3f {
				if stats != nil {
					stats.scannerSteps++
				}
				j++
			}
			for j < len(s) && s[j] >= 0x20 && s[j] <= 0x2f {
				if stats != nil {
					stats.scannerSteps++
				}
				j++
			}
			if j < len(s) && s[j] >= 0x40 && s[j] <= 0x7e {
				i = j + 1
				continue
			}
		}
		if ch < 0x80 {
			b.WriteByte(ch)
			pos = append(pos, i)
			i++
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		if (r == utf8.RuneError && size == 1) || r == utf8.RuneError || unicode.Is(unicode.Cf, r) {
			i += size
			continue
		}
		for k := range size {
			b.WriteByte(s[i+k])
			pos = append(pos, i+k)
		}
		i += size
	}
	return b.String(), pos
}

func findAWSAccessKeySpans(s string, stats *scanStats) []span {
	if stats != nil {
		stats.bytesScanned += len(s)
	}
	locs := awsAccessKeyRe.FindAllStringSubmatchIndex(s, -1)
	if len(locs) == 0 {
		return nil
	}
	spans := make([]span, 0, len(locs))
	for _, loc := range locs {
		if stats != nil {
			stats.scannerSteps++
		}
		start, end := loc[2], loc[3]
		if end < len(s) && isUpperAlnum(s[end]) {
			continue
		}
		spans = append(spans, span{start: start, end: end})
	}
	return spans
}

func isUpperAlnum(b byte) bool {
	return (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9')
}

func findCurlUserSpans(s string, stats *scanStats) []span {
	if stats != nil {
		stats.bytesScanned += len(s)
	}
	locs := curlUserRe.FindAllStringSubmatchIndex(s, -1)
	if len(locs) == 0 {
		return nil
	}
	spans := make([]span, 0, len(locs))
	for _, loc := range locs {
		if stats != nil {
			stats.scannerSteps++
		}
		for g := 1; g <= 3; g++ {
			if loc[2*g] >= 0 && loc[2*g+1] > loc[2*g] {
				spans = append(spans, span{start: loc[2*g], end: loc[2*g+1]})
				break
			}
		}
	}
	return spans
}

func findBearerHeaderSpans(s string, stats *scanStats) []span {
	if stats != nil {
		stats.bytesScanned += len(s)
	}
	locs := bearerAuthHeaderRe.FindAllStringSubmatchIndex(s, -1)
	if len(locs) == 0 {
		return nil
	}
	spans := make([]span, 0, len(locs))
	for _, loc := range locs {
		if stats != nil {
			stats.scannerSteps++
		}
		start, end := loc[2], loc[3]
		if isAuthProseWord(s[start:end]) {
			continue
		}
		spans = append(spans, span{start: start, end: end})
	}
	return spans
}

func findBasicAuthHeaderSpans(s string, stats *scanStats) []span {
	if stats != nil {
		stats.bytesScanned += len(s)
	}
	locs := basicAuthHeaderRe.FindAllStringSubmatchIndex(s, -1)
	if len(locs) == 0 {
		return nil
	}
	spans := make([]span, 0, len(locs))
	for _, loc := range locs {
		if stats != nil {
			stats.scannerSteps++
		}
		start, end := loc[2], loc[3]
		if isAuthProseWord(s[start:end]) {
			continue
		}
		spans = append(spans, span{start: start, end: end})
	}
	return spans
}

func findOtherAuthHeaderSpans(s string, stats *scanStats) []span {
	if stats != nil {
		stats.bytesScanned += len(s)
	}
	locs := otherAuthHeaderRe.FindAllStringSubmatchIndex(s, -1)
	if len(locs) == 0 {
		return nil
	}
	var spans []span
	cursor := 0
	for _, loc := range locs {
		if stats != nil {
			stats.scannerSteps++
		}
		if loc[0] < cursor {
			continue
		}
		keyQuote := s[loc[2]:loc[3]]
		prefix := s[loc[4]:loc[5]]
		rawScheme := s[loc[6]:loc[7]]
		scheme := strings.ToLower(rawScheme)
		if scheme == "bearer" || scheme == "basic" {
			continue
		}
		if _, skip := authSchemeStopwords[scheme]; skip {
			continue
		}
		if isProseStatusWord(rawScheme, true) {
			continue
		}
		valStart := loc[1]
		if valStart >= len(s) {
			continue
		}
		if _, multi := multiParamAuthSchemes[scheme]; multi {
			quoteChar := enclosingHeaderQuote(s, loc[0], keyQuote, prefix)
			valEnd := scanHeaderValueEnd(s, valStart, quoteChar, false, stats)
			cursor = valEnd
			valEnd = trimRightSpaceIndex(s, valStart, valEnd)
			if valEnd > valStart {
				spans = append(spans, span{start: valStart, end: valEnd})
			}
			continue
		}
		valEnd := valStart
		for valEnd < len(s) {
			if stats != nil {
				stats.scannerSteps++
			}
			if matchMarkerPrefix(s, valEnd) > valEnd {
				break
			}
			b := s[valEnd]
			if b == ' ' || b == '\t' || b == '\r' || b == '\n' ||
				b == '"' || b == '\'' || b == '[' || b == ']' ||
				b == '}' || b == ')' || b == ',' || b == ';' || b == '<' || b == '>' {
				break
			}
			if b == 0xc2 && valEnd+1 < len(s) && s[valEnd+1] == 0xa0 {
				break
			}
			valEnd++
		}
		cursor = valEnd
		if valEnd-valStart >= 8 && !isAuthProseWord(s[valStart:valEnd]) {
			spans = append(spans, span{start: valStart, end: valEnd})
		}
	}
	return spans
}

func enclosingHeaderQuote(s string, matchStart int, keyQuote, prefix string) byte {
	if strings.IndexByte(prefix, '"') >= 0 {
		return '"'
	}
	if strings.IndexByte(prefix, '\'') >= 0 {
		return '\''
	}
	if keyQuote == "" && matchStart > 0 && (s[matchStart-1] == '\'' || s[matchStart-1] == '"') {
		return s[matchStart-1]
	}
	return 0
}

func findCookieHeaderSpans(s string, stats *scanStats) []span {
	if stats != nil {
		stats.bytesScanned += len(s)
	}
	locs := cookieHeaderRe.FindAllStringSubmatchIndex(s, -1)
	if len(locs) == 0 {
		return nil
	}
	var spans []span
	cursor := 0
	for _, loc := range locs {
		if stats != nil {
			stats.scannerSteps++
		}
		if loc[0] < cursor {
			continue
		}
		isSetCookie := loc[2] >= 0
		keyQuote := s[loc[4]:loc[5]]
		prefix := s[loc[6]:loc[7]]
		inBracket := strings.IndexByte(prefix, '[') >= 0
		quoteChar := enclosingHeaderQuote(s, loc[0], keyQuote, prefix)
		valStart := loc[1]

		if inBracket && quoteChar != 0 {
			// JSON or Python array of cookie strings: redact every quoted element
			// containing '=' without stopping at ';' inside an element.
			end := scanQuotedHeaderArray(s, loc[7]-1, quoteChar, &spans, stats)
			cursor = end
			continue
		}

		// Inside an unquoted array/map bracket (e.g. Go fmt.Sprint of http.Header),
		// do not stop at ';' so every cookie value in the slice is scrubbed.
		stopSemi := isSetCookie && !inBracket
		valEnd := scanHeaderValueEnd(s, valStart, quoteChar, stopSemi, stats)
		cursor = valEnd
		valEnd = trimRightSpaceIndex(s, valStart, valEnd)
		if valEnd <= valStart {
			continue
		}
		val := s[valStart:valEnd]
		if strings.IndexByte(val, '=') < 0 {
			continue
		}
		spans = append(spans, span{start: valStart, end: valEnd})
	}
	return spans
}

func scanQuotedHeaderArray(s string, firstQuotePos int, quoteChar byte, spans *[]span, stats *scanStats) int {
	i := firstQuotePos
	for i < len(s) {
		if stats != nil {
			stats.scannerSteps++
		}
		b := s[i]
		if b == ']' || b == '}' {
			return i
		}
		if b == quoteChar {
			qEnd := scanQuotedLiteral(s, i, quoteChar, stats)
			elemStart := i + 1
			elemEnd := qEnd
			if elemEnd > elemStart {
				elem := s[elemStart:elemEnd]
				if strings.IndexByte(elem, '=') >= 0 && !isMarker(elem) {
					*spans = append(*spans, span{start: elemStart, end: elemEnd})
				}
			}
			if qEnd < len(s) && s[qEnd] == quoteChar {
				i = qEnd + 1
			} else {
				return qEnd
			}
			continue
		}
		if b == ' ' || b == '\t' || b == '\r' || b == '\n' || b == ',' {
			i++
			continue
		}
		return i
	}
	return i
}

func scanHeaderValueEnd(s string, start int, quoteChar byte, stopAtSemicolon bool, stats *scanStats) int {
	i := start
	for i < len(s) {
		if stats != nil {
			stats.scannerSteps++
		}
		if mEnd := matchMarkerPrefix(s, i); mEnd > i {
			i = mEnd
			continue
		}
		b := s[i]
		if b == '\r' || b == '\n' || b == ']' || b == '}' {
			break
		}
		if stopAtSemicolon && b == ';' {
			break
		}
		if quoteChar != 0 {
			if b == '\\' && i+1 < len(s) {
				i += 2
				continue
			}
			if b == quoteChar {
				break
			}
		}
		i++
	}
	return i
}

func findURLCredentialSpans(s string, stats *scanStats) []span {
	if stats != nil {
		stats.bytesScanned += len(s)
	}
	var spans []span
	// Cache the enclosing non-whitespace token boundary and fallback '@' state
	// so repeated schemes inside one contiguous token stay strictly O(n).
	runEnd := -1
	runLastAt := -1
	runFallbackDead := false

	for i := 0; i < len(s); {
		if stats != nil {
			stats.scannerSteps++
		}
		colon := strings.IndexByte(s[i:], ':')
		if colon < 0 {
			break
		}
		cPos := i + colon
		var sepLen int
		if strings.HasPrefix(s[cPos:], "://") {
			sepLen = 3
		} else if strings.HasPrefix(s[cPos:], `:\/\/`) {
			sepLen = 5
		} else {
			i = cPos + 1
			continue
		}

		// Verify valid URL scheme immediately before ':'.
		schemeStart := cPos
		for schemeStart > 0 && isSchemeChar(s[schemeStart-1]) {
			schemeStart--
			if stats != nil {
				stats.scannerSteps++
			}
		}
		if schemeStart == cPos || !isASCIIAlpha(s[schemeStart]) {
			i = cPos + sepLen
			continue
		}

		escapedSlash := sepLen == 5
		singleQuoted := schemeStart > 0 && s[schemeStart-1] == '\''
		afterSep := cPos + sepLen

		if afterSep >= runEnd {
			runEnd, runLastAt = scanURLTokenEnd(s, afterSep, stats)
			runFallbackDead = false
		}
		urlEnd := runEnd
		if singleQuoted {
			// Two single-quoted URLs are separated by their closing/opening quotes,
			// so bounding by the first '\'' stays disjoint and O(n).
			for q := afterSep; q < urlEnd; q++ {
				if stats != nil {
					stats.scannerSteps++
				}
				if s[q] == '\'' {
					urlEnd = q
					break
				}
			}
		}
		if runLastAt < afterSep {
			i = runEnd
			continue
		}
		rest := s[afterSep:urlEnd]
		if mEnd := matchMarkerPrefix(rest, 0); mEnd > 0 {
			i = afterSep + mEnd
			continue
		}
		if isLeadingIPv6Host(rest) {
			i = afterSep
			continue
		}

		firstSlash := findURLFirstDelim(rest, escapedSlash, stats)
		firstSoft := findURLFirstSoftPunct(rest[:firstSlash], stats)
		cleanHead := rest[:firstSoft]

		// Case 1: clean userinfo before any '/', '?', '#', or soft punctuation.
		if atIdx := strings.LastIndexByte(cleanHead, '@'); atIdx > 0 {
			userinfo := cleanHead[:atIdx]
			if !isMarker(userinfo) && hasURLCredentialShape(userinfo) {
				spans = append(spans, span{start: afterSep, end: afterSep + atIdx})
			}
			i = afterSep + atIdx + 1
			continue
		}

		colonInClean := strings.IndexByte(cleanHead, ':')
		hasUserPassPrefix := colonInClean >= 0 && !(colonInClean > 0 && isPortDigits(cleanHead[colonInClean+1:]))

		// Case 2: password containing ',', ';', ')', ']', or '}' before the first slash.
		if hasUserPassPrefix && firstSoft < firstSlash {
			if atIdx := findURLHostAt(rest, firstSoft, firstSlash, stats); atIdx > 0 {
				userinfo := rest[:atIdx]
				if !isMarker(userinfo) && hasURLCredentialShape(userinfo) {
					spans = append(spans, span{start: afterSep, end: afterSep + atIdx})
				}
				i = afterSep + atIdx + 1
				continue
			}
		}

		// Case 3: unencoded '/', '?', or '#' in password (e.g. redis://:pa/ss@host).
		if hasUserPassPrefix && firstSlash < len(rest) && !runFallbackDead {
			if atIdx := findURLHostAt(rest, firstSlash, len(rest), stats); atIdx > 0 {
				userinfo := rest[:atIdx]
				if !isMarker(userinfo) && hasURLCredentialShape(userinfo) {
					spans = append(spans, span{start: afterSep, end: afterSep + atIdx})
				}
				i = afterSep + atIdx + 1
				continue
			}
			if !singleQuoted {
				runFallbackDead = true
			}
		}

		i = afterSep
	}
	return spans
}

func scanURLTokenEnd(s string, start int, stats *scanStats) (end int, lastAt int) {
	lastAt = -1
	i := start
	for i < len(s) {
		if stats != nil {
			stats.scannerSteps++
		}
		b := s[i]
		if b == ' ' || b == '\t' || b == '\r' || b == '\n' ||
			b == '"' || b == '<' || b == '>' {
			break
		}
		if b == 0xc2 && i+1 < len(s) && s[i+1] == 0xa0 {
			break
		}
		if b == '@' {
			lastAt = i
		}
		i++
	}
	return i, lastAt
}

func isLeadingIPv6Host(rest string) bool {
	if len(rest) < 4 || rest[0] != '[' {
		return false
	}
	colons := 0
	j := 1
	for j < len(rest) && rest[j] != ']' {
		b := rest[j]
		if b == ':' {
			colons++
		} else if !((b >= '0' && b <= '9') || (b >= 'a' && b <= 'f') || (b >= 'A' && b <= 'F') || b == '.' || b == '%') {
			return false
		}
		j++
	}
	if colons == 0 || j >= len(rest) || rest[j] != ']' {
		return false
	}
	next := j + 1
	if next >= len(rest) {
		return true
	}
	b := rest[next]
	return b == '/' || b == '?' || b == '#' || b == ':' || b == '\\' ||
		b == ',' || b == ';' || b == ')' || b == ']' || b == '}'
}

func findURLFirstDelim(rest string, escapedSlash bool, stats *scanStats) int {
	for j := range len(rest) {
		if stats != nil {
			stats.scannerSteps++
		}
		b := rest[j]
		if b == '/' || b == '?' || b == '#' {
			return j
		}
		if escapedSlash && b == '\\' && j+1 < len(rest) && rest[j+1] == '/' {
			return j
		}
	}
	return len(rest)
}

func findURLFirstSoftPunct(head string, stats *scanStats) int {
	for j := 0; j < len(head); {
		if stats != nil {
			stats.scannerSteps++
		}
		if mEnd := matchMarkerPrefix(head, j); mEnd > j {
			j = mEnd
			continue
		}
		switch head[j] {
		case ',', ';', ')', ']', '}':
			return j
		}
		j++
	}
	return len(head)
}

func findURLHostAt(rest string, from, limit int, stats *scanStats) int {
	atIdx := -1
	for j := from; j < limit; j++ {
		if stats != nil {
			stats.scannerSteps++
		}
		if rest[j] == '@' && j > 0 && rest[j-1] != '/' && j+1 < len(rest) && isURLHostStart(rest[j+1]) {
			atIdx = j
			afterHost := j + 1
			for afterHost < limit {
				c := rest[afterHost]
				if c == '/' || c == '?' || c == '#' || c == '@' || c == '\\' ||
					c == ',' || c == ';' || c == ')' || c == ']' || c == '}' {
					break
				}
				afterHost++
			}
			if afterHost >= limit || rest[afterHost] != '@' {
				break
			}
		}
	}
	return atIdx
}

func isSchemeChar(b byte) bool {
	return isASCIIAlpha(b) || (b >= '0' && b <= '9') || b == '+' || b == '.' || b == '-'
}

func isASCIIAlpha(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z')
}

func isPortDigits(s string) bool {
	if len(s) == 0 || len(s) > 5 {
		return false
	}
	for i := range len(s) {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

func isURLHostStart(b byte) bool {
	return isASCIIAlpha(b) || (b >= '0' && b <= '9') || b == '['
}

func hasURLCredentialShape(userinfo string) bool {
	if !strings.Contains(userinfo, "[REDACTED") {
		return strings.IndexByte(userinfo, ':') >= 0 || looksLikeURLUserToken(userinfo)
	}
	// Strip existing redaction markers before checking for ':' or token length so
	// inserting "[REDACTED:secret]" inside userinfo cannot trigger a second pass.
	var b strings.Builder
	b.Grow(len(userinfo))
	for i := 0; i < len(userinfo); {
		if mEnd := matchMarkerPrefix(userinfo, i); mEnd > i {
			i = mEnd
			continue
		}
		b.WriteByte(userinfo[i])
		i++
	}
	stripped := b.String()
	return strings.IndexByte(stripped, ':') >= 0 || looksLikeURLUserToken(stripped)
}

func looksLikeURLUserToken(u string) bool {
	if len(u) >= 16 {
		return true
	}
	if strings.HasPrefix(u, "ghp_") || strings.HasPrefix(u, "gho_") || strings.HasPrefix(u, "sk-") || strings.HasPrefix(u, "tok_") {
		return true
	}
	if len(u) >= 10 {
		digits := 0
		for i := range len(u) {
			if u[i] >= '0' && u[i] <= '9' {
				digits++
			}
		}
		return digits >= 2
	}
	return false
}

func findJWTSpans(s string, stats *scanStats) []span {
	var spans []span
	for i := 0; i < len(s); {
		if stats != nil {
			stats.scannerSteps++
		}
		idx := strings.Index(s[i:], "eyJ")
		if idx < 0 {
			break
		}
		start := i + idx
		seg1End := scanBase64Seg(s, start, stats)
		seg1 := s[start:seg1End]
		if len(seg1) < 9 || !isJWTJSONSegment(seg1) {
			i = max(start+3, seg1End)
			continue
		}

		afterDot1, ok := consumeJWTDot(s, seg1End, stats)
		if !ok {
			i = seg1End
			continue
		}

		seg2End := scanBase64Seg(s, afterDot1, stats)
		seg2 := s[afterDot1:seg2End]

		afterDot2, hasDot2 := consumeJWTDot(s, seg2End, stats)
		if !hasDot2 {
			// Two-segment JWT: require payload segment to also be base64 JSON ("eyJ...").
			if len(seg2) >= 9 && strings.HasPrefix(seg2, "eyJ") && isJWTJSONSegment(seg2) {
				spans = append(spans, span{start: start, end: seg2End})
				i = seg2End
				continue
			}
			i = seg1End
			continue
		}

		seg3End := scanBase64Seg(s, afterDot2, stats)
		seg3Len := seg3End - afterDot2

		// Unsigned JWT with empty third segment (alg=none): "eyJ....eyJ....".
		if seg3Len == 0 {
			if len(seg2) >= 9 && strings.HasPrefix(seg2, "eyJ") {
				// Include the trailing dot immediately after seg2 (not trailing whitespace after dot).
				dotPos := seg2End
				for dotPos < afterDot2 && s[dotPos] != '.' {
					dotPos++
				}
				spans = append(spans, span{start: start, end: dotPos + 1})
				i = dotPos + 1
				continue
			}
			i = seg1End
			continue
		}

		if len(seg2) < 6 && len(seg2) != 0 {
			i = seg1End
			continue
		}
		if seg3Len < 6 {
			i = seg1End
			continue
		}

		// Handle optional line-wrapped signature continuation (e.g. c2lnbmF0\ndXJlMTIz).
		end := extendWrappedJWTSeg(s, seg3End, stats)

		// Handle 5-segment JWE (header.encKey.iv.ciphertext.tag).
		if afterDot3, ok3 := consumeJWTDot(s, end, stats); ok3 {
			seg4End := scanBase64Seg(s, afterDot3, stats)
			if seg4End-afterDot3 >= 4 {
				if afterDot4, ok4 := consumeJWTDot(s, seg4End, stats); ok4 {
					seg5End := scanBase64Seg(s, afterDot4, stats)
					if seg5End-afterDot4 >= 4 {
						end = extendWrappedJWTSeg(s, seg5End, stats)
					}
				}
			}
		} else if len(seg2) == 0 {
			// Empty seg2 is only valid for 5-segment JWE direct encryption.
			i = seg1End
			continue
		}

		spans = append(spans, span{start: start, end: end})
		i = end
	}
	return spans
}

func scanBase64Seg(s string, start int, stats *scanStats) int {
	i := start
	for i < len(s) && isB64Char(s[i]) {
		i++
		if stats != nil {
			stats.scannerSteps++
		}
	}
	eq := 0
	for i < len(s) && s[i] == '=' && eq < 2 {
		i++
		eq++
	}
	return i
}

func consumeJWTDot(s string, pos int, stats *scanStats) (int, bool) {
	i := pos
	for i < len(s) && (s[i] == ' ' || s[i] == '\t' || s[i] == '\r' || s[i] == '\n') {
		i++
		if stats != nil {
			stats.scannerSteps++
		}
	}
	if i >= len(s) || s[i] != '.' {
		return pos, false
	}
	i++
	for i < len(s) && (s[i] == ' ' || s[i] == '\t' || s[i] == '\r' || s[i] == '\n') {
		i++
		if stats != nil {
			stats.scannerSteps++
		}
	}
	return i, true
}

func extendWrappedJWTSeg(s string, end int, stats *scanStats) int {
	for {
		j := end
		if j < len(s) && s[j] == '\r' {
			j++
		}
		if j >= len(s) || s[j] != '\n' {
			return end
		}
		j++
		if j >= len(s) || !isB64Char(s[j]) {
			return end
		}
		contEnd := scanBase64Seg(s, j, stats)
		if contEnd-j < 4 {
			return end
		}
		// Only treat the next line as a wrapped base64 continuation if it is a standalone
		// base64 chunk (not a key=val or key: val line or prose sentence).
		if contEnd < len(s) && s[contEnd] != '\r' && s[contEnd] != '\n' && s[contEnd] != '"' && s[contEnd] != '\'' {
			return end
		}
		chunk := s[j:contEnd]
		if !hasMixedOrDigitB64(chunk) {
			return end
		}
		end = contEnd
	}
}

func hasMixedOrDigitB64(s string) bool {
	hasUpper, hasLower, hasDigit, hasSym := false, false, false, false
	for i := range len(s) {
		b := s[i]
		switch {
		case b >= 'A' && b <= 'Z':
			hasUpper = true
		case b >= 'a' && b <= 'z':
			hasLower = true
		case b >= '0' && b <= '9':
			hasDigit = true
		case b == '_' || b == '-' || b == '+' || b == '/' || b == '=':
			hasSym = true
		}
	}
	return (hasUpper && hasLower) || hasDigit || hasSym
}

func isB64Char(b byte) bool {
	return (b >= 'A' && b <= 'Z') ||
		(b >= 'a' && b <= 'z') ||
		(b >= '0' && b <= '9') ||
		b == '_' || b == '-' || b == '+' || b == '/'
}

func isJWTJSONSegment(seg string) bool {
	raw := strings.TrimRight(seg, "=")
	if len(raw) > 512 {
		raw = raw[:512]
		raw = raw[:len(raw)-(len(raw)%4)]
	}
	if len(raw) < 4 {
		return false
	}
	var dec []byte
	var err error
	if strings.ContainsAny(raw, "+/") {
		dec, err = base64.RawStdEncoding.DecodeString(raw)
	} else {
		dec, err = base64.RawURLEncoding.DecodeString(raw)
	}
	if err != nil || len(dec) < 4 || dec[0] != '{' || dec[1] != '"' {
		return false
	}
	hasColon := false
	for _, b := range dec {
		if b == ':' {
			hasColon = true
		}
		if b < 0x20 && b != '\t' && b != '\n' && b != '\r' {
			return false
		}
		if b >= 0x7f {
			return false
		}
	}
	return hasColon
}

func findPrivateKeySpans(s string, stats *scanStats) []span {
	if stats != nil {
		stats.bytesScanned += len(s)
	}
	headers := privateKeyHeader.FindAllStringSubmatchIndex(s, -1)
	if len(headers) == 0 {
		return nil
	}

	// Pre-index all PEM footers once so matching is O(n) even on adversarial inputs.
	footersByLabel := make(map[string][][2]int)
	if strings.Contains(s, "-----END ") {
		if stats != nil {
			stats.bytesScanned += len(s)
		}
		for _, fl := range privateKeyFooter.FindAllStringSubmatchIndex(s, -1) {
			if stats != nil {
				stats.scannerSteps++
			}
			lbl := s[fl[2]:fl[3]]
			footersByLabel[lbl] = append(footersByLabel[lbl], [2]int{fl[0], fl[1]})
		}
	}
	footerCursor := make(map[string]int, len(footersByLabel))

	var spans []span
	offset := 0
	for hIdx, loc := range headers {
		if stats != nil {
			stats.scannerSteps++
		}
		start := loc[0]
		if start < offset {
			continue
		}
		afterHeader := loc[1]
		label := s[loc[2]:loc[3]]

		// Advance footer cursor past afterHeader.
		flist := footersByLabel[label]
		fc := footerCursor[label]
		for fc < len(flist) && flist[fc][0] < afterHeader {
			fc++
		}
		footerCursor[label] = fc

		nextHeaderStart := len(s)
		if hIdx+1 < len(headers) {
			nextHeaderStart = headers[hIdx+1][0]
		}

		if fc < len(flist) && flist[fc][0] < nextHeaderStart {
			end := flist[fc][1]
			spans = append(spans, span{start: start, end: end})
			offset = end
			continue
		}

		// Truncated or mismatched PEM block: scan forward only through PEM body lines.
		end := scanTruncatedPEMEnd(s, afterHeader, stats)
		spans = append(spans, span{start: start, end: end})
		offset = end
	}
	return spans
}

func scanTruncatedPEMEnd(s string, afterHeader int, stats *scanStats) int {
	if afterHeader >= len(s) {
		return afterHeader
	}
	rest := s[afterHeader:]
	// JSON-escaped or double-escaped PEM block: fail closed inside the enclosing
	// string without eating text outside it.
	doubleEscaped := strings.HasPrefix(rest, `\\n`) || strings.HasPrefix(rest, `\\r\\n`) ||
		strings.HasPrefix(rest, `\\u000a`) || strings.HasPrefix(rest, `\\u000A`)
	singleEscaped := !doubleEscaped && (strings.HasPrefix(rest, `\n`) || strings.HasPrefix(rest, `\r\n`) ||
		strings.HasPrefix(rest, `\u000a`) || strings.HasPrefix(rest, `\u000A`))
	if doubleEscaped || singleEscaped {
		pos := afterHeader
		for pos < len(s) {
			if stats != nil {
				stats.scannerSteps++
			}
			if s[pos] == '\r' || s[pos] == '\n' || s[pos] == '"' {
				break
			}
			if s[pos] == '\\' && pos+1 < len(s) {
				if doubleEscaped && s[pos+1] == '"' {
					break
				}
				pos += 2
				continue
			}
			pos++
		}
		return pos
	}

	pos := afterHeader
	for pos < len(s) && (s[pos] == ' ' || s[pos] == '\t') {
		if stats != nil {
			stats.scannerSteps++
		}
		pos++
	}
	if pos >= len(s) {
		return afterHeader
	}
	if s[pos] == '\r' || s[pos] == '\n' {
		// Standalone unclosed PEM header followed by newline: fail closed to end of input.
		return len(s)
	}

	// Non-whitespace follows on the same line: stop right after the header only
	// when the trailing text starts with a quote or a non-base64 word (such as
	// `grep '-----BEGIN PRIVATE KEY-----' *.pem`). Otherwise scrub the inline body.
	if s[pos] == '\'' || s[pos] == '"' {
		return afterHeader
	}

	glued := pos == afterHeader
	lastBodyEnd := afterHeader
	cur := pos
	for cur < len(s) && s[cur] != '\r' && s[cur] != '\n' {
		tokStart := cur
		for cur < len(s) && s[cur] != ' ' && s[cur] != '\t' && s[cur] != '\r' && s[cur] != '\n' && s[cur] != '"' && s[cur] != '\'' {
			if stats != nil {
				stats.scannerSteps++
			}
			cur++
		}
		tok := s[tokStart:cur]
		if !isPEMBodyToken(tok, glued && tokStart == pos) {
			return lastBodyEnd
		}
		lastBodyEnd = cur
		if cur < len(s) && (s[cur] == '"' || s[cur] == '\'') {
			return lastBodyEnd
		}
		for cur < len(s) && (s[cur] == ' ' || s[cur] == '\t') {
			if stats != nil {
				stats.scannerSteps++
			}
			cur++
		}
	}
	// Entire remainder of the header line was PEM body material: fail closed to end of input.
	return len(s)
}

func isPEMBodyToken(tok string, glued bool) bool {
	if strings.HasPrefix(tok, "Proc-Type:") || strings.HasPrefix(tok, "DEK-Info:") {
		return true
	}
	if len(tok) < 4 {
		return false
	}
	hasSignal := false
	for i := range len(tok) {
		b := tok[i]
		switch {
		case b >= 'A' && b <= 'Z':
			hasSignal = true
		case b >= 'a' && b <= 'z':
		case b >= '0' && b <= '9', b == '+', b == '/', b == '=':
			hasSignal = true
		default:
			return false
		}
	}
	return glued || hasSignal || len(tok) >= 16
}

type yamlScanState struct {
	secretIndent     int
	headerJustOpened bool
	inSameIndentList bool
	inBlockScalar    bool
	blockIndent      int
	blockStart       int
	blockEnd         int
	plainStart       int
	plainEnd         int
	escapedJSON      bool
}

func newYAMLScanState(escapedJSON bool) yamlScanState {
	return yamlScanState{
		secretIndent: -1,
		blockIndent:  -1,
		blockStart:   -1,
		blockEnd:     -1,
		plainStart:   -1,
		plainEnd:     -1,
		escapedJSON:  escapedJSON,
	}
}

func (st *yamlScanState) flushBlock(spans *[]span) {
	if st.blockStart >= 0 && st.blockEnd > st.blockStart {
		*spans = append(*spans, span{start: st.blockStart, end: st.blockEnd})
	}
	st.inBlockScalar = false
	st.blockIndent = -1
	st.blockStart = -1
	st.blockEnd = -1
}

func (st *yamlScanState) flushPlain(spans *[]span) {
	if st.plainStart >= 0 && st.plainEnd > st.plainStart {
		*spans = append(*spans, span{start: st.plainStart, end: st.plainEnd})
	}
	st.plainStart = -1
	st.plainEnd = -1
}

func (st *yamlScanState) finish(spans *[]span) {
	if st.inBlockScalar {
		st.flushBlock(spans)
	}
	st.flushPlain(spans)
}

func (st *yamlScanState) processLine(line string, lineAbsStart int, spans *[]span, stats *scanStats) {
	if stats != nil {
		stats.scannerSteps++
	}
	indent, contentOffset, isEmpty, isComment := measureIndent(line)
	contentEnd := len(line)

	if st.inBlockScalar {
		if isEmpty {
			return
		}
		if indent > st.blockIndent {
			if st.blockStart < 0 {
				st.blockStart = lineAbsStart + contentOffset
			}
			st.blockEnd = trimRightSpaceIndex(line, contentOffset, contentEnd) + lineAbsStart
			return
		}
		st.flushBlock(spans)
	}

	if isEmpty || isComment {
		return
	}

	if st.secretIndent >= 0 {
		if indent < st.secretIndent {
			st.flushPlain(spans)
			st.secretIndent = -1
			st.headerJustOpened = false
			st.inSameIndentList = false
		} else if indent == st.secretIndent {
			if (st.headerJustOpened || st.inSameIndentList) && yamlListItemRe.MatchString(line) {
				if km := yamlKeyLineRe.FindStringSubmatchIndex(line); km != nil {
					childKey := normalizeKeyName(line[km[4]:km[5]])
					valRelStart := km[6]
					valRelEnd := trimRightSpaceIndex(line, valRelStart, km[7])
					val := line[valRelStart:valRelEnd]
					switch {
					case val == "" || strings.HasPrefix(val, "#"):
					case yamlBlockIndicatorRe.MatchString(val):
						if !isYAMLMetadataKey(childKey) {
							st.inBlockScalar = true
							st.blockIndent = indent
							st.blockStart = -1
							st.blockEnd = -1
						}
					default:
						if !isYAMLMetadataKey(childKey) && !isMarker(val) &&
							(val[0] == '"' || val[0] == '\'' || !isYAMLNonSecretScalar(val)) {
							if sp, ok := yamlValueSpan(lineAbsStart+valRelStart, val, st.escapedJSON); ok {
								*spans = append(*spans, sp)
							}
						}
					}
					st.headerJustOpened = false
					st.inSameIndentList = true
					return
				}
				m := yamlListItemRe.FindStringSubmatchIndex(line)
				valRelStart := m[4]
				valRelEnd := trimRightSpaceIndex(line, valRelStart, m[5])
				val := line[valRelStart:valRelEnd]
				if val != "" && !strings.HasPrefix(val, "#") && !isMarker(val) {
					if sp, ok := yamlValueSpan(lineAbsStart+valRelStart, val, st.escapedJSON); ok {
						*spans = append(*spans, sp)
					}
				}
				st.headerJustOpened = false
				st.inSameIndentList = true
				return
			}
			if st.headerJustOpened && !yamlKeyLineRe.MatchString(line) && !yamlListItemRe.MatchString(line) {
				valEnd := trimRightSpaceIndex(line, contentOffset, contentEnd)
				val := line[contentOffset:valEnd]
				if val != "" && !isMarker(val) && !isYAMLNonSecretScalar(val) {
					if sp, ok := yamlValueSpan(lineAbsStart+contentOffset, val, st.escapedJSON); ok {
						*spans = append(*spans, sp)
					}
				}
				st.headerJustOpened = false
				st.secretIndent = -1
				return
			}
			st.flushPlain(spans)
			st.secretIndent = -1
			st.headerJustOpened = false
			st.inSameIndentList = false
		} else {
			st.headerJustOpened = false
			st.inSameIndentList = false
		}
	}

	if st.secretIndent < 0 {
		if m := yamlSensitiveHeaderRe.FindStringSubmatchIndex(line); m != nil {
			st.secretIndent = visualWidth(line[:m[3]])
			st.headerJustOpened = true
			st.inSameIndentList = false
			if m[4] >= 0 {
				st.inBlockScalar = true
				st.blockIndent = st.secretIndent
				st.blockStart = -1
				st.blockEnd = -1
				st.headerJustOpened = false
			}
		} else if m := yamlLogPrefixedHeaderRe.FindStringSubmatchIndex(line); m != nil {
			st.secretIndent = 0
			st.headerJustOpened = true
			st.inSameIndentList = false
			if m[2] >= 0 {
				st.inBlockScalar = true
				st.blockIndent = 0
				st.blockStart = -1
				st.blockEnd = -1
				st.headerJustOpened = false
			}
		}
		return
	}

	// Inside a nested secret YAML block (indent > secretIndent).
	trimmedEnd := trimRightSpaceIndex(line, contentOffset, contentEnd)
	if isMarker(line[contentOffset:trimmedEnd]) {
		st.flushPlain(spans)
		return
	}
	if m := yamlKeyLineRe.FindStringSubmatchIndex(line); m != nil {
		st.flushPlain(spans)
		childKey := normalizeKeyName(line[m[4]:m[5]])
		valRelStart := m[6]
		valRelEnd := trimRightSpaceIndex(line, valRelStart, m[7])
		val := line[valRelStart:valRelEnd]
		switch {
		case val == "" || strings.HasPrefix(val, "#"):
			// Nested mapping header (e.g. "prod:"); keep header and stay inside block.
		case yamlBlockIndicatorRe.MatchString(val):
			if !isYAMLMetadataKey(childKey) {
				st.inBlockScalar = true
				st.blockIndent = indent
				st.blockStart = -1
				st.blockEnd = -1
			}
		default:
			if isYAMLMetadataKey(childKey) {
				return
			}
			if val[0] != '"' && val[0] != '\'' && isYAMLNonSecretScalar(val) {
				return
			}
			if sp, ok := yamlValueSpan(lineAbsStart+valRelStart, val, st.escapedJSON); ok {
				*spans = append(*spans, sp)
			}
		}
	} else if m := yamlListItemRe.FindStringSubmatchIndex(line); m != nil {
		st.flushPlain(spans)
		valRelStart := m[4]
		valRelEnd := trimRightSpaceIndex(line, valRelStart, m[5])
		val := line[valRelStart:valRelEnd]
		if val != "" && !strings.HasPrefix(val, "#") && !isYAMLNonSecretScalar(val) {
			if sp, ok := yamlValueSpan(lineAbsStart+valRelStart, val, st.escapedJSON); ok {
				*spans = append(*spans, sp)
			}
		}
	} else {
		valEnd := trimRightSpaceIndex(line, contentOffset, contentEnd)
		if valEnd > contentOffset {
			if st.plainStart < 0 {
				st.plainStart = lineAbsStart + contentOffset
			}
			st.plainEnd = lineAbsStart + valEnd
		}
	}
}

func findNestedYAMLSecretSpans(s string, stats *scanStats) []span {
	var spans []span
	if strings.IndexByte(s, '\n') >= 0 || strings.IndexByte(s, '\r') >= 0 || strings.Contains(s, "\u2028") {
		st := newYAMLScanState(false)
		for lineStart := 0; lineStart < len(s); {
			lineEnd, nextStart := nextRealLineBreak(s, lineStart, stats)
			st.processLine(s[lineStart:lineEnd], lineStart, &spans, stats)
			lineStart = nextStart
		}
		st.finish(&spans)
	}
	if strings.Contains(s, `\n`) || strings.Contains(s, `\u000a`) || strings.Contains(s, `\u000A`) {
		spans = append(spans, findEscapedJSONNestedYAMLSpans(s, stats)...)
	}
	if len(spans) > 1 {
		sort.Slice(spans, func(i, j int) bool {
			if spans[i].start != spans[j].start {
				return spans[i].start < spans[j].start
			}
			return spans[i].end < spans[j].end
		})
	}
	return spans
}

func nextRealLineBreak(s string, start int, stats *scanStats) (lineEnd, nextStart int) {
	for i := start; i < len(s); i++ {
		if stats != nil {
			stats.scannerSteps++
		}
		b := s[i]
		if b == '\n' {
			return i, i + 1
		}
		if b == '\r' {
			if i+1 < len(s) && s[i+1] == '\n' {
				return i, i + 2
			}
			return i, i + 1
		}
		if b == 0xe2 && i+2 < len(s) && s[i+1] == 0x80 && s[i+2] == 0xa8 {
			return i, i + 3
		}
	}
	return len(s), len(s)
}

func findEscapedJSONNestedYAMLSpans(s string, stats *scanStats) []span {
	var spans []span
	for i := 0; i < len(s); i++ {
		if stats != nil {
			stats.scannerSteps++
		}
		if s[i] != '"' {
			continue
		}
		strStart := i + 1
		j := strStart
		hasEscNL := false
		hasColon := false
		for j < len(s) {
			if stats != nil {
				stats.scannerSteps++
			}
			if s[j] == '\\' && j+1 < len(s) {
				if s[j+1] == 'n' {
					hasEscNL = true
				} else if s[j+1] == 'u' && j+5 < len(s) && s[j+2] == '0' && s[j+3] == '0' && s[j+4] == '0' && (s[j+5] == 'a' || s[j+5] == 'A') {
					hasEscNL = true
				}
				j += 2
				continue
			}
			if s[j] == ':' {
				hasColon = true
			}
			if s[j] == '"' {
				break
			}
			j++
		}
		strEnd := j
		i = j
		if !hasEscNL || !hasColon || strEnd <= strStart {
			continue
		}
		seg := s[strStart:strEnd]
		st := newYAMLScanState(true)
		for pos := 0; pos < len(seg); {
			lineEnd, nextPos := nextEscapedLineBreak(seg, pos, stats)
			st.processLine(seg[pos:lineEnd], strStart+pos, &spans, stats)
			pos = nextPos
		}
		st.finish(&spans)
	}
	return spans
}

func nextEscapedLineBreak(seg string, start int, stats *scanStats) (lineEnd, nextStart int) {
	for i := start; i < len(seg); i++ {
		if stats != nil {
			stats.scannerSteps++
		}
		if seg[i] == '\\' && i+1 < len(seg) {
			if seg[i+1] == 'r' && i+3 < len(seg) && seg[i+2] == '\\' && seg[i+3] == 'n' {
				return i, i + 4
			}
			if seg[i+1] == 'n' {
				return i, i + 2
			}
			if seg[i+1] == 'u' && i+5 < len(seg) && seg[i+2] == '0' && seg[i+3] == '0' && seg[i+4] == '0' {
				if seg[i+5] == 'a' || seg[i+5] == 'A' {
					return i, i + 6
				}
				if (seg[i+5] == 'd' || seg[i+5] == 'D') && i+11 < len(seg) &&
					seg[i+6] == '\\' && seg[i+7] == 'u' && seg[i+8] == '0' && seg[i+9] == '0' && seg[i+10] == '0' &&
					(seg[i+11] == 'a' || seg[i+11] == 'A') {
					return i, i + 12
				}
			}
			i++
		}
	}
	return len(seg), len(seg)
}

func findFlowCollectionSecretSpans(s string, stats *scanStats) []span {
	if stats != nil {
		stats.bytesScanned += len(s)
	}
	locs := flowStartRe.FindAllStringSubmatchIndex(s, -1)
	if len(locs) == 0 {
		return nil
	}
	var spans []span
	cursor := 0
	for _, loc := range locs {
		if stats != nil {
			stats.scannerSteps++
		}
		openPos := loc[2]
		if openPos < cursor {
			continue
		}
		if mEnd := matchMarkerPrefix(s, openPos); mEnd > openPos {
			cursor = mEnd
			continue
		}
		endPos := scanFlowCollection(s, openPos, &spans, stats)
		if endPos > cursor {
			cursor = endPos
		}
	}
	return spans
}

type flowFrame struct {
	delim     byte // '{' or '['
	expectKey bool
	skipVal   bool
	inValue   bool
}

func scanFlowCollection(s string, openPos int, spans *[]span, stats *scanStats) int {
	if openPos >= len(s) || (s[openPos] != '{' && s[openPos] != '[') {
		return openPos
	}
	stack := []flowFrame{{delim: s[openPos], expectKey: s[openPos] == '{'}}
	i := openPos + 1
	for i < len(s) && len(stack) > 0 {
		if stats != nil {
			stats.scannerSteps++
		}
		b := s[i]
		if b == ' ' || b == '\t' || b == '\r' || b == '\n' {
			i++
			continue
		}
		top := &stack[len(stack)-1]
		if b == '[' {
			if mEnd := matchMarkerPrefix(s, i); mEnd > i {
				i = mEnd
				continue
			}
		}
		switch b {
		case '{', '[':
			stack = append(stack, flowFrame{delim: b, expectKey: b == '{', skipVal: top.skipVal})
			i++
		case '}', ']':
			if (b == '}' && top.delim == '{') || (b == ']' && top.delim == '[') {
				stack = stack[:len(stack)-1]
			}
			i++
		case ',':
			if top.delim == '{' {
				top.expectKey = true
				top.inValue = false
				top.skipVal = false
			}
			i++
		case ':':
			if top.delim == '{' && top.expectKey {
				top.expectKey = false
				top.inValue = true
			}
			i++
		case '"', '\'':
			qEnd := scanQuotedLiteral(s, i, b, stats)
			innerStart := i + 1
			innerEnd := qEnd
			if qEnd < len(s) && s[qEnd] == b {
				qEnd++
			}
			if top.delim == '{' && top.expectKey {
				keyName := normalizeKeyName(s[innerStart:innerEnd])
				top.skipVal = isYAMLMetadataKey(keyName)
			} else if !top.skipVal && innerEnd > innerStart {
				*spans = append(*spans, span{start: innerStart, end: innerEnd})
			}
			i = qEnd
		default:
			tokStart := i
			for i < len(s) {
				c := s[i]
				if c == ',' || c == ':' || c == '{' || c == '}' || c == '[' || c == ']' || c == '\r' || c == '\n' {
					break
				}
				i++
				if stats != nil {
					stats.scannerSteps++
				}
			}
			tokEnd := trimRightSpaceIndex(s, tokStart, i)
			tok := s[tokStart:tokEnd]
			if tok == "" {
				continue
			}
			if top.delim == '{' && top.expectKey {
				top.skipVal = isYAMLMetadataKey(normalizeKeyName(tok))
			} else if !top.skipVal && !isMarker(tok) && !isYAMLNonSecretScalar(tok) {
				*spans = append(*spans, span{start: tokStart, end: tokEnd})
			}
		}
	}
	return i
}

func scanQuotedLiteral(s string, start int, quote byte, stats *scanStats) int {
	i := start + 1
	for i < len(s) {
		if stats != nil {
			stats.scannerSteps++
		}
		if s[i] == '\\' && i+1 < len(s) {
			i += 2
			continue
		}
		if s[i] == quote {
			return i
		}
		i++
	}
	return i
}

func findKVSpans(s string, stats *scanStats) []span {
	if stats != nil {
		stats.bytesScanned += len(s)
	}
	locs := kvKeyRe.FindAllStringSubmatchIndex(s, -1)
	if len(locs) == 0 {
		return nil
	}
	var spans []span
	cursor := 0
	for _, loc := range locs {
		if stats != nil {
			stats.scannerSteps++
		}
		if loc[0] < cursor {
			continue
		}
		valStart := loc[1]
		if valStart >= len(s) {
			continue
		}
		var rawKey string
		bareKey := false
		switch {
		case loc[2] >= 0:
			rawKey = s[loc[2]:loc[3]]
		case loc[4] >= 0:
			rawKey = s[loc[4]:loc[5]]
		case loc[6] >= 0:
			rawKey = s[loc[6]:loc[7]]
			bareKey = true
		}
		sep := s[loc[8]:loc[9]]

		first := s[valStart]
		// Do not mistake the closing quote of `"password:"` (e.g. `grep "password:" file`)
		// for the opening quote of a secret value.
		if bareKey && loc[0] > 0 && (first == '"' || first == '\'') && s[loc[0]-1] == first &&
			len(sep) > 0 && (sep[len(sep)-1] == ':' || sep[len(sep)-1] == '=') {
			continue
		}

		// JSON-escaped quoted string value: `\"secret\"`.
		if first == '\\' && valStart+1 < len(s) && s[valStart+1] == '"' {
			innerStart := valStart + 2
			qEnd, closed := scanEscapedJSONStringValue(s, innerStart, stats)
			if closed {
				if qEnd > innerStart {
					spans = append(spans, span{start: innerStart, end: qEnd})
				}
				cursor = qEnd + 2
			} else {
				if qEnd-innerStart >= 6 {
					spans = append(spans, span{start: innerStart, end: qEnd})
				}
				cursor = qEnd
			}
			continue
		}

		// Double-quoted value (supports escapes, multiline values without ':'/'=' on
		// continuation lines, and unterminated quotes of 6+ chars).
		if first == '"' {
			innerStart := valStart + 1
			qEnd, nextCursor, ok := scanDoubleQuotedKVValue(s, innerStart, stats)
			cursor = nextCursor
			if ok && qEnd > innerStart {
				spans = append(spans, span{start: innerStart, end: qEnd})
			}
			continue
		}

		// Single-quoted value (supports \' and '' escapes, and unterminated quotes of 6+ chars).
		if first == '\'' {
			innerStart := valStart + 1
			qEnd, closed := scanSingleQuotedKVValue(s, innerStart, stats)
			if closed {
				if qEnd > innerStart {
					spans = append(spans, span{start: innerStart, end: qEnd})
				}
				cursor = qEnd + 1
			} else {
				if qEnd-innerStart >= 6 {
					spans = append(spans, span{start: innerStart, end: qEnd})
				}
				cursor = qEnd
			}
			continue
		}

		if first == ' ' || first == '\t' || first == '\r' || first == '\n' ||
			first == '{' || first == '[' || first == '(' || first == '<' ||
			first == ',' || first == ';' || first == '&' {
			continue
		}

		valEnd := valStart
		skippedCall := false
		for valEnd < len(s) {
			if stats != nil {
				stats.scannerSteps++
			}
			if matchMarkerPrefix(s, valEnd) > valEnd {
				break
			}
			b := s[valEnd]
			if b == ' ' || b == '\t' || b == '\r' || b == '\n' ||
				b == '"' || b == '<' || b == '>' || b == '&' {
				break
			}
			if b == 0xc2 && valEnd+1 < len(s) && s[valEnd+1] == 0xa0 {
				break
			}
			if (b == ',' || b == ';') && startsNextKV(s, valEnd+1) {
				break
			}
			if b == '(' && isCodeCallAt(s, valStart, valEnd) {
				// Advance past '(' so any secret inside call arguments is still scanned.
				cursor = valEnd + 1
				skippedCall = true
				break
			}
			valEnd++
		}
		if skippedCall {
			continue
		}

		rawEnd := valEnd
		cursor = rawEnd
		valEnd = trimUnquotedKVTrailing(s, valStart, valEnd)
		if valEnd-valStart < 6 {
			continue
		}
		val := s[valStart:valEnd]
		if isMarker(val) {
			continue
		}
		if isFilesystemPath(rawKey, val) {
			continue
		}
		isColonSep := strings.IndexByte(sep, ':') >= 0 && strings.IndexByte(sep, '=') < 0
		if isProseStatusWord(val, isColonSep) {
			continue
		}
		spans = append(spans, span{start: valStart, end: valEnd})
	}
	return spans
}

func scanEscapedJSONStringValue(s string, start int, stats *scanStats) (end int, closed bool) {
	i := start
	for i < len(s) {
		if stats != nil {
			stats.scannerSteps++
		}
		if s[i] == '\r' || s[i] == '\n' || s[i] == '"' {
			return i, false
		}
		if s[i] == '\\' && i+1 < len(s) {
			if s[i+1] == '"' {
				return i, true
			}
			i += 2
			continue
		}
		i++
	}
	return i, false
}

func scanDoubleQuotedKVValue(s string, start int, stats *scanStats) (end int, nextCursor int, ok bool) {
	i := start
	for i < len(s) {
		if stats != nil {
			stats.scannerSteps++
		}
		if s[i] == '\\' && i+1 < len(s) {
			i += 2
			continue
		}
		if s[i] == '"' {
			return i, i + 1, i > start
		}
		if s[i] == '\r' || s[i] == '\n' {
			break
		}
		i++
	}
	firstLineEnd := i
	if i < len(s) && (s[i] == '\r' || s[i] == '\n') {
		// Allow multiline double-quoted values only when continuation lines do not
		// contain ':' or '=' (so an unclosed quote cannot swallow the next key=val).
		j := i
		for j < len(s) {
			if stats != nil {
				stats.scannerSteps++
			}
			if s[j] == '\r' && j+1 < len(s) && s[j+1] == '\n' {
				j += 2
			} else if s[j] == '\n' {
				j++
			} else {
				break
			}
			lineHasContent := false
			for j < len(s) && s[j] != '\r' && s[j] != '\n' {
				if stats != nil {
					stats.scannerSteps++
				}
				if s[j] == '\\' && j+1 < len(s) {
					lineHasContent = true
					j += 2
					continue
				}
				if s[j] == '"' {
					if lineHasContent {
						return j, j + 1, true
					}
					break
				}
				if s[j] == ':' || s[j] == '=' {
					// Next line looks like another key=value pair: treat line 1 as unterminated.
					if firstLineEnd-start >= 6 {
						return firstLineEnd, firstLineEnd, true
					}
					return firstLineEnd, firstLineEnd, false
				}
				lineHasContent = true
				j++
			}
			if j < len(s) && s[j] == '"' {
				break
			}
		}
	}
	if firstLineEnd-start >= 6 {
		return firstLineEnd, firstLineEnd, true
	}
	return firstLineEnd, firstLineEnd, false
}

func scanSingleQuotedKVValue(s string, start int, stats *scanStats) (end int, closed bool) {
	i := start
	for i < len(s) {
		if stats != nil {
			stats.scannerSteps++
		}
		if s[i] == '\r' || s[i] == '\n' {
			return i, false
		}
		if s[i] == '\\' && i+1 < len(s) {
			i += 2
			continue
		}
		if s[i] == '\'' {
			if i+1 < len(s) && s[i+1] == '\'' {
				i += 2
				continue
			}
			return i, true
		}
		i++
	}
	return i, false
}

// isCodeCallAt reports whether s[valStart:parenPos] + "(" looks like a function
// or method call (such as os.Getenv("TOKEN") or getPassword()) rather than a
// password containing a literal '(' (such as Xy9(kLm2nPq).
func isCodeCallAt(s string, valStart, parenPos int) bool {
	if parenPos <= valStart {
		return false
	}
	first := s[valStart]
	if !isASCIIAlpha(first) && first != '_' {
		return false
	}
	for i := valStart + 1; i < parenPos; i++ {
		b := s[i]
		if !(isASCIIAlpha(b) || (b >= '0' && b <= '9') || b == '_' || b == '.') {
			return false
		}
	}
	if s[parenPos-1] == '.' {
		return false
	}
	next := parenPos + 1
	if next >= len(s) {
		return false
	}
	if s[next] == '"' || s[next] == '\'' || s[next] == ')' {
		return true
	}
	// Bounded O(1) peek for a clean argument list closed by ')' at a token boundary.
	limit := min(len(s), next+64)
	for j := next; j < limit; j++ {
		b := s[j]
		if b == ')' {
			after := j + 1
			if after >= len(s) {
				return true
			}
			c := s[after]
			return c == ' ' || c == '\t' || c == '\r' || c == '\n' ||
				c == ',' || c == ';' || c == ')' || c == ']' || c == '}'
		}
		if b == ' ' || b == '\t' || b == '\r' || b == '\n' {
			return false
		}
	}
	return false
}

func trimUnquotedKVTrailing(s string, start, end int) int {
	// Count balanced bracket pairs inside the token so unmatched closing delimiters
	// from an outer container (e.g. (password=val), map[password:val]) are trimmed
	// while mid-token closing delimiters stay part of the value.
	openParen, closeParen := 0, 0
	openBrace, closeBrace := 0, 0
	openBrack, closeBrack := 0, 0
	for i := start; i < end; i++ {
		switch s[i] {
		case '(':
			openParen++
		case ')':
			closeParen++
		case '{':
			openBrace++
		case '}':
			closeBrace++
		case '[':
			openBrack++
		case ']':
			closeBrack++
		}
	}
	for end > start {
		b := s[end-1]
		switch b {
		case ',', ';', '\'':
			end--
		case ')':
			if closeParen > openParen {
				closeParen--
				end--
				continue
			}
			return end
		case '}':
			if closeBrace > openBrace {
				closeBrace--
				end--
				continue
			}
			return end
		case ']':
			if closeBrack > openBrack {
				closeBrack--
				end--
				continue
			}
			return end
		default:
			return end
		}
	}
	return end
}

func isFilesystemPath(rawKey, val string) bool {
	if rawKey == "PWD" && strings.HasPrefix(val, "/") {
		return true
	}
	var rest string
	switch {
	case strings.HasPrefix(val, "/"):
		rest = val[1:]
	case strings.HasPrefix(val, "./"):
		rest = val[2:]
	case strings.HasPrefix(val, "../"):
		rest = val[3:]
	case strings.HasPrefix(val, "~/"):
		rest = val[2:]
	default:
		return false
	}
	if strings.IndexByte(rest, '/') < 0 {
		return false
	}
	// Require normal path segments so base64 secrets starting with '/' are not skipped.
	segLen := 0
	hasUpper, hasLower, hasDigit := false, false, false
	for i := range len(rest) {
		b := rest[i]
		if b == '/' {
			if segLen >= 24 && hasUpper && hasLower && hasDigit {
				return false
			}
			segLen = 0
			hasUpper, hasLower, hasDigit = false, false, false
			continue
		}
		switch {
		case b >= 'A' && b <= 'Z':
			hasUpper = true
		case b >= 'a' && b <= 'z':
			hasLower = true
		case b >= '0' && b <= '9':
			hasDigit = true
		case b == '.' || b == '_' || b == '-' || b == '@' || b == '+':
		default:
			return false
		}
		segLen++
	}
	return !(segLen >= 24 && hasUpper && hasLower && hasDigit)
}

func isAuthProseWord(raw string) bool {
	w, ok := normalizeProseWord(raw)
	if !ok {
		return false
	}
	if _, skip := basicAuthStopwords[w]; skip {
		return true
	}
	return isProseStatusWordLower(w, true)
}

func isProseStatusWord(raw string, allowSuffix bool) bool {
	w, ok := normalizeProseWord(raw)
	if !ok {
		return false
	}
	return isProseStatusWordLower(w, allowSuffix)
}

// normalizeProseWord accepts all-lowercase or Titlecase ASCII words up to 15 bytes
// so mixed-case tokens (e.g. base64) and long passphrases are never treated as prose.
func normalizeProseWord(raw string) (string, bool) {
	if len(raw) == 0 || len(raw) > 15 {
		return "", false
	}
	first := raw[0]
	upperFirst := first >= 'A' && first <= 'Z'
	if !upperFirst && (first < 'a' || first > 'z') {
		return "", false
	}
	for i := 1; i < len(raw); i++ {
		if raw[i] < 'a' || raw[i] > 'z' {
			return "", false
		}
	}
	if !upperFirst {
		return raw, true
	}
	var buf [15]byte
	buf[0] = first + ('a' - 'A')
	copy(buf[1:], raw[1:])
	return string(buf[:len(raw)]), true
}

func isProseStatusWordLower(w string, allowSuffix bool) bool {
	if _, skip := unquotedKVProseWords[w]; skip {
		return true
	}
	if !allowSuffix || len(w) < 6 || !hasASCIIVowel(w) {
		return false
	}
	return strings.HasSuffix(w, "ed") ||
		strings.HasSuffix(w, "ing") ||
		strings.HasSuffix(w, "tion") ||
		strings.HasSuffix(w, "sion") ||
		strings.HasSuffix(w, "ment") ||
		strings.HasSuffix(w, "ness") ||
		strings.HasSuffix(w, "able") ||
		strings.HasSuffix(w, "ible")
}

func hasASCIIVowel(s string) bool {
	for i := range len(s) {
		switch s[i] {
		case 'a', 'e', 'i', 'o', 'u', 'y':
			return true
		}
	}
	return false
}

func startsNextKV(s string, pos int) bool {
	if pos >= len(s) || !isASCIIAlpha(s[pos]) {
		return false
	}
	i := pos + 1
	for i < len(s) && i-pos < 32 && (isASCIIAlpha(s[i]) || (s[i] >= '0' && s[i] <= '9') || s[i] == '_' || s[i] == '-') {
		i++
	}
	return i < len(s) && (s[i] == '=' || s[i] == ':')
}

func measureIndent(line string) (indent int, offset int, isEmpty bool, isComment bool) {
	i := 0
	for i < len(line) {
		switch line[i] {
		case ' ':
			indent++
			i++
		case '\t':
			indent += 4
			i++
		case 0xc2:
			if i+1 < len(line) && line[i+1] == 0xa0 {
				indent++
				i += 2
				continue
			}
			return indent, i, false, false
		default:
			if line[i] == '#' {
				return indent, i, false, true
			}
			return indent, i, false, false
		}
	}
	return indent, i, true, false
}

func visualWidth(prefix string) int {
	w := 0
	for i := 0; i < len(prefix); {
		if prefix[i] == '\t' {
			w += 4
			i++
		} else if prefix[i] == 0xc2 && i+1 < len(prefix) && prefix[i+1] == 0xa0 {
			w++
			i += 2
		} else {
			w++
			i++
		}
	}
	return w
}

func trimRightSpaceIndex(s string, start, end int) int {
	for end > start {
		if s[end-1] == ' ' || s[end-1] == '\t' {
			end--
			continue
		}
		if end-start >= 2 && s[end-2] == 0xc2 && s[end-1] == 0xa0 {
			end -= 2
			continue
		}
		break
	}
	return end
}

func yamlValueSpan(absStart int, val string, escapedJSON bool) (span, bool) {
	if len(val) == 0 {
		return span{}, false
	}
	if escapedJSON && strings.HasPrefix(val, `\"`) {
		for i := 2; i < len(val); i++ {
			if val[i] == '\\' && i+1 < len(val) {
				if val[i+1] == '"' {
					if i == 2 {
						return span{}, false
					}
					return span{start: absStart + 2, end: absStart + i}, true
				}
				i++
				continue
			}
		}
	}
	if val[0] == '"' {
		for i := 1; i < len(val); i++ {
			if val[i] == '\\' && i+1 < len(val) {
				i++
				continue
			}
			if val[i] == '"' {
				if i == 1 {
					return span{}, false
				}
				return span{start: absStart + 1, end: absStart + i}, true
			}
		}
	}
	if val[0] == '\'' {
		for i := 1; i < len(val); i++ {
			if val[i] == '\'' {
				if i+1 < len(val) && val[i+1] == '\'' {
					i++
					continue
				}
				if i == 1 {
					return span{}, false
				}
				return span{start: absStart + 1, end: absStart + i}, true
			}
		}
	}
	return span{start: absStart, end: absStart + len(val)}, true
}

func normalizeKeyName(raw string) string {
	trimmed := strings.Trim(raw, " \t\"'")
	var b strings.Builder
	b.Grow(len(trimmed))
	for i := range len(trimmed) {
		c := trimmed[i]
		if c == '_' || c == '-' {
			continue
		}
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		b.WriteByte(c)
	}
	return b.String()
}

func isYAMLMetadataKey(normalized string) bool {
	_, ok := yamlMetadataKeys[normalized]
	return ok
}

func isYAMLNonSecretScalar(val string) bool {
	lower := strings.ToLower(val)
	switch lower {
	case "true", "false", "yes", "no", "null", "nil", "~":
		return true
	}
	// Short numeric values (e.g. file modes like 0400, minLength 8, ports) are
	// metadata rather than secrets, while 6+ digit numeric values may be PINs.
	if len(val) > 0 && len(val) < 6 {
		allDigits := true
		for i := range len(val) {
			if val[i] < '0' || val[i] > '9' {
				allDigits = false
				break
			}
		}
		if allDigits {
			return true
		}
	}
	return false
}
