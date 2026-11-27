// Package waitart draws a short animated scene on a terminal while sablectl
// waits on a long event: a hooded dark elf at work or in a fight, with a
// status line under it. The scenes are data (frames of coloured sprites); a
// Player plays one in place on stderr and clears it before each line of real
// output, so the animation never hides or splits that output.
package waitart

import (
	"fmt"
	"strings"
	"time"

	"github.com/mattn/go-runewidth"
)

const (
	// Width is the number of columns every frame takes.
	Width = 64
	// Height is the number of lines every frame takes: five rows of art,
	// the ground and the status line.
	Height  = artRows + 2
	artRows = 5
)

// colour is one palette entry: the colours sablectl prints with
// (cmd/sablectl/render.go), plus a neutral grey for the elf's cloak.
type colour struct {
	r, g, b uint8
	ansi    int // 256-colour fallback
}

var palette = map[byte]colour{
	'B': {0xEF, 0xE9, 0xDC, 255}, // bone
	'J': {0x2D, 0xD4, 0xBF, 43},  // jade
	'I': {0x6D, 0x28, 0xD9, 56},  // indigo
	'M': {0x9C, 0xA3, 0xAF, 248}, // muted
	'A': {0xF0, 0xB8, 0x6A, 215}, // amber
	'R': {0xF8, 0x71, 0x71, 210}, // red
	'G': {0x31, 0x2B, 0x4A, 237}, // border
	'C': {0x8A, 0x85, 0x94, 245}, // cloak
}

// sprite is a block of text with a colour mask of the same shape. A space in
// the text is transparent; each mask byte names a palette entry.
type sprite struct {
	text []string
	mask []string
}

// sp builds a sprite from pairs of text and mask lines.
func sp(lines ...string) sprite {
	s := sprite{}
	for i := 0; i < len(lines); i += 2 {
		t, m := lines[i], lines[i+1]
		if len([]rune(t)) != len(m) {
			panic(fmt.Sprintf("waitart: mask width mismatch: %q (%d) vs %q (%d)", t, len([]rune(t)), m, len(m)))
		}
		s.text = append(s.text, t)
		s.mask = append(s.mask, m)
	}
	return s
}

// recolour returns a copy of s with every mask byte in from replaced by to.
func (s sprite) recolour(from string, to byte) sprite {
	out := sprite{text: s.text}
	for _, m := range s.mask {
		b := []byte(m)
		for i := range b {
			if strings.IndexByte(from, b[i]) >= 0 {
				b[i] = to
			}
		}
		out.mask = append(out.mask, string(b))
	}
	return out
}

// over draws b on top of a at column x, row y (spaces in b are transparent).
func over(a sprite, x, y int, b sprite) sprite {
	out := sprite{text: append([]string(nil), a.text...), mask: append([]string(nil), a.mask...)}
	for len(out.text) < y+len(b.text) {
		out.text = append(out.text, "")
		out.mask = append(out.mask, "")
	}
	for dy, line := range b.text {
		t := []rune(out.text[y+dy])
		m := []byte(out.mask[y+dy])
		for dx, r := range []rune(line) {
			if r == ' ' {
				continue
			}
			for len(t) <= x+dx {
				t = append(t, ' ')
				m = append(m, ' ')
			}
			t[x+dx] = r
			m[x+dx] = b.mask[dy][dx]
		}
		out.text[y+dy] = string(t)
		out.mask[y+dy] = string(m)
	}
	return out
}

type placement struct {
	s   sprite
	x   int
	row int // top row
}

// Frame is one picture of a scene and how long it stays.
type Frame struct {
	parts []placement
	// Delay is how long the frame stays before the next one.
	Delay time.Duration
	// Note says what happens in the frame.
	Note string
}

func fx(x, row int, text, mask string) placement {
	return placement{s: sp(text, mask), x: x, row: row}
}

type cell struct {
	r rune
	c byte // palette key, 0 for none
}

type canvas [Height][Width]cell

var spinner = []rune("⠋⠙⠹⠸⠼⠴⠦⠧⠇⠏")

// caveFloor is the default ground: a cave floor with a few stones, fixed so
// it does not flicker.
func caveFloor() [Width]cell {
	var row [Width]cell
	for i := range row {
		row[i] = cell{'▁', 'G'}
	}
	for _, i := range []int{2, 17, 31, 38, 58} {
		row[i] = cell{'▂', 'G'}
	}
	return row
}

