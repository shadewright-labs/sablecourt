package sqlite

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/shadewright-labs/sablecourt/internal/domain"
	"github.com/shadewright-labs/sablecourt/internal/store"
	"github.com/shadewright-labs/sablecourt/internal/store/postgres"
)

func councilFixture() (domain.ConclaveSession, store.CouncilSpec) {
	room := domain.ConclaveSession{ID: "panel", State: domain.ConclaveStateActive, CreatedAt: time.Now().UTC()}
	spec := store.CouncilSpec{Question: "What changed?"}
	for _, role := range []string{"reviewer", "author"} {
		spec.Members = append(spec.Members, store.CouncilMember{
			Vassal: domain.ConclaveVassal{Role: role, Workload: "worker", CanMessage: []string{"all"}},
			Zone:   domain.ZoneSandbox, RoleCard: "Read the change.",
		})
	}
	return room, spec
}

func councilDB(t *testing.T, path string) *SQLiteStore {
	t.Helper()
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if err := s.Migrate(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertWorkload(t.Context(), domain.WorkloadRecord{Name: "worker", Zone: domain.ZoneSandbox}); err != nil {
		t.Fatal(err)
	}
	return s
}

func councilTask(role string, round, attempt int) domain.Task {
	a := store.ConclaveAssignment{ConclaveID: "panel", Role: role, Round: round, Attempt: attempt}
	raw, err := json.Marshal(a)
	if err != nil {
		panic(err)
	}
	return domain.Task{ID: fmt.Sprintf("%s-%d-%d", role, round, attempt), WorkloadName: "worker", State: domain.TaskStateSubmitted, InputJSON: string(raw), IdempotencyKey: a.TurnKey()}
}

func reserve(t *testing.T, s *SQLiteStore, task domain.Task) error {
	t.Helper()
	limits := store.ConclaveLimits{Now: time.Now, MaxMessages: 8, MaxPingPong: 8, MaxRuns: 6}
	_, err := store.SubmitConclaveTurn(t.Context(), "panel", task.IdempotencyKey, limits, func(ctx context.Context) (domain.Task, error) {
		return task, s.CreateTask(ctx, task)
	})
	return err
}

func TestCouncilAttemptAtomicity(t *testing.T) {
	for _, fault := range []string{"task insert", "attempt insert", "two connections"} {
		t.Run(fault, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "state.db")
			s := councilDB(t, path)
			peer := councilDB(t, path)
			room, spec := councilFixture()
			if err := s.CreateCouncil(t.Context(), room, spec); err != nil {
				t.Fatal(err)
			}
			task := councilTask("reviewer", 1, 1)
			if fault == "two connections" {
				start := make(chan struct{})
				results := make(chan error, 2)
				for i, db := range []*SQLiteStore{s, peer} {
					candidate := task
					candidate.ID = fmt.Sprintf("candidate-%d", i)
					go func() { <-start; results <- reserve(t, db, candidate) }()
				}
				close(start)
				var successes, conflicts int
				for range 2 {
					err := <-results
					if err == nil {
						successes++
					} else if errors.Is(err, store.ErrConflict) {
						conflicts++
					} else {
						t.Fatal(err)
					}
				}
				if successes != 1 || conflicts != 1 {
					t.Fatalf("successes %d conflicts %d", successes, conflicts)
				}
			} else {
				table := "tasks"
				if fault == "attempt insert" {
					table = "council_attempts"
				}
				if _, err := s.db.Exec("CREATE TRIGGER inject BEFORE INSERT ON " + table + " BEGIN SELECT RAISE(ABORT,'injected'); END"); err != nil {
					t.Fatal(err)
				}
				if err := reserve(t, s, task); err == nil {
					t.Fatal("injected failure accepted")
				}
				snap, err := peer.CouncilSnapshot(t.Context(), room.ID)
				if err != nil || snap.Turns == nil || len(snap.Attempts) != 0 {
					t.Fatalf("after failure: %+v %v", snap, err)
				}
				tasks, err := peer.ListTasks(t.Context())
				if err != nil || len(tasks) != 0 {
					t.Fatalf("orphan tasks %v %v", tasks, err)
				}
				if _, err := s.db.Exec("DROP TRIGGER inject"); err != nil {
					t.Fatal(err)
				}
				if err := reserve(t, peer, task); err != nil {
					t.Fatal(err)
				}
			}
			tasks, err := s.ListTasks(t.Context())
			if err != nil || len(tasks) != 1 {
				t.Fatalf("tasks %v %v", tasks, err)
			}
			if err := s.UpdateTaskState(t.Context(), tasks[0].ID, domain.TaskStateSubmitted, domain.TaskStateFailed, "test", "{}"); err != nil {
				t.Fatal(err)
			}
			if err := reserve(t, s, councilTask("reviewer", 1, 2)); err != nil {
				t.Fatal(err)
			}
			snap, err := peer.CouncilSnapshot(t.Context(), room.ID)
			if err != nil || len(snap.Turns) != 1 || snap.Turns[0].Starts != 2 || len(snap.Attempts) != 2 || snap.Attempts[0].State != domain.TaskStateFailed {
				t.Fatalf("retry snapshot %+v %v", snap, err)
			}
		})
	}
}

