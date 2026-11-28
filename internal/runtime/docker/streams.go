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
	// keyOpen records that the bytes compaction dropped end inside a
	// private key: a BEGIN line with no END line after it. The key body
	// then runs from the start of buf to the first END line in it, and
	// visible hides it although its BEGIN line is gone.
	keyOpen bool
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
	b.buf.Write(p)
	// Compact only past twice the window, so each byte is copied a bounded
	// number of times and the buffer holds at most twice the window plus
	// one write. Compaction is the only place that drops raw bytes.
	if b.buf.Len() > 2*window {
		b.compact(window)
	}
	return n, nil
}

var (
	privateKeyStart = regexp.MustCompile(`-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----`)
	privateKeyEnd   = regexp.MustCompile(`-----END [A-Z0-9 ]*PRIVATE KEY-----`)
	// privateKeyStartOpen is a BEGIN line still running at the end of the
	// buffer, too long to tell yet whether it starts a private key. It is
	// treated as one.
	privateKeyStartOpen = regexp.MustCompile(`-----BEGIN [A-Z0-9 ]*\z`)
)

// compact drops all but the last window bytes of the buffer and records in
// keyOpen whether the dropped bytes end inside a private key. A BEGIN or END
// line counts as dropped when it starts in the dropped bytes, so one split
// by the compaction point is not lost.
func (b *councilOutput) compact(window int) {
	data := b.buf.Bytes()
	drop := len(data) - window
	last, open := -1, b.keyOpen
	for _, m := range privateKeyStart.FindAllIndex(data, -1) {
		if m[0] < drop && m[0] > last {
			last, open = m[0], true
		}
	}
	for _, m := range privateKeyEnd.FindAllIndex(data, -1) {
		if m[0] < drop && m[0] > last {
			last, open = m[0], false
		}
	}
	if m := privateKeyStartOpen.FindIndex(data); m != nil && m[0] < drop && m[0] > last {
		open = true
	}
	b.keyOpen = open
	tail := bytes.Clone(data[drop:])
	b.buf.Reset()
	b.buf.Write(tail)
}

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
	if b.keyOpen {
		// The BEGIN line of the key the buffer starts in was dropped: put
		// one back, so redact.Tail hides the body up to its END line, or
		// everything when there is none.
		raw = "-----BEGIN " + "PRIVATE KEY-----\n" + raw
	}
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
		return redact.String(s)
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
