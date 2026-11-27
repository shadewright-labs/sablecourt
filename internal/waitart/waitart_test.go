package waitart

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mattn/go-runewidth"
)

// fakeTerm is a terminal writer for tests: it records every byte.
type fakeTerm struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (f *fakeTerm) Write(b []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.buf.Write(b)
}

func (f *fakeTerm) String() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.buf.String()
}

func env(vars map[string]string) func(string) (string, bool) {
	return func(k string) (string, bool) {
		v, ok := vars[k]
		return v, ok
	}
}

// onTerminal is a Config that sees any writer as an 80-column terminal and
// reads only the given environment.
func onTerminal(vars map[string]string) Config {
	return Config{
		LookupEnv: env(vars),
		Terminal:  func(io.Writer) (int, bool) { return 80, true },
	}
}

// fast returns a scene whose frames change every millisecond.
func fast(name string) Scene {
	sc, _ := Lookup(name)
	sc.Frames = slices.Clone(sc.Frames)
	for i := range sc.Frames {
		sc.Frames[i].Delay = time.Millisecond
	}
	return sc
}

func TestOffCasesDrawNothing(t *testing.T) {
	notTerm := func(io.Writer) (int, bool) { return 0, false }
	narrow := func(io.Writer) (int, bool) { return Width - 1, true }
	for _, tc := range []struct {
		name string
		cfg  Config
	}{
		{"not a terminal", Config{LookupEnv: env(nil), Terminal: notTerm}},
		{"real check on a plain writer", Config{LookupEnv: env(nil)}},
		{"terminal narrower than a frame", Config{LookupEnv: env(nil), Terminal: narrow}},
		{"NO_COLOR", onTerminal(map[string]string{"NO_COLOR": "1"})},
		{"TERM=dumb", onTerminal(map[string]string{"TERM": "dumb"})},
		{"--no-art", func() Config { c := onTerminal(nil); c.NoArt = true; return c }()},
		{"SABLECOURT_NO_ART=1", onTerminal(map[string]string{NoArtEnv: "1"})},
		{"SABLECOURT_NO_ART=yes", onTerminal(map[string]string{NoArtEnv: "yes"})},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var w fakeTerm
			if tc.cfg.Enabled(&w) {
				t.Fatal("Enabled = true, want false")
			}
			p := tc.cfg.Start(t.Context(), &w, fast("duel"), "Council in session")
			if p != nil {
				t.Fatal("Start returned a player, want nil")
			}
			out := p.Wrap(&w)
			fmt.Fprintln(out, "real output")
			p.SetStatus("ignored")
			p.Stop()
			if got := w.String(); got != "real output\n" {
				t.Fatalf("writer got %q, want only the real output", got)
			}
		})
	}
}

func TestOnCases(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  Config
	}{
		{"plain terminal", onTerminal(nil)},
		{"empty NO_COLOR", onTerminal(map[string]string{"NO_COLOR": ""})},
		{"SABLECOURT_NO_ART=0", onTerminal(map[string]string{NoArtEnv: "0"})},
		{"TERM=xterm-256color", onTerminal(map[string]string{"TERM": "xterm-256color"})},
		{"unknown width", Config{LookupEnv: env(nil), Terminal: func(io.Writer) (int, bool) { return 0, true }}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var w fakeTerm
			p := tc.cfg.Start(t.Context(), &w, fast("blade"), "Waiting")
			if p == nil {
				t.Fatal("Start returned nil, want a player")
			}
			p.Stop()
			if !strings.Contains(w.String(), "Waiting") {
				t.Fatalf("no frame drawn: %q", w.String())
			}
		})
	}
}

