package store_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/shadewright-labs/sablecourt/internal/domain"
	"github.com/shadewright-labs/sablecourt/internal/store"
	"github.com/shadewright-labs/sablecourt/internal/store/postgres"
	"github.com/shadewright-labs/sablecourt/internal/store/sqlite"
)

func TestLimitsForConclave(t *testing.T) {
	now := func() time.Time { return time.Time{} }
	tpl := func(seats int, wards domain.ConclaveWards, budgetRef string) domain.Conclave {
		c := domain.Conclave{Spec: domain.ConclaveSpec{Wards: wards, BudgetRef: budgetRef}}
		for range seats {
			c.Spec.Vassals = append(c.Spec.Vassals, domain.ConclaveVassal{})
		}
		return c
	}
	budgets := map[string]domain.Budget{"small": {Spec: domain.BudgetSpec{PerTask: domain.PerTaskBudget{MaxInputTokens: 7}}}}
	for _, tc := range []struct {
		name        string
		tpl         domain.Conclave
		hasTemplate bool
		want        store.ConclaveLimits
	}{
		{"no template uses three seats", tpl(5, domain.ConclaveWards{}, ""), false,
			store.ConclaveLimits{MaxMessages: 6, MaxPingPong: 6, MaxRuns: domain.ConclaveRunCeiling}},
		{"template without seats", tpl(0, domain.ConclaveWards{}, ""), true,
			store.ConclaveLimits{MaxMessages: 6, MaxPingPong: 6, MaxRuns: domain.ConclaveRunCeiling, MaxAge: store.ConclaveWallTime}},
		{"seats scale defaults", tpl(4, domain.ConclaveWards{}, ""), true,
			store.ConclaveLimits{MaxMessages: 8, MaxPingPong: 8, MaxRuns: domain.ConclaveRunCeiling, MaxAge: store.ConclaveWallTime}},
		{"explicit wards", tpl(4, domain.ConclaveWards{MaxMessages: 3, MaxPingPong: 2, MaxRuns: 1}, ""), true,
			store.ConclaveLimits{MaxMessages: 3, MaxPingPong: 2, MaxRuns: 1, MaxAge: store.ConclaveWallTime}},
		{"negative wards fall back", tpl(2, domain.ConclaveWards{MaxMessages: -1, MaxPingPong: -1, MaxRuns: -1}, ""), true,
			store.ConclaveLimits{MaxMessages: 4, MaxPingPong: 4, MaxRuns: domain.ConclaveRunCeiling, MaxAge: store.ConclaveWallTime}},
		{"missing budget fails closed", tpl(2, domain.ConclaveWards{}, "absent"), true,
			store.ConclaveLimits{MaxMessages: 4, MaxPingPong: 4, MaxRuns: domain.ConclaveRunCeiling, MaxAge: store.ConclaveWallTime, BudgetMissing: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := store.LimitsForConclave(tc.tpl, tc.hasTemplate, budgets, now)
			if got.Now == nil || got.Budget != nil {
				t.Fatalf("limits = %+v", got)
			}
			if got.MaxMessages != tc.want.MaxMessages || got.MaxPingPong != tc.want.MaxPingPong || got.MaxRuns != tc.want.MaxRuns ||
				got.MaxAge != tc.want.MaxAge || got.BudgetMissing != tc.want.BudgetMissing {
				t.Fatalf("LimitsForConclave = %+v, want %+v", got, tc.want)
			}
		})
	}
	got := store.LimitsForConclave(tpl(2, domain.ConclaveWards{}, "small"), true, budgets, now)
	if got.BudgetMissing || got.Budget == nil || got.Budget.MaxInputTokens != 7 {
		t.Fatalf("referenced budget = %+v", got)
	}
}

