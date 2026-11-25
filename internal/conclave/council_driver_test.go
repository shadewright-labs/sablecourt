package conclave

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/shadewright-labs/sablecourt/internal/config"
	"github.com/shadewright-labs/sablecourt/internal/controlplane"
	"github.com/shadewright-labs/sablecourt/internal/domain"
	"github.com/shadewright-labs/sablecourt/internal/gate/model"
	rt "github.com/shadewright-labs/sablecourt/internal/runtime"
	"github.com/shadewright-labs/sablecourt/internal/store"
)

type councilStub struct {
	rt.Adapter
	mu        sync.Mutex
	starts    map[string]string
	answers   map[string]string
	unknown   bool
	adoptions int
	observed  rt.ObservedState
	stop      func(rt.Handle) error
}

func (s *councilStub) Start(_ context.Context, spec rt.LaunchSpec) (rt.Handle, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.starts[spec.TaskID]; ok {
		return rt.Handle{}, errors.New("duplicate start")
	}
	s.starts[spec.TaskID] = spec.TaskToken
	return rt.Handle{AdapterName: "stub", ExternalID: spec.TaskID, PID: 1, ProcessStartTime: time.Now().UTC()}, nil
}
func (s *councilStub) HandleAlive(_ context.Context, h rt.Handle) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.adoptions++
	if s.unknown {
		return false, rt.ErrProcessUnverified
	}
	_, ok := s.starts[h.ExternalID]
	return ok, nil
}
func (s *councilStub) Observe(_ context.Context, h rt.Handle) (rt.Observation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	state := rt.StateRunning
	if _, ok := s.answers[h.ExternalID]; ok {
		state = rt.StateCompleted
	}
	if s.observed != "" {
		state = s.observed
	}
	return rt.Observation{State: state, ObservedAt: time.Now().UTC()}, nil
}
func (s *councilStub) Stop(_ context.Context, h rt.Handle) error {
	if s.stop != nil {
		return s.stop(h)
	}
	return nil
}
func (s *councilStub) CollectArtifacts(_ context.Context, h rt.Handle) ([]rt.Artifact, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return []rt.Artifact{{Name: "final-message", ContentType: "text/plain", Text: s.answers[h.ExternalID]}}, nil
}

type driverFixture struct {
	store.Store
	store.CouncilStore
	*Broker
	rec    *controlplane.Reconciler
	bundle *config.Bundle
	stub   *councilStub
	clock  time.Time
}

func driverRoom(t *testing.T, sequential bool, maxStarts, maxRuns int) *driverFixture {
	t.Helper()
	return driverRoomWithCheckout(t, sequential, maxStarts, maxRuns, store.CouncilCheckout{})
}

func driverRoomWithCheckout(t *testing.T, sequential bool, maxStarts, maxRuns int, checkout store.CouncilCheckout) *driverFixture {
	t.Helper()
	st := brokerTestStore(t)
	cs, err := store.Councils(st)
	if err != nil {
		t.Fatal(err)
	}
	f := &driverFixture{Store: st, CouncilStore: cs, clock: time.Now().UTC(), stub: &councilStub{starts: map[string]string{}, answers: map[string]string{}}}
	roles := []domain.ConclaveVassal{}
	spec := store.CouncilSpec{Question: "Which change should land?", Sequential: sequential, Checkout: checkout, MaxStarts: &maxStarts, Wards: domain.ConclaveWards{MaxRuns: maxRuns}}
	for i, name := range []string{"north", "south", "west"} {
		v := domain.ConclaveVassal{Role: name, Workload: "worker", Activation: domain.ActivationOnMention, CanMessage: []string{"all"}}
		if i == 0 {
			v.Activation = domain.ActivationAlwaysOn
		}
		roles = append(roles, v)
		spec.Members = append(spec.Members, store.CouncilMember{Vassal: v, Zone: domain.ZoneSandbox})
	}
	f.bundle = limitsBundle(domain.ConclaveWards{}, roles...)
	w := f.bundle.Workloads["worker"]
	w.Metadata.Zone = domain.ZoneSandbox
	w.Spec.Class = domain.ClassLongLivedAgent
	f.bundle.Workloads["worker"] = w
	if err := st.UpsertWorkload(t.Context(), domain.WorkloadRecord{Name: "worker", Zone: w.Metadata.Zone, Class: w.Spec.Class}); err != nil {
		t.Fatal(err)
	}
	if err := cs.CreateCouncil(t.Context(), domain.ConclaveSession{ID: "room", State: domain.ConclaveStateActive, TemplateName: "council", CreatedAt: f.clock}, spec); err != nil {
		t.Fatal(err)
	}
	f.restart()
	return f
}
func (f *driverFixture) restart() {
	f.rec = controlplane.NewReconciler(controlplane.Options{Store: f.Store, Bundle: f.bundle, Adapters: map[domain.WorkloadClass]rt.Adapter{domain.ClassLongLivedAgent: f.stub}})
	f.Broker = NewBroker(Options{Store: f.Store, Reconciler: f.rec, GetBundle: func() *config.Bundle { return f.bundle }, Now: func() time.Time { return f.clock }})
}

