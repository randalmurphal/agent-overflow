package main

import (
	"encoding/json"
	"log"
	"sync/atomic"
	"time"

	"agent-overflow/internal/mcpstatus"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/claude"
)

func (a *App) sessionEventHandler(threadID, sessionToken, providerType string) func(provider.ProviderEvent) {
	// NewApp installs built-in observers during App construction. Keep the
	// once-guarded install here as well for tests and other internal callers
	// that intentionally construct a bare App before starting a session.
	a.installDiscussionTurnObserver()

	// deathReported flips true if the read loop emits a session_status
	// "error" — the wire-typed signal for an abnormal exit (signal, crash,
	// clean exit-0 we didn't initiate). Captured in the closure so the
	// disconnected branch can distinguish "process died on its own" (auto-
	// reconnect candidate) from "we asked it to stop" (no-op). Plain bool:
	// providers serialize delivery to this handler, so no atomic is needed.
	// Claude calls it from its read loop; Codex also has collaboration workers
	// and serializes all producers through Session.emitEvent.
	var deathReported bool
	// sawInit flips true on the session's first EventInit. An error
	// turn-complete BEFORE init is the dead-on-arrival signature: the
	// process failed during startup (Claude rejecting its
	// --resume-session-at cursor emits result{error_during_execution}
	// pre-init, then lingers alive) and can never serve a send. Same
	// serialized-callback justification as deathReported.
	var sawInit bool
	return func(evt provider.ProviderEvent) {
		// Design-mode tools used to be wired through Claude event
		// interception (handleClaudeDesignTool); after the v42 rewrite
		// Claude consumes the design MCP tools the same way Codex does
		// — via the HTTP MCP server registered through --mcp-config.
		// No event-side dispatch is required.

		if evt.Kind == provider.EventRateLimits {
			evt = a.attributeSessionRateLimits(evt, threadID, sessionToken)
		}
		a.recordSessionActivity(threadID, sessionToken, evt.Kind, evt.Content)

		// EventInit carries Claude's `system/init.mcp_servers` array
		// via the SessionInfo meta. Feed each entry into the status
		// cache so the popup shows authoritative provider state
		// (with the provider's own credentials) without a separate
		// fetch. Codex equivalents are wired in app_session.go via
		// the dedicated startup-update handler.
		if evt.Kind == provider.EventInit {
			sawInit = true
			if providerType == string(provider.Claude) {
				a.ingestClaudeInitMCPStatus(evt.Meta)
				go a.reconcileClaudeMCPOnInit(threadID)
			}
		}

		// A successful turn start is the wire-level proof that the new
		// session is alive and serving. Reset the per-thread auto-
		// reconnect attempt counter so a later (unrelated) death gets a
		// fresh recovery attempt instead of falling straight through to
		// the banner.
		if evt.Kind == provider.EventTurnStart {
			a.clearAutoReconnectAttempted(threadID)
		}

		if a.triage != nil {
			if err := a.triage.Handle(evt); err != nil {
				log.Printf("triage: %v", err)
			}
		}
		a.dispatchTurnObservers(threadID, evt)

		// Dead-on-arrival reap: a wire error result before this session
		// ever reached init means startup failed and the process is
		// useless yet still alive — Claude does not exit after failing
		// its --resume-session-at validation. Runs AFTER triage.Handle
		// so the orphan error item is already persisted and visible.
		// Goroutine because Close blocks on this very read loop.
		// Claude-only: the lingering-process failure mode is specific to
		// the Claude CLI, Codex startup failures surface through its
		// session-status/error paths, and Codex's readLoop starts before
		// its synchronous EventInit emission — sawInit could in
		// principle still be false when an early Codex wire frame lands,
		// so gating on it alone would not be sound there.
		if !sawInit && providerType == string(provider.Claude) && evt.Kind == provider.EventTurnComplete {
			if wire, ok := evt.TurnComplete.(*provider.WireTurnCompleteMeta); ok && wire != nil && wire.ErrorMessage != "" {
				go a.teardownDeadPreInitSession(threadID, sessionToken)
			}
		}

		if evt.Kind == provider.EventTurnComplete {
			// Rate-limit refresh on turn completion: piggy-back on the
			// event the user already triggered so the rings reflect the
			// cost of the turn that just finished. Fires in a goroutine
			// so the provider-specific probe doesn't block downstream
			// event handlers.
			switch providerType {
			case string(provider.Claude):
				go a.probeClaudeRateLimits(a.lifeCtx())
			case string(provider.Codex):
				go a.probeCodexRateLimits(a.lifeCtx())
			}
		}

		if evt.Kind == provider.EventSessionStatus && evt.Content == "error" {
			deathReported = true
			a.restoreUnconfirmedQueueOnSessionDeath(threadID)
		}

		if evt.Kind == provider.EventSessionStatus && evt.Content == "disconnected" {
			a.unregisterSession(threadID, sessionToken)
			if a.workflowRunner != nil {
				a.workflowRunner.sessionDisconnected(threadID)
			}
			if deathReported {
				go a.attemptAutoReconnect(threadID)
			}
		}
	}
}

