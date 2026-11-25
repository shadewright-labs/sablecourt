package controlplane

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/shadewright-labs/sablecourt/internal/contextpack"
	"github.com/shadewright-labs/sablecourt/internal/domain"
	"github.com/shadewright-labs/sablecourt/internal/store"
)

func TestCouncilPacketAtLaunch(t *testing.T) {
	const question = "Which recovery path works?"
	const decree = "Review the retry."
	secret := "sk-" + strings.Repeat("x", 32)
	packet, err := contextpack.AssembleCouncil("", question, []string{decree}, []contextpack.CouncilRound{
		{Number: 1, Answers: []contextpack.CouncilAnswer{{Role: "writer", Body: secret + strings.Repeat(" detail", 5000)}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		packet any
		valid  bool
	}{
		{"reserved", packet.Markdown, true},
		{"wrong type", true, false},
		{"null", nil, false},
		{"blank", "  ", false},
		{"missing", nil, false},
		{"oversize", strings.Repeat("p", (32<<10)+1), false},
		{"secret", "fixture " + secret, false},
		{"invalid UTF-8", "invalid-utf8-marker", false},
	} {
		for _, class := range []domain.WorkloadClass{domain.ClassCodingAgent, domain.ClassLongLivedAgent, domain.ClassDeterministicController} {
			t.Run(tc.name+"/"+string(class), func(t *testing.T) {
				ctx := t.Context()
				r, st, _, _ := regressionFixture(t)
				wl := r.bundle.Workloads["worker"]
				wl.Spec.Class = class
				if err := st.UpsertWorkload(ctx, domain.WorkloadRecord{Name: "worker", Zone: domain.ZoneSandbox, Class: wl.Spec.Class}); err != nil {
					t.Fatal(err)
				}
				room := domain.ConclaveSession{ID: "panel", State: domain.ConclaveStateActive}
				member := store.CouncilMember{Vassal: domain.ConclaveVassal{Role: "reader", Workload: "worker"}, Zone: domain.ZoneSandbox}
				if err := st.CreateCouncil(ctx, room, store.CouncilSpec{Members: []store.CouncilMember{member}, Question: question}); err != nil {
					t.Fatal(err)
				}
				a := store.ConclaveAssignment{ConclaveID: room.ID, Role: "reader", Round: 1, Attempt: 1}
				payload := map[string]any{"conclaveId": room.ID, "role": a.Role, "round": a.Round, "attempt": a.Attempt}
				if tc.name != "missing" {
					payload["context_packet"] = tc.packet
				}
				input, err := json.Marshal(payload)
				if err != nil {
					t.Fatal(err)
				}
				if tc.name == "invalid UTF-8" {
					input = []byte(strings.Replace(string(input), "invalid-utf8-marker", "\xff", 1))
				}
				task, err := store.SubmitConclaveTurn(ctx, room.ID, a.TurnKey(), store.ConclaveLimits{MaxRuns: 6, MaxMessages: 8, MaxPingPong: 8}, func(ctx context.Context) (domain.Task, error) {
					return r.SubmitTask(ctx, "worker", a.TurnKey(), string(input))
				})
				if err != nil {
					t.Fatal(err)
				}
				stub := &fakeAdapter{}
				err = launchForTest(ctx, r, task, wl, stub)
				if !tc.valid {
					if err == nil || stub.lastSpec.TaskID != "" {
						t.Fatal("invalid packet reached launch")
					}
					requireState(t, st, task.ID, domain.TaskStateFailed)
					return
				}
				if err != nil {
					t.Fatal(err)
				}
				var launch struct {
					Packet string `json:"context_packet"`
				}
				if err := json.Unmarshal([]byte(stub.lastSpec.InputJSON), &launch); err != nil {
					t.Fatal(err)
				}
				if launch.Packet != packet.Markdown {
					t.Fatal("adapter did not receive the reserved packet")
				}
				for _, want := range []string{question, decree, `"role":"writer"`, "[REDACTED:", "[peer text omitted]"} {
					if !strings.Contains(launch.Packet, want) {
						t.Errorf("missing %q", want)
					}
				}
				start := strings.Index(launch.Packet, "\n### Round")
				if start < 0 || len(launch.Packet[start:]) > 16<<10 || strings.Contains(launch.Packet, secret) {
					t.Fatal("peer allowance or redaction failed")
				}
			})
		}
	}
}

func TestUnreservedCouncilPacket(t *testing.T) {
	for _, fields := range []struct{ packet, room string }{
		{"context_packet", "conclaveId"},
		{"Context_Packet", "ConclaveId"},
		{"CONTEXT_PACKET", "CONCLAVEID"},
	} {
		for _, class := range []domain.WorkloadClass{domain.ClassCodingAgent, domain.ClassDeterministicController} {
			t.Run(fields.packet+"/"+string(class), func(t *testing.T) {
				r, st, _, _ := regressionFixture(t)
				ctx := t.Context()
				member := store.CouncilMember{Vassal: domain.ConclaveVassal{Role: "another", Workload: "worker"}, Zone: domain.ZoneSandbox}
				if err := st.CreateCouncil(ctx, domain.ConclaveSession{ID: "panel", State: domain.ConclaveStateActive}, store.CouncilSpec{Question: "Review?", Members: []store.CouncilMember{member}}); err != nil {
					t.Fatal(err)
				}
				if _, err := st.AppendConclaveMessage(ctx, domain.ConclaveMessage{ConclaveID: "panel", FromRole: "operator", ToRole: "another", Kind: "decree", Body: "private-decree"}); err != nil {
					t.Fatal(err)
				}
				wl := r.bundle.Workloads["worker"]
				wl.Spec.Class = class
				raw, err := json.Marshal(map[string]any{fields.packet: "replacement", fields.room: "panel", "role": "reader", "round": 1, "attempt": 1, "prompt": "review"})
				if err != nil {
					t.Fatal(err)
				}
				task, err := r.SubmitTask(ctx, "worker", "unreserved", string(raw))
				if err != nil {
					t.Fatal(err)
				}
				stub := &fakeAdapter{}
				if err := launchForTest(ctx, r, task, wl, stub); err != nil {
					t.Fatal(err)
				}
				if strings.Contains(stub.lastSpec.InputJSON, "replacement") || strings.Contains(stub.lastSpec.InputJSON, "private-decree") || strings.Contains(stub.lastSpec.InputJSON, "panel") {
					t.Fatal("caller supplied packet or room reached adapter")
				}
			})
		}
	}
}

func TestAssignedPacketUsesReservedRoom(t *testing.T) {
	for _, alias := range []string{"CONCLAVEID", "ConclaveId"} {
		t.Run(alias, func(t *testing.T) {
			ctx := t.Context()
			r, st, _, _ := regressionFixture(t)
			wl := r.bundle.Workloads["worker"]
			wl.Spec.Class = domain.ClassCodingAgent
			for _, id := range []string{"assigned-room", "private-room"} {
				if err := st.CreateConclave(ctx, domain.ConclaveSession{ID: id, State: domain.ConclaveStateActive, CreatedAt: r.now()}); err != nil {
					t.Fatal(err)
				}
				if _, err := st.AppendConclaveMessage(ctx, domain.ConclaveMessage{ConclaveID: id, FromRole: "operator", ToRole: "all", Kind: "decree", Body: id + "-decree"}); err != nil {
					t.Fatal(err)
				}
			}
			a := store.ConclaveAssignment{ConclaveID: "assigned-room", Role: "reader", Activation: domain.ActivationAlwaysOn}
			input := `{"conclaveId":"private-room","` + alias + `":"assigned-room","role":"reader","activation":"always-on","prompt":"review","repo_path":""}`
			limits := store.ConclaveLimits{MaxRuns: 6, MaxMessages: 8, MaxPingPong: 8, Now: r.now}
			task, err := store.SubmitConclaveTurn(ctx, a.ConclaveID, a.TurnKey(), limits, func(ctx context.Context) (domain.Task, error) { return r.SubmitTask(ctx, "worker", a.TurnKey(), input) })
			if err != nil {
				t.Fatal(err)
			}
			stub := &fakeAdapter{}
			if err := launchForTest(ctx, r, task, wl, stub); err != nil {
				t.Fatal(err)
			}
			if strings.Contains(stub.lastSpec.InputJSON, "private-room") || !strings.Contains(stub.lastSpec.InputJSON, "assigned-room-decree") {
				t.Fatal("conflicting fields redirected the room lookup")
			}
		})
	}
}

func TestOrdinaryConclaveLaunchMatchesMain(t *testing.T) {
	const roomID = "ordinary"
	const input = `{"conclaveId":"ordinary","prompt":"Review the evidence"}`
	for _, path := range []string{"api submission", "workflow step", "schedule", "delegated child"} {
		t.Run(path, func(t *testing.T) {
			ctx := t.Context()
			r, st, _, _ := regressionFixture(t)
			wl := r.bundle.Workloads["worker"]
			wl.Spec.Class = domain.ClassLongLivedAgent
			if err := st.CreateConclave(ctx, domain.ConclaveSession{ID: roomID, State: domain.ConclaveStateActive, CreatedAt: r.now()}); err != nil {
				t.Fatal(err)
			}
			if _, err := st.AppendConclaveMessage(ctx, domain.ConclaveMessage{ConclaveID: roomID, FromRole: "operator", ToRole: "all", Kind: "decree", Body: "ordinary-decree"}); err != nil {
				t.Fatal(err)
			}
			if err := st.PutConclaveRelic(ctx, domain.ConclaveRelic{ConclaveID: roomID, Name: "notes", AuthorRole: "reader", Body: "ordinary-evidence"}); err != nil {
				t.Fatal(err)
			}

			var task domain.Task
			var err error
			switch path {
			case "api submission":
				task, err = r.SubmitTask(ctx, "worker", "api", input)
			case "workflow step":
				const stepInput = `{"workflow":"flow","step":"review","step_index":0,"conclaveId":"ordinary","prompt":"Review the evidence"}`
				task, err = r.submitTask(ctx, "worker", "", stepInput, "", &workflowStepRecord{Workflow: "flow"})
			case "schedule":
				scheduler := NewScheduler(r)
				slot := scheduler.now()
				scheduler.now = func() time.Time { return slot }
				sc := domain.Schedule{Metadata: domain.ObjectMeta{Name: "review"}, Spec: domain.ScheduleSpec{TargetKind: "workload", TargetName: "worker", Input: input}}
				if err := scheduler.Fire(ctx, sc); err != nil {
					t.Fatal(err)
				}
				task, err = st.GetTaskByIdempotencyKey(ctx, "sched:review:"+scheduler.now().UTC().Format("200601021504"))
			case "delegated child":
				a := store.ConclaveAssignment{ConclaveID: roomID, Role: "reader", Activation: domain.ActivationAlwaysOn}
				raw, err := json.Marshal(a)
				if err != nil {
					t.Fatal(err)
				}
				parent, err := store.SubmitConclaveTurn(ctx, roomID, a.TurnKey(), store.ConclaveLimits{MaxRuns: 6, MaxMessages: 8, MaxPingPong: 8, Now: r.now}, func(ctx context.Context) (domain.Task, error) {
					return r.SubmitTask(ctx, "worker", a.TurnKey(), string(raw))
				})
				if err != nil {
					t.Fatal(err)
				}
				task, err = r.ProposeChildTask(ctx, parent.ID, "worker", "child", input, "{}")
				if err != nil {
					t.Fatal(err)
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, assigned := store.TaskConclaveAssignment(task); assigned {
				t.Fatal("expected an ordinary task without an assignment")
			}

			// Main kept the submitted fields and assembled the packet from the task input.
			var ordinaryFields map[string]json.RawMessage
			if err := json.Unmarshal([]byte(task.InputJSON), &ordinaryFields); err != nil {
				t.Fatal(err)
			}
			packet := contextpack.Assemble(ctx, st, task, wl).Markdown
			for _, text := range []string{"ordinary-decree", "ordinary-evidence"} {
				if !strings.Contains(packet, text) {
					t.Fatalf("baseline packet missing %q", text)
				}
			}
			ordinaryFields["context_packet"], _ = json.Marshal(packet)

			stub := &fakeAdapter{}
			if err := launchForTest(ctx, r, task, wl, stub); err != nil {
				t.Fatal(err)
			}
			var launchFields map[string]json.RawMessage
			if err := json.Unmarshal([]byte(stub.lastSpec.InputJSON), &launchFields); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(launchFields, ordinaryFields) {
				t.Errorf("launch input = %s, want original fields and ordinary Conclave packet", stub.lastSpec.InputJSON)
			}
		})
	}
}