// render draws frame f of scene sc with the spinner at step idx and the
// status text on the last line.
func render(sc Scene, f Frame, idx int, status string) *canvas {
	var cv canvas
	for y := range cv {
		for x := range cv[y] {
			cv[y][x] = cell{' ', 0}
		}
	}
	for _, p := range f.parts {
		for dy, line := range p.s.text {
			y := p.row + dy
			if y < 0 || y >= artRows {
				continue
			}
			for dx, r := range []rune(line) {
				x := p.x + dx
				if r == ' ' || x < 0 || x >= Width {
					continue
				}
				cv[y][x] = cell{r, p.s.mask[dy][dx]}
			}
		}
	}
	if sc.ground != nil {
		cv[artRows] = sc.ground()
	} else {
		cv[artRows] = caveFloor()
	}
	line := []cell{{spinner[idx%len(spinner)], 'J'}, {' ', 0}}
	label, rest, found := strings.Cut(cleanStatus(status), ":")
	for _, r := range label {
		line = appendCell(line, r, 'B')
	}
	if found {
		for _, r := range ":" + rest {
			line = appendCell(line, r, 'M')
		}
	}
	if len(line) > Width {
		line = line[:Width]
		// Do not leave half of a wide rune at the cut.
		if line[Width-1].r == wideTail {
			line = line[:Width-1]
		}
		line[len(line)-1] = cell{'…', 'M'}
	}
	copy(cv[Height-1][:], line)
	return &cv
}

// cond measures columns the same way on every machine: the package default
// follows the locale (RUNEWIDTH_EASTASIAN, CJK locales) and would count
// ambiguous runes such as the block characters and the ellipsis as wide.
var cond = func() *runewidth.Condition {
	c := runewidth.NewCondition()
	c.EastAsianWidth = false
	return c
}()

// wideTail marks the second column of a wide rune; it prints nothing.
const wideTail rune = -1

// appendCell adds r to line, one cell per terminal column: a wide rune takes
// two cells, the second a wideTail, so the cut at Width counts columns.
func appendCell(line []cell, r rune, c byte) []cell {
	line = append(line, cell{r, c})
	if cond.RuneWidth(r) == 2 {
		line = append(line, cell{wideTail, c})
	}
	return line
}

// cleanStatus keeps the status on one line and drops control characters, so
// text from the daemon cannot move the cursor or change the terminal.
func cleanStatus(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r == '\n' || r == '\r' || r == '\t':
			return ' '
		case r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0):
			return -1
		case cond.RuneWidth(r) == 0:
			// Combining and zero-width marks take no column of their own.
			return -1
		}
		return r
	}, s)
}

// plain returns the canvas as text without colour, trailing spaces trimmed.
func (cv *canvas) plain() string {
	var b strings.Builder
	for _, row := range cv {
		var l strings.Builder
		for _, c := range row {
			if c.r != wideTail {
				l.WriteRune(c.r)
			}
		}
		b.WriteString(strings.TrimRight(l.String(), " "))
		b.WriteByte('\n')
	}
	return b.String()
}

func sgr(key byte, truecolor bool) string {
	c := palette[key]
	if truecolor {
		return fmt.Sprintf("\x1b[38;2;%d;%d;%dm", c.r, c.g, c.b)
	}
	return fmt.Sprintf("\x1b[38;5;%dm", c.ansi)
}

// ansi renders the canvas with colour codes. Every line resets the colour and
// clears to its end, so a redraw leaves nothing behind.
func (cv *canvas) ansi(truecolor bool) string {
	var b strings.Builder
	for _, row := range cv {
		end := len(row)
		for end > 0 && row[end-1].r == ' ' {
			end--
		}
		var cur byte
		for _, c := range row[:end] {
			if c.c != cur && c.r != ' ' {
				if c.c == 0 {
					b.WriteString("\x1b[0m")
				} else {
					b.WriteString(sgr(c.c, truecolor))
				}
				cur = c.c
			}
			if c.r != wideTail {
				b.WriteRune(c.r)
			}
		}
		// A carriage return too: a terminal in raw mode does not add one to
		// the line feed, and a row must start at column 0.
		b.WriteString("\x1b[0m\x1b[K\r\n")
	}
	return b.String()
}
