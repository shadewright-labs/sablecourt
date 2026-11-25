package redact

import (
	"regexp"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
)

const (
	// maxJSONNesting bounds the container stack, so the scanner's extra
	// memory is fixed. A deeper document is redacted as text.
	maxJSONNesting = 512
	// maxStringNesting bounds how many times a string holding JSON is parsed
	// again (JSON inside a string inside a string, and so on).
	maxStringNesting = 4
	// unverifiableMarker replaces a value the text redactor cannot vouch for.
	unverifiableMarker = "[REDACTED:unverifiable]"
	secretMarker       = "[REDACTED:secret]"
)

var sensitiveKey = regexp.MustCompile(`(?i)^(?:` + sensitiveField + `|authorization)$`)

// sensitiveNonString finds a secret-named field whose value is not a JSON
// string (a number, object, array or literal): the text rules only cover
// string and bare-word values.
var sensitiveNonString = regexp.MustCompile(`(?i)"(?:` + sensitiveField + `|authorization)"\s*:\s*[^"\s]`)

// JSON redacts a JSON document token by token, so the result is still valid
// JSON with its keys in the same order and every duplicate member kept. Each
// string passes the text redactor; a string that itself holds JSON is redacted
// as JSON, up to a fixed depth. Every value beneath a secret-named key (keys
// are compared after unescaping, ignoring case) is replaced, whatever its
// nesting, except literals and empty strings. Input that does not parse, or
// is nested too deep, is redacted as text and withheld whole when the text
// redactor cannot vouch for it. Input without a hit is returned unchanged.
//
// Time is linear in the input. Extra memory apart from the output is the
// fixed container stack and the decoded form of the string being redacted.
func JSON(input string) string {
	if input == "" {
		return ""
	}
	out, changed := redactJSONOrText(input, 0)
	if !changed {
		return input
	}
	return out
}

// redactJSONOrText redacts s as a JSON document if it parses, otherwise as
// vouched text.
func redactJSONOrText(s string, strDepth int) (string, bool) {
	sc := jsonScanner{in: s, strDepth: strDepth}
	if sc.document() {
		if !sc.changed {
			return s, false
		}
		return sc.out.String(), true
	}
	return vouchedText(s)
}

// vouchedText runs the text redactor and checks its result: if unescaping
// JSON escapes reveals more to redact, or a secret-named field still holds a
// non-string value, the text redactor cannot vouch for it and the whole value
// is withheld.
func vouchedText(s string) (string, bool) {
	clean := String(s)
	u := clean
	for range maxStringNesting + 1 {
		next := unescapeLoose(u)
		if next == u {
			break
		}
		u = next
	}
	if String(u) != u || sensitiveNonString.MatchString(u) {
		return unverifiableMarker, true
	}
	return clean, clean != s
}

