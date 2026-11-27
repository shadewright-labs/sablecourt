package conclave

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/shadewright-labs/sablecourt/internal/config"
	"github.com/shadewright-labs/sablecourt/internal/domain"
	mcpgate "github.com/shadewright-labs/sablecourt/internal/gate/mcp"
	"github.com/shadewright-labs/sablecourt/internal/policy"
	"github.com/shadewright-labs/sablecourt/internal/store"
)

var conclaveTools = []string{"conclave.post", "conclave.read", "conclave.relic_put", "conclave.relic_get"}

// conclaveToolClient connects an in-memory MCP client to the gate as the given task.
func conclaveToolClient(t *testing.T, st store.Store, b *Broker, taskID string) *mcp.ClientSession {
	t.Helper()
	ctx := context.Background()
	wl := domain.Workload{Metadata: domain.ObjectMeta{Name: "worker"}, Spec: domain.WorkloadSpec{Grants: domain.GrantSet{MCPTools: conclaveTools}}}
	rules := make([]domain.PolicyRule, 0, len(conclaveTools))
	for _, tool := range conclaveTools {
		rules = append(rules, domain.PolicyRule{Workload: "worker", Capability: "mcp:" + tool, Effect: domain.EffectAllow})
	}
	g := mcpgate.New(mcpgate.Options{Store: st, Policy: policy.NewEngine([]domain.Policy{{Spec: domain.PolicySpec{Rules: rules}}}, nil)})
	b.RegisterMCPTools(g)
	b.RegisterMCPTools(nil) // a nil gate is ignored
	srv := g.NewServerForWorkload(domain.Lease{TaskID: taskID}, wl)
	clientTransport, serverTransport := mcp.NewInMemoryTransports()
	ss, err := srv.Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ss.Close() })
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil).Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

func callTool(t *testing.T, cs *mcp.ClientSession, name string, args any) (string, bool) {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("CallTool %s: %v", name, err)
	}
	text := ""
	if len(res.Content) > 0 {
		if tc, ok := res.Content[0].(*mcp.TextContent); ok {
			text = tc.Text
		}
	}
	return text, res.IsError
}

