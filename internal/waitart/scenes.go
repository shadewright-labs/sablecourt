package waitart

import (
	"strings"
	"sync/atomic"
	"time"
)

// Scene is one animated scene: a creature, its frames and the frame used as
// the still in previews.
type Scene struct {
	// Name is the short name, such as "duel".
	Name string
	// Creature describes what the elf is up against.
	Creature string
	// Key is the index of the frame that sums the scene up.
	Key int
	// Frames play in order and then loop.
	Frames []Frame

	ground func() [Width]cell // nil: the cave floor
}

// Kind is the kind of wait a scene is chosen for.
type Kind string

const (
	// Council is a council round running.
	Council Kind = "council"
	// Build is a harness image build or a tool install.
	Build Kind = "build"
	// Person is a wait on a person, such as an approval.
	Person Kind = "person"
	// Other is any other wait.
	Other Kind = ""
)

var scenes = []Scene{
	{Name: "blade", Creature: "hollowmaw, a hunched cave beast with a bone-crystal ridge", Key: 5, Frames: bladeFrames()},
	{Name: "bow", Creature: "gloomkite, a floating ray-winged night flyer", Key: 6, Frames: bowFrames()},
	{Name: "spell", Creature: "slateback, a slow crawler under a plated stone shell", Key: 6, Frames: spellFrames()},
	{Name: "duel", Creature: "hushhand, a many-armed shadow", Key: 4, Frames: duelFrames()},
	{Name: "forge", Creature: "cinderkin, a small ember thief", Key: 4, Frames: forgeFrames()},
	{Name: "watch", Creature: "a duskwing circling far off", Key: 3, Frames: watchFrames(), ground: ledge},
}

// kindScenes names the scenes that fit each kind of wait.
var kindScenes = map[Kind][]string{
	Council: {"duel", "blade"},
	Build:   {"forge"},
	Person:  {"watch"},
}

// Scenes returns every scene.
func Scenes() []Scene { return append([]Scene(nil), scenes...) }

// Lookup returns the scene with the given name.
func Lookup(name string) (Scene, bool) {
	for _, s := range scenes {
		if s.Name == name {
			return s, true
		}
	}
	return Scene{}, false
}

// sceneTurn counts scene choices so that successive waits rotate through the
// fitting scenes without any random source.
var sceneTurn atomic.Uint64

// ForKind returns a scene that fits the kind of wait, rotating through the
// fitting scenes on successive calls, and any scene for a kind with none of
// its own.
func ForKind(k Kind) Scene {
	turn := sceneTurn.Add(1) - 1
	names := kindScenes[k]
	if len(names) == 0 {
		return scenes[turn%uint64(len(scenes))]
	}
	s, _ := Lookup(names[turn%uint64(len(names))])
	return s
}

func ms(n int) time.Duration { return time.Duration(n) * time.Millisecond }

func fr(delay int, note string, parts ...[]placement) Frame {
	var all []placement
	for _, p := range parts {
		all = append(all, p...)
	}
	return Frame{parts: all, Delay: ms(delay), Note: note}
}

func at(s sprite, x, row int) []placement { return []placement{{s, x, row}} }

func fxs(ps ...placement) []placement { return ps }

// --- blade: the elf against the hollowmaw ---------------------------------

// The hollowmaw faces left: a hunched cave beast with a ridge of bone
// crystals, two amber eyes and a jaw of bone teeth.
var (
	beastIdle = sp(
		"      ▲ ▲ ▲   ", "      B B B   ",
		"   ▗▄██████▄  ", "   IIIIIIIII  ",
		" ▗█● ●███████▖", " IIA AIIIIIIII",
		" ▜▼▼▼▄███████▌", " IBBBIIIIIIIII",
		"   ▜▘ ▜▘ ▝▛ ▝▛", "   II II II II",
	)
	beastRoar = sp(
		"     ▲ ▲ ▲    ", "     B B B    ",
		"   ▗▄██████▄  ", "   IIIIIIIII  ",
		" ▗█● ●███████▖", " IIA AIIIIIIII",
		" ▼ ▼ ▀███████▌", " B B IIIIIIIII",
		" ▲▲▲ ▜▘ ▝▛ ▝▛ ", " BBB II II II ",
	)
	beastHit = sp(
		"      ▲ ▲ ▲   ", "      B B B   ",
		"   ▗▄██████▄  ", "   RRRRRRRRR  ",
		" ▗█× ×███████▖", " RRB BRRRRRRRR",
		" ▜▼▼▼▄███████▌", " RBBBRRRRRRRRR",
		"   ▜▘ ▜▘ ▝▛ ▝▛", "   RR RR RR RR",
	)
)

