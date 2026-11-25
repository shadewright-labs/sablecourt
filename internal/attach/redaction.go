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
	bearerSpace
	bearerValue
)

type credentialKind string

const (
	noCredential credentialKind = ""
	secret       credentialKind = "secret"
	bearer       credentialKind = "bearer_token"
	openAI       credentialKind = "openai_key"
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
	fragment   []byte
	kind       credentialKind
	value      []byte
	overflow   bool
	prevWord   bool
	label      [32]byte
	labelLen   int
	assignment assignmentState
	bearerPos  int
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
		if r.kind != noCredential {
			n := 0
			for n < len(raw) && r.valueByte(raw[n]) {
				b := raw[n]
				if r.quote == 0 && (r.kind == secret || r.kind == bearer) && r.assignmentAt(b) == secret {
					// A nested assignment can extend past the bearer value.
					r.kind = secret
				}
				r.observe(b)
				n++
			}
			keep := min(n, maxPendingBytes-len(r.value))
			r.value = append(r.value, raw[:keep]...)
			r.overflow = r.overflow || keep < n
			raw = raw[n:]
			if len(raw) == 0 {
				return
			}
			r.emitValue(out, false)
		}
		b := raw[0]
		if kind := r.assignmentAt(b); kind != noCredential {
			r.kind = kind
			r.labelLen = 0
			continue
		}
		if !r.prevWord {
			for _, entry := range credentialPrefixes {
				if bytes.HasPrefix(raw, []byte(entry.prefix)) {
					r.kind = entry.kind
					break
				}
				if bytes.HasPrefix([]byte(entry.prefix), raw) {
					r.fragment = bytes.Clone(raw)
					return
				}
			}
			if r.kind != noCredential {
				continue
			}
		}
		out.WriteByte(b)
		r.observe(b)
		raw = raw[1:]
	}
}

func (r *textRedactor) finish(out *strings.Builder) {
	if r.kind != noCredential {
		r.emitValue(out, true)
	}
	out.Write(r.fragment)
}

func (r *textRedactor) emitValue(out *strings.Builder, eof bool) {
	kind := r.kind
	if kind == openAI && bytes.HasPrefix(r.value, []byte("sk-ant-")) {
		kind = "anthropic_key"
	}
	value := string(r.value)
	switch {
	case r.overflow, kind == secret && r.quote != 0:
		value = ""
	case kind == secret:
		if utf8.RuneCount(r.value) >= 6 || eof {
			value = ""
		}
	case kind == bearer:
		if len(r.value) >= 8 || eof {
			value = ""
		}
	default:
		value = redact.String(value)
		if eof && value == string(r.value) {
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
	if r.labelLen != 0 && (space || b == ':' || b == '=' || quoteByte(b)) {
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
			return noCredential
		}
	case bearerWord:
		if (space || quoteByte(b)) && r.bearerPos == 0 {
			return noCredential
		}
		const word = "bearer"
		if b|0x20 == word[r.bearerPos] {
			r.bearerPos++
			if r.bearerPos == len(word) {
				r.assignment = bearerSpace
			}
			return noCredential
		}
	case bearerSpace:
		if space {
			r.assignment = bearerValue
			return noCredential
		}
	case bearerValue:
		if space {
			return noCredential
		}
		r.assignment = noAssignment
		if credentialByte(bearer, b) {
			return bearer
		}
	}
	r.assignment = noAssignment
	return noCredential
}