func TestConclaveMCPToolsReadAndRelics(t *testing.T) {
	ctx := context.Background()
	st := brokerTestStore(t)
	bundle := limitsBundle(domain.ConclaveWards{MaxMessages: 20, MaxPingPong: 20},
		domain.ConclaveVassal{Role: "reader", Workload: "worker", CanMessage: []string{"all"}})
	b := NewBroker(Options{Store: st, GetBundle: func() *config.Bundle { return bundle }})
	room, err := b.Open(ctx, "council", "tools", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertWorkload(ctx, domain.WorkloadRecord{Name: "worker"}); err != nil {
		t.Fatal(err)
	}
	member := domain.Task{
		ID: "member", WorkloadName: "worker", State: domain.TaskStateWorking,
		IdempotencyKey: "conclave-" + room.ID + "-init-reader",
		InputJSON:      fmt.Sprintf(`{"conclaveId":%q,"role":"reader","activation":"always-on"}`, room.ID),
	}
	outsider := domain.Task{ID: "outsider", WorkloadName: "worker", State: domain.TaskStateWorking, IdempotencyKey: "unrelated", InputJSON: `{}`}
	for _, task := range []domain.Task{member, outsider} {
		if err := st.CreateTask(ctx, task); err != nil {
			t.Fatal(err)
		}
	}
	leaked := "ghp_" + strings.Repeat("R4t", 12)

	cs := conclaveToolClient(t, st, b, member.ID)
	if out, isErr := callTool(t, cs, "conclave.post", map[string]string{"conclave_id": room.ID, "to": "all", "body": "first " + leaked}); isErr {
		t.Fatalf("post: %s", out)
	}
	if _, err := b.PostMessage(ctx, room.ID, "operator", "all", "decree", "second", ""); err != nil {
		t.Fatal(err)
	}

	out, isErr := callTool(t, cs, "conclave.read", map[string]any{"conclave_id": room.ID})
	if isErr {
		t.Fatalf("read: %s", out)
	}
	var msgs []domain.ConclaveMessage
	if err := json.Unmarshal([]byte(out), &msgs); err != nil {
		t.Fatalf("decode %q: %v", out, err)
	}
	if len(msgs) != 2 || !strings.HasPrefix(msgs[0].Body, "first ") || msgs[1].Body != "second" || strings.Contains(out, leaked) {
		t.Fatalf("messages = %+v", msgs)
	}
	out, _ = callTool(t, cs, "conclave.read", map[string]any{"conclave_id": room.ID, "since_id": msgs[0].ID})
	if err := json.Unmarshal([]byte(out), &msgs); err != nil || len(msgs) != 1 || msgs[0].Body != "second" {
		t.Fatalf("since read = %q, %v", out, err)
	}

	out, isErr = callTool(t, cs, "conclave.relic_put", map[string]string{"conclave_id": room.ID, "name": "notes.txt", "body": "token " + leaked})
	if isErr {
		t.Fatalf("relic_put: %s", out)
	}
	var relic domain.ConclaveRelic
	if err := json.Unmarshal([]byte(out), &relic); err != nil || relic.AuthorRole != "reader" || relic.ContentType != "text/plain" || strings.Contains(relic.Body, leaked) {
		t.Fatalf("relic = %+v, %v", relic, err)
	}
	out, isErr = callTool(t, cs, "conclave.relic_get", map[string]string{"conclave_id": room.ID, "name": "notes.txt"})
	if isErr || !strings.Contains(out, relic.SHA256) {
		t.Fatalf("relic_get = %s", out)
	}
	if out, isErr := callTool(t, cs, "conclave.relic_get", map[string]string{"conclave_id": room.ID, "name": "absent"}); !isErr {
		t.Fatalf("relic_get of a missing relic = %s", out)
	}
	if out, isErr := callTool(t, cs, "conclave.relic_put", map[string]string{"conclave_id": room.ID, "name": " ", "body": "x"}); !isErr || !strings.Contains(out, "relic name is required") {
		t.Fatalf("relic_put without a name = %s", out)
	}
	if out, isErr := callTool(t, cs, "conclave.post", map[string]string{"conclave_id": "ccl-missing", "to": "all", "body": "x"}); !isErr {
		t.Fatalf("post to a missing room = %s", out)
	}

	// A task that is not assigned to the room can neither read nor touch relics.
	other := conclaveToolClient(t, st, b, outsider.ID)
	for _, call := range []struct {
		tool string
		args map[string]string
	}{
		{"conclave.read", map[string]string{"conclave_id": room.ID}},
		{"conclave.relic_get", map[string]string{"conclave_id": room.ID, "name": "notes.txt"}},
		{"conclave.relic_put", map[string]string{"conclave_id": room.ID, "name": "evil.txt", "body": "x"}},
		{"conclave.post", map[string]string{"conclave_id": room.ID, "to": "all", "body": "x"}},
	} {
		if out, isErr := callTool(t, other, call.tool, call.args); !isErr || strings.Contains(out, "first") {
			t.Fatalf("%s by an outsider = %s", call.tool, out)
		}
	}
	if _, err := st.GetConclaveRelic(ctx, room.ID, "evil.txt"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("outsider stored a relic: %v", err)
	}
}

func TestPostIntegrationReport(t *testing.T) {
	ctx := context.Background()
	st := brokerTestStore(t)
	bundle := limitsBundle(domain.ConclaveWards{MaxMessages: 10, MaxPingPong: 10}, domain.ConclaveVassal{Role: "reader", Workload: "worker"})
	b := NewBroker(Options{Store: st, GetBundle: func() *config.Bundle { return bundle }})
	room, err := b.Open(ctx, "council", "verify", "")
	if err != nil {
		t.Fatal(err)
	}

	for _, role := range []string{"reader", "operator", " integration:shadewright", "integration:shadewright "} {
		if _, _, err := b.PostIntegrationReport(ctx, room, role, "x", nil); !errors.Is(err, ErrMessageDenied) {
			t.Fatalf("role %q: %v, want ErrMessageDenied", role, err)
		}
	}
	missingTpl := room
	missingTpl.TemplateName = "gone"
	if _, _, err := b.PostIntegrationReport(ctx, missingTpl, "integration:shadewright", "x", nil); err == nil || !strings.Contains(err.Error(), "template unavailable") {
		t.Fatalf("missing template: %v", err)
	}
	if _, _, err := b.PostIntegrationReport(ctx, room, "integration:shadewright", "   ", nil); err == nil || !strings.Contains(err.Error(), "body is required") {
		t.Fatalf("blank body: %v", err)
	}
	if _, _, err := b.PostIntegrationReport(ctx, room, "integration:shadewright", "x", &domain.ConclaveRelic{Name: " "}); err == nil || !strings.Contains(err.Error(), "relic name is required") {
		t.Fatalf("relic without a name: %v", err)
	}
	if msgs, _ := st.ListConclaveMessages(ctx, room.ID, 0); len(msgs) != 0 {
		t.Fatalf("refused reports stored messages: %+v", msgs)
	}

	leaked := "ghp_" + strings.Repeat("P0o", 12)
	msg, relic, err := b.PostIntegrationReport(ctx, room, "integration:shadewright", "verified "+leaked, &domain.ConclaveRelic{Name: "verify.json", ContentType: "application/json", Body: `{"k":"` + leaked + `"}`})
	if err != nil {
		t.Fatalf("PostIntegrationReport: %v", err)
	}
	if msg.ID == 0 || msg.ToRole != "operator" || msg.Kind != "report" || strings.Contains(msg.Body, leaked) {
		t.Fatalf("message = %+v", msg)
	}
	stored, err := st.GetConclaveRelic(ctx, room.ID, "verify.json")
	if err != nil || stored.SHA256 != relic.SHA256 || stored.AuthorRole != "integration:shadewright" || strings.Contains(stored.Body, leaked) {
		t.Fatalf("stored relic = %+v, %v", stored, err)
	}
}
