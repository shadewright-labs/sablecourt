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

type councilOutput struct {
	buf      bytes.Buffer
	overflow bool
}

func (b *councilOutput) Write(p []byte) (int, error) {
	if b.overflow {
		return len(p), nil
	}
	if len(p) > maxCouncilOutput-b.buf.Len() {
		b.overflow = true
		b.buf.Reset()
		return len(p), nil
	}
	return b.buf.Write(p)
}

var privateKeyStart = regexp.MustCompile(`-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----`)

func (b *councilOutput) visible(prompt, lease string) string {
	if b.overflow {
		return outputLimitMarker
	}
	raw := b.buf.String()
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
		return outputLimitMarker
	}
	return clean
}
