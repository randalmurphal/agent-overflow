package codex

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// MCPAuthResult mirrors the `McpServerOauthLoginResponse` returned by
// Codex's `mcpServer/oauth/login` JSON-RPC: a single `authorizationUrl`
// the wrapper must open. Codex spawns its own loopback listener (port
// configured by `mcp_oauth_callback_port` / `mcp_oauth_callback_url` in
// `~/.codex/config.toml`) and emits an
// `mcpServer/oauthLogin/completed` notification when the round-trip
// finishes.
type MCPAuthResult struct {
	AuthorizationURL string `json:"authorizationUrl"`
}

// AuthenticateMCP starts the OAuth handshake for a streamable-HTTP MCP
// server registered with Codex. The CLI returns the URL we should open
// in the user's browser. Completion is signalled asynchronously via
// `mcpServer/oauthLogin/completed` (`success` + optional `error`).
//
// IMPORTANT (Codex constraint): the OAuth handler reads from the live
// on-disk config (`load_latest_config`), NOT from per-thread
// `configOverrides`. A library server declared only in the AO database
// won't be visible to Codex's OAuth path. Callers that need OAuth on an
// AO-managed server must write the server into `~/.codex/config.toml`
// before invoking this method, OR fall back to env-var bearer tokens.
// See codex-rs/app-server/src/request_processors/mcp_processor.rs
// (`mcp_server_oauth_login_response`).
func (s *Session) AuthenticateMCP(ctx context.Context, serverName string, scopes []string, timeoutSecs int64) (*MCPAuthResult, error) {
	serverName = strings.TrimSpace(serverName)
	if serverName == "" {
		return nil, fmt.Errorf("codex: mcpServer/oauth/login: server name required")
	}
	params := map[string]any{
		"name": serverName,
	}
	if len(scopes) > 0 {
		params["scopes"] = scopes
	}
	if timeoutSecs > 0 {
		params["timeoutSecs"] = timeoutSecs
	}
	resp, err := s.sendRequest(ctx, "mcpServer/oauth/login", params)
	if err != nil {
		return nil, fmt.Errorf("codex: mcpServer/oauth/login %s: %w", serverName, err)
	}
	out := &MCPAuthResult{}
	if len(resp) > 0 {
		if err := json.Unmarshal(resp, out); err != nil {
			return nil, fmt.Errorf("codex: mcpServer/oauth/login %s: decode response: %w", serverName, err)
		}
	}
	if out.AuthorizationURL == "" {
		return nil, fmt.Errorf("codex: mcpServer/oauth/login %s: empty authorizationUrl in response", serverName)
	}
	return out, nil
}

// RefreshMCPServers asks Codex to re-read its on-disk MCP server config
// and apply changes to the live runtime. Used after AO touches
// `~/.codex/config.toml` so the change takes effect without a session
// restart. The request returns an empty success envelope.
func (s *Session) RefreshMCPServers(ctx context.Context) error {
	_, err := s.sendRequest(ctx, "config/mcpServer/reload", nil)
	if err != nil {
		return fmt.Errorf("codex: config/mcpServer/reload: %w", err)
	}
	return nil
}

