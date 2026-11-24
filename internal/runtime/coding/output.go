package coding

import (
	"bytes"
	"os"
	"regexp"
	"strings"

	"github.com/shadewright-labs/sablecourt/internal/attach"
	"github.com/shadewright-labs/sablecourt/internal/redact"
)

const maxSessionLine = 1024 * 1024

var (
	privateKeyStart = regexp.MustCompile(`-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----`)
	credentialTail  = regexp.MustCompile(`(?i)(?:\b[a-z0-9_-]*(?:api[_-]?key|secret|password|passwd|access[_-]?token)\b\s*[:=]|authorization\s*:\s*(?:bearer)?)\s*$`)
	privateKeyEnd   = regexp.MustCompile(`-----END [A-Z0-9 ]*PRIVATE KEY-----`)
)

type sessionOutput struct {
	file       *os.File
	hub        *attach.Hub
	taskID     string
	pending    []byte
	lineStart  int
	privateKey bool
	suppressed bool
}

func (s *sessionOutput) Write(chunk []byte) (int, error) {
	n := len(chunk)
	for len(chunk) > 0 && !s.suppressed {
		newline := bytes.IndexByte(chunk, '\n')
		size := len(chunk)
		if newline >= 0 {
			size = newline + 1
		}
		if len(s.pending)+size > maxSessionLine {
			// The discarded line might contain the start of a private key block.
			s.pending = nil
			s.suppressed = true
			return n, s.emit("[REDACTED:output limit]\n")
		}
		s.pending = append(s.pending, chunk[:size]...)
		chunk = chunk[size:]
		if newline >= 0 {
			line := s.pending[s.lineStart:]
			trimmed := bytes.TrimSpace(line)
			continuation := s.lineStart > 0 && (len(trimmed) == 0 || bytes.EqualFold(trimmed, []byte("Bearer")))
			if credentialTail.Match(line) || continuation {
				s.lineStart = len(s.pending)
				continue
			}
			s.lineStart = 0
			err := s.writeLine(string(s.pending))
			s.pending = s.pending[:0]
			if err != nil {
				return n - len(chunk), err
			}
		}
	}
	return n, nil
}

func (s *sessionOutput) Flush() error {
	err := s.writeLine(string(s.pending))
	s.pending = nil
	s.lineStart = 0
	return err
}

func (s *sessionOutput) writeLine(line string) error {
	var visible strings.Builder
	for line != "" {
		if s.privateKey {
			end := privateKeyEnd.FindStringIndex(line)
			if end == nil {
				break
			}
			s.privateKey = false
			line = line[end[1]:]
			continue
		}
		start := privateKeyStart.FindStringIndex(line)
		if start == nil {
			visible.WriteString(line)
			break
		}
		visible.WriteString(line[:start[0]])
		visible.WriteString("[REDACTED:private_key]")
		s.privateKey = true
		line = line[start[1]:]
	}
	return s.emit(redact.String(visible.String()))
}

func (s *sessionOutput) emit(clean string) error {
	if clean == "" {
		return nil
	}
	if s.hub != nil {
		s.hub.WriteOutput(s.taskID, []byte(clean))
	}
	_, err := s.file.WriteString(clean)
	return err
}
