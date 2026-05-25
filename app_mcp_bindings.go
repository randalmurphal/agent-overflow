package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sort"
	"strings"
	"time"

	"agent-overflow/internal/claudeconfig"
	"agent-overflow/internal/codexconfig"
	"agent-overflow/internal/ctxutil"
	"agent-overflow/internal/mcpstatus"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/claude"
	"agent-overflow/internal/provider/codex"
	"agent-overflow/internal/store"
)

// ErrMCPProviderUnsupported reports an inbound binding call carrying a
// provider value AO doesn't currently route MCP for. New providers
// register here when they ship.
var ErrMCPProviderUnsupported = errors.New("mcp: unsupported provider")

// ErrMCPSessionUnavailable fires when a thread's provider session is
// not live (and auto-start failed) so a binding cannot drive the
// provider-side operation it needs.
var ErrMCPSessionUnavailable = errors.New("mcp: thread session not available")

// ErrMCPReadOnlyEntry fires when a binding tries to mutate a Claude
// plugin/cloud entry. Those are surfaced to the UI so the user can
// toggle them on/off via `disabledMcpServers`, but AO doesn't own
// their definitions.
var ErrMCPReadOnlyEntry = errors.New("mcp: entry is not user-managed")

const (
	// mcpEphemeralFetchTimeout bounds the one-shot `claude mcp list` /
	// `codex mcpServerStatus/list` subprocess invocations that back the
	// popup's initial fetch and explicit Refresh. Generous enough to
	// absorb a slow first-launch CLI warm-up without leaving the popup
	// spinning indefinitely if the binary is wedged.
	mcpEphemeralFetchTimeout = 20 * time.Second
	// mcpAuthRoundTripTimeout bounds the provider-side MCP OAuth
	// handshake. The user is interacting with a browser tab during the
	// flow, so a longer ceiling is intentional — too short and a
	// distracted approval re-issues a fresh login.
	mcpAuthRoundTripTimeout = 60 * time.Second
	// mcpLiveReconcileTimeout bounds the per-thread live reconcile
	// sent to an active provider session (Claude `mcp_set_servers`,
	// Codex `RefreshMCPServers`). 30s is the same ceiling as
	// reconcileCodexAfterStart and absorbs the provider's per-server
	// connect fan-out without holding a thread-action lock forever.
	mcpLiveReconcileTimeout = 30 * time.Second
)

// MCPServer is the wire shape every MCP binding speaks. It unifies
// claudeconfig.Server (which carries Source + the per-workspace
// Disabled flag) and codexconfig.Server (which carries a global
// Enabled flag and Codex-specific transport names) into a single shape
// the frontend renders without a provider branch. Transport values
// stay provider-native ("stdio" | "http" | "sse" for Claude;
// "stdio" | "streamable_http" for Codex) so the editor form can pick
// the right input set.
//
// Disabled is the unified UI flag — true means "this server is not
// active in the current scope". For Claude that translates to the
// thread's workspace `disabledMcpServers` list; for Codex it
// translates to the global `enabled = false` field in
// ~/.codex/config.toml.
type MCPServer struct {
	Provider       string            `json:"provider"`
	Name           string            `json:"name"`
	Source         string            `json:"source,omitempty"`
	Transport      string            `json:"transport"`
	Command        string            `json:"command,omitempty"`
	Args           []string          `json:"args,omitempty"`
	Env            map[string]string `json:"env,omitempty"`
	URL            string            `json:"url,omitempty"`
	Headers        map[string]string `json:"headers,omitempty"`
	BearerTokenEnv string            `json:"bearerTokenEnv,omitempty"`
	Disabled       bool              `json:"disabled"`
}

// MCPAuthInitResult is the response shape for TriggerMcpAuth.
type MCPAuthInitResult struct {
	AuthURL            string `json:"authUrl"`
	Provider           string `json:"provider"`
	RequiresUserAction bool   `json:"requiresUserAction"`
}

// ListMcpServers returns every MCP server visible to the caller's
// scope. `provider` selects which config file we read. For Claude,
// `workspacePath` resolves the `projects.<path>.disabledMcpServers`
// list so the unified `Disabled` flag reflects this workspace; passing
// an empty workspacePath returns the library with every entry as
// enabled (used by the Settings UI's "library" view that doesn't
// belong to a specific thread). For Codex, workspacePath is ignored —
// the `enabled` flag is global.
func (a *App) ListMcpServers(provider, workspacePath string) ([]MCPServer, error) {
	switch provider {
	case mcpProviderClaude:
		return a.listClaudeMcpServers(workspacePath)
	case mcpProviderCodex:
		return a.listCodexMcpServers()
	default:
		return nil, fmt.Errorf("%w: %s", ErrMCPProviderUnsupported, provider)
	}
}

// CreateMcpServer adds a new entry to the provider's config file.
// Plugin/cloud Claude entries are refused — AO only manages
// SourceUser. The provider name is taken from input.Provider so the
// binding stays a single Wails entry rather than two parallel ones.
func (a *App) CreateMcpServer(input MCPServer) (MCPServer, error) {
	switch input.Provider {
	case mcpProviderClaude:
		return a.createClaudeMcpServer(input)
	case mcpProviderCodex:
		return a.createCodexMcpServer(input)
	default:
		return MCPServer{}, fmt.Errorf("%w: %s", ErrMCPProviderUnsupported, input.Provider)
	}
}

