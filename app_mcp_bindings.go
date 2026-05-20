package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"agent-overflow/internal/mcp"
	"agent-overflow/internal/mcpprobe"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"

	"github.com/google/uuid"
)

// ErrMCPServerNotSelected fires when a binding tries to operate on a
// library server that is not currently enabled for the target thread.
// TriggerMcpAuth returns it so the frontend can prompt the user to
// toggle the server on before retrying the sign-in.
var ErrMCPServerNotSelected = errors.New("mcp: server not enabled for thread")

// ErrMCPSessionUnavailable fires when a thread's provider session is
// not live (and auto-start failed) so a binding cannot drive the
// provider-side operation it needs.
var ErrMCPSessionUnavailable = errors.New("mcp: thread session not available")

// MCPAuthInitResult is the response shape for TriggerMcpAuth. The
// frontend opens AuthURL via OpenExternalURL; Provider lets the UI
// pick the right "completing sign-in" copy. RequiresUserAction is
// always true today (both providers hand back a URL we must surface)
// but stays on the wire so a future "completed inline" path can
// arrive without a binding-shape change.
type MCPAuthInitResult struct {
	AuthURL            string `json:"authUrl"`
	Provider           string `json:"provider"`
	RequiresUserAction bool   `json:"requiresUserAction"`
}

func (a *App) ensureMCPStore(op string) error {
	if a.store == nil {
		return fmt.Errorf("%s: store unavailable", op)
	}
	return nil
}

// ListMcpServers returns the MCP library, alphabetical by name.
func (a *App) ListMcpServers() ([]store.MCPServer, error) {
	if err := a.ensureMCPStore("list mcp servers"); err != nil {
		return nil, err
	}
	return a.store.ListMCPServers()
}

// GetMcpThreadProfile returns the last-selected seed used to populate
// freshly-created threads. An unset row is normalised to an empty
// list so the frontend never has to branch on "missing".
func (a *App) GetMcpThreadProfile() (store.MCPThreadProfile, error) {
	if err := a.ensureMCPStore("get mcp thread profile"); err != nil {
		return store.MCPThreadProfile{}, err
	}
	profile, err := a.store.GetMCPThreadProfile()
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return store.MCPThreadProfile{ServerIDs: []string{}}, nil
		}
		return store.MCPThreadProfile{}, err
	}
	if profile.ServerIDs == nil {
		profile.ServerIDs = []string{}
	}
	return profile, nil
}

// GetThreadMcpServers returns the enabled server ids for one thread.
func (a *App) GetThreadMcpServers(threadID string) ([]string, error) {
	if err := a.ensureMCPStore("get thread mcp servers"); err != nil {
		return nil, err
	}
	ids, err := a.store.ListThreadMCPServerIDs(threadID)
	if err != nil {
		return nil, err
	}
	if ids == nil {
		ids = []string{}
	}
	return ids, nil
}

// CreateMcpServer inserts a library row. The id is allocated here if
// the caller didn't supply one; library-level Enabled defaults to
// true (a "create disabled" flow isn't useful — the kill-switch
// matters after a server has been working). The new id is appended
// to mcp_thread_profile so any subsequent CreateThread inherits the
// server by default — that's the plan's "new library entries
// enabled by default in new threads" rule.
func (a *App) CreateMcpServer(input store.MCPServer) (store.MCPServer, error) {
	if err := a.ensureMCPStore("create mcp server"); err != nil {
		return store.MCPServer{}, err
	}
	if strings.TrimSpace(input.ID) == "" {
		input.ID = uuid.New().String()
	}
	// Force enabled on create regardless of what the caller sent.
	// The Enabled column is a library-level kill-switch flipped from
	// the existing Settings UI later — a "create already disabled"
	// path through this binding has no UX surface, and silently
	// honouring an inbound false would let a malformed import flow
	// land library rows that are invisible from the popup.
	input.Enabled = true
	created, err := a.store.CreateMCPServer(input)
	if err != nil {
		return store.MCPServer{}, err
	}
	a.appendMCPServerToProfile(created.ID)
	return created, nil
}

// UpdateMcpServer replaces the library row and drops the cached
// probe state so the popup doesn't show stale "ready" against a
// transport change.
func (a *App) UpdateMcpServer(input store.MCPServer) (store.MCPServer, error) {
	if err := a.ensureMCPStore("update mcp server"); err != nil {
		return store.MCPServer{}, err
	}
	updated, err := a.store.UpdateMCPServer(input)
	if err != nil {
		return store.MCPServer{}, err
	}
	a.mcpProbe().Invalidate(updated.ID)
	return updated, nil
}