// unescapeLoose decodes JSON backslash escapes wherever they appear and keeps
// any other backslash as it is.
func unescapeLoose(s string) string {
	i := strings.IndexByte(s, '\\')
	if i < 0 {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	b.WriteString(s[:i])
	for i < len(s) {
		c := s[i]
		if c != '\\' || i+1 >= len(s) {
			b.WriteByte(c)
			i++
			continue
		}
		switch e := s[i+1]; e {
		case '"', '\\', '/':
			b.WriteByte(e)
			i += 2
		case 'n':
			b.WriteByte('\n')
			i += 2
		case 't':
			b.WriteByte('\t')
			i += 2
		case 'r':
			b.WriteByte('\r')
			i += 2
		case 'b', 'f':
			b.WriteByte(' ')
			i += 2
		case 'u':
			if r, ok := hex4(s, i+2); ok {
				b.WriteRune(r)
				i += 6
				continue
			}
			b.WriteByte(c)
			i++
		default:
			b.WriteByte(c)
			i++
		}
	}
	return b.String()
}

func hex4(s string, at int) (rune, bool) {
	if at+4 > len(s) {
		return 0, false
	}
	var r rune
	for _, c := range []byte(s[at : at+4]) {
		switch {
		case c >= '0' && c <= '9':
			r = r<<4 | rune(c-'0')
		case c >= 'a' && c <= 'f':
			r = r<<4 | rune(c-'a'+10)
		case c >= 'A' && c <= 'F':
			r = r<<4 | rune(c-'A'+10)
		default:
			return 0, false
		}
	}
	return r, true
}

// jsonScanner copies a JSON document to out byte for byte, replacing only the
// strings and numbers it redacts.
type jsonScanner struct {
	in       string
	pos      int
	out      strings.Builder // written only from the first change on
	changed  bool
	strDepth int
}

func closerOf(c byte) byte {
	if c == '{' {
		return '}'
	}
	return ']'
}

// document scans one JSON value and reports whether the whole input was a
// single valid value.
func (sc *jsonScanner) document() bool {
	var stack [maxJSONNesting]byte
	depth := 0
	// sensAt is the stack depth at which a secret-named subtree started, or
	// -1 outside one.
	sensAt := -1
	sensitive := false
	sc.ws()
	for {
		if sc.pos >= len(sc.in) {
			return false
		}
		switch c := sc.in[sc.pos]; c {
		case '{', '[':
			if depth == maxJSONNesting {
				return false
			}
			if sensitive && sensAt < 0 {
				sensAt = depth
			}
			stack[depth] = c
			depth++
			sc.emit(sc.in[sc.pos : sc.pos+1])
			sc.pos++
			sc.ws()
			if sc.pos < len(sc.in) && sc.in[sc.pos] == closerOf(c) {
				sc.emit(sc.in[sc.pos : sc.pos+1])
				sc.pos++
				depth--
				if sensAt == depth {
					sensAt = -1
				}
				break
			}
			if c == '{' {
				var ok bool
				if sensitive, ok = sc.key(sensAt >= 0); !ok {
					return false
				}
			} else {
				sensitive = sensAt >= 0
			}
			continue
		case '"':
			if !sc.stringValue(sensitive) {
				return false
			}
		default:
			if !sc.scalar(sensitive) {
				return false
			}
		}
		// After a value: close finished containers, then move to the next
		// member or element.
		for {
			sc.ws()
			if depth == 0 {
				return sc.pos == len(sc.in)
			}
			if sc.pos >= len(sc.in) {
				return false
			}
			top := stack[depth-1]
			c := sc.in[sc.pos]
			if c == ',' {
				sc.emit(sc.in[sc.pos : sc.pos+1])
				sc.pos++
				sc.ws()
				if top == '{' {
					var ok bool
					if sensitive, ok = sc.key(sensAt >= 0); !ok {
						return false
					}
				} else {
					sensitive = sensAt >= 0
				}
				break
			}
			if c != closerOf(top) {
				return false
			}
			sc.emit(sc.in[sc.pos : sc.pos+1])
			sc.pos++
			depth--
			if sensAt == depth {
				sensAt = -1
			}
		}
	}
}

// emit copies part of the input to the output. Until the first change the
// output is the input itself, so nothing is copied.
func (sc *jsonScanner) emit(s string) {
	if sc.changed {
		sc.out.WriteString(s)
	}
}

// replace starts writing at the first change, from the input before the token
// at start, then writes the replacement as a JSON string.
func (sc *jsonScanner) replace(start int, clean string) {
	if !sc.changed {
		sc.changed = true
		sc.out.Grow(len(sc.in) + len(clean))
		sc.out.WriteString(sc.in[:start])
	}
	writeJSONString(&sc.out, clean)
}

func (sc *jsonScanner) ws() {
	start := sc.pos
	for sc.pos < len(sc.in) {
		switch sc.in[sc.pos] {
		case ' ', '\t', '\n', '\r':
			sc.pos++
			continue
		}
		break
	}
	sc.emit(sc.in[start:sc.pos])
}

// key scans an object key and its colon and reports whether the member's
// value is secret.
func (sc *jsonScanner) key(inSecret bool) (bool, bool) {
	raw, decoded, ok := sc.str()
	if !ok {
		return false, false
	}
	clean, changed := sc.cleanString(decoded)
	sc.put(raw, clean, changed)
	sc.ws()
	if sc.pos >= len(sc.in) || sc.in[sc.pos] != ':' {
		return false, false
	}
	sc.emit(":")
	sc.pos++
	sc.ws()
	return inSecret || sensitiveKey.MatchString(decoded), true
}

func (sc *jsonScanner) stringValue(secret bool) bool {
	raw, decoded, ok := sc.str()
	if !ok {
		return false
	}
	if secret && decoded != "" {
		sc.put(raw, secretMarker, true)
		return true
	}
	clean, changed := sc.cleanString(decoded)
	sc.put(raw, clean, changed)
	return true
}

// cleanString redacts a decoded string: as JSON when it holds a document and
// the nesting limit allows, otherwise as vouched text.
func (sc *jsonScanner) cleanString(s string) (string, bool) {
	t := strings.TrimLeft(s, " \t\r\n")
	if sc.strDepth < maxStringNesting && t != "" && (t[0] == '{' || t[0] == '[' || t[0] == '"') {
		return redactJSONOrText(s, sc.strDepth+1)
	}
	return vouchedText(s)
}

// put writes the original string token, or the replacement encoded as a JSON
// string when it changed.
func (sc *jsonScanner) put(raw, clean string, changed bool) {
	if !changed {
		sc.emit(raw)
		return
	}
	sc.replace(sc.pos-len(raw), clean)
}

func (sc *jsonScanner) scalar(secret bool) bool {
	start := sc.pos
	for _, lit := range []string{"true", "false", "null"} {
		if strings.HasPrefix(sc.in[sc.pos:], lit) {
			sc.pos += len(lit)
			sc.emit(lit)
			return true
		}
	}
	if !sc.number() {
		return false
	}
	if secret {
		sc.replace(start, secretMarker)
		return true
	}
	sc.emit(sc.in[start:sc.pos])
	return true
}

// number consumes a JSON number.
func (sc *jsonScanner) number() bool {
	s, i := sc.in, sc.pos
	digits := func() int {
		n := 0
		for i < len(s) && s[i] >= '0' && s[i] <= '9' {
			i++
			n++
		}
		return n
	}
	if i < len(s) && s[i] == '-' {
		i++
	}
	if i < len(s) && s[i] == '0' {
		i++
	} else if digits() == 0 {
		return false
	}
	if i < len(s) && s[i] == '.' {
		i++
		if digits() == 0 {
			return false
		}
	}
	if i < len(s) && (s[i] == 'e' || s[i] == 'E') {
		i++
		if i < len(s) && (s[i] == '+' || s[i] == '-') {
			i++
		}
		if digits() == 0 {
			return false
		}
	}
	sc.pos = i
	return true
}

// str consumes a JSON string token and returns it raw and decoded.
func (sc *jsonScanner) str() (raw, decoded string, ok bool) {
	s := sc.in
	start := sc.pos
	if start >= len(s) || s[start] != '"' {
		return "", "", false
	}
	i := start + 1
	escaped := false
	for i < len(s) && s[i] != '"' {
		switch {
		case s[i] < 0x20:
			return "", "", false
		case s[i] == '\\':
			escaped = true
			if i+1 >= len(s) {
				return "", "", false
			}
			if s[i+1] == 'u' {
				if _, ok := hex4(s, i+2); !ok {
					return "", "", false
				}
				i += 6
				continue
			}
			if !strings.ContainsRune(`"\/bfnrt`, rune(s[i+1])) {
				return "", "", false
			}
			i += 2
		default:
			i++
		}
	}
	if i >= len(s) {
		return "", "", false
	}
	sc.pos = i + 1
	raw = s[start:sc.pos]
	body := s[start+1 : i]
	if !escaped {
		return raw, body, true
	}
	return raw, decodeJSONString(body), true
}

// decodeJSONString decodes the body of a string the scanner already checked.
func decodeJSONString(body string) string {
	var b strings.Builder
	b.Grow(len(body))
	for i := 0; i < len(body); {
		c := body[i]
		if c != '\\' {
			b.WriteByte(c)
			i++
			continue
		}
		e := body[i+1]
		i += 2
		switch e {
		case 'b':
			b.WriteByte('\b')
		case 'f':
			b.WriteByte('\f')
		case 'n':
			b.WriteByte('\n')
		case 'r':
			b.WriteByte('\r')
		case 't':
			b.WriteByte('\t')
		case 'u':
			r, _ := hex4(body, i)
			i += 4
			if utf16.IsSurrogate(r) {
				if i+6 <= len(body) && body[i] == '\\' && body[i+1] == 'u' {
					if r2, ok := hex4(body, i+2); ok {
						if dec := utf16.DecodeRune(r, r2); dec != utf8.RuneError {
							b.WriteRune(dec)
							i += 6
							continue
						}
					}
				}
				r = utf8.RuneError
			}
			b.WriteRune(r)
		default:
			b.WriteByte(e)
		}
	}
	return b.String()
}

func writeJSONString(b *strings.Builder, s string) {
	const hexDigits = "0123456789abcdef"
	b.WriteByte('"')
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '"' || c == '\\':
			b.WriteByte('\\')
			b.WriteByte(c)
		case c == '\n':
			b.WriteString(`\n`)
		case c == '\r':
			b.WriteString(`\r`)
		case c == '\t':
			b.WriteString(`\t`)
		case c < 0x20:
			b.Write([]byte{'\\', 'u', '0', '0', hexDigits[c>>4], hexDigits[c&0xf]})
		default:
			b.WriteByte(c)
		}
	}
	b.WriteByte('"')
}