// UpdateMcpServer replaces the entry at input.Provider+input.Name with
// the new shape. Renaming is not supported.
func (a *App) UpdateMcpServer(input MCPServer) (MCPServer, error) {
	switch input.Provider {
	case mcpProviderClaude:
		return a.updateClaudeMcpServer(input)
	case mcpProviderCodex:
		return a.updateCodexMcpServer(input)
	default:
		return MCPServer{}, fmt.Errorf("%w: %s", ErrMCPProviderUnsupported, input.Provider)
	}
}

// DeleteMcpServer removes the entry. For Claude the call also strips
// the name from every workspace's `disabledMcpServers` so re-adding
// the server later doesn't silently surface as disabled.
func (a *App) DeleteMcpServer(provider, name string) error {
	switch provider {
	case mcpProviderClaude:
		return a.deleteClaudeMcpServer(name)
	case mcpProviderCodex:
		return a.deleteCodexMcpServer(name)
	default:
		return fmt.Errorf("%w: %s", ErrMCPProviderUnsupported, provider)
	}
}

// SetMcpServerEnabled toggles the unified Disabled flag for a server
// in the calling thread's scope. Claude: writes to the workspace's
// `disabledMcpServers` array. Codex: writes the global
// `enabled = false` field. After the file write the binding tries to
// reconcile the calling thread's live session so the change applies
// without a manual restart (Claude: mcp_set_servers; Codex:
// config/mcpServer/reload). Other live sessions outside this thread
// pick up the change on their next session start — that's the
// documented divergence from "every thread sees live disk state".
func (a *App) SetMcpServerEnabled(threadID, name string, enabled bool) error {
	if a.store == nil {
		return errors.New("set mcp server enabled: store unavailable")
	}
	thread, err := a.store.GetThread(threadID)
	if err != nil {
		return fmt.Errorf("set mcp server enabled: load thread: %w", err)
	}
	switch thread.Provider {
	case string(provider.Claude):
		return a.setClaudeMcpDisabled(thread, name, !enabled)
	case string(provider.Codex):
		return a.setCodexMcpEnabled(thread, name, enabled)
	default:
		return fmt.Errorf("%w: %s", ErrMCPProviderUnsupported, thread.Provider)
	}
}

// GetMcpServerStatus returns the cached status for one server. If
// no fresh entry exists, it runs the appropriate ephemeral fetcher
// (Claude `mcp list` / Codex `mcpServerStatus/list`) to populate
// the cache, then returns the result. force=true bypasses cache
// hits but still single-flights concurrent callers.
func (a *App) GetMcpServerStatus(providerName, name string, force bool) (mcpstatus.ServerStatus, error) {
	prov, err := parseMCPStatusProvider(providerName)
	if err != nil {
		return mcpstatus.ServerStatus{}, err
	}
	key := mcpstatus.Key{Provider: prov, Name: name}
	fetcher, err := a.mcpStatusFetcher(prov)
	if err != nil {
		return mcpstatus.ServerStatus{}, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), mcpEphemeralFetchTimeout)
	defer cancel()
	return a.mcpStatus().GetOrFetch(ctx, key, fetcher, force)
}

// ListMcpServerStatuses returns the cached snapshot for one
// provider. Does NOT trigger an ephemeral fetch — used to render
// the popup on first open before the explicit refresh kicks in.
// Live sessions push their own state into the cache continuously,
// so for any provider that has run a thread this lifetime the
// snapshot is already populated.
func (a *App) ListMcpServerStatuses(providerName string) ([]mcpstatus.ServerStatus, error) {
	prov, err := parseMCPStatusProvider(providerName)
	if err != nil {
		return nil, err
	}
	out := a.mcpStatus().SnapshotProvider(prov)
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// RefreshMcpServerStatus forces a fresh ephemeral fetch for the
// named provider and returns the resulting list. The popup's
// Refresh button uses this when the cached snapshot looks stale or
// the user wants to re-check after editing a server.
func (a *App) RefreshMcpServerStatus(providerName string) ([]mcpstatus.ServerStatus, error) {
	prov, err := parseMCPStatusProvider(providerName)
	if err != nil {
		return nil, err
	}
	fetcher, err := a.mcpStatusFetcher(prov)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), mcpEphemeralFetchTimeout)
	defer cancel()
	servers, err := a.mcpStatus().RefreshProvider(ctx, prov, fetcher)
	if err != nil {
		return nil, err
	}
	sort.Slice(servers, func(i, j int) bool { return servers[i].Name < servers[j].Name })
	return servers, nil
}

// mcpStatusFetcher returns the per-provider ephemeral fetcher,
// configured with the user's installed CLI binary. Errors surface
// when the user hasn't installed the provider's CLI; callers
// translate that into a user-facing "binary not found" toast.
func (a *App) mcpStatusFetcher(prov mcpstatus.Provider) (mcpstatus.Fetcher, error) {
	switch prov {
	case mcpstatus.ProviderClaude:
		bin := a.providerBinaryPath(string(provider.Claude))
		if bin == "" {
			return nil, fmt.Errorf("mcp status: claude binary not configured")
		}
		return &claude.MCPStatusFetcher{Binary: bin}, nil
	case mcpstatus.ProviderCodex:
		bin := a.providerBinaryPath(string(provider.Codex))
		if bin == "" {
			return nil, fmt.Errorf("mcp status: codex binary not configured")
		}
		return &codex.MCPStatusFetcher{Binary: bin}, nil
	default:
		return nil, fmt.Errorf("%w: %s", ErrMCPProviderUnsupported, prov)
	}
}