func bladeFrames() []Frame {
	b := 46
	return []Frame{
		fr(160, "stand-off: elf on guard, beast waiting",
			elf(elfGuard, 3, 0), at(beastIdle, b, 0)),
		fr(140, "elf advances",
			elf(elfStride, 8, 1), at(beastIdle, b, 0)),
		fr(140, "elf advances, beast opens its jaw",
			elf(elfGuard, 13, 2), at(beastRoar, b, 0)),
		fr(140, "elf draws the blade back",
			elf(elfWindup, 20, 1), at(beastRoar, b, 0)),
		fr(110, "lunge: blade reaches the beast",
			elf(elfLunge, 28, 2), at(beastIdle, b, 0),
			fxs(fx(46, 1, "⠈⠂", "AB"), fx(45, 3, "⠠", "B"))),
		fr(180, "strike: sparks, beast flashes red",
			elf(elfLunge, 28, 2), at(beastHit, b+1, 0),
			fxs(fx(44, 0, "⠐ ✦ ⠂", "A J B"), fx(45, 1, "⠡✦", "BJ"), fx(45, 3, "⠌✦", "AJ"), fx(44, 4, "⠂  ⠁", "B  A"))),
		fr(160, "beast recoils, sparks fade",
			elf(elfGuard, 27, 1), at(beastHit, b+3, 0),
			fxs(fx(43, 0, "⠁   ⠈", "A   B"), fx(45, 4, "⠄", "A"))),
		fr(140, "elf steps back, beast shakes it off",
			elf(elfStride, 20, 0), at(beastIdle, b+2, 0)),
		fr(140, "elf resets",
			elf(elfGuard, 13, 1), at(beastIdle, b+1, 0)),
		fr(140, "elf resets, beast back in place",
			elf(elfStride, 7, 2), at(beastIdle, b, 0)),
	}
}

// --- bow: the elf against the gloomkite -----------------------------------

// The gloomkite hangs in the air on wide ray wings, two amber eyes on a
// small body and a hooked tail. Its body sits on row 2, the arrow's row.
var (
	kiteUp = sp(
		"█▙        ▟█", "II        II",
		"▝██▙▗▄▄▖▟██▘", "IIIIIIIIIIII",
		"  ▝▜●██●▛▘  ", "  IIAIIAII  ",
		"     ▐▌     ", "     II     ",
		"      ▝▖    ", "      II    ",
	)
	kiteDown = sp(
		"            ", "            ",
		"    ▗▄▄▖    ", "    IIII    ",
		"▗▟██●██●██▙▖", "IIIIAIIAIIII",
		"█▛▘ ▝▐▌▘ ▝▜█", "III IIII III",
		"▘     ▝▖   ▝", "I     II   I",
	)
	kiteHit = sp(
		" ▙        ▟█", " R        RR",
		" ▝▙▄▗▄▄▖▟██▘", " RRRRRRRRRRR",
		"  ▝▜×██×▛▘  ", "  RRBRRBRR  ",
		"     ▐▌     ", "     RR     ",
		"     ▗▘     ", "     RR     ",
	)
	// Tumbling: one wing folded, body tipped. It is drawn one row down, so
	// its tail falls below the art and is left out.
	kiteReel = sp(
		"         ▟█ ", "         RR ",
		"   ▗▄▄▖▟██▘ ", "   RRRRRRRR ",
		"▗▟▛×██×▛▘   ", "RRRBRRBRR   ",
		"▀▘  ▐▌      ", "RR  RR      ",
	)
)

const arrow, arrowMask = "»━━━▸", "JBBBB"

