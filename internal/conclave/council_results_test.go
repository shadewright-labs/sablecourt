package conclave

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/shadewright-labs/sablecourt/internal/domain"
	scruntime "github.com/shadewright-labs/sablecourt/internal/runtime"
	"github.com/shadewright-labs/sablecourt/internal/store"
)

func resultArtifact(body string) string {
	raw, err := json.Marshal([]scruntime.Artifact{{Name: "final-message", ContentType: "text/plain", Text: body}})
	if err != nil {
		panic(err)
	}
	return string(raw)
}

func resultCouncil(t *testing.T, roles ...string) (store.Store, store.CouncilStore, domain.ConclaveSession) {
	t.Helper()
	st := brokerTestStore(t)
	councils, err := store.Councils(st)
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	if err := st.UpsertWorkload(ctx, domain.WorkloadRecord{Name: "worker", Zone: domain.ZoneSandbox}); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	room := domain.ConclaveSession{ID: "panel", State: domain.ConclaveStateActive, CreatedAt: now, UpdatedAt: now}
	spec := store.CouncilSpec{Question: "What should ship?"}
	for _, role := range roles {
		spec.Members = append(spec.Members, store.CouncilMember{
			Vassal: domain.ConclaveVassal{Role: role, Workload: "worker", CanMessage: []string{"all"}},
			Zone:   domain.ZoneSandbox,
		})
	}
	if err := councils.CreateCouncil(ctx, room, spec); err != nil {
		t.Fatal(err)
	}
	return st, councils, room
}

func reserveResult(t *testing.T, st store.Store, room, role string, round, attempt int) domain.Task {
	t.Helper()
	a := store.ConclaveAssignment{ConclaveID: room, Role: role, Round: round, Attempt: attempt}
	raw, err := json.Marshal(a)
	if err != nil {
		t.Fatal(err)
	}
	task := domain.Task{
		ID: fmt.Sprintf("%s-%d-%d", role, round, attempt), WorkloadName: "worker",
		State: domain.TaskStateSubmitted, InputJSON: string(raw), IdempotencyKey: a.TurnKey(),
	}
	_, err = store.SubmitConclaveTurn(t.Context(), room, task.IdempotencyKey, store.ConclaveLimits{Now: time.Now},
		func(ctx context.Context) (domain.Task, error) { return task, st.CreateTask(ctx, task) })
	if err != nil {
		t.Fatal(err)
	}
	return task
}

func finishResult(t *testing.T, st store.Store, task domain.Task, state domain.TaskState, facts, artifacts string) {
	t.Helper()
	if err := st.UpdateTaskArtifacts(t.Context(), task.ID, artifacts); err != nil {
		t.Fatal(err)
	}
	if err := st.UpdateTaskState(t.Context(), task.ID, task.State, state, "runtime_exit_observed", facts); err != nil {
		t.Fatal(err)
	}
}