func (f *driverFixture) pass(t *testing.T) {
	t.Helper()
	if err := f.ReconcileCouncils(t.Context(), true); err != nil {
		t.Fatal(err)
	}
}
func (f *driverFixture) snapshot(t *testing.T, want int) store.CouncilSnapshot {
	t.Helper()
	s, err := f.CouncilSnapshot(t.Context(), "room")
	if err != nil || len(s.Attempts) != want {
		t.Fatalf("got %d attempts, want %d: %v", len(s.Attempts), want, err)
	}
	return s
}
func (f *driverFixture) launch(t *testing.T, want int) {
	t.Helper()
	if err := f.rec.ReconcileOnce(t.Context()); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		ready := 0
		for _, a := range f.snapshot(t, want).Attempts {
			task, err := f.GetTask(t.Context(), a.TaskID)
			if err != nil {
				t.Fatal(err)
			}
			if task.LastHeartbeatAt != nil {
				ready++
			}
		}
		if ready == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("launched %d of %d", ready, want)
		}
		time.Sleep(time.Millisecond)
	}
}
func (f *driverFixture) answer(t *testing.T, id, body string) {
	t.Helper()
	f.stub.mu.Lock()
	f.stub.answers[id] = body
	f.stub.mu.Unlock()
	if err := f.rec.ReconcileOnce(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func TestCouncilDriverBarrier(t *testing.T) {
	for _, sequential := range []bool{false, true} {
		t.Run(fmt.Sprintf("sequential=%t", sequential), func(t *testing.T) {
			f := driverRoom(t, sequential, 6, 6)
			count := 3
			if sequential {
				count = 1
			}
			for i := 0; i < 3; i++ {
				f.pass(t)
				f.pass(t)
				f.launch(t, count)
				s := f.snapshot(t, count)
				f.answer(t, s.Attempts[i].TaskID, "answer from "+s.Attempts[i].Role)
				if sequential {
					count++
				}
			}
			f.pass(t)
			f.pass(t)
			reviews := 3
			if sequential {
				reviews = 1
			}
			s := f.snapshot(t, 3+reviews)
			for _, a := range s.Attempts[3:] {
				task, err := f.GetTask(t.Context(), a.TaskID)
				var input struct {
					Packet string `json:"context_packet"`
				}
				if err != nil || json.Unmarshal([]byte(task.InputJSON), &input) != nil || !strings.Contains(input.Packet, "answer from") || a.Round != 2 || a.Number != 1 {
					t.Fatal("missing reserved review packet", err)
				}
			}
		})
	}
}

func TestCouncilDriverLegacyEntrypoints(t *testing.T) {
	for _, entry := range []string{"activation", "mention", "decree", "retry", "handoff"} {
		t.Run(entry, func(t *testing.T) {
			f := driverRoom(t, false, 6, 6)
			ctx := t.Context()
			room := f.snapshot(t, 0).Session
			tpl := f.bundle.Conclaves[room.TemplateName]
			var err error
			switch entry {
			case "activation":
				err = f.ActivateAlwaysOn(ctx, room, tpl, "opening")
			case "mention":
				err = f.WakeMentions(ctx, tpl, domain.ConclaveMessage{ConclaveID: room.ID, ID: 1, FromRole: "north", ToRole: "south"})
			case "decree":
				_, err = f.PostMessage(ctx, room.ID, "operator", "all", "decree", "look again", "")
			default:
				task := reserveResult(t, f.Store, room.ID, "north", 1, 1)
				finishResult(t, f.Store, task, domain.TaskStateFailed, `{"exit_code":1}`, "[]")
				_, err = f.rec.RetryTaskWithHandoff(ctx, task.ID, entry == "handoff", "manual")
				if !errors.Is(err, store.ErrConflict) {
					t.Fatalf("want council driver conflict, got %v", err)
				}
				f.snapshot(t, 1)
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			f.snapshot(t, 0)
		})
	}
}

func TestCouncilDriverStop(t *testing.T) {
	for _, reason := range []string{"owner_stop", "owner_close", "deadline", "nothing_to_add", "silence"} {
		t.Run(reason, func(t *testing.T) {
			f := driverRoom(t, false, 6, 6)
			f.pass(t)
			f.launch(t, 3)
			s := f.snapshot(t, 3)
			f.answer(t, s.Attempts[0].TaskID, CouncilNothingToAdd)
			switch reason {
			case "owner_stop", "owner_close":
				state := domain.ConclaveStateCancelled
				if reason == "owner_close" {
					state = domain.ConclaveStateCompleted
				}
				if err := f.Broker.Close(t.Context(), "room", state); err != nil {
					t.Fatal(err)
				}
			case "deadline":
				f.clock = s.Deadline
			default:
				f.answer(t, s.Attempts[1].TaskID, CouncilNothingToAdd)
				body := CouncilNothingToAdd
				if reason == "silence" {
					body = ""
				}
				f.answer(t, s.Attempts[2].TaskID, body)
			}
			f.pass(t)
			f.pass(t)
			if reason == "silence" {
				s = f.snapshot(t, 4)
				if s.Turns[2].Answer != nil || s.Attempts[3].Round != 1 || s.Attempts[3].Number != 2 {
					t.Fatal("silence passed the round barrier")
				}
				return
			}
			s = f.snapshot(t, 3)
			if s.Session.WardReason != reason || s.Turns[0].Answer == nil {
				t.Fatalf("lost reason or partial answer: %+v", s.Session)
			}
			for _, a := range s.Attempts {
				if a.State != domain.TaskStateCompleted && a.State != domain.TaskStateCancelled {
					t.Fatalf("still active: %+v", a)
				}
				f.stub.mu.Lock()
				token := f.stub.starts[a.TaskID]
				f.stub.mu.Unlock()
				if _, err := f.GetActiveLeaseByTokenHash(t.Context(), model.HashToken(token), time.Now()); !errors.Is(err, store.ErrNotFound) {
					t.Fatalf("lease still valid: %v", err)
				}
			}
			d, err := f.GetConclaveRelic(t.Context(), "room", CouncilDigestRelicName)
			if err != nil || !strings.Contains(d.Body, CouncilNothingToAdd) || !strings.Contains(d.Body, reason) {
				t.Fatal("missing digest", err)
			}
		})
	}
}

func TestCouncilDriverRetry(t *testing.T) {
	for _, tc := range []struct {
		name            string
		cap, ward, want int
	}{
		{"retry", 6, 6, 2}, {"start cap", 1, 6, 1}, {"run Ward", 6, 1, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := driverRoom(t, true, tc.cap, tc.ward)
			task := reserveResult(t, f.Store, "room", "north", 1, 1)
			if err := f.UpdateTaskState(t.Context(), task.ID, task.State, domain.TaskStateFailed, "launch_failed", "{}"); err != nil {
				t.Fatal(err)
			}
			f.pass(t)
			s := f.snapshot(t, tc.want)
			if tc.want == 2 && s.Attempts[1].Number != 2 || tc.cap == 1 && s.Session.WardReason != "start_cap" || tc.ward == 1 && s.Session.WardReason != store.ConclaveMaxRunsWard {
				t.Fatalf("unexpected retry decision: %+v", s)
			}
		})
	}
}

func TestCouncilDriverRestart(t *testing.T) {
	for _, unknown := range []bool{false, true} {
		t.Run(fmt.Sprintf("unknown=%t", unknown), func(t *testing.T) {
			f := driverRoom(t, false, 6, 6)
			f.pass(t)
			f.launch(t, 3)
			s := f.snapshot(t, 3)
			for _, a := range s.Attempts[:2] {
				f.answer(t, a.TaskID, "completed before restart")
			}
			f.restart()
			f.stub.mu.Lock()
			f.stub.unknown = unknown
			f.stub.mu.Unlock()
			err := f.rec.Recover(t.Context())
			if unknown != errors.Is(err, rt.ErrProcessUnverified) || !unknown && err != nil {
				t.Fatal("recovery", err)
			}
			for range 2 {
				f.pass(t)
			}
			s = f.snapshot(t, 3)
			msgs, err := f.ListConclaveMessages(t.Context(), "room", 0)
			if err != nil || len(msgs) != 2 || s.Attempts[2].State != domain.TaskStateWorking {
				t.Fatal("restart lost a worker or answer", err)
			}
			f.stub.mu.Lock()
			calls := f.stub.adoptions
			f.stub.unknown = false
			f.stub.mu.Unlock()
			if calls == 0 {
				t.Fatal("live task was never inspected")
			}
			f.answer(t, s.Attempts[2].TaskID, "released")
			f.pass(t)
			f.snapshot(t, 6)
		})
	}
}

func TestCouncilDriverUnknownObservation(t *testing.T) {
	for _, state := range []rt.ObservedState{rt.StateUnknown, rt.ObservedState("unexpected")} {
		t.Run(string(state), func(t *testing.T) {
			f := driverRoom(t, true, 6, 6)
			f.pass(t)
			f.launch(t, 1)
			f.stub.mu.Lock()
			f.stub.observed = state
			f.stub.mu.Unlock()
			for range 4 {
				_ = f.rec.ReconcileOnce(t.Context())
				f.pass(t)
			}
			s := f.snapshot(t, 1)
			if s.Turns[0].State != domain.TaskStateWorking {
				t.Fatal("unknown attempt replaced")
			}
			f.stub.mu.Lock()
			token := f.stub.starts[s.Turns[0].TaskID]
			f.stub.observed = ""
			f.stub.mu.Unlock()
			if _, err := f.GetActiveLeaseByTokenHash(t.Context(), model.HashToken(token), time.Now()); !errors.Is(err, store.ErrNotFound) {
				t.Fatal("unknown runtime retained lease", err)
			}
			f.answer(t, s.Turns[0].TaskID, "answer")
			f.pass(t)
			f.snapshot(t, 2)
		})
	}
}

func TestCouncilDriverArtifactRedaction(t *testing.T) {
	for _, footer := range []string{"", "-----END EC PRIVATE KEY-----"} {
		t.Run("footer="+footer, func(t *testing.T) {
			f := driverRoom(t, true, 6, 6)
			f.pass(t)
			f.launch(t, 1)
			taskID := f.snapshot(t, 1).Turns[0].TaskID
			f.answer(t, taskID, "-----BEGIN RSA PRIVATE KEY-----\nfixture-before\n"+footer+"\nfixture-after")
			task, err := f.GetTask(t.Context(), taskID)
			if err != nil || strings.Contains(task.ArtifactsJSON, "fixture-") {
				t.Fatal("unredacted key persisted", err)
			}
			f.pass(t)
			s := f.snapshot(t, 2)
			if s.Turns[0].Answer == nil || strings.Contains(s.Turns[0].Answer.Body, "fixture-") {
				t.Fatal("unredacted council answer")
			}
		})
	}
}

func TestCouncilDriverCurrentWards(t *testing.T) {
	for _, ward := range []string{"missing budget", "run ceiling"} {
		t.Run(ward, func(t *testing.T) {
			f := driverRoom(t, false, 6, 6)
			tpl := f.bundle.Conclaves["council"]
			want := 0
			if ward == "missing budget" {
				tpl.Spec.BudgetRef = "absent"
			} else {
				tpl.Spec.Wards.MaxRuns = 1
				want = 1
			}
			f.bundle.Conclaves["council"] = tpl
			f.pass(t)
			if s := f.snapshot(t, want); s.Session.State != domain.ConclaveStateInputRequired {
				t.Fatal("current Ward bypassed")
			}
		})
	}
}

func TestCouncilDriverDeadlineOnReadFailure(t *testing.T) {
	for _, closed := range []bool{false, true} {
		t.Run(fmt.Sprintf("owner=%t", closed), func(t *testing.T) {
			f := driverRoom(t, false, 6, 6)
			f.pass(t)
			f.launch(t, 3)
			s := f.snapshot(t, 3)
			f.answer(t, s.Turns[0].TaskID, "partial")
			f.Broker.store = resultLinkFailure{f.Store, f.CouncilStore}
			f.clock = s.Deadline
			var err error
			if closed {
				err = f.Broker.Close(t.Context(), "room", domain.ConclaveStateCancelled)
			} else {
				err = f.ReconcileCouncils(t.Context(), true)
			}
			if !errors.Is(err, errResultLink) {
				t.Fatal("lost storage failure", err)
			}
			s = f.snapshot(t, 3)
			if s.Turns[1].State != domain.TaskStateCancelled || s.Turns[2].State != domain.TaskStateCancelled {
				t.Fatal("active workers escaped cancellation")
			}
			f.Broker.store = f.Store
			if closed {
				if err := f.Broker.Close(t.Context(), "room", domain.ConclaveStateCancelled); err != nil {
					t.Fatal(err)
				}
			} else {
				f.pass(t)
			}
			s = f.snapshot(t, 3)
			if !councilClosed(&s) || (s.Turns[0].Answer == nil) != closed {
				t.Fatal("unexpected terminal evidence after retry")
			}
			if closed && !strings.Contains(s.Session.WardReason, errResultLink.Error()) {
				t.Fatalf("missing stop detail: %q", s.Session.WardReason)
			}
		})
	}
}

func TestCouncilDriverOwnerCloseOnAnswerFailure(t *testing.T) {
	for _, tc := range []struct {
		state domain.ConclaveState
		cause error
	}{
		{domain.ConclaveStateCancelled, errResultLink},
		{domain.ConclaveStateCompleted, store.ErrConflict},
	} {
		t.Run(string(tc.state), func(t *testing.T) {
			f := driverRoom(t, false, 6, 6)
			f.pass(t)
			f.launch(t, 3)
			s := f.snapshot(t, 3)
			f.answer(t, s.Turns[0].TaskID, "partial")
			f.Broker.store = &interruptedCouncilPost{f.Store, f.CouncilStore, tc.cause}
			if !f.clock.Before(s.Deadline) {
				t.Fatal("fixture must close before the deadline")
			}
			if err := f.Broker.Close(t.Context(), "room", tc.state); !errors.Is(err, tc.cause) {
				t.Fatalf("close error = %v, want %v", err, tc.cause)
			}
			s = f.snapshot(t, 3)
			if s.Session.State != tc.state || !strings.Contains(s.Session.WardReason, tc.cause.Error()) {
				t.Errorf("close state = %s, detail = %q", s.Session.State, s.Session.WardReason)
			}
			digest, err := f.GetConclaveRelic(t.Context(), "room", CouncilDigestRelicName)
			if err != nil || !strings.Contains(digest.Body, tc.cause.Error()) {
				t.Errorf("missing digest stop detail: %v", err)
			}
			for _, a := range s.Attempts[1:] {
				if a.State != domain.TaskStateCancelled {
					t.Errorf("task state = %s, want cancelled", a.State)
				}
				if _, err := f.GetActiveLeaseByTokenHash(t.Context(), model.HashToken(f.stub.starts[a.TaskID]), time.Now()); !errors.Is(err, store.ErrNotFound) {
					t.Errorf("lease lookup = %v, want not found", err)
				}
			}
		})
	}
}

func TestCouncilDriverCurrentAnswerWards(t *testing.T) {
	f := driverRoom(t, false, 6, 6)
	f.pass(t)
	f.launch(t, 3)
	for _, a := range f.snapshot(t, 3).Attempts {
		f.answer(t, a.TaskID, "completed answer")
	}
	tpl := f.bundle.Conclaves["council"]
	tpl.Spec.Wards.MaxMessages = 1
	f.bundle.Conclaves["council"] = tpl
	f.pass(t)
	s := f.snapshot(t, 3)
	messages, err := f.ListConclaveMessages(t.Context(), "room", 0)
	reviews := slices.DeleteFunc(messages, func(msg domain.ConclaveMessage) bool { return msg.Kind != "review" })
	if err != nil || len(reviews) != 1 || s.Turns[0].Answer == nil || s.Turns[1].Answer != nil || s.Turns[2].Answer != nil ||
		s.Session.State != domain.ConclaveStateInputRequired || s.Session.WardReason != "max_messages_reached(1)" {
		t.Fatalf("reviews = %d, state = %s, ward = %q, err = %v", len(reviews), s.Session.State, s.Session.WardReason, err)
	}
}

type councilSnapshotReads struct {
	store.Store
	store.CouncilStore
	ids []string
}

func (s *councilSnapshotReads) CouncilSnapshot(ctx context.Context, id string) (store.CouncilSnapshot, error) {
	s.ids = append(s.ids, id)
	return s.CouncilStore.CouncilSnapshot(ctx, id)
}

func TestCouncilDriverTickCandidates(t *testing.T) {
	ctx := t.Context()
	f := driverRoom(t, false, 6, 6)
	s := f.snapshot(t, 0)
	for _, id := range []string{"ordinary-open", "ordinary-closed", "closed-idle", "closed-pending"} {
		room := domain.ConclaveSession{ID: id, State: domain.ConclaveStateActive, TemplateName: "council", CreatedAt: f.clock}
		if strings.HasPrefix(id, "ordinary") {
			if err := f.CreateConclave(ctx, room); err != nil {
				t.Fatal(err)
			}
			if id == "ordinary-closed" {
				if err := f.UpdateConclaveState(ctx, id, domain.ConclaveStateCompleted, ""); err != nil {
					t.Fatal(err)
				}
			}
			continue
		}
		if err := f.CreateCouncil(ctx, room, store.CouncilSpec{Question: s.Question, Members: s.Members}); err != nil {
			t.Fatal(err)
		}
		if id == "closed-pending" {
			reserveResult(t, f.Store, id, "north", 1, 1)
		}
		if _, err := f.CouncilStore.CloseCouncil(ctx, id, domain.ConclaveStateCancelled, "owner_stop"); err != nil {
			t.Fatal(err)
		}
	}
	reads := &councilSnapshotReads{Store: f.Store, CouncilStore: f.CouncilStore}
	f.Broker.store = reads
	for _, want := range [][]string{{"closed-pending", "room"}, {"room"}} {
		reads.ids = nil
		if err := f.ReconcileCouncils(ctx, false); err != nil {
			t.Fatal(err)
		}
		slices.Sort(reads.ids)
		if !slices.Equal(reads.ids, want) {
			t.Errorf("snapshot reads = %v, want %v", reads.ids, want)
		}
	}
}

func TestCouncilDriverReservedCheckout(t *testing.T) {
	commit := strings.Repeat("a", 40)
	for _, tc := range []struct {
		mode     string
		checkout store.CouncilCheckout
	}{
		{"live", store.CouncilCheckout{Source: "project"}},
		{"ref", store.CouncilCheckout{Source: "project", Ref: "main", Commit: commit}},
		{"writer", store.CouncilCheckout{Source: "project", Ref: "main", Commit: commit, WriterRole: "north"}},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			f := driverRoomWithCheckout(t, false, 6, 6, tc.checkout)
			f.pass(t)
			for _, a := range f.snapshot(t, 3).Attempts {
				task, err := f.GetTask(t.Context(), a.TaskID)
				if err != nil {
					t.Fatal(err)
				}
				var input struct {
					Mode   string `json:"checkoutMode"`
					Ref    string `json:"checkoutRef"`
					Writer string `json:"checkoutWriter"`
				}
				if err := json.Unmarshal([]byte(task.InputJSON), &input); err != nil {
					t.Fatal(err)
				}
				if input.Mode != tc.mode || input.Ref != tc.checkout.Commit || input.Writer != tc.checkout.WriterRole {
					t.Errorf("role %s checkout input = %+v", a.Role, input)
				}
			}
		})
	}
}

