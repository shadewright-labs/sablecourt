package main

import (
	"context"
	"encoding/json"
	"errors"
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
	change func(*store.CouncilSnapshot) error
}

func (s visibilityStore) CouncilSnapshot(ctx context.Context, room string) (store.CouncilSnapshot, error) {
	snap, err := s.CouncilStore.CouncilSnapshot(ctx, room)
	if err == nil && s.change != nil {
		err = s.change(&snap)
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

const visibilityKey = "-----BEGIN PRIVATE KEY-----\nfixture-key-data\n-----END PRIVATE KEY-----"

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
	w := visibilityResponse(h, token, method, path, body)
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

func visibilityResponse(h http.Handler, token, method, path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
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
			st := visibilityStore{base, councils, func(snap *store.CouncilSnapshot) error {
				if tc.change != nil {
					tc.change(snap)
				}
				return nil
			}}
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
			broker, h := newVisibilityHandler(st, seats, nil)
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

func newVisibilityHandler(st store.Store, seats []domain.ConclaveVassal, now func() time.Time) (*conclave.Broker, http.Handler) {
	wl := domain.Workload{Metadata: domain.ObjectMeta{Name: "worker"}, Spec: domain.WorkloadSpec{Grants: domain.GrantSet{MCPTools: []string{"conclave.read", "conclave.relic_get"}}}}
	bundle := &config.Bundle{Workloads: map[string]domain.Workload{"worker": wl}, Conclaves: map[string]domain.Conclave{"review": {Spec: domain.ConclaveSpec{Vassals: seats}}}}
	getBundle := func() *config.Bundle { return bundle }
	broker := conclave.NewBroker(conclave.Options{Store: st, GetBundle: getBundle, Now: now})
	var rules []domain.PolicyRule
	for _, name := range wl.Spec.Grants.MCPTools {
		rules = append(rules, domain.PolicyRule{Workload: "worker", Capability: "mcp:" + name, Effect: domain.EffectAllow})
	}
	gate := mcpgate.New(mcpgate.Options{Store: gateLeaseStore{st}, Bundle: bundle, Policy: policy.NewEngine([]domain.Policy{{Spec: domain.PolicySpec{Rules: rules}}}, nil)})
	broker.RegisterMCPTools(gate)
	h := newHandler(st, nil, getBundle, nil, nil, gate, nil, nil, nil, broker, nil, nil, nil)
	return broker, h
}

func TestCouncilReadSessionChecks(t *testing.T) {
	for _, tc := range []struct {
		name    string
		state   domain.ConclaveState
		elapsed time.Duration
		change  func(*store.CouncilSnapshot)
		want    int
	}{
		{name: "active", want: http.StatusOK},
		{name: "completed", state: domain.ConclaveStateCompleted, want: http.StatusForbidden},
		{name: "cancelled", state: domain.ConclaveStateCancelled, want: http.StatusForbidden},
		{name: "deadline", elapsed: 15 * time.Minute, want: http.StatusForbidden},
		{name: "past deadline", elapsed: 16 * time.Minute, want: http.StatusForbidden},
		{name: "unknown state", change: func(s *store.CouncilSnapshot) { s.Session.State = "broken" }, want: http.StatusInternalServerError},
		{name: "active Ward", change: func(s *store.CouncilSnapshot) { s.Session.WardReason = "max_runs" }, want: http.StatusInternalServerError},
		{name: "paused without Ward", change: func(s *store.CouncilSnapshot) { s.Session.State = domain.ConclaveStateInputRequired }, want: http.StatusInternalServerError},
		{name: "paused with Ward", change: func(s *store.CouncilSnapshot) {
			s.Session.State, s.Session.WardReason = domain.ConclaveStateInputRequired, "max_runs"
		}, want: http.StatusForbidden},
		{name: "missing time", change: func(s *store.CouncilSnapshot) { s.Session.CreatedAt = time.Time{} }, want: http.StatusInternalServerError},
		{name: "future creation", elapsed: -time.Second, want: http.StatusInternalServerError},
		{name: "invalid deadline", change: func(s *store.CouncilSnapshot) { s.Deadline = s.Session.CreatedAt }, want: http.StatusInternalServerError},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clock, _, broker, h, token := visibilityReadFixture(t, func(s *store.CouncilSnapshot) error {
				if tc.change != nil {
					tc.change(s)
				}
				return nil
			})
			if tc.state != "" {
				if err := broker.Close(t.Context(), "panel", tc.state); err != nil {
					t.Fatal(err)
				}
			}
			*clock = clock.Add(tc.elapsed)
			for resource, evidence := range map[string]string{"messages": "writer-answer", "relics/writer-notes": "writer-evidence"} {
				w := visibilityResponse(h, token, http.MethodGet, "/gate/conclave/panel/"+resource, "")
				if w.Code != tc.want || strings.Contains(w.Body.String(), evidence) != (tc.want == http.StatusOK) {
					t.Errorf("HTTP %s status = %d, want %d", resource, w.Code, tc.want)
				}
				body, ok := visibilityRequest(t, h, token, "panel", resource, true)
				if ok != (tc.want == http.StatusOK) || strings.Contains(body, evidence) != ok {
					t.Errorf("MCP %s success = %t, want %t", resource, ok, tc.want == http.StatusOK)
				}
			}
		})
	}
}

func TestGateRelicEvidenceErrors(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*store.CouncilSnapshot) error
		want   int
	}{
		{"missing relic", nil, http.StatusNotFound},
		{"snapshot error", func(*store.CouncilSnapshot) error { return errors.New("broken snapshot") }, http.StatusInternalServerError},
		{"invalid turns", func(s *store.CouncilSnapshot) error { s.Turns = nil; return nil }, http.StatusInternalServerError},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, _, h, token := visibilityReadFixture(t, tc.change)
			w := visibilityResponse(h, token, http.MethodGet, "/gate/conclave/panel/relics/missing", "")
			if w.Code != tc.want {
				t.Errorf("status = %d, want %d", w.Code, tc.want)
			}
		})
	}
}

