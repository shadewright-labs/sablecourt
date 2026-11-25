package controlplane

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/shadewright-labs/sablecourt/internal/domain"
	scruntime "github.com/shadewright-labs/sablecourt/internal/runtime"
)

type unreadInputAdapter struct{ fakeAdapter }

func (*unreadInputAdapter) Start(context.Context, scruntime.LaunchSpec) (scruntime.Handle, error) {
	return scruntime.Handle{}, &scruntime.InputDeliveryError{Err: errors.New("the process did not read its input before the launch deadline")}
}

// TestUnreadTaskInputFailsWithItsOwnReason checks that a workload which does
// not read its input fails with a reason that says so, not a generic launch
// failure.
func TestUnreadTaskInputFailsWithItsOwnReason(t *testing.T) {
	ctx := t.Context()
	r, st, _, task := regressionFixture(t)
	wl := r.bundle.Workloads["worker"]
	if err := launchForTest(ctx, r, task, wl, &unreadInputAdapter{}); err == nil {
		t.Fatal("launch succeeded")
	}
	requireState(t, st, task.ID, domain.TaskStateFailed)
	events, err := st.ListTaskEvents(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	last := events[len(events)-1]
	if last.Reason != "input_not_delivered" || !strings.Contains(last.FactJSON, `"input_not_delivered"`) {
		t.Fatalf("last event = %s %s, want reason input_not_delivered", last.Reason, last.FactJSON)
	}
}