func TestCouncilResultArtifacts(t *testing.T) {
	const success = `{"exit_code":0}`
	for _, tc := range []struct {
		name, artifacts, exit, want string
		vote                        bool
		state                       domain.TaskState
	}{
		{name: "redacts and replays", artifacts: resultArtifact("Use plan one. password=syntheticfixture"), want: "Use plan one. password=[REDACTED:secret]"},
		{name: "unclosed private key", artifacts: resultArtifact("Before\n-----BEGIN" + " RSA PRIVATE KEY-----\nfixture\ntrailer"), want: "Before\n[REDACTED:private_key]"},
		{name: "mismatched key footer", artifacts: resultArtifact("Before\n-----BEGIN" + " RSA PRIVATE KEY-----\nfixture\n-----END EC PRIVATE KEY-----\ntrailer"), want: "Before\n[REDACTED:private_key]"},
		{name: "closed then unclosed key", artifacts: resultArtifact("-----BEGIN" + " PRIVATE KEY-----\nx\n-----END PRIVATE KEY-----\nkeep\n-----BEGIN" + " OPENSSH PRIVATE KEY-----\ny"), want: "[REDACTED:private_key]\nkeep\n[REDACTED:private_key]"},
		{name: "explicit vote", artifacts: resultArtifact(" \n" + CouncilNothingToAdd + "\n"), want: CouncilNothingToAdd, vote: true},
		{name: "quotes peer", artifacts: resultArtifact("> " + CouncilNothingToAdd + "\nI object."), want: "> " + CouncilNothingToAdd + "\nI object."},
		{name: "phrase in prose", artifacts: resultArtifact("I read NOTHING TO ADD in the peer answer."), want: "I read NOTHING TO ADD in the peer answer."},
		{name: "failed process", artifacts: resultArtifact(CouncilNothingToAdd), state: domain.TaskStateFailed, exit: `{"exit_code":17}`},
		{name: "nonzero exit marked complete", artifacts: resultArtifact(CouncilNothingToAdd), exit: `{"exit_code":17}`},
		{name: "null exit code", artifacts: resultArtifact(CouncilNothingToAdd), exit: `{"exit_code":null}`},
		{name: "wrong exit code type", artifacts: resultArtifact(CouncilNothingToAdd), exit: `{"exit_code":"0"}`},
		{name: "missing exit code", artifacts: resultArtifact(CouncilNothingToAdd), exit: `{}`},
		{name: "duplicate exit code", artifacts: resultArtifact(CouncilNothingToAdd), exit: `{"exit_code":17,"exit_code":0}`},
		{name: "blank text", artifacts: resultArtifact(" \t\n")},
		{name: "duplicate final artifacts", artifacts: `[{"name":"final-message","contentType":"text/plain","text":"one"},{"name":"final-message","contentType":"text/plain","text":"two"}]`},
		{name: "only diagnostic", artifacts: `[{"name":"session-log","contentType":"text/plain","text":"NOTHING TO ADD"}]`},
		{name: "wrong type", artifacts: `[{"name":"final-message","contentType":"text/plain","text":123}]`},
		{name: "wrong content type", artifacts: `[{"name":"final-message","contentType":"text/html","text":"NOTHING TO ADD"}]`},
		{name: "indirect artifact", artifacts: `[{"name":"final-message","contentType":"text/plain","uri":"opaque-ref","text":"NOTHING TO ADD"}]`},
		{name: "null text", artifacts: `[{"name":"final-message","contentType":"text/plain","text":null}]`},
		{name: "null array", artifacts: `null`},
		{name: "null member", artifacts: `[null,{"name":"final-message","contentType":"text/plain","text":"NOTHING TO ADD"}]`},
		{name: "object for array", artifacts: `{"name":"final-message","contentType":"text/plain","text":"NOTHING TO ADD"}`},
		{name: "duplicate object field", artifacts: `[{"name":"session-log","name":"final-message","contentType":"text/plain","text":"NOTHING TO ADD"}]`},
		{name: "oversized answer", artifacts: resultArtifact(strings.Repeat("x", store.MaxCouncilText+1))},
		{name: "oversized after redaction", artifacts: resultArtifact("password=fixture\n" + strings.Repeat("x", store.MaxCouncilText-17))},
		{name: "oversized envelope", artifacts: strings.Repeat(" ", store.MaxConclaveTurnInput) + resultArtifact(CouncilNothingToAdd)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st, councils, room := resultCouncil(t, "reviewer")
			task := reserveResult(t, st, room.ID, "reviewer", 1, 1)
			state, facts := tc.state, tc.exit
			if state == "" {
				state = domain.TaskStateCompleted
			}
			if facts == "" {
				facts = success
			}
			finishResult(t, st, task, state, facts, tc.artifacts)
			for range 2 {
				b := NewBroker(Options{Store: st})
				recorded, err := b.RecordCouncilAnswer(t.Context(), room.ID, task.ID)
				if err != nil || recorded != (tc.want != "") {
					t.Fatalf("recorded %v, err %v", recorded, err)
				}
			}
			msgs, err := st.ListConclaveMessages(t.Context(), room.ID, 0)
			if err != nil {
				t.Fatal(err)
			}
			snap, err := councils.CouncilSnapshot(t.Context(), room.ID)
			if err != nil || len(snap.Turns) != 1 {
				t.Fatalf("read turn: %v", err)
			}
			turn := snap.Turns[0]
			if tc.want == "" {
				if len(msgs) != 0 || turn.Answer != nil || turn.NothingToAdd {
					t.Fatal("refused output was posted or counted")
				}
			} else if len(msgs) != 1 || turn.Answer == nil || turn.Answer.ID != msgs[0].ID ||
				turn.Answer.Body != tc.want || turn.Answer.FromRole != "reviewer" || turn.Answer.TaskID != task.ID ||
				turn.Round != 1 || turn.NothingToAdd != tc.vote {
				t.Fatalf("messages %d, turn %+v", len(msgs), turn)
			}
		})
	}
}

