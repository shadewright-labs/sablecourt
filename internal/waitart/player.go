package waitart

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"sync"
	"time"

	"golang.org/x/term"
)

const (
	hideCursor = "\x1b[?25l"
	showCursor = "\x1b[0m\x1b[?25h"
	// NoArtEnv turns the art off when set to any value other than "" or "0".
	NoArtEnv = "SABLECOURT_NO_ART"
)

// Config decides whether a scene is drawn. The zero value reads the process
// environment and checks the writer for a terminal.
type Config struct {
	// NoArt turns the art off, as the --no-art flag does.
	NoArt bool
	// LookupEnv reads an environment variable; nil uses os.LookupEnv.
	LookupEnv func(string) (string, bool)
	// Terminal reports whether w is a terminal and how many columns it has
	// (0 when unknown). nil checks for an *os.File on a terminal.
	Terminal func(w io.Writer) (cols int, ok bool)
}

func (c Config) env(name string) string {
	look := c.LookupEnv
	if look == nil {
		look = processEnv
	}
	v, _ := look(name)
	return v
}

// processEnv reads the variables the art depends on, each by its literal
// name so the configuration reference test sees every read.
func processEnv(name string) (string, bool) {
	switch name {
	case "NO_COLOR":
		return os.LookupEnv("NO_COLOR")
	case "TERM":
		return os.LookupEnv("TERM")
	case "COLORTERM":
		return os.LookupEnv("COLORTERM")
	case NoArtEnv:
		return os.LookupEnv("SABLECOURT_NO_ART")
	}
	return "", false
}

func fileTerminal(w io.Writer) (int, bool) {
	f, ok := w.(*os.File)
	if !ok || !term.IsTerminal(int(f.Fd())) {
		return 0, false
	}
	cols, _, err := term.GetSize(int(f.Fd()))
	if err != nil {
		return 0, true
	}
	return cols, true
}

// Enabled reports whether a scene would be drawn on w: only on a terminal at
// least Width columns wide, and not with NoArt, NO_COLOR, TERM=dumb or
// SABLECOURT_NO_ART set.
func (c Config) Enabled(w io.Writer) bool {
	if c.NoArt || w == nil {
		return false
	}
	if c.env("NO_COLOR") != "" || c.env("TERM") == "dumb" {
		return false
	}
	if v := c.env(NoArtEnv); v != "" && v != "0" {
		return false
	}
	isTerm := c.Terminal
	if isTerm == nil {
		isTerm = fileTerminal
	}
	cols, ok := isTerm(w)
	if !ok {
		return false
	}
	// A narrower terminal would wrap the frame and break the redraw.
	return cols == 0 || cols >= Width
}

// Player plays one scene in place. A nil *Player is valid and does nothing,
// so callers need no check when the art is off.
type Player struct {
	mu        sync.Mutex
	w         io.Writer
	scene     Scene
	status    string
	truecolor bool
	idx       int
	drawn     bool // a frame is on the screen
	held      bool // a partial output line is on the screen; draw nothing
	stopped   bool
	disabled  bool // a write failed or the terminal narrowed; draw nothing more
	terminal  func(io.Writer) (int, bool)

	once sync.Once
	quit chan struct{}
	done chan struct{}
}

// Start plays sc on w with the status line under it until ctx ends or Stop is
// called, when Config{} allows it; otherwise it returns nil.
func Start(ctx context.Context, w io.Writer, sc Scene, status string) *Player {
	return Config{}.Start(ctx, w, sc, status)
}

// Start plays sc on w with the status line under it until ctx ends or Stop is
// called. It returns nil, and draws nothing, when c is not Enabled for w.
func (c Config) Start(ctx context.Context, w io.Writer, sc Scene, status string) *Player {
	if ctx == nil {
		ctx = context.Background()
	}
	if !c.Enabled(w) || len(sc.Frames) == 0 || ctx.Err() != nil {
		return nil
	}
	ct := c.env("COLORTERM")
	terminal := c.Terminal
	if terminal == nil {
		terminal = fileTerminal
	}
	p := &Player{
		w:         w,
		terminal:  terminal,
		scene:     sc,
		status:    status,
		truecolor: ct == "truecolor" || ct == "24bit",
		quit:      make(chan struct{}),
		done:      make(chan struct{}),
	}
	p.mu.Lock()
	p.write(hideCursor)
	p.draw()
	p.mu.Unlock()
	go p.run(ctx)
	return p
}

