package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/shadewright-labs/sablecourt/internal/conclave"
	"github.com/shadewright-labs/sablecourt/internal/config"
	"github.com/shadewright-labs/sablecourt/internal/domain"
	mcpgate "github.com/shadewright-labs/sablecourt/internal/gate/mcp"
	"github.com/shadewright-labs/sablecourt/internal/gate/model"
	"github.com/shadewright-labs/sablecourt/internal/policy"
	"github.com/shadewright-labs/sablecourt/internal/store"
)

// Inject one inconsistent fact at a time into an otherwise committed snapshot.
type visibilityStore struct {
	store.Store
	store.CouncilStore
	change func(*store.CouncilSnapshot)
}

func (s visibilityStore) CouncilSnapshot(ctx context.Context, room string) (store.CouncilSnapshot, error) {
	snap, err := s.CouncilStore.CouncilSnapshot(ctx, room)
	if err == nil && s.change != nil {
		s.change(&snap)
	}
	return snap, err
}

func reserveVisibilityTurn(t *testing.T, st store.Store, role string, round int) domain.Task {
	t.Helper()
	a := store.ConclaveAssignment{ConclaveID: "panel", Role: role, Round: round, Attempt: 1}
	raw, err := json.Marshal(a)
	if err != nil {
		t.Fatal(err)
	}
	task := domain.Task{ID: fmt.Sprintf("%s-%d", role, round), WorkloadName: "worker", State: domain.TaskStateSubmitted, IdempotencyKey: a.TurnKey(), InputJSON: string(raw)}
	limits := store.ConclaveLimits{Now: time.Now, MaxRuns: 6, MaxMessages: 16, MaxPingPong: 16}
	_, err = store.SubmitConclaveTurn(t.Context(), "panel", task.IdempotencyKey, limits, func(ctx context.Context) (domain.Task, error) { return task, st.CreateTask(ctx, task) })
	if err != nil {
		t.Fatal(err)
	}
	return task
}

func recordVisibilityAnswer(t *testing.T, st visibilityStore, task domain.Task, role, to string) {
	t.Helper()
	ctx := t.Context()
	if err := st.PutCouncilRelic(ctx, task.ID, domain.ConclaveRelic{ConclaveID: "panel", Name: role + "-notes", AuthorRole: role, Body: role + "-evidence"}); err != nil {
		t.Fatal(err)
	}
	msg := domain.ConclaveMessage{ConclaveID: "panel", FromRole: role, ToRole: to, Kind: "review", TaskID: task.ID, Body: role + "-answer"}
	id, err := st.AppendConclaveMessage(ctx, msg)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.UpdateTaskState(ctx, task.ID, domain.TaskStateSubmitted, domain.TaskStateCompleted, "done", "{}"); err != nil {
		t.Fatal(err)
	}
	if err := st.AcceptCouncilAnswer(ctx, task.ID, id, false); err != nil {
		t.Fatal(err)
	}
}

func visibilityRequest(t *testing.T, h http.Handler, token, room, resource string, mcp bool) (string, bool) {
	t.Helper()
	method, path, body := "GET", "/gate/conclave/"+room+"/"+resource, ""
	if mcp {
		tool := "conclave.read"
		args := map[string]string{"conclave_id": room}
		if name, ok := strings.CutPrefix(resource, "relics/"); ok {
			tool = "conclave.relic_get"
			args["name"] = name
		}
		raw, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": tool, "arguments": args}})
		if err != nil {
			t.Fatal(err)
		}
		method, path, body = "POST", "/gate/mcp", string(raw)
	}
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if !mcp {
		return w.Body.String(), w.Code == http.StatusOK
	}
	if w.Code != http.StatusOK {
		t.Fatalf("MCP status %d: %s", w.Code, w.Body.String())
	}
	for line := range strings.SplitSeq(w.Body.String(), "\n") {
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if !strings.HasPrefix(data, "{") {
			continue
		}
		var response struct {
			Result *struct {
				IsError bool
				Content []struct{ Text string }
			}
		}
		if err := json.Unmarshal([]byte(data), &response); err != nil || response.Result == nil || len(response.Result.Content) != 1 {
			t.Fatalf("unexpected MCP response: %s", data)
		}
		return response.Result.Content[0].Text, !response.Result.IsError
	}
	t.Fatalf("missing MCP result: %s", w.Body.String())
	return "", false
}

