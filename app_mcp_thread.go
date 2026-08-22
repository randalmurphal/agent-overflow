package main

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"agent-overflow/internal/claudeconfig"
	"agent-overflow/internal/mcpstatus"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/claude"
	"agent-overflow/internal/provider/codex"
	"agent-overflow/internal/store"
	"agent-overflow/internal/threadmode"
)

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

const (
	mcpRowSourceSession = "session"
	mcpRowSourceConfig  = "config"

	// mcpScopeClaudeAI marks claude.ai cloud connectors in Claude's
	// mcp_status response. Out of scope for AO's menu — they belong to
	// the claude.ai account, not the local workspace.
	mcpScopeClaudeAI = "claudeai"
)

// ListThreadMcpServers returns the MCP servers as THIS thread sees
// them. With a live session the provider process answers (Claude
// `mcp_status`, Codex `mcpServerStatus/list` scoped by threadId) —
// including plugin/project-layer servers AO's config files know nothing
// about. Without one, the provider-native config plus the status cache
// stand in, labeled Source "config" so the UI can say the rows describe
// what the next session will get rather than a running one.
func (a *App) ListThreadMcpServers(threadID string) ([]ThreadMCPServer, error) {
	if a.store == nil {
		return nil, errors.New("list thread mcp servers: store unavailable")
	}
	t, err := a.store.GetThread(threadID)
	if err != nil {
		return nil, fmt.Errorf("list thread mcp servers: load thread: %w", err)
	}
	if sess, ok := a.sessionManager().get(threadID); ok {
		ctx, cancel := context.WithTimeout(a.lifeCtx(), mcpLiveApplyTimeout)
		defer cancel()
		switch {
		case sess.claude != nil:
			rows, err := claudeSessionMCPRows(ctx, sess.claude)
			if err != nil {
				return nil, fmt.Errorf("list thread mcp servers: %w", err)
			}
			return rows, nil
		case sess.codex != nil:
			rows, err := a.codexSessionMCPRows(ctx, sess.codex)
			if err != nil {
				return nil, fmt.Errorf("list thread mcp servers: %w", err)
			}
			return rows, nil
		}
	}
	return a.ListWorkspaceMcpServers(t.Provider, t.WorkspacePath)
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
func (a *App) ListWorkspaceMcpServers(providerName, workspacePath string) ([]ThreadMCPServer, error) {
	switch providerName {
	case mcpProviderClaude:
		return a.claudeConfigMCPRows(workspacePath)
	case mcpProviderCodex:
		return a.codexConfigMCPRows()
	default:
		return nil, fmt.Errorf("%w: %s", ErrMCPProviderUnsupported, providerName)
	}
}

// SetThreadMcpServerEnabled toggles one MCP server for a thread the
// way the thread's provider does it natively. Claude with a live
// session: `mcp_toggle`, which disconnects/reconnects the server AND
// persists the name into the canonical project entry's
// `disabledMcpServers` — the CLI owns the config write (it is
// debounced CLI-side, so AO never double-writes or reads it back to
// confirm). Claude without a session (or a design session, which runs
// --strict-mcp-config and cannot see user MCP): AO writes the same
// list directly, keyed identically (claudeconfig.ProjectKey). Codex: the global `enabled` flag in ~/.codex/config.toml,
// hot-reloaded into this thread's live session when there is one.
// Other running threads keep their current state until their next
// session start — provider-native semantics, by design.
func (a *App) SetThreadMcpServerEnabled(threadID, name string, enabled bool) error {
	if a.store == nil {
		return errors.New("set thread mcp server enabled: store unavailable")
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return errors.New("set thread mcp server enabled: server name is required")
	}
	t, err := a.store.GetThread(threadID)
	if err != nil {
		return fmt.Errorf("set thread mcp server enabled: load thread: %w", err)
	}
	switch t.Provider {
	case string(provider.Claude):
		return a.setClaudeThreadMCPEnabled(t, name, enabled)
	case string(provider.Codex):
		return a.setCodexMCPEnabled(t.ID, name, enabled)
	default:
		return fmt.Errorf("%w: %s", ErrMCPProviderUnsupported, t.Provider)
	}
}

// SetWorkspaceMcpServerEnabled is the draft-composer counterpart of
// SetThreadMcpServerEnabled: no thread row exists yet, so the toggle
// goes straight to provider-native config for the workspace a new
// thread would start in.
func (a *App) SetWorkspaceMcpServerEnabled(providerName, workspacePath, name string, enabled bool) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return errors.New("set workspace mcp server enabled: server name is required")
	}
	switch providerName {
	case mcpProviderClaude:
		return a.setClaudeWorkspaceMCPDisabled(workspacePath, name, !enabled)
	case mcpProviderCodex:
		return a.setCodexMCPEnabled("", name, enabled)
	default:
		return fmt.Errorf("%w: %s", ErrMCPProviderUnsupported, providerName)
	}
}

