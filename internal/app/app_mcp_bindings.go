package app

import (
	"context"
	"errors"
	"io/fs"
	"time"

	"agent-overflow/internal/claudeconfig"
	"agent-overflow/internal/mcpapp"
	"agent-overflow/internal/mcpstatus"
	"agent-overflow/internal/provider/claude"
	"agent-overflow/internal/provider/codex"
)

const (
	mcpRowSourceSession = "session"
	mcpRowSourceConfig  = "config"
)

type claudeMCPStatusQuerier func(context.Context) ([]claude.MCPServerStatus, error)

var defaultClaudeMCPOAuthIntervals = mcpapp.DefaultClaudeMCPOAuthIntervals()

// ErrMCPProviderUnsupported reports an inbound binding call carrying a
// provider value AO doesn't currently route MCP for. New providers
// register here when they ship.
var ErrMCPProviderUnsupported = mcpapp.ErrMCPProviderUnsupported

// ErrMCPSessionUnavailable fires when a thread's provider session is
// not live (and auto-start failed) so a binding cannot drive the
// provider-side operation it needs.
var ErrMCPSessionUnavailable = mcpapp.ErrMCPSessionUnavailable

// MCPAuthInitResult is the response shape for TriggerMcpAuth.
type MCPAuthInitResult struct {
	AuthURL            string `json:"authUrl"`
	Provider           string `json:"provider"`
	RequiresUserAction bool   `json:"requiresUserAction"`
}

// ThreadMCPServer is the per-thread MCP row the composer menu renders.
// The provider session is the source of truth for what a thread can
// actually see (Source "session"); when no session is live the row
// comes from provider-native config plus the status cache (Source
// "config"). The shape deliberately carries no command/args/env/headers
// — server config can hold live tokens and this shape flows to the
// wire (see claude.MCPServerStatus's config rationale).
type ThreadMCPServer struct {
	Provider string `json:"provider"`
	Name     string `json:"name"`
	// Status is a mcpstatus.Status string ("connected", "needs-auth",
	// "failed", "starting", "disabled", "unknown").
	Status string   `json:"status"`
	Error  string   `json:"error,omitempty"`
	Tools  []string `json:"tools,omitempty"`
	// Scope is provider vocabulary: Claude session rows report the
	// CLI's own scope ("user", "project", "local", "dynamic" for
	// plugin servers), config rows report the claudeconfig source
	// ("user", "local", "plugin", "project"). Codex rows carry no
	// scope.
	Scope string `json:"scope,omitempty"`
	// AuthStatus is Codex's own auth enum for the row —
	// "unsupported" | "notLoggedIn" | "bearerToken" | "oAuth" | "unknown"
	// (`McpAuthStatus`, codex >= 0.147) — carried through so the UI can
	// tell a failed server that HAS an OAuth grant (offer "Sign in
	// again") from one that never needed credentials. "unknown" means
	// the server's OAuth support could not be determined, which is a
	// statement about the PROBE, not about the server: the codex
	// projection puts it in the evidence-decides set alongside
	// "unsupported"/"bearerToken"/"oAuth" rather than the give-up set
	// (internal/provider/codex/mcpstatus.go).
	// Session rows carry it from the live list; config rows carry the
	// cache's copy (the ephemeral fetch records it). Empty on Claude
	// rows and on config rows the cache has never seen.
	AuthStatus string `json:"authStatus,omitempty"`
	Disabled   bool   `json:"disabled"`
	// Source is "session" when the row is live provider truth for this
	// thread, "config" when it is the config+cache fallback.
	Source string `json:"source"`
	// Stale marks a config row whose Status is a last-known value from
	// an expired cache entry. Membership is still real — the menu keeps
	// rendering the row — but the frontend chains a background status
	// refresh instead of trusting it.
	Stale bool `json:"stale,omitempty"`
}

func (a *App) mcpService() *mcpapp.Service {
	a.mcpAppOnce.Do(func() {
		a.mcpApp = mcpapp.New(a.mcpDeps())
	})
	return a.mcpApp
}

