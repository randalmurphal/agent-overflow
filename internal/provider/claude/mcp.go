package claude

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// ToggleMCPServer sends `control_request{subtype:"mcp_toggle"}` for one
// server. This is the CLI's own persist-and-apply toggle: disabling
// disconnects the server, strips its tools from the model's view, and
// writes the name into the session cwd's `disabledMcpServers` list;
// enabling reverses all three. Plugin servers toggle by their
// qualified `plugin:<plugin>:<server>` name. Verified live on 2.1.219
// (2026-08-04 spike); the config write is debounced CLI-side, so
// callers must not read the config file back to confirm it.
func (s *Session) ToggleMCPServer(ctx context.Context, serverName string, enabled bool) error {
	serverName = strings.TrimSpace(serverName)
	if serverName == "" {
		return fmt.Errorf("claude: mcp_toggle: server name required")
	}
	res, err := s.sendControlRequest(ctx, "mcp_toggle "+serverName, map[string]any{
		"subtype":    "mcp_toggle",
		"serverName": serverName,
		"enabled":    enabled,
	})
	if err != nil {
		return err
	}
	return interpretControlResponse(res, "mcp_toggle "+serverName)
}

// ReconnectMCPServer sends `control_request{subtype:"mcp_reconnect"}`
// for a disconnected or failed server. The CLI re-runs the connection
// in place — the fix for "authenticated the server after the session
// spawned" without killing the session. A server that still cannot
// connect surfaces as an error response with the connection failure.
// Verified live on 2.1.219 (2026-08-04 spike: failed → fixed →
// reconnect → connected with tools restored).
func (s *Session) ReconnectMCPServer(ctx context.Context, serverName string) error {
	serverName = strings.TrimSpace(serverName)
	if serverName == "" {
		return fmt.Errorf("claude: mcp_reconnect: server name required")
	}
	res, err := s.sendControlRequest(ctx, "mcp_reconnect "+serverName, map[string]any{
		"subtype":    "mcp_reconnect",
		"serverName": serverName,
	})
	if err != nil {
		return err
	}
	return interpretControlResponse(res, "mcp_reconnect "+serverName)
}

// MCPAuthResult mirrors the response shape of mcp_authenticate. AuthURL
// is the OAuth URL the App must open; RequiresUserAction is the CLI's
// signal that the wrapper / user is expected to drive the next step
// (open browser, paste callback) — false would mean the CLI completed
// the flow itself, which doesn't happen in headless agent-overflow.
type MCPAuthResult struct {
	AuthURL            string `json:"authUrl"`
	RequiresUserAction bool   `json:"requiresUserAction"`
}

// AuthenticateMCP starts the OAuth handshake for an http/sse MCP
// server that reported needs-auth. The CLI starts a localhost
// callback listener (random port in 49152-65535 / 39152-49151 on
// Windows; can be overridden via MCP_OAUTH_CALLBACK_PORT) and returns
// the authorization URL the App should open. The App can let the
// browser hit the loopback callback directly, OR capture the
// redirect URL and complete via CompleteMCPOAuth.
func (s *Session) AuthenticateMCP(ctx context.Context, serverName string) (*MCPAuthResult, error) {
	serverName = strings.TrimSpace(serverName)
	if serverName == "" {
		return nil, fmt.Errorf("claude: mcp_authenticate: server name required")
	}
	res, err := s.sendControlRequest(ctx, "mcp_authenticate "+serverName, map[string]any{
		"subtype":     "mcp_authenticate",
		"server_name": serverName,
	})
	if err != nil {
		return nil, err
	}
	if !res.ok {
		if res.errMsg == "" {
			return nil, fmt.Errorf("claude: mcp_authenticate %s: provider returned unspecified error", serverName)
		}
		return nil, fmt.Errorf("claude: mcp_authenticate %s: %s", serverName, res.errMsg)
	}
	out := &MCPAuthResult{}
	if len(res.payload) > 0 {
		if err := json.Unmarshal(res.payload, out); err != nil {
			return nil, fmt.Errorf("claude: mcp_authenticate %s: decode response: %w", serverName, err)
		}
	}
	if out.AuthURL == "" {
		return nil, fmt.Errorf("claude: mcp_authenticate %s: empty authUrl in response", serverName)
	}
	return out, nil
}