func TestCouncilReservationGuards(t *testing.T) {
	for _, tc := range []string{"closed", "deadline after decree", "ward", "message ceiling", "budget", "starts after decree", "round barrier", "sequential barrier", "live retry", "skipped attempt", "unknown role", "workload", "bare", "ordinary guard", "ordinary bare", "round ceiling", "active ward"} {
		t.Run(tc, func(t *testing.T) {
			ctx := t.Context()
			s := councilDB(t, filepath.Join(t.TempDir(), "state.db"))
			room, spec := councilFixture()
			spec.Sequential = tc == "sequential barrier"
			if tc == "deadline after decree" {
				room.CreatedAt = room.CreatedAt.Add(-time.Hour)
			}
			if tc == "message ceiling" {
				spec.Wards.MaxMessages = 1
			}
			if tc == "budget" {
				spec.Budget = &domain.PerTaskBudget{MaxInputTokens: 1}
			}
			cap := 1
			if tc == "starts after decree" {
				spec.MaxStarts = &cap
			}
			if err := s.CreateCouncil(ctx, room, spec); err != nil {
				t.Fatal(err)
			}
			task := councilTask("reviewer", 1, 1)
			wantStarts := 0
			switch tc {
			case "closed", "ward", "active ward":
				state := domain.ConclaveStateCompleted
				if tc == "ward" {
					state = domain.ConclaveStateInputRequired
				}
				if tc == "active ward" {
					state = domain.ConclaveStateActive
				}
				if err := s.UpdateConclaveState(ctx, room.ID, state, "stop"); err != nil {
					t.Fatal(err)
				}
			case "message ceiling":
				if _, err := s.AppendConclaveMessage(ctx, domain.ConclaveMessage{ConclaveID: room.ID, FromRole: "reviewer", Body: "one"}); err != nil {
					t.Fatal(err)
				}
			case "budget", "live retry", "starts after decree":
				if err := reserve(t, s, task); err != nil {
					t.Fatal(err)
				}
				wantStarts = 1
				switch tc {
				case "budget":
					if err := s.RecordBudgetUsage(ctx, domain.BudgetEntry{TaskID: task.ID, WorkloadName: "worker", InputTokens: 1}); err != nil {
						t.Fatal(err)
					}
					task = councilTask("author", 1, 1)
				case "live retry":
					task = councilTask("reviewer", 1, 2)
				default:
					if err := s.UpdateConclaveState(ctx, room.ID, domain.ConclaveStateInputRequired, store.ConclaveMaxRunsWard); err != nil {
						t.Fatal(err)
					}
					task = councilTask("author", 1, 1)
				}
			case "round barrier":
				task = councilTask("reviewer", 2, 1)
			case "round ceiling":
				task = councilTask("reviewer", 3, 1)
			case "sequential barrier":
				task = councilTask("author", 1, 1)
			case "skipped attempt":
				task = councilTask("reviewer", 1, 2)
			case "unknown role":
				task = councilTask("outsider", 1, 1)
			case "workload":
				if err := s.UpsertWorkload(ctx, domain.WorkloadRecord{Name: "other"}); err != nil {
					t.Fatal(err)
				}
				task.WorkloadName = "other"
			case "ordinary guard", "ordinary bare":
				a := store.ConclaveAssignment{ConclaveID: room.ID, Role: "reviewer", Activation: domain.ActivationAlwaysOn}
				raw, _ := json.Marshal(a)
				task.InputJSON = string(raw)
				task.IdempotencyKey = a.TurnKey()
			}
			if strings.HasSuffix(tc, "after decree") {
				_, _, _, err := store.AppendConclavePost(ctx, s, domain.ConclaveMessage{ConclaveID: room.ID, FromRole: "operator", ToRole: "all", Kind: "decree", Body: "continue", CreatedAt: time.Now()}, store.ConclaveLimits{Now: time.Now, MaxRuns: 6}, true, false)
				if err != nil && !(tc == "deadline after decree" && errors.Is(err, store.ErrWardTripped)) {
					t.Fatal(err)
				}
			}
			var err error
			if tc == "bare" || tc == "ordinary bare" {
				err = s.CreateTask(ctx, task)
			} else {
				err = reserve(t, s, task)
			}
			if err == nil {
				t.Fatal("stale or unauthorized reservation succeeded")
			}
			snap, err := s.CouncilSnapshot(ctx, room.ID)
			if err != nil || len(snap.Attempts) != wantStarts || !snap.Deadline.Equal(room.CreatedAt.Add(15*time.Minute)) {
				t.Fatalf("guard changed facts: %+v %v", snap, err)
			}
		})
	}
}

