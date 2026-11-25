package conclave

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode"

	"github.com/shadewright-labs/sablecourt/internal/domain"
	"github.com/shadewright-labs/sablecourt/internal/redact"
	"github.com/shadewright-labs/sablecourt/internal/store"
)

const CouncilDigestRelicName = "digest"

// PrepareCouncilDigest renders the recorded turns for a daemon close. The
// close transaction must commit the digest and stop reason together.
func (b *Broker) PrepareCouncilDigest(ctx context.Context, roomID, reason string) (domain.ConclaveRelic, error) {
	if !store.ValidCouncilText(reason) || !store.ValidCouncilText(roomID) || redact.String(roomID) != roomID {
		return domain.ConclaveRelic{}, errors.New("invalid council id or stop reason")
	}
	councils, err := store.Councils(b.store)
	if err != nil {
		return domain.ConclaveRelic{}, err
	}
	snap, err := councils.CouncilSnapshot(ctx, roomID)
	if err != nil {
		return domain.ConclaveRelic{}, err
	}
	if snap.Session.ID != roomID || len(snap.Members) == 0 || snap.Turns == nil {
		return domain.ConclaveRelic{}, errors.New("missing council digest evidence")
	}

	type seat struct {
		round int
		role  string
	}
	turns := make(map[seat]store.CouncilTurn, len(snap.Turns))
	lastRound := 1
	for _, turn := range snap.Turns {
		key := seat{turn.Round, turn.Role}
		if _, exists := turns[key]; exists || turn.Round < 1 || turn.Round > snap.MaxRounds || turn.Round > snap.MaxStarts {
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
	for round := 1; round <= lastRound; round++ {
		fmt.Fprintf(&digest, "Round %d\n", round)
		for _, member := range snap.Members {
			role := member.Vassal.Role
			if !store.ValidCouncilText(role) || redact.String(role) != role {
				return domain.ConclaveRelic{}, errors.New("invalid council digest role")
			}
			fmt.Fprintf(&digest, "  %q\n", role)
			turn := turns[seat{round, role}]
			if err := writeCouncilDigestTurn(&digest, roomID, turn, evidence[turn.TaskID]); err != nil {
				return domain.ConclaveRelic{}, err
			}
		}
	}
	body := digest.String()
	return domain.ConclaveRelic{
		ConclaveID: roomID, Name: CouncilDigestRelicName, AuthorRole: "system", ContentType: "text/plain",
		Body: body, SHA256: fmt.Sprintf("%x", sha256.Sum256([]byte(body))),
	}, nil
}

func writeCouncilDigestTurn(digest *strings.Builder, roomID string, turn store.CouncilTurn, evidence []domain.ConclaveRelic) error {
	if answer := turn.Answer; answer != nil {
		if turn.State != domain.TaskStateCompleted || answer.ConclaveID != roomID ||
			answer.FromRole != turn.Role || answer.TaskID != turn.TaskID || !store.ValidCouncilText(answer.Body) {
			return errors.New("invalid recorded answer in council digest")
		}
		digest.WriteString("    Answer\n")
		for line := range strings.SplitSeq(redact.String(answer.Body), "\n") {
			line = strings.TrimSuffix(line, "\r")
			if strings.ContainsFunc(line, func(r rune) bool { return unicode.IsControl(r) && r != '\t' }) {
				line = strconv.Quote(line)
			}
			fmt.Fprintf(digest, "      %s\n", line)
			if digest.Len() > store.MaxConclaveTurnInput {
				return errors.New("council digest exceeds size limit")
			}
		}
	} else {
		digest.WriteString("    Missing answer\n")
	}
	// CouncilSnapshot returns each attempt's evidence in name order.
	for _, relic := range evidence {
		hash, err := hex.DecodeString(relic.SHA256)
		if err != nil || len(hash) != sha256.Size || relic.ConclaveID != roomID ||
			relic.AuthorRole != turn.Role || !store.ValidCouncilText(relic.Name) {
			return errors.New("invalid evidence reference in council digest")
		}
		fmt.Fprintf(digest, "    Evidence %q (sha256 %s)\n", redact.String(relic.Name), relic.SHA256)
		if digest.Len() > store.MaxConclaveTurnInput {
			return errors.New("council digest exceeds size limit")
		}
	}
	if digest.Len() > store.MaxConclaveTurnInput {
		return errors.New("council digest exceeds size limit")
	}
	return nil
}