// MCPServerStatus is a slim projection of a single entry in the
// `control_response.response.mcpServers[]` array Claude returns for
// `control_request{subtype:"mcp_status"}`. We decode name, raw status,
// scope, error, and tool NAMES. The CLI also returns `serverInfo` and
// `config` per entry — `config` is deliberately never decoded: it
// carries the server's args/env verbatim, which can hold live tokens
// (a real GITLAB_TOKEN was observed in the 2026-08-03 probe), and this
// shape flows to the `mcp:status` wire channel.
//
// Use [MCPStatusFromRaw] to project Status onto the unified
// `mcpstatus.Status` enum.
type MCPServerStatus struct {
	Name   string        `json:"name"`
	Status string        `json:"status"`
	Scope  string        `json:"scope,omitempty"`
	Error  string        `json:"error,omitempty"`
	Tools  []MCPToolInfo `json:"tools,omitempty"`
}

// MCPToolInfo is the name-only projection of a tool entry in the
// `mcp_status` response. Schemas and annotations are dropped by not
// being declared, and `config` (args/env can carry live tokens) is
// never decoded for the same reason.
type MCPToolInfo struct {
	Name string `json:"name"`
}

// ToolNames returns the entry's tool names in wire order.
func (m MCPServerStatus) ToolNames() []string {
	if len(m.Tools) == 0 {
		return nil
	}
	names := make([]string, 0, len(m.Tools))
	for _, t := range m.Tools {
		if t.Name != "" {
			names = append(names, t.Name)
		}
	}
	return names
}

// QueryMCPStatus sends `control_request{subtype:"mcp_status"}` and
// returns the current MCP server status as the CLI sees it. The
// response is built from three in-memory client pools
// (`currentMcpClients`, `sdkClients`, `dynamicMcpState.clients`);
// servers that were configured but never attempted may be absent.
// Callers that need to detect "still pending" should treat a missing
// entry as a keep-retrying signal, not a terminal state.
//
// Used by the OAuth-completion poller in `app_mcp_auth.go`: after
// `AuthenticateMCP` returns the auth URL and the user finishes the
// browser hop, AO polls this synchronously until the named server
// flips out of `needs-auth`. The handler runs entirely against the
// CLI's local state — no API call, no token cost.
//
// Wire shape verified against Claude CLI 2.1.139 (May 2026 spike);
// see `docs/references/claude-wire.md#control_request` for the
// canonical response example.
func (s *Session) QueryMCPStatus(ctx context.Context) ([]MCPServerStatus, error) {
	res, err := s.sendControlRequest(ctx, "mcp_status", map[string]any{
		"subtype": "mcp_status",
	})
	if err != nil {
		return nil, err
	}
	if !res.ok {
		if res.errMsg == "" {
			return nil, fmt.Errorf("claude: mcp_status: provider returned unspecified error")
		}
		return nil, fmt.Errorf("claude: mcp_status: %s", res.errMsg)
	}
	if len(res.payload) == 0 {
		return nil, nil
	}
	var decoded struct {
		MCPServers []MCPServerStatus `json:"mcpServers"`
	}
	if err := json.Unmarshal(res.payload, &decoded); err != nil {
		return nil, fmt.Errorf("claude: mcp_status: decode response: %w", err)
	}
	return decoded.MCPServers, nil
}

// CompleteMCPOAuth posts the captured callback URL back to the CLI to
// finish the OAuth handshake when the browser landed somewhere other
// than the local loopback listener. The CLI parses `code` + `state`
// from any URL we hand it, so anything resembling
// `http://*?code=XXX&state=YYY` is accepted. On success the CLI fires
// `mcp_reconnect` internally and the next tool-list refresh picks up
// the now-authed tools.
func (s *Session) CompleteMCPOAuth(ctx context.Context, serverName, callbackURL string) error {
	serverName = strings.TrimSpace(serverName)
	if serverName == "" {
		return fmt.Errorf("claude: mcp_oauth_callback_url: server name required")
	}
	callbackURL = strings.TrimSpace(callbackURL)
	if callbackURL == "" {
		return fmt.Errorf("claude: mcp_oauth_callback_url: callback url required")
	}
	res, err := s.sendControlRequest(ctx, "mcp_oauth_callback_url "+serverName, map[string]any{
		"subtype":      "mcp_oauth_callback_url",
		"server_name":  serverName,
		"callback_url": callbackURL,
	})
	if err != nil {
		return err
	}
	return interpretControlResponse(res, "mcp_oauth_callback_url "+serverName)
}
