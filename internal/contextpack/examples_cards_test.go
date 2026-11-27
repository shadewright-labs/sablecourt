package contextpack_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/shadewright-labs/sablecourt/internal/config"
	"github.com/shadewright-labs/sablecourt/internal/contextpack"
)

// The packet holds at most MaxPacketBytes. The role card is required text and is
// never trimmed. Optional text (learnings, decrees, relics, the pre-check) is
// dropped whole, oldest first, to fit what the required part leaves.
//
// Margin, from internal/contextpack/assembler.go:
//   - the pre-check can fill MaxPrecheckBytes (16 KiB) on its own;
//   - learnings are read 20 at a time (learnings.go), reserved here as 8 KiB,
//     about 400 bytes each;
//   - the packet header, intent and a failure handoff are reserved as 2 KiB.
//
// What is left for the card is 32 - 16 - 8 - 2 = 6 KiB. This is a byte budget
// with stated reserves: the test does not assemble a packet. Author cards must also
// stay near the 1 KiB the ADR-0010 amendment asks for, so the limit is 1.25 KiB.
const (
	learningsReserve = 8 << 10
	headerReserve    = 2 << 10
	authorCardLimit  = 1280
)

func cardLimit() int {
	return contextpack.MaxPacketBytesForTest - contextpack.MaxPrecheckBytesForTest - learningsReserve - headerReserve
}

func TestExampleCodingCourtCardsFitContextPacket(t *testing.T) {
	root := filepath.Join("..", "..", "examples", "coding-court")
	if got := cardLimit(); got != 6<<10 {
		t.Fatalf("card limit %d, the reasoning in this test assumes 6 KiB", got)
	}
	banned := []string{"claude", "codex", "agy", "gemini", "gpt-", "tier", "verdict", "critic"}

	for _, set := range []struct {
		name    string
		path    string
		authors []string
		cards   []string
		prefix  string
	}{
		{"generic", "coding-court.yaml", []string{"developer-go", "developer-docs"}, []string{"reviewer", "security"}, ""},
		{"sablecourt", filepath.Join("sablecourt", "coding-court.yaml"), []string{"sablecourt-developer-go", "sablecourt-developer-docs"}, []string{"sablecourt-reviewer", "sablecourt-security"}, "sablecourt-"},
	} {
		t.Run(set.name, func(t *testing.T) {
			dir := filepath.Dir(filepath.Join(root, set.path))
			bundle, err := config.LoadPath(filepath.Join(root, set.path))
			if err != nil {
				t.Fatalf("load %s: %v", set.path, err)
			}
			architect, err := os.ReadFile(filepath.Join(dir, "architect-question.md"))
			if err != nil {
				t.Fatal(err)
			}
			cards := map[string]string{"architect": string(architect)}
			for _, name := range append(append([]string{}, set.authors...), set.cards...) {
				wl, ok := bundle.Workloads[name]
				if !ok {
					t.Fatalf("workload %q missing", name)
				}
				cards[name] = strings.TrimSpace(wl.Spec.RoleCard)
			}
			for name, card := range cards {
				if card == "" {
					t.Errorf("%s: empty card", name)
				}
				if len(card) > cardLimit() {
					t.Errorf("%s: card is %d bytes, limit %d", name, len(card), cardLimit())
				}
				lower := strings.ToLower(card)
				for _, word := range banned {
					if strings.Contains(lower, word) {
						t.Errorf("%s: card names %q, which belongs to the workflow step", name, word)
					}
				}
				if strings.ContainsAny(card, "\u2013\u2014") {
					t.Errorf("%s: card has a long dash", name)
				}
			}
			for _, name := range set.authors {
				if n := len(cards[name]); n > authorCardLimit {
					t.Errorf("%s: author card is %d bytes, limit %d", name, n, authorCardLimit)
				}
			}
			for _, need := range []string{"numbered list", "recommended answer", "main alternative", "Decree", "round two"} {
				if !strings.Contains(cards["architect"], need) {
					t.Errorf("architect text lacks %q", need)
				}
			}
			for _, name := range set.cards {
				if !strings.Contains(cards[name], "file and line") {
					t.Errorf("%s: verifier card lacks the file and line evidence rule", name)
				}
			}
		})
	}
}
