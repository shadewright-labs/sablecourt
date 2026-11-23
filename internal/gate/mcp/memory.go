package mcpgate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/shadewright-labs/sablecourt/internal/domain"
	"github.com/shadewright-labs/sablecourt/internal/redact"
	"github.com/shadewright-labs/sablecourt/internal/store"
)

// RegisterBuiltinMemory registers redacted project-level Court Memory MCP tools
// (memory.put, memory.get, memory.search) on the Data/Action Gate.
func RegisterBuiltinMemory(gate *Gate, st store.Store) {
	if gate == nil || st == nil {
		return
	}
	gate.RegisterTool(ToolDef{
		Name:        "memory.put",
		Description: "Store or update a redacted project memory entry under a namespace and key.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"namespace":{"type":"string"},"key":{"type":"string"},"value":{"type":"string"}},"required":["key","value"]}`),
		Handler: func(ctx context.Context, wl domain.Workload, taskID string, args json.RawMessage) (string, error) {
			var in struct {
				Namespace string `json:"namespace"`
				Key       string `json:"key"`
				Value     string `json:"value"`
			}
			if err := json.Unmarshal(args, &in); err != nil {
				return "", err
			}
			in.Key = strings.TrimSpace(in.Key)
			if in.Key == "" {
				return "", errors.New("key is required")
			}
			if strings.TrimSpace(in.Namespace) == "" {
				in.Namespace = "default"
			}
			entry := domain.MemoryEntry{
				Namespace: strings.TrimSpace(in.Namespace),
				Key:       in.Key,
				Value:     redact.String(strings.TrimSpace(in.Value)),
				Author:    wl.Metadata.Name,
				UpdatedAt: time.Now().UTC(),
			}
			if err := st.PutMemory(ctx, entry); err != nil {
				return "", err
			}
			_ = st.AppendAudit(ctx, domain.AuditRecord{
				Category:     "memory",
				TaskID:       taskID,
				WorkloadName: wl.Metadata.Name,
				Action:       "memory.put",
				Outcome:      "stored",
				DetailJSON:   fmt.Sprintf(`{"namespace":%q,"key":%q}`, entry.Namespace, entry.Key),
				CreatedAt:    entry.UpdatedAt,
			})
			out, _ := json.Marshal(entry)
			return string(out), nil
		},
	})
	gate.RegisterTool(ToolDef{
		Name:        "memory.get",
		Description: "Retrieve a project memory entry by namespace and key.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"namespace":{"type":"string"},"key":{"type":"string"}},"required":["key"]}`),
		Handler: func(ctx context.Context, _ domain.Workload, _ string, args json.RawMessage) (string, error) {
			var in struct {
				Namespace string `json:"namespace"`
				Key       string `json:"key"`
			}
			if err := json.Unmarshal(args, &in); err != nil {
				return "", err
			}
			entry, err := st.GetMemory(ctx, strings.TrimSpace(in.Namespace), strings.TrimSpace(in.Key))
			if err != nil {
				return "", err
			}
			out, _ := json.Marshal(entry)
			return string(out), nil
		},
	})
	gate.RegisterTool(ToolDef{
		Name:        "memory.search",
		Description: "Search project memory entries by optional namespace and substring query.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"namespace":{"type":"string"},"query":{"type":"string"},"limit":{"type":"integer"}}}`),
		Handler: func(ctx context.Context, _ domain.Workload, _ string, args json.RawMessage) (string, error) {
			var in struct {
				Namespace string `json:"namespace"`
				Query     string `json:"query"`
				Limit     int    `json:"limit"`
			}
			_ = json.Unmarshal(args, &in)
			items, err := st.SearchMemory(ctx, in.Namespace, in.Query, in.Limit)
			if err != nil {
				return "", err
			}
			out, _ := json.Marshal(items)
			return string(out), nil
		},
	})
}