func TestStopIsIdempotentAndRestoresCursor(t *testing.T) {
	var w fakeTerm
	p := onTerminal(nil).Start(t.Context(), &w, fast("forge"), "Building")
	time.Sleep(10 * time.Millisecond)
	p.Stop()
	p.Stop()
	out := w.String()
	if !strings.HasPrefix(out, hideCursor) {
		t.Fatalf("output does not start by hiding the cursor: %q", out[:min(len(out), 20)])
	}
	if !strings.HasSuffix(out, fmt.Sprintf("\x1b[%dA\r\x1b[J", Height)+showCursor) {
		t.Fatalf("output does not end by clearing the frame and showing the cursor: %q", out[max(0, len(out)-40):])
	}
	if n := strings.Count(out, showCursor); n != 1 {
		t.Fatalf("cursor shown %d times, want 1", n)
	}
	// Nothing is drawn after Stop.
	time.Sleep(5 * time.Millisecond)
	if w.String() != out {
		t.Fatal("the player wrote after Stop")
	}
	var nilPlayer *Player
	nilPlayer.Stop()
}

func TestContextEndRestoresCursor(t *testing.T) {
	var w fakeTerm
	ctx, cancel := context.WithCancel(t.Context())
	p := onTerminal(nil).Start(ctx, &w, fast("watch"), "Waiting for approval")
	cancel() // what Ctrl-C does through signal.NotifyContext
	<-p.done
	if !strings.HasSuffix(w.String(), showCursor) {
		t.Fatalf("cursor not restored after the context ended: %q", w.String())
	}
	p.Stop() // the caller's deferred Stop still returns
	if n := strings.Count(w.String(), showCursor); n != 1 {
		t.Fatalf("cursor shown %d times, want 1", n)
	}
}

// TestOutputLinesAreNeverSplit writes lines in pieces through Wrap while the
// player redraws every millisecond on the same terminal, and checks that each
// line reaches the terminal in one piece, with no frame byte inside it.
func TestOutputLinesAreNeverSplit(t *testing.T) {
	var w fakeTerm
	p := onTerminal(nil).Start(t.Context(), &w, fast("duel"), "Council in session")
	out := p.Wrap(&w)
	var lines []string
	for i := range 150 {
		parts := []string{fmt.Sprintf("@seat-%03d ", i), "(answer) ", "body text", "\n"}
		lines = append(lines, strings.Join(parts, ""))
		for _, part := range parts {
			if _, err := io.WriteString(out, part); err != nil {
				t.Fatal(err)
			}
			if i%10 == 0 {
				time.Sleep(time.Millisecond) // let frames land between writes
			}
		}
	}
	p.Stop()
	stream := w.String()
	if strings.Count(stream, "Council in session") < 2 {
		t.Fatal("too few frames drawn for the test to mean anything")
	}
	for _, l := range lines {
		if !strings.Contains(stream, l) {
			t.Fatalf("line %q was split or lost", l)
		}
	}
	// Every frame starts at the start of a line: right after a newline, a
	// clear, or the hidden cursor at the very start. A draw begins with a
	// carriage return that is not the one inside a clear (after the cursor-up,
	// before an erase) and not the one of a row's \\r\\n.
	checked := 0
	for i := range len(stream) {
		if stream[i] != '\r' || (i > 0 && stream[i-1] == 'A') || strings.HasPrefix(stream[i+1:], "\x1b[J") || strings.HasPrefix(stream[i+1:], "\n") {
			continue
		}
		checked++
		before := stream[:i]
		if !(strings.HasSuffix(before, "\n") || strings.HasSuffix(before, "\x1b[J") || before == hideCursor) {
			t.Fatalf("frame starts mid-line after %q", before[max(0, len(before)-30):])
		}
	}
	if checked < 2 {
		t.Fatalf("checked %d frame starts, want at least 2", checked)
	}
	// Output written after Stop passes straight through.
	fmt.Fprint(out, "after")
	if !strings.HasSuffix(w.String(), showCursor+"after") {
		t.Fatal("output after Stop was not passed through")
	}
}