func (a *App) attributeSessionRateLimits(
	evt provider.ProviderEvent,
	threadID, sessionToken string,
) provider.ProviderEvent {
	sess, ok := a.sessionManager().get(threadID)
	if !ok || sess.token != sessionToken || sess.credentialAccountID == "" || len(evt.Meta) == 0 {
		return evt
	}
	var snapshot provider.RateLimitsSnapshot
	if err := json.Unmarshal(evt.Meta, &snapshot); err != nil || snapshot.AccountID != "" {
		return evt
	}
	snapshot.AccountID = sess.credentialAccountID
	meta, err := json.Marshal(snapshot)
	if err != nil {
		log.Printf("provider events: attribute rate limits for thread %s: %v", threadID, err)
		return evt
	}
	evt.Meta = meta
	return evt
}

// attemptAutoReconnect is the recovery hook fired after the read loop
// observed an abnormal exit ("error" → "disconnected" pair) on a session.
// At this point the provider process is already reaped and the session is
// unregistered, so there are no live background processes to disturb; the
// orphaned tool_call rows in the store match exactly what manual Reconnect
// would have left behind. We resume from the thread's stored SessionRef
// when one exists and the death is the first since the last observed
// turn_started; if reconnect itself fails or a second death lands before
// the new session reaches turn_started, the session_died banner already
// dispatched stays put for the user to act on.
func (a *App) attemptAutoReconnect(threadID string) {
	if a.shuttingDown.Load() {
		return
	}
	thread, err := a.store.GetThread(threadID)
	if err != nil {
		log.Printf("app: auto-reconnect lookup failed for %s: %v", threadID, err)
		return
	}
	if thread.Mode == "workflow" {
		// Workflow attempts are resumed only by the workflow engine. Generic
		// reconnect would spawn the provider after the runner has already
		// parked the failed attempt and released its phase schema.
		return
	}
	if thread.SessionRef == "" && thread.PendingForkRef == "" {
		// Death before the provider published a resume cursor — nothing
		// to --resume against. Leave the banner up; the user can decide
		// whether to retry or abandon the thread. Don't consume the
		// attempt slot here: a later (manually-started) session that
		// reaches system/init and then dies still deserves an auto-
		// reconnect attempt.
		return
	}
	if !a.markAutoReconnectAttempted(threadID) {
		return
	}
	if err := a.ReconnectSession(threadID); err != nil {
		log.Printf("app: auto-reconnect after session death failed for %s: %v", threadID, err)
	}
}

func (a *App) markAutoReconnectAttempted(threadID string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.autoReconnectAttempted == nil {
		a.autoReconnectAttempted = make(map[string]bool)
	}
	if a.autoReconnectAttempted[threadID] {
		return false
	}
	a.autoReconnectAttempted[threadID] = true
	return true
}

func (a *App) clearAutoReconnectAttempted(threadID string) {
	a.mu.Lock()
	delete(a.autoReconnectAttempted, threadID)
	a.mu.Unlock()
}

