package attach

import (
	"bytes"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/shadewright-labs/sablecourt/internal/redact"
)

const (
	maxPendingBytes = 16 * 1024
	pemBegin        = "-----BEGIN "
	pemEnd          = "-----END "
	pemLabel        = "PRIVATE KEY"
	pemMarker       = "[REDACTED:private_key]"
)

type streamRedactor struct {
	text      textRedactor
	inPEM     bool
	pemFooter []byte
	pemTail   []byte
}

// Filter PEM blocks before parsing text credentials, since the block may span
// lines or begin inside an assignment value.
func (r *streamRedactor) write(raw []byte, eof bool) string {
	var out strings.Builder
	if len(r.pemTail) != 0 {
		raw = append(r.pemTail, raw...)
		r.pemTail = nil
	}
	for len(raw) != 0 {
		if r.inPEM {
			if len(r.pemFooter) == 0 {
				break
			}
			i := bytes.Index(raw, r.pemFooter)
			if i < 0 {
				if !eof {
					start := bytes.LastIndex(raw, []byte(pemEnd))
					if start >= 0 && bytes.HasPrefix(r.pemFooter, raw[start:]) {
						r.pemTail = bytes.Clone(raw[start:])
					} else {
						r.pemTail = bytes.Clone(partialPrefix(raw, pemEnd))
					}
				}
				break
			}
			r.text.write(&out, []byte(pemMarker))
			raw = raw[i+len(r.pemFooter):]
			r.inPEM = false
			r.pemFooter = nil
			continue
		}

		start, end := pemHeader(raw)
		r.text.write(&out, raw[:start])
		if end >= 0 {
			r.inPEM = true
			if end-start <= maxPendingBytes {
				r.pemFooter = append([]byte(pemEnd), raw[start+len(pemBegin):end]...)
			}
			raw = raw[end:]
			continue
		}
		tail := raw[start:]
		switch {
		case len(tail) > maxPendingBytes || eof && bytes.HasPrefix(tail, []byte(pemBegin)):
			// Without a bounded header, there is no safe footer to match.
			r.inPEM = true
		case eof:
			r.text.write(&out, tail)
		default:
			r.pemTail = bytes.Clone(tail)
		}
		break
	}
	if eof {
		if r.inPEM {
			r.text.write(&out, []byte(pemMarker))
		}
		r.text.finish(&out)
	}
	return out.String()
}

// pemHeader finds a complete private key header or an ambiguous suffix.
// A negative end means the suffix at start needs more input.
func pemHeader(raw []byte) (start, end int) {
	for offset := 0; ; {
		i := bytes.Index(raw[offset:], []byte(pemBegin))
		if i < 0 {
			return len(raw) - len(partialPrefix(raw[offset:], pemBegin)), -1
		}
		start = offset + i
		label := raw[start+len(pemBegin):]
		n := 0
		for n < len(label) && (label[n] == ' ' || label[n] >= 'A' && label[n] <= 'Z' || label[n] >= '0' && label[n] <= '9') {
			n++
		}
		if n == len(label) {
			return start, -1
		}
		if bytes.HasSuffix(label[:n], []byte(pemLabel)) {
			dashes := label[n:]
			if bytes.HasPrefix(dashes, []byte("-----")) {
				return start, start + len(pemBegin) + n + 5
			}
			if bytes.HasPrefix([]byte("-----"), dashes) {
				return start, -1
			}
		}
		offset = start + 1
	}
}

func partialPrefix(raw []byte, prefix string) []byte {
	for n := min(len(raw), len(prefix)-1); n > 0; n-- {
		if bytes.HasSuffix(raw, []byte(prefix[:n])) {
			return raw[len(raw)-n:]
		}
	}
	return nil
}

type assignmentState uint8

const (
	noAssignment assignmentState = iota
	secretSeparator
	secretValue
	secretQuotedValue
	bearerColon
	bearerWord
	bearerValue
)