func assignmentInput(t *testing.T, v any) string {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func TestTaskConclaveAssignment(t *testing.T) {
	council := store.ConclaveAssignment{ConclaveID: "room", Role: "reviewer", Round: 1, Attempt: 2}
	initTurn := store.ConclaveAssignment{ConclaveID: "room", Role: "a", Activation: domain.ActivationAlwaysOn}
	mention := store.ConclaveAssignment{ConclaveID: "room", Role: "b", MessageID: 9, Activation: domain.ActivationOnMention}
	if got := council.TurnKey(); got != "conclave-round:4:room:1:2:reviewer" {
		t.Fatalf("council key = %q", got)
	}
	if initTurn.TurnKey() != "conclave-room-init-a" || mention.TurnKey() != "conclave-room-msg-9-b" {
		t.Fatalf("keys = %q, %q", initTurn.TurnKey(), mention.TurnKey())
	}
	retryInput := map[string]any{"conclaveId": "room", "role": "a", "activation": "always-on", "retried_from": "t1", "retry_count": 2}
	for _, tc := range []struct {
		name string
		task domain.Task
		want *store.ConclaveAssignment
	}{
		{"council turn", domain.Task{IdempotencyKey: council.TurnKey(), InputJSON: assignmentInput(t, council)}, &council},
		{"init turn", domain.Task{IdempotencyKey: initTurn.TurnKey(), InputJSON: assignmentInput(t, initTurn)}, &initTurn},
		{"mention turn", domain.Task{IdempotencyKey: mention.TurnKey(), InputJSON: assignmentInput(t, mention)}, &mention},
		{"retry turn", domain.Task{IdempotencyKey: "conclave-room-retry-t1-2", InputJSON: assignmentInput(t, retryInput)}, &initTurn},
		{"retry with the turn key", domain.Task{IdempotencyKey: initTurn.TurnKey(), InputJSON: assignmentInput(t, retryInput)}, nil},
		{"child task", domain.Task{ParentTaskID: "p", IdempotencyKey: initTurn.TurnKey(), InputJSON: assignmentInput(t, initTurn)}, nil},
		{"ordinary key", domain.Task{IdempotencyKey: "job-1", InputJSON: assignmentInput(t, initTurn)}, nil},
		{"key for another room", domain.Task{IdempotencyKey: "conclave-other-init-a", InputJSON: assignmentInput(t, initTurn)}, nil},
		{"oversize input", domain.Task{IdempotencyKey: initTurn.TurnKey(), InputJSON: `{"conclaveId":"room","role":"a","activation":"always-on","pad":"` +
			strings.Repeat("x", store.MaxConclaveTurnInput) + `"}`}, nil},
		{"malformed input", domain.Task{IdempotencyKey: initTurn.TurnKey(), InputJSON: `{`}, nil},
		{"always-on with message", domain.Task{IdempotencyKey: "conclave-room-msg-9-a",
			InputJSON: assignmentInput(t, store.ConclaveAssignment{ConclaveID: "room", Role: "a", MessageID: 9, Activation: domain.ActivationAlwaysOn})}, nil},
		{"on-mention without message", domain.Task{IdempotencyKey: "conclave-room-init-b",
			InputJSON: assignmentInput(t, store.ConclaveAssignment{ConclaveID: "room", Role: "b", Activation: domain.ActivationOnMention})}, nil},
		{"missing role", domain.Task{IdempotencyKey: "conclave-room-init-",
			InputJSON: assignmentInput(t, store.ConclaveAssignment{ConclaveID: "room", Activation: domain.ActivationAlwaysOn})}, nil},
		{"council turn with activation", domain.Task{IdempotencyKey: council.TurnKey(),
			InputJSON: assignmentInput(t, store.ConclaveAssignment{ConclaveID: "room", Role: "reviewer", Round: 1, Attempt: 2, Activation: domain.ActivationAlwaysOn})}, nil},
		{"council turn as a retry", domain.Task{IdempotencyKey: council.TurnKey(),
			InputJSON: `{"conclaveId":"room","role":"reviewer","round":1,"attempt":2,"retried_from":"x","retry_count":1}`}, nil},
		{"council key for another seat", domain.Task{IdempotencyKey: council.TurnKey(),
			InputJSON: assignmentInput(t, store.ConclaveAssignment{ConclaveID: "room", Role: "author", Round: 1, Attempt: 2})}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := store.TaskConclaveAssignment(tc.task)
			if tc.want == nil {
				if ok {
					t.Fatalf("TaskConclaveAssignment accepted %+v as %+v", tc.task, got)
				}
				return
			}
			if !ok || got != *tc.want {
				t.Fatalf("TaskConclaveAssignment = %+v, %v; want %+v", got, ok, *tc.want)
			}
		})
	}
}

type conclaveRig struct {
	st  *sqlite.SQLiteStore
	now time.Time
}