// recordSessionActivity is the single chokepoint that bumps a session's
// last-activity timestamp and tracks turn-in-flight state for the idle
// reaper. Called from sessionEventHandler on every wire event so any
// provider-driven signal — text deltas, tool starts, approvals, errors,
// thinking, init — counts as activity that keeps the subprocess alive.
//
// The token guard matches unregisterSession's pattern: if a fresher
// session has replaced the one this handler closes over, ignore the
// bump rather than mutating the new session's liveness counters.
//
// activeTurns is ±1 on EventTurnStart/EventTurnComplete. Provider
// asymmetry: only Codex emits EventTurnStart (see
// internal/provider/codex/protocol.go); Claude's session never emits
// it (triage synthesizes turn-start from system.init downstream of
// this chokepoint). For Claude sessions activeTurns therefore stays
// at zero and the reaper's mid-turn skip relies entirely on the
// lastActivity floor + the running-bg-tool-calls store probe. That's
// safe because Claude's deltas stream through here at high cadence
// during a turn — lastActivity is constantly being refreshed.
//
// On EventSessionStatus("disconnected") we drain activeTurns to 0
// because the subprocess is going away and any in-flight counter is
// no longer meaningful. Without the drain, a turn that errored before
// EventTurnComplete would leave the counter stuck at 1 forever — the
// reaper would then refuse to reap that thread even if a new session
// took its place with no active turn.
func (a *App) recordSessionActivity(threadID, sessionToken string, kind provider.EventKind, content string) {
	a.sessionManager().recordActivity(threadID, sessionToken, kind, content, time.Now())
}

// decrementActiveTurnsClamped does activeTurns.Add(-1) with a clamp
// at zero so an unmatched EventTurnComplete (replay, double-fire)
// can't drive the counter negative. Race-correct under any number of
// concurrent decrements via CAS retry.
func decrementActiveTurnsClamped(turns *atomic.Int32) {
	for {
		cur := turns.Load()
		if cur <= 0 {
			return
		}
		if turns.CompareAndSwap(cur, cur-1) {
			return
		}
	}
}

// ingestClaudeInitMCPStatus decodes the SessionInfo carried on an
// EventInit Meta and pushes each mcp_servers entry into the
// mcpstatus cache. Source is stamped as live-session so the UI can
// disclose freshness; the raw provider status string is preserved
// for forensics.
//
// Silently no-ops on decode failure or empty list — init is also
// emitted by Codex (via its own session.go) and other future
// providers where the field may not be populated. Cache feeds are
// best-effort; the popup falls back to ephemeral fetch when no
// live entry exists.
func (a *App) ingestClaudeInitMCPStatus(meta json.RawMessage) {
	if len(meta) == 0 {
		return
	}
	var info provider.SessionInfo
	if err := json.Unmarshal(meta, &info); err != nil {
		return
	}
	if len(info.MCPServers) == 0 {
		return
	}
	cache := a.mcpStatus()
	now := time.Now()
	for _, s := range info.MCPServers {
		name := s.Name
		if name == "" {
			continue
		}
		cache.Put(mcpstatus.ServerStatus{
			Key:       mcpstatus.Key{Provider: mcpstatus.ProviderClaude, Name: name},
			Status:    claude.MCPStatusFromRaw(s.Status),
			Raw:       s.Status,
			Source:    mcpstatus.SourceLiveSession,
			CheckedAt: now,
		})
	}
}

func (a *App) unregisterSession(threadID, sessionToken string) {
	current, exists := a.sessionManager().get(threadID)
	if !exists || current.token != sessionToken {
		return
	}
	removed, ok := a.sessionManager().unregister(threadID, sessionToken)
	if !ok {
		return
	}
	a.emitProviderSessionDisconnected(threadID, removed.provider)

	// Self-exit teardown bypasses closeProviderSession (the subprocess is
	// already dead — see attemptAutoReconnect), so release its group from
	// the orphan reaper here. Without this, a crashed provider's pgid would
	// linger in the sidecar's watched set with no start-time guard, risking
	// a wrong-group kill if the OS later recycles that PID and the app then
	// dies. Release is idempotent and pgid<=1 is guarded inside.
	if ps := removed.providerSession(); ps != nil {
		a.releaseSessionProcess(ps.PID())
	}
	if a.triage != nil {
		a.triage.ClearEffectiveModel(threadID)
	}

	// The provider read loop is gone, so no flush tick can re-register
	// seeder state; drop what a mid-stream crash stranded (racing settle
	// goroutines only send final ticks, which self-clean on an ephemeral
	// state). Without this, a crashed stream's entries survive into the
	// thread's next session with a stale fence watermark.
	a.highlightSeeder.purgeThread(threadID)

	a.teardownDesignThread(threadID)
}
