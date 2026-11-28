package coding

import (
	"bytes"
	"io"
	"regexp"
	"strings"
	"sync"

	"github.com/shadewright-labs/sablecourt/internal/attach"
	"github.com/shadewright-labs/sablecourt/internal/redact"
)

const (
	maxSessionLine = 1024 * 1024
	maxSessionLog  = 1024 * 1024
)

var (
	privateKeyStart = regexp.MustCompile(`-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----`)
	credentialTail  = regexp.MustCompile(`(?i)(?:\b[a-z0-9_-]*(?:api[_-]?key|secret|password|passwd|access[_-]?token)\b\s*[:=]|authorization\s*:\s*(?:bearer)?)\s*$`)
	privateKeyEnd   = regexp.MustCompile(`-----END [A-Z0-9 ]*PRIVATE KEY-----`)
)

type sessionOutput struct {
	archive    *boundedLog
	file       io.StringWriter
	tail       *diagnosticTail
	hub        *attach.Hub
	taskID     string
	known      *redact.Stream
	pending    []byte
	lineStart  int
	privateKey bool
	suppressed bool
}

func (s *sessionOutput) knownStream() *redact.Stream {
	if s.known == nil {
		s.known = redact.NewStream()
	}
	return s.known
}

func hasUnfinishedPrivateKey(text string) bool {
	starts := privateKeyStart.FindAllStringIndex(text, -1)
	return len(starts) > 0 && !privateKeyEnd.MatchString(text[starts[len(starts)-1][1]:])
}

func (s *sessionOutput) Write(chunk []byte) (int, error) {
	origLen := len(chunk)
	if origLen == 0 || s.suppressed {
		return origLen, nil
	}
	filtered := s.knownStream().Write(chunk, false)
	if err := s.writeFiltered(filtered); err != nil {
		return origLen, err
	}
	return origLen, nil
}

func (s *sessionOutput) writeFiltered(chunk []byte) error {
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
			return s.emit("[REDACTED:output limit]\n")
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
				return err
			}
		}
	}
	return nil
}

func (s *sessionOutput) Flush() error {
	if !s.suppressed {
		if tail := s.knownStream().Write(nil, true); len(tail) > 0 {
			if err := s.writeFiltered(tail); err != nil {
				s.pending = nil
				s.lineStart = 0
				return err
			}
		}
	}
	if s.suppressed {
		s.pending = nil
		s.lineStart = 0
		return nil
	}
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
	if s.tail != nil {
		s.tail.add(clean)
	}
	if s.hub != nil {
		s.hub.WriteOutput(s.taskID, []byte(clean))
	}
	if s.archive != nil {
		if _, err := s.archive.WriteString(clean); err != nil {
			return err
		}
	}
	_, err := s.file.WriteString(clean)
	return err
}

type boundedLog struct {
	live    io.Writer
	mu      sync.Mutex
	file    io.StringWriter
	written int
	full    bool
	partial bool
}

func (l *boundedLog) WriteString(record string) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	// A later record can come from the other stream after an incomplete line.
	line := record
	if l.partial {
		line = "\n" + line
	}
	l.partial = !strings.HasSuffix(line, "\n")
	if l.live != nil {
		_, _ = io.WriteString(l.live, line)
	}

	if l.full {
		return len(record), nil
	}
	if l.written+len(line) > maxSessionLog {
		l.full = true
		const marker = "\n...[truncated]\n"
		if l.written+len(marker) <= maxSessionLog {
			_, err := l.file.WriteString(marker)
			return len(record), err
		}
		return len(record), nil
	}
	n, err := l.file.WriteString(line)
	l.written += n
	return len(record), err
}