// ---------------- Claude ----------------

func (a *App) setClaudeThreadMCPEnabled(t store.Thread, name string, enabled bool) error {
	// Design Claude sessions launch with --strict-mcp-config — user MCP
	// isn't visible to them, so only the config write (affecting future
	// non-design sessions in this workspace) applies.
	if t.Mode != threadmode.ModeDesign {
		if sess, ok := a.sessionManager().get(t.ID); ok && sess.claude != nil {
			ctx, cancel := context.WithTimeout(a.lifeCtx(), mcpLiveApplyTimeout)
			defer cancel()
			if err := sess.claude.ToggleMCPServer(ctx, name, enabled); err != nil {
				return err
			}
			a.mcpStatus().Invalidate(mcpstatus.Key{Provider: mcpstatus.ProviderClaude, Name: name})
			return nil
		}
	}
	return a.setClaudeWorkspaceMCPDisabled(t.WorkspacePath, name, !enabled)
}

func (a *App) setClaudeWorkspaceMCPDisabled(workspacePath, name string, disabled bool) error {
	workspacePath = strings.TrimSpace(workspacePath)
	if workspacePath == "" {
		return errors.New("set claude mcp disabled: workspace path is required")
	}
	st, err := a.claudeConfig()
	if err != nil {
		return err
	}
	if err := st.SetDisabled(workspacePath, name, disabled); err != nil {
		return fmt.Errorf("set claude mcp disabled: %w", err)
	}
	a.mcpStatus().Invalidate(mcpstatus.Key{Provider: mcpstatus.ProviderClaude, Name: name})
	return nil
}

// claudeSessionMCPRows projects a live session's `mcp_status` answer
// onto menu rows. claude.ai cloud connectors are filtered out; plugin
// servers arrive with scope "dynamic" and their qualified
// `plugin:<plugin>:<server>` name, which is also the name mcp_toggle
// expects back.
func claudeSessionMCPRows(ctx context.Context, sess *claude.Session) ([]ThreadMCPServer, error) {
	statuses, err := sess.QueryMCPStatus(ctx)
	if err != nil {
		return nil, err
	}
	rows := make([]ThreadMCPServer, 0, len(statuses))
	for _, st := range statuses {
		if st.Scope == mcpScopeClaudeAI {
			continue
		}
		mapped := claude.MCPStatusFromRaw(st.Status)
		rows = append(rows, ThreadMCPServer{
			Provider: mcpProviderClaude,
			Name:     st.Name,
			Status:   string(mapped),
			Error:    sanitizeMCPError(st.Error),
			Tools:    st.ToolNames(),
			Scope:    st.Scope,
			Disabled: mapped == mcpstatus.StatusDisabled,
			Source:   mcpRowSourceSession,
		})
	}
	sortThreadMCPServers(rows)
	return rows, nil
}

func (a *App) claudeConfigMCPRows(workspacePath string) ([]ThreadMCPServer, error) {
	st, err := a.claudeConfig()
	if err != nil {
		return nil, err
	}
	servers, err := st.ListServers(workspacePath)
	if err != nil {
		return nil, fmt.Errorf("list claude mcp servers: %w", err)
	}
	cached := a.cachedMCPStatusByName(mcpstatus.ProviderClaude)
	rows := make([]ThreadMCPServer, 0, len(servers))
	for _, srv := range servers {
		if srv.Source == claudeconfig.SourceCloud {
			continue
		}
		rows = append(rows, configMCPRow(mcpProviderClaude, srv.Name, string(srv.Source), srv.Disabled, cached))
	}
	sortThreadMCPServers(rows)
	return rows, nil
}

// ---------------- Codex ----------------