func parseMCPStatusProvider(s string) (mcpstatus.Provider, error) {
	switch s {
	case mcpProviderClaude:
		return mcpstatus.ProviderClaude, nil
	case mcpProviderCodex:
		return mcpstatus.ProviderCodex, nil
	default:
		return "", fmt.Errorf("%w: %s", ErrMCPProviderUnsupported, s)
	}
}

// TriggerMcpAuth drives the provider-side OAuth handshake for an
// http/sse / streamable_http MCP server. The thread must be live
// because the OAuth listener is owned by the provider process; if no
// session is live, we auto-start one for the round-trip.
func (a *App) TriggerMcpAuth(threadID, name string) (MCPAuthInitResult, error) {
	if a.store == nil {
		return MCPAuthInitResult{}, errors.New("trigger mcp auth: store unavailable")
	}
	thread, err := a.store.GetThread(threadID)
	if err != nil {
		return MCPAuthInitResult{}, err
	}
	if !a.hasActiveSession(threadID) {
		if err := a.startSession(threadID); err != nil {
			return MCPAuthInitResult{}, fmt.Errorf("auto-start session: %w", err)
		}
	}
	sess, ok := a.sessionManager().get(threadID)
	if !ok {
		return MCPAuthInitResult{}, ErrMCPSessionUnavailable
	}
	ctx, cancel := context.WithTimeout(context.Background(), mcpAuthRoundTripTimeout)
	defer cancel()
	switch thread.Provider {
	case string(provider.Claude):
		if sess.claude == nil {
			return MCPAuthInitResult{}, ErrMCPSessionUnavailable
		}
		res, err := sess.claude.AuthenticateMCP(ctx, name)
		if err != nil {
			return MCPAuthInitResult{}, err
		}
		// Claude has no spontaneous post-OAuth wire envelope (verified
		// against 2.1.139 source + spike), so kick off a Go-side poll
		// of `mcp_status` to detect the flip out of needs-auth and
		// emit `mcp:oauth-completed` for parity with Codex's
		// `mcpServer/oauthLogin/completed` handler below.
		// startClaudeMCPOAuthPoll cancels any prior in-flight poll
		// for the same serverName so a spam-click of Sign In doesn't
		// fan out multiple goroutines all racing to fire the same
		// terminal event.
		a.startClaudeMCPOAuthPoll(threadID, name)
		return MCPAuthInitResult{
			AuthURL:            res.AuthURL,
			Provider:           string(provider.Claude),
			RequiresUserAction: res.RequiresUserAction,
		}, nil
	case string(provider.Codex):
		if sess.codex == nil {
			return MCPAuthInitResult{}, ErrMCPSessionUnavailable
		}
		res, err := sess.codex.AuthenticateMCP(ctx, name, nil, 0)
		if err != nil {
			return MCPAuthInitResult{}, err
		}
		return MCPAuthInitResult{
			AuthURL:            res.AuthorizationURL,
			Provider:           string(provider.Codex),
			RequiresUserAction: true,
		}, nil
	default:
		return MCPAuthInitResult{}, fmt.Errorf("%w: %s", ErrMCPProviderUnsupported, thread.Provider)
	}
}

// sanitizeMCPError bounds an error string surfaced by a provider's
// MCP channel before it lands on the wire (mcp:status,
// mcp:oauth-completed, provider:item_event) or in a user-facing
// toast. Neither channel is loopback-only, so a LAN-attached
// subscriber sees the verbatim string. The Claude CLI and Codex
// app-server both inherit AO's `os.Environ()` (intentionally — env
// vars resolve MCP bearer-token indirection), so a future provider
// panic that dumped its env could otherwise channel a token through
// to remote subscribers verbatim. 256B + newline collapse matches
// the equivalent defense `internal/provider/claude/mcpstatus.go`
// applies to child-process stderr; keeping a second copy here is
// deliberate so the wire-facing handlers don't depend on a private
// claude helper.
func sanitizeMCPError(s string) string {
	s = strings.TrimSpace(s)
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "\r", " ")
	const limit = 256
	if len(s) > limit {
		return s[:limit] + "…(truncated)"
	}
	return s
}

// claudeMCPOAuthPoll is the dedup identity stored in
// App.claudeMCPOAuthPolls. The struct address is the identity — a
// stale defer can compare its own *claudeMCPOAuthPoll against the
// current map entry to avoid wiping a newer poller's registration.
// `cancel` cuts off the poll's ctx so a superseding TriggerMcpAuth
// click stops the prior loop before starting a fresh one.
type claudeMCPOAuthPoll struct {
	cancel context.CancelFunc
}

