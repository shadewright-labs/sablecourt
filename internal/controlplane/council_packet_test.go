package controlplane

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

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
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			r, st, _, _ := regressionFixture(t)
			wl := r.bundle.Workloads["worker"]
			wl.Spec.Class = domain.ClassCodingAgent
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

func TestUnreservedCouncilPacket(t *testing.T) {
	for _, key := range []string{"context_packet", "Context_Packet", "CONTEXT_PACKET"} {
		t.Run(key, func(t *testing.T) {
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
			wl.Spec.Class = domain.ClassCodingAgent
			raw, err := json.Marshal(map[string]any{key: "replacement", "conclaveId": "panel", "role": "reader", "round": 1, "attempt": 1, "prompt": "review"})
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
			if strings.Contains(stub.lastSpec.InputJSON, "replacement") || strings.Contains(stub.lastSpec.InputJSON, "private-decree") {
				t.Fatal("caller supplied packet or room reached adapter")
			}
		})
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