// setCodexMCPEnabled writes the global `enabled` flag and, when
// reloadThreadID names a thread with a live Codex session, hot-reloads
// that session so the toggle applies without a restart (pass "" for the
// no-thread workspace toggle). The reload is async: enabling can mean a
// cold server spawn, and the menu click shouldn't block on it.
func (a *App) setCodexMCPEnabled(reloadThreadID, name string, enabled bool) error {
	st, err := a.codexConfig()
	if err != nil {
		return err
	}
	if err := st.SetEnabled(name, enabled); err != nil {
		return fmt.Errorf("set codex mcp enabled: %w", err)
	}
	a.mcpStatus().Invalidate(mcpstatus.Key{Provider: mcpstatus.ProviderCodex, Name: name})
	if reloadThreadID == "" {
		return nil
	}
	sess, ok := a.sessionManager().get(reloadThreadID)
	if !ok || sess.codex == nil {
		return nil
	}
	sess.codex.ForgetMCPStartupState(name)
	a.requestCodexMCPReload(reloadThreadID)
	return nil
}

// codexMCPReloadState coalesces concurrent live-reload requests for one
// thread's Codex session. Present in App.codexMCPReloads exactly while a
// reload runner is live for the thread.
type codexMCPReloadState struct {
	rerun bool
}

// requestCodexMCPReload schedules an async `config/mcpServer/reload` on
// the thread's live Codex session. The RPC is a level trigger — it
// re-reads the whole config — so N requests while one is in flight
// (several OAuth completions landing together, a fast toggle run)
// collapse into a single follow-up run instead of N stacked round-trips;
// the follow-up re-reads config written by every request it absorbed.
//
// A reload failure surfaces through emitWireErrorToThread: by the time
// the RPC settles the triggering call has long returned, and the
// stopped-thread gate is what should decide whether an error for a
// since-closed thread still matters (Bug B5 / invariant 29). The one
// silent path is app shutdown — a timeout is a real failure the user
// sees.
func (a *App) requestCodexMCPReload(threadID string) {
	a.codexMCPReloadsMu.Lock()
	if st, ok := a.codexMCPReloads[threadID]; ok {
		st.rerun = true
		a.codexMCPReloadsMu.Unlock()
		return
	}
	if a.codexMCPReloads == nil {
		a.codexMCPReloads = map[string]*codexMCPReloadState{}
	}
	st := &codexMCPReloadState{}
	a.codexMCPReloads[threadID] = st
	a.codexMCPReloadsMu.Unlock()

	go func() {
		for {
			err := a.runCodexMCPReload(threadID)

			a.codexMCPReloadsMu.Lock()
			rerun := st.rerun
			st.rerun = false
			if !rerun {
				delete(a.codexMCPReloads, threadID)
			}
			a.codexMCPReloadsMu.Unlock()

			if err != nil && a.lifeCtx().Err() == nil {
				a.emitWireErrorToThread(threadID, fmt.Sprintf("mcp: live reload failed: %s", sanitizeMCPError(err.Error())))
			}
			if !rerun {
				return
			}
		}
	}()
}

// runCodexMCPReload performs one bounded reload RPC. A session that is
// gone by now is a no-op, not an error: the next session start loads
// the current config anyway, so there is nothing left to reload and
// nothing to report.
func (a *App) runCodexMCPReload(threadID string) error {
	sess, ok := a.sessionManager().get(threadID)
	if !ok || sess.codex == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(a.lifeCtx(), mcpLiveApplyTimeout)
	defer cancel()
	return sess.codex.RefreshMCPServers(ctx)
}

