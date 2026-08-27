package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"agent-overflow/internal/ctxutil"
	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/mcpstatus"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/claude"
	"agent-overflow/internal/provider/codex"
	"agent-overflow/internal/threadmode"
)

func (a *App) triggerMcpAuth(threadID, name string) (MCPAuthInitResult, error) {
	name = strings.TrimSpace(name)
	if isAppManagedMCPServer(name) {
		return MCPAuthInitResult{}, errors.New("trigger mcp auth: built-in browser does not use provider OAuth")
	}
	if a.store == nil {
		return MCPAuthInitResult{}, errors.New("trigger mcp auth: store unavailable")
	}
	thread, err := a.store.GetThread(threadID)
	if err != nil {
		return MCPAuthInitResult{}, err
	}
	// A design thread spawns with `--mcp-config <design servers>
	// --strict-mcp-config`, so its session cannot see a single workspace
	// MCP server. Without this guard the auto-start below would spawn
	// exactly that session and ask it to authenticate a name it will
	// never resolve — the CLI answers `Server not found: <name>` and the
	// user gets a sign-in-failed toast blaming the server. The toggle
	// path already carves design threads out the same way
	// (setClaudeThreadMCPEnabled); the OAuth grant itself is global to
	// the CLI's secure storage, so a regular thread in any workspace can
	// perform it.
	if thread.Mode == threadmode.ModeDesign {
		return MCPAuthInitResult{}, ErrMCPDesignThreadUnsupported
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
// App.mcp.claudeOAuthPolls. The struct address is the identity — a
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

	a.mcp.claudeOAuthPollsMu.Lock()
	if a.mcp.claudeOAuthPolls == nil {
		a.mcp.claudeOAuthPolls = map[string]*claudeMCPOAuthPoll{}
	}
	if prior, ok := a.mcp.claudeOAuthPolls[serverName]; ok {
		prior.cancel()
	}
	a.mcp.claudeOAuthPolls[serverName] = poll
	a.mcp.claudeOAuthPollsMu.Unlock()

	go func() {
		defer func() {
			a.mcp.claudeOAuthPollsMu.Lock()
			// Only clear the slot if it still points at our poll —
			// a newer caller may have replaced us mid-loop.
			if a.mcp.claudeOAuthPolls[serverName] == poll {
				delete(a.mcp.claudeOAuthPolls, serverName)
			}
			a.mcp.claudeOAuthPollsMu.Unlock()
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

type claudeMCPOAuthObservation struct {
	status   mcpstatus.Status
	raw      string
	error    string
	timedOut bool
	aborted  bool
}

// waitForClaudeMCPOAuth is the shared provider-observation loop for both a
// thread-owned session and a temporary workspace-auth process. Keeping the
// state machine here prevents those two entry points from disagreeing about
// terminal statuses, transient query failures, cancellation, or timeout.
func waitForClaudeMCPOAuth(
	ctx context.Context,
	serverName string,
	intervals []time.Duration,
	getQuerier func() claudeMCPStatusQuerier,
) claudeMCPOAuthObservation {
	for _, d := range intervals {
		if !ctxutil.Sleep(ctx, d) {
			return claudeMCPOAuthObservation{aborted: true}
		}
		query := getQuerier()
		if query == nil {
			return claudeMCPOAuthObservation{aborted: true}
		}
		statuses, err := query(ctx)
		if err != nil {
			continue
		}
		for i := range statuses {
			if statuses[i].Name != serverName {
				continue
			}
			mapped := claude.MCPStatusFromRaw(statuses[i].Status)
			if mapped == mcpstatus.StatusConnected || mapped == mcpstatus.StatusFailed {
				return claudeMCPOAuthObservation{
					status: mapped,
					raw:    statuses[i].Status,
					error:  statuses[i].Error,
				}
			}
			break
		}
	}
	return claudeMCPOAuthObservation{timedOut: true, error: "sign-in not confirmed"}
}

// defaultClaudeMCPOAuthIntervals is a Fibonacci-shaped ramp for fast
// browser flows (most OAuth completes inside 10s), then a steady 15s
// cadence out to a ~5-minute total budget for the slow ones: an IdP
// login the user has to complete first, or a hop they came back to
// after a distraction. Each tick is one `mcp_status` control_request
// answered from the CLI's in-memory client pools — no API call, no
// token cost — so the long tail is nearly free. When the budget
// exhausts, the poll reports "not confirmed" rather than going
// silent (see pollClaudeMCPAfterOAuth). Tests pass zero-duration
// slices to drive the loop deterministically without changing the
// tick count.
var defaultClaudeMCPOAuthIntervals = func() []time.Duration {
	intervals := []time.Duration{
		1 * time.Second,
		2 * time.Second,
		3 * time.Second,
		5 * time.Second,
		8 * time.Second,
		13 * time.Second,
	}
	total := 32 * time.Second
	for total < 5*time.Minute {
		intervals = append(intervals, 15*time.Second)
		total += 15 * time.Second
	}
	return intervals
}()

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
// canceled.
//
// Exhaustion is "not confirmed", never "failed": the CLI's in-flight
// OAuth flow outlives this poll (it dies only when superseded or the
// process exits), so a sign-in the poll gave up on can still land.
// The tail therefore invalidates the cache entry instead of writing
// one — the sentinel re-lists every pane carrying the server, and a
// live-session re-list queries the CLI fresh, flipping the row to
// connected when the slow hop did complete — and emits
// `mcp:oauth-completed{success:false, timedOut:true}`. Deliberately
// NO thread-error: the common way to reach the tail is the user
// abandoning the browser hop on purpose, and an error item landing
// five minutes after they changed their mind is noise, not signal
// (user ruling 2026-08-21). Abandon-and-retry needs no affordance
// here — the row keeps its Sign in button throughout, and a new
// click supersedes both the CLI's flow and this poll. Before this
// tail existed the poll ended in silence, and a sign-in completed
// after ~32s left the row on "Needs sign-in" with no signal anything
// was stale. Ctx cancellation (shutdown, or a superseding Sign In
// click) still exits without emitting — a fresh poll owns the flow
// now, and a timeout verdict from the dead one would be a lie.
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
	observation := waitForClaudeMCPOAuth(ctx, serverName, intervals, getQuerier)
	if observation.aborted {
		return
	}
	if !observation.timedOut {
		// Shutdown race guard. appCtx is cancelled in Shutdown step 1b,
		// BEFORE drainTriage. Bail before emitErrorToThread can file a
		// triage.Handle past the drain barrier.
		if ctx.Err() != nil {
			return
		}
		sanitizedErr := sanitizeMCPError(observation.error)
		a.mcpStatus().Put(mcpstatus.ServerStatus{
			Key:       mcpstatus.Key{Provider: mcpstatus.ProviderClaude, Name: serverName},
			Status:    observation.status,
			Raw:       observation.raw,
			Error:     sanitizedErr,
			Source:    mcpstatus.SourceLiveSession,
			CheckedAt: time.Now(),
		})
		success := observation.status == mcpstatus.StatusConnected
		a.emit(eventchan.MCPOAuthCompleted, map[string]any{
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
	}
	// Budget exhausted without a terminal answer. Same shutdown race
	// guard as the terminal branch: past the drain barrier nothing may
	// emit or file a triage.
	if ctx.Err() != nil {
		return
	}
	a.mcpStatus().Invalidate(mcpstatus.Key{Provider: mcpstatus.ProviderClaude, Name: serverName})
	a.emit(eventchan.MCPOAuthCompleted, map[string]any{
		"threadId":   threadID,
		"provider":   mcpProviderClaude,
		"serverName": serverName,
		"success":    false,
		"timedOut":   true,
		"error":      "sign-in not confirmed",
	})
}

// handleCodexMCPOAuthCompleted is the side-channel callback Codex
// fires after the user's browser hop completes the OAuth handshake.
// AO invalidates the status cache so the next read reflects the
// freshly-credentialed session, surfaces a `mcp:oauth-completed` event
// for any popup listening, and — on success — hot-reloads every live Codex
// session. The grant and config are provider-global, so limiting the reload to
// the initiating thread leaves sibling app-servers holding the same stale
// startup failure.
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
	a.emit(eventchan.MCPOAuthCompleted, map[string]any{
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
		if threadID != "" {
			a.emitErrorToThread(threadID, fmt.Sprintf("mcp: %s: %s", sanitizeMCPError(serverName), msg))
		}
		return
	}
	// The grant and on-disk Codex MCP config are provider-global. Every live
	// app-server can retain the failed startup this login invalidated, so reload
	// all of them rather than leaving sibling panes stale until their next
	// restart.
	for _, live := range a.sessionManager().codexMCPSessions() {
		live.session.ForgetMCPStartupState(serverName)
		a.requestCodexMCPReload(live.threadID)
	}
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