func newConclaveRig(t *testing.T) *conclaveRig {
	t.Helper()
	st, err := sqlite.Open(filepath.Join(t.TempDir(), "conclave.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.Migrate(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertWorkload(t.Context(), domain.WorkloadRecord{Name: "worker", Zone: domain.ZoneSandbox}); err != nil {
		t.Fatal(err)
	}
	return &conclaveRig{st: st, now: time.Now().UTC()}
}

func (r *conclaveRig) room(t *testing.T, id string, state domain.ConclaveState, age time.Duration) {
	t.Helper()
	created := r.now.Add(-age)
	if err := r.st.CreateConclave(t.Context(), domain.ConclaveSession{ID: id, State: state, CreatedAt: created, UpdatedAt: created}); err != nil {
		t.Fatal(err)
	}
}

func (r *conclaveRig) limits() store.ConclaveLimits {
	return store.ConclaveLimits{MaxMessages: 10, MaxPingPong: 10, MaxRuns: 6, MaxAge: store.ConclaveWallTime, Now: func() time.Time { return r.now }}
}

func (r *conclaveRig) post(t *testing.T, room, from string, limits store.ConclaveLimits, resume, wake bool) (int64, string, time.Time, error) {
	t.Helper()
	to := "all"
	kind := "proposal"
	if from == "operator" {
		kind = "decree"
	}
	return store.AppendConclavePost(t.Context(), r.st, domain.ConclaveMessage{
		ConclaveID: room, FromRole: from, ToRole: to, Kind: kind, Body: "from " + from, CreatedAt: r.now,
	}, limits, resume, wake)
}

func roomState(t *testing.T, st store.Store, id string) domain.ConclaveSession {
	t.Helper()
	c, err := st.GetConclave(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestConclavePostWards(t *testing.T) {
	t.Run("message ceiling pauses and refuses, and a Decree cannot lift it", func(t *testing.T) {
		r := newConclaveRig(t)
		r.room(t, "room", domain.ConclaveStateActive, 0)
		limits := r.limits()
		limits.MaxMessages = 3
		for i, from := range []string{"a", "b", "c"} {
			id, reason, _, err := r.post(t, "room", from, limits, false, false)
			if err != nil || id == 0 {
				t.Fatalf("post %d: %d, %v", i, id, err)
			}
			if want := map[bool]string{true: "max_messages_reached(3)"}[i == 2]; reason != want {
				t.Fatalf("post %d reason = %q, want %q", i, reason, want)
			}
		}
		if c := roomState(t, r.st, "room"); c.State != domain.ConclaveStateInputRequired || c.WardReason != "max_messages_reached(3)" {
			t.Fatalf("room after ceiling = %+v", c)
		}
		_, _, _, err := r.post(t, "room", "a", limits, false, false)
		if !errors.Is(err, store.ErrConclavePaused) || !errors.Is(err, store.ErrWardTripped) {
			t.Fatalf("post at the ceiling = %v", err)
		}
		// An operator post without resume stays refused.
		if _, _, _, err := r.post(t, "room", "operator", limits, false, false); !errors.Is(err, store.ErrConclavePaused) {
			t.Fatalf("operator post without resume = %v", err)
		}
		if _, _, _, err := r.post(t, "room", "operator", limits, true, false); !errors.Is(err, store.ErrWardTripped) {
			t.Fatalf("resume past maxMessages = %v", err)
		}
	})

	t.Run("a Decree resumes a ping-pong pause and opens a run window", func(t *testing.T) {
		r := newConclaveRig(t)
		r.room(t, "room", domain.ConclaveStateActive, 0)
		limits := r.limits()
		limits.MaxPingPong = 2
		for _, from := range []string{"a", "b"} {
			if _, _, _, err := r.post(t, "room", from, limits, false, false); err != nil {
				t.Fatal(err)
			}
		}
		if c := roomState(t, r.st, "room"); c.WardReason != "ping_pong_detected(2)" {
			t.Fatalf("room after ping pong = %+v", c)
		}
		id, reason, _, err := r.post(t, "room", "operator", limits, true, false)
		if err != nil || id == 0 || reason != "" {
			t.Fatalf("operator resume = %d, %q, %v", id, reason, err)
		}
		if c := roomState(t, r.st, "room"); c.State != domain.ConclaveStateActive || c.WardReason != "" {
			t.Fatalf("room after resume = %+v", c)
		}
		audit, err := r.st.ListAudit(t.Context(), 10)
		if err != nil {
			t.Fatal(err)
		}
		if len(audit) == 0 || audit[0].Action != "conclave.run_window_reset" || !strings.Contains(audit[0].DetailJSON, `"conclave_id":"room"`) {
			t.Fatalf("resume audit = %+v", audit)
		}
		// The ping-pong window restarts after the decree.
		if _, reason, _, err := r.post(t, "room", "a", limits, false, false); err != nil || reason != "" {
			t.Fatalf("post after resume = %q, %v", reason, err)
		}
	})

	t.Run("ping pong", func(t *testing.T) {
		r := newConclaveRig(t)
		r.room(t, "room", domain.ConclaveStateActive, 0)
		limits := r.limits()
		limits.MaxPingPong = 3
		var reason string
		for _, from := range []string{"a", "b", "a"} {
			var err error
			if _, reason, _, err = r.post(t, "room", from, limits, false, false); err != nil {
				t.Fatal(err)
			}
		}
		if reason != "ping_pong_detected(3)" || roomState(t, r.st, "room").State != domain.ConclaveStateInputRequired {
			t.Fatalf("ping pong reason = %q", reason)
		}
	})

	t.Run("three voices are not ping pong", func(t *testing.T) {
		r := newConclaveRig(t)
		r.room(t, "room", domain.ConclaveStateActive, 0)
		limits := r.limits()
		limits.MaxPingPong = 3
		for _, from := range []string{"a", "b", "c"} {
			if _, reason, _, err := r.post(t, "room", from, limits, false, false); err != nil || reason != "" {
				t.Fatalf("post %s = %q, %v", from, reason, err)
			}
		}
	})

	t.Run("refusals", func(t *testing.T) {
		r := newConclaveRig(t)
		r.room(t, "old", domain.ConclaveStateActive, time.Hour)
		r.room(t, "done", domain.ConclaveStateCompleted, 0)
		r.room(t, "nobudget", domain.ConclaveStateActive, 0)
		if _, _, _, err := r.post(t, "old", "a", r.limits(), false, false); !errors.Is(err, store.ErrWardTripped) || !strings.Contains(err.Error(), "wall_time_reached") {
			t.Fatalf("expired room = %v", err)
		}
		if c := roomState(t, r.st, "old"); c.State != domain.ConclaveStateInputRequired || c.WardReason != "wall_time_reached" {
			t.Fatalf("expired room state = %+v", c)
		}
		// The deadline cannot be lifted by an operator decree.
		if _, _, _, err := r.post(t, "old", "operator", r.limits(), true, false); !errors.Is(err, store.ErrConclavePaused) {
			t.Fatalf("resume past the deadline = %v", err)
		}
		if _, _, _, err := r.post(t, "done", "a", r.limits(), false, false); !errors.Is(err, store.ErrConclaveClosed) {
			t.Fatalf("closed room = %v", err)
		}
		if _, _, _, err := r.post(t, "missing", "a", r.limits(), false, false); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("missing room = %v", err)
		}
		limits := r.limits()
		limits.BudgetMissing = true
		if _, reason, _, err := r.post(t, "nobudget", "a", limits, false, false); !errors.Is(err, store.ErrWardTripped) || reason != "budget_evidence_unavailable" {
			t.Fatalf("missing budget = %q, %v", reason, err)
		}
		msgs, err := r.st.ListConclaveMessages(t.Context(), "nobudget", 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range msgs {
			if m.FromRole == "a" {
				t.Fatalf("refused post was stored: %+v", m)
			}
		}
	})

	t.Run("wake replays the pending dispatch", func(t *testing.T) {
		r := newConclaveRig(t)
		r.room(t, "room", domain.ConclaveStateActive, 0)
		first, _, retryAt, err := r.post(t, "room", "a", r.limits(), false, true)
		if err != nil || first == 0 || !retryAt.IsZero() {
			t.Fatalf("first wake post = %d, %v, %v", first, retryAt, err)
		}
		again, _, retryAt, err := r.post(t, "room", "a", r.limits(), false, true)
		if err != nil || again != first || !retryAt.Equal(r.now) {
			t.Fatalf("replayed wake post = %d (want %d), %v, %v", again, first, retryAt, err)
		}
		if err := r.st.FinishConclaveWake(t.Context(), first); err != nil {
			t.Fatal(err)
		}
		third, _, _, err := r.post(t, "room", "a", r.limits(), false, true)
		if err != nil || third == first {
			t.Fatalf("post after the wake finished = %d, %v", third, err)
		}
	})

	t.Run("budget ward counts the room's turns", func(t *testing.T) {
		r := newConclaveRig(t)
		r.room(t, "room", domain.ConclaveStateActive, 0)
		turn := store.ConclaveAssignment{ConclaveID: "room", Role: "a", Activation: domain.ActivationAlwaysOn}
		task := domain.Task{ID: "turn-a", IdempotencyKey: turn.TurnKey(), WorkloadName: "worker", State: domain.TaskStateSubmitted, InputJSON: assignmentInput(t, turn)}
		if err := r.st.CreateTask(t.Context(), task); err != nil {
			t.Fatal(err)
		}
		if err := r.st.RecordBudgetUsage(t.Context(), domain.BudgetEntry{TaskID: task.ID, WorkloadName: "worker", OutputTokens: 30, CreatedAt: r.now}); err != nil {
			t.Fatal(err)
		}
		limits := r.limits()
		limits.Budget = &domain.PerTaskBudget{MaxOutputTokens: 30}
		id, reason, _, err := r.post(t, "room", "a", limits, false, false)
		if err != nil || id == 0 || reason != "output_tokens_exceeded(30 >= 30)" {
			t.Fatalf("post over budget = %d, %q, %v", id, reason, err)
		}
		if c := roomState(t, r.st, "room"); c.WardReason != reason {
			t.Fatalf("room = %+v", c)
		}
	})
}

func TestConclaveReport(t *testing.T) {
	r := newConclaveRig(t)
	r.room(t, "room", domain.ConclaveStateActive, 0)
	ctx := t.Context()
	msg := domain.ConclaveMessage{ConclaveID: "room", FromRole: "verifier", ToRole: "operator", Kind: "review", Body: "checked", CreatedAt: r.now}
	relic := domain.ConclaveRelic{ConclaveID: "room", Name: "report.txt", AuthorRole: "verifier", ContentType: "text/plain", Body: "ok", SHA256: relicHash("ok"), CreatedAt: r.now}

	wrong := relic
	wrong.AuthorRole = "other"
	if _, _, err := store.AppendConclaveReport(ctx, r.st, msg, r.limits(), &wrong); err == nil {
		t.Fatal("report with another author's relic accepted")
	}
	limits := r.limits()
	limits.Budget = &domain.PerTaskBudget{MaxCostUSD: 5}
	id, reason, err := store.AppendConclaveReport(ctx, r.st, msg, limits, &relic)
	if err != nil || id == 0 || reason != "" {
		t.Fatalf("report = %d, %q, %v", id, reason, err)
	}
	saved, err := r.st.GetConclaveRelic(ctx, "room", "report.txt")
	if err != nil || saved.Body != "ok" || saved.AuthorRole != "verifier" {
		t.Fatalf("saved relic = %+v, %v", saved, err)
	}
	// The same relic again is idempotent; a different body under the same name conflicts.
	if _, _, err := store.AppendConclaveReport(ctx, r.st, msg, r.limits(), &relic); err != nil {
		t.Fatalf("repeated report = %v", err)
	}
	changed := relic
	changed.Body, changed.SHA256 = "tampered", relicHash("tampered")
	if _, _, err := store.AppendConclaveReport(ctx, r.st, msg, r.limits(), &changed); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("conflicting relic = %v", err)
	}
	if saved, _ := r.st.GetConclaveRelic(ctx, "room", "report.txt"); saved.Body != "ok" {
		t.Fatalf("relic overwritten: %+v", saved)
	}
	// A report refused by a Ward stores neither the message nor the relic.
	limits = r.limits()
	limits.BudgetMissing = true
	other := relic
	other.Name = "late.txt"
	if _, _, err := store.AppendConclaveReport(ctx, r.st, msg, limits, &other); !errors.Is(err, store.ErrWardTripped) {
		t.Fatalf("report without budget evidence = %v", err)
	}
	if _, err := r.st.GetConclaveRelic(ctx, "room", "late.txt"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("refused report stored its relic: %v", err)
	}
}

func (r *conclaveRig) submit(t *testing.T, room string, task domain.Task, limits store.ConclaveLimits) error {
	t.Helper()
	_, err := store.SubmitConclaveTurn(t.Context(), room, task.IdempotencyKey, limits, func(ctx context.Context) (domain.Task, error) {
		return task, r.st.CreateTask(ctx, task)
	})
	return err
}

func turnTask(t *testing.T, room, role string, messageID int64) domain.Task {
	a := store.ConclaveAssignment{ConclaveID: room, Role: role, Activation: domain.ActivationAlwaysOn}
	if messageID != 0 {
		a.MessageID, a.Activation = messageID, domain.ActivationOnMention
	}
	return domain.Task{ID: fmt.Sprintf("%s-%s-%d", room, role, messageID), IdempotencyKey: a.TurnKey(), WorkloadName: "worker",
		State: domain.TaskStateSubmitted, InputJSON: assignmentInput(t, a)}
}

func TestConclaveTurnGuard(t *testing.T) {
	r := newConclaveRig(t)
	ctx := t.Context()
	r.room(t, "room", domain.ConclaveStateActive, 0)
	if err := r.submit(t, "room", turnTask(t, "room", "a", 0), r.limits()); err != nil {
		t.Fatal(err)
	}
	if err := r.submit(t, "room", turnTask(t, "room", "a", 0), r.limits()); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("duplicate turn = %v", err)
	}
	if err := r.submit(t, "other", turnTask(t, "room", "b", 0), r.limits()); err == nil {
		t.Fatal("turn for another room accepted")
	}
	// Without the guard, a council key cannot be inserted.
	council := store.ConclaveAssignment{ConclaveID: "room", Role: "a", Round: 1, Attempt: 1}
	if err := r.st.CreateTask(ctx, domain.Task{ID: "c", IdempotencyKey: council.TurnKey(), WorkloadName: "worker", State: domain.TaskStateSubmitted,
		InputJSON: assignmentInput(t, council)}); err == nil {
		t.Fatal("council attempt inserted without a reservation")
	}
	// A plain room refuses a council turn.
	if err := r.submit(t, "room", domain.Task{ID: "c", IdempotencyKey: council.TurnKey(), WorkloadName: "worker", State: domain.TaskStateSubmitted,
		InputJSON: assignmentInput(t, council)}, r.limits()); err == nil {
		t.Fatal("council turn accepted in a plain room")
	}

	limits := r.limits()
	limits.MaxRuns = 2
	if err := r.submit(t, "room", turnTask(t, "room", "b", 0), limits); err != nil {
		t.Fatal(err)
	}
	if err := r.submit(t, "room", turnTask(t, "room", "c", 0), limits); !errors.Is(err, store.ErrConclaveRunLimit) {
		t.Fatalf("turn past the run limit = %v", err)
	}
	if c := roomState(t, r.st, "room"); c.State != domain.ConclaveStateInputRequired || c.WardReason != store.ConclaveMaxRunsWard {
		t.Fatalf("room after the run limit = %+v", c)
	}
	if err := r.submit(t, "room", turnTask(t, "room", "c", 0), limits); !errors.Is(err, store.ErrConclavePaused) {
		t.Fatalf("turn in a paused room = %v", err)
	}
	if _, err := r.st.GetTaskByIdempotencyKey(ctx, turnTask(t, "room", "c", 0).IdempotencyKey); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("refused turn was stored: %v", err)
	}

	r.room(t, "closed", domain.ConclaveStateCancelled, 0)
	if err := r.submit(t, "closed", turnTask(t, "closed", "a", 0), r.limits()); !errors.Is(err, store.ErrConclaveClosed) {
		t.Fatalf("turn in a closed room = %v", err)
	}
	r.room(t, "old", domain.ConclaveStateActive, time.Hour)
	if err := r.submit(t, "old", turnTask(t, "old", "a", 0), r.limits()); !errors.Is(err, store.ErrConclavePaused) {
		t.Fatalf("turn in an expired room = %v", err)
	}
	if c := roomState(t, r.st, "old"); c.WardReason != "wall_time_reached" {
		t.Fatalf("expired room = %+v", c)
	}
	r.room(t, "nobudget", domain.ConclaveStateActive, 0)
	limits = r.limits()
	limits.BudgetMissing = true
	if err := r.submit(t, "nobudget", turnTask(t, "nobudget", "a", 0), limits); !errors.Is(err, store.ErrConclavePaused) {
		t.Fatalf("turn without budget evidence = %v", err)
	}
	if err := r.submit(t, "missing", turnTask(t, "missing", "a", 0), r.limits()); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("turn in a missing room = %v", err)
	}
}