func TestPartialLineHoldsFrames(t *testing.T) {
	var w fakeTerm
	p := onTerminal(nil).Start(t.Context(), &w, fast("bow"), "Waiting")
	out := p.Wrap(&w)
	fmt.Fprint(out, "Question: ")
	mark := len(w.String())
	time.Sleep(20 * time.Millisecond)
	if got := w.String()[mark:]; got != "" {
		t.Fatalf("frames drawn while a line was unfinished: %q", got)
	}
	fmt.Fprintln(out, "answer")
	p.Stop()
	if !strings.Contains(w.String(), "Question: answer\n") {
		t.Fatal("partial line was split")
	}
}

// TestScenesFitTheFrame checks every scene: at most Height lines by Width
// columns, every frame the same height, and nothing placed outside the frame
// (which render would clip without a word).
func TestScenesFitTheFrame(t *testing.T) {
	names := map[string]bool{}
	for _, sc := range Scenes() {
		names[sc.Name] = true
		if len(sc.Frames) < 2 || sc.Key < 0 || sc.Key >= len(sc.Frames) {
			t.Errorf("%s: %d frames, key %d", sc.Name, len(sc.Frames), sc.Key)
		}
		for i, f := range sc.Frames {
			if f.Delay <= 0 || f.Note == "" {
				t.Errorf("%s frame %d: delay %v, note %q", sc.Name, i, f.Delay, f.Note)
			}
			for _, p := range f.parts {
				for dy, line := range p.s.text {
					for dx, r := range []rune(line) {
						if r == ' ' {
							continue
						}
						if _, ok := palette[p.s.mask[dy][dx]]; !ok {
							t.Errorf("%s frame %d (%s): %q has mask %q, not a palette colour", sc.Name, i, f.Note, r, p.s.mask[dy][dx])
						}
						x, y := p.x+dx, p.row+dy
						if x < 0 || x >= Width || y < 0 || y >= artRows {
							t.Errorf("%s frame %d (%s): %q at column %d, row %d is outside the art", sc.Name, i, f.Note, r, x, y)
						}
					}
				}
			}
			plain := render(sc, f, i, strings.Repeat("long status ", 10)).plain()
			lines := strings.Split(strings.TrimSuffix(plain, "\n"), "\n")
			if len(lines) != Height {
				t.Errorf("%s frame %d: %d lines, want %d", sc.Name, i, len(lines), Height)
			}
			for _, l := range lines {
				if n := len([]rune(l)); n > Width {
					t.Errorf("%s frame %d: line of %d columns", sc.Name, i, n)
				}
			}
		}
	}
	for _, want := range []string{"blade", "bow", "spell", "duel", "forge", "watch"} {
		if !names[want] {
			t.Errorf("scene %q missing", want)
		}
	}
	if Height > 7 || Width > 64 {
		t.Errorf("frame is %dx%d, limit 7x64", Height, Width)
	}
}

func TestForKind(t *testing.T) {
	for _, tc := range []struct {
		kind Kind
		want []string
	}{
		{Council, []string{"duel", "blade"}},
		{Build, []string{"forge"}},
		{Person, []string{"watch"}},
		{Other, []string{"blade", "bow", "spell", "duel", "forge", "watch"}},
		{Kind("unknown"), []string{"blade", "bow", "spell", "duel", "forge", "watch"}},
	} {
		for range 20 {
			if got := ForKind(tc.kind).Name; !slices.Contains(tc.want, got) {
				t.Errorf("ForKind(%q) = %q, want one of %v", tc.kind, got, tc.want)
			}
		}
	}
}

func TestStatusIsCleanedAndCut(t *testing.T) {
	sc, _ := Lookup("blade")
	cv := render(sc, sc.Frames[0], 0, "Round 2:\x1b[2J\x07 seat\nanswered "+strings.Repeat("x", 100))
	last := strings.Split(cv.plain(), "\n")[Height-1]
	if strings.ContainsAny(last, "\x1b\x07\n") {
		t.Fatalf("control characters reached the status line: %q", last)
	}
	if n := len([]rune(last)); n != Width || !strings.HasSuffix(last, "…") {
		t.Fatalf("long status not cut to %d columns with an ellipsis: %d %q", Width, n, last)
	}
}