func TestCouncilDriverPostBoundary(t *testing.T) {
	for _, tc := range []struct{ name, from, to, body string }{
		{"narrower route", "north", "south", "message"},
		{"truncated key", "operator", "all", "-----BEGIN PRIVATE KEY-----\nfixture-body"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := driverRoom(t, false, 6, 6)
			tpl := f.bundle.Conclaves["council"]
			tpl.Spec.Vassals[0].CanMessage = []string{"operator"}
			f.bundle.Conclaves["council"] = tpl
			msg, err := f.PostMessage(t.Context(), "room", tc.from, tc.to, "review", tc.body, "")
			if tc.from == "operator" {
				if err != nil || strings.Contains(msg.Body, "fixture-body") {
					t.Fatal("decree contains key material", err)
				}
			} else if !errors.Is(err, ErrMessageDenied) {
				t.Fatal("narrower route was bypassed", err)
			}
		})
	}
}

func TestCouncilDriverSlowStop(t *testing.T) {
	for _, reason := range []string{"owner", "deadline"} {
		t.Run(reason, func(t *testing.T) {
			f := driverRoom(t, false, 6, 6)
			f.pass(t)
			f.launch(t, 3)
			s := f.snapshot(t, 3)
			release := make(chan struct{})
			started := make(chan struct{})
			others := make(chan struct{}, 2)
			f.stub.stop = func(h rt.Handle) error {
				if h.ExternalID == s.Attempts[0].TaskID {
					close(started)
					<-release
				} else {
					others <- struct{}{}
				}
				return nil
			}
			done := make(chan error, 1)
			if reason == "deadline" {
				f.clock = s.Deadline
			}
			go func() {
				if reason == "deadline" {
					done <- f.ReconcileCouncils(t.Context(), true)
				} else {
					done <- f.Broker.Close(t.Context(), "room", domain.ConclaveStateCancelled)
				}
			}()
			defer func() {
				close(release)
				if err := <-done; err != nil {
					t.Error(err)
				}
			}()
			select {
			case <-started:
			case <-time.After(3 * time.Second):
				t.Fatal("first worker never stopped")
			}
			for range 2 {
				select {
				case <-others:
				case <-time.After(time.Second):
					t.Fatal("slow stop delayed the other cancellations")
				}
			}
		})
	}
}