func TestCouncilVisibilityAcrossReads(t *testing.T) {
	for _, tc := range []struct {
		name       string
		sequential bool
		round      int
		to         string
		visible    bool
		change     func(*store.CouncilSnapshot)
	}{
		{"parallel", false, 1, "all", false, nil},
		{"completed predecessor", true, 1, "reader", true, nil},
		{"earlier round", false, 2, "all", true, nil},
		{"another recipient", true, 1, "writer", false, nil},
		{"no route", true, 1, "all", false, func(s *store.CouncilSnapshot) { s.Members[0].Vassal.CanMessage = []string{"operator"} }},
		{"sandbox to runtime", true, 1, "all", false, func(s *store.CouncilSnapshot) { s.Members[0].Zone = domain.ZoneSandbox }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			base := daemonTestStore(t)
			councils, err := store.Councils(base)
			if err != nil {
				t.Fatal(err)
			}
			st := visibilityStore{base, councils, tc.change}
			if err := st.UpsertWorkload(ctx, domain.WorkloadRecord{Name: "worker", Zone: domain.ZoneRuntime}); err != nil {
				t.Fatal(err)
			}
			room := domain.ConclaveSession{ID: "panel", TemplateName: "review", CreatedAt: time.Now().UTC(), State: domain.ConclaveStateActive}
			spec := store.CouncilSpec{Question: "What changed?", Sequential: tc.sequential}
			var seats []domain.ConclaveVassal
			for _, role := range []string{"writer", "reader"} {
				v := domain.ConclaveVassal{Role: role, Workload: "worker", CanMessage: []string{"all"}}
				seats = append(seats, v)
				spec.Members = append(spec.Members, store.CouncilMember{Vassal: v, Zone: domain.ZoneRuntime})
			}
			if err := st.CreateCouncil(ctx, room, spec); err != nil {
				t.Fatal(err)
			}
			writer := reserveVisibilityTurn(t, st, "writer", 1)
			recordVisibilityAnswer(t, st, writer, "writer", tc.to)
			if tc.round == 2 {
				recordVisibilityAnswer(t, st, reserveVisibilityTurn(t, st, "reader", 1), "reader", "all")
			}
			const testKey = "-----BEGIN PRIVATE KEY-----\nfixture-key-data\n-----END PRIVATE KEY-----"
			for _, to := range []string{"writer", "reader"} {
				if _, err := st.AppendConclaveMessage(ctx, domain.ConclaveMessage{ConclaveID: room.ID, Kind: "decree", FromRole: "operator", ToRole: to, Body: to + "-decree " + testKey}); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := st.AppendConclaveMessage(ctx, domain.ConclaveMessage{ConclaveID: room.ID, Kind: "review", FromRole: "writer", ToRole: "all", Body: "unrecorded-answer", TaskID: writer.ID}); err != nil {
				t.Fatal(err)
			}
			if err := st.PutConclaveRelic(ctx, domain.ConclaveRelic{ConclaveID: room.ID, AuthorRole: "operator", Name: "unassigned", Body: "unassigned-evidence"}); err != nil {
				t.Fatal(err)
			}
			snap, err := st.CouncilSnapshot(ctx, room.ID)
			if err != nil {
				t.Fatal(err)
			}
			plan := conclave.CouncilSnapshot{
				Session: snap.Session, Question: snap.Question, MaxRounds: snap.MaxRounds, MaxStarts: snap.MaxStarts,
				Deadline: snap.Deadline, Sequential: snap.Sequential, Decrees: snap.Decrees, Members: snap.Members, Turns: snap.Turns,
			}
			step, err := plan.Next(room.CreatedAt)
			if err != nil {
				t.Fatal(err)
			}
			var packet string
			for _, start := range step.Starts {
				if start.Role == "reader" {
					packet = start.Packet.Markdown
				}
			}
			if !strings.Contains(packet, spec.Question) || !strings.Contains(packet, "reader-decree") || strings.Contains(packet, "writer-decree") || strings.Contains(packet, "writer-answer") != tc.visible {
				t.Errorf("wrong planner packet: %s", packet)
			}
			reader := reserveVisibilityTurn(t, st, "reader", tc.round)
			if err := st.UpdateTaskState(ctx, reader.ID, domain.TaskStateSubmitted, domain.TaskStateWorking, "started", "{}"); err != nil {
				t.Fatal(err)
			}
			lease := func(id string) string {
				token := model.TokenPrefix + id
				if err := st.CreateLease(ctx, domain.Lease{TaskID: id, WorkloadName: "worker", TokenSHA256: model.HashToken(token), ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
					t.Fatal(err)
				}
				return token
			}
			token := lease(reader.ID)
			wl := domain.Workload{Metadata: domain.ObjectMeta{Name: "worker"}, Spec: domain.WorkloadSpec{Grants: domain.GrantSet{MCPTools: []string{"conclave.read", "conclave.relic_get"}}}}
			bundle := &config.Bundle{Workloads: map[string]domain.Workload{"worker": wl}, Conclaves: map[string]domain.Conclave{"review": {Spec: domain.ConclaveSpec{Vassals: seats}}}}
			getBundle := func() *config.Bundle { return bundle }
			broker := conclave.NewBroker(conclave.Options{Store: st, GetBundle: getBundle})
			var rules []domain.PolicyRule
			for _, name := range wl.Spec.Grants.MCPTools {
				rules = append(rules, domain.PolicyRule{Workload: "worker", Capability: "mcp:" + name, Effect: domain.EffectAllow})
			}
			gate := mcpgate.New(mcpgate.Options{Store: gateLeaseStore{st}, Bundle: bundle, Policy: policy.NewEngine([]domain.Policy{{Spec: domain.PolicySpec{Rules: rules}}}, nil)})
			broker.RegisterMCPTools(gate)
			h := newHandler(st, nil, getBundle, nil, nil, gate, nil, nil, nil, broker, nil, nil, nil)
			other, err := broker.Open(ctx, "review", "", "ordinary-decree")
			if err != nil {
				t.Fatal(err)
			}
			addAssignedConclaveLease(t, st, other.ID, "reader")
			otherToken := lease("assigned")
			if _, err := broker.PutRelic(ctx, other.ID, "writer", "shared", "text/plain", "ordinary-evidence"); err != nil {
				t.Fatal(err)
			}
			for _, mcp := range []bool{false, true} {
				msgs, ok := visibilityRequest(t, h, token, room.ID, "messages", mcp)
				if !ok || strings.Contains(msgs, "writer-answer") != tc.visible || !strings.Contains(msgs, "reader-decree") || strings.Contains(msgs, "writer-decree") || strings.Contains(msgs, "unrecorded-answer") || strings.Contains(msgs, "fixture-key-data") {
					t.Errorf("MCP=%t unexpected council messages: %s", mcp, msgs)
				}
				relic, ok := visibilityRequest(t, h, token, room.ID, "relics/writer-notes", mcp)
				if ok != tc.visible || strings.Contains(relic, "writer-evidence") != tc.visible {
					t.Errorf("MCP=%t wrong named relic: %s", mcp, relic)
				}
				if _, ok := visibilityRequest(t, h, token, room.ID, "relics/unassigned", mcp); ok {
					t.Errorf("MCP=%t returned unattributed relic", mcp)
				}
				for _, resource := range []string{"messages", "relics/writer-notes"} {
					if _, ok := visibilityRequest(t, h, otherToken, room.ID, resource, mcp); ok {
						t.Errorf("MCP=%t foreign task read %s", mcp, resource)
					}
				}
				for resource, want := range map[string]string{"messages": "ordinary-decree", "relics/shared": "ordinary-evidence"} {
					if body, ok := visibilityRequest(t, h, otherToken, other.ID, resource, mcp); !ok || !strings.Contains(body, want) {
						t.Errorf("MCP=%t ordinary read %s: %s", mcp, resource, body)
					}
				}
			}
		})
	}
}