// startClaudeMCPOAuthPoll spawns a new OAuth-completion poller for
// `serverName`, cancelling any prior in-flight poll for the same
// name first. Returns immediately; the goroutine bounds itself via
// defaultClaudeMCPOAuthIntervals and self-deregisters on exit. Safe
// to call from multiple goroutines: only the most recent caller's
// poll fires the terminal event for that server. The session-gone
// path (getQuerier returning nil between ticks) and the shutdown
// guard inside pollClaudeMCPAfterOAuth still apply.
func (a *App) startClaudeMCPOAuthPoll(threadID, serverName string) {
	ctx, cancel := context.WithCancel(a.lifeCtx())
	poll := &claudeMCPOAuthPoll{cancel: cancel}

	a.claudeMCPOAuthPollsMu.Lock()
	if a.claudeMCPOAuthPolls == nil {
		a.claudeMCPOAuthPolls = map[string]*claudeMCPOAuthPoll{}
	}
	if prior, ok := a.claudeMCPOAuthPolls[serverName]; ok {
		prior.cancel()
	}
	a.claudeMCPOAuthPolls[serverName] = poll
	a.claudeMCPOAuthPollsMu.Unlock()

	go func() {
		defer func() {
			a.claudeMCPOAuthPollsMu.Lock()
			// Only clear the slot if it still points at our poll —
			// a newer caller may have replaced us mid-loop.
			if a.claudeMCPOAuthPolls[serverName] == poll {
				delete(a.claudeMCPOAuthPolls, serverName)
			}
			a.claudeMCPOAuthPollsMu.Unlock()
			cancel()
		}()
		a.pollClaudeMCPAfterOAuth(
			ctx,
			threadID,
			serverName,
			defaultClaudeMCPOAuthIntervals,
			func() claudeMCPStatusQuerier {
				sess, ok := a.sessionManager().get(threadID)
				if !ok || sess.claude == nil {
					return nil
				}
				return sess.claude.QueryMCPStatus
			},
		)
	}()
}

// claudeMCPStatusQuerier is the slim functional shape of
// `(*claude.Session).QueryMCPStatus`. Lifted out so the poller can be
// invoked with a test-controlled closure without dragging in the full
// Session struct.
type claudeMCPStatusQuerier func(ctx context.Context) ([]claude.MCPServerStatus, error)

// defaultClaudeMCPOAuthIntervals is a Fibonacci-shaped backoff:
// short early ticks for fast browser flows (most OAuth completes
// inside 10s), a trailing 13s tick as slack for slow IdPs and the
// CLI's client-pool warm-up before a freshly-credentialed server
// reports as `connected`. Total wall budget across 6 attempts is
// ~32s. Tests pass zero-duration slices to drive the loop
// deterministically without changing the tick count.
var defaultClaudeMCPOAuthIntervals = []time.Duration{
	1 * time.Second,
	2 * time.Second,
	3 * time.Second,
	5 * time.Second,
	8 * time.Second,
	13 * time.Second,
}

// pollClaudeMCPAfterOAuth polls Claude's `mcp_status` control_request
// after a successful TriggerMcpAuth, waiting for `serverName` to flip
// out of `needs-auth`. Claude emits no spontaneous post-OAuth wire
// envelope, so a Go-side poll is the only way to mirror Codex's
// `mcpServer/oauthLogin/completed` notification path.
//
// Terminal states ({connected, failed}) fire a cache Put + a
// `mcp:oauth-completed` emission shaped identically to the Codex
// handler (`{threadId, provider, serverName, success, error}`).
// `failed` additionally surfaces a thread-error so the user sees
// inline feedback. Other states ({needs-auth, starting/pending,
// unknown, server-missing-from-response, or transient query error})
// keep the loop running until intervals is exhausted or ctx is
// canceled. On timeout no event fires — the prior cache entry stays
// in place and the user can hit Refresh manually.
//
// getQuerier is re-invoked each tick so a session that dies mid-poll
// stops the loop cleanly. Production passes a closure that resolves
// the live session through sessionManager().get(threadID); tests
// pass a controlled closure to inject canned responses.
func (a *App) pollClaudeMCPAfterOAuth(
	ctx context.Context,
	threadID, serverName string,
	intervals []time.Duration,
	getQuerier func() claudeMCPStatusQuerier,
) {
	for _, d := range intervals {
		if !ctxutil.Sleep(ctx, d) {
			return
		}
		query := getQuerier()
		if query == nil {
			return
		}
		statuses, err := query(ctx)
		if err != nil {
			continue
		}
		var entry *claude.MCPServerStatus
		for i := range statuses {
			if statuses[i].Name == serverName {
				entry = &statuses[i]
				break
			}
		}
		if entry == nil {
			continue
		}
		mapped := claude.MCPStatusFromRaw(entry.Status)
		switch mapped {
		case mcpstatus.StatusConnected, mcpstatus.StatusFailed:
			// Shutdown race guard. appCtx is cancelled in Shutdown
			// step 1b, BEFORE drainTriage. If we landed here between
			// the query returning and the side effects, ctx.Err()
			// has flipped — bail before emitErrorToThread can file a
			// triage.Handle past the drain barrier.
			if ctx.Err() != nil {
				return
			}
			sanitizedErr := sanitizeMCPError(entry.Error)
			a.mcpStatus().Put(mcpstatus.ServerStatus{
				Key:       mcpstatus.Key{Provider: mcpstatus.ProviderClaude, Name: serverName},
				Status:    mapped,
				Raw:       entry.Status,
				Error:     sanitizedErr,
				Source:    mcpstatus.SourceLiveSession,
				CheckedAt: time.Now(),
			})
			success := mapped == mcpstatus.StatusConnected
			a.emit("mcp:oauth-completed", map[string]any{
				"threadId":   threadID,
				"provider":   mcpProviderClaude,
				"serverName": serverName,
				"success":    success,
				"error":      sanitizedErr,
			})
			if !success {
				msg := sanitizedErr
				if msg == "" {
					msg = "sign-in did not complete"
				}
				a.emitErrorToThread(threadID, fmt.Sprintf("mcp: %s: %s", serverName, msg))
			}
			return
		default:
			// needs-auth, starting/pending, unknown, or any future
			// status the projector returns as a non-terminal value:
			// keep polling. Missing-from-response (entry == nil)
			// already continues earlier.
		}
	}
}