func bowFrames() []Frame {
	e, k := 4, 46
	return []Frame{
		fr(160, "elf holds the bow, the gloomkite hovers",
			elf(elfLoose, e, 0), at(kiteUp, k, 0)),
		fr(140, "elf nocks an arrow",
			elf(elfLoose, e, 1), at(kiteDown, k, 0)),
		fr(160, "full draw",
			elf(elfDraw, e, 2), at(kiteUp, k, 0)),
		fr(90, "loose: arrow leaves the string",
			elf(elfLoose, e, 1), at(kiteDown, k, 0), fxs(fx(17, 2, arrow, arrowMask), fx(14, 2, "⠒", "M"))),
		fr(90, "arrow in flight",
			elf(elfLoose, e, 2), at(kiteUp, k, 0), fxs(fx(28, 2, arrow, arrowMask), fx(25, 2, "⠂", "M"))),
		fr(90, "arrow reaches the kite",
			elf(elfLoose, e, 0), at(kiteDown, k, 0), fxs(fx(39, 2, arrow, arrowMask))),
		fr(190, "hit: the kite flashes red and jerks back",
			elf(elfLoose, e, 1), at(kiteHit, k+2, 0),
			fxs(fx(45, 1, "⠐✦", "AJ"), fx(46, 2, "✦", "J"), fx(45, 3, "⠌✦", "BJ"), fx(43, 2, "⠂", "A"))),
		fr(170, "the kite reels, tumbling sideways",
			elf(elfLoose, e, 2), at(kiteReel, k+4, 1), fxs(fx(47, 0, "⠁", "A"), fx(44, 2, "⠄", "B"))),
		fr(150, "it catches the air",
			elf(elfLoose, e, 0), at(kiteDown, k+3, 0)),
		fr(150, "and climbs back",
			elf(elfLoose, e, 1), at(kiteUp, k+1, 0)),
		fr(150, "the kite hovers again",
			elf(elfLoose, e, 2), at(kiteDown, k, 0)),
	}
}

// --- spell: the elf against the slateback ---------------------------------

// The slateback is a slow crawler under a domed stone shell: bone plates
// with grey seams, an indigo head with one amber eye low at the front and
// a row of short legs that alternate as it walks.
func slateback(step int, shell byte, crack byte, crackLen int) sprite {
	legs := "   ▜▘ ▜▘ ▜▘ ▜▘ ▜▘ "
	if step%2 == 1 {
		legs = "   ▝▛ ▝▛ ▝▛ ▝▛ ▝▛ "
	}
	sh := string(shell)
	plates := func(t string) string {
		var m strings.Builder
		for _, r := range t {
			switch r {
			case ' ':
				m.WriteByte(' ')
			case '▓':
				m.WriteByte('M')
			default:
				m.WriteString(sh)
			}
		}
		return m.String()
	}
	r0 := "      ▗▄▄▄▄▄▄▖    "
	r1 := "    ▗▟█▓██▓██▓█▙  "
	r2 := "██▓██▓██▓████▌"
	r3 := "▀▀▀▀▀▀▀▀▀▀▀▀▀▘"
	s := sp(
		r0, plates(r0),
		r1, plates(r1),
		" ▗▄▖"+r2, " III"+plates(r2),
		"▐●▄▙"+r3, "IAII"+plates(r3),
		legs, "   II II II II II ",
	)
	if crack != 0 {
		// A zigzag crack down the middle of the shell, top first.
		zig := []string{"╲", "╱", "╲", "╱"}
		for i := 0; i < crackLen && i < len(zig); i++ {
			s = over(s, 10, i, sp(zig[i], string(crack)))
		}
	}
	return s
}

func spellFrames() []Frame {
	e, c := 5, 43
	return []Frame{
		fr(170, "stand-off: the slateback crawls in",
			elf(elfGuard, e, 0), at(slateback(0, 'B', 0, 0), c+1, 0)),
		fr(150, "elf reaches out, palm open",
			elf(elfReach, e, 1), at(slateback(1, 'B', 0, 0), c, 0)),
		fr(150, "hand raised, a rune forms",
			elf(elfRaise, e, 2), at(slateback(0, 'B', 0, 0), c, 0),
			fxs(fx(e+10, 0, "◇", "J"))),
		fr(150, "the rune flares",
			elf(elfRaise, e, 0), at(slateback(1, 'B', 0, 0), c-1, 0),
			fxs(fx(e+9, 0, "⠐◈⠂", "JJJ"), fx(e+10, 1, "⠁", "J"))),
		fr(100, "cast: a teal bolt leaves the hand",
			elf(elfReach, e, 1), at(slateback(0, 'B', 0, 0), c-1, 0),
			fxs(fx(e+11, 2, "⠒≈≈◆", "JJJJ"))),
		fr(100, "the bolt flies",
			elf(elfReach, e, 2), at(slateback(1, 'B', 0, 0), c-1, 0),
			fxs(fx(28, 2, "⠒≈≈◆", "JJJJ"))),
		fr(200, "impact: the shell cracks",
			elf(elfReach, e, 0), at(slateback(0, 'R', 'R', 4), c-1, 0),
			fxs(fx(39, 1, "⠐✦", "AJ"), fx(40, 2, "✦", "J"), fx(39, 3, "⠌✦", "BJ"))),
		fr(170, "shards fly, the crawler is pushed back",
			elf(elfGuard, e, 1), at(slateback(1, 'B', 'R', 4), c, 0),
			fxs(fx(50, 0, "▘", "B"), fx(56, 0, "▝", "M"), fx(48, 1, "⠂", "B"), fx(58, 1, "▖", "B"))),
		fr(170, "the crack glows as it knits",
			elf(elfGuard, e, 2), at(slateback(0, 'B', 'A', 4), c, 0)),
		fr(170, "the crack closes",
			elf(elfGuard, e, 0), at(slateback(1, 'B', 'A', 2), c, 0)),
		fr(170, "whole again, it crawls on",
			elf(elfGuard, e, 1), at(slateback(0, 'B', 0, 0), c+1, 0)),
	}
}