func TestCouncilDriverStaleObservation(t *testing.T) {
	for _, state := range []rt.ObservedState{rt.StateRunning, rt.StateSuspended} {
		t.Run(string(state), func(t *testing.T) {
			f := driverRoom(t, true, 6, 6)
			f.pass(t)
			f.launch(t, 1)
			id := f.snapshot(t, 1).Attempts[0].TaskID
			f.stub.mu.Lock()
			f.stub.observed = state
			f.stub.mu.Unlock()
			if err := f.RecordHeartbeat(t.Context(), id, time.Now().Add(-time.Hour)); err != nil {
				t.Fatal(err)
			}
			if err := f.rec.ReconcileOnce(t.Context()); err != nil {
				t.Fatal(err)
			}
			task, err := f.GetTask(t.Context(), id)
			if err != nil || task.State != domain.TaskStateFailed {
				t.Fatal("stale live council worker retained", err)
			}
		})
	}
}

func TestCouncilDriverCloseDeniedAnswer(t *testing.T) {
	for _, reason := range []string{"owner", "deadline"} {
		t.Run(reason, func(t *testing.T) {
			st := brokerTestStore(t)
			cs, err := store.Councils(st)
			if err != nil {
				t.Fatal(err)
			}
			if err := st.UpsertWorkload(t.Context(), domain.WorkloadRecord{Name: "worker", Zone: domain.ZoneSandbox}); err != nil {
				t.Fatal(err)
			}
			now := time.Now().UTC()
			spec := store.CouncilSpec{Question: "Review?", Members: []store.CouncilMember{{Vassal: domain.ConclaveVassal{Role: "reader", Workload: "worker"}, Zone: domain.ZoneSandbox}}}
			if err := cs.CreateCouncil(t.Context(), domain.ConclaveSession{ID: "room", State: domain.ConclaveStateActive, CreatedAt: now}, spec); err != nil {
				t.Fatal(err)
			}
			task := reserveResult(t, st, "room", "reader", 1, 1)
			finishResult(t, st, task, domain.TaskStateCompleted, `{"exit_code":0}`, resultArtifact("restricted"))
			b := NewBroker(Options{Store: st, Now: func() time.Time { return now }})
			if reason == "owner" {
				err = b.Close(t.Context(), "room", domain.ConclaveStateCancelled)
			} else {
				now = now.Add(time.Hour)
				err = b.ReconcileCouncils(t.Context(), true)
			}
			if err != nil {
				t.Fatal(err)
			}
			s, err := cs.CouncilSnapshot(t.Context(), "room")
			if err != nil || !councilClosed(&s) || s.Turns[0].Answer != nil {
				t.Fatal("denied evidence prevented close or entered the digest", err)
			}
		})
	}
}

