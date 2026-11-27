package waitart

// The elf faces right. Rows 0 and 1 are the hood: a peak that leans back,
// an indigo ear tip poking out behind it, a jade eye slit in the dark face
// opening and the hood's front lip. Columns 0 to 2 at rows 3 and 4 hold the
// cloak hem, which changes from frame to frame (see hem).

const (
	hood0 = "    ▗▙ "
	mask0 = "    CC "
	hood1 = "  ▚▟██━▖"
	mask1 = "  ICCCJC"
)

// pose is one stance in three hem phases.
type pose [3]sprite

// mkPose builds a pose from what is drawn right of the two hood rows and
// the three body rows.
func mkPose(r0, m0, r1, m1 string, body ...string) pose {
	base := sp(append([]string{hood0 + r0, mask0 + m0, hood1 + r1, mask1 + m1}, body...)...)
	var p pose
	for i := range p {
		p[i] = over(base, 0, 3, hem[i])
	}
	return p
}

// hem is the back edge of the cloak at rows 3 and 4: hanging, lifted, swept
// back. It joins the body at column 3.
var hem = [3]sprite{
	sp(
		"   ", "   ",
		" ▗▛", " CC",
	),
	sp(
		"  ▗", "  C",
		"▗▛▘", "CCC",
	),
	sp(
		"▗▄▟", "CCC",
		"▘  ", "C  ",
	),
}

var (
	// Blade raised, ready.
	elfGuard = mkPose(
		"   ╱", "   J",
		" ╱", " J",
		"   ▐█▙▄▄╋", "   CCCCCB",
		"   ██▛▀  ", "   CCCC  ",
		"   ▟▘ ▝▙ ", "   CC CC ",
	)
	elfStride = mkPose(
		"   ╱", "   J",
		" ╱", " J",
		"   ▐█▙▄▄╋", "   CCCCCB",
		"   ██▛▀  ", "   CCCC  ",
		"  ▗▛   ▜▖", "  CC   CC",
	)
	// Blade swung overhead and back.
	elfWindup = mkPose(
		"╺━━╋", "JJJB",
		" ▟▘", " CC",
		"   ▐█▙▄▟", "   CCCCC",
		"   ██▛▀ ", "   CCCC ",
		"  ▗▛   ▜▖", "  CC   CC",
	)
	// Full lunge, blade level.
	elfLunge = mkPose(
		"", "",
		"", "",
		"   ▐█▙▄▄▄╋━━━━━━━╸", "   CCCCCCBJJJJJJJJ",
		"   ██▛▀           ", "   CCCC           ",
		"  ▟▘    ▝▙        ", "  CC    CC        ",
	)
	// Low parry: blade angled down in front.
	elfLow = mkPose(
		"", "",
		"", "",
		"   ▐█▙▄▄╋  ", "   CCCCCB  ",
		"   ██▛▀  ╲ ", "   CCCC  J ",
		"   ▟▘ ▝▙  ╲", "   CC CC  J",
	)
	// Bow at full draw: stave in bone, string in muted grey pulled back to
	// the hand, arrow nocked.
	elfDraw = mkPose(
		"   ╲", "   B",
		" ╱ ┃", " M B",
		"   ▐█▙▄━━━━┃▸", "   CCCCBBBBBJ",
		"   ██▛▀  ╲ ┃", "   CCCC  M B",
		"   ▟▘ ▝▙  ╱", "   CC CC  B",
	)
	// Bow held up, string straight, nothing nocked.
	elfLoose = mkPose(
		"   ╲", "   B",
		"  │┃", "  MB",
		"   ▐█▙▄▄▄▄│┃", "   CCCCCCCMB",
		"   ██▛▀   │┃", "   CCCC   MB",
		"   ▟▘ ▝▙  ╱", "   CC CC  B",
	)
	// Palm forward, ready to cast.
	elfReach = mkPose(
		"", "",
		"", "",
		"   ▐█▙▄▄▄▸", "   CCCCCCM",
		"   ██▛▀   ", "   CCCC   ",
		"   ▟▘ ▝▙  ", "   CC CC  ",
	)
	// Hand raised high, drawing the rune.
	elfRaise = mkPose(
		"  ▗", "  M",
		" ▟▘", " CC",
		"   ▐█▙▄▟ ", "   CCCCC ",
		"   ██▛▀  ", "   CCCC  ",
		"   ▟▘ ▝▙ ", "   CC CC ",
	)
	// Forge hammer raised over the head: an iron head above the hood and
	// the haft down to the raised hand.
	elfHammerUp = mkPose(
		"▄█▄", "MMM",
		"▐", "A",
		"   ▐█▙▄▟", "   CCCCC",
		"   ██▛▀ ", "   CCCC ",
		"   ▟▘ ▝▙", "   CC CC",
	)
	// Hammer brought down: the haft level and the head on the blade.
	elfHammerDown = mkPose(
		"", "",
		"━━━━▆█", "AAAAMM",
		"   ▐█▙▄▟", "   CCCCC",
		"   ██▛▀ ", "   CCCC ",
		"  ▗▛   ▜▖", "  CC   CC",
	)
	// On lookout: a hand raised to shade the eyes, bow slung on the back.
	elfLookout = mkPose(
		"", "",
		"▄", "C",
		"   ▐█▙▟▘", "   CCCCC",
		"   ██▛▀ ", "   CCCC ",
		"   ▟▘ ▝▙", "   CC CC",
	)
	// On lookout, the hand lowered to the hip.
	elfWatch = mkPose(
		"", "",
		"", "",
		"   ▐█▙  ", "   CCC  ",
		"   ██▛▙ ", "   CCCC ",
		"   ▟▘ ▝▙", "   CC CC",
	)
)

// elf places a pose at x with the hem in a given phase.
func elf(p pose, x, phase int) []placement {
	return []placement{{p[phase%3], x, 0}}
}
