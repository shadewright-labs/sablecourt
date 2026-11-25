package store

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/shadewright-labs/sablecourt/internal/domain"
	"github.com/shadewright-labs/sablecourt/internal/redact"
)

var privateKeyHeader = regexp.MustCompile(`-----BEGIN ([A-Z0-9 ]*PRIVATE KEY)-----`)

// A truncated final artifact can end inside a PEM block. Only a matching
// footer ends that block; a malformed nested header also consumes the tail.
func RedactCouncilAnswer(text string) string {
	var out strings.Builder
	for {
		header := privateKeyHeader.FindStringSubmatchIndex(text)
		if header == nil {
			out.WriteString(text)
			break
		}
		out.WriteString(text[:header[0]])
		out.WriteString("[REDACTED:private_key]")
		body, tail, closed := strings.Cut(text[header[1]:], "-----END "+text[header[2]:header[3]]+"-----")
		if !closed || privateKeyHeader.MatchString(body) {
			break
		}
		text = tail
	}
	return redact.String(out.String())
}

// Digest renders the recorded turns in declared seat order from this snapshot.
func (snap CouncilSnapshot) Digest(reason string) (domain.ConclaveRelic, error) {
	roomID := snap.Session.ID
	if !ValidCouncilText(reason) || !ValidCouncilText(roomID) || redact.String(roomID) != roomID || len(snap.Members) == 0 || snap.Turns == nil {
		return domain.ConclaveRelic{}, errors.New("missing council digest evidence")
	}
	roles := map[string]bool{}
	for _, member := range snap.Members {
		role := member.Vassal.Role
		if !ValidCouncilText(role) || redact.String(role) != role || roles[role] {
			return domain.ConclaveRelic{}, errors.New("invalid council digest role")
		}
		roles[role] = true
	}
	type seat struct {
		round int
		role  string
	}
	turns := make(map[seat]CouncilTurn, len(snap.Turns))
	lastRound := 1
	for _, turn := range snap.Turns {
		key := seat{turn.Round, turn.Role}
		if _, exists := turns[key]; exists || !roles[turn.Role] || turn.NothingToAdd && turn.Answer == nil || turn.Round < 1 || turn.Round > snap.MaxRounds || turn.Round > snap.MaxStarts || turn.Round > domain.ConclaveRunCeiling {
			return domain.ConclaveRelic{}, errors.New("invalid council digest turn")
		}
		turns[key] = turn
		lastRound = max(lastRound, turn.Round)
	}
	evidence := make(map[string][]domain.ConclaveRelic, len(snap.Attempts))
	for _, attempt := range snap.Attempts {
		if turn := turns[seat{attempt.Round, attempt.Role}]; turn.TaskID == attempt.TaskID {
			evidence[turn.TaskID] = attempt.Relics
		}
	}
	var digest strings.Builder
	fmt.Fprintf(&digest, "Stop reason %q\n", redact.String(reason))
	// Reserve space for every round header, then share the rest between seats.
	var headers int
	for round := 1; round <= lastRound; round++ {
		headers += len(fmt.Sprintf("Round %d\n", round))
	}
	perTurn := (MaxConclaveTurnInput - digest.Len() - headers) / lastRound / len(snap.Members)
	if perTurn < len(digestOmission) {
		return domain.ConclaveRelic{}, errors.New("invalid council digest dimensions")
	}
	for round := 1; round <= lastRound; round++ {
		fmt.Fprintf(&digest, "Round %d\n", round)
		for _, member := range snap.Members {
			role := member.Vassal.Role
			turn := turns[seat{round, role}]
			out := digestTurn{remaining: perTurn, out: &digest}
			out.write(fmt.Sprintf("  %q\n", role))
			if err := writeCouncilDigestTurn(&out, roomID, turn, evidence[turn.TaskID]); err != nil {
				return domain.ConclaveRelic{}, err
			}
		}
	}
	body := digest.String()
	return domain.ConclaveRelic{ConclaveID: roomID, Name: CouncilDigestRelicName, AuthorRole: "system", ContentType: "text/plain", Body: body, SHA256: fmt.Sprintf("%x", sha256.Sum256([]byte(body)))}, nil
}

const digestOmission = "    [omitted]\n"

type digestTurn struct {
	out       *strings.Builder
	remaining int
	omitted   bool
}

func (d *digestTurn) write(text string) {
	if d.omitted {
		return
	}
	if len(text)+len(digestOmission) <= d.remaining {
		d.out.WriteString(text)
		d.remaining -= len(text)
		return
	}
	n := max(0, d.remaining-len(digestOmission)-1)
	if n > 0 {
		for n < len(text) && !utf8.RuneStart(text[n]) {
			n--
		}
		d.out.WriteString(text[:min(n, len(text))])
		d.out.WriteByte('\n')
	}
	d.out.WriteString(digestOmission)
	d.omitted = true
}

func writeCouncilDigestTurn(digest *digestTurn, roomID string, turn CouncilTurn, evidence []domain.ConclaveRelic) error {
	if answer := turn.Answer; answer != nil {
		if turn.State != domain.TaskStateCompleted || answer.ConclaveID != roomID ||
			answer.FromRole != turn.Role || answer.TaskID != turn.TaskID || !ValidCouncilText(answer.Body) {
			return errors.New("invalid recorded answer in council digest")
		}
		digest.write("    Answer\n")
		for line := range strings.SplitSeq(RedactCouncilAnswer(answer.Body), "\n") {
			if digest.omitted {
				break
			}
			line = strings.TrimSuffix(line, "\r")
			if strings.ContainsFunc(line, func(r rune) bool { return unicode.IsControl(r) && r != '\t' }) {
				line = strconv.Quote(line)
			}
			digest.write("      " + line + "\n")
		}
	} else {
		digest.write("    Missing answer\n")
	}
	for _, relic := range evidence {
		hash, err := hex.DecodeString(relic.SHA256)
		if err != nil || len(hash) != sha256.Size || relic.ConclaveID != roomID ||
			relic.AuthorRole != turn.Role || !ValidCouncilText(relic.Name) || relic.Name == CouncilDigestRelicName {
			return errors.New("invalid evidence reference in council digest")
		}
		if !digest.omitted {
			digest.write(fmt.Sprintf("    Evidence %q (sha256 %s)\n", redact.String(relic.Name), relic.SHA256))
		}
	}
	return nil
}