type interruptedCouncilPost struct {
	store.Store
	store.CouncilStore
	refusal error
}

func (s *interruptedCouncilPost) AppendConclaveMessage(ctx context.Context, msg domain.ConclaveMessage) (int64, error) {
	if s.refusal != nil {
		err := s.refusal
		s.refusal = nil
		return 0, err
	}
	return s.Store.AppendConclaveMessage(ctx, msg)
}

func TestCouncilDriverWardResumeDuringCollection(t *testing.T) {
	for _, refusal := range []error{ErrWardTripped, ErrConclavePaused} {
		t.Run(refusal.Error(), func(t *testing.T) {
			f := driverRoom(t, true, 6, 6)
			f.pass(t)
			f.launch(t, 1)
			first := f.snapshot(t, 1).Attempts[0]
			f.answer(t, first.TaskID, "completed answer")
			// The post saw a Ward. A concurrent Decree resumed the room before the next snapshot.
			f.Broker.store = &interruptedCouncilPost{f.Store, f.CouncilStore, refusal}
			for range 2 {
				err := f.ReconcileCouncils(t.Context(), true)
				if err != nil && !errors.Is(err, refusal) {
					t.Fatal(err)
				}
			}
			s := f.snapshot(t, 2)
			if s.Attempts[0].Answer == nil || s.Attempts[1].Role != "south" {
				t.Fatal("a refused post was treated as silence")
			}
		})
	}
}