// TestProcessEnvironment checks the default Config against the real process
// environment, which is how sablectl runs.
func TestProcessEnvironment(t *testing.T) {
	term80 := func(io.Writer) (int, bool) { return 80, true }
	for _, tc := range []struct {
		name, value string
		on          bool
	}{
		{"NO_COLOR", "1", false},
		{"NO_COLOR", "", true},
		{"TERM", "dumb", false},
		{"TERM", "xterm", true},
		{NoArtEnv, "1", false},
		{NoArtEnv, "0", true},
	} {
		t.Run(tc.name+"="+tc.value, func(t *testing.T) {
			for _, n := range []string{"NO_COLOR", "TERM", NoArtEnv} {
				t.Setenv(n, "")
			}
			t.Setenv(tc.name, tc.value)
			if got := (Config{Terminal: term80}).Enabled(io.Discard); got != tc.on {
				t.Fatalf("Enabled = %v, want %v", got, tc.on)
			}
		})
	}
}

// shortWriter writes only the first n bytes of each call and then fails,
// like a terminal that went away mid-write.
type shortWriter struct {
	fakeTerm
	n   int
	err error
}

func (s *shortWriter) Write(b []byte) (int, error) {
	n := min(s.n, len(b))
	_, _ = s.fakeTerm.Write(b[:n])
	if n < len(b) {
		return n, s.err
	}
	return n, nil
}

// TestFailedWritesNeverDrawMidLine checks that the player decides whether a
// line is finished from the bytes that were written, not from the bytes the
// caller offered.
func TestFailedWritesNeverDrawMidLine(t *testing.T) {
	errBoom := errors.New("boom")
	for _, tc := range []struct {
		name      string
		first     string // written whole, to leave a line unfinished (or not)
		then      string // offered second, written only in part
		keep      int    // bytes of then that get written
		wantDraws bool   // a frame may follow the second write
	}{
		{"short write of a full line", "", "hello\n", 3, false},
		{"failed write completing a line", "Question: ", "answer\n", 0, false},
		{"short write that keeps the newline out", "Q: ", "abc\n", 3, false},
		{"complete write of a line", "", "hello\n", 6, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var term fakeTerm
			// A long delay keeps the ticker out: only the line writer draws.
			sc, _ := Lookup("bow")
			sc.Frames = slices.Clone(sc.Frames)
			for i := range sc.Frames {
				sc.Frames[i].Delay = time.Hour
			}
			p := onTerminal(nil).Start(t.Context(), &term, sc, "Waiting")
			defer p.Stop()
			if tc.first != "" {
				fmt.Fprint(p.Wrap(&term), tc.first)
			}
			mark := len(term.String())
			sw := &shortWriter{n: tc.keep, err: errBoom}
			_, _ = io.WriteString(p.Wrap(sw), tc.then)
			p.mu.Lock()
			held := p.held
			p.mu.Unlock()
			if held == tc.wantDraws {
				t.Fatalf("line held = %v, want %v", held, !tc.wantDraws)
			}
			drawn := strings.Contains(term.String()[mark:], "Waiting")
			if drawn != tc.wantDraws {
				t.Fatalf("frame drawn = %v, want %v; terminal got %q", drawn, tc.wantDraws, term.String()[mark:])
			}
		})
	}
}

// TestStatusFitsInColumns checks that wide runes count by terminal columns,
// so the status row never wraps and the cursor-up count stays right.
func TestStatusFitsInColumns(t *testing.T) {
	// Behave as under a CJK locale or RUNEWIDTH_EASTASIAN=1: the package
	// default condition is read once at start-up, so set it directly.
	t.Setenv("RUNEWIDTH_EASTASIAN", "1")
	t.Setenv("LC_ALL", "ja_JP.UTF-8")
	old := runewidth.DefaultCondition.EastAsianWidth
	runewidth.DefaultCondition.EastAsianWidth = true
	t.Cleanup(func() { runewidth.DefaultCondition.EastAsianWidth = old })
	sc, _ := Lookup("blade")
	for _, status := range []string{
		"Waiting for out-of-band confirmation: " + strings.Repeat("\u6f22", 30),
		strings.Repeat("\U0001F600", 40),
		"e\u0301" + strings.Repeat("e\u0301", 70),
		strings.Repeat("\u00b0", 90), // ambiguous width: one column everywhere
	} {
		cv := render(sc, sc.Frames[0], 0, status)
		for _, line := range strings.Split(cv.ansi(false), "\n") {
			if w := cond.StringWidth(stripSGR(line)); w > Width {
				t.Errorf("line is %d columns wide, want at most %d: %q", w, Width, line)
			}
		}
	}
}