func TestCouncilPolicy(t *testing.T) {
	for _, tc := range []string{"defaults", "empty question", "oversized question", "missing seats", "duplicate role", "unknown route", "zone crossing", "missing zone", "invalid role card UTF-8", "zero rounds", "negative starts", "starts above run ceiling", "zero duration", "unpinned ref", "unknown writer", "bad pin"} {
		t.Run(tc, func(t *testing.T) {
			s := openTestStore(t)
			room, spec := councilFixture()
			zero, neg, d := 0, -1, time.Duration(0)
			switch tc {
			case "empty question":
				spec.Question = " "
			case "oversized question":
				spec.Question = strings.Repeat("x", 1<<20)
			case "missing seats":
				spec.Members = nil
			case "duplicate role":
				spec.Members[1] = spec.Members[0]
			case "unknown route":
				spec.Members[0].Vassal.CanMessage = []string{"unknown"}
			case "zone crossing":
				spec.Members[1].Zone = domain.ZoneRuntime
			case "missing zone":
				spec.Members[1].Zone = ""
			case "invalid role card UTF-8":
				spec.Members[0].RoleCard = "Read the change.\xff"
			case "zero rounds":
				spec.MaxRounds = &zero
			case "negative starts":
				spec.MaxStarts = &neg
			case "starts above run ceiling":
				rounds, starts := 3, domain.ConclaveRunCeiling+1
				spec.MaxRounds, spec.MaxStarts = &rounds, &starts
			case "zero duration":
				spec.Duration = &d
			case "unpinned ref":
				spec.Checkout.Ref = "main"
			case "unknown writer":
				spec.Checkout.WriterRole = "other"
				spec.Checkout.Commit = strings.Repeat("a", 40)
			case "bad pin":
				spec.Checkout.Commit = "HEAD"
			}
			err := s.CreateCouncil(t.Context(), room, spec)
			if tc != "defaults" {
				if err == nil {
					t.Fatal("invalid policy accepted")
				}
				if _, err := s.GetConclave(t.Context(), room.ID); !errors.Is(err, store.ErrNotFound) {
					t.Fatalf("partial room: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			snap, err := s.CouncilSnapshot(t.Context(), room.ID)
			if err != nil || snap.MaxStarts != 6 || snap.MaxRounds != 2 || snap.Sequential || snap.Turns == nil || !snap.Deadline.Equal(room.CreatedAt.Add(15*time.Minute)) {
				t.Fatalf("defaults: %+v %v", snap, err)
			}
		})
	}
}

func TestCouncilSilentRetry(t *testing.T) {
	for _, tc := range []struct {
		name     string
		answered bool
	}{
		{name: "silent"},
		{name: "answered", answered: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			s := councilDB(t, filepath.Join(t.TempDir(), "state.db"))
			room, spec := councilFixture()
			if err := s.CreateCouncil(ctx, room, spec); err != nil {
				t.Fatal(err)
			}
			first := councilTask("reviewer", 1, 1)
			if err := reserve(t, s, first); err != nil {
				t.Fatal(err)
			}
			msg := domain.ConclaveMessage{ConclaveID: room.ID, TaskID: first.ID, FromRole: "reviewer", ToRole: "all", Kind: "review", Body: "first answer"}
			id, err := s.AppendConclaveMessage(ctx, msg)
			if err != nil {
				t.Fatal(err)
			}
			if err := s.UpdateTaskState(ctx, first.ID, first.State, domain.TaskStateCompleted, "done", "{}"); err != nil {
				t.Fatal(err)
			}
			if tc.answered {
				if err := s.AcceptCouncilAnswer(ctx, first.ID, id, false); err != nil {
					t.Fatal(err)
				}
			}

			retry := councilTask("reviewer", 1, 2)
			err = reserve(t, s, retry)
			if tc.answered {
				if err == nil {
					t.Fatal("retried an answered turn")
				}
				return
			}
			if err != nil {
				t.Fatalf("retry after silent completion: %v", err)
			}
			if err := s.AcceptCouncilAnswer(ctx, first.ID, id, true); err == nil {
				t.Fatal("accepted a superseded answer")
			}
			msg.TaskID, msg.Body = retry.ID, "retry answer"
			id, err = s.AppendConclaveMessage(ctx, msg)
			if err != nil {
				t.Fatal(err)
			}
			if err := s.UpdateTaskState(ctx, retry.ID, retry.State, domain.TaskStateCompleted, "done", "{}"); err != nil {
				t.Fatal(err)
			}
			if err := s.AcceptCouncilAnswer(ctx, retry.ID, id, false); err != nil {
				t.Fatal(err)
			}
			snap, err := s.CouncilSnapshot(ctx, room.ID)
			if err != nil || len(snap.Attempts) != 2 || len(snap.Turns) != 1 ||
				snap.Attempts[0].State != domain.TaskStateCompleted || snap.Attempts[0].Answer != nil || snap.Attempts[0].NothingToAdd ||
				snap.Turns[0].Starts != 2 || snap.Turns[0].Answer == nil || snap.Turns[0].Answer.ID != id {
				t.Fatalf("retry snapshot: %+v, %v", snap, err)
			}
		})
	}
}

func TestCouncilDefaultWards(t *testing.T) {
	ctx := t.Context()
	s := councilDB(t, filepath.Join(t.TempDir(), "state.db"))
	room, spec := councilFixture()
	third := spec.Members[0]
	third.Vassal.Role = "observer"
	spec.Members = append(spec.Members, third)
	if err := s.CreateCouncil(ctx, room, spec); err != nil {
		t.Fatal(err)
	}
	for round := 1; round <= 2; round++ {
		for _, member := range spec.Members {
			role := member.Vassal.Role
			task := councilTask(role, round, 1)
			if err := reserve(t, s, task); err != nil {
				t.Fatal(err)
			}
			msg := domain.ConclaveMessage{ConclaveID: room.ID, TaskID: task.ID, FromRole: role, ToRole: "all", Kind: "review", Body: "answer"}
			id, reason, _, err := store.AppendConclavePost(ctx, s, msg, store.ConclaveLimits{}, false, false)
			if err != nil {
				t.Fatal(err)
			}
			if err := s.UpdateTaskState(ctx, task.ID, task.State, domain.TaskStateCompleted, "done", "{}"); err != nil {
				t.Fatal(err)
			}
			if err := s.AcceptCouncilAnswer(ctx, task.ID, id, false); err != nil {
				t.Fatal(err)
			}
			if reason != "" {
				t.Fatalf("round %d role %s tripped Ward %q", round, role, reason)
			}
		}
	}
	snap, err := s.CouncilSnapshot(ctx, room.ID)
	if err != nil || len(snap.Turns) != 6 || snap.Session.State != domain.ConclaveStateActive ||
		snap.Wards.MaxMessages <= snap.MaxStarts || snap.Wards.MaxPingPong <= snap.MaxStarts {
		t.Fatalf("default council after six answers: %+v, %v", snap, err)
	}
}

func TestCouncilInvalidCallerBudget(t *testing.T) {
	for _, tc := range []struct {
		name   string
		pinned domain.PerTaskBudget
		caller domain.PerTaskBudget
	}{
		{"negative cost", domain.PerTaskBudget{MaxCostUSD: 1}, domain.PerTaskBudget{MaxCostUSD: -1}},
		{"NaN cost", domain.PerTaskBudget{MaxCostUSD: 1}, domain.PerTaskBudget{MaxCostUSD: math.NaN()}},
		{"negative input", domain.PerTaskBudget{MaxInputTokens: 1}, domain.PerTaskBudget{MaxInputTokens: -1}},
		{"negative output", domain.PerTaskBudget{MaxOutputTokens: 1}, domain.PerTaskBudget{MaxOutputTokens: -1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			s := councilDB(t, filepath.Join(t.TempDir(), "state.db"))
			room, spec := councilFixture()
			spec.Budget = &tc.pinned
			if err := s.CreateCouncil(ctx, room, spec); err != nil {
				t.Fatal(err)
			}
			first := councilTask("reviewer", 1, 1)
			if err := reserve(t, s, first); err != nil {
				t.Fatal(err)
			}
			usage := domain.BudgetEntry{TaskID: first.ID, WorkloadName: "worker", InputTokens: 1, OutputTokens: 1, CostMicroUSD: 1_000_000}
			if err := s.RecordBudgetUsage(ctx, usage); err != nil {
				t.Fatal(err)
			}
			next := councilTask("author", 1, 1)
			limits := store.ConclaveLimits{Budget: &tc.caller}
			_, err := store.SubmitConclaveTurn(ctx, room.ID, next.IdempotencyKey, limits, func(ctx context.Context) (domain.Task, error) {
				return next, s.CreateTask(ctx, next)
			})
			if !errors.Is(err, store.ErrConclavePaused) {
				t.Fatalf("expected invalid budget refusal, got %v", err)
			}
			snap, err := s.CouncilSnapshot(ctx, room.ID)
			if err != nil || len(snap.Attempts) != 1 || snap.Session.State != domain.ConclaveStateInputRequired || snap.Session.WardReason == "" {
				t.Fatalf("invalid budget snapshot: %+v, %v", snap, err)
			}
		})
	}
}

func TestCouncilOperatorDecrees(t *testing.T) {
	for _, tc := range []struct {
		name, kind, recipient, body string
		valid                       bool
	}{
		{"broadcast", "decree", "all", "continue", true},
		{"seat at size limit", "decree", "reviewer", strings.Repeat("x", 64<<10), true},
		{"unknown recipient", "decree", "outsider", "continue", false},
		{"empty recipient", "decree", "", "continue", false},
		{"blank text", "decree", "all", " \t\n", false},
		{"oversized text", "decree", "all", strings.Repeat("x", (64<<10)+1), false},
		{"invalid UTF-8", "decree", "all", "continue\xff", false},
		{"invalid decision", "decision", "outsider", "continue", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			s := openTestStore(t)
			room, spec := councilFixture()
			if err := s.CreateCouncil(ctx, room, spec); err != nil {
				t.Fatal(err)
			}
			if err := s.UpdateConclaveState(ctx, room.ID, domain.ConclaveStateInputRequired, "test_pause"); err != nil {
				t.Fatal(err)
			}
			msg := domain.ConclaveMessage{ConclaveID: room.ID, FromRole: "operator", Kind: tc.kind, ToRole: tc.recipient, Body: tc.body}
			id, _, _, err := store.AppendConclavePost(ctx, s, msg, store.ConclaveLimits{}, true, false)
			if (err == nil) != tc.valid {
				t.Fatalf("valid %v, got id %d and error %v", tc.valid, id, err)
			}
			snap, err := s.CouncilSnapshot(ctx, room.ID)
			if err != nil {
				t.Fatal(err)
			}
			if tc.valid {
				if id == 0 || snap.Session.State != domain.ConclaveStateActive || len(snap.Decrees) != 1 || snap.Decrees[0].ID != id {
					t.Fatalf("decree missing or room still paused: state %s, decrees %d", snap.Session.State, len(snap.Decrees))
				}
			} else if id != 0 || snap.Session.State != domain.ConclaveStateInputRequired || snap.Session.WardReason != "test_pause" || len(snap.Decrees) != 0 {
				t.Fatalf("invalid decree changed room: state %s, reason %q, decrees %d", snap.Session.State, snap.Session.WardReason, len(snap.Decrees))
			}
		})
	}
}

func TestCouncilRecovery(t *testing.T) {
	for _, vote := range []bool{false, true} {
		t.Run(fmt.Sprint(vote), func(t *testing.T) {
			ctx := t.Context()
			path := filepath.Join(t.TempDir(), "state.db")
			s := councilDB(t, path)
			room, spec := councilFixture()
			spec.Sequential = true
			maxRounds, maxStarts, duration := 3, 5, 9*time.Minute
			spec.MaxRounds, spec.MaxStarts, spec.Duration = &maxRounds, &maxStarts, &duration
			spec.Checkout = store.CouncilCheckout{Source: "project", Ref: "main", Commit: strings.Repeat("a", 40), WriterRole: "reviewer"}
			if err := s.CreateCouncil(ctx, room, spec); err != nil {
				t.Fatal(err)
			}
			task := councilTask("reviewer", 1, 1)
			if err := reserve(t, s, task); err != nil {
				t.Fatal(err)
			}
			relic := domain.ConclaveRelic{ConclaveID: room.ID, AuthorRole: "reviewer", Name: "patch", Body: "difference", ContentType: "text/plain"}
			if err := s.PutCouncilRelic(ctx, task.ID, relic); err != nil {
				t.Fatal(err)
			}
			if err := s.PutCouncilRelic(ctx, task.ID, relic); err != nil {
				t.Fatalf("replay relic: %v", err)
			}
			relic.Body = "replacement"
			if err := s.PutConclaveRelic(ctx, relic); err == nil {
				t.Fatal("replaced attributed relic")
			}
			msg := domain.ConclaveMessage{ConclaveID: room.ID, FromRole: "reviewer", ToRole: "all", Kind: "review", Body: "answer", TaskID: task.ID}
			id, err := s.AppendConclaveMessage(ctx, msg)
			if err != nil {
				t.Fatal(err)
			}
			if err := s.UpdateTaskState(ctx, task.ID, domain.TaskStateSubmitted, domain.TaskStateCompleted, "done", "{}"); err != nil {
				t.Fatal(err)
			}
			if err := s.AcceptCouncilAnswer(ctx, task.ID, id, vote); err != nil {
				t.Fatal(err)
			}
			if err := s.AcceptCouncilAnswer(ctx, task.ID, id, vote); err != nil {
				t.Fatalf("replay: %v", err)
			}
			if err := s.AcceptCouncilAnswer(ctx, task.ID, id, !vote); err == nil {
				t.Fatal("vote overwritten")
			}
			if err := reserve(t, s, councilTask("author", 1, 1)); err != nil {
				t.Fatal(err)
			}
			_, err = s.AppendConclaveMessage(ctx, domain.ConclaveMessage{ConclaveID: room.ID, FromRole: "operator", Kind: "decree", ToRole: "all", Body: "keep detail"})
			if err != nil {
				t.Fatal(err)
			}
			if err := s.UpdateConclaveState(ctx, room.ID, domain.ConclaveStateCancelled, "owner_stop"); err != nil {
				t.Fatal(err)
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			peer := councilDB(t, path)
			snap, err := peer.CouncilSnapshot(ctx, room.ID)
			if err != nil {
				t.Fatal(err)
			}
			if snap.Question != spec.Question || !reflect.DeepEqual(snap.Members, spec.Members) || snap.Checkout != spec.Checkout || !snap.Sequential || snap.MaxRounds != 3 || snap.MaxStarts != 5 || !snap.Deadline.Equal(room.CreatedAt.Add(duration)) || snap.Session.WardReason != "owner_stop" || len(snap.Decrees) != 1 || len(snap.Turns) != 2 || len(snap.Attempts) != 2 {
				t.Fatalf("recovered %+v", snap)
			}
			first := snap.Turns[0]
			if first.Role != "reviewer" || first.Answer == nil || first.Answer.ID != id || first.Answer.TaskID != task.ID || first.Answer.Body != "answer" || first.NothingToAdd != vote || first.State != domain.TaskStateCompleted || len(snap.Attempts[0].Relics) != 1 || snap.Attempts[0].Relics[0].Body != "difference" {
				t.Fatalf("turn %+v attempts %+v", first, snap.Attempts)
			}
		})
	}
}

func TestCouncilBackend(t *testing.T) {
	for _, tc := range []struct {
		name    string
		s       store.Store
		allowed bool
	}{{"sqlite", openTestStore(t), true}, {"postgres", &postgres.PostgresStore{}, false}} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := store.Councils(tc.s)
			if (err == nil) != tc.allowed {
				t.Fatalf("capability: %v", err)
			}
		})
	}
}