type credentialKind string

const (
	noCredential credentialKind = ""
	secret       credentialKind = "secret"
	bearer       credentialKind = "bearer_token"
	// authorization is an Authorization value under any other scheme.
	authorization credentialKind = "authorization"
	openAI        credentialKind = "openai_key"
)

var credentialPrefixes = []struct {
	prefix string
	kind   credentialKind
}{
	{"sc_task_", "sablecourt_token"},
	{"op_", "sablecourt_token"},
	{"sk-", openAI},
	{"ghp_", "github_token"},
	{"gho_", "github_token"},
	{"ghu_", "github_token"},
	{"ghs_", "github_token"},
	{"ghr_", "github_token"},
	{"github_pat_", "github_token"},
	{"AIza", "google_api_key"},
	{"AKIA", "aws_access_key"},
	{"ASIA", "aws_access_key"},
}

var (
	secretLabel = regexp.MustCompile(`(?i)(?:api[_-]?key|secret|password|passwd|access[_-]?token|operator[_-]?token)$`)
	bearerLabel = regexp.MustCompile(`(?i)authorization$`)
)

type textRedactor struct {
	fragment []byte
	kind     credentialKind
	// value holds the credential as written; norm holds it with ANSI
	// sequences dropped and unicode escapes decoded, which is what the
	// rules match.
	value      []byte
	norm       []byte
	overflow   bool
	prevWord   bool
	label      [32]byte
	labelLen   int
	assignment assignmentState
	// bearerPos counts the scheme bytes after "Authorization:"; isBearer
	// tracks whether they still spell "bearer".
	bearerPos int
	isBearer  bool
	authKind  credentialKind
	// quote is the quote that opened a secret value, so the value runs to
	// its closing quote (JSON strings may hold spaces); escaped tracks a
	// backslash inside it.
	quote   byte
	escaped bool
}

func (r *textRedactor) write(out *strings.Builder, raw []byte) {
	if len(r.fragment) != 0 {
		raw = append(r.fragment, raw...)
		r.fragment = nil
	}
	for len(raw) != 0 {
		n, b, ansi, more := unitAt(raw)
		if more {
			r.fragment = bytes.Clone(raw)
			return
		}
		unit := raw[:n]
		if r.kind != noCredential {
			if ansi {
				// A terminal shows a token broken by an ANSI sequence whole.
				r.keepValue(unit, nil)
				raw = raw[n:]
				continue
			}
			if r.valueByte(b) {
				if r.quote == 0 && (r.kind == secret || r.kind == bearer || r.kind == authorization) && r.assignmentAt(b) == secret {
					// A nested assignment can extend past the bearer value.
					r.kind = secret
				}
				r.observe(b)
				r.keepValue(unit, []byte{b})
				raw = raw[n:]
				continue
			}
			r.emitValue(out, false)
		}
		if ansi {
			out.Write(unit)
			raw = raw[n:]
			continue
		}
		if kind := r.assignmentAt(b); kind != noCredential {
			r.kind = kind
			r.labelLen = 0
			continue
		}
		if !r.prevWord {
			look, atEnd := normLookahead(raw)
			for _, entry := range credentialPrefixes {
				if bytes.HasPrefix(look, []byte(entry.prefix)) {
					r.kind = entry.kind
					break
				}
				if atEnd && bytes.HasPrefix([]byte(entry.prefix), look) {
					r.fragment = bytes.Clone(raw)
					return
				}
			}
			if r.kind != noCredential {
				continue
			}
		}
		out.Write(unit)
		r.observe(b)
		raw = raw[n:]
	}
}

// keepValue appends a unit of the current credential, up to the pending
// limit.
func (r *textRedactor) keepValue(unit, norm []byte) {
	if len(r.value)+len(unit) > maxPendingBytes {
		r.overflow = true
		return
	}
	r.value = append(r.value, unit...)
	r.norm = append(r.norm, norm...)
}

