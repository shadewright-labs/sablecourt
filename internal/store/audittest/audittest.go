// Package audittest wraps a store so tests can make audit writes fail.
package audittest

import (
	"context"
	"errors"
	"sync"

	"github.com/shadewright-labs/sablecourt/internal/domain"
	"github.com/shadewright-labs/sablecourt/internal/store"
)

// ErrInjected is the error a failed audit write returns.
var ErrInjected = errors.New("injected audit write failure")

// Store passes every call to the wrapped store, except AppendAudit, which
// fails while Failing is set and records each refused record.
type Store struct {
	store.Store

	mu      sync.Mutex
	failing bool
	match   func(domain.AuditRecord) bool
	refused []domain.AuditRecord
}

// Wrap returns st wrapped, with audit writes succeeding until Fail is called.
func Wrap(st store.Store) *Store {
	return &Store{Store: st}
}

// Fail makes every later audit write fail, or only those match accepts when
// match is not nil.
func (s *Store) Fail(match func(domain.AuditRecord) bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failing = true
	s.match = match
}

// Heal makes audit writes succeed again.
func (s *Store) Heal() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failing = false
	s.match = nil
}

// Refused returns the records whose write was made to fail.
func (s *Store) Refused() []domain.AuditRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]domain.AuditRecord(nil), s.refused...)
}

// AppendAudit fails while Fail is in effect and passes through otherwise.
func (s *Store) AppendAudit(ctx context.Context, rec domain.AuditRecord) error {
	s.mu.Lock()
	fail := s.failing && (s.match == nil || s.match(rec))
	if fail {
		s.refused = append(s.refused, rec)
	}
	s.mu.Unlock()
	if fail {
		return ErrInjected
	}
	return s.Store.AppendAudit(ctx, rec)
}

// Action matches records of the named action.
func Action(name string) func(domain.AuditRecord) bool {
	return func(rec domain.AuditRecord) bool { return rec.Action == name }
}