func TestGateMessagesSince(t *testing.T) {
	_, _, _, h, token := visibilityReadFixture(t, nil)
	for _, tc := range []struct {
		query  string
		want   int
		answer bool
	}{
		{"", http.StatusOK, true},
		{"?since=", http.StatusOK, true},
		{"?since=0", http.StatusOK, true},
		{"?since=9223372036854775807", http.StatusOK, false},
		{"?since=notanumber", http.StatusBadRequest, false},
		{"?since=0junk", http.StatusBadRequest, false},
		{"?since=-1", http.StatusBadRequest, false},
		{"?since=9223372036854775808", http.StatusBadRequest, false},
	} {
		t.Run(tc.query, func(t *testing.T) {
			w := visibilityResponse(h, token, http.MethodGet, "/gate/conclave/panel/messages"+tc.query, "")
			if w.Code != tc.want || strings.Contains(w.Body.String(), "writer-answer") != tc.answer {
				t.Errorf("status = %d, want %d, body = %s", w.Code, tc.want, w.Body.String())
			}
		})
	}
}

func TestGateMissingOrdinaryRoom(t *testing.T) {
	_, st, _, h, _ := visibilityReadFixture(t, nil)
	token := addAssignedConclaveLease(t, st, "missing", "reader")
	w := visibilityResponse(h, token, http.MethodGet, "/gate/conclave/missing/messages", "")
	if w.Code != http.StatusForbidden {
		t.Errorf("status = %d, want %d", w.Code, http.StatusForbidden)
	}
}

