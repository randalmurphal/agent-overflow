package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"agent-overflow/internal/ctxutil"
	"agent-overflow/internal/mcpstatus"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/claude"
	"agent-overflow/internal/provider/codex"
)

func (a *App) triggerMcpAuth(threadID, name string) (MCPAuthInitResult, error) {
	if a.store == nil {
		return MCPAuthInitResult{}, errors.New("trigger mcp auth: store unavailable")
	}
	thread, err := a.store.GetThread(threadID)
	if err != nil {
		return MCPAuthInitResult{}, err
	}
	if !a.hasActiveSession(threadID) {
		if err := a.startSession(context.Background(), threadID); err != nil {
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
// toast. mcp:status and mcp:oauth-completed are loopback-only
// (internal/transport/event_visibility.go — every MCP RPC is LocalOnly,
// so the push side is the third door), but provider:item_event and
// toasts are not, and the bound also keeps a provider's stderr dump out
// of the UI wholesale. The Claude CLI and Codex app-server both inherit
// AO's `os.Environ()` (intentionally — env vars resolve MCP
// bearer-token indirection), so a future provider panic that dumped its
// env could otherwise channel a token through verbatim. 256B + newline
// collapse matches the equivalent defense
// `internal/provider/claude/mcpstatus.go` applies to child-process
// stderr; keeping a second copy here is deliberate so the wire-facing
// handlers don't depend on a private claude helper. The truncation cut
// backs off to a rune boundary so it never manufactures U+FFFD.
func sanitizeMCPError(s string) string {
	s = strings.TrimSpace(s)
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "\r", " ")
	limit := 256
	if len(s) <= limit {
		return s
	}
	for limit > 0 && !utf8.RuneStart(s[limit]) {
		limit--
	}
	return s[:limit] + "…(truncated)"
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
// AO invalidates the status cache so the next read reflects the
// freshly-credentialed session, surfaces a `mcp:oauth-completed` event
// for any popup listening, and — on success — hot-reloads the thread's
// live Codex session.
//
// The reload is what makes the sign-in take effect. A loaded thread
// keeps the MCP manager it started with, so a server that failed
// startup with an expired grant stays failed for the rest of the
// session no matter how the OAuth went; the user's only other recovery
// is a manual toggle or an app restart. `config/mcpServer/reload`
// spawns nothing of AO's — it is one RPC on the session that is already
// running, telling Codex to re-read config and mark loaded threads' MCP
// runtime dirty. Codex applies it at the next turn boundary and emits a
// fresh `startupStatus` round, which flows back through the retained
// session state and the status cache. (The older "don't refetch here"
// rule was about spawning ephemeral fetch processes per notification —
// that still holds, and this is not that.)
//
// Accepted side effect: the reload re-reads the WHOLE config, so an
// unrelated hand-edit to `~/.codex/config.toml` also lands at the next
// turn. That is the same bargain the MCP toggle path already makes.
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
		// serverName is provider-supplied too — same bound as the error
		// beside it before it reaches a persisted thread item.
		a.emitErrorToThread(threadID, fmt.Sprintf("mcp: %s: %s", sanitizeMCPError(serverName), msg))
		return
	}
	sess, ok := a.sessionManager().get(threadID)
	if !ok || sess.codex == nil {
		return
	}
	// The retained startup failure describes the run this sign-in just
	// invalidated; without the forget it would outrank the settled list
	// until Codex's next startupStatus round (the next turn boundary),
	// rendering "Failed / Sign in again" over a sign-in that succeeded.
	sess.codex.ForgetMCPStartupState(serverName)
	a.requestCodexMCPReload(threadID)
}

// handleCodexMCPStartupUpdate is the per-thread side-channel
// callback Codex fires as MCP servers move through
// starting → ready / failed / cancelled during thread/start.
// Feed each delta into the status cache with Source="notification"
// so the popup reflects live provider state without an ephemeral
// refetch. Cache emits over `mcp:status`, so subscribers update
// reactively.
//
// The whole update goes to the projector, not just the state: a failure
// carrying `reauthenticationRequired` resolves to needs-auth, which is
// what turns the popup row's dead error string into the existing Sign in
// action.
func (a *App) handleCodexMCPStartupUpdate(u codex.MCPStartupUpdate) {
	if strings.TrimSpace(u.Name) == "" {
		return
	}
	a.mcpStatus().Put(mcpstatus.ServerStatus{
		Key:       mcpstatus.Key{Provider: mcpstatus.ProviderCodex, Name: u.Name},
		Status:    codex.MCPStatusFromNotif(u),
		Raw:       u.State,
		Error:     sanitizeMCPError(u.Error),
		Source:    mcpstatus.SourceNotification,
		CheckedAt: time.Now(),
	})
}
