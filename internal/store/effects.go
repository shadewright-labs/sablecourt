package store

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"sync"

	"github.com/shadewright-labs/sablecourt/internal/redact"
)

// Effects records what a request has changed so far: every store method that
// writes, and every runtime call that changes state (a takeover confirmed or
// released, a branch created or pushed, a tool or action run), appends one
// line when it succeeds. AuditFailureText builds its answer from these lines
// only, so no caller writes by hand what did or did not change.
type Effects struct {
	mu   sync.Mutex
	done []string
}

type effectsKey struct{}

// TrackEffects returns ctx carrying an effects recorder, and the recorder.
// When ctx already carries one, it is kept, so a request has one recorder
// however many handlers it passes through.
func TrackEffects(ctx context.Context) (context.Context, *Effects) {
	if e := effectsFrom(ctx); e != nil {
		return ctx, e
	}
	e := &Effects{}
	return context.WithValue(ctx, effectsKey{}, e), e
}

// TrackNewEffects returns ctx carrying a new effects recorder, and the
// recorder, replacing any recorder ctx carries. A caller that runs several
// operations on one context, such as tool calls, uses it for each one.
func TrackNewEffects(ctx context.Context) (context.Context, *Effects) {
	e := &Effects{}
	return context.WithValue(ctx, effectsKey{}, e), e
}

// TrackEffectsHandler gives every request to h an effects recorder.
func TrackEffectsHandler(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, _ := TrackEffects(r.Context())
		h.ServeHTTP(w, r.WithContext(ctx))
	})
}

func effectsFrom(ctx context.Context) *Effects {
	if ctx == nil {
		return nil
	}
	e, _ := ctx.Value(effectsKey{}).(*Effects)
	return e
}

// NoteEffect appends an effect, formatted from format and args, to the
// recorder of ctx. It does nothing when ctx carries none. The text is
// redacted, since names in it (memory keys, relic names) come from callers.
// Inside a transaction (see HoldEffects) the effect is held until the
// transaction commits.
func NoteEffect(ctx context.Context, format string, args ...any) {
	if effectsFrom(ctx) == nil {
		return
	}
	noteLine(ctx, redact.String(fmt.Sprintf(format, args...)))
}

func noteLine(ctx context.Context, text string) {
	if h, ok := ctx.Value(heldKey{}).(*heldEffects); ok && h.hold(text) {
		return
	}
	e := effectsFrom(ctx)
	if e == nil {
		return
	}
	e.mu.Lock()
	e.done = append(e.done, text)
	e.mu.Unlock()
}

// heldEffects buffers the effects noted while a transaction is open. Commit
// releases them to the recorder when the transaction commits; when it never
// commits (a rollback, a refusal, or a replay that changed nothing), they are
// dropped with it.
type heldEffects struct {
	mu       sync.Mutex
	parent   context.Context
	lines    []string
	released bool
}

type heldKey struct{}

// hold buffers text and reports true, or reports false once the effects were
// released, so later effects go straight to the recorder.
func (h *heldEffects) hold(text string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.released {
		return false
	}
	h.lines = append(h.lines, text)
	return true
}

// HoldEffects returns ctx on which effects are held until a transaction is
// committed with Commit. Every store method that writes in a transaction
// calls it before the transaction begins and commits with Commit, so an
// effect is recorded only once the change is durable, and never for a
// transaction that rolls back or a replay that commits nothing. When ctx
// already holds effects that were not released, ctx is returned as it is:
// the guarded store paths pass one transaction through several methods, and
// all of them commit together.
func HoldEffects(ctx context.Context) context.Context {
	if h, ok := ctx.Value(heldKey{}).(*heldEffects); ok {
		h.mu.Lock()
		open := !h.released
		h.mu.Unlock()
		if open {
			return ctx
		}
	}
	return context.WithValue(ctx, heldKey{}, &heldEffects{parent: ctx})
}

// holdsEffects reports whether ctx holds effects that were not released.
func holdsEffects(ctx context.Context) bool {
	h, ok := ctx.Value(heldKey{}).(*heldEffects)
	if !ok {
		return false
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return !h.released
}

// Commit commits tx and, when that succeeds, releases the effects ctx holds
// (see HoldEffects) to the recorder. It is the only place a store
// transaction is committed.
func Commit(ctx context.Context, tx *sql.Tx) error {
	if err := tx.Commit(); err != nil {
		return err
	}
	h, ok := ctx.Value(heldKey{}).(*heldEffects)
	if !ok {
		return nil
	}
	h.mu.Lock()
	lines := h.lines
	h.lines = nil
	h.released = true
	h.mu.Unlock()
	for _, line := range lines {
		noteLine(h.parent, line)
	}
	return nil
}

// NoteEffectIf is NoteEffect for a store method that has just returned err:
// it notes the effect only when err is nil.
func NoteEffectIf(ctx context.Context, err error, format string, args ...any) {
	if err == nil {
		NoteEffect(ctx, format, args...)
	}
}

// NoteEffectRows is NoteEffectIf for a statement that may change no row (an
// update or delete whose condition matched nothing): it notes the effect
// only when err is nil and res reports at least one changed row.
func NoteEffectRows(ctx context.Context, res sql.Result, err error, format string, args ...any) {
	if err != nil || res == nil {
		return
	}
	if n, err := res.RowsAffected(); err == nil && n > 0 {
		NoteEffect(ctx, format, args...)
	}
}

// List returns the effects recorded so far, in the order they happened.
func (e *Effects) List() []string {
	if e == nil {
		return nil
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.done...)
}

// NoteEffectWhen is NoteEffectIf for a store method whose success may change
// nothing: it notes the effect only when err is nil and changed is true.
func NoteEffectWhen(ctx context.Context, changed bool, err error, format string, args ...any) {
	if changed {
		NoteEffectIf(ctx, err, format, args...)
	}
}