func TestCouncilEvidenceRefusals(t *testing.T) {
	for _, tc := range []string{"no message", "wrong task", "wrong author", "wrong kind", "empty", "oversized", "unrouted", "unfinished", "failed", "unattributed relic", "wrong relic author", "unknown relic task", "foreign relic room", "changed relic", "late relic"} {
		t.Run(tc, func(t *testing.T) {
			ctx := t.Context()
			s := councilDB(t, filepath.Join(t.TempDir(), "state.db"))
			room, spec := councilFixture()
			spec.Members[0].Vassal.CanMessage = []string{"operator"}
			if err := s.CreateCouncil(ctx, room, spec); err != nil {
				t.Fatal(err)
			}
			task := councilTask("reviewer", 1, 1)
			if err := reserve(t, s, task); err != nil {
				t.Fatal(err)
			}
			if strings.Contains(tc, "relic") {
				r := domain.ConclaveRelic{ConclaveID: room.ID, Name: "finding", AuthorRole: "reviewer", Body: "evidence", ContentType: "text/plain"}
				id := task.ID
				switch tc {
				case "wrong relic author":
					r.AuthorRole = "author"
				case "unknown relic task":
					id = "missing"
				case "foreign relic room":
					r.ConclaveID = "elsewhere"
				case "late relic":
					if err := s.UpdateTaskState(ctx, id, domain.TaskStateSubmitted, domain.TaskStateFailed, "done", "{}"); err != nil {
						t.Fatal(err)
					}
				case "changed relic":
					if err := s.PutCouncilRelic(ctx, id, r); err != nil {
						t.Fatal(err)
					}
					r.Body = "changed"
				}
				var err error
				if tc == "unattributed relic" {
					err = s.PutConclaveRelic(ctx, r)
				} else {
					err = s.PutCouncilRelic(ctx, id, r)
				}
				if err == nil {
					t.Fatal("accepted relic")
				}
				return
			}
			msg := domain.ConclaveMessage{ConclaveID: room.ID, TaskID: task.ID, FromRole: "reviewer", ToRole: "operator", Kind: "review", Body: "evidence"}
			switch tc {
			case "wrong task":
				msg.TaskID = "other"
			case "wrong author":
				msg.FromRole = "author"
			case "wrong kind":
				msg.Kind = "decision"
			case "empty":
				msg.Body = " "
			case "oversized":
				msg.Body = strings.Repeat("x", (64<<10)+1)
			case "unrouted":
				msg.ToRole = "all"
			}
			id, err := s.AppendConclaveMessage(ctx, msg)
			if err != nil {
				t.Fatal(err)
			}
			if tc == "no message" {
				id++
			}
			if tc != "unfinished" {
				state := domain.TaskStateCompleted
				if tc == "failed" {
					state = domain.TaskStateFailed
				}
				if err := s.UpdateTaskState(ctx, task.ID, domain.TaskStateSubmitted, state, "done", "{}"); err != nil {
					t.Fatal(err)
				}
			}
			if err := s.AcceptCouncilAnswer(ctx, task.ID, id, true); err == nil {
				t.Fatal("accepted invalid answer")
			}
			snap, err := s.CouncilSnapshot(ctx, room.ID)
			if err != nil || snap.Turns[0].Answer != nil || snap.Turns[0].NothingToAdd {
				t.Fatalf("invalid evidence became visible: %+v %v", snap, err)
			}
		})
	}
}