type resultLinkFailure struct {
	store.Store
	store.CouncilStore
}

var errResultLink = errors.New("injected link failure")

func (s resultLinkFailure) AppendConclaveMessage(context.Context, domain.ConclaveMessage) (int64, error) {
	return 0, errResultLink
}

func TestCouncilResultRecovery(t *testing.T) {
	for _, tc := range []struct {
		name         string
		workloadPost bool
	}{
		{name: "retry after append"},
		{name: "ignore pre-completion lookalike", workloadPost: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st, councils, room := resultCouncil(t, "reviewer")
			task := reserveResult(t, st, room.ID, "reviewer", 1, 1)
			var oldID int64
			if tc.workloadPost {
				var err error
				oldID, err = st.AppendConclaveMessage(t.Context(), domain.ConclaveMessage{
					ConclaveID: room.ID, FromRole: "reviewer", ToRole: "all", Kind: "review",
					Body: CouncilNothingToAdd, TaskID: task.ID, CreatedAt: room.CreatedAt,
				})
				if err != nil {
					t.Fatal(err)
				}
			}
			finishResult(t, st, task, domain.TaskStateCompleted, `{"exit_code":0}`, resultArtifact(CouncilNothingToAdd))
			first := NewBroker(Options{Store: resultLinkFailure{Store: st, CouncilStore: councils}})
			if recorded, err := first.RecordCouncilAnswer(t.Context(), room.ID, task.ID); recorded || !errors.Is(err, errResultLink) {
				t.Fatalf("first call recorded %v, err %v", recorded, err)
			}
			snap, err := councils.CouncilSnapshot(t.Context(), room.ID)
			if err != nil || len(snap.Turns) != 1 || snap.Turns[0].Answer != nil || snap.Turns[0].NothingToAdd {
				t.Fatalf("unlinked answer counted: %v", err)
			}
			if msgs, err := st.ListConclaveMessages(t.Context(), room.ID, oldID); err != nil || len(msgs) != 0 {
				t.Fatalf("failed acceptance left %d new posts: %v", len(msgs), err)
			}
			reopened := NewBroker(Options{Store: st})
			if recorded, err := reopened.RecordCouncilAnswer(t.Context(), room.ID, task.ID); !recorded || err != nil {
				t.Fatalf("retry recorded %v, err %v", recorded, err)
			}
			msgs, err := st.ListConclaveMessages(t.Context(), room.ID, oldID)
			snap, snapErr := councils.CouncilSnapshot(t.Context(), room.ID)
			if err != nil || snapErr != nil || len(msgs) != 1 || len(snap.Turns) != 1 || snap.Turns[0].Answer == nil ||
				snap.Turns[0].Answer.ID != msgs[0].ID || !snap.Turns[0].NothingToAdd {
				t.Fatalf("retry did not link exactly one new post: %v %v", err, snapErr)
			}
		})
	}
}