func (p *Player) run(ctx context.Context) {
	defer close(p.done)
	for {
		p.mu.Lock()
		delay := p.scene.Frames[p.idx%len(p.scene.Frames)].Delay
		p.mu.Unlock()
		if delay <= 0 {
			delay = 150 * time.Millisecond
		}
		t := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			t.Stop()
			p.stop()
			return
		case <-p.quit:
			t.Stop()
			return
		case <-t.C:
		}
		p.mu.Lock()
		if !p.stopped {
			p.idx++
			if !p.held && p.fits() {
				p.clear()
				p.draw()
			}
		}
		p.mu.Unlock()
	}
}

// Stop clears the frame and shows the cursor again. It is safe to call more
// than once and on a nil Player, and it returns once the player is idle.
func (p *Player) Stop() {
	if p == nil {
		return
	}
	p.stop()
	<-p.done
}

func (p *Player) stop() {
	p.once.Do(func() {
		p.mu.Lock()
		p.clear()
		// Written directly: write refuses once drawing is disabled, and the
		// show inside disable may have failed while the writer was down.
		_, _ = io.WriteString(p.w, showCursor)
		p.stopped = true
		p.mu.Unlock()
		close(p.quit)
	})
}

// SetStatus changes the status line; the next frame shows it.
func (p *Player) SetStatus(status string) {
	if p == nil {
		return
	}
	p.mu.Lock()
	p.status = status
	p.mu.Unlock()
}

// Wrap returns a writer for real output that shares the screen with the
// player: before each write the frame is cleared, and once the output ends a
// line the frame is drawn again below it. A frame is never drawn while a
// line is unfinished, so no frame byte lands inside an output line. On a nil
// Player it returns out.
func (p *Player) Wrap(out io.Writer) io.Writer {
	if p == nil {
		return out
	}
	return &lineWriter{p: p, out: out}
}

type lineWriter struct {
	p   *Player
	out io.Writer
}

func (l *lineWriter) Write(b []byte) (int, error) {
	p := l.p
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.stopped || len(b) == 0 {
		return l.out.Write(b)
	}
	if p.fits() {
		p.clear()
	}
	n, err := l.out.Write(b)
	// Only the bytes that were written count: after a short or failed write
	// the line on the screen is whatever reached it.
	if n > 0 {
		p.held = b[n-1] != '\n'
	}
	if !p.held && err == nil {
		p.draw()
	}
	return n, err
}

// fits rechecks the terminal width before a frame is cleared or drawn. When a
// frame no longer fits, or the writer is no longer a terminal, it turns the
// art off. The caller holds p.mu.
func (p *Player) fits() bool {
	if p.disabled {
		return false
	}
	cols, ok := p.terminal(p.w)
	if ok && (cols == 0 || cols >= Width) {
		return true
	}
	p.disable()
	return false
}

// disable stops all drawing. The frame on the screen may have wrapped or been
// cut short, so the cursor-up count for clearing it can no longer be trusted:
// the clear geometry is dropped and nothing is cleared. The cursor is shown
// again at once. The caller holds p.mu.
func (p *Player) disable() {
	if p.disabled {
		return
	}
	p.disabled = true
	p.drawn = false
	_, _ = io.WriteString(p.w, showCursor)
}

// clear removes the frame from the screen: back to the frame's first line,
// then erase to the end of the screen. The caller holds p.mu.
func (p *Player) clear() {
	if !p.drawn || p.disabled {
		return
	}
	if !p.write(fmt.Sprintf("\x1b[%dA\r\x1b[J", Height)) {
		return
	}
	p.drawn = false
}

// draw writes the current frame from the cursor down and leaves the cursor
// at the start of the line below it. The caller holds p.mu.
func (p *Player) draw() {
	if p.stopped || p.held || p.disabled {
		return
	}
	f := p.scene.Frames[p.idx%len(p.scene.Frames)]
	var b bytes.Buffer
	b.WriteString("\r")
	b.WriteString(render(p.scene, f, p.idx, p.status).ansi(p.truecolor))
	// A frame that did not reach the screen whole is not cleared later: the
	// seven-row move would erase lines above it.
	p.drawn = p.write(b.String())
}

// write sends s to the terminal and reports whether all of it arrived. After
// a short or failed write the art is turned off, since the failure only
// loses the art. The caller holds p.mu.
func (p *Player) write(s string) bool {
	if p.disabled {
		return false
	}
	n, err := io.WriteString(p.w, s)
	if err != nil || n < len(s) {
		p.disable()
		return false
	}
	return true
}