func TestCouncilDriverStaleReservationAtCap(t *testing.T) {
	for _, tc := range []struct {
		name               string
		maxStarts, maxRuns int
	}{
		{"start ceiling", 2, 6},
		{"run ward", 6, 3},
		{"message ward", 6, 3},
		{"deadline", 6, 3},
		{"closed", 6, 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := driverRoom(t, false, tc.maxStarts, tc.maxRuns)
			f.pass(t)
			count := min(3, tc.maxStarts)
			s := f.snapshot(t, count)
			limits := store.ConclaveLimits{}
			switch tc.name {
			case "message ward":
				if _, err := f.PostMessage(t.Context(), "room", "north", "all", "review", "one post", ""); err != nil {
					t.Fatal(err)
				}
				limits.MaxMessages = 1
			case "deadline":
				f.clock = s.Deadline
			case "closed":
				if err := f.Broker.Close(t.Context(), "room", domain.ConclaveStateCancelled); err != nil {
					t.Fatal(err)
				}
			}
			before := f.snapshot(t, count)
			// Replay a proposal from another driver's older snapshot.
			input := turnInput{ConclaveAssignment: store.ConclaveAssignment{ConclaveID: "room", Role: "north", Round: 1, Attempt: 1}, Packet: "Review?"}
			task, err := f.submitTurn(t.Context(), s.Members[0].Vassal, input, limits)
			if err != nil || task.ID != s.Turns[0].TaskID {
				t.Fatalf("replay = %q, %v, want %q", task.ID, err, s.Turns[0].TaskID)
			}
			after := f.snapshot(t, count)
			if after.Session != before.Session {
				t.Fatalf("replay changed session from %+v to %+v", before.Session, after.Session)
			}
			audit, err := f.ListAudit(t.Context(), 100)
			if err != nil {
				t.Fatal(err)
			}
			for _, entry := range audit {
				if entry.Action == "conclave.ward" {
					t.Fatal("reported a Ward that did not trip")
				}
			}
		})
	}
}
