package store_test

import (
	"crypto/sha256"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/shadewright-labs/sablecourt/internal/domain"
	"github.com/shadewright-labs/sablecourt/internal/store"
)

func relicHash(s string) string { return fmt.Sprintf("%x", sha256.Sum256([]byte(s))) }

func digestSnapshot() store.CouncilSnapshot {
	answer := func(role, task, body string) *domain.ConclaveMessage {
		return &domain.ConclaveMessage{ConclaveID: "room", FromRole: role, TaskID: task, Kind: "review", Body: body}
	}
	return store.CouncilSnapshot{
		CouncilPolicy: store.CouncilPolicy{
			MaxRounds: 2, MaxStarts: 4,
			Members: []store.CouncilMember{
				{Vassal: domain.ConclaveVassal{Role: "reviewer"}},
				{Vassal: domain.ConclaveVassal{Role: "author"}},
			},
		},
		Session: domain.ConclaveSession{ID: "room"},
		Turns: []store.CouncilTurn{
			{Round: 1, Role: "author", TaskID: "a1", State: domain.TaskStateCompleted, Answer: answer("author", "a1", "Looks fine.\r\nShip it.")},
			{Round: 1, Role: "reviewer", TaskID: "r1", State: domain.TaskStateCompleted, Answer: answer("reviewer", "r1", "One nit.")},
			{Round: 2, Role: "reviewer", TaskID: "r2", State: domain.TaskStateFailed},
		},
		Attempts: []store.CouncilAttempt{
			{Round: 1, Role: "reviewer", TaskID: "r1", Relics: []domain.ConclaveRelic{
				{ConclaveID: "room", AuthorRole: "reviewer", Name: "notes.md", SHA256: relicHash("notes")},
			}},
			// An attempt whose task is not the recorded turn contributes no evidence.
			{Round: 1, Role: "author", TaskID: "a0", Relics: []domain.ConclaveRelic{
				{ConclaveID: "room", AuthorRole: "author", Name: "stale.md", SHA256: relicHash("stale")},
			}},
		},
	}
}

func TestCouncilDigestRendering(t *testing.T) {
	snap := digestSnapshot()
	relic, err := snap.Digest("unanimous")
	if err != nil {
		t.Fatal(err)
	}
	want := "Stop reason \"unanimous\"\n" +
		"Round 1\n" +
		"  \"reviewer\"\n    Answer\n      One nit.\n    Evidence \"notes.md\" (sha256 " + relicHash("notes") + ")\n" +
		"  \"author\"\n    Answer\n      Looks fine.\n      Ship it.\n" +
		"Round 2\n" +
		"  \"reviewer\"\n    Missing answer\n" +
		"  \"author\"\n    Missing answer\n"
	if relic.Body != want {
		t.Fatalf("digest body:\n%s\nwant:\n%s", relic.Body, want)
	}
	if relic.ConclaveID != "room" || relic.Name != store.CouncilDigestRelicName || relic.AuthorRole != "system" ||
		relic.ContentType != "text/plain" || relic.SHA256 != relicHash(want) {
		t.Fatalf("digest relic = %+v", relic)
	}
	again, err := digestSnapshot().Digest("unanimous")
	if err != nil || again != relic {
		t.Fatalf("digest is not deterministic: %+v, %v", again, err)
	}
}

