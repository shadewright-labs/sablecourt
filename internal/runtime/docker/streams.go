package docker

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"regexp"
	"strings"

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

// outputLookback is how much of a command stream the runner keeps before
// its last maxCouncilOutput bytes. It is redacted together with the tail, so
// a secret that starts before the cut (a key name, a header, the start of a
// token) is replaced whole, but none of it is shown.
const outputLookback = 64 << 10

// councilOutput captures one stream of a council or command container, at
// most maxCouncilOutput bytes of it. Past that limit a council stream is
// dropped whole and replaced by outputLimitMarker. A command stream sets
// keepTail: it keeps its last maxCouncilOutput bytes, plus outputLookback
// bytes of context before them, so the final diagnostic of a command
// survives, and still reports the overflow.
type councilOutput struct {
	buf      bytes.Buffer
	total    int64
	overflow bool
	keepTail bool
	// limit and lookback replace maxCouncilOutput and outputLookback in
	// tests when set.
	limit, lookback int
}

func (b *councilOutput) limits() (limit, window int) {
	limit, lookback := maxCouncilOutput, outputLookback
	if b.limit > 0 {
		limit, lookback = b.limit, b.lookback
	}
	return limit, limit + lookback
}

func (b *councilOutput) Write(p []byte) (int, error) {
	n := len(p)
	if b.overflow && !b.keepTail {
		return n, nil
	}
	limit, window := b.limits()
	b.total += int64(n)
	if b.total > int64(limit) {
		b.overflow = true
		if !b.keepTail {
			b.buf.Reset()
			return n, nil
		}
	}
	if len(p) >= window {
		b.buf.Reset()
		p = p[len(p)-window:]
	}
	b.buf.Write(p)
	// Compact only past twice the window, so each byte is copied a bounded
	// number of times and the buffer holds at most twice the window.
	if b.buf.Len() > 2*window {
		tail := bytes.Clone(b.buf.Bytes()[b.buf.Len()-window:])
		b.buf.Reset()
		b.buf.Write(tail)
	}
	return n, nil
}

var privateKeyStart = regexp.MustCompile(`-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----`)

// visible returns the stream as it may be shown or stored: redacted, then
// cut to the limit by redact.Tail, which owns the order of redaction and
// cuts. Any cut, by the stream's size or by redaction growth, sets overflow.
// A council stream that was cut is replaced by outputLimitMarker; so is a
// command stream with no whole line left after its cut.
func (b *councilOutput) visible(prompt, lease string) string {
	if b.overflow && !b.keepTail {
		return outputLimitMarker
	}
	limit, _ := b.limits()
	raw := b.buf.String()
	from := 0
	if b.overflow {
		// The stream is longer than the window: its last limit bytes are
		// kept, the bytes before them are context only.
		from = max(len(raw)-limit, 1)
	}
	clean := func(s string) string {
		for _, secret := range []string{lease, prompt} {
			if secret != "" {
				s = strings.ReplaceAll(s, secret, "[REDACTED]")
			}
		}
		s = redact.String(s)
		// A private key with no END line matches no rule: drop it and
		// everything after it.
		if start := privateKeyStart.FindStringIndex(s); start != nil {
			s = s[:start[0]] + "[REDACTED:private_key]"
		}
		return s
	}
	out, cut := redact.Tail(raw, from, limit, clean)
	if cut {
		b.overflow = true
		if !b.keepTail || out == "" {
			return outputLimitMarker
		}
	}
	return out
}