func TestCouncilAssignmentInput(t *testing.T) {
	for _, tc := range []struct {
		raw string
		ok  bool
	}{
		{`{"conclaveId":"panel","role":"reviewer","round":1,"attempt":1}`, true},
		{`{"conclaveId":"panel","role":"reviewer","round":"1","attempt":1}`, false},
		{`{"conclaveId":"panel","role":"reviewer","round":1,"attempt":null}`, false},
		{`{"conclaveId":"panel","role":"reviewer","round":1,"attempt":1,"messageId":2}`, false},
		{`{"conclaveId":"panel","role":"reviewer","round":1,"attempt":1,"retry_count":1}`, false},
		{"", false}, {"null", false}, {"[]", false}, {strings.Repeat(" ", store.MaxConclaveTurnInput) + `{}`, false},
	} {
		task := councilTask("reviewer", 1, 1)
		task.InputJSON = tc.raw
		if _, ok := store.TaskConclaveAssignment(task); ok != tc.ok {
			t.Fatalf("input of length %d: accepted %v", len(tc.raw), ok)
		}
	}
}

func TestCouncilLegacyInsertPaths(t *testing.T) {
	for _, tc := range []string{"case folded", "duplicate field", "memory registration", "ordinary with its own round"} {
		t.Run(tc, func(t *testing.T) {
			s := councilDB(t, filepath.Join(t.TempDir(), "state.db"))
			ctx := t.Context()
			room, spec := councilFixture()
			a := store.ConclaveAssignment{ConclaveID: room.ID, Role: "reviewer", Activation: domain.ActivationAlwaysOn}
			task := domain.Task{ID: "legacy", WorkloadName: "worker", State: domain.TaskStateSubmitted, IdempotencyKey: a.TurnKey()}
			if tc == "ordinary with its own round" {
				task.InputJSON = `{"conclaveId":"panel","role":"reviewer","activation":"always-on","round":"custom"}`
				if err := s.CreateConclave(ctx, room); err != nil {
					t.Fatal(err)
				}
				if err := reserve(t, s, task); err != nil {
					t.Fatalf("ordinary submission: %v", err)
				}
				return
			}
			if err := s.CreateCouncil(ctx, room, spec); err != nil {
				t.Fatal(err)
			}
			task.InputJSON = `{"ConclaveID":"panel","role":"reviewer","activation":"always-on"}`
			if tc == "duplicate field" {
				task.InputJSON = `{"conclaveId":"other","conclaveId":"panel","role":"reviewer","activation":"always-on"}`
			}
			var err error
			if tc == "memory registration" {
				err = store.CreateTaskWithMemory(ctx, s, task, domain.MemoryEntry{Namespace: "tasks", Key: task.ID, Value: "entry"})
			} else {
				err = s.CreateTask(ctx, task)
			}
			if err == nil {
				t.Fatal("inserted council task outside reservation")
			}
			if _, err := s.GetTask(ctx, task.ID); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("task survived: %v", err)
			}
		})
	}
}