// --- duel: the elf against the hushhand -----------------------------------

// The hushhand is a hooded shadow with six arms fanned out, bone claws, two
// amber eyes and a hem that frays into smoke. hide removes the upper ('u')
// or lower ('l') arm on the elf's side while that arm is lashing out.
func shade(col byte, hide string) sprite {
	l := []string{
		"  ◂╲  ▗▟▙▖  ╱▸  ", "  BI  IIII  IB  ",
		"    ╲▟█●●█▙╱    ", "    IIIAAIII    ",
		"◂━━━━██████━━━━▸", "BIIIIIIIIIIIIIIB",
		"    ╱▐█▓▓█▌╲    ", "    IIIIIIII    ",
		"  ◂╱ ▝▛▘▝▜▘ ╲▸  ", "  BI IIIIII IB  ",
	}
	blank := func(i, from, to int) {
		r := []rune(l[i])
		m := []byte(l[i+1])
		for x := from; x < to; x++ {
			r[x], m[x] = ' ', ' '
		}
		l[i], l[i+1] = string(r), string(m)
	}
	if strings.Contains(hide, "u") {
		blank(0, 2, 4)
		blank(2, 4, 5)
	}
	if strings.Contains(hide, "l") {
		blank(6, 4, 5)
		blank(8, 2, 4)
	}
	return sp(l...).recolour("I", col)
}

// reach is a shadow arm stretching from the body edge to a clawed hand at
// column x. It is thick and dense at the shoulder, thins and wavers as it
// goes, and trails wisps of smoke, so it reads as a limb and not a beam.
func reach(x, row, edge int, col byte) []placement {
	n := edge - x // columns from the claw to the body
	if n < 4 {
		return nil
	}
	// The arm runs from the palm (left) to the shoulder (right): a thin
	// wavering forearm, a knobbed elbow, a thicker upper arm that turns to
	// smoke where it leaves the body.
	m := n - 3 // cells between the palm and the body
	elbow := m * 3 / 5
	arm := []rune{}
	mask := []byte{}
	wave := []rune("▄▄█▀▀█")
	for i := range m {
		var r rune
		switch {
		case i < elbow:
			r = wave[i%len(wave)]
		case i == elbow:
			r = '█'
		case i >= m-2:
			r = '▓'
		default:
			r = '▆'
		}
		arm = append(arm, r)
		mask = append(mask, col)
	}
	ps := []placement{
		// Hand: three bone claws spread from a dark palm.
		fx(x, row-1, "◂╲", "BB"),
		fx(x, row, "◂━▆", "BB"+string(col)),
		fx(x, row+1, "◂╱", "BB"),
		{s: sprite{text: []string{string(arm)}, mask: []string{string(mask)}}, x: x + 3, row: row},
		// The elbow knob sticks up out of the line.
		fx(x+3+elbow-1, row-1, "▗▄", string([]byte{col, col})),
	}
	// Wisps of smoke trail off the forearm.
	for i := 5; i < elbow; i += 5 {
		ps = append(ps, fx(x+3+i, row+1, "⠁", "M"))
	}
	return ps
}