func (a *App) mcpDeps() mcpapp.Deps {
	return mcpapp.Deps{
		Context:        a.lifeCtx,
		IsShuttingDown: a.shuttingDown.Load,
		StartSession: func(threadID string) error {
			return a.startSession(context.Background(), threadID)
		},
		Session: func(threadID string) (mcpapp.Session, bool) {
			sess, ok := a.sessionManager().get(threadID)
			if !ok {
				return mcpapp.Session{}, false
			}
			return mcpapp.Session{Claude: sess.Claude, Codex: sess.Codex}, true
		},
		CodexSessions: func() []mcpapp.CodexLiveSession {
			live := a.sessionManager().runtime.CodexMCPSessions()
			out := make([]mcpapp.CodexLiveSession, len(live))
			for i, item := range live {
				out[i] = mcpapp.CodexLiveSession{ThreadID: item.ThreadID, Session: item.Session}
			}
			return out
		},
		ClaudeSessions: func(workspacePath string) []mcpapp.ClaudeLiveSession {
			live := a.sessionManager().runtime.ClaudeMCPSessions(workspacePath)
			out := make([]mcpapp.ClaudeLiveSession, len(live))
			for i, item := range live {
				out[i] = mcpapp.ClaudeLiveSession{ThreadID: item.ThreadID, Session: item.Session}
			}
			return out
		},
		ProviderBinary: a.providerBinaryPath,
		SessionProcessEnv: func(providerName string) map[string]string {
			return a.sessionProcessEnv(providerName, nil, aoSessionCredential{})
		},
		ReadClaudeCredential: a.readClaudeMCPCredential,
		ClaudeConfigPath:     a.claudeConfigJSONPath,
		CodexConfigPath:      a.codexConfigTOMLPath,
		Emit:                 a.emit,
		EmitThreadError:      a.emitErrorToThread,
		EmitWireError:        a.emitWireErrorToThread,
		Store:                a.store,
		Logger:               a.logger,
		ShutdownError:        ErrShuttingDown,
	}
}

func (a *App) readClaudeMCPCredential() ([]byte, error) {
	if a.providerAccounts == nil {
		return nil, errors.New("provider credential store unavailable")
	}
	snapshot, present, err := a.readCanonicalCredentialIfPresent("claude")
	if err != nil {
		return nil, err
	}
	if !present {
		return nil, fs.ErrNotExist
	}
	return snapshot.Data, nil
}

func mcpAuthInitResult(result mcpapp.MCPAuthInitResult) MCPAuthInitResult {
	return MCPAuthInitResult{
		AuthURL:            result.AuthURL,
		Provider:           result.Provider,
		RequiresUserAction: result.RequiresUserAction,
	}
}

func threadMCPServers(rows []mcpapp.ThreadMCPServer) []ThreadMCPServer {
	if rows == nil {
		return nil
	}
	out := make([]ThreadMCPServer, len(rows))
	for i, row := range rows {
		out[i] = ThreadMCPServer{
			Provider: row.Provider, Name: row.Name, Status: row.Status,
			Error: row.Error, Tools: row.Tools, Scope: row.Scope,
			AuthStatus: row.AuthStatus, Disabled: row.Disabled,
			Source: row.Source, Stale: row.Stale,
		}
	}
	return out
}

// TriggerMcpAuth drives the provider-side OAuth handshake for an
// http/sse / streamable_http MCP server. The thread must be live
// because the OAuth listener is owned by the provider process; if no
// session is live, we auto-start one for the round-trip.
//
//ao:scope settings:write
func (a *App) TriggerMcpAuth(threadID, name string) (MCPAuthInitResult, error) {
	if isAppManagedMCPServer(name) {
		return MCPAuthInitResult{}, errors.New("trigger mcp auth: built-in browser does not use provider OAuth")
	}
	result, err := a.mcpService().TriggerMcpAuth(threadID, name)
	return mcpAuthInitResult(result), err
}

