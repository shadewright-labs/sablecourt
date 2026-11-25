package contextpack

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"unicode"

	"github.com/shadewright-labs/sablecourt/internal/redact"
)

const (
	councilPeerBudget = 16 << 10
	// MaxCouncilEncodedPacketBytes bounds the JSON-encoded packet string so the
	// full launch payload fits inside the coding adapter's 64 KiB input limit.
	MaxCouncilEncodedPacketBytes = 63 << 10
	// MaxCouncilOpeningEncodedPacketBytes reserves encoded headroom for later
	// round peer excerpts when validating the opening question.
	MaxCouncilOpeningEncodedPacketBytes = 59 << 10
)

func jsonEncodedStringLen(s string) int {
	raw, err := json.Marshal(s)
	if err != nil {
		return MaxCouncilEncodedPacketBytes + 1
	}
	return len(raw)
}

func jsonEncodedContentLen(s string) int {
	n := jsonEncodedStringLen(s)
	if n < 2 {
		return n
	}
	return n - 2
}

type CouncilAnswer struct {
	Role string `json:"role"`
	Body string `json:"body"`
}

type CouncilRound struct {
	Number  int
	Answers []CouncilAnswer
}

// AssembleCouncil takes the scheduler's permitted view in round and seat order.
// It never consults the checkout or an unfiltered room message or Relic list.
func AssembleCouncil(card, question string, decrees []string, rounds []CouncilRound) (Packet, error) {
	if strings.TrimSpace(question) == "" {
		return Packet{}, errors.New("council question missing")
	}
	var b strings.Builder
	if card != "" {
		b.WriteString("## Role Card\n\n" + redact.String(card) + "\n\n")
	}
	b.WriteString("## Council so far\n\n" + redact.String(question) + "\n")
	if len(decrees) > 0 {
		b.WriteString("\n## Operator decrees\n")
		for _, d := range decrees {
			b.WriteString("\n" + redact.String(d) + "\n")
		}
	}
	prefixEncoded := jsonEncodedStringLen(b.String())
	if b.Len() > maxPacketBytes || prefixEncoded > MaxCouncilEncodedPacketBytes {
		return Packet{}, errors.New("council question and instructions exceed packet budget")
	}
	budget := min(councilPeerBudget, maxPacketBytes-b.Len())
	encodedBudget := MaxCouncilEncodedPacketBytes - prefixEncoded
	var blocks, summaries []string
	var total, encodedTotal, previous int
	var latest CouncilRound
	for _, round := range rounds {
		if round.Number <= previous || len(round.Answers) == 0 {
			return Packet{}, errors.New("invalid council round")
		}
		previous = round.Number
		latest = CouncilRound{Number: round.Number}
		for _, a := range round.Answers {
			if strings.TrimSpace(a.Role) == "" || strings.TrimSpace(a.Body) == "" {
				return Packet{}, errors.New("council answer missing role or text")
			}
			latest.Answers = append(latest.Answers, CouncilAnswer{redact.String(a.Role), redact.String(a.Body)})
		}
		block := renderCouncilRound(latest)
		blocks = append(blocks, block)
		total += len(block)
		encodedTotal += jsonEncodedContentLen(block)
		if len(blocks) < len(rounds) {
			summary := CouncilRound{Number: latest.Number}
			for _, a := range latest.Answers {
				a.Body, _, _ = strings.Cut(strings.TrimLeftFunc(a.Body, unicode.IsSpace), "\n")
				summary.Answers = append(summary.Answers, a)
			}
			summaries = append(summaries, renderCouncilRound(summary)+"[older detail omitted]\n")
		}
	}
	for i, summary := range summaries {
		if total <= budget && encodedTotal <= encodedBudget {
			break
		}
		sumEnc := jsonEncodedContentLen(summary)
		blkEnc := jsonEncodedContentLen(blocks[i])
		if len(summary) < len(blocks[i]) || sumEnc < blkEnc {
			total += len(summary) - len(blocks[i])
			encodedTotal += sumEnc - blkEnc
			blocks[i] = summary
		}
	}
	const omitted = "[older rounds omitted]\n"
	for i := range summaries {
		if total <= budget && encodedTotal <= encodedBudget {
			break
		}
		total -= len(blocks[i])
		encodedTotal -= jsonEncodedContentLen(blocks[i])
		blocks[i] = ""
		if i == 0 {
			b.WriteString(omitted)
			budget = min(councilPeerBudget-len(omitted), maxPacketBytes-b.Len())
			encodedBudget = MaxCouncilEncodedPacketBytes - jsonEncodedStringLen(b.String())
		}
	}
	if total > budget || encodedTotal > encodedBudget {
		// An oversized latest round gives each role the same maximum excerpt length.
		// Redaction above covered the entire answer, including any truncated suffix.
		const marker = "[peer text omitted]\n"
		bodies := make([][]rune, len(latest.Answers))
		var longest int
		for i, a := range latest.Answers {
			bodies[i] = []rune(a.Body)
			longest = max(longest, len(bodies[i]))
		}
		excerpt := func(limit int) string {
			for i, body := range bodies {
				latest.Answers[i].Body = string(body[:min(limit, len(body))])
			}
			return renderCouncilRound(latest) + marker
		}
		limit := sort.Search(longest+1, func(n int) bool {
			ex := excerpt(n)
			return len(ex) > budget || jsonEncodedContentLen(ex) > encodedBudget
		}) - 1
		if limit <= 0 {
			return Packet{}, errors.New("no peer text fits within the remaining council budget")
		}
		blocks[len(blocks)-1] = excerpt(limit)
	}
	for _, block := range blocks {
		b.WriteString(block)
	}
	md := b.String()
	if len(md) > maxPacketBytes || jsonEncodedStringLen(md) > MaxCouncilEncodedPacketBytes {
		return Packet{}, errors.New("council packet exceeds launch input budget")
	}
	return Packet{Markdown: md}, nil
}

func renderCouncilRound(round CouncilRound) string {
	return fmt.Sprintf("\n### Round %d\n\n", round.Number) + peerDataBlock(round.Answers)
}