// handleCodexMCPOAuthCompleted is the side-channel callback Codex
// fires after the user's browser hop completes the OAuth handshake.
// AO's job is small but load-bearing: invalidate the status cache
// so the next read reflects the freshly-credentialed session, and
// surface a `mcp:oauth-completed` event for any popup listening.
//
// We deliberately do NOT trigger a refetch here — the next thread
// session start (or the next user-triggered refresh) will repopulate
// the entry. Firing a fetch from a notification handler risks
// stacking processes if the user OAuths several servers quickly.
func (a *App) handleCodexMCPOAuthCompleted(threadID, serverName string, success bool, errMsg string) {
	sanitizedErr := sanitizeMCPError(errMsg)
	a.mcpStatus().Invalidate(mcpstatus.Key{Provider: mcpstatus.ProviderCodex, Name: serverName})
	a.emit("mcp:oauth-completed", map[string]any{
		"threadId":   threadID,
		"provider":   mcpProviderCodex,
		"serverName": serverName,
		"success":    success,
		"error":      sanitizedErr,
	})
	if !success {
		msg := sanitizedErr
		if msg == "" {
			msg = "sign-in did not complete"
		}
		a.emitErrorToThread(threadID, fmt.Sprintf("mcp: %s: %s", serverName, msg))
	}
}

// handleCodexMCPStartupUpdate is the per-thread side-channel
// callback Codex fires as MCP servers move through
// starting → ready / failed / cancelled during thread/start.
// Feed each delta into the status cache with Source="notification"
// so the popup reflects live provider state without an ephemeral
// refetch. Cache emits over `mcp:status`, so subscribers update
// reactively.
func (a *App) handleCodexMCPStartupUpdate(u codex.MCPStartupUpdate) {
	if strings.TrimSpace(u.Name) == "" {
		return
	}
	a.mcpStatus().Put(mcpstatus.ServerStatus{
		Key:       mcpstatus.Key{Provider: mcpstatus.ProviderCodex, Name: u.Name},
		Status:    codex.MCPStatusFromNotif(u.State),
		Raw:       u.State,
		Error:     sanitizeMCPError(u.Error),
		Source:    mcpstatus.SourceNotification,
		CheckedAt: time.Now(),
	})
}

// ---------------- Claude implementation ----------------

const (
	mcpProviderClaude = "claude"
	mcpProviderCodex  = "codex"
)

func (a *App) listClaudeMcpServers(workspacePath string) ([]MCPServer, error) {
	st, err := a.claudeConfig()
	if err != nil {
		return nil, err
	}
	servers, err := st.ListServers(workspacePath)
	if err != nil {
		return nil, fmt.Errorf("list claude mcp servers: %w", err)
	}
	out := make([]MCPServer, 0, len(servers))
	for _, srv := range servers {
		out = append(out, claudeServerToWire(srv))
	}
	return out, nil
}

func (a *App) createClaudeMcpServer(input MCPServer) (MCPServer, error) {
	if input.Source != "" && input.Source != string(claudeconfig.SourceUser) {
		return MCPServer{}, fmt.Errorf("%w: source=%s", ErrMCPReadOnlyEntry, input.Source)
	}
	st, err := a.claudeConfig()
	if err != nil {
		return MCPServer{}, err
	}
	target := wireToClaudeServer(input)
	target.Source = claudeconfig.SourceUser
	if err := st.CreateServer(target); err != nil {
		return MCPServer{}, fmt.Errorf("create claude mcp server: %w", err)
	}
	a.mcpStatus().Invalidate(mcpstatus.Key{Provider: mcpstatus.ProviderClaude, Name: target.Name})
	return claudeServerToWire(target), nil
}

func (a *App) updateClaudeMcpServer(input MCPServer) (MCPServer, error) {
	if input.Source != "" && input.Source != string(claudeconfig.SourceUser) {
		return MCPServer{}, fmt.Errorf("%w: source=%s", ErrMCPReadOnlyEntry, input.Source)
	}
	st, err := a.claudeConfig()
	if err != nil {
		return MCPServer{}, err
	}
	target := wireToClaudeServer(input)
	target.Source = claudeconfig.SourceUser
	if err := st.UpdateServer(target); err != nil {
		return MCPServer{}, fmt.Errorf("update claude mcp server: %w", err)
	}
	a.mcpStatus().Invalidate(mcpstatus.Key{Provider: mcpstatus.ProviderClaude, Name: target.Name})
	return claudeServerToWire(target), nil
}

func (a *App) deleteClaudeMcpServer(name string) error {
	st, err := a.claudeConfig()
	if err != nil {
		return err
	}
	if err := st.DeleteServer(name); err != nil {
		return fmt.Errorf("delete claude mcp server: %w", err)
	}
	a.mcpStatus().Invalidate(mcpstatus.Key{Provider: mcpstatus.ProviderClaude, Name: name})
	return nil
}

