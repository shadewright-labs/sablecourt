package store_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/shadewright-labs/sablecourt/internal/domain"
	"github.com/shadewright-labs/sablecourt/internal/store"
)

func councilSpec(sequential bool) store.CouncilSpec {
	rounds := 2
	spec := store.CouncilSpec{Question: "What changed?", Sequential: sequential, MaxRounds: &rounds}
	for _, role := range []string{"reviewer", "author"} {
		spec.Members = append(spec.Members, store.CouncilMember{
			Vassal: domain.ConclaveVassal{Role: role, Workload: "worker", CanMessage: []string{"all", "operator"}},
			Zone:   domain.ZoneSandbox, RoleCard: "Read the change.",
		})
	}
	return spec
}

func (r *conclaveRig) council(t *testing.T, id string, spec store.CouncilSpec) {
	t.Helper()
	if err := r.st.CreateCouncil(t.Context(), domain.ConclaveSession{ID: id, State: domain.ConclaveStateActive, CreatedAt: r.now}, spec); err != nil {
		t.Fatal(err)
	}
}

func councilTurnTask(t *testing.T, room, role string, round, attempt int) domain.Task {
	a := store.ConclaveAssignment{ConclaveID: room, Role: role, Round: round, Attempt: attempt}
	return domain.Task{ID: fmt.Sprintf("%s-%s-%d-%d", room, role, round, attempt), WorkloadName: "worker", State: domain.TaskStateSubmitted,
		InputJSON: assignmentInput(t, a), IdempotencyKey: a.TurnKey()}
}

func (r *conclaveRig) reserveTurn(t *testing.T, room string, task domain.Task) error {
	t.Helper()
	limits := store.ConclaveLimits{Now: time.Now, MaxMessages: 20, MaxPingPong: 20, MaxRuns: 6}
	_, err := store.SubmitConclaveTurn(t.Context(), room, task.IdempotencyKey, limits, func(ctx context.Context) (domain.Task, error) {
		return task, r.st.CreateTask(ctx, task)
	})
	return err
}

func TestCouncilAttemptReservation(t *testing.T) {
	r := newConclaveRig(t)
	ctx := t.Context()
	r.council(t, "panel", councilSpec(false))
	if err := r.st.UpsertWorkload(ctx, domain.WorkloadRecord{Name: "runner", Zone: domain.ZoneRuntime}); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		mutate func(*domain.Task)
		task   domain.Task
	}{
		{name: "unknown seat", task: councilTurnTask(t, "panel", "intruder", 1, 1)},
		{name: "round past the policy", task: councilTurnTask(t, "panel", "reviewer", 3, 1)},
		{name: "second round before the first", task: councilTurnTask(t, "panel", "reviewer", 2, 1)},
		{name: "attempt without a first attempt", task: councilTurnTask(t, "panel", "reviewer", 1, 2)},
		{name: "other workload", task: councilTurnTask(t, "panel", "reviewer", 1, 1), mutate: func(task *domain.Task) { task.WorkloadName = "runner" }},
		{name: "task already running", task: councilTurnTask(t, "panel", "reviewer", 1, 1), mutate: func(task *domain.Task) { task.State = domain.TaskStateWorking }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			task := tc.task
			if tc.mutate != nil {
				tc.mutate(&task)
			}
			if err := r.reserveTurn(t, "panel", task); err == nil {
				t.Fatal("reservation accepted")
			}
			if _, err := r.st.GetTask(ctx, task.ID); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("refused reservation stored a task: %v", err)
			}
		})
	}

	first := councilTurnTask(t, "panel", "reviewer", 1, 1)
	if err := r.reserveTurn(t, "panel", first); err != nil {
		t.Fatal(err)
	}
	// A live attempt cannot be retried.
	if err := r.reserveTurn(t, "panel", councilTurnTask(t, "panel", "reviewer", 1, 2)); err == nil {
		t.Fatal("retry of a running attempt accepted")
	}
	if err := r.st.UpdateTaskState(ctx, first.ID, domain.TaskStateSubmitted, domain.TaskStateFailed, "test", "{}"); err != nil {
		t.Fatal(err)
	}
	if err := r.reserveTurn(t, "panel", councilTurnTask(t, "panel", "reviewer", 1, 2)); err != nil {
		t.Fatalf("retry of a failed attempt = %v", err)
	}
}