func TestClaimConclaveTask(t *testing.T) {
	r := newConclaveRig(t)
	ctx := t.Context()
	now := func() time.Time { return r.now }
	r.room(t, "room", domain.ConclaveStateActive, 0)
	task := turnTask(t, "room", "a", 0)
	if err := r.submit(t, "room", task, r.limits()); err != nil {
		t.Fatal(err)
	}
	if err := store.ClaimConclaveTask(ctx, r.st, task.ID, "room", now); err != nil {
		t.Fatal(err)
	}
	if got, _ := r.st.GetTask(ctx, task.ID); got.State != domain.TaskStateWorking {
		t.Fatalf("claimed task state = %s", got.State)
	}

	// A room closed after submission cancels the claim.
	second := turnTask(t, "room", "b", 0)
	if err := r.submit(t, "room", second, r.limits()); err != nil {
		t.Fatal(err)
	}
	if err := r.st.UpdateConclaveState(ctx, "room", domain.ConclaveStateCompleted, ""); err != nil {
		t.Fatal(err)
	}
	if err := store.ClaimConclaveTask(ctx, r.st, second.ID, "room", now); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("claim in a closed room = %v", err)
	}
	if got, _ := r.st.GetTask(ctx, second.ID); got.State != domain.TaskStateCancelled {
		t.Fatalf("task in a closed room = %s, want cancelled", got.State)
	}
	events, err := r.st.ListTaskEvents(ctx, second.ID)
	if err != nil || len(events) == 0 || events[len(events)-1].Reason != "conclave_inactive" {
		t.Fatalf("events = %+v, %v", events, err)
	}

	// Past the wall time the room pauses and the claim is cancelled.
	r.room(t, "late", domain.ConclaveStateActive, 0)
	late := turnTask(t, "late", "a", 0)
	if err := r.submit(t, "late", late, r.limits()); err != nil {
		t.Fatal(err)
	}
	later := func() time.Time { return r.now.Add(time.Hour) }
	if err := store.ClaimConclaveTask(ctx, r.st, late.ID, "late", later); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("claim past the deadline = %v", err)
	}
	if c := roomState(t, r.st, "late"); c.State != domain.ConclaveStateInputRequired || c.WardReason != "wall_time_reached" {
		t.Fatalf("room after a late claim = %+v", c)
	}
	if err := store.ClaimConclaveTask(ctx, r.st, late.ID, "missing", now); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("claim in a missing room = %v", err)
	}
}

