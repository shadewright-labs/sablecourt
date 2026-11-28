package mcpgate

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/shadewright-labs/sablecourt/internal/config"
	"github.com/shadewright-labs/sablecourt/internal/domain"
	"github.com/shadewright-labs/sablecourt/internal/outbound"
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

// RegisterUpstreamTools adds the tools of servers to the upstream registry
// and keeps bridge for later reloads. Each tool is exposed under
// "<serverName>.<toolName>" and evaluated by the Policy Engine as
// "mcp:<serverName>.<toolName>" before any outbound HTTP call is made. If any
// tool takes a built-in tool name or falls in a built-in namespace, nothing is
// added, the bridge is not kept, and the error names every refused tool.
func (g *Gate) RegisterUpstreamTools(servers []domain.UpstreamMCPServer, bridge *UpstreamBridge) error {
	if bridge == nil {
		return nil
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	next := maps.Clone(g.upstream)
	if err := g.addUpstreamLocked(next, servers, bridge); err != nil {
		return err
	}
	g.upstream = next
	g.bridge = bridge
	return nil
}

// SetUpstreamBridge keeps bridge and rebuilds the upstream registry from the
// current bundle; later calls to Reload rebuild it from each new bundle. If
// any upstream tool is refused, the registry is left empty and the error
// names every refused tool.
func (g *Gate) SetUpstreamBridge(bridge *UpstreamBridge) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.bridge = bridge
	g.upstream = make(map[string]ToolDef)
	if bridge == nil {
		return nil
	}
	next, err := g.buildUpstreamLocked(g.bundle, bridge)
	if err != nil {
		return err
	}
	g.upstream = next
	return nil
}

// CheckReload reports whether Reload would accept bundle, without changing
// anything. The daemon calls it before it applies a new bundle anywhere.
func (g *Gate) CheckReload(bundle *config.Bundle) error {
	g.mu.RLock()
	defer g.mu.RUnlock()
	if g.bridge == nil {
		return nil
	}
	if _, err := g.buildUpstreamLocked(bundle, g.bridge); err != nil {
		return fmt.Errorf("reload refused: %w", err)
	}
	return nil
}

// buildUpstreamLocked returns the upstream registry for bundle's mcpServers,
// in workload name order, or an error naming every refused tool. g.mu must be
// held.
func (g *Gate) buildUpstreamLocked(bundle *config.Bundle, bridge *UpstreamBridge) (map[string]ToolDef, error) {
	next := make(map[string]ToolDef)
	var errs []error
	if bundle != nil {
		names := slices.Sorted(maps.Keys(bundle.Workloads))
		for _, n := range names {
			if err := g.addUpstreamLocked(next, bundle.Workloads[n].Spec.Grants.MCPServers, bridge); err != nil {
				errs = append(errs, err)
			}
		}
	}
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	return next, nil
}

// addUpstreamLocked adds the tools of servers to into, refusing any that
// would take a built-in name. g.mu must be held.
func (g *Gate) addUpstreamLocked(into map[string]ToolDef, servers []domain.UpstreamMCPServer, bridge *UpstreamBridge) error {
	var errs []error
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
			if _, builtin := g.tools[fullName]; builtin {
				errs = append(errs, fmt.Errorf("upstream MCP tool %q refused: a built-in tool has that name", fullName))
				continue
			}
			if ns, reserved := config.BuiltinToolNamespace(fullName); reserved {
				errs = append(errs, fmt.Errorf("upstream MCP tool %q refused: %q is a built-in tool namespace", fullName, ns))
				continue
			}
			into[fullName] = ToolDef{
				Name:        fullName,
				Description: fmt.Sprintf("Proxied MCP tool %q on upstream server %q", toolName, srvCfg.Name),
				InputSchema: json.RawMessage(`{"type":"object"}`),
				Handler: func(ctx context.Context, _ domain.Workload, _ string, args json.RawMessage) (string, error) {
					return bridge.CallTool(ctx, srvCfg, toolName, args)
				},
			}
		}
	}
	return errors.Join(errs...)
}

func (b *UpstreamBridge) CallTool(ctx context.Context, srv domain.UpstreamMCPServer, toolName string, args json.RawMessage) (string, error) {
	field := fmt.Sprintf("upstream MCP server %q url", srv.Name)
	if err := outbound.Check(field, srv.URL); err != nil {
		return "", err
	}
	u, err := url.Parse(strings.TrimSpace(srv.URL))
	if err != nil {
		return "", fmt.Errorf("%s: not a valid URL", field)
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
		var shortErr *redact.ShortSecretError
		if errors.As(err, &shortErr) {
			return "", err
		}
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

	resp, err := outbound.Client(b.Client, field).Do(req)
	if err != nil {
		// The *url.Error text repeats the URL, user name included.
		if ue := (*url.Error)(nil); errors.As(err, &ue) {
			err = ue.Err
		}
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
