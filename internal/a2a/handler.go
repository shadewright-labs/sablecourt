package a2a

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strings"

	"github.com/shadewright-labs/sablecourt/internal/controlplane"
	"github.com/shadewright-labs/sablecourt/internal/redact"
	"github.com/shadewright-labs/sablecourt/internal/secret"
	"github.com/shadewright-labs/sablecourt/internal/store"
)

type Options struct {
	Store      store.Store
	Reconciler *controlplane.Reconciler
	Secrets    secret.SecretStore
	SecretName string
}

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      any             `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

// BrokerHandler implements a policy-governed A2A JSON-RPC endpoint supporting
// tasks/send and tasks/get, authenticated via an operator-configured bearer token.
func BrokerHandler(opts Options) http.Handler {
	secName := opts.SecretName
	if secName == "" {
		secName = "a2a_bearer_token"
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeRPCError(w, nil, -32600, "POST required")
			return
		}
		if opts.Secrets == nil || opts.Reconciler == nil || opts.Store == nil {
			writeRPCError(w, nil, -32000, "A2A broker unavailable")
			return
		}
		expectedToken, err := opts.Secrets.Get(r.Context(), secName)
		if err != nil || strings.TrimSpace(expectedToken) == "" {
			w.WriteHeader(http.StatusUnauthorized)
			writeRPCError(w, nil, -32001, "A2A bearer token not configured")
			return
		}
		// A token too short to redact is refused; the caller is not
		// authenticated yet, so the reply does not say why.
		if err := redact.RegisterSetting(secName, expectedToken); err != nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			writeRPCError(w, nil, -32001, "A2A bearer token refused")
			return
		}
		auth := r.Header.Get("Authorization")
		gotToken := strings.TrimSpace(strings.TrimPrefix(auth, "Bearer "))
		if !strings.HasPrefix(auth, "Bearer ") || subtle.ConstantTimeCompare([]byte(gotToken), []byte(strings.TrimSpace(expectedToken))) != 1 {
			w.WriteHeader(http.StatusUnauthorized)
			writeRPCError(w, nil, -32001, "unauthorized")
			return
		}
		// The observer secret is read-only: a deployment that reuses its value
		// for A2A must not turn the observer into a task submitter.
		observer, err := opts.Secrets.Get(r.Context(), "observer_bearer_token")
		if err != nil && !errors.Is(err, secret.ErrSecretNotFound) {
			log.Printf("a2a: observer secret lookup failed: %v", err)
			w.WriteHeader(http.StatusServiceUnavailable)
			writeRPCError(w, nil, -32000, "A2A authorization unavailable")
			return
		}
		if err == nil &&
			strings.TrimSpace(observer) != "" && subtle.ConstantTimeCompare([]byte(gotToken), []byte(strings.TrimSpace(observer))) == 1 {
			w.WriteHeader(http.StatusForbidden)
			writeRPCError(w, nil, -32001, "observer credential cannot use A2A")
			return
		}

		var req rpcRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeRPCError(w, nil, -32700, "invalid JSON-RPC request")
			return
		}

		switch req.Method {
		case "tasks/send":
			var p struct {
				ID       string `json:"id"`
				Workload string `json:"workload"`
				Message  struct {
					Parts []struct {
						Text string `json:"text"`
					} `json:"parts"`
				} `json:"message"`
			}
			if err := json.Unmarshal(req.Params, &p); err != nil || p.Workload == "" {
				writeRPCError(w, req.ID, -32602, "workload is required")
				return
			}
			var texts []string
			for _, part := range p.Message.Parts {
				if part.Text != "" {
					texts = append(texts, part.Text)
				}
			}
			inBytes, _ := json.Marshal(map[string]string{"prompt": strings.Join(texts, "\n")})
			task, err := opts.Reconciler.SubmitTask(r.Context(), p.Workload, p.ID, string(inBytes))
			if err != nil {
				writeRPCError(w, req.ID, -32002, err.Error())
				return
			}
			writeRPCResult(w, req.ID, map[string]any{
				"id":       task.ID,
				"workload": task.WorkloadName,
				"status":   map[string]any{"state": task.State},
			})

		case "tasks/get":
			var p struct {
				ID string `json:"id"`
			}
			if err := json.Unmarshal(req.Params, &p); err != nil || p.ID == "" {
				writeRPCError(w, req.ID, -32602, "id is required")
				return
			}
			task, err := opts.Store.GetTask(r.Context(), p.ID)
			if err != nil {
				writeRPCError(w, req.ID, -32004, "task not found")
				return
			}
			msgs, _ := opts.Store.ListTaskMessages(r.Context(), p.ID)
			task = controlplane.TaskForRead(task)
			msgs = controlplane.TaskMessagesForRead(msgs)
			writeRPCResult(w, req.ID, map[string]any{
				"id":        task.ID,
				"workload":  task.WorkloadName,
				"status":    map[string]any{"state": task.State},
				"messages":  msgs,
				"artifacts": json.RawMessage(task.ArtifactsJSON),
			})

		default:
			writeRPCError(w, req.ID, -32601, "method not found")
		}
	})
}

func writeRPCResult(w http.ResponseWriter, id any, result any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"result":  result,
	})
}

func writeRPCError(w http.ResponseWriter, id any, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"error":   map[string]any{"code": code, "message": msg},
	})
}