func TestConclaveAuditAndCloseStores(t *testing.T) {
	for _, backend := range []string{"sqlite", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			var st store.Store
			var err error
			var sqlitePath string
			if backend == "sqlite" {
				sqlitePath = filepath.Join(t.TempDir(), "state.db")
				st, err = sqlite.Open(sqlitePath)
			} else {
				dsn := os.Getenv("SABLECOURT_TEST_POSTGRES_DSN")
				if dsn == "" {
					t.Skip("SABLECOURT_TEST_POSTGRES_DSN is unset")
				}
				st, err = postgres.Open(isolatedPostgresDSN(t, dsn))
			}
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = st.Close() })
			if err := st.Migrate(t.Context()); err != nil {
				t.Fatal(err)
			}
			if err := st.UpsertWorkload(t.Context(), domain.WorkloadRecord{Name: "worker", Zone: domain.ZoneSandbox}); err != nil {
				t.Fatal(err)
			}
			t.Run("plain_close_preserves_terminal_state", func(t *testing.T) {
				testPlainClosePreservesTerminalState(t, st)
			})
			t.Run("ward_trips_audited_once_across_all_paths", func(t *testing.T) {
				testWardTripsAuditedOnce(t, st)
			})
			t.Run("ward_trip_revokes_turn_leases", func(t *testing.T) {
				testWardTripRevokesLeases(t, st)
			})
			if backend == "sqlite" {
				t.Run("plain_close_audit_failure_rolls_back", func(t *testing.T) {
					ctx := t.Context()
					if err := st.CreateConclave(ctx, domain.ConclaveSession{ID: "close-atomic", State: domain.ConclaveStateActive, CreatedAt: time.Now().UTC()}); err != nil {
						t.Fatal(err)
					}
					db, err := sql.Open("sqlite", sqlitePath)
					if err != nil {
						t.Fatal(err)
					}
					defer db.Close()
					if _, err := db.ExecContext(ctx, `CREATE TRIGGER fail_plain_close_audit BEFORE INSERT ON audit_log
						WHEN NEW.action = 'conclave.close' BEGIN SELECT RAISE(ABORT, 'audit unavailable'); END`); err != nil {
						t.Fatal(err)
					}
					if err := st.UpdateConclaveState(ctx, "close-atomic", domain.ConclaveStateCompleted, ""); err == nil {
						t.Fatal("UpdateConclaveState succeeded when audit insert failed")
					}
					if got := roomState(t, st, "close-atomic"); got.State != domain.ConclaveStateActive {
						t.Fatalf("failed audit left room %+v", got)
					}
					if _, err := db.ExecContext(ctx, "DROP TRIGGER fail_plain_close_audit"); err != nil {
						t.Fatal(err)
					}
				})
			}
		})
	}
}