func TestCouncilResultDigest(t *testing.T) {
	for _, tc := range []struct {
		name  string
		order []int
	}{
		{name: "declared completion order", order: []int{0, 1}},
		{name: "reverse completion order", order: []int{1, 0}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st, councils, room := resultCouncil(t, "reviewer", "author")
			b := NewBroker(Options{Store: st})
			ctx := t.Context()
			tasks := []domain.Task{
				reserveResult(t, st, room.ID, "reviewer", 1, 1),
				reserveResult(t, st, room.ID, "author", 1, 1),
			}
			if err := councils.PutCouncilRelic(ctx, tasks[0].ID, domain.ConclaveRelic{
				ConclaveID: room.ID, AuthorRole: "reviewer", Name: "patch", Body: "evidence body",
			}); err != nil {
				t.Fatal(err)
			}
			answers := []string{"Ship the patch. See patch.", "Do not ship. The change drops a check."}
			for _, i := range tc.order {
				finishResult(t, st, tasks[i], domain.TaskStateCompleted, `{"exit_code":0}`, resultArtifact(answers[i]))
				if _, err := b.RecordCouncilAnswer(ctx, room.ID, tasks[i].ID); err != nil {
					t.Fatal(err)
				}
			}
			task := reserveResult(t, st, room.ID, "reviewer", 2, 1)
			finishResult(t, st, task, domain.TaskStateCompleted, `{"exit_code":0}`, resultArtifact("I still support shipping."))
			if _, err := b.RecordCouncilAnswer(ctx, room.ID, task.ID); err != nil {
				t.Fatal(err)
			}
			preview, err := b.PrepareCouncilDigest(ctx, room.ID, "owner_stop")
			if err != nil {
				t.Fatal(err)
			}
			got, err := b.CloseCouncil(ctx, room.ID, "owner_stop")
			if err != nil {
				t.Fatal(err)
			}
			want := fmt.Sprintf(`Stop reason "owner_stop"
Round 1
  "reviewer"
    Answer
      Ship the patch. See patch.
    Evidence "patch" (sha256 %x)
  "author"
    Answer
      Do not ship. The change drops a check.
Round 2
  "reviewer"
    Answer
      I still support shipping.
  "author"
    Missing answer
`, sha256.Sum256([]byte("evidence body")))
			saved, readErr := st.GetConclaveRelic(ctx, room.ID, CouncilDigestRelicName)
			if readErr != nil || saved != got {
				t.Fatalf("closing digest was not stored: %v", readErr)
			}
			if preview.Body != want || got.Body != want || got.Name != "digest" || got.AuthorRole != "system" || got.ConclaveID != room.ID ||
				got.ContentType != "text/plain" || got.SHA256 != fmt.Sprintf("%x", sha256.Sum256([]byte(want))) {
				t.Fatalf("unexpected digest: %+v", got)
			}
			again, err := b.CloseCouncil(ctx, room.ID, "owner_stop")
			if err != nil || again != got {
				t.Fatalf("digest changed on replay: %v", err)
			}
		})
	}
}

type resultBlockedRead struct {
	store.Store
	store.CouncilStore
	listed, resume chan struct{}
}

func (s resultBlockedRead) CouncilSnapshot(ctx context.Context, roomID string) (store.CouncilSnapshot, error) {
	snap, err := s.CouncilStore.CouncilSnapshot(ctx, roomID)
	close(s.listed)
	<-s.resume
	return snap, err
}

func TestCouncilResultConcurrentReplay(t *testing.T) {
	for _, vote := range []bool{false, true} {
		t.Run(fmt.Sprint(vote), func(t *testing.T) {
			st, councils, room := resultCouncil(t, "reviewer")
			task := reserveResult(t, st, room.ID, "reviewer", 1, 1)
			answer := "Ship the patch."
			if vote {
				answer = CouncilNothingToAdd
			}
			finishResult(t, st, task, domain.TaskStateCompleted, `{"exit_code":0}`, resultArtifact(answer))

			listed, resume := make(chan struct{}), make(chan struct{})
			first := NewBroker(Options{Store: resultBlockedRead{st, councils, listed, resume}})
			result := make(chan error, 1)
			go func() {
				recorded, err := first.RecordCouncilAnswer(t.Context(), room.ID, task.ID)
				if !recorded && err == nil {
					err = errors.New("first call did not record an answer")
				}
				result <- err
			}()
			select {
			case <-listed:
			case err := <-result:
				t.Fatalf("first call returned before reading the snapshot: %v", err)
			}
			separate, _, otherRoom := resultCouncil(t, "reviewer")
			independent := reserveResult(t, separate, otherRoom.ID, "reviewer", 1, 1)
			finishResult(t, separate, independent, domain.TaskStateCompleted, `{"exit_code":0}`, resultArtifact(answer))
			if ok, err := NewBroker(Options{Store: separate}).RecordCouncilAnswer(t.Context(), otherRoom.ID, independent.ID); !ok || err != nil {
				t.Errorf("independent room: %v, %v", ok, err)
			}
			other := NewBroker(Options{Store: st})
			recorded, err := other.RecordCouncilAnswer(t.Context(), room.ID, task.ID)
			close(resume)
			if firstErr := <-result; firstErr != nil {
				t.Errorf("first call failed: %v", firstErr)
			}
			if !recorded || err != nil {
				t.Fatalf("overlapping call recorded %v, err %v", recorded, err)
			}
			if recorded, err := other.RecordCouncilAnswer(t.Context(), room.ID, task.ID); !recorded || err != nil {
				t.Fatalf("replay recorded %v, err %v", recorded, err)
			}
			msgs, err := st.ListConclaveMessages(t.Context(), room.ID, 0)
			if err != nil || len(msgs) != 1 {
				t.Fatalf("messages %d, error %v", len(msgs), err)
			}
		})
	}
}

