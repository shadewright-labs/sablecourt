package coding

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/shadewright-labs/sablecourt/internal/domain"
	scruntime "github.com/shadewright-labs/sablecourt/internal/runtime"
)

func TestObserveOrphanContainer(t *testing.T) {
	for _, tc := range []struct {
		name      string
		container *fakeContainerState // nil: container is gone
		state     scruntime.ObservedState
		exitCode  int
		notFound  bool
	}{
		{name: "gone", notFound: true},
		{name: "running", container: &fakeContainerState{running: true}, state: scruntime.StateRunning},
		{name: "exited cleanly", container: &fakeContainerState{exited: true}, state: scruntime.StateCompleted},
		{name: "exited with failure", container: &fakeContainerState{exited: true, exitCode: 3}, state: scruntime.StateFailed, exitCode: 3},
		{name: "created but never started", container: &fakeContainerState{}, notFound: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runner := newFakeCouncilRunner()
			if tc.container != nil {
				tc.container.id, tc.container.exists = "cid-1", true
				runner.containers["cid-1"] = tc.container
			}
			a := NewAdapter(t.TempDir())
			a.CouncilRunner = runner
			obs, err := a.observeOrphanContainer(context.Background(), scruntime.Handle{ContainerID: "cid-1"})
			if tc.notFound {
				if !errors.Is(err, scruntime.ErrProcessNotFound) {
					t.Fatalf("observeOrphanContainer = %+v, %v; want ErrProcessNotFound", obs, err)
				}
				return
			}
			if err != nil || obs.State != tc.state || obs.ExitCode != tc.exitCode {
				t.Fatalf("observeOrphanContainer = %+v, %v; want %s exit %d", obs, err, tc.state, tc.exitCode)
			}
			if tc.exitCode != 0 && !strings.Contains(obs.Message, "status 3") {
				t.Errorf("failure message = %q", obs.Message)
			}
		})
	}
	t.Run("runner without inspection", func(t *testing.T) {
		a := NewAdapter(t.TempDir())
		if _, err := a.observeOrphanContainer(context.Background(), scruntime.Handle{ContainerID: "cid-1"}); !errors.Is(err, scruntime.ErrProcessNotFound) {
			t.Fatalf("observeOrphanContainer without inspector = %v", err)
		}
	})
}

// A daemon restart loses the private gate listener of a council container,
// so a fresh adapter must not report the container alive even while the
// engine still runs it.
func TestContainerHandleProbeAcrossRestart(t *testing.T) {
	repo := initTempGitRepo(t)
	wtRoot := filepath.Join(t.TempDir(), "worktrees")
	runner := newFakeCouncilRunner()
	runner.blockCh = make(chan struct{})
	unblocked := false
	unblock := func() {
		if !unblocked {
			unblocked = true
			close(runner.blockCh)
		}
	}
	t.Cleanup(unblock)
	adapter := NewAdapter(wtRoot)
	adapter.CouncilRunner = runner
	adapter.CouncilProxy = newFakeProxyDocker()
	adapter.CouncilProxyImage = "sha256:" + strings.Repeat("c", 64)
	adapter.DaemonListen = "[::1]:7432"
	raw, err := json.Marshal(map[string]string{
		"conclaveId": "ccl_probe", "role": "claude", "context_packet": "Review.", "checkoutMode": checkoutModeLive,
	})
	if err != nil {
		t.Fatal(err)
	}
	spec := scruntime.LaunchSpec{
		TaskID: "task_probe",
		Workload: domain.Workload{Metadata: domain.ObjectMeta{Name: "claude", Zone: domain.ZoneSandbox}, Spec: domain.WorkloadSpec{
			Class: domain.ClassCodingAgent, Coding: &domain.CodingConfig{Harness: "claude", RepoPath: repo, Sandbox: "seatbelt"},
		}},
		InputJSON:        string(raw),
		TaskToken:        "sc_task_probe_lease",
		CouncilContainer: true,
		CouncilDeadline:  time.Now().Add(2 * time.Minute),
	}
	handle, err := adapter.Start(t.Context(), spec)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	restarted := NewAdapter(wtRoot)
	restarted.CouncilRunner = runner
	ctx := context.Background()
	probe := func(a *Adapter, h scruntime.Handle, exit bool) bool {
		t.Helper()
		var got bool
		var err error
		if exit {
			got, err = a.HandleExited(ctx, h)
		} else {
			got, err = a.HandleAlive(ctx, h)
		}
		if err != nil {
			t.Fatalf("probe(exit=%t) = %v", exit, err)
		}
		return got
	}
	if !probe(adapter, handle, false) {
		t.Error("running container reported dead by its own adapter")
	}
	if probe(restarted, handle, false) {
		t.Error("restarted adapter reported a container alive without its gate")
	}
	if probe(adapter, handle, true) || probe(restarted, handle, true) {
		t.Error("running container reported exited")
	}
	foreign := handle
	foreign.LogPath = filepath.Join(t.TempDir(), "other.log")
	if probe(adapter, foreign, false) {
		t.Error("handle with a foreign log path reported alive")
	}
	anonymous := handle
	anonymous.ExternalID = ""
	if probe(adapter, anonymous, false) {
		t.Error("handle without a task ID reported alive")
	}

	unblock()
	taskBase, err := adapter.taskBase(spec.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	<-adapter.containerProc(taskBase).done
	if !probe(restarted, handle, true) {
		t.Error("restarted adapter did not see the recorded exit")
	}
	if probe(adapter, handle, false) {
		t.Error("finished container reported alive")
	}
	if err := adapter.Stop(ctx, handle); err != nil {
		t.Fatal(err)
	}
}