// TestAmbiguousRunesAreOneColumn checks that the status keeps ambiguous-width
// runes at one column whatever the locale says.
func TestAmbiguousRunesAreOneColumn(t *testing.T) {
	runewidth.DefaultCondition.EastAsianWidth = true
	t.Cleanup(func() { runewidth.DefaultCondition.EastAsianWidth = false })
	sc, _ := Lookup("blade")
	cv := render(sc, sc.Frames[0], 0, strings.Repeat("\u00b0", 90))
	if n := strings.Count(stripSGR(cv.ansi(false)), "\u00b0"); n < Width-3 {
		t.Errorf("status kept %d ambiguous runes, want at least %d", n, Width-3)
	}
}

func stripSGR(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == 0x1b {
			for i < len(s) && !(s[i] >= 'A' && s[i] <= 'Z' || s[i] >= 'a' && s[i] <= 'z') {
				i++
			}
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// rawColumns replays a terminal stream on a terminal that does not turn a
// line feed into a carriage return and line feed (raw mode). It counts the
// frame rows, from a drawing carriage return to the next one, that start at
// a column other than 0.
func rawColumns(stream string) (rows, offsets int) {
	col, inFrame, fresh := 0, false, false
	rs := []rune(stream)
	for i := 0; i < len(rs); i++ {
		r := rs[i]
		switch {
		case r == 0x1b:
			j := i + 1
			if j < len(rs) && rs[j] == '[' {
				j++
				for j < len(rs) && !(rs[j] >= '@' && rs[j] <= '~') {
					j++
				}
				if j < len(rs) && rs[j] == 'K' && fresh && col != 0 {
					offsets++
				}
				if j < len(rs) && rs[j] == 'A' {
					inFrame = false
				}
				fresh = false
			}
			i = j
		case r == '\r':
			col = 0
			if i > 0 && rs[i-1] != 'A' && i+1 < len(rs) && rs[i+1] != '\n' && !(i+3 < len(rs) && rs[i+1] == 0x1b && rs[i+2] == '[' && rs[i+3] == 'J') {
				inFrame, fresh = true, true
			}
		case r == '\n':
			if inFrame {
				rows++
				fresh = true
			}
		default:
			if inFrame && fresh && col != 0 {
				offsets++
			}
			fresh = false
			col += cond.RuneWidth(r)
		}
	}
	return rows, offsets
}

// TestFramesStartAtColumnZero replays the stream on a terminal in raw mode,
// after output lines that end in the middle of the screen width: every frame
// row still starts at column 0.
func TestFramesStartAtColumnZero(t *testing.T) {
	var w fakeTerm
	sc, _ := Lookup("duel")
	sc.Frames = slices.Clone(sc.Frames)
	for i := range sc.Frames {
		sc.Frames[i].Delay = time.Hour
	}
	p := onTerminal(nil).Start(t.Context(), &w, sc, "Council in session")
	out := p.Wrap(&w)
	fmt.Fprintln(out, "@claude (proposal, #1)")
	fmt.Fprintln(out, "First answer.")
	p.Stop()
	rows, offsets := rawColumns(w.String())
	if rows < 2*Height {
		t.Fatalf("replayed %d frame rows, want at least %d", rows, 2*Height)
	}
	if offsets != 0 {
		t.Fatalf("%d frame rows start away from column 0", offsets)
	}
}