// DeleteMcpServer removes the library row, prunes the id from
// mcp_thread_profile (no FK on the JSON blob; manual cleanup keeps
// future seed inserts from failing on a dangling id), and clears the
// cached probe. The thread_mcp_servers cascade handles live threads.
func (a *App) DeleteMcpServer(id string) error {
	if err := a.ensureMCPStore("delete mcp server"); err != nil {
		return err
	}
	id = strings.TrimSpace(id)
	if id == "" {
		return fmt.Errorf("delete mcp server: id required")
	}
	if err := a.store.DeleteMCPServer(id); err != nil {
		return err
	}
	a.pruneMCPServerFromProfile(id)
	a.mcpProbe().Invalidate(id)
	return nil
}

// UpdateThreadMcpServers replaces the per-thread enable list and
// reconciles the live provider session if one exists. Claude takes a
// `mcp_set_servers` control_request (in-process diff, no respawn);
// Codex reconnects because `configOverrides["mcp_servers"]` is only
// honoured at thread/start. Both paths write the new selection back
// to `mcp_thread_profile` so the next CreateThread inherits the
// user's most recent intent.
func (a *App) UpdateThreadMcpServers(threadID string, serverIDs []string) (store.Thread, error) {
	if err := a.ensureMCPStore("update thread mcp servers"); err != nil {
		return store.Thread{}, err
	}
	unlock := a.threadLocks().Lock(threadID)
	defer unlock()

	thread, err := a.store.GetThread(threadID)
	if err != nil {
		return store.Thread{}, err
	}
	if serverIDs == nil {
		serverIDs = []string{}
	}
	if err := a.store.SetThreadMCPServers(threadID, serverIDs); err != nil {
		return store.Thread{}, err
	}
	if err := a.store.SetMCPThreadProfile(serverIDs); err != nil {
		log.Printf("mcp: update profile after thread %s set: %v", threadID, err)
	}

	if !a.hasActiveSession(threadID) {
		return a.store.GetThread(threadID)
	}

	switch thread.Provider {
	case string(provider.Claude):
		if err := a.reconcileClaudeMCPLive(threadID, thread); err != nil {
			a.emitErrorToThread(threadID, fmt.Sprintf("mcp: live reconcile failed: %v", err))
			log.Printf("mcp: thread %s claude live reconcile: %v", threadID, err)
		}
	case string(provider.Codex):
		go func() {
			if err := a.ReconnectSession(threadID); err != nil {
				log.Printf("mcp: thread %s codex reconnect for mcp change: %v", threadID, err)
				a.emitErrorToThread(threadID, fmt.Sprintf("mcp: reconnect failed: %v", err))
			}
		}()
	}
	return a.store.GetThread(threadID)
}

