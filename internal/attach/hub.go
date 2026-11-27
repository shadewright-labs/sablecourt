package attach

import (
	"errors"
	"io"
	"sync"
	"time"

	"github.com/shadewright-labs/sablecourt/internal/redact"
)

const defaultRingSize = 64 * 1024

var (
	ErrSessionNotFound     = errors.New("live attach session not found")
	ErrTakeoverHeld        = errors.New("takeover already held by another operator")
	ErrTakeoverUnsupported = errors.New("process does not accept stdin takeover")
)

type session struct {
	mu            sync.Mutex
	ring          []byte
	maxRing       int
	output        streamRedactor
	stdin         io.WriteCloser
	takeoverOwner string
	closed        bool
	subs          map[int]chan string
	nextSubID     int
}

// Hub multiplexes live redacted output (Scry) and exclusive stdin control (Takeover)
// for running Sablecourt tasks.
type Hub struct {
	mu       sync.RWMutex
	sessions map[string]*session
	registry *redact.Registry
}

func NewHub() *Hub {
	return NewHubWithRegistry(redact.DefaultRegistry())
}

// NewHubWithRegistry returns a Hub whose live output redaction consults reg.
func NewHubWithRegistry(reg *redact.Registry) *Hub {
	if reg == nil {
		reg = redact.DefaultRegistry()
	}
	return &Hub{
		sessions: make(map[string]*session),
		registry: reg,
	}
}

func (h *Hub) secretRegistry() *redact.Registry {
	if h != nil && h.registry != nil {
		return h.registry
	}
	return redact.DefaultRegistry()
}

// Register creates or replaces a live attach session for taskID.
// stdin may be nil if the runtime only supports read-only Scry.
func (h *Hub) Register(taskID string, stdin io.WriteCloser) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if old, ok := h.sessions[taskID]; ok {
		old.closeLocked()
	}
	reg := h.secretRegistry()
	h.sessions[taskID] = &session{
		maxRing: defaultRingSize,
		output:  newStreamRedactor(reg),
		stdin:   stdin,
		subs:    make(map[int]chan string),
	}
}

// WriteOutput redacts credentials across writes and sends safe output to the
// history ring and active Scry/Takeover subscribers.
func (h *Hub) WriteOutput(taskID string, raw []byte) {
	if len(raw) == 0 {
		return
	}
	h.mu.RLock()
	s, ok := h.sessions[taskID]
	h.mu.RUnlock()
	if !ok {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	s.emitLocked(s.output.write(raw, false))
}

func (s *session) emitLocked(clean string) {
	if clean == "" {
		return
	}
	s.ring = append(s.ring, []byte(clean)...)
	if len(s.ring) > s.maxRing {
		s.ring = append([]byte(nil), s.ring[len(s.ring)-s.maxRing:]...)
	}
	for _, ch := range s.subs {
		select {
		case ch <- clean:
		default:
			// Drop chunk for slow consumer rather than blocking the agent process.
		}
	}
}

// Subscribe attaches a read-only Scry observer. Returns buffered history, a live chunk channel,
// whether the session is already closed, and an unsubscribe callback.
func (h *Hub) Subscribe(taskID string) (string, <-chan string, bool, func(), error) {
	h.mu.RLock()
	s, ok := h.sessions[taskID]
	h.mu.RUnlock()
	if !ok {
		return "", nil, false, nil, ErrSessionNotFound
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	history := string(s.ring)
	if s.closed {
		ch := make(chan string)
		close(ch)
		return history, ch, true, func() {}, nil
	}
	id := s.nextSubID
	s.nextSubID++
	ch := make(chan string, 64)
	s.subs[id] = ch
	unsub := func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		if c, exists := s.subs[id]; exists {
			delete(s.subs, id)
			close(c)
		}
	}
	return history, ch, false, unsub, nil
}

// AcquireTakeover grants exclusive write access to stdin for operatorID.
func (h *Hub) AcquireTakeover(taskID, operatorID string) error {
	h.mu.RLock()
	s, ok := h.sessions[taskID]
	h.mu.RUnlock()
	if !ok {
		return ErrSessionNotFound
	}
	if operatorID == "" {
		operatorID = "operator"
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrSessionNotFound
	}
	if s.stdin == nil {
		return ErrTakeoverUnsupported
	}
	if s.takeoverOwner != "" && s.takeoverOwner != operatorID {
		return ErrTakeoverHeld
	}
	s.takeoverOwner = operatorID
	return nil
}

// ReleaseTakeover relinquishes exclusive stdin control.
func (h *Hub) ReleaseTakeover(taskID, operatorID string) {
	h.mu.RLock()
	s, ok := h.sessions[taskID]
	h.mu.RUnlock()
	if !ok {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if operatorID == "" || s.takeoverOwner == operatorID {
		s.takeoverOwner = ""
	}
}

// SendInput writes operator input to the running process stdin.
// If no operator holds Takeover yet, it automatically acquires it for operatorID.
func (h *Hub) SendInput(taskID, operatorID string, data []byte) (int, error) {
	h.mu.RLock()
	s, ok := h.sessions[taskID]
	h.mu.RUnlock()
	if !ok {
		return 0, ErrSessionNotFound
	}
	if operatorID == "" {
		operatorID = "operator"
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return 0, ErrSessionNotFound
	}
	if s.stdin == nil {
		s.mu.Unlock()
		return 0, ErrTakeoverUnsupported
	}
	if s.takeoverOwner != "" && s.takeoverOwner != operatorID {
		s.mu.Unlock()
		return 0, ErrTakeoverHeld
	}
	s.takeoverOwner = operatorID
	stdin := s.stdin
	s.mu.Unlock()

	return stdin.Write(data)
}

// Status reports whether a live attach session exists and who holds Takeover.
func (h *Hub) Status(taskID string) (active bool, canTakeover bool, owner string) {
	h.mu.RLock()
	s, ok := h.sessions[taskID]
	h.mu.RUnlock()
	if !ok {
		return false, false, ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return !s.closed, s.stdin != nil && !s.closed, s.takeoverOwner
}

// Close marks a task session finished, closes all active Scry subscribers,
// and evicts the buffered session from memory after a short grace period.
func (h *Hub) Close(taskID string) {
	h.mu.RLock()
	s, ok := h.sessions[taskID]
	h.mu.RUnlock()
	if !ok {
		return
	}
	s.closeLocked()
	time.AfterFunc(10*time.Minute, func() {
		h.mu.Lock()
		if cur, exists := h.sessions[taskID]; exists && cur == s {
			delete(h.sessions, taskID)
		}
		h.mu.Unlock()
	})
}

func (s *session) closeLocked() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	s.emitLocked(s.output.write(nil, true))
	s.output = streamRedactor{}
	s.closed = true
	s.takeoverOwner = ""
	if s.stdin != nil {
		_ = s.stdin.Close()
	}
	for id, ch := range s.subs {
		delete(s.subs, id)
		close(ch)
	}
}