func duelFrames() []Frame {
	e, s := 12, 42
	edge := s + 5
	return []Frame{
		fr(170, "stand-off: the hushhand spreads its arms",
			elf(elfGuard, e, 0), at(shade('I', ""), s, 0)),
		fr(120, "an upper arm reaches out",
			elf(elfGuard, e, 1), at(shade('I', "u"), s, 0), reach(31, 1, edge, 'I')),
		fr(170, "high parry: the blade turns the claw",
			elf(elfGuard, e, 2), at(shade('I', "u"), s, 0), reach(23, 1, edge, 'I'),
			fxs(fx(22, 0, "⠐✦⠂", "AJB"))),
		fr(120, "a low arm creeps in",
			elf(elfLow, e, 0), at(shade('I', "l"), s, 0), reach(31, 3, edge, 'I')),
		fr(170, "low parry, sparks",
			elf(elfLow, e, 1), at(shade('I', "l"), s, 0), reach(24, 3, edge, 'I'),
			fxs(fx(22, 2, "⠐✦⠂", "AJB"))),
		fr(110, "riposte: the elf lunges",
			elf(elfLunge, e+10, 2), at(shade('I', ""), s, 0)),
		fr(190, "the blade bites, the shadow flares red",
			elf(elfLunge, e+10, 0), at(shade('R', ""), s+1, 0),
			fxs(fx(39, 1, "⠐✦", "AJ"), fx(40, 3, "✦⠂", "JB"))),
		fr(160, "an arm frays into smoke",
			elf(elfGuard, e+6, 1), at(shade('I', "l"), s+1, 0),
			fxs(fx(40, 4, "⠒⠂ ⠐", "MMMM"))),
		fr(160, "the smoke draws back into an arm",
			elf(elfStride, e+3, 2), at(shade('I', "l"), s, 0), fxs(fx(44, 4, "⠐⠂", "MM"))),
		fr(160, "stand-off again",
			elf(elfGuard, e, 0), at(shade('I', ""), s, 0)),
	}
}

// --- forge: the elf at the anvil, a cinderkin after the sparks -------------

// The anvil stands right of the elf, its face on row 2 with the horn to the
// right. The rune blade lies on the face: amber while hot, bone as it cools,
// with a jade rune.
var anvil = sp(
	"▜███████▛▀▀", "MMMMMMMMMMM",
	"  ▝▜██▛▘   ", "  MMMMMM   ",
	"  ▟████▙   ", "  MMMMMM   ",
)

func blade(heat byte) sprite {
	h := heat
	return sp("▄▄▄▄▄▄▄▄▖", string([]byte{h, h, h, h, 'J', h, h, h, h}))
}

// The cinderkin is a small ember thief: a flame tuft, a coal body with one
// bright eye and quick legs. glow is the colour of its flame.
func cinderkin(step int, glow byte, carrying bool) sprite {
	legs := "▘▝"
	if step%2 == 1 {
		legs = "▝▘"
	}
	g := string(glow)
	s := sp(
		" ▴ ", " "+g+" ",
		"▟●▙", "RAR",
		" "+legs, " RR",
	)
	if carrying {
		s = over(s, 0, 0, sp("✦", "A"))
	}
	return s
}