func TestCouncilReservationReplay(t *testing.T) {
	for _, tc := range []string{"closed", "ward", "start cap"} {
		t.Run(tc, func(t *testing.T) {
			s := councilDB(t, filepath.Join(t.TempDir(), "state.db"))
			room, spec := councilFixture()
			if tc == "start cap" {
				n := 1
				spec.MaxStarts = &n
			}
			if err := s.CreateCouncil(t.Context(), room, spec); err != nil {
				t.Fatal(err)
			}
			task := councilTask("reviewer", 1, 1)
			if err := reserve(t, s, task); err != nil {
				t.Fatal(err)
			}
			want := store.ErrConclaveRunLimit
			if tc != "start cap" {
				state := domain.ConclaveStateCancelled
				want = store.ErrConclaveClosed
				if tc == "ward" {
					state = domain.ConclaveStateInputRequired
					want = store.ErrConclavePaused
				}
				if err := s.UpdateConclaveState(t.Context(), room.ID, state, "owner"); err != nil {
					t.Fatal(err)
				}
			}
			if err := reserve(t, s, task); !errors.Is(err, want) {
				t.Fatalf("got %v, want %v", err, want)
			}
		})
	}
}

func TestCouncilDispatchWards(t *testing.T) {
	for _, tc := range []string{"message", "ping pong", "token budget", "missing budget", "run window", "pinned budget"} {
		t.Run(tc, func(t *testing.T) {
			ctx := t.Context()
			s := councilDB(t, filepath.Join(t.TempDir(), "state.db"))
			room, spec := councilFixture()
			if tc == "pinned budget" {
				spec.Budget = &domain.PerTaskBudget{MaxInputTokens: 1}
			}
			if err := s.CreateCouncil(ctx, room, spec); err != nil {
				t.Fatal(err)
			}
			first := councilTask("reviewer", 1, 1)
			if err := reserve(t, s, first); err != nil {
				t.Fatal(err)
			}
			limits := store.ConclaveLimits{Now: time.Now, MaxMessages: 10, MaxPingPong: 10, MaxRuns: 6}
			switch tc {
			case "message", "ping pong":
				if tc == "message" {
					limits.MaxMessages = 1
				} else {
					limits.MaxPingPong = 1
				}
				if _, err := s.AppendConclaveMessage(ctx, domain.ConclaveMessage{ConclaveID: room.ID, FromRole: "reviewer", Kind: "question", Body: "finding"}); err != nil {
					t.Fatal(err)
				}
			case "token budget", "pinned budget":
				limits.Budget = &domain.PerTaskBudget{MaxInputTokens: 1}
				if tc == "pinned budget" {
					limits.Budget.MaxInputTokens = 10
				}
				if err := s.RecordBudgetUsage(ctx, domain.BudgetEntry{TaskID: first.ID, WorkloadName: "worker", InputTokens: 1}); err != nil {
					t.Fatal(err)
				}
			case "missing budget":
				limits.BudgetMissing = true
			case "run window":
				limits.MaxRuns = 1
			}
			next := councilTask("author", 1, 1)
			_, err := store.SubmitConclaveTurn(ctx, room.ID, next.IdempotencyKey, limits, func(ctx context.Context) (domain.Task, error) { return next, s.CreateTask(ctx, next) })
			if !errors.Is(err, store.ErrConclavePaused) && !errors.Is(err, store.ErrConclaveRunLimit) {
				t.Fatalf("expected Ward refusal, got %v", err)
			}
			snap, err := s.CouncilSnapshot(ctx, room.ID)
			if err != nil || snap.Session.State != domain.ConclaveStateInputRequired || snap.Session.WardReason == "" || len(snap.Attempts) != 1 {
				t.Fatalf("Ward reason or reservation lost: %+v %v", snap.Session, err)
			}
		})
	}
}