func TestCouncilDigestChangesWithEveryField(t *testing.T) {
	base, err := digestSnapshot().Digest("unanimous")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		reason string
		mutate func(*store.CouncilSnapshot)
	}{
		{"reason", "deadline", func(*store.CouncilSnapshot) {}},
		{"seat order", "", func(s *store.CouncilSnapshot) { s.Members[0], s.Members[1] = s.Members[1], s.Members[0] }},
		{"answer body", "", func(s *store.CouncilSnapshot) { s.Turns[1].Answer.Body = "Two nits." }},
		{"evidence name", "", func(s *store.CouncilSnapshot) { s.Attempts[0].Relics[0].Name = "other.md" }},
		{"evidence hash", "", func(s *store.CouncilSnapshot) { s.Attempts[0].Relics[0].SHA256 = relicHash("changed") }},
		{"turn answered", "", func(s *store.CouncilSnapshot) {
			s.Turns[2].State = domain.TaskStateCompleted
			s.Turns[2].Answer = &domain.ConclaveMessage{ConclaveID: "room", FromRole: "reviewer", TaskID: "r2", Body: "Late."}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			snap := digestSnapshot()
			tc.mutate(&snap)
			reason := cmpOr(tc.reason, "unanimous")
			got, err := snap.Digest(reason)
			if err != nil {
				t.Fatal(err)
			}
			if got.Body == base.Body || got.SHA256 == base.SHA256 {
				t.Fatalf("digest did not change when %s changed", tc.name)
			}
		})
	}
}

// The room is not in the body; it is carried on the relic.
func TestCouncilDigestRoom(t *testing.T) {
	snap := digestSnapshot()
	snap.Session.ID = "room2"
	for i := range snap.Turns {
		if snap.Turns[i].Answer != nil {
			snap.Turns[i].Answer.ConclaveID = "room2"
		}
	}
	snap.Attempts[0].Relics[0].ConclaveID = "room2"
	relic, err := snap.Digest("unanimous")
	if err != nil || relic.ConclaveID != "room2" {
		t.Fatalf("digest of another room = %+v, %v", relic, err)
	}
}

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func TestCouncilDigestRefusals(t *testing.T) {
	for _, tc := range []struct {
		name   string
		reason string
		mutate func(*store.CouncilSnapshot)
	}{
		{"blank reason", " ", func(*store.CouncilSnapshot) {}},
		{"no room", "", func(s *store.CouncilSnapshot) { s.Session.ID = "" }},
		{"secret room id", "", func(s *store.CouncilSnapshot) { s.Session.ID = githubToken() }},
		{"no members", "", func(s *store.CouncilSnapshot) { s.Members = nil }},
		{"nil turns", "", func(s *store.CouncilSnapshot) { s.Turns = nil }},
		{"duplicate role", "", func(s *store.CouncilSnapshot) { s.Members[1].Vassal.Role = "reviewer" }},
		{"blank role", "", func(s *store.CouncilSnapshot) { s.Members[1].Vassal.Role = "" }},
		{"secret role", "", func(s *store.CouncilSnapshot) { s.Members[1].Vassal.Role = githubToken() }},
		{"duplicate turn", "", func(s *store.CouncilSnapshot) { s.Turns = append(s.Turns, s.Turns[0]) }},
		{"turn of unknown role", "", func(s *store.CouncilSnapshot) { s.Turns[2].Role = "intruder" }},
		{"stop vote without answer", "", func(s *store.CouncilSnapshot) { s.Turns[2].NothingToAdd = true }},
		{"round zero", "", func(s *store.CouncilSnapshot) { s.Turns[2].Round = 0 }},
		{"round past max rounds", "", func(s *store.CouncilSnapshot) { s.MaxRounds = 1 }},
		{"round past max starts", "", func(s *store.CouncilSnapshot) { s.MaxStarts = 1 }},
		{"round past the run ceiling", "", func(s *store.CouncilSnapshot) {
			s.MaxRounds, s.MaxStarts = 100, 100
			s.Turns[2].Round = domain.ConclaveRunCeiling + 1
		}},
		{"answer of unfinished task", "", func(s *store.CouncilSnapshot) { s.Turns[0].State = domain.TaskStateWorking }},
		{"answer from another room", "", func(s *store.CouncilSnapshot) { s.Turns[0].Answer.ConclaveID = "elsewhere" }},
		{"answer from another role", "", func(s *store.CouncilSnapshot) { s.Turns[0].Answer.FromRole = "reviewer" }},
		{"answer from another task", "", func(s *store.CouncilSnapshot) { s.Turns[0].Answer.TaskID = "x" }},
		{"blank answer", "", func(s *store.CouncilSnapshot) { s.Turns[0].Answer.Body = "\n" }},
		{"evidence with bad hash", "", func(s *store.CouncilSnapshot) { s.Attempts[0].Relics[0].SHA256 = "zz" }},
		{"evidence with short hash", "", func(s *store.CouncilSnapshot) { s.Attempts[0].Relics[0].SHA256 = "abcd" }},
		{"evidence from another room", "", func(s *store.CouncilSnapshot) { s.Attempts[0].Relics[0].ConclaveID = "x" }},
		{"evidence by another role", "", func(s *store.CouncilSnapshot) { s.Attempts[0].Relics[0].AuthorRole = "author" }},
		{"evidence without name", "", func(s *store.CouncilSnapshot) { s.Attempts[0].Relics[0].Name = "" }},
		{"evidence named digest", "", func(s *store.CouncilSnapshot) { s.Attempts[0].Relics[0].Name = store.CouncilDigestRelicName }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			snap := digestSnapshot()
			tc.mutate(&snap)
			relic, err := snap.Digest(cmpOr(tc.reason, "unanimous"))
			if err == nil {
				t.Fatalf("Digest accepted the snapshot: %q", relic.Body)
			}
			if relic != (domain.ConclaveRelic{}) {
				t.Fatalf("refused digest returned a relic: %+v", relic)
			}
		})
	}
}