func countRoomAudits(t *testing.T, st store.Store, action, roomID string) []domain.AuditRecord {
	t.Helper()
	rows, err := st.ListAudit(t.Context(), 500)
	if err != nil {
		t.Fatal(err)
	}
	wantRoom := fmt.Sprintf(`"conclave_id":%q`, roomID)
	var out []domain.AuditRecord
	for _, rec := range rows {
		if rec.Action == action && strings.Contains(rec.DetailJSON, wantRoom) {
			out = append(out, rec)
		}
	}
	return out
}

func testPlainClosePreservesTerminalState(t *testing.T, st store.Store) {
	ctx := t.Context()
	now := time.Now().UTC()
	for i, tc := range []struct {
		name   string
		first  domain.ConclaveState
		second domain.ConclaveState
	}{
		{"completed_then_cancelled", domain.ConclaveStateCompleted, domain.ConclaveStateCancelled},
		{"completed_then_completed", domain.ConclaveStateCompleted, domain.ConclaveStateCompleted},
		{"cancelled_then_completed", domain.ConclaveStateCancelled, domain.ConclaveStateCompleted},
		{"cancelled_then_cancelled", domain.ConclaveStateCancelled, domain.ConclaveStateCancelled},
	} {
		t.Run(tc.name, func(t *testing.T) {
			id := fmt.Sprintf("close-%d", i)
			if err := st.CreateConclave(ctx, domain.ConclaveSession{ID: id, State: domain.ConclaveStateActive, CreatedAt: now, UpdatedAt: now}); err != nil {
				t.Fatal(err)
			}
			if err := st.UpdateConclaveState(ctx, id, tc.first, "first_reason"); err != nil {
				t.Fatalf("first close: %v", err)
			}
			afterFirst := roomState(t, st, id)
			if afterFirst.State != tc.first || afterFirst.WardReason != "first_reason" {
				t.Fatalf("after first close = %+v", afterFirst)
			}
			// Second close with another terminal state must be a no-op that writes nothing.
			if err := st.UpdateConclaveState(ctx, id, tc.second, "second_reason"); err != nil {
				t.Fatalf("second close: %v", err)
			}
			afterSecond := roomState(t, st, id)
			if afterSecond.State != tc.first || afterSecond.WardReason != "first_reason" || !afterSecond.UpdatedAt.Equal(afterFirst.UpdatedAt) {
				t.Fatalf("second close overwrote terminal room: got %+v, want %+v", afterSecond, afterFirst)
			}
			// Reopening a closed room must fail without changing state.
			if err := st.UpdateConclaveState(ctx, id, domain.ConclaveStateActive, ""); !errors.Is(err, store.ErrConclaveClosed) {
				t.Fatalf("reopening closed room = %v, want ErrConclaveClosed", err)
			}
			closes := countRoomAudits(t, st, "conclave.close", id)
			if len(closes) != 1 || closes[0].Outcome != string(tc.first) {
				t.Fatalf("conclave.close audits for %s = %+v, want 1 row with outcome %s", id, closes, tc.first)
			}
			if _, err := st.VerifyAuditChain(ctx, nil); err != nil {
				t.Fatalf("VerifyAuditChain: %v", err)
			}
		})
	}
}

