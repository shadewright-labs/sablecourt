package redact

import (
	"regexp"
	"sort"
	"strings"
)

var (
	foldedAuthHeaderOnlyRe = regexp.MustCompile(`(?i)\b(?:proxy-)?authorization["']?[\s\x{00a0}]*:[\s\x{00a0}\["']*$`)
	foldedAuthWithSchemeRe = regexp.MustCompile(`(?i)\b(?:proxy-)?authorization["']?[\s\x{00a0}]*:[\s\x{00a0}\["']*([a-z][a-z0-9_-]{1,31})[\s\x{00a0}]*$`)
	foldedSchemeOnlyLineRe = regexp.MustCompile(`(?i)^[\s\x{00a0}\["']*([a-z][a-z0-9_-]{1,31})[\s\x{00a0}]*$`)
)

// LineRedactor scrubs credentials across a sequence of lines using O(1) state
// for multi-line PEM private keys, nested YAML secret blocks, YAML block scalars,
// and folded Authorization headers.
type LineRedactor struct {
	inPEM            bool
	pemFooter        string
	yaml             yamlScanState
	pendingAuth      bool
	pendingAuthToken string
}

// NewLineRedactor returns a fresh stateful line-at-a-time redactor.
func NewLineRedactor() *LineRedactor {
	return &LineRedactor{
		yaml: newYAMLScanState(false),
	}
}

// Reset clears all cross-line state.
func (l *LineRedactor) Reset() {
	l.inPEM = false
	l.pemFooter = ""
	l.yaml = newYAMLScanState(false)
	l.pendingAuth = false
	l.pendingAuthToken = ""
}

// String redacts one line and returns only the redacted text.
func (l *LineRedactor) String(line string) string {
	return l.RedactLine(line).Text
}

// RedactLine redacts one line (with or without a trailing newline) while
// maintaining cross-line secret context.
func (l *LineRedactor) RedactLine(line string) Result {
	res, _ := l.redactLineWithStats(line)
	return res
}

func (l *LineRedactor) redactLineWithStats(line string) (Result, scanStats) {
	var stats scanStats
	if line == "" {
		return Result{}, stats
	}

	content, ending := splitLineEnding(line)

	// 1. Continue an open PEM private key block across lines (fail closed until footer).
	if l.inPEM {
		if idx := strings.Index(content, l.pemFooter); idx >= 0 {
			after := content[idx+len(l.pemFooter):]
			l.inPEM = false
			l.pemFooter = ""
			if after == "" {
				return Result{Text: "", Hits: 1, Categories: []string{"private_key"}}, stats
			}
			rest, st := textWithStats(after + ending)
			stats.scannerSteps += st.scannerSteps
			stats.bytesScanned += st.bytesScanned
			cats := mergeCategories(rest.Categories, "private_key")
			return Result{Text: rest.Text, Hits: rest.Hits + 1, Categories: cats}, stats
		}
		return Result{Text: "", Hits: 1, Categories: []string{"private_key"}}, stats
	}

	// 2. Continue a folded Authorization header from the previous line.
	if l.pendingAuth || l.pendingAuthToken != "" {
		if strings.TrimSpace(content) == "" {
			return Result{Text: line}, stats
		}
		if l.pendingAuth {
			if m := foldedSchemeOnlyLineRe.FindStringSubmatch(content); m != nil {
				scheme := strings.ToLower(m[1])
				if _, skip := authSchemeStopwords[scheme]; !skip && !isProseStatusWord(m[1], true) {
					l.pendingAuth = false
					l.pendingAuthToken = m[1]
					return Result{Text: line}, stats
				}
			}
			l.pendingAuth = false
			prefix := "Authorization: "
			res, st := textWithStats(prefix + line)
			stats.scannerSteps += st.scannerSteps
			stats.bytesScanned += st.bytesScanned
			if strings.HasPrefix(res.Text, prefix) && res.Hits > 0 {
				res.Text = res.Text[len(prefix):]
				return res, stats
			}
		} else {
			scheme := l.pendingAuthToken
			l.pendingAuthToken = ""
			prefix := "Authorization: " + scheme + " "
			res, st := textWithStats(prefix + line)
			stats.scannerSteps += st.scannerSteps
			stats.bytesScanned += st.bytesScanned
			if strings.HasPrefix(res.Text, prefix) && res.Hits > 0 {
				res.Text = res.Text[len(prefix):]
				return res, stats
			}
		}
	}

	// 3. Detect an unclosed PEM header starting on this line before single-line Text blanks it.
	if loc := privateKeyHeader.FindStringSubmatchIndex(content); loc != nil {
		label := content[loc[2]:loc[3]]
		footer := "-----END " + label + "-----"
		afterHdr := content[loc[1]:]
		if !strings.Contains(afterHdr, footer) && scanTruncatedPEMEnd(content+"\n", loc[1], nil) == len(content)+1 {
			l.inPEM = true
			l.pemFooter = footer
		}
	}

	// 4. Apply standard single-line redaction.
	res, st := textWithStats(line)
	stats.scannerSteps += st.scannerSteps
	stats.bytesScanned += st.bytesScanned
	outContent, outEnding := splitLineEnding(res.Text)

	// 5. Apply cross-line YAML state machine to the current line.
	var yamlSpans []span
	wasBlock := l.yaml.inBlockScalar
	l.yaml.processLine(outContent, 0, &yamlSpans, &stats)
	// In line-at-a-time mode, flush any single-line block/plain span accumulated on this line
	// while keeping block/secret mode active for subsequent lines.
	if l.yaml.inBlockScalar && l.yaml.blockStart >= 0 && l.yaml.blockEnd > l.yaml.blockStart {
		yamlSpans = append(yamlSpans, span{start: l.yaml.blockStart, end: l.yaml.blockEnd})
		l.yaml.blockStart = -1
		l.yaml.blockEnd = -1
	} else if !wasBlock && l.yaml.plainStart >= 0 && l.yaml.plainEnd > l.yaml.plainStart {
		yamlSpans = append(yamlSpans, span{start: l.yaml.plainStart, end: l.yaml.plainEnd})
		l.yaml.plainStart = -1
		l.yaml.plainEnd = -1
	}
	if len(yamlSpans) > 0 {
		redContent, hits := applySpans(outContent, yamlSpans, nil, "[REDACTED:secret]", &stats)
		if hits > 0 {
			res.Text = redContent + outEnding
			res.Hits += hits
			res.Categories = mergeCategories(res.Categories, "generic_secret_kv")
		}
	}

	// 6. Detect folded Authorization header at end of this line.
	if !l.inPEM && res.Hits == 0 {
		if m := foldedAuthWithSchemeRe.FindStringSubmatch(outContent); m != nil {
			scheme := strings.ToLower(m[1])
			if _, skip := authSchemeStopwords[scheme]; !skip && !isProseStatusWord(m[1], true) {
				l.pendingAuthToken = m[1]
			}
		} else if foldedAuthHeaderOnlyRe.MatchString(outContent) {
			l.pendingAuth = true
		}
	}

	return res, stats
}

func splitLineEnding(line string) (content, ending string) {
	if strings.HasSuffix(line, "\r\n") {
		return line[:len(line)-2], "\r\n"
	}
	if strings.HasSuffix(line, "\n") {
		return line[:len(line)-1], "\n"
	}
	if strings.HasSuffix(line, "\r") {
		return line[:len(line)-1], "\r"
	}
	return line, ""
}

func mergeCategories(cats []string, add string) []string {
	for _, c := range cats {
		if c == add {
			return cats
		}
	}
	out := append(append([]string(nil), cats...), add)
	sort.Strings(out)
	return out
}