func TestCouncilDigestBoundsAndRedaction(t *testing.T) {
	snap := digestSnapshot()
	pem := "-----BEGIN " + "RSA PRIVATE KEY-----\nMIIabc\n-----END " + "RSA PRIVATE KEY-----"
	snap.Turns[1].Answer.Body = "before\n" + pem + "\nbell\x07here"
	relic, err := snap.Digest("done")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(relic.Body, "MIIabc") || !strings.Contains(relic.Body, "[REDACTED:private_key]") {
		t.Fatalf("private key not redacted:\n%s", relic.Body)
	}
	if !strings.Contains(relic.Body, `"bell\ahere"`) {
		t.Fatalf("control character not quoted:\n%s", relic.Body)
	}

	// With many seats each one gets a small share; a large answer is cut at a rune
	// boundary and the rest of that seat is marked omitted.
	snap = digestSnapshot()
	for i := range 38 {
		snap.Members = append(snap.Members, store.CouncilMember{Vassal: domain.ConclaveVassal{Role: fmt.Sprint("seat", i)}})
	}
	snap.MaxRounds, snap.MaxStarts = 6, 6
	snap.Turns[2].Round = 6
	snap.Turns[1].Answer.Body = "x" + strings.Repeat("é", store.MaxCouncilText/2-1)
	relic, err = snap.Digest("done")
	if err != nil {
		t.Fatal(err)
	}
	if len(relic.Body) > store.MaxConclaveTurnInput {
		t.Fatalf("digest is %d bytes, over %d", len(relic.Body), store.MaxConclaveTurnInput)
	}
	if !utf8.ValidString(relic.Body) {
		t.Fatal("answer cut inside a rune")
	}
	reviewer := relic.Body[strings.Index(relic.Body, "\"reviewer\""):strings.Index(relic.Body, "\"author\"")]
	if !strings.HasSuffix(reviewer, "    [omitted]\n  ") || strings.Contains(reviewer, "Evidence") {
		t.Fatalf("oversized seat not cut and marked: %d bytes, tail %q", len(reviewer), reviewer[max(0, len(reviewer)-40):])
	}
	if !strings.Contains(relic.Body, "Round 6\n") || !strings.Contains(relic.Body, "  \"seat37\"\n    Missing answer\n") {
		t.Fatal("digest lost later seats or rounds")
	}
}
