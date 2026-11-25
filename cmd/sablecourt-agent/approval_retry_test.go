package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"testing"
)

// Medium 5: the gate runs an approved action only when the call carries the
// approval ID (docs/how-it-works/approvals.md, step 5). A directive that names
// the approval must send it as approvalId with the same payload.
func TestActionDirectiveSendsTheApprovalID(t *testing.T) {
	var sent map[string]any
	useFakeGate(t, func(r *http.Request) (*http.Response, error) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &sent)
		return gateResponse(200, `{}`), nil
	})
	dir, ok := parseToolDirective(`{"action":"git.open_pr","approval_id":"apr-1","args":{"owner":"acme"}}`)
	if !ok {
		t.Fatal("directive with an approval ID refused")
	}
	executeDirective(context.Background(), nil, "http://gate.local", "t", dir)
	if sent["approvalId"] != "apr-1" {
		t.Fatalf("approval ID not sent: %v", sent)
	}
	if p, _ := sent["payload"].(map[string]any); p["owner"] != "acme" {
		t.Fatalf("payload changed: %v", sent)
	}

	sent = nil
	dir, _ = parseToolDirective(`{"action":"git.open_pr","args":{"owner":"acme"}}`)
	executeDirective(context.Background(), nil, "http://gate.local", "t", dir)
	if _, has := sent["approvalId"]; has {
		t.Fatalf("approvalId sent without one in the directive: %v", sent)
	}
}