func TestGateReadBodiesRedacted(t *testing.T) {
	_, st, _, h, token := visibilityReadFixture(t, func(s *store.CouncilSnapshot) error {
		for _, turn := range s.Turns {
			if turn.Answer != nil {
				turn.Answer.Body += " " + visibilityKey
			}
		}
		for _, attempt := range s.Attempts {
			for i := range attempt.Relics {
				attempt.Relics[i].Body += " " + visibilityKey
			}
		}
		return nil
	})
	ctx := t.Context()
	ordinary := domain.ConclaveSession{ID: "ordinary", TemplateName: "review", State: domain.ConclaveStateActive}
	if err := st.CreateConclave(ctx, ordinary); err != nil {
		t.Fatal(err)
	}
	addAssignedConclaveLease(t, st, ordinary.ID, "reader")
	ordinaryToken := model.TokenPrefix + "assigned"
	if err := st.CreateLease(ctx, domain.Lease{TaskID: "assigned", WorkloadName: "worker", TokenSHA256: model.HashToken(ordinaryToken), ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	body := "public-text " + visibilityKey
	if _, err := st.AppendConclaveMessage(ctx, domain.ConclaveMessage{ConclaveID: ordinary.ID, Kind: "decree", FromRole: "operator", ToRole: "all", Body: body}); err != nil {
		t.Fatal(err)
	}
	if err := st.PutConclaveRelic(ctx, domain.ConclaveRelic{ConclaveID: ordinary.ID, Name: "notes", AuthorRole: "writer", Body: body}); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		room, token, resource, want string
	}{
		{ordinary.ID, ordinaryToken, "messages", "public-text"},
		{ordinary.ID, ordinaryToken, "relics/notes", "public-text"},
		{"panel", token, "relics/writer-notes", "writer-evidence"},
		{"panel", token, "messages", "writer-answer"},
	} {
		for _, mcp := range []bool{false, true} {
			got, ok := visibilityRequest(t, h, tc.token, tc.room, tc.resource, mcp)
			if !ok || !strings.Contains(got, tc.want) || !strings.Contains(got, "[REDACTED:") || strings.Contains(got, "fixture-key-data") {
				t.Errorf("MCP=%t %s %s: expected redacted body", mcp, tc.room, tc.resource)
			}
		}
	}
}

func visibilityReadFixture(t *testing.T, change func(*store.CouncilSnapshot) error) (*time.Time, visibilityStore, *conclave.Broker, http.Handler, string) {
	t.Helper()
	ctx := t.Context()
	base := daemonTestStore(t)
	councils, err := store.Councils(base)
	if err != nil {
		t.Fatal(err)
	}
	st := visibilityStore{base, councils, change}
	if err := st.UpsertWorkload(ctx, domain.WorkloadRecord{Name: "worker", Zone: domain.ZoneRuntime}); err != nil {
		t.Fatal(err)
	}
	clock := time.Now().UTC()
	room := domain.ConclaveSession{ID: "panel", TemplateName: "review", CreatedAt: clock, State: domain.ConclaveStateActive}
	spec := store.CouncilSpec{Question: "Review?", Sequential: true}
	var seats []domain.ConclaveVassal
	for _, role := range []string{"writer", "reader"} {
		v := domain.ConclaveVassal{Role: role, Workload: "worker", CanMessage: []string{"all"}}
		seats = append(seats, v)
		spec.Members = append(spec.Members, store.CouncilMember{Vassal: v, Zone: domain.ZoneRuntime})
	}
	if err := st.CreateCouncil(ctx, room, spec); err != nil {
		t.Fatal(err)
	}
	recordVisibilityAnswer(t, st, reserveVisibilityTurn(t, st, "writer", 1), "writer", "all")
	reader := reserveVisibilityTurn(t, st, "reader", 1)
	if err := st.UpdateTaskState(ctx, reader.ID, domain.TaskStateSubmitted, domain.TaskStateWorking, "started", "{}"); err != nil {
		t.Fatal(err)
	}
	token := model.TokenPrefix + reader.ID
	if err := st.CreateLease(ctx, domain.Lease{TaskID: reader.ID, WorkloadName: "worker", TokenSHA256: model.HashToken(token), ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	broker, h := newVisibilityHandler(st, seats, func() time.Time { return clock })
	return &clock, st, broker, h, token
}
