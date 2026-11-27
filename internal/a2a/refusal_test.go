package a2a

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/shadewright-labs/sablecourt/internal/config"
	"github.com/shadewright-labs/sablecourt/internal/controlplane"
	"github.com/shadewright-labs/sablecourt/internal/domain"
	"github.com/shadewright-labs/sablecourt/internal/secret"
	"github.com/shadewright-labs/sablecourt/internal/store/sqlite"
)

type rpcReply struct {
	ID     any `json:"id"`
	Result struct {
		ID       string `json:"id"`
		Workload string `json:"workload"`
		Status   struct {
			State string `json:"state"`
		} `json:"status"`
	} `json:"result"`
	Error *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func newBroker(t *testing.T) (*sqlite.SQLiteStore, *controlplane.Reconciler) {
	t.Helper()
	ctx := context.Background()
	st, err := sqlite.Open(filepath.Join(t.TempDir(), "sablecourt.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	bundle := &config.Bundle{Workloads: map[string]domain.Workload{
		"llm-agent": {Metadata: domain.ObjectMeta{Name: "llm-agent", Zone: domain.ZoneRuntime}, Spec: domain.WorkloadSpec{Class: domain.ClassLongLivedAgent}},
	}}
	_ = st.UpsertWorkload(ctx, domain.WorkloadRecord{Name: "llm-agent", Zone: domain.ZoneRuntime, Class: domain.ClassLongLivedAgent, ManifestDigest: "d1", AgentCardJSON: "{}"})
	return st, controlplane.NewReconciler(controlplane.Options{Store: st, Bundle: bundle})
}

func TestBrokerHandlerRefusals(t *testing.T) {
	st, rec := newBroker(t)
	token := "a2a-" + strings.Repeat("t", 16)
	secrets := secret.NewMemoryStore(map[string]string{"a2a_bearer_token": token, "blank": " "})
	ok := Options{Store: st, Reconciler: rec, Secrets: secrets}
	custom := ok
	custom.SecretName = "blank"

	cases := []struct {
		name   string
		opts   Options
		method string
		auth   string
		body   string
		status int
		code   int
		msg    string
	}{
		{"GET", ok, http.MethodGet, "Bearer " + token, ``, http.StatusOK, -32600, "POST required"},
		{"no store", Options{Reconciler: rec, Secrets: secrets}, http.MethodPost, "Bearer " + token, `{}`, http.StatusOK, -32000, "unavailable"},
		{"no secrets", Options{Store: st, Reconciler: rec}, http.MethodPost, "Bearer " + token, `{}`, http.StatusOK, -32000, "unavailable"},
		{"blank configured token", custom, http.MethodPost, "Bearer  ", `{}`, http.StatusUnauthorized, -32001, "not configured"},
		{"missing configured token", Options{Store: st, Reconciler: rec, Secrets: secrets, SecretName: "absent"}, http.MethodPost, "Bearer " + token, `{}`, http.StatusUnauthorized, -32001, "not configured"},
		{"token without Bearer", ok, http.MethodPost, token, `{}`, http.StatusUnauthorized, -32001, "unauthorized"},
		{"wrong token", ok, http.MethodPost, "Bearer " + token + "x", `{}`, http.StatusUnauthorized, -32001, "unauthorized"},
		{"token prefix only", ok, http.MethodPost, "Bearer " + token[:len(token)-1], `{}`, http.StatusUnauthorized, -32001, "unauthorized"},
		{"bad JSON", ok, http.MethodPost, "Bearer " + token, `{`, http.StatusOK, -32700, "invalid JSON-RPC"},
		{"unknown method", ok, http.MethodPost, "Bearer " + token, `{"id":3,"method":"tasks/cancel"}`, http.StatusOK, -32601, "method not found"},
		{"send without workload", ok, http.MethodPost, "Bearer " + token, `{"id":4,"method":"tasks/send","params":{"id":"x"}}`, http.StatusOK, -32602, "workload is required"},
		{"send with bad params", ok, http.MethodPost, "Bearer " + token, `{"id":5,"method":"tasks/send","params":[1]}`, http.StatusOK, -32602, "workload is required"},
		{"send to undeclared workload", ok, http.MethodPost, "Bearer " + token, `{"id":6,"method":"tasks/send","params":{"id":"x","workload":"ghost"}}`, http.StatusOK, -32002, "ghost"},
		{"get without id", ok, http.MethodPost, "Bearer " + token, `{"id":7,"method":"tasks/get","params":{}}`, http.StatusOK, -32602, "id is required"},
		{"get unknown task", ok, http.MethodPost, "Bearer " + token, `{"id":8,"method":"tasks/get","params":{"id":"nope"}}`, http.StatusOK, -32004, "task not found"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, "/a2a", strings.NewReader(tc.body))
			req.Header.Set("Authorization", tc.auth)
			rr := httptest.NewRecorder()
			BrokerHandler(tc.opts).ServeHTTP(rr, req)
			var reply rpcReply
			if err := json.Unmarshal(rr.Body.Bytes(), &reply); err != nil {
				t.Fatalf("decode %q: %v", rr.Body.String(), err)
			}
			if rr.Code != tc.status || reply.Error == nil || reply.Error.Code != tc.code || !strings.Contains(reply.Error.Message, tc.msg) {
				t.Fatalf("got %d %s, want %d code %d %q", rr.Code, rr.Body.String(), tc.status, tc.code, tc.msg)
			}
		})
	}
	tasks, _ := st.ListTasks(context.Background())
	if len(tasks) != 0 {
		t.Fatalf("refused requests created tasks: %+v", tasks)
	}
}