// WriteMCPServerToUserConfig upserts a single MCP server entry into the
// user's `~/.codex/config.toml` via the `config/batchWrite` JSON-RPC and
// hot-reloads the running app-server's loaded threads in one round-trip.
// Used by the OAuth flow: Codex's `mcpServer/oauth/login` reads from
// disk config, so an AO-only library server has to exist on disk before
// authentication can begin.
//
// `Replace` merge strategy is intentional — we want the server entry to
// be exactly the rendered spec, not partial-merged with whatever the
// user might have hand-edited there. `Upsert` is for sparse value
// patches (toggling one field on a nested table); a server definition
// is a whole object, not a delta.
//
// IMPORTANT: writes are not torn down on thread close. Once a server
// is on disk it stays there. Cross-thread isolation is enforced via the
// `enabled: false` overlay in `mcp.MergeForProvider`, not by cleaning
// up the disk write.
func (s *Session) WriteMCPServerToUserConfig(ctx context.Context, name string, spec map[string]any) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return fmt.Errorf("codex: config/batchWrite mcp: server name required")
	}
	if len(spec) == 0 {
		return fmt.Errorf("codex: config/batchWrite mcp %s: empty spec", name)
	}
	params := map[string]any{
		"edits": []map[string]any{
			{
				"keyPath":       "mcp_servers." + name,
				"value":         spec,
				"mergeStrategy": "replace",
			},
		},
		"reloadUserConfig": true,
	}
	_, err := s.sendRequest(ctx, "config/batchWrite", params)
	if err != nil {
		return fmt.Errorf("codex: config/batchWrite mcp %s: %w", name, err)
	}
	return nil
}

// MCPServerStatus is one entry returned by `mcpServerStatus/list`.
// Tools carries the wire's name→definition map so callers can list a
// server's tool names; values (schemas) are never inspected, and
// `resources` / `resourceTemplates` are not decoded. AuthStatus is one
// of `"unknown" | "unsupported" | "notLoggedIn" | "bearerToken" |
// "oAuth"` per the Codex enum (`unknown` added in 0.147 — see
// MCPStatusFromList for why it does not mean "not connected").
//
// ServerInfo is non-nil exactly when the server's `initialize`
// succeeded — MCP makes the field mandatory in a successful initialize
// response and codex fills it at every detail level. Since a list
// response only describes settled connection attempts, its absence is
// what proves a failure (see MCPStatusFromList). Only presence-safe
// identity fields are decoded; server config never enters this shape.
type MCPServerStatus struct {
	Name       string                     `json:"name"`
	AuthStatus string                     `json:"authStatus"`
	ServerInfo *MCPServerInfo             `json:"serverInfo"`
	Tools      map[string]json.RawMessage `json:"tools"`
}

// MCPServerInfo is the identity block an MCP server returns from a
// successful `initialize`, as `mcpServerStatus/list` echoes it back.
// Only presence is load-bearing (see MCPStatusFromList); the decoded
// fields are for forensics. The wire's remaining per-server payload —
// command, args, env, headers — is deliberately NOT decoded anywhere in
// this package: it can hold live tokens and these shapes feed
// wire-facing status rows (see mcpstatus.ServerStatus's own rationale).
type MCPServerInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// ToolNames returns the entry's tool names, sorted.
func (m MCPServerStatus) ToolNames() []string {
	return sortedToolNames(m.Tools)
}

// MCPServerStatusList is the response shape: paginated list of statuses.
type MCPServerStatusList struct {
	Data       []MCPServerStatus `json:"data"`
	NextCursor *string           `json:"nextCursor,omitempty"`
}

// ListMCPServerStatuses asks Codex for the current auth/connection
// status and tool inventory of every MCP server visible to THIS
// session's root thread. The `threadId` param is what scopes the answer
// to the thread — without it the app-server answers for the global
// config view, which can omit thread-scoped servers (plugins, project
// layers). Detail stays "toolsAndAuthOnly": full detail additionally
// ships resources and resource templates the status surface has no use
// for.
func (s *Session) ListMCPServerStatuses(ctx context.Context) (*MCPServerStatusList, error) {
	params := map[string]any{
		"detail": "toolsAndAuthOnly",
	}
	if id := s.rootThreadID(); id != "" {
		params["threadId"] = id
	}
	resp, err := s.sendRequest(ctx, "mcpServerStatus/list", params)
	if err != nil {
		return nil, fmt.Errorf("codex: mcpServerStatus/list: %w", err)
	}
	out := &MCPServerStatusList{}
	if len(resp) > 0 {
		if err := json.Unmarshal(resp, out); err != nil {
			return nil, fmt.Errorf("codex: mcpServerStatus/list: decode response: %w", err)
		}
	}
	return out, nil
}
