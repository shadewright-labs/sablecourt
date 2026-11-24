package contextpack

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/shadewright-labs/sablecourt/internal/redact"
)

const councilPeerBudget = 16 << 10

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
	if b.Len() > maxPacketBytes {
		return Packet{}, errors.New("council question and instructions exceed packet budget")
	}
	budget := min(councilPeerBudget, maxPacketBytes-b.Len())
	var blocks, summaries []string
	var total, previous int
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
		if len(blocks) < len(rounds) {
			summary := CouncilRound{Number: latest.Number}
			for _, a := range latest.Answers {
				a.Body, _, _ = strings.Cut(a.Body, "\n")
				summary.Answers = append(summary.Answers, a)
			}
			summaries = append(summaries, renderCouncilRound(summary)+"[older detail omitted]\n")
		}
	}
	for i, summary := range summaries {
		if total <= budget {
			break
		}
		if len(summary) < len(blocks[i]) {
			total += len(summary) - len(blocks[i])
			blocks[i] = summary
		}
	}
	const omitted = "[older rounds omitted]\n"
	for i := range summaries {
		if total <= budget {
			break
		}
		total -= len(blocks[i])
		blocks[i] = ""
		if i == 0 {
			b.WriteString(omitted)
			budget = min(councilPeerBudget, maxPacketBytes-b.Len())
		}
	}
	if total > budget {
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
		limit := sort.Search(longest+1, func(n int) bool { return len(excerpt(n)) > budget }) - 1
		if limit < 0 {
			return Packet{}, errors.New("council role labels exceed remaining peer budget")
		}
		blocks[len(blocks)-1] = excerpt(limit)
	}
	for _, block := range blocks {
		b.WriteString(block)
	}
	return Packet{Markdown: b.String()}, nil
}

func renderCouncilRound(round CouncilRound) string {
	return fmt.Sprintf("\n### Round %d\n\n", round.Number) + peerDataBlock(round.Answers)
}