func TestBrokerHandlerSendIsIdempotentAndJoinsParts(t *testing.T) {
	st, rec := newBroker(t)
	token := "a2a-" + strings.Repeat("u", 16)
	h := BrokerHandler(Options{Store: st, Reconciler: rec, Secrets: secret.NewMemoryStore(map[string]string{"a2a_bearer_token": token})})
	send := func() rpcReply {
		body := `{"jsonrpc":"2.0","id":"s","method":"tasks/send","params":{"id":"ext-9","workload":"llm-agent","message":{"parts":[{"text":"one"},{"text":""},{"text":"two"}]}}}`
		req := httptest.NewRequest(http.MethodPost, "/a2a", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		var reply rpcReply
		if err := json.Unmarshal(rr.Body.Bytes(), &reply); err != nil || reply.Error != nil {
			t.Fatalf("send: %s, %v", rr.Body.String(), err)
		}
		return reply
	}
	first, second := send(), send()
	if first.Result.ID == "" || first.Result.ID != second.Result.ID || first.ID != "s" || first.Result.Workload != "llm-agent" {
		t.Fatalf("replies = %+v, %+v", first, second)
	}
	tasks, _ := st.ListTasks(context.Background())
	if len(tasks) != 1 {
		t.Fatalf("tasks = %d, want 1", len(tasks))
	}
	var in map[string]string
	_ = json.Unmarshal([]byte(tasks[0].InputJSON), &in)
	if in["prompt"] != "one\ntwo" {
		t.Fatalf("prompt = %q", in["prompt"])
	}
}

func TestBrokerHandlerRefusesObserverSecret(t *testing.T) {
	const shared = "shared-secret-for-both-roles"
	for _, tc := range []struct {
		name     string
		secrets  map[string]string
		token    string
		wantCode int
	}{
		{"observer secret equal to a2a secret", map[string]string{"a2a_bearer_token": shared, "observer_bearer_token": shared}, shared, http.StatusForbidden},
		{"observer secret padded with space", map[string]string{"a2a_bearer_token": shared, "observer_bearer_token": shared + "\n"}, shared, http.StatusForbidden},
		{"observer secret differs", map[string]string{"a2a_bearer_token": shared, "observer_bearer_token": "other-observer"}, "other-observer", http.StatusUnauthorized},
		{"a2a secret alone works", map[string]string{"a2a_bearer_token": shared}, shared, http.StatusOK},
	} {
		for _, method := range []string{"tasks/send", "tasks/get"} {
			t.Run(tc.name+" "+method, func(t *testing.T) {
				st, rec := newBroker(t)
				h := BrokerHandler(Options{Store: st, Reconciler: rec, Secrets: secret.NewMemoryStore(tc.secrets)})
				body := `{"jsonrpc":"2.0","id":1,"method":"` + method + `","params":{"id":"x","workload":"llm-agent"}}`
				req := httptest.NewRequest(http.MethodPost, "/a2a", strings.NewReader(body))
				req.Header.Set("Authorization", "Bearer "+tc.token)
				rr := httptest.NewRecorder()
				h.ServeHTTP(rr, req)
				if rr.Code != tc.wantCode {
					t.Fatalf("status = %d, want %d: %s", rr.Code, tc.wantCode, rr.Body.String())
				}
				if tc.wantCode != http.StatusOK {
					if tasks, _ := st.ListTasks(context.Background()); len(tasks) != 0 {
						t.Fatalf("refused request created tasks: %+v", tasks)
					}
				}
			})
		}
	}
}

type observerErrStore struct {
	secret.SecretStore
	err error
}

func (s observerErrStore) Get(ctx context.Context, name string) (string, error) {
	if name == "observer_bearer_token" {
		return "", s.err
	}
	return s.SecretStore.Get(ctx, name)
}

func TestBrokerHandlerFailsClosedOnObserverLookupError(t *testing.T) {
	const shared = "shared-secret-for-both-roles"
	st, rec := newBroker(t)
	secrets := observerErrStore{SecretStore: secret.NewMemoryStore(map[string]string{"a2a_bearer_token": shared}), err: secret.ErrInsecureFileMode}
	h := BrokerHandler(Options{Store: st, Reconciler: rec, Secrets: secrets})
	body := `{"jsonrpc":"2.0","id":1,"method":"tasks/send","params":{"id":"x","workload":"llm-agent"}}`
	req := httptest.NewRequest(http.MethodPost, "/a2a", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+shared)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503: %s", rr.Code, rr.Body.String())
	}
	if strings.Contains(rr.Body.String(), shared) {
		t.Fatal("response leaks the token")
	}
	if tasks, _ := st.ListTasks(context.Background()); len(tasks) != 0 {
		t.Fatalf("refused request created tasks: %+v", tasks)
	}
}