func forgeFrames() []Frame {
	e, a, b := 4, 14, 15 // elf, anvil (rows 2 to 4), blade (row 1)
	work := func(heat byte) [][]placement {
		return [][]placement{at(anvil, a, 2), at(blade(heat), b, 1)}
	}
	up := func(phase int, heat byte, rest ...[]placement) Frame {
		return Frame{}.with(append(append(work(heat), elf(elfHammerUp, e, phase)), rest...)...)
	}
	down := func(phase int, heat byte, rest ...[]placement) Frame {
		return Frame{}.with(append(append(work(heat), elf(elfHammerDown, e, phase)), rest...)...)
	}
	kin := func(step int, glow byte, carrying bool, x, row int) []placement {
		return at(cinderkin(step, glow, carrying), x, row)
	}
	return []Frame{
		up(0, 'A', kin(0, 'A', false, 58, 2)).at(170, "hammer up over a hot blade, something peeks in"),
		down(1, 'A', fxs(fx(18, 0, "⠐✦ ⠂", "AABA")), kin(1, 'A', false, 56, 2)).
			at(120, "clang: sparks jump off the blade"),
		up(2, 'A', fxs(fx(23, 0, "⠁  ✦", "A  A"), fx(25, 1, "⠄", "A")), kin(0, 'A', false, 48, 2)).
			at(150, "hammer up, sparks drift; the cinderkin creeps closer"),
		down(0, 'A', fxs(fx(18, 0, "⠐✦⠂", "ABA"), fx(26, 0, "✦", "A")), kin(1, 'A', false, 38, 2)).
			at(120, "clang again, a big spark leaps"),
		up(1, 'A', kin(0, 'R', true, 30, 0), fxs(fx(29, 1, "⠂", "A"))).
			at(200, "the cinderkin jumps and snatches the spark"),
		up(2, 'A', kin(1, 'R', true, 40, 2), fxs(fx(36, 3, "⠒⠂", "AA"))).
			at(140, "and bolts with it"),
		up(0, 'B', kin(0, 'R', true, 50, 2), fxs(fx(46, 3, "⠒⠂", "AA"))).
			at(140, "the blade cools while the elf looks round"),
		up(1, 'B', kin(1, 'B', false, 57, 2), fxs(fx(56, 1, "⠐✦⠂", "AAA"))).
			at(180, "safe at a distance, it gulps the spark and flares"),
		down(2, 'A', fxs(fx(18, 0, "⠐✦", "AA")), kin(0, 'A', false, 59, 2)).
			at(150, "back to work"),
		up(0, 'A', kin(1, 'A', false, 58, 2)).at(150, "the cinderkin settles, watching the anvil"),
	}
}

// with returns f with the parts added.
func (f Frame) with(parts ...[]placement) Frame {
	for _, p := range parts {
		f.parts = append(f.parts, p...)
	}
	return f
}

// at sets the delay in milliseconds and the note.
func (f Frame) at(delay int, note string) Frame {
	f.Delay, f.Note = ms(delay), note
	return f
}

// --- watch: the elf on a ledge, a duskwing circling far off ----------------

// ledge is the ground for the watch scene: the stone top of a cliff under
// the elf with a broken edge, then nothing: the valley floor is far below.
func ledge() [Width]cell {
	var row [Width]cell
	for i := range row {
		row[i] = cell{' ', 0}
	}
	for i, r := range []rune("▇██▇███▇██▇██▛▘ ▖") {
		row[i] = cell{r, 'M'}
	}
	return row
}

// A few stars in the night sky.
var stars = []placement{fx(24, 0, "⠁", "M"), fx(33, 1, "⠂", "G"), fx(44, 0, "⠐", "M"), fx(53, 2, "⠁", "G"), fx(29, 3, "⠄", "G")}

// Far hills along the horizon, in the border colour so they sit back.
var hills = sp("▁▂▃▂▁▁  ▁▂▄▅▄▂▁    ▁▂▃▂▁▁▁  ▁▂▁", "GGGGGG  GGGGGGG    GGGGGGG  GGG")

// The duskwing is far off, so it is small: a body and two wings that beat.
var (
	wingUp   = sp("╲▾╱", "III")
	wingDown = sp("╱▾╲", "III")
	wingFlat = sp("‿▾‿", "III")
)

func watchFrames() []Frame {
	e := 4
	// The duskwing's path: a slow loop over the far valley, the far side
	// higher up.
	path := []struct{ x, row int }{
		{52, 0}, {46, 0}, {40, 1}, {36, 2}, {39, 3}, {45, 3}, {52, 3}, {57, 2}, {59, 1}, {56, 0},
	}
	wings := []sprite{wingUp, wingFlat, wingDown, wingFlat}
	poses := []pose{elfLookout, elfLookout, elfLookout, elfLookout, elfLookout, elfLookout, elfWatch, elfWatch, elfWatch, elfLookout}
	var out []Frame
	notes := []string{
		"on the ledge, a hand shading the eyes",
		"something circles over the valley",
		"it banks and drops lower",
		"the elf keeps it in sight",
		"it skims the far hills",
		"it turns back along the valley",
		"the elf lowers the hand and waits",
		"it climbs on the far side",
		"high again, wings beating",
		"the elf looks out again",
	}
	for i, p := range path {
		parts := [][]placement{
			elf(poses[i], e, i%3),
			at(hills, 22, 4),
			at(sp("◐", "B"), 60, 0),
			stars,
			at(wings[i%len(wings)], p.x, p.row),
		}
		out = append(out, fr(180, notes[i], parts...))
	}
	return out
}
