package mcpgate

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/shadewright-labs/sablecourt/internal/domain"
	"github.com/shadewright-labs/sablecourt/internal/redact"
	"github.com/shadewright-labs/sablecourt/internal/store"
)

var errToolRefused = errors.New("refused")

func isReservedWriteNamespace(ns string) bool {
	lower := strings.ToLower(strings.TrimSpace(ns))
	return strings.HasPrefix(lower, "learning:") ||
		strings.HasPrefix(lower, "proposed:learning:") ||
		strings.HasPrefix(lower, "system:")
}

func isReservedReadNamespace(ns string) bool {
	lower := strings.ToLower(strings.TrimSpace(ns))
	return strings.HasPrefix(lower, "system:")
}

func auditMemory(ctx context.Context, gate *Gate, st store.Store, taskID, workloadName, action, outcome string, detail map[string]string) {
	for field, value := range detail {
		detail[field] = redact.String(value)
	}
	detailBytes, _ := json.Marshal(detail)
	now := time.Now().UTC()
	if gate != nil && gate.now != nil {
		now = gate.now()
	}
	_ = st.AppendAudit(ctx, domain.AuditRecord{
		Category:     "memory",
		TaskID:       taskID,
		WorkloadName: workloadName,
		Action:       action,
		Outcome:      outcome,
		DetailJSON:   string(detailBytes),
		CreatedAt:    now,
	})
}

// RegisterBuiltinMemory registers redacted project-level Court Memory MCP tools
// (memory.put, memory.get, memory.search, memory.delete) on the Data/Action Gate.
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
			ns := strings.TrimSpace(in.Namespace)
			if ns == "" {
				ns = "default"
			}
			if isReservedWriteNamespace(ns) {
				auditMemory(ctx, gate, st, taskID, wl.Metadata.Name, "memory.put", "denied", map[string]string{
					"namespace": ns,
					"key":       in.Key,
					"reason":    "reserved_namespace",
				})
				return "", fmt.Errorf("%w: namespace %q is reserved and cannot be written via memory.put", errToolRefused, redact.String(ns))
			}
			if in.Key == "" {
				return "", errors.New("key is required")
			}
			entry := domain.MemoryEntry{
				Namespace: ns,
				Key:       in.Key,
				Value:     redact.String(strings.TrimSpace(in.Value)),
				Author:    wl.Metadata.Name,
				UpdatedAt: gate.now(),
			}
			if err := st.PutMemory(ctx, entry); err != nil {
				return "", err
			}
			auditMemory(ctx, gate, st, taskID, wl.Metadata.Name, "memory.put", "stored", map[string]string{
				"namespace": entry.Namespace,
				"key":       entry.Key,
			})
			out, _ := json.Marshal(entry)
			return string(out), nil
		},
	})
	gate.RegisterTool(ToolDef{
		Name:        "memory.delete",
		Description: "Delete a project memory entry by namespace and key.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"namespace":{"type":"string"},"key":{"type":"string"}},"required":["key"]}`),
		Handler: func(ctx context.Context, wl domain.Workload, taskID string, args json.RawMessage) (string, error) {
			var in struct {
				Namespace string `json:"namespace"`
				Key       string `json:"key"`
			}
			if err := json.Unmarshal(args, &in); err != nil {
				return "", err
			}
			key := strings.TrimSpace(in.Key)
			ns := strings.TrimSpace(in.Namespace)
			if ns == "" {
				ns = "default"
			}
			if isReservedWriteNamespace(ns) {
				auditMemory(ctx, gate, st, taskID, wl.Metadata.Name, "memory.delete", "denied", map[string]string{
					"namespace": ns,
					"key":       key,
					"reason":    "reserved_namespace",
				})
				return "", fmt.Errorf("%w: namespace %q is reserved and cannot be deleted via memory.delete", errToolRefused, redact.String(ns))
			}
			if key == "" {
				return "", errors.New("key is required")
			}
			if err := st.DeleteMemory(ctx, ns, key); err != nil {
				return "", err
			}
			auditMemory(ctx, gate, st, taskID, wl.Metadata.Name, "memory.delete", "deleted", map[string]string{
				"namespace": ns,
				"key":       key,
			})
			out, _ := json.Marshal(map[string]string{
				"namespace": ns,
				"key":       key,
				"status":    "deleted",
			})
			return string(out), nil
		},
	})
	gate.RegisterTool(ToolDef{
		Name:        "memory.get",
		Description: "Retrieve a project memory entry by namespace and key.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"namespace":{"type":"string"},"key":{"type":"string"}},"required":["key"]}`),
		Handler: func(ctx context.Context, wl domain.Workload, taskID string, args json.RawMessage) (string, error) {
			var in struct {
				Namespace string `json:"namespace"`
				Key       string `json:"key"`
			}
			if err := json.Unmarshal(args, &in); err != nil {
				return "", err
			}
			ns := strings.TrimSpace(in.Namespace)
			key := strings.TrimSpace(in.Key)
			if isReservedReadNamespace(ns) {
				auditMemory(ctx, gate, st, taskID, wl.Metadata.Name, "memory.get", "denied", map[string]string{
					"namespace": ns,
					"key":       key,
					"reason":    "reserved_namespace",
				})
				return "", fmt.Errorf("%w: namespace %q is reserved and cannot be read via memory.get", errToolRefused, redact.String(ns))
			}
			entry, err := st.GetMemory(ctx, ns, key)
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
		Handler: func(ctx context.Context, wl domain.Workload, taskID string, args json.RawMessage) (string, error) {
			var in struct {
				Namespace string `json:"namespace"`
				Query     string `json:"query"`
				Limit     int    `json:"limit"`
			}
			if len(bytes.TrimSpace(args)) > 0 {
				if err := json.Unmarshal(args, &in); err != nil {
					return "", err
				}
			}
			ns := strings.TrimSpace(in.Namespace)
			if isReservedReadNamespace(ns) {
				auditMemory(ctx, gate, st, taskID, wl.Metadata.Name, "memory.search", "denied", map[string]string{
					"namespace": ns,
					"query":     in.Query,
					"reason":    "reserved_namespace",
				})
				return "", fmt.Errorf("%w: namespace %q is reserved and cannot be searched via memory.search", errToolRefused, redact.String(ns))
			}
			items, err := searchVisibleMemory(ctx, st, ns, in.Query, in.Limit)
			if err != nil {
				return "", err
			}
			out, _ := json.Marshal(items)
			return string(out), nil
		},
	})
}

func searchVisibleMemory(ctx context.Context, st store.Store, namespace, query string, limit int) ([]domain.MemoryEntry, error) {
	if limit <= 0 {
		limit = 50
	}
	fetchLimit := limit
	for {
		items, err := st.SearchMemory(ctx, namespace, query, fetchLimit)
		if err != nil {
			return nil, err
		}
		visible := make([]domain.MemoryEntry, 0, min(len(items), limit))
		for _, item := range items {
			if !isReservedReadNamespace(item.Namespace) {
				visible = append(visible, item)
				if len(visible) == limit {
					return visible, nil
				}
			}
		}
		if len(items) < fetchLimit || fetchLimit == math.MaxInt {
			return visible, nil
		}
		// The store limits all matches. Fetch a larger prefix when hidden rows fill part of it.
		fetchLimit += min(fetchLimit, math.MaxInt-fetchLimit)
	}
}