func TestCouncilResultRecipient(t *testing.T) {
	for _, tc := range []struct {
		name   string
		routes []string
		peer   domain.WorkloadZone
		want   string
	}{
		{name: "broadcast", routes: []string{"all", "operator"}, peer: domain.ZoneSandbox, want: "all"},
		{name: "permitted peer precedes operator", routes: []string{"operator", "author"}, peer: domain.ZoneSandbox, want: "author"},
		{name: "permitted peer precedes sender", routes: []string{"reviewer", "author"}, peer: domain.ZoneSandbox, want: "author"},
		{name: "operator fallback", routes: []string{"operator"}, peer: domain.ZoneSandbox, want: "operator"},
		{name: "sender only", routes: []string{"reviewer"}, peer: domain.ZoneSandbox, want: "reviewer"},
		{name: "sandbox cannot send to runtime", routes: []string{"author"}, peer: domain.ZoneRuntime},
		{name: "no route", peer: domain.ZoneSandbox},
	} {
		t.Run(tc.name, func(t *testing.T) {
			members := []store.CouncilMember{
				{Vassal: domain.ConclaveVassal{Role: "reviewer", Workload: "worker", CanMessage: tc.routes}, Zone: domain.ZoneSandbox},
				{Vassal: domain.ConclaveVassal{Role: "author", Workload: "author-worker"}, Zone: tc.peer},
			}
			if got := councilAnswerRecipient(members, "reviewer", "worker"); got != tc.want {
				t.Fatalf("recipient %q, want %q", got, tc.want)
			}
		})
	}
}

func TestCouncilResultClose(t *testing.T) {
	for _, tc := range []struct {
		reason string
		state  domain.ConclaveState
	}{{"owner_stop", domain.ConclaveStateCancelled}, {"round_cap", domain.ConclaveStateCompleted}} {
		t.Run(tc.reason, func(t *testing.T) {
			st, _, room := resultCouncil(t, "one", "two", "three")
			b := NewBroker(Options{Store: st})
			for _, role := range []string{"one", "two", "three"} {
				task := reserveResult(t, st, room.ID, role, 1, 1)
				body := "begin" + strings.Repeat("\n", store.MaxCouncilText-8) + "end"
				finishResult(t, st, task, domain.TaskStateCompleted, `{"exit_code":0}`, resultArtifact(body))
				if ok, err := b.RecordCouncilAnswer(t.Context(), room.ID, task.ID); !ok || err != nil {
					t.Fatal(ok, err)
				}
			}
			d, err := b.PrepareCouncilDigest(t.Context(), room.ID, tc.reason)
			if err != nil || len(d.Body) > store.MaxConclaveTurnInput || strings.Count(d.Body, "[omitted]") != 3 || strings.Count(d.Body, "begin") != 3 {
				t.Fatalf("bounded digest: %d bytes, %v", len(d.Body), err)
			}
			d, err = b.CloseCouncil(t.Context(), room.ID, tc.reason)
			if err != nil {
				t.Fatal(err)
			}
			saved, err := st.GetConclaveRelic(t.Context(), room.ID, "digest")
			if err != nil || saved != d {
				t.Fatalf("stored digest differs: %v", err)
			}
			closed, err := st.GetConclave(t.Context(), room.ID)
			if err != nil || closed.State != tc.state || closed.WardReason != tc.reason {
				t.Fatalf("state %v: %v", closed, err)
			}
			replay, err := NewBroker(Options{Store: st}).CloseCouncil(t.Context(), room.ID, "deadline")
			if err != nil || replay != d {
				t.Fatalf("replay differs: %v", err)
			}
		})
	}
}