func TestCouncilSequentialAndZone(t *testing.T) {
	r := newConclaveRig(t)
	ctx := t.Context()
	r.council(t, "seq", councilSpec(true))
	if err := r.reserveTurn(t, "seq", councilTurnTask(t, "seq", "author", 1, 1)); err == nil {
		t.Fatal("second seat reserved before the first answered")
	}
	if err := r.reserveTurn(t, "seq", councilTurnTask(t, "seq", "reviewer", 1, 1)); err != nil {
		t.Fatal(err)
	}

	spec := councilSpec(false)
	spec.Members[0].Vassal.Workload = "drifted"
	if err := r.st.UpsertWorkload(ctx, domain.WorkloadRecord{Name: "drifted", Zone: domain.ZoneSandbox}); err != nil {
		t.Fatal(err)
	}
	r.council(t, "zone", spec)
	// The workload moved to another zone after the council pinned it.
	if err := r.st.UpsertWorkload(ctx, domain.WorkloadRecord{Name: "drifted", Zone: domain.ZoneRuntime}); err != nil {
		t.Fatal(err)
	}
	task := councilTurnTask(t, "zone", "reviewer", 1, 1)
	task.WorkloadName = "drifted"
	if err := r.reserveTurn(t, "zone", task); err == nil {
		t.Fatal("seat reserved for a workload outside the pinned zone")
	}
}

func TestCouncilReservedAnswer(t *testing.T) {
	r := newConclaveRig(t)
	ctx := t.Context()
	r.council(t, "panel", councilSpec(false))
	task := councilTurnTask(t, "panel", "reviewer", 1, 1)
	if err := r.reserveTurn(t, "panel", task); err != nil {
		t.Fatal(err)
	}
	answer := domain.ConclaveMessage{ConclaveID: "panel", TaskID: task.ID, FromRole: "reviewer", ToRole: "all", Kind: "review", Body: "Ship it.", CreatedAt: r.now}

	if _, _, err := r.st.RecordCouncilAnswer(ctx, answer, false); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("answer before the task completed = %v", err)
	}
	if err := r.st.UpdateTaskState(ctx, task.ID, domain.TaskStateSubmitted, domain.TaskStateCompleted, "done", "{}"); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		mutate func(*domain.ConclaveMessage)
	}{
		{"not a review", func(m *domain.ConclaveMessage) { m.Kind = "proposal" }},
		{"blank body", func(m *domain.ConclaveMessage) { m.Body = " " }},
		{"secret in body", func(m *domain.ConclaveMessage) { m.Body = "token " + githubToken() }},
		{"no timestamp", func(m *domain.ConclaveMessage) { m.CreatedAt = time.Time{} }},
		{"unknown author", func(m *domain.ConclaveMessage) { m.FromRole = "intruder" }},
		{"unknown recipient", func(m *domain.ConclaveMessage) { m.ToRole = "ghost" }},
		{"another seat's task", func(m *domain.ConclaveMessage) { m.FromRole = "author" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			msg := answer
			tc.mutate(&msg)
			if id, _, err := r.st.RecordCouncilAnswer(ctx, msg, false); !errors.Is(err, store.ErrConflict) || id != 0 {
				t.Fatalf("RecordCouncilAnswer = %d, %v; want ErrConflict", id, err)
			}
		})
	}

	id, _, err := r.st.RecordCouncilAnswer(ctx, answer, true)
	if err != nil || id == 0 {
		t.Fatalf("answer = %d, %v", id, err)
	}
	again, _, err := r.st.RecordCouncilAnswer(ctx, answer, true)
	if err != nil || again != id {
		t.Fatalf("replayed answer = %d (want %d), %v", again, id, err)
	}
	if _, _, err := r.st.RecordCouncilAnswer(ctx, answer, false); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("answer with a changed vote = %v", err)
	}
	other := answer
	other.Body = "Changed my mind."
	if _, _, err := r.st.RecordCouncilAnswer(ctx, other, true); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("second answer for the same attempt = %v", err)
	}
	snap, err := r.st.CouncilSnapshot(ctx, "panel")
	if err != nil || len(snap.Turns) != 1 || snap.Turns[0].Answer == nil || snap.Turns[0].Answer.ID != id || !snap.Turns[0].NothingToAdd {
		t.Fatalf("snapshot = %+v, %v", snap, err)
	}
}