func (a *App) setClaudeMcpDisabled(thread store.Thread, name string, disabled bool) error {
	workspacePath := strings.TrimSpace(thread.WorkspacePath)
	if workspacePath == "" {
		return fmt.Errorf("set claude mcp disabled: thread %s has no workspace path", thread.ID)
	}
	st, err := a.claudeConfig()
	if err != nil {
		return err
	}
	if err := st.SetDisabled(workspacePath, name, disabled); err != nil {
		return fmt.Errorf("set claude mcp disabled: %w", err)
	}
	a.mcpStatus().Invalidate(mcpstatus.Key{Provider: mcpstatus.ProviderClaude, Name: name})
	if thread.Mode == "design" {
		// Design Claude sessions launched with --strict-mcp-config — user
		// MCP isn't visible to them regardless of the disabled flag.
		// Skip the live reconcile so we don't spend an RPC on a no-op.
		return nil
	}
	hasSession := a.hasActiveSession(thread.ID)
	go func() {
		// SQLite dual-write and reconcile run inside the per-thread lock
		// so rapid toggles serialize correctly.
		unlock := a.threadLocks().Lock(thread.ID)
		defer unlock()
		current, err := a.ensureDisabledMcpSnapshot(thread.ID, thread.Provider, workspacePath)
		if err != nil {
			log.Printf("mcp: thread %s claude snapshot disabled: %v", thread.ID, err)
			return
		}
		updated := mutateDisabledSet(current, name, disabled)
		if err := a.store.SetDisabledMcpServers(thread.ID, updated); err != nil {
			log.Printf("mcp: thread %s claude set disabled: %v", thread.ID, err)
			return
		}
		if !hasSession {
			return
		}
		err = a.reconcileClaudeMCPLive(thread)
		if err == nil {
			return
		}
		log.Printf("mcp: thread %s claude live reconcile: %v", thread.ID, err)
		if a.lifeCtx().Err() != nil {
			return
		}
		a.emitErrorToThread(thread.ID, fmt.Sprintf("mcp: live reconcile failed: %s", sanitizeMCPError(err.Error())))
	}()
	return nil
}

// reconcileClaudeMCPOnInit pushes the per-thread MCP disabled set after
// a Claude session initializes. Skips design threads and threads with an
// empty disabled set (nothing to override — native discovery is correct).
func (a *App) reconcileClaudeMCPOnInit(threadID string) {
	t, err := a.store.GetThread(threadID)
	if err != nil {
		log.Printf("mcp: thread %s post-init reconcile: load thread: %v", threadID, err)
		return
	}
	if t.Mode == "design" {
		return
	}
	disabled, err := a.ensureDisabledMcpSnapshot(t.ID, t.Provider, t.WorkspacePath)
	if err != nil {
		log.Printf("mcp: thread %s post-init reconcile: snapshot: %v", threadID, err)
		return
	}
	if len(disabled) == 0 {
		return
	}
	unlock := a.threadLocks().Lock(threadID)
	defer unlock()
	if err := a.reconcileClaudeMCPLive(t); err != nil {
		log.Printf("mcp: thread %s post-init reconcile: %v", threadID, err)
	}
}

// reconcileClaudeMCPLive pushes the thread-specific user MCP set to the
// live Claude session via mcp_set_servers. Reads the disabled set from
// SQLite (per-thread state) instead of the workspace config file.
func (a *App) reconcileClaudeMCPLive(thread store.Thread) error {
	sess, ok := a.sessionManager().get(thread.ID)
	if !ok || sess.claude == nil {
		return nil
	}
	disabled, err := a.ensureDisabledMcpSnapshot(thread.ID, thread.Provider, thread.WorkspacePath)
	if err != nil {
		return fmt.Errorf("ensure mcp snapshot for reconcile: %w", err)
	}
	disabledSet := make(map[string]bool, len(disabled))
	for _, name := range disabled {
		disabledSet[name] = true
	}
	st, err := a.claudeConfig()
	if err != nil {
		return err
	}
	servers, err := st.ListServers(thread.WorkspacePath)
	if err != nil {
		return fmt.Errorf("list claude mcp servers for reconcile: %w", err)
	}
	target := map[string]any{}
	for _, srv := range servers {
		if srv.Source != claudeconfig.SourceUser {
			continue
		}
		if disabledSet[srv.Name] {
			continue
		}
		spec, err := srv.RenderForCLI()
		if err != nil {
			log.Printf("mcp: thread %s render %q for live reconcile: %v", thread.ID, srv.Name, err)
			continue
		}
		target[srv.Name] = spec
	}
	ctx, cancel := context.WithTimeout(a.lifeCtx(), mcpLiveReconcileTimeout)
	defer cancel()
	diff, err := sess.claude.SetMCPServers(ctx, target)
	if err != nil {
		return err
	}
	if diff != nil && (len(diff.Added) > 0 || len(diff.Removed) > 0 || len(diff.Errors) > 0) {
		log.Printf("mcp: thread %s claude diff added=%v removed=%v errors=%v", thread.ID, diff.Added, diff.Removed, diff.Errors)
	}
	return nil
}

func claudeServerToWire(s claudeconfig.Server) MCPServer {
	return MCPServer{
		Provider:  mcpProviderClaude,
		Name:      s.Name,
		Source:    string(s.Source),
		Transport: s.Transport,
		Command:   s.Command,
		Args:      append([]string{}, s.Args...),
		Env:       copyStringMap(s.Env),
		URL:       s.URL,
		Headers:   copyStringMap(s.Headers),
		Disabled:  s.Disabled,
	}
}

