package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/shadewright-labs/sablecourt/internal/conclave"
	"github.com/shadewright-labs/sablecourt/internal/config"
	"github.com/shadewright-labs/sablecourt/internal/domain"
	"github.com/shadewright-labs/sablecourt/internal/gate/model"
	"github.com/shadewright-labs/sablecourt/internal/store"
	"github.com/shadewright-labs/sablecourt/internal/store/sqlite"
)

func daemonTestStore(t *testing.T) store.Store {
	t.Helper()
	st, err := sqlite.Open(filepath.Join(t.TempDir(), "daemon.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	return st
}

func addMessageTestLease(t *testing.T, st store.Store) string {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC()
	if err := st.UpsertWorkload(ctx, domain.WorkloadRecord{Name: "worker", Zone: domain.ZoneRuntime, Class: domain.ClassLongLivedAgent, AgentCardJSON: "{}"}); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateTask(ctx, domain.Task{ID: "task", IdempotencyKey: "task", WorkloadName: "worker", State: domain.TaskStateWorking, InputJSON: "{}", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	token := "test-task-token"
	if err := st.CreateLease(ctx, domain.Lease{TaskID: "task", WorkloadName: "worker", TokenSHA256: model.HashToken(token), ExpiresAt: now.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	return token
}

func TestTaskMessageRefusesSystemRole(t *testing.T) {
	for _, tc := range []struct {
		role string
		want int
	}{
		{"system", 403}, {" SYSTEM ", 403}, {"System", 403},
		{"agent", 201}, {"", 201}, {"user", 201},
	} {
		t.Run(tc.role, func(t *testing.T) {
			st := daemonTestStore(t)
			token := addMessageTestLease(t, st)
			h := newHandler(st, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)
			body, _ := json.Marshal(map[string]string{"role": tc.role, "content": "participant text"})
			r := httptest.NewRequest(http.MethodPost, "/gate/task/messages", strings.NewReader(string(body)))
			r.Header.Set("Authorization", "Bearer "+token)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != tc.want {
				t.Fatalf("status = %d, want %d: %s", w.Code, tc.want, w.Body.String())
			}
			msgs, err := st.ListTaskMessages(context.Background(), "task")
			if err != nil {
				t.Fatal(err)
			}
			if tc.want == 403 && len(msgs) != 0 {
				t.Fatal("refused message persisted")
			}
			if tc.want == 201 && (len(msgs) != 1 || msgs[0].Role != "agent") {
				t.Fatalf("unexpected stored messages: %+v", msgs)
			}
		})
	}
}

func TestParticipantMessageKindIsServerAssigned(t *testing.T) {
	for _, kind := range []string{"decree", "system", "review", "arbitrary", ""} {
		t.Run(kind, func(t *testing.T) {
			st := daemonTestStore(t)
			token := addMessageTestLease(t, st)
			bundle := &config.Bundle{Conclaves: map[string]domain.Conclave{
				"room": {Spec: domain.ConclaveSpec{Vassals: []domain.ConclaveVassal{{Role: "worker", Workload: "worker", CanMessage: []string{"all"}}}}},
			}}
			getBundle := func() *config.Bundle { return bundle }
			broker := conclave.NewBroker(conclave.Options{Store: st, GetBundle: getBundle})
			sess, err := broker.Open(context.Background(), "room", "Test room", "")
			if err != nil {
				t.Fatal(err)
			}
			h := newHandler(st, nil, getBundle, nil, nil, nil, nil, nil, nil, broker, nil, nil, nil)
			body, _ := json.Marshal(map[string]string{"kind": kind, "body": "participant text", "to": "all", "role": "operator"})
			r := httptest.NewRequest(http.MethodPost, "/gate/conclave/"+sess.ID+"/messages", strings.NewReader(string(body)))
			r.Header.Set("Authorization", "Bearer "+token)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != 201 {
				t.Fatalf("status = %d: %s", w.Code, w.Body.String())
			}
			msgs, err := st.ListConclaveMessages(context.Background(), sess.ID, 0)
			if err != nil || len(msgs) != 1 || msgs[0].Kind != "proposal" || msgs[0].FromRole != "worker" {
				t.Fatalf("messages = %+v, error = %v", msgs, err)
			}
		})
	}
}