func testWardTripsAuditedOnce(t *testing.T, st store.Store) {
	ctx := t.Context()
	now := time.Now().UTC()
	baseLimits := store.ConclaveLimits{MaxMessages: 10, MaxPingPong: 10, MaxRuns: 6, MaxAge: store.ConclaveWallTime, Now: func() time.Time { return now }}
	mkRoom := func(id string, age time.Duration) {
		t.Helper()
		created := now.Add(-age)
		if err := st.CreateConclave(ctx, domain.ConclaveSession{ID: id, State: domain.ConclaveStateActive, CreatedAt: created, UpdatedAt: created}); err != nil {
			t.Fatal(err)
		}
	}
	submitTurn := func(room string, task domain.Task, limits store.ConclaveLimits) error {
		_, err := store.SubmitConclaveTurn(ctx, room, task.IdempotencyKey, limits, func(ctx context.Context) (domain.Task, error) {
			return task, st.CreateTask(ctx, task)
		})
		return err
	}

	t.Run("post", func(t *testing.T) {
		mkRoom("ward-post", 0)
		lim := baseLimits
		lim.MaxMessages = 1
		if _, reason, _, err := store.AppendConclavePost(ctx, st, domain.ConclaveMessage{
			ConclaveID: "ward-post", FromRole: "a", ToRole: "all", Kind: "proposal", Body: "hi", CreatedAt: now,
		}, lim, false, false); err != nil || reason != "max_messages_reached(1)" {
			t.Fatalf("post trip = %q, %v", reason, err)
		}
		if _, _, _, err := store.AppendConclavePost(ctx, st, domain.ConclaveMessage{
			ConclaveID: "ward-post", FromRole: "a", ToRole: "all", Kind: "proposal", Body: "extra", CreatedAt: now,
		}, lim, false, false); !errors.Is(err, store.ErrConclavePaused) {
			t.Fatalf("second post = %v", err)
		}
		if got := countRoomAudits(t, st, "conclave.ward", "ward-post"); len(got) != 1 || !strings.Contains(got[0].DetailJSON, `"ward":"max_messages_reached(1)"`) {
			t.Fatalf("post ward audits = %+v", got)
		}
	})

	t.Run("report", func(t *testing.T) {
		mkRoom("ward-report", 0)
		lim := baseLimits
		lim.BudgetMissing = true
		if _, _, err := store.AppendConclaveReport(ctx, st, domain.ConclaveMessage{
			ConclaveID: "ward-report", FromRole: "verifier", ToRole: "operator", Kind: "report", Body: "checked", CreatedAt: now,
		}, lim, nil); !errors.Is(err, store.ErrWardTripped) {
			t.Fatalf("report trip = %v", err)
		}
		if got := countRoomAudits(t, st, "conclave.ward", "ward-report"); len(got) != 1 || !strings.Contains(got[0].DetailJSON, `"ward":"budget_evidence_unavailable"`) {
			t.Fatalf("report ward audits = %+v", got)
		}
	})

	t.Run("turn_max_runs", func(t *testing.T) {
		mkRoom("ward-runs", 0)
		lim := baseLimits
		lim.MaxRuns = 1
		firstTurn := turnTask(t, "ward-runs", "a", 0)
		if err := submitTurn("ward-runs", firstTurn, lim); err != nil {
			t.Fatal(err)
		}
		if err := submitTurn("ward-runs", turnTask(t, "ward-runs", "b", 0), lim); !errors.Is(err, store.ErrConclaveRunLimit) {
			t.Fatalf("max_runs trip = %v", err)
		}
		if _, _, _, err := store.AppendConclavePost(ctx, st, domain.ConclaveMessage{
			ConclaveID: "ward-runs", FromRole: "a", ToRole: "all", Kind: "proposal", Body: "paid reply", TaskID: firstTurn.ID, CreatedAt: now,
		}, lim, false, false); err != nil {
			t.Fatalf("admitted reply after max_runs: %v", err)
		}
		if got := countRoomAudits(t, st, "conclave.ward", "ward-runs"); len(got) != 1 || !strings.Contains(got[0].DetailJSON, `"ward":"max_runs"`) {
			t.Fatalf("max_runs ward audits = %+v", got)
		}
	})

	t.Run("turn_deadline", func(t *testing.T) {
		mkRoom("ward-turn-deadline", time.Hour)
		if err := submitTurn("ward-turn-deadline", turnTask(t, "ward-turn-deadline", "a", 0), baseLimits); !errors.Is(err, store.ErrConclavePaused) {
			t.Fatalf("turn deadline trip = %v", err)
		}
		if got := countRoomAudits(t, st, "conclave.ward", "ward-turn-deadline"); len(got) != 1 || !strings.Contains(got[0].DetailJSON, `"ward":"wall_time_reached"`) {
			t.Fatalf("turn deadline ward audits = %+v", got)
		}
	})

	// A plain room pauses and audits a turn Ward the same way a council does (ADR-0008).
	t.Run("turn_plain_room_wards", func(t *testing.T) {
		noBudget := baseLimits
		noBudget.BudgetMissing = true
		oneMessage := baseLimits
		oneMessage.MaxMessages = 1
		for _, tc := range []struct {
			room, ward string
			limits     store.ConclaveLimits
		}{
			{"ward-turn-budget", "budget_evidence_unavailable", noBudget},
			{"ward-turn-messages", "max_messages_reached(1)", oneMessage},
		} {
			mkRoom(tc.room, 0)
			if _, _, _, err := store.AppendConclavePost(ctx, st, domain.ConclaveMessage{
				ConclaveID: tc.room, FromRole: "a", ToRole: "all", Kind: "proposal", Body: "hi", CreatedAt: now,
			}, baseLimits, false, false); err != nil {
				t.Fatal(err)
			}
			for _, role := range []string{"b", "c"} {
				if err := submitTurn(tc.room, turnTask(t, tc.room, role, 0), tc.limits); !errors.Is(err, store.ErrConclavePaused) {
					t.Fatalf("%s turn %s = %v", tc.room, role, err)
				}
			}
			if c := roomState(t, st, tc.room); c.State != domain.ConclaveStateInputRequired || c.WardReason != tc.ward {
				t.Fatalf("%s after the turn Ward = %+v", tc.room, c)
			}
			if got := countRoomAudits(t, st, "conclave.ward", tc.room); len(got) != 1 || !strings.Contains(got[0].DetailJSON, fmt.Sprintf(`"ward":%q`, tc.ward)) {
				t.Fatalf("%s ward audits = %+v", tc.room, got)
			}
		}
	})

	t.Run("claim_deadline", func(t *testing.T) {
		mkRoom("ward-claim-deadline", 0)
		claimA := turnTask(t, "ward-claim-deadline", "a", 0)
		claimB := turnTask(t, "ward-claim-deadline", "b", 0)
		if err := submitTurn("ward-claim-deadline", claimA, baseLimits); err != nil {
			t.Fatal(err)
		}
		if err := submitTurn("ward-claim-deadline", claimB, baseLimits); err != nil {
			t.Fatal(err)
		}
		later := func() time.Time { return now.Add(time.Hour) }
		if err := store.ClaimConclaveTask(ctx, st, claimA.ID, "ward-claim-deadline", later); !errors.Is(err, store.ErrConflict) {
			t.Fatalf("late claim A = %v", err)
		}
		if err := store.ClaimConclaveTask(ctx, st, claimB.ID, "ward-claim-deadline", later); !errors.Is(err, store.ErrConflict) {
			t.Fatalf("late claim B = %v", err)
		}
		if got := countRoomAudits(t, st, "conclave.ward", "ward-claim-deadline"); len(got) != 1 || !strings.Contains(got[0].DetailJSON, `"ward":"wall_time_reached"`) {
			t.Fatalf("claim deadline ward audits = %+v", got)
		}
	})

	t.Run("update_state_pause", func(t *testing.T) {
		mkRoom("ward-update-state", 0)
		if err := st.UpdateConclaveState(ctx, "ward-update-state", domain.ConclaveStateInputRequired, "manual_ward"); err != nil {
			t.Fatalf("first pause: %v", err)
		}
		if err := st.UpdateConclaveState(ctx, "ward-update-state", domain.ConclaveStateInputRequired, "manual_ward"); err != nil {
			t.Fatalf("duplicate pause: %v", err)
		}
		if got := countRoomAudits(t, st, "conclave.ward", "ward-update-state"); len(got) != 1 || !strings.Contains(got[0].DetailJSON, `"ward":"manual_ward"`) {
			t.Fatalf("update state ward audits = %+v", got)
		}
	})

	if _, err := st.VerifyAuditChain(ctx, nil); err != nil {
		t.Fatalf("VerifyAuditChain: %v", err)
	}
}