const (
	// maxANSI bounds a CSI sequence; a longer one is passed on as text.
	maxANSI = 32
	// maxLookahead bounds the bytes read to match a credential prefix.
	maxLookahead = 256
)

// unitAt reads one unit of terminal output: an ANSI CSI sequence (ansi), a
// JSON unicode escape for a token character (b is the character), or a
// single byte. more means the input ends inside a unit that may still
// complete.
func unitAt(raw []byte) (n int, b byte, ansi, more bool) {
	switch raw[0] {
	case 0x1b:
		if len(raw) == 1 {
			return 0, 0, false, true
		}
		if raw[1] != '[' {
			return 1, raw[0], false, false
		}
		for i := 2; i < len(raw) && i < maxANSI; i++ {
			switch c := raw[i]; {
			case c >= 0x40 && c <= 0x7e:
				return i + 1, 0, true, false
			case c < 0x20 || c > 0x3f:
				return 1, raw[0], false, false
			}
		}
		if len(raw) < maxANSI {
			return 0, 0, false, true
		}
		return 1, raw[0], false, false
	case '\\':
		if len(raw) < 6 {
			if len(raw) == 1 || raw[1] == 'u' && allHex(raw[2:]) {
				return 0, 0, false, true
			}
			return 1, raw[0], false, false
		}
		if raw[1] == 'u' && allHex(raw[2:6]) {
			r := hexValue(raw[2:6])
			if r < utf8.RuneSelf && (wordByte(byte(r)) || r == '-') {
				return 6, byte(r), false, false
			}
		}
		return 1, raw[0], false, false
	}
	return 1, raw[0], false, false
}

func allHex(p []byte) bool {
	for _, c := range p {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
			return false
		}
	}
	return true
}

func hexValue(p []byte) rune {
	var r rune
	for _, c := range p {
		switch {
		case c <= '9':
			r = r<<4 | rune(c-'0')
		case c >= 'a':
			r = r<<4 | rune(c-'a'+10)
		default:
			r = r<<4 | rune(c-'A'+10)
		}
	}
	return r
}

// normLookahead returns the start of raw as the rules see it, long enough to
// match any credential prefix, and whether it reached the end of raw (so more
// input could still complete a prefix).
func normLookahead(raw []byte) ([]byte, bool) {
	var look []byte
	i := 0
	for i < len(raw) && i < maxLookahead && len(look) < 16 {
		n, b, ansi, more := unitAt(raw[i:])
		if more {
			return look, true
		}
		if !ansi {
			look = append(look, b)
		}
		i += n
	}
	return look, i == len(raw)
}

func (r *textRedactor) finish(out *strings.Builder) {
	if r.kind != noCredential {
		r.emitValue(out, true)
	}
	out.Write(r.fragment)
}

func (r *textRedactor) emitValue(out *strings.Builder, eof bool) {
	kind := r.kind
	if kind == openAI && bytes.HasPrefix(r.norm, []byte("sk-ant-")) {
		kind = "anthropic_key"
	}
	raw, norm := string(r.value), string(r.norm)
	value := raw
	switch {
	case r.overflow, kind == secret && r.quote != 0:
		value = ""
	case kind == secret:
		if utf8.RuneCountInString(norm) >= 6 || eof {
			value = ""
		}
	case kind == bearer || kind == authorization:
		if len(norm) >= 8 || eof {
			value = ""
		}
	default:
		clean := redact.String(norm)
		switch {
		case clean == norm:
			if eof {
				value = ""
			}
		case norm == raw:
			value = clean
		default:
			value = ""
		}
	}
	if value == "" {
		out.WriteString("[REDACTED:" + string(kind) + "]")
	} else {
		out.WriteString(value)
	}
	r.kind = noCredential
	r.value = r.value[:0]
	r.norm = r.norm[:0]
	r.overflow = false
	r.quote = 0
	r.escaped = false
}

