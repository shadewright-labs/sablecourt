package store

import (
	"context"
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
func NoteEffect(ctx context.Context, format string, args ...any) {
	e := effectsFrom(ctx)
	if e == nil {
		return
	}
	text := redact.String(fmt.Sprintf(format, args...))
	e.mu.Lock()
	e.done = append(e.done, text)
	e.mu.Unlock()
}

// NoteEffectIf is NoteEffect for a store method that has just returned err:
// it notes the effect only when err is nil.
func NoteEffectIf(ctx context.Context, err error, format string, args ...any) {
	if err == nil {
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