func TestCouncilTerminalLease(t *testing.T) {
	for _, state := range []domain.TaskState{domain.TaskStateCompleted, domain.TaskStateFailed, domain.TaskStateCancelled} {
		t.Run(string(state), func(t *testing.T) {
			ctx := t.Context()
			s := councilDB(t, filepath.Join(t.TempDir(), "state.db"))
			room, spec := councilFixture()
			if err := s.CreateCouncil(ctx, room, spec); err != nil {
				t.Fatal(err)
			}
			task := councilTask("reviewer", 1, 1)
			if err := reserve(t, s, task); err != nil {
				t.Fatal(err)
			}
			lease := domain.Lease{TaskID: task.ID, WorkloadName: task.WorkloadName, TokenSHA256: "test-digest", ExpiresAt: time.Now().Add(time.Hour)}
			if err := s.CreateLease(ctx, lease); err != nil {
				t.Fatal(err)
			}
			if _, err := s.GetActiveLeaseByTokenHash(ctx, lease.TokenSHA256, time.Now()); err != nil {
				t.Fatal(err)
			}
			if err := s.UpdateTaskState(ctx, task.ID, domain.TaskStateSubmitted, state, "done", "{}"); err != nil {
				t.Fatal(err)
			}
			if _, err := s.GetActiveLeaseByTokenHash(ctx, lease.TokenSHA256, time.Now()); !errors.Is(err, store.ErrNotFound) {
				t.Errorf("lease survived terminal transition: %v", err)
			}
			lease.TokenSHA256 = "late-digest"
			if err := s.CreateLease(ctx, lease); err == nil {
				t.Error("created lease after terminal transition")
			}
			if err := s.UpdateTaskState(ctx, task.ID, state, domain.TaskStateSubmitted, "reopen", "{}"); err == nil {
				t.Error("reopened terminal attempt")
			}
		})
	}
}

func TestCouncilRelicTextBoundaries(t *testing.T) {
	for _, tc := range []string{"sensitive metadata", "oversized media type", "invalid text", "digest", "long name", "too many"} {
		t.Run(tc, func(t *testing.T) {
			ctx := t.Context()
			s := councilDB(t, filepath.Join(t.TempDir(), "state.db"))
			room, spec := councilFixture()
			if err := s.CreateCouncil(ctx, room, spec); err != nil {
				t.Fatal(err)
			}
			task := councilTask("reviewer", 1, 1)
			if err := reserve(t, s, task); err != nil {
				t.Fatal(err)
			}
			r := domain.ConclaveRelic{ConclaveID: room.ID, AuthorRole: "reviewer", Name: "result", ContentType: "text/plain", Body: "answer"}
			switch tc {
			case "digest":
				r.Name = "digest"
			case "long name":
				r.Name = strings.Repeat("x", 256)
			case "too many":
				for i := range 16 {
					r.Name = fmt.Sprintf("item-%d", i)
					if err := s.PutCouncilRelic(ctx, task.ID, r); err != nil {
						t.Fatal(err)
					}
				}
				if err := s.PutCouncilRelic(ctx, task.ID, r); err != nil {
					t.Fatal("replay at limit", err)
				}
				r.Name = "extra"
			case "sensitive metadata":
				r.Name = "password=fixture-value"
			case "oversized media type":
				r.ContentType = strings.Repeat("x", store.MaxCouncilText+1)
			case "invalid text":
				r.Body = string([]byte{0xff})
			}
			if err := s.PutCouncilRelic(ctx, task.ID, r); err == nil {
				t.Fatal("invalid relic persisted")
			}
		})
	}
}

func TestCouncilSeatDrift(t *testing.T) {
	for _, zone := range []domain.WorkloadZone{domain.ZoneRuntime, ""} {
		t.Run("changed to "+string(zone), func(t *testing.T) {
			ctx := t.Context()
			s := councilDB(t, filepath.Join(t.TempDir(), "state.db"))
			room, spec := councilFixture()
			if err := s.CreateCouncil(ctx, room, spec); err != nil {
				t.Fatal(err)
			}
			if err := s.UpsertWorkload(ctx, domain.WorkloadRecord{Name: "worker", Zone: zone}); err != nil {
				t.Fatal(err)
			}
			if err := reserve(t, s, councilTask("reviewer", 1, 1)); err == nil {
				t.Fatal("reserved seat after its workload zone changed")
			}
			snap, err := s.CouncilSnapshot(ctx, room.ID)
			if err != nil || len(snap.Attempts) != 0 || snap.Members[0].Zone != domain.ZoneSandbox {
				t.Fatalf("pinned seat or starts changed: %v", err)
			}
		})
	}
}

