package docker

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/shadewright-labs/sablecourt/internal/redact"
)

// copyDockerStreams uses a fixed buffer even when the peer declares a huge frame.
func copyDockerStreams(r io.Reader, stdout, stderr io.Writer) error {
	var header [8]byte
	var buf [32 * 1024]byte
	for {
		_, err := io.ReadFull(r, header[:])
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		dst := stdout
		switch header[0] {
		case 1:
		case 2:
			dst = stderr
		default:
			return errors.New("invalid Docker stream type")
		}
		if header[1] != 0 || header[2] != 0 || header[3] != 0 {
			return errors.New("invalid Docker stream header")
		}
		left := binary.BigEndian.Uint32(header[4:])
		for left > 0 {
			chunk := buf[:min(left, uint32(len(buf)))]
			if _, err := io.ReadFull(r, chunk); err != nil {
				return err
			}
			n, err := dst.Write(chunk)
			if err != nil {
				return err
			}
			if n < 0 || n > len(buf) || n != len(chunk) {
				return io.ErrShortWrite
			}
			left -= uint32(n)
		}
	}
}

const maxCouncilOutput = 1 << 20
const outputLimitMarker = "[REDACTED:output limit]"

// councilOutput captures one stream of a council or command container, at
// most maxCouncilOutput bytes of it. Past that limit a council stream is
// dropped whole and replaced by outputLimitMarker. A command stream sets
// keepTail: it keeps its last maxCouncilOutput bytes instead, so the final
// diagnostic of a command survives, and still reports the overflow.
type councilOutput struct {
	buf      bytes.Buffer
	overflow bool
	keepTail bool
}

func (b *councilOutput) Write(p []byte) (int, error) {
	n := len(p)
	if b.overflow && !b.keepTail {
		return n, nil
	}
	if len(p) > maxCouncilOutput-b.buf.Len() {
		b.overflow = true
		if !b.keepTail {
			b.buf.Reset()
			return n, nil
		}
	}
	if b.keepTail && len(p) >= maxCouncilOutput {
		b.buf.Reset()
		p = p[len(p)-maxCouncilOutput:]
	}
	b.buf.Write(p)
	// Compact only past twice the limit, so each byte is copied a bounded
	// number of times and the buffer never holds more than 2 MiB.
	if b.keepTail && b.buf.Len() > 2*maxCouncilOutput {
		tail := bytes.Clone(b.buf.Bytes()[b.buf.Len()-maxCouncilOutput:])
		b.buf.Reset()
		b.buf.Write(tail)
	}
	return n, nil
}

var (
	privateKeyStart = regexp.MustCompile(`-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----`)
	privateKeyEnd   = regexp.MustCompile(`-----END [A-Z0-9 ]*PRIVATE KEY-----`)
)

func (b *councilOutput) visible(prompt, lease string) string {
	raw := b.buf.String()
	if b.overflow {
		if !b.keepTail {
			return outputLimitMarker
		}
		var ok bool
		if raw, ok = overflowTail(raw); !ok {
			return outputLimitMarker
		}
	}
	for _, secret := range []string{lease, prompt} {
		if secret != "" {
			raw = strings.ReplaceAll(raw, secret, "[REDACTED]")
		}
	}
	clean := redact.String(raw)
	if start := privateKeyStart.FindStringIndex(clean); start != nil {
		clean = clean[:start[0]] + "[REDACTED:private_key]"
	}
	if len(clean) > maxCouncilOutput {
		if !b.keepTail {
			return outputLimitMarker
		}
		// Redaction ran before this cut, so the cut cannot expose a secret.
		clean = utf8Suffix(clean, maxCouncilOutput)
	}
	return clean
}

// overflowTail trims the kept tail of an overflowed stream before redaction:
// it keeps its last maxCouncilOutput bytes, then drops the first, partial
// line, since a secret cut in half there would match no rule and no exact
// value. A tail that starts inside a private key is dropped through the last
// END line before the first BEGIN line. It reports false when no whole line
// is left.
func overflowTail(raw string) (string, bool) {
	if len(raw) > maxCouncilOutput {
		raw = raw[len(raw)-maxCouncilOutput:]
	}
	nl := strings.IndexByte(raw, '\n')
	if nl < 0 {
		return "", false
	}
	raw = raw[nl+1:]
	head := raw
	if start := privateKeyStart.FindStringIndex(raw); start != nil {
		head = raw[:start[0]]
	}
	if ends := privateKeyEnd.FindAllStringIndex(head, -1); len(ends) > 0 {
		raw = raw[ends[len(ends)-1][1]:]
	}
	return raw, true
}

// utf8Suffix returns at most n bytes from the end of s, starting on a rune.
func utf8Suffix(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := len(s) - n
	for cut < len(s) && !utf8.RuneStart(s[cut]) {
		cut++
	}
	return s[cut:]
}