// reconcileClaudeMCPLive sends the same merged map startup uses to
// the live Claude session via mcp_set_servers. Re-using
// mergeMCPServersForThread (instead of inlining) is load-bearing —
// design entries, name collisions, and FilterEnabled rules must
// stay identical between launch and live update or the next turn
// would silently see a different tool surface than the previous.
func (a *App) reconcileClaudeMCPLive(threadID string, thread store.Thread) error {
	sess, ok := a.sessionManager().get(threadID)
	if !ok || sess.claude == nil {
		return nil
	}
	var designServers map[string]any
	if thread.Mode == "design" && a.designMCP != nil {
		servers, err := a.designMCPConfigForThread(thread)
		if err != nil {
			return fmt.Errorf("design mcp: %w", err)
		}
		designServers = servers
	}
	merged, collisions, err := a.mergeMCPServersForThread(threadID, thread.Provider, designServers)
	if err != nil {
		return err
	}
	if len(collisions) > 0 {
		log.Printf("mcp: thread %s: user library entries shadowed by design names: %v", threadID, collisions)
	}
	if merged == nil {
		merged = map[string]any{}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	diff, err := sess.claude.SetMCPServers(ctx, merged)
	if err != nil {
		return err
	}
	if diff != nil && (len(diff.Added) > 0 || len(diff.Removed) > 0 || len(diff.Errors) > 0) {
		log.Printf("mcp: thread %s claude diff added=%v removed=%v errors=%v", threadID, diff.Added, diff.Removed, diff.Errors)
	}
	return nil
}

// ProbeMcpServer runs the handshake for a library server. force
// bypasses the cache (the user-facing "Refresh" button uses it);
// the default path returns the cached result when fresh.
func (a *App) ProbeMcpServer(id string, force bool) (mcpprobe.Result, error) {
	if err := a.ensureMCPStore("probe mcp server"); err != nil {
		return mcpprobe.Result{}, err
	}
	server, err := a.store.GetMCPServer(id)
	if err != nil {
		return mcpprobe.Result{}, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	return a.mcpProbe().Get(ctx, server, force), nil
}

// GetMcpProbeSnapshot returns the currently-cached probe results for
// every server with a fresh entry. The popup uses it to render
// instant status on first open without waiting for one probe per
// server.
func (a *App) GetMcpProbeSnapshot() (map[string]mcpprobe.Result, error) {
	return a.mcpProbe().Snapshot(), nil
}

// TriggerMcpAuth drives the provider-side OAuth handshake for an
// http/sse MCP server the thread has selected. Server enablement is
// enforced at the binding boundary as defense in depth — the
// frontend hides the "Sign in" affordance when the server is not
// selected, but the backend refuses too so a LAN token-holder
// can't drive auth on an unselected (and therefore unmasked from
// disk) server.
//
// Codex's OAuth handler reads from on-disk config, so we batch-write
// the entry (with reloadUserConfig=true) before invoking the RPC.
// Claude's `mcp_authenticate` works directly against the in-session
// config that --mcp-config produced.
//
// If the thread's provider session is not live, we auto-start one —
// the OAuth callback listener belongs to the provider process, so
// the round-trip can't happen without a live session.
func (a *App) TriggerMcpAuth(threadID, serverID string) (MCPAuthInitResult, error) {
	if err := a.ensureMCPStore("trigger mcp auth"); err != nil {
		return MCPAuthInitResult{}, err
	}
	thread, err := a.store.GetThread(threadID)
	if err != nil {
		return MCPAuthInitResult{}, err
	}
	server, err := a.store.GetMCPServer(serverID)
	if err != nil {
		return MCPAuthInitResult{}, err
	}
	if !server.Enabled {
		return MCPAuthInitResult{}, fmt.Errorf("mcp: server %q is disabled at library level", server.Name)
	}
	if server.Transport != mcp.TransportHTTP && server.Transport != mcp.TransportSSE {
		return MCPAuthInitResult{}, fmt.Errorf("mcp: server %q transport %q does not support oauth", server.Name, server.Transport)
	}

	selectedIDs, err := a.store.ListThreadMCPServerIDs(threadID)
	if err != nil {
		return MCPAuthInitResult{}, err
	}
	selected := false
	for _, id := range selectedIDs {
		if id == serverID {
			selected = true
			break
		}
	}
	if !selected {
		return MCPAuthInitResult{}, fmt.Errorf("%w: %s", ErrMCPServerNotSelected, server.Name)
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

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	switch thread.Provider {
	case string(provider.Claude):
		if sess.claude == nil {
			return MCPAuthInitResult{}, ErrMCPSessionUnavailable
		}
		res, err := sess.claude.AuthenticateMCP(ctx, server.Name)
		if err != nil {
			return MCPAuthInitResult{}, err
		}
		// Probe-cache invalidation belongs in the OAuth-completion
		// notification handler, not here. AuthenticateMCP returns
		// before the user has even opened the browser, so an
		// invalidation at this point would race a follow-up probe
		// (popup re-render, status tick) and repopulate the cache
		// with stale "needs-auth" state.
		return MCPAuthInitResult{
			AuthURL:            res.AuthURL,
			Provider:           string(provider.Claude),
			RequiresUserAction: res.RequiresUserAction,
		}, nil

	case string(provider.Codex):
		if sess.codex == nil {
			return MCPAuthInitResult{}, ErrMCPSessionUnavailable
		}
		spec, err := mcp.RenderCodexSpec(server)
		if err != nil {
			return MCPAuthInitResult{}, fmt.Errorf("render codex spec: %w", err)
		}
		// Force enabled:true on the persisted row so a user-side
		// `enabled = false` left over from a manual hand-edit
		// doesn't shadow the AO-driven sign-in flow.
		spec["enabled"] = true
		if err := sess.codex.WriteMCPServerToUserConfig(ctx, server.Name, spec); err != nil {
			return MCPAuthInitResult{}, err
		}
		res, err := sess.codex.AuthenticateMCP(ctx, server.Name, nil, 0)
		if err != nil {
			return MCPAuthInitResult{}, err
		}
		// Same reasoning as the Claude branch: the OAuth-completion
		// notification handler owns invalidation; eagerly dropping
		// here would race a follow-up probe.
		return MCPAuthInitResult{
			AuthURL:            res.AuthorizationURL,
			Provider:           string(provider.Codex),
			RequiresUserAction: true,
		}, nil

	default:
		return MCPAuthInitResult{}, fmt.Errorf("mcp: unsupported provider %q", thread.Provider)
	}
}

// handleCodexMCPOAuthCompleted is the side-channel callback Codex
// fires after the user's browser hop completes the OAuth handshake.
// AO's job is small but load-bearing: invalidate any cached
// `needs-auth` probe entry for the matching library row so the next
// status read reflects the freshly-credentialed session. Success and
// failure both invalidate (failure may carry a different error
// shape on the next probe — `needs-auth` for retry, or `failed` for
// a permanent rejection — and we don't want a stale "needs-auth"
// to mask either).
//
// We also surface a `mcp:oauth-completed` event so an open popup
// listening for it can re-render immediately rather than waiting for
// the user to click Refresh. Failures additionally route to the
// thread error rail so the user sees why Sign-in didn't take.
func (a *App) handleCodexMCPOAuthCompleted(threadID, serverName string, success bool, errMsg string) {
	if a.store == nil {
		return
	}
	servers, err := a.store.ListMCPServers()
	if err != nil {
		log.Printf("mcp: oauth completion for %q (thread %s): list servers: %v", serverName, threadID, err)
		return
	}
	var matched *store.MCPServer
	for i := range servers {
		if servers[i].Name == serverName {
			matched = &servers[i]
			break
		}
	}
	if matched == nil {
		log.Printf("mcp: oauth completion for unknown server %q (thread %s)", serverName, threadID)
		return
	}
	a.mcpProbe().Invalidate(matched.ID)
	a.emit("mcp:oauth-completed", map[string]any{
		"threadId":   threadID,
		"serverId":   matched.ID,
		"serverName": matched.Name,
		"success":    success,
		"error":      errMsg,
	})
	if !success {
		msg := strings.TrimSpace(errMsg)
		if msg == "" {
			msg = "sign-in did not complete"
		}
		a.emitErrorToThread(threadID, fmt.Sprintf("mcp: %s: %s", serverName, msg))
	}
}

// appendMCPServerToProfile adds the id to the seed list if it isn't
// already there. Missing profile is treated as empty. Failures are
// logged but never propagated — the caller's CreateMcpServer has
// already succeeded and a missed profile-append recovers naturally
// on the next UpdateThreadMcpServers. Read-modify-write is serialised
// behind a.mcpProfileMu so two concurrent CreateMcpServer calls don't
// each read the same baseline and clobber the other's id on write.
func (a *App) appendMCPServerToProfile(id string) {
	a.mcpProfileMu.Lock()
	defer a.mcpProfileMu.Unlock()
	profile, err := a.store.GetMCPThreadProfile()
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		log.Printf("mcp: read profile for append %s: %v", id, err)
		return
	}
	for _, existing := range profile.ServerIDs {
		if existing == id {
			return
		}
	}
	next := append(profile.ServerIDs, id)
	if err := a.store.SetMCPThreadProfile(next); err != nil {
		log.Printf("mcp: append %s to profile: %v", id, err)
	}
}

// pruneMCPServerFromProfile strips id from the seed list so a
// CreateThread → seed cycle doesn't carry a now-dangling FK into the
// next thread's `thread_mcp_servers` insert. Shares a.mcpProfileMu
// with the append helper — the same race window applies (read,
// rewrite without the other call's mutation).
func (a *App) pruneMCPServerFromProfile(id string) {
	a.mcpProfileMu.Lock()
	defer a.mcpProfileMu.Unlock()
	profile, err := a.store.GetMCPThreadProfile()
	if err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			log.Printf("mcp: read profile for prune %s: %v", id, err)
		}
		return
	}
	filtered := profile.ServerIDs[:0]
	dirty := false
	for _, existing := range profile.ServerIDs {
		if existing == id {
			dirty = true
			continue
		}
		filtered = append(filtered, existing)
	}
	if !dirty {
		return
	}
	if err := a.store.SetMCPThreadProfile(filtered); err != nil {
		log.Printf("mcp: prune %s from profile: %v", id, err)
	}
}