// codexSessionMCPRows merges the live session's thread-scoped
// `mcpServerStatus/list` (every server the thread actually loaded,
// including plugin/project layers) with the config file's disabled
// entries, which the session never loads and therefore never reports.
//
// The list answers for a FRESH connection attempt, not for the manager
// this thread is running, so a TERMINAL retained
// `mcpServer/startupStatus/updated` state (failed/cancelled — see
// MCPStartupUpdate.TerminalFailure) is the lifecycle authority where
// the two disagree: a server this thread watched fail is reported that
// way, with the cause string the probe cannot carry, even when a fresh
// attempt would now succeed — the thread still holds the manager that
// failed. Everything else defers to the list: it describes settled
// attempts, so a retained "starting" (or an unrecognized state) is an
// older observation than the probe by construction, and letting it win
// would latch "Starting…" whenever the terminal notification was lost.
// The list also stays the membership answer, so a startup state for a
// server since removed from config simply has no row to land on. The
// retained state's other exit is ForgetMCPStartupState, taken whenever
// AO itself asks Codex to re-run a server's startup.
func (a *App) codexSessionMCPRows(ctx context.Context, sess *codex.Session) ([]ThreadMCPServer, error) {
	list, err := sess.ListMCPServerStatuses(ctx)
	if err != nil {
		return nil, err
	}
	startupStates := sess.MCPStartupStates()
	rows := make([]ThreadMCPServer, 0, len(list.Data))
	seen := make(map[string]struct{}, len(list.Data))
	for _, entry := range list.Data {
		row := ThreadMCPServer{
			Provider:   mcpProviderCodex,
			Name:       entry.Name,
			AuthStatus: entry.AuthStatus,
			Tools:      entry.ToolNames(),
			Source:     mcpRowSourceSession,
		}
		if u, ok := startupStates[entry.Name]; ok && u.TerminalFailure() {
			row.Status = string(codex.MCPStatusFromNotif(u))
			row.Error = sanitizeMCPError(u.Error)
			// The tool list came from the probe's fresh attempt; a row
			// reporting the thread's failed manager must not claim them.
			row.Tools = nil
		} else {
			row.Status = string(codex.MCPStatusFromList(entry))
		}
		rows = append(rows, row)
		seen[entry.Name] = struct{}{}
	}
	st, err := a.codexConfig()
	if err != nil {
		return nil, err
	}
	servers, err := st.ListServers()
	if err != nil {
		return nil, fmt.Errorf("list codex mcp servers: %w", err)
	}
	for _, srv := range servers {
		if _, ok := seen[srv.Name]; ok || srv.Enabled {
			continue
		}
		rows = append(rows, ThreadMCPServer{
			Provider: mcpProviderCodex,
			Name:     srv.Name,
			Status:   string(mcpstatus.StatusDisabled),
			Disabled: true,
			Source:   mcpRowSourceConfig,
		})
	}
	sortThreadMCPServers(rows)
	return rows, nil
}

func (a *App) codexConfigMCPRows() ([]ThreadMCPServer, error) {
	st, err := a.codexConfig()
	if err != nil {
		return nil, err
	}
	servers, err := st.ListServers()
	if err != nil {
		return nil, fmt.Errorf("list codex mcp servers: %w", err)
	}
	cached := a.cachedMCPStatusByName(mcpstatus.ProviderCodex)
	rows := make([]ThreadMCPServer, 0, len(servers))
	for _, srv := range servers {
		rows = append(rows, configMCPRow(mcpProviderCodex, srv.Name, "", !srv.Enabled, cached))
	}
	// No cache-only merge for Codex: config.toml enumerates the global
	// server set completely, and a cached name it doesn't carry is a
	// project-layer server from whatever cwd fed the cache — surfacing
	// it here would leak it across workspaces. Project-layer servers
	// show through live-session truth (codexSessionMCPRows).
	sortThreadMCPServers(rows)
	return rows, nil
}

// ---------------- shared row shaping ----------------

// configMCPRow builds a config-sourced row: disabled state comes from
// the config file, connection status from the cache when it has an
// entry (a disabled server's cached status is stale by definition, so
// it reports "disabled" instead). An expired cache entry still lends
// its last-known status, marked Stale.
func configMCPRow(providerName, name, scope string, disabled bool, cached map[string]mcpstatus.CachedStatus) ThreadMCPServer {
	row := ThreadMCPServer{
		Provider: providerName,
		Name:     name,
		Status:   string(mcpstatus.StatusUnknown),
		Scope:    scope,
		Disabled: disabled,
		Source:   mcpRowSourceConfig,
	}
	if disabled {
		row.Status = string(mcpstatus.StatusDisabled)
		return row
	}
	// A cached "disabled" is another workspace's view (this row's config
	// says enabled) — keep the status unknown rather than contradicting
	// the toggle.
	if cs, ok := cached[name]; ok && cs.Status != mcpstatus.StatusDisabled {
		row.Status = string(cs.Status)
		row.Error = cs.Error
		row.Tools = cs.Tools
		// Without this an inactive thread's failed-oAuth row could never
		// offer "Sign in again" — the exact dead end the auth enum exists
		// to remove, on the most common (no live session) path.
		row.AuthStatus = cs.AuthStatus
		row.Stale = !cs.Fresh
	}
	return row
}

func (a *App) cachedMCPStatusByName(prov mcpstatus.Provider) map[string]mcpstatus.CachedStatus {
	snapshot := a.mcpStatus().SnapshotProviderWithFreshness(prov)
	out := make(map[string]mcpstatus.CachedStatus, len(snapshot))
	for _, entry := range snapshot {
		out[entry.Name] = entry
	}
	return out
}

func sortThreadMCPServers(rows []ThreadMCPServer) {
	sort.Slice(rows, func(i, j int) bool { return rows[i].Name < rows[j].Name })
}