func TestCouncilResultTransactions(t *testing.T) {
	for _, fault := range []string{"answer link", "digest insert", "two writers", "legacy post", "stale retry"} {
		t.Run(fault, func(t *testing.T) {
			ctx := t.Context()
			path := filepath.Join(t.TempDir(), "state.db")
			s := councilDB(t, path)
			peer := councilDB(t, path)
			room, spec := councilFixture()
			if err := s.CreateCouncil(ctx, room, spec); err != nil {
				t.Fatal(err)
			}
			task := councilTask("author", 1, 1)
			if err := reserve(t, s, task); err != nil {
				t.Fatal(err)
			}
			if err := s.UpdateTaskState(ctx, task.ID, task.State, domain.TaskStateCompleted, "done", "{}"); err != nil {
				t.Fatal(err)
			}
			msg := domain.ConclaveMessage{ConclaveID: room.ID, TaskID: task.ID, FromRole: "author", ToRole: "all", Kind: "review", Body: "result", CreatedAt: time.Now().UTC()}
			switch fault {
			case "digest insert":
				if _, err := s.db.ExecContext(ctx, `CREATE TRIGGER fail_digest BEFORE INSERT ON conclave_relics BEGIN SELECT RAISE(ABORT,'injected'); END`); err != nil {
					t.Fatal(err)
				}
				if _, err := s.CloseCouncil(ctx, room.ID, domain.ConclaveStateCancelled, "owner_stop"); err == nil {
					t.Fatal("closed through fault")
				}
				saved, err := peer.GetConclave(ctx, room.ID)
				if err != nil || saved.State != domain.ConclaveStateActive || saved.WardReason != "" {
					t.Fatal(saved, err)
				}
				if _, err := peer.GetConclaveRelic(ctx, room.ID, "digest"); !errors.Is(err, store.ErrNotFound) {
					t.Fatal("unexpected digest", err)
				}
				if _, err := s.db.ExecContext(ctx, "DROP TRIGGER fail_digest"); err != nil {
					t.Fatal(err)
				}
				d, err := peer.CloseCouncil(ctx, room.ID, domain.ConclaveStateCancelled, "owner_stop")
				if err != nil {
					t.Fatal(err)
				}
				d.Body = "forged"
				if err := s.PutConclaveRelic(ctx, d); err == nil {
					t.Fatal("overwrote closed digest")
				}
				return
			case "stale retry":
				if err := reserve(t, s, councilTask("author", 1, 2)); err != nil {
					t.Fatal(err)
				}
				if _, _, err := peer.RecordCouncilAnswer(ctx, msg, false); !errors.Is(err, store.ErrConflict) {
					t.Fatal("stale answer", err)
				}
				msgs, err := s.ListConclaveMessages(ctx, room.ID, 0)
				if err != nil || len(msgs) != 0 {
					t.Fatal("stale post", err)
				}
				return
			case "legacy post":
				if _, err := s.AppendConclaveMessage(ctx, msg); err != nil {
					t.Fatal(err)
				}
			case "answer link":
				if _, err := s.db.ExecContext(ctx, `CREATE TRIGGER fail_link BEFORE UPDATE OF answer_id ON council_attempts BEGIN SELECT RAISE(ABORT,'injected'); END`); err != nil {
					t.Fatal(err)
				}
				if _, _, err := s.RecordCouncilAnswer(ctx, msg, false); err == nil {
					t.Fatal("recorded through fault")
				}
				msgs, err := peer.ListConclaveMessages(ctx, room.ID, 0)
				if err != nil || len(msgs) != 0 {
					t.Fatal("post survived rollback", err)
				}
				if _, err := s.db.ExecContext(ctx, "DROP TRIGGER fail_link"); err != nil {
					t.Fatal(err)
				}
			}
			var writers = []*SQLiteStore{peer}
			if fault == "two writers" {
				writers = append(writers, s)
			}
			start := make(chan struct{})
			results := make(chan error, len(writers))
			for _, w := range writers {
				go func() { <-start; _, _, err := w.RecordCouncilAnswer(ctx, msg, false); results <- err }()
			}
			close(start)
			for range writers {
				if err := <-results; err != nil {
					t.Fatal(err)
				}
			}
			msgs, err := peer.ListConclaveMessages(ctx, room.ID, 0)
			if err != nil || len(msgs) != 1 {
				t.Fatalf("got %d posts: %v", len(msgs), err)
			}
			snap, err := s.CouncilSnapshot(ctx, room.ID)
			if err != nil || len(snap.Turns) != 1 || snap.Turns[0].Answer == nil || snap.Turns[0].Answer.ID != msgs[0].ID {
				t.Fatal("missing linked answer", err)
			}
		})
	}
}

func TestCouncilLegacyOversizedEvidence(t *testing.T) {
	ctx := t.Context()
	s := councilDB(t, filepath.Join(t.TempDir(), "state.db"))
	room, spec := councilFixture()
	if err := s.CreateCouncil(ctx, room, spec); err != nil {
		t.Fatal(err)
	}
	task := councilTask("reviewer", 1, 1)
	if err := reserve(t, s, task); err != nil {
		t.Fatal(err)
	}
	for i := range 17 {
		name := fmt.Sprintf("%02d", i) + strings.Repeat("é", (store.MaxCouncilText-2)/2)
		_, err := s.db.ExecContext(ctx, `INSERT INTO conclave_relics(conclave_id,name,author_role,content_type,body,sha256,created_at,council_task_id)
			VALUES(?,?,'reviewer','text/plain','body',?,?,?)`, room.ID, name, fmt.Sprintf("%x", sha256.Sum256([]byte("body"))), nowRFC3339(), task.ID)
		if err != nil {
			t.Fatal(err)
		}
	}
	d, err := s.CloseCouncil(ctx, room.ID, domain.ConclaveStateCancelled, "owner_stop")
	if err != nil || len(d.Body) > store.MaxConclaveTurnInput || !utf8.ValidString(d.Body) || !strings.Contains(d.Body, "[omitted]") || !strings.HasSuffix(d.Body, "  \"author\"\n    Missing answer\n") {
		t.Fatalf("legacy digest %d bytes: %v", len(d.Body), err)
	}
}

func TestCouncilTerminalUpdateConflict(t *testing.T) {
	for _, state := range []domain.ConclaveState{domain.ConclaveStateCompleted, domain.ConclaveStateCancelled} {
		t.Run(string(state), func(t *testing.T) {
			ctx := t.Context()
			s := councilDB(t, filepath.Join(t.TempDir(), "state.db"))
			room, spec := councilFixture()
			if err := s.CreateCouncil(ctx, room, spec); err != nil {
				t.Fatal(err)
			}
			if err := s.UpdateConclaveState(ctx, room.ID, state, "original"); err != nil {
				t.Fatal(err)
			}
			if err := s.UpdateConclaveState(ctx, room.ID, state, "different"); err == nil {
				t.Fatal("reported a change to the terminal stop reason")
			}
		})
	}
}