// ReconnectMcpServer re-runs the connection for one MCP server on the
// thread's live session — the fix for "I authenticated / repaired the
// server after the session spawned" without waiting out the idle
// reaper. Claude reconnects the named server in place (`mcp_reconnect`);
// Codex has no per-server primitive, so it re-reads config and
// re-pushes MCP state into the loaded thread (`config/mcpServer/reload`).
// Requires a live session: with none, the next session start connects
// fresh anyway, so there is nothing to fix.
//
//ao:scope settings:write
func (a *App) ReconnectMcpServer(threadID, name string) error {
	if isAppManagedMCPServer(name) {
		return errors.New("reconnect mcp server: built-in browser is controlled in Settings")
	}
	return a.mcpService().ReconnectMcpServer(threadID, name)
}

// GetMcpServerStatus returns the cached status for one server. If
// no fresh entry exists, it runs the appropriate ephemeral fetcher
// (Claude `mcp list` / Codex `mcpServerStatus/list`) to populate
// the cache, then returns the result. force=true bypasses cache
// hits but still single-flights concurrent callers.
//
//ao:scope settings:write
//ao:route home
func (a *App) GetMcpServerStatus(providerName, name string, force bool) (mcpstatus.ServerStatus, error) {
	return a.mcpService().GetMcpServerStatus(providerName, name, force)
}

// ListMcpServerStatuses returns the cached snapshot for one
// provider. Does NOT trigger an ephemeral fetch — used to render
// the popup on first open before the explicit refresh kicks in.
// Live sessions push their own state into the cache continuously,
// so for any provider that has run a thread this lifetime the
// snapshot is already populated.
//
//ao:scope settings:write
//ao:route home
func (a *App) ListMcpServerStatuses(providerName string) ([]mcpstatus.ServerStatus, error) {
	return a.mcpService().ListMcpServerStatuses(providerName)
}

// RefreshMcpServerStatus forces a fresh ephemeral fetch for the
// named provider and returns the resulting list. The menu chains this
// after a config-sourced listing whose cache looks cold, then re-lists
// — it is how enabled plugin servers (invisible in ~/.claude.json)
// reach the no-session view. workspacePath becomes the fetch cwd so
// `claude mcp list` sees the workspace's project-scope servers; pass
// "" for a workspace-agnostic refresh.
//
//ao:scope settings:write
//ao:route selected
func (a *App) RefreshMcpServerStatus(providerName, workspacePath string) ([]mcpstatus.ServerStatus, error) {
	return a.mcpService().RefreshMcpServerStatus(providerName, workspacePath)
}

// ListThreadMcpServers returns the MCP servers as THIS thread sees
// them. With a live session the provider process answers (Claude
// `mcp_status`, Codex `mcpServerStatus/list` scoped by threadId) —
// including plugin/project-layer servers AO's config files know nothing
// about. Without one, the provider-native config plus the status cache
// stand in, labeled Source "config" so the UI can say the rows describe
// what the next session will get rather than a running one.
//
//ao:scope settings:write
func (a *App) ListThreadMcpServers(threadID string) ([]ThreadMCPServer, error) {
	rows, err := a.mcpService().ListThreadMcpServers(threadID)
	if err != nil {
		return nil, err
	}
	thread, err := a.store.GetThread(threadID)
	if err != nil {
		return nil, err
	}
	_, live := a.sessionManager().get(threadID)
	return a.withBrowserMCPRow(thread, threadMCPServers(rows), live), nil
}

