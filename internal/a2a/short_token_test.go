package a2a

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/shadewright-labs/sablecourt/internal/secret"
)

// A bearer token too short to redact is refused even when the caller sends
// it, and the reply does not show it.
func TestBrokerHandlerRefusesShortToken(t *testing.T) {
	st, rec := newBroker(t)
	const short = "short12"
	h := BrokerHandler(Options{Store: st, Reconciler: rec, Secrets: secret.NewMemoryStore(map[string]string{"a2a_bearer_token": short})})
	body := `{"jsonrpc":"2.0","id":"s","method":"tasks/send","params":{"id":"ext-1","workload":"llm-agent","message":{"parts":[{"text":"hi"}]}}}`
	req := httptest.NewRequest(http.MethodPost, "/a2a", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+short)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	var reply rpcReply
	if err := json.Unmarshal(rr.Body.Bytes(), &reply); err != nil {
		t.Fatalf("reply: %v", err)
	}
	if rr.Code != http.StatusServiceUnavailable || reply.Error == nil || strings.Contains(rr.Body.String(), short) {
		t.Fatalf("short token: %d %s", rr.Code, rr.Body.String())
	}
	if tasks, _ := st.ListTasks(t.Context()); len(tasks) != 0 {
		t.Fatalf("refused request created tasks: %d", len(tasks))
	}
}