type resultSnapshot struct {
	store.Store
	store.CouncilStore
	snapshot store.CouncilSnapshot
}

func (s resultSnapshot) CouncilSnapshot(context.Context, string) (store.CouncilSnapshot, error) {
	return s.snapshot, nil
}

func TestCouncilDigestInvalidTurn(t *testing.T) {
	for _, turn := range []store.CouncilTurn{{Role: "unknown", Round: 1}, {Role: "author", Round: 1, NothingToAdd: true}} {
		t.Run(turn.Role, func(t *testing.T) {
			st, c, room := resultCouncil(t, "author")
			snap, err := c.CouncilSnapshot(t.Context(), room.ID)
			if err != nil {
				t.Fatal(err)
			}
			snap.Turns = []store.CouncilTurn{turn}
			b := NewBroker(Options{Store: resultSnapshot{st, c, snap}})
			if _, err := b.PrepareCouncilDigest(t.Context(), room.ID, "owner_stop"); err == nil {
				t.Fatal("accepted invalid turn")
			}
		})
	}
}

func TestCouncilAnswerVisibility(t *testing.T) {
	st := brokerTestStore(t)
	c, err := store.Councils(st)
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	room := domain.ConclaveSession{ID: "panel", State: domain.ConclaveStateActive, CreatedAt: time.Now().UTC()}
	spec := store.CouncilSpec{Question: "question"}
	roles := []string{"reviewer", "one", "two", "restricted"}
	for i, role := range roles {
		zone := domain.ZoneSandbox
		workload := "worker"
		if i == 3 {
			zone = domain.ZoneRuntime
			workload = "restricted-worker"
		}
		if err := st.UpsertWorkload(ctx, domain.WorkloadRecord{Name: workload, Zone: zone}); err != nil {
			t.Fatal(err)
		}
		spec.Members = append(spec.Members, store.CouncilMember{Vassal: domain.ConclaveVassal{Role: role, Workload: workload, CanMessage: []string{"one", "two"}}, Zone: zone})
	}
	if err := c.CreateCouncil(ctx, room, spec); err != nil {
		t.Fatal(err)
	}
	task := reserveResult(t, st, room.ID, "reviewer", 1, 1)
	finishResult(t, st, task, domain.TaskStateCompleted, `{"exit_code":0}`, resultArtifact("reviewer conclusion"))
	if ok, err := NewBroker(Options{Store: st}).RecordCouncilAnswer(ctx, room.ID, task.ID); !ok || err != nil {
		t.Fatal(ok, err)
	}
	snap, err := c.CouncilSnapshot(ctx, room.ID)
	if err != nil {
		t.Fatal(err)
	}
	panel := CouncilSnapshot{Question: snap.Question}
	panel.Members = append(panel.Members, snap.Members...)
	turns := turnsByRound{1: {"reviewer": snap.Turns[0]}}
	for _, m := range panel.Members {
		p, err := panel.packet(2, m, turns)
		want := m.Vassal.Role == "one" || m.Vassal.Role == "two"
		if err != nil || strings.Contains(p.Markdown, "reviewer conclusion") != want {
			t.Errorf("%s visibility, want %t: %v", m.Vassal.Role, want, err)
		}
	}
}

func TestCouncilOperatorClose(t *testing.T) {
	for _, state := range []domain.ConclaveState{domain.ConclaveStateCancelled, domain.ConclaveStateCompleted} {
		t.Run(string(state), func(t *testing.T) {
			st, _, room := resultCouncil(t, "author")
			b := NewBroker(Options{Store: st})
			if err := b.Close(t.Context(), room.ID, state); err != nil {
				t.Fatal(err)
			}
			closed, err := st.GetConclave(t.Context(), room.ID)
			if err != nil || closed.State != state || closed.WardReason == "" {
				t.Fatal("missing terminal reason", err)
			}
			d, err := st.GetConclaveRelic(t.Context(), room.ID, "digest")
			if err != nil || !strings.Contains(d.Body, closed.WardReason) || !strings.Contains(d.Body, "Missing answer") {
				t.Fatal("missing operator digest", err)
			}
		})
	}
}