// ListWorkspaceMcpServers returns the config+cache view of a
// provider's MCP servers for a workspace — what a session started
// there would get. Backs the composer's draft/new-thread menu and the
// no-live-session fallback of ListThreadMcpServers. For Claude,
// membership is fully config-derived with zero spawns (user/local
// entries, enabled plugins' manifests, .mcp.json files) and
// `disabledMcpServers` is read from the workspace's canonical project
// entry — a worktree shares the main checkout's toggles, exactly like
// the CLI. For Codex the `enabled` flag is global and workspacePath is
// ignored.
//
//ao:scope settings:write
//ao:route selected
func (a *App) ListWorkspaceMcpServers(providerName, workspacePath string) ([]ThreadMCPServer, error) {
	rows, err := a.mcpService().ListWorkspaceMcpServers(providerName, workspacePath)
	return threadMCPServers(rows), err
}

// SetThreadMcpServerEnabled toggles one MCP server for a thread the
// way the thread's provider does it natively. Claude with a live
// session: `mcp_toggle`, which disconnects/reconnects the server AND
// persists the name into the canonical project entry's
// `disabledMcpServers` — the CLI owns the config write (it is
// debounced CLI-side, so AO never double-writes or reads it back to
// confirm). Claude without a live session: AO writes the same list directly,
// keyed identically (claudeconfig.ProjectKey). Codex: the global `enabled`
// flag in ~/.codex/config.toml,
// hot-reloaded into this thread's live session when there is one.
// Other running threads keep their current state until their next
// session start — provider-native semantics, by design.
//
//ao:scope settings:write
//ao:stepup
func (a *App) SetThreadMcpServerEnabled(threadID, name string, enabled bool) error {
	if isAppManagedMCPServer(name) {
		thread, err := a.store.GetThread(threadID)
		if err != nil {
			return err
		}
		return a.setBrowserThreadMCPEnabled(thread, enabled)
	}
	return a.mcpService().SetThreadMcpServerEnabled(threadID, name, enabled)
}

// SetWorkspaceMcpServerEnabled is the draft-composer counterpart of
// SetThreadMcpServerEnabled: no thread row exists yet, so the toggle
// goes straight to provider-native config for the workspace a new
// thread would start in.
//
//ao:scope settings:write
//ao:route selected
//ao:stepup
func (a *App) SetWorkspaceMcpServerEnabled(providerName, workspacePath, name string, enabled bool) error {
	if isAppManagedMCPServer(name) {
		return errors.New("set workspace mcp server enabled: built-in browser is controlled in Settings")
	}
	return a.mcpService().SetWorkspaceMcpServerEnabled(providerName, workspacePath, name, enabled)
}

// TriggerWorkspaceMcpAuth authenticates a provider/workspace MCP server
// without creating an Agent Overflow thread. A temporary provider process owns
// the loopback listener through the browser hop, then exits when completion is
// confirmed, rejected, timed out, or the app shuts down.
//
//ao:scope settings:write
//ao:route selected
func (a *App) TriggerWorkspaceMcpAuth(providerName, workspacePath, serverName string) (MCPAuthInitResult, error) {
	result, err := a.mcpService().TriggerWorkspaceMcpAuth(providerName, workspacePath, serverName)
	return mcpAuthInitResult(result), err
}

func (a *App) mcpStatus() *mcpstatus.Cache {
	return a.mcpService().StatusCache()
}

func (a *App) claudeConfig() (*claudeconfig.Store, error) {
	return a.mcpService().ClaudeConfig()
}

func (a *App) handleCodexMCPOAuthCompleted(threadID, serverName string, success bool, errMsg string) {
	a.mcpService().HandleCodexMCPOAuthCompleted(threadID, serverName, success, errMsg)
}

func (a *App) handleCodexMCPStartupUpdate(update codex.MCPStartupUpdate) {
	a.mcpService().HandleCodexMCPStartupUpdate(update)
}

func (a *App) pollClaudeMCPAfterOAuth(
	ctx context.Context,
	threadID, serverName string,
	intervals []time.Duration,
	getQuerier func() claudeMCPStatusQuerier,
) {
	a.mcpService().PollClaudeMCPAfterOAuth(ctx, threadID, serverName, intervals, func() mcpapp.ClaudeMCPStatusQuerier {
		return mcpapp.ClaudeMCPStatusQuerier(getQuerier())
	})
}