func wireToClaudeServer(w MCPServer) claudeconfig.Server {
	return claudeconfig.Server{
		Name:      strings.TrimSpace(w.Name),
		Transport: w.Transport,
		Command:   w.Command,
		Args:      append([]string{}, w.Args...),
		Env:       copyStringMap(w.Env),
		URL:       w.URL,
		Headers:   copyStringMap(w.Headers),
	}
}

// ---------------- Codex implementation ----------------

func (a *App) listCodexMcpServers() ([]MCPServer, error) {
	st, err := a.codexConfig()
	if err != nil {
		return nil, err
	}
	servers, err := st.ListServers()
	if err != nil {
		return nil, fmt.Errorf("list codex mcp servers: %w", err)
	}
	out := make([]MCPServer, 0, len(servers))
	for _, srv := range servers {
		out = append(out, codexServerToWire(srv))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (a *App) createCodexMcpServer(input MCPServer) (MCPServer, error) {
	st, err := a.codexConfig()
	if err != nil {
		return MCPServer{}, err
	}
	target := wireToCodexServer(input)
	if err := st.CreateServer(target); err != nil {
		return MCPServer{}, fmt.Errorf("create codex mcp server: %w", err)
	}
	a.mcpStatus().Invalidate(mcpstatus.Key{Provider: mcpstatus.ProviderCodex, Name: target.Name})
	return codexServerToWire(target), nil
}

func (a *App) updateCodexMcpServer(input MCPServer) (MCPServer, error) {
	st, err := a.codexConfig()
	if err != nil {
		return MCPServer{}, err
	}
	target := wireToCodexServer(input)
	if err := st.UpdateServer(target); err != nil {
		return MCPServer{}, fmt.Errorf("update codex mcp server: %w", err)
	}
	a.mcpStatus().Invalidate(mcpstatus.Key{Provider: mcpstatus.ProviderCodex, Name: target.Name})
	return codexServerToWire(target), nil
}

func (a *App) deleteCodexMcpServer(name string) error {
	st, err := a.codexConfig()
	if err != nil {
		return err
	}
	if err := st.DeleteServer(name); err != nil {
		return fmt.Errorf("delete codex mcp server: %w", err)
	}
	a.mcpStatus().Invalidate(mcpstatus.Key{Provider: mcpstatus.ProviderCodex, Name: name})
	return nil
}

func (a *App) setCodexMcpEnabled(thread store.Thread, name string, enabled bool) error {
	st, err := a.codexConfig()
	if err != nil {
		return err
	}
	if err := st.SetEnabled(name, enabled); err != nil {
		return fmt.Errorf("set codex mcp enabled: %w", err)
	}
	a.mcpStatus().Invalidate(mcpstatus.Key{Provider: mcpstatus.ProviderCodex, Name: name})
	go func() {
		unlock := a.threadLocks().Lock(thread.ID)
		defer unlock()
		current, err := a.ensureDisabledMcpSnapshot(thread.ID, thread.Provider, thread.WorkspacePath)
		if err != nil {
			log.Printf("mcp: thread %s codex snapshot disabled: %v", thread.ID, err)
			return
		}
		updated := mutateDisabledSet(current, name, !enabled)
		if err := a.store.SetDisabledMcpServers(thread.ID, updated); err != nil {
			log.Printf("mcp: thread %s codex set disabled: %v", thread.ID, err)
			return
		}
		sess, ok := a.sessionManager().get(thread.ID)
		if !ok || sess.codex == nil {
			return
		}
		ctx, cancel := context.WithTimeout(a.lifeCtx(), mcpLiveReconcileTimeout)
		defer cancel()
		err = sess.codex.RefreshMCPServers(ctx)
		if err == nil {
			return
		}
		log.Printf("mcp: thread %s codex live reload: %v", thread.ID, err)
		if ctx.Err() != nil {
			return
		}
		a.emitErrorToThread(thread.ID, fmt.Sprintf("mcp: live reload failed: %s", sanitizeMCPError(err.Error())))
	}()
	return nil
}

func codexServerToWire(s codexconfig.Server) MCPServer {
	return MCPServer{
		Provider:       mcpProviderCodex,
		Name:           s.Name,
		Transport:      s.Transport,
		Command:        s.Command,
		Args:           append([]string{}, s.Args...),
		Env:            copyStringMap(s.Env),
		URL:            s.URL,
		Headers:        copyStringMap(s.HTTPHeaders),
		BearerTokenEnv: s.BearerTokenEnv,
		Disabled:       !s.Enabled,
	}
}

func wireToCodexServer(w MCPServer) codexconfig.Server {
	return codexconfig.Server{
		Name:           strings.TrimSpace(w.Name),
		Transport:      w.Transport,
		Command:        w.Command,
		Args:           append([]string{}, w.Args...),
		Env:            copyStringMap(w.Env),
		URL:            w.URL,
		HTTPHeaders:    copyStringMap(w.Headers),
		BearerTokenEnv: w.BearerTokenEnv,
		Enabled:        !w.Disabled,
	}
}

func copyStringMap(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// snapshotDisabledMcpServers reads the current global disabled set from
// the provider config file for use as a new thread's initial snapshot.
func (a *App) snapshotDisabledMcpServers(providerName, workspacePath string) *[]string {
	var disabled []string
	switch providerName {
	case mcpProviderClaude:
		st, err := a.claudeConfig()
		if err != nil {
			log.Printf("mcp: snapshot claude disabled: %v", err)
			return &disabled
		}
		servers, err := st.ListServers(workspacePath)
		if err != nil {
			log.Printf("mcp: snapshot claude disabled: %v", err)
			return &disabled
		}
		for _, srv := range servers {
			if srv.Disabled {
				disabled = append(disabled, srv.Name)
			}
		}
	case mcpProviderCodex:
		st, err := a.codexConfig()
		if err != nil {
			log.Printf("mcp: snapshot codex disabled: %v", err)
			return &disabled
		}
		servers, err := st.ListServers()
		if err != nil {
			log.Printf("mcp: snapshot codex disabled: %v", err)
			return &disabled
		}
		for _, srv := range servers {
			if !srv.Enabled {
				disabled = append(disabled, srv.Name)
			}
		}
	}
	return &disabled
}

// ensureDisabledMcpSnapshot returns the per-thread disabled set,
// lazy-snapshotting from the global config if the thread has no
// snapshot yet (NULL column, pre-feature thread).
func (a *App) ensureDisabledMcpSnapshot(threadID, providerName, workspacePath string) ([]string, error) {
	names, snapshotted, err := a.store.GetDisabledMcpServers(threadID)
	if err != nil {
		return nil, err
	}
	if snapshotted {
		return names, nil
	}
	snapshot := a.snapshotDisabledMcpServers(providerName, workspacePath)
	if err := a.store.SetDisabledMcpServers(threadID, *snapshot); err != nil {
		return nil, err
	}
	return *snapshot, nil
}

// ListMcpServersForThread returns the MCP server library with per-thread
// disabled state from SQLite instead of global config. Used by the
// composer toolbar popup. Settings UI continues to use ListMcpServers.
func (a *App) ListMcpServersForThread(threadID string) ([]MCPServer, error) {
	t, err := a.store.GetThread(threadID)
	if err != nil {
		return nil, fmt.Errorf("list mcp servers for thread: %w", err)
	}
	disabled, err := a.ensureDisabledMcpSnapshot(t.ID, t.Provider, t.WorkspacePath)
	if err != nil {
		return nil, fmt.Errorf("list mcp servers for thread: %w", err)
	}
	disabledSet := make(map[string]bool, len(disabled))
	for _, name := range disabled {
		disabledSet[name] = true
	}

	switch t.Provider {
	case "claude":
		st, err := a.claudeConfig()
		if err != nil {
			return nil, err
		}
		servers, err := st.ListServers(t.WorkspacePath)
		if err != nil {
			return nil, fmt.Errorf("list claude mcp servers for thread: %w", err)
		}
		out := make([]MCPServer, 0, len(servers))
		for _, srv := range servers {
			ws := claudeServerToWire(srv)
			ws.Disabled = disabledSet[srv.Name]
			out = append(out, ws)
		}
		sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
		return out, nil

	case "codex":
		st, err := a.codexConfig()
		if err != nil {
			return nil, err
		}
		servers, err := st.ListServers()
		if err != nil {
			return nil, fmt.Errorf("list codex mcp servers for thread: %w", err)
		}
		out := make([]MCPServer, 0, len(servers))
		for _, srv := range servers {
			ws := codexServerToWire(srv)
			ws.Disabled = disabledSet[srv.Name]
			out = append(out, ws)
		}
		sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
		return out, nil

	default:
		return nil, fmt.Errorf("%w: %s", ErrMCPProviderUnsupported, t.Provider)
	}
}

// buildCodexMCPServersForThread builds the full enabled server set for
// a Codex thread, reading disabled state from SQLite. The returned map
// replaces native Codex config discovery via config.mcp_servers in
// thread start params. Returns nil when nothing is disabled (native
// discovery gives the correct result).
func (a *App) buildCodexMCPServersForThread(t store.Thread) (map[string]any, error) {
	disabled, err := a.ensureDisabledMcpSnapshot(t.ID, t.Provider, t.WorkspacePath)
	if err != nil {
		return nil, err
	}
	if len(disabled) == 0 {
		return nil, nil
	}
	disabledSet := make(map[string]bool, len(disabled))
	for _, name := range disabled {
		disabledSet[name] = true
	}
	st, err := a.codexConfig()
	if err != nil {
		return nil, err
	}
	servers, err := st.ListServers()
	if err != nil {
		return nil, err
	}
	out := map[string]any{}
	for _, srv := range servers {
		if disabledSet[srv.Name] {
			continue
		}
		spec := map[string]any{}
		switch srv.Transport {
		case "stdio":
			spec["command"] = srv.Command
			if len(srv.Args) > 0 {
				spec["args"] = srv.Args
			}
			if len(srv.Env) > 0 {
				spec["env"] = srv.Env
			}
		case "streamable_http":
			spec["url"] = srv.URL
			if len(srv.HTTPHeaders) > 0 {
				spec["http_headers"] = srv.HTTPHeaders
			}
			if srv.BearerTokenEnv != "" {
				spec["bearer_token_env_var"] = srv.BearerTokenEnv
			}
		default:
			continue
		}
		out[srv.Name] = spec
	}
	return out, nil
}

// mutateDisabledSet returns a copy of the disabled set with name
// added (disabled=true) or removed (disabled=false).
func mutateDisabledSet(current []string, name string, disabled bool) []string {
	out := make([]string, 0, len(current)+1)
	for _, n := range current {
		if n != name {
			out = append(out, n)
		}
	}
	if disabled {
		out = append(out, name)
	}
	return out
}
