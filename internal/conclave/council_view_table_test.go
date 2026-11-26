package conclave

import (
	"errors"
	"testing"
	"time"

	"github.com/shadewright-labs/sablecourt/internal/domain"
	"github.com/shadewright-labs/sablecourt/internal/store"
)

func TestCouncilViewForAssignmentRefusals(t *testing.T) {
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	const taskID = "reader-task"
	base := func() store.CouncilSnapshot {
		return store.CouncilSnapshot{
			CouncilPolicy: store.CouncilPolicy{
				Members:   []store.CouncilMember{{Vassal: domain.ConclaveVassal{Role: "reader", Workload: "worker"}, Zone: domain.ZoneSandbox}},
				Question:  "Review?",
				MaxRounds: 1,
				MaxStarts: 2,
				Deadline:  now.Add(time.Hour),
			},
			Session: domain.ConclaveSession{ID: "room", State: domain.ConclaveStateActive, CreatedAt: now.Add(-time.Minute)},
			Turns:   []store.CouncilTurn{{Role: "reader", Round: 1, Starts: 1, TaskID: taskID, State: domain.TaskStateWorking}},
		}
	}
	assignment := store.ConclaveAssignment{ConclaveID: "room", Role: "reader", Round: 1, Attempt: 1}

	cases := []struct {
		name     string
		mutate   func(*store.CouncilSnapshot, *store.ConclaveAssignment) (workload string, at time.Time)
		denied   bool // ErrMessageDenied: the caller may not read
		invalid  bool // another error: the snapshot itself is inconsistent
		wantView bool
	}{
		{"current working turn reads", func(*store.CouncilSnapshot, *store.ConclaveAssignment) (string, time.Time) { return "worker", now }, false, false, true},
		{"input required turn reads", func(s *store.CouncilSnapshot, _ *store.ConclaveAssignment) (string, time.Time) {
			s.Turns[0].State = domain.TaskStateInputRequired
			return "worker", now
		}, false, false, true},
		{"missing session id", func(s *store.CouncilSnapshot, _ *store.ConclaveAssignment) (string, time.Time) {
			s.Session.ID = ""
			return "worker", now
		}, false, true, false},
		{"clock before creation", func(*store.CouncilSnapshot, *store.ConclaveAssignment) (string, time.Time) {
			return "worker", now.Add(-time.Hour)
		}, false, true, false},
		{"deadline not after creation", func(s *store.CouncilSnapshot, _ *store.ConclaveAssignment) (string, time.Time) {
			s.Deadline = s.Session.CreatedAt
			return "worker", now
		}, false, true, false},
		{"active with a ward", func(s *store.CouncilSnapshot, _ *store.ConclaveAssignment) (string, time.Time) {
			s.Session.WardReason = "budget"
			return "worker", now
		}, false, true, false},
		{"paused without a ward", func(s *store.CouncilSnapshot, _ *store.ConclaveAssignment) (string, time.Time) {
			s.Session.State = domain.ConclaveStateInputRequired
			return "worker", now
		}, false, true, false},
		{"paused by a ward", func(s *store.CouncilSnapshot, _ *store.ConclaveAssignment) (string, time.Time) {
			s.Session.State = domain.ConclaveStateInputRequired
			s.Session.WardReason = "budget"
			return "worker", now
		}, true, false, false},
		{"completed", func(s *store.CouncilSnapshot, _ *store.ConclaveAssignment) (string, time.Time) {
			s.Session.State = domain.ConclaveStateCompleted
			return "worker", now
		}, true, false, false},
		{"cancelled", func(s *store.CouncilSnapshot, _ *store.ConclaveAssignment) (string, time.Time) {
			s.Session.State = domain.ConclaveStateCancelled
			return "worker", now
		}, true, false, false},
		{"unknown state", func(s *store.CouncilSnapshot, _ *store.ConclaveAssignment) (string, time.Time) {
			s.Session.State = domain.ConclaveState("frozen")
			return "worker", now
		}, false, true, false},
		{"at the deadline", func(s *store.CouncilSnapshot, _ *store.ConclaveAssignment) (string, time.Time) {
			return "worker", s.Deadline
		}, true, false, false},
		{"assignment for another room", func(_ *store.CouncilSnapshot, a *store.ConclaveAssignment) (string, time.Time) {
			a.ConclaveID = "other"
			return "worker", now
		}, true, false, false},
		{"round zero assignment", func(_ *store.CouncilSnapshot, a *store.ConclaveAssignment) (string, time.Time) {
			a.Round = 0
			return "worker", now
		}, true, false, false},
		{"workload is not the member", func(*store.CouncilSnapshot, *store.ConclaveAssignment) (string, time.Time) {
			return "intruder", now
		}, true, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			snap, a := base(), assignment
			workload, at := tc.mutate(&snap, &a)
			view, err := councilViewForAssignment(snap, a, workload, taskID, at)
			switch {
			case tc.wantView:
				if err != nil || view == nil {
					t.Fatalf("view = %v, %v; want a view", view, err)
				}
			case tc.denied:
				if view != nil || !errors.Is(err, ErrMessageDenied) {
					t.Fatalf("view = %v, %v; want ErrMessageDenied", view, err)
				}
			case tc.invalid:
				if view != nil || err == nil || errors.Is(err, ErrMessageDenied) {
					t.Fatalf("view = %v, %v; want an inconsistency error", view, err)
				}
			}
		})
	}
}
