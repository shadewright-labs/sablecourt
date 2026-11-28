package attach

import (
	"context"
	"errors"
	"io"
	"sync"
	"time"

	"github.com/shadewright-labs/sablecourt/internal/redact"
	"github.com/shadewright-labs/sablecourt/internal/store"
)

const defaultRingSize = 64 * 1024

// GapMarker is sent to a Scry subscriber before the next chunk it receives
// after it missed chunks (its channel was full), so the viewer does not join
// two chunks the redactor never saw side by side.
const GapMarker = "\n[scry: output dropped]\n"

var (
	ErrSessionNotFound     = errors.New("live attach session not found")
	ErrTakeoverHeld        = errors.New("takeover already held by another operator")
	ErrTakeoverUnsupported = errors.New("process does not accept stdin takeover")
	// ErrTakeoverNotHeld refuses input from an operator who does not hold a
	// confirmed takeover.
	ErrTakeoverNotHeld = errors.New("operator does not hold the takeover")
)

type session struct {
	mu            sync.Mutex
	ring          []byte
	maxRing       int
	output        streamRedactor
	stdin         io.WriteCloser
	takeoverOwner string
	// takeoverPending is set from AcquireTakeover until ConfirmTakeover:
	// the owner holds the takeover against other operators but may not
	// write input yet.
	takeoverPending bool
	closed          bool
	subs            map[int]*subscriber
	nextSubID       int
	registry        *redact.Registry
}

// subscriber is one Scry observer. dropped is set when a chunk could not be
// delivered, and cleared once the gap marker is sent.
type subscriber struct {
	ch      chan string
	dropped bool
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
		maxRing:  defaultRingSize,
		output:   newStreamRedactor(reg),
		stdin:    stdin,
		subs:     make(map[int]*subscriber),
		registry: reg,
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
	for _, sub := range s.subs {
		if sub.dropped {
			select {
			case sub.ch <- s.gapMarker():
				sub.dropped = false
			default:
				continue
			}
		}
		select {
		case sub.ch <- clean:
		default:
			// Drop chunk for slow consumer rather than blocking the agent
			// process, and mark the gap before the next chunk it gets.
			sub.dropped = true
		}
	}
}

func (s *session) gapMarker() string {
	if s.registry == nil {
		return GapMarker
	}
	return s.registry.RedactExact(GapMarker)
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
	// The ring was filtered when it was written. A value registered since
	// then is caught here, so replay matches the registry as it is now.
	history := string(s.ring)
	if s.registry != nil {
		history = s.registry.RedactExact(history)
	}
	if s.closed {
		ch := make(chan string)
		close(ch)
		return history, ch, true, func() {}, nil
	}
	id := s.nextSubID
	s.nextSubID++
	ch := make(chan string, 64)
	s.subs[id] = &subscriber{ch: ch}
	unsub := func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		if sub, exists := s.subs[id]; exists {
			delete(s.subs, id)
			close(sub.ch)
		}
	}
	return history, ch, false, unsub, nil
}

// AcquireTakeover reserves exclusive stdin control for operatorID and
// reports whether this call made the reservation. The reservation refuses
// other operators but allows no input until ConfirmTakeover, so the caller
// can record the takeover first and drop the reservation with
// AbandonTakeover when the record cannot be written. An operator who already
// holds the takeover, pending or confirmed, keeps it as it is, and the call
// reports no reservation, so the caller has nothing to drop.
func (h *Hub) AcquireTakeover(taskID, operatorID string) (bool, error) {
	h.mu.RLock()
	s, ok := h.sessions[taskID]
	h.mu.RUnlock()
	if !ok {
		return false, ErrSessionNotFound
	}
	if operatorID == "" {
		operatorID = "operator"
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return false, ErrSessionNotFound
	}
	if s.stdin == nil {
		return false, ErrTakeoverUnsupported
	}
	if s.takeoverOwner != "" && s.takeoverOwner != operatorID {
		return false, ErrTakeoverHeld
	}
	if s.takeoverOwner == operatorID {
		return false, nil
	}
	s.takeoverOwner = operatorID
	s.takeoverPending = true
	return true, nil
}

// AbandonTakeover drops a reservation operatorID made with AcquireTakeover
// while it is still pending. A confirmed takeover is kept.
func (h *Hub) AbandonTakeover(taskID, operatorID string) {
	h.mu.RLock()
	s, ok := h.sessions[taskID]
	h.mu.RUnlock()
	if !ok {
		return
	}
	if operatorID == "" {
		operatorID = "operator"
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.takeoverOwner == operatorID && s.takeoverPending {
		s.takeoverOwner = ""
		s.takeoverPending = false
	}
}

// ConfirmTakeover lets operatorID, who reserved the takeover with
// AcquireTakeover, write input, and notes the takeover on ctx
// (store.NoteEffect). It fails when the reservation is gone.
func (h *Hub) ConfirmTakeover(ctx context.Context, taskID, operatorID string) error {
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
	if s.takeoverOwner != operatorID {
		return ErrTakeoverNotHeld
	}
	if s.takeoverPending {
		s.takeoverPending = false
		store.NoteEffect(ctx, "operator %s took over task %s", operatorID, taskID)
	}
	return nil
}

// ReleaseTakeover relinquishes exclusive stdin control and notes a released
// takeover on ctx (store.NoteEffect).
func (h *Hub) ReleaseTakeover(ctx context.Context, taskID, operatorID string) {
	h.mu.RLock()
	s, ok := h.sessions[taskID]
	h.mu.RUnlock()
	if !ok {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.takeoverOwner != "" && (operatorID == "" || s.takeoverOwner == operatorID) {
		if s.takeoverPending {
			store.NoteEffect(ctx, "the pending takeover of task %s by %s was dropped", taskID, s.takeoverOwner)
		} else {
			store.NoteEffect(ctx, "the takeover of task %s by %s was released", taskID, s.takeoverOwner)
		}
		s.takeoverOwner = ""
		s.takeoverPending = false
	}
}

// SendInput writes operator input to the running process stdin. operatorID
// must hold a confirmed takeover: input never takes over implicitly, so no
// input reaches a task without a recorded takeover.
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
	if s.takeoverOwner != operatorID || s.takeoverPending {
		s.mu.Unlock()
		return 0, ErrTakeoverNotHeld
	}
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
	s.takeoverPending = false
	if s.stdin != nil {
		_ = s.stdin.Close()
	}
	for id, sub := range s.subs {
		delete(s.subs, id)
		close(sub.ch)
	}
}
