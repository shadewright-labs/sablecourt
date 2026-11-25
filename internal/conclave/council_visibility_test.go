package conclave

import (
	"errors"
	"testing"

	"github.com/shadewright-labs/sablecourt/internal/domain"
	"github.com/shadewright-labs/sablecourt/internal/store"
)

// Authentication can succeed just before the turn finishes or is superseded.
func TestCouncilReadRequiresCurrentTurn(t *testing.T) {
	assignment := store.ConclaveAssignment{ConclaveID: "room", Role: "reader", Round: 1, Attempt: 1}
	const taskID = "reader-task"
	for _, tc := range []struct {
		name    string
		task    string
		state   domain.TaskState
		attempt int
	}{
		{"completed", taskID, domain.TaskStateCompleted, 1},
		{"failed", taskID, domain.TaskStateFailed, 1},
		{"cancelled", taskID, domain.TaskStateCancelled, 1},
		{"superseded", "retry", domain.TaskStateWorking, 2},
		{"missing", "", domain.TaskStateWorking, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			snap := store.CouncilSnapshot{
				CouncilPolicy: store.CouncilPolicy{Members: []store.CouncilMember{{Vassal: domain.ConclaveVassal{Role: "reader", Workload: "worker"}, Zone: domain.ZoneSandbox}}, Question: "Review?", MaxRounds: 1, MaxStarts: 2},
				Session:       domain.ConclaveSession{ID: "room"}, Turns: []store.CouncilTurn{},
			}
			if tc.task != "" {
				snap.Turns = append(snap.Turns, store.CouncilTurn{Role: "reader", Round: 1, Starts: tc.attempt, TaskID: tc.task, State: tc.state})
			}
			view, err := councilViewForAssignment(snap, assignment, "worker", taskID)
			if view != nil || !errors.Is(err, ErrMessageDenied) {
				t.Fatal("inactive or unreserved reader received a view")
			}
		})
	}
}