// valueByte reports whether b continues the current credential value.
func (r *textRedactor) valueByte(b byte) bool {
	if r.kind != secret || r.quote == 0 {
		return credentialByte(r.kind, b)
	}
	if b == '\n' || b == '\r' || b == r.quote && !r.escaped {
		return false
	}
	r.escaped = b == '\\' && !r.escaped
	return true
}

func quoteByte(b byte) bool {
	return b == '"' || b == '\''
}

func credentialByte(kind credentialKind, b byte) bool {
	switch kind {
	case secret:
		return !redactSpace(b) && b != '"' && b != '\'' && b != ',' && b != ';'
	case bearer:
		return wordByte(b) || strings.ContainsRune(".~+/=-", rune(b))
	default:
		return wordByte(b) || b == '-'
	}
}

func (r *textRedactor) observe(b byte) {
	r.prevWord = wordByte(b)
	if !r.prevWord && b != '-' && b < utf8.RuneSelf {
		r.labelLen = 0
		return
	}
	if r.labelLen == len(r.label) {
		copy(r.label[:], r.label[1:])
		r.labelLen--
	}
	r.label[r.labelLen] = b
	r.labelLen++
}

func wordByte(b byte) bool {
	return b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= '0' && b <= '9' || b == '_'
}

func redactSpace(b byte) bool {
	return b == ' ' || b == '\t' || b == '\n' || b == '\r' || b == '\f'
}

// assignmentAt tracks header and assignment syntax while the label and its
// separators are sent to the caller. Only the value needs to be withheld.
func (r *textRedactor) assignmentAt(b byte) credentialKind {
	space := redactSpace(b)
	// A closing quote after the label covers JSON keys such as "password".
	// An Authorization scheme word such as ApiKey is not a label of its own.
	if r.labelLen != 0 && r.assignment != bearerWord && (space || b == ':' || b == '=' || quoteByte(b)) {
		label := r.label[:r.labelLen]
		switch {
		case secretLabel.Match(label):
			r.assignment = secretSeparator
		case bearerLabel.Match(label):
			r.assignment = bearerColon
		}
	}
	switch r.assignment {
	case secretSeparator:
		if space || quoteByte(b) {
			return noCredential
		}
		if b == ':' || b == '=' {
			r.assignment = secretValue
			return noCredential
		}
	case secretValue:
		if space {
			return noCredential
		}
		if quoteByte(b) {
			r.assignment = secretQuotedValue
			r.quote = b
			return noCredential
		}
		r.quote = 0
		fallthrough
	case secretQuotedValue:
		r.assignment = noAssignment
		if r.quote != 0 {
			if b != r.quote && b != '\n' && b != '\r' {
				return secret
			}
			r.quote = 0
			return noCredential
		}
		if credentialByte(secret, b) {
			return secret
		}
	case bearerColon:
		if space || quoteByte(b) {
			return noCredential
		}
		if b == ':' {
			r.assignment = bearerWord
			r.bearerPos = 0
			r.isBearer = true
			return noCredential
		}
	case bearerWord:
		// The scheme: Bearer, or any other word (Basic, Token, ...).
		if (space || quoteByte(b)) && r.bearerPos == 0 {
			return noCredential
		}
		if space {
			r.assignment = bearerValue
			r.authKind = authorization
			if r.isBearer && r.bearerPos == len("bearer") {
				r.authKind = bearer
			}
			return noCredential
		}
		if (wordByte(b) || b == '-') && r.bearerPos < len(r.label) {
			r.isBearer = r.isBearer && r.bearerPos < len("bearer") && b|0x20 == "bearer"[r.bearerPos]
			r.bearerPos++
			return noCredential
		}
	case bearerValue:
		if space {
			return noCredential
		}
		r.assignment = noAssignment
		if credentialByte(bearer, b) {
			return r.authKind
		}
	}
	r.assignment = noAssignment
	return noCredential
}
