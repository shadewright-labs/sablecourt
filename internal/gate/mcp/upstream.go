package mcpgate

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/shadewright-labs/sablecourt/internal/domain"
	"github.com/shadewright-labs/sablecourt/internal/redact"
	"github.com/shadewright-labs/sablecourt/internal/secret"
)

// UpstreamBridge proxies allowlisted tool calls to remote MCP Streamable HTTP servers
// while injecting upstream secrets strictly inside sablecourtd and redacting all responses.
type UpstreamBridge struct {
	Secrets secret.SecretStore
	Client  *http.Client
}

func NewUpstreamBridge(secrets secret.SecretStore, client *http.Client) *UpstreamBridge {
	if client == nil {
		client = &http.Client{Timeout: 45 * time.Second}
	}
	return &UpstreamBridge{Secrets: secrets, Client: client}
}

// RegisterUpstreamTools wires declared UpstreamMCPServer tools for a workload into the Gate.
// Each tool is exposed under "<serverName>.<toolName>" and evaluated by the Policy Engine
// as "mcp:<serverName>.<toolName>" before any outbound HTTP call is made.
func (g *Gate) RegisterUpstreamTools(servers []domain.UpstreamMCPServer, bridge *UpstreamBridge) {
	if bridge == nil {
		return
	}
	for _, srv := range servers {
		srvCfg := srv
		if strings.TrimSpace(srvCfg.Name) == "" || strings.TrimSpace(srvCfg.URL) == "" {
			continue
		}
		for _, tool := range srvCfg.AllowedTools {
			toolName := strings.TrimSpace(tool)
			if toolName == "" {
				continue
			}
			fullName := srvCfg.Name + "." + toolName
			g.RegisterTool(ToolDef{
				Name:        fullName,
				Description: fmt.Sprintf("Proxied MCP tool %q on upstream server %q", toolName, srvCfg.Name),
				Handler: func(ctx context.Context, _ domain.Workload, _ string, args json.RawMessage) (string, error) {
					return bridge.CallTool(ctx, srvCfg, toolName, args)
				},
			})
		}
	}
}

func (b *UpstreamBridge) CallTool(ctx context.Context, srv domain.UpstreamMCPServer, toolName string, args json.RawMessage) (string, error) {
	u, err := url.Parse(strings.TrimSpace(srv.URL))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", fmt.Errorf("invalid upstream MCP server URL %q", srv.URL)
	}
	var parsedArgs any
	if len(args) > 0 {
		if err := json.Unmarshal(args, &parsedArgs); err != nil {
			parsedArgs = map[string]any{}
		}
	} else {
		parsedArgs = map[string]any{}
	}

	rpcBody, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/call",
		"params": map[string]any{
			"name":      toolName,
			"arguments": parsedArgs,
		},
	})
	if err != nil {
		return "", err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), bytes.NewReader(rpcBody))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	if srv.SecretName != "" {
		if b.Secrets == nil {
			return "", errors.New("upstream MCP secret store unavailable")
		}
		val, err := b.Secrets.Get(ctx, srv.SecretName)
		if err != nil || strings.TrimSpace(val) == "" {
			return "", fmt.Errorf("upstream MCP secret %q unavailable", srv.SecretName)
		}
		if err := redact.RegisterSetting(srv.SecretName, val); err != nil {
			return "", err
		}
		hdr := strings.TrimSpace(srv.AuthHeader)
		if hdr == "" {
			hdr = "Authorization"
		}
		prefix := srv.AuthPrefix
		if prefix == "" && strings.EqualFold(hdr, "Authorization") {
			prefix = "Bearer "
		}
		req.Header.Set(hdr, prefix+strings.TrimSpace(val))
	}

	resp, err := b.Client.Do(req)
	if err != nil {
		return "", fmt.Errorf("upstream MCP request failed: %w", err)
	}
	defer resp.Body.Close()

	rawResp, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return "", err
	}
	if resp.StatusCode >= 300 {
		return "", fmt.Errorf("upstream MCP server returned HTTP %d: %s", resp.StatusCode, redact.Text(string(rawResp)).Text)
	}

	var rpcResp struct {
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rawResp, &rpcResp); err == nil {
		if rpcResp.Error != nil {
			return "", fmt.Errorf("upstream MCP error %d: %s", rpcResp.Error.Code, redact.Text(rpcResp.Error.Message).Text)
		}
		if len(rpcResp.Result) > 0 {
			return redact.Text(string(rpcResp.Result)).Text, nil
		}
	}
	return redact.Text(string(rawResp)).Text, nil
}
