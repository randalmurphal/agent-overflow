package claude

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// MCPSetServersResult is the diff Claude returns from a
// `control_request{subtype:"mcp_set_servers"}` round-trip. Added /
// Removed enumerate which servers actually changed; Errors maps a
// server name to the failure surfaced during reconcile (e.g.
// connection refused). The presence of an entry in Errors does NOT
// mean we should fail loudly here — that's up to the caller; a
// non-fatal startup error per server is the documented behavior
// (the rest of the configured servers still come up).
type MCPSetServersResult struct {
	Added   []string          `json:"added,omitempty"`
	Removed []string          `json:"removed,omitempty"`
	Errors  map[string]string `json:"errors,omitempty"`
}

// SetMCPServers reconciles the live session's MCP server list against
// the desired full set. Sends `control_request{subtype:"mcp_set_servers"}`
// and waits for the CLI's diff response. The `servers` argument MUST be
// the complete desired set — names not present are removed in-process,
// names already present pass through, new names spawn a fresh server
// connection. The map shape is the same as Config.MCPServers; this method
// re-applies `withClaudeTransportType` so callers can pass the same merged
// map they pass at launch (where design MCP entries arrive untagged with
// just a `url` field and need backfilling).
//
// On success the CLI handles add/remove/reload synchronously and the
// next user turn sees the updated tool list. Returns the typed diff so
// the App layer can surface "1 server added, 1 failed to start" to the
// UI.
func (s *Session) SetMCPServers(ctx context.Context, servers map[string]any) (*MCPSetServersResult, error) {
	stamped := make(map[string]any, len(servers))
	for name, spec := range servers {
		stamped[name] = withClaudeTransportType(spec)
	}
	res, err := s.sendControlRequest(ctx, "mcp_set_servers", map[string]any{
		"subtype": "mcp_set_servers",
		"servers": stamped,
	})
	if err != nil {
		return nil, err
	}
	if !res.ok {
		if res.errMsg == "" {
			return nil, fmt.Errorf("claude: mcp_set_servers: provider returned unspecified error")
		}
		return nil, fmt.Errorf("claude: mcp_set_servers: %s", res.errMsg)
	}
	out := &MCPSetServersResult{}
	if len(res.payload) > 0 {
		if err := json.Unmarshal(res.payload, out); err != nil {
			// Reconcile succeeded on the CLI side; an opaque payload
			// shouldn't fail the call. Log via the wrapped error and
			// return an empty diff so the caller still treats the
			// outcome as success.
			return &MCPSetServersResult{}, nil
		}
	}
	return out, nil
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
