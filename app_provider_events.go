package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"sync/atomic"
	"time"

	"agent-overflow/internal/provider"
)

func (a *App) sessionEventHandler(threadID, sessionToken, providerType string) func(provider.ProviderEvent) {
	return func(evt provider.ProviderEvent) {
		// Design-mode tools used to be wired through Claude event
		// interception (handleClaudeDesignTool); after the v42 rewrite
		// Claude consumes the design MCP tools the same way Codex does
		// — via the HTTP MCP server registered through --mcp-config.
		// No event-side dispatch is required.

		a.recordSessionActivity(threadID, sessionToken, evt.Kind, evt.Content)

		if evt.Kind == provider.EventInit {
			a.cacheSlashCommandsFromInit(threadID, evt.Meta)
		}

		if a.triage != nil {
			if err := a.triage.Handle(evt); err != nil {
				log.Printf("triage: %v", err)
			}
		}
		if evt.Kind == provider.EventTurnComplete {
			if err := a.syncDiscussionTurn(threadID); err != nil {
				log.Printf("discussion runtime: %v", err)
				// Emit an error event so the UI knows the discussion sync
				// failed. The turn-complete event still propagates (we can't
				// block it), but the error should be visible.
				a.emitErrorToThread(threadID, fmt.Sprintf("discussion sync failed: %v", err))
			}
			// Rate-limit refresh on Claude turn completion: piggy-back on
			// the event the user already triggered so the rings reflect
			// the cost of the turn that just finished. Fires in a
			// goroutine so the HTTP call doesn't block downstream event
			// handlers; Codex turns intentionally skip this because the
			// probe targets Anthropic's API.
			if providerType == string(provider.Claude) {
				go a.probeClaudeRateLimits(context.Background())
			}
		}

		if evt.Kind == provider.EventSessionStatus && evt.Content == "disconnected" {
			a.unregisterSession(threadID, sessionToken)
		}
	}
}

// cacheSlashCommandsFromInit decodes the SessionInfo.Meta payload shipped with
// an EventInit and caches the Claude slash-command list for the thread. No-ops
// for payloads that lack the field (Codex) or fail to parse — the composer
// popover tolerates an empty cache.
func (a *App) cacheSlashCommandsFromInit(threadID string, meta json.RawMessage) {
	if len(meta) == 0 {
		return
	}
	var info provider.SessionInfo
	if err := json.Unmarshal(meta, &info); err != nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.threadSlashCommands == nil {
		a.threadSlashCommands = make(map[string][]string)
	}
	// Always overwrite: a fresh init replaces the prior list even when the new
	// one is empty (e.g. user deleted their command files between sessions).
	if len(info.SlashCommands) == 0 {
		delete(a.threadSlashCommands, threadID)
		return
	}
	copied := make([]string, len(info.SlashCommands))
	copy(copied, info.SlashCommands)
	a.threadSlashCommands[threadID] = copied
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
	a.mu.Lock()
	current, ok := a.sessions[threadID]
	a.mu.Unlock()
	if !ok || current.token != sessionToken || current.liveness == nil {
		return
	}
	current.liveness.bumpActivity(time.Now())
	switch kind {
	case provider.EventTurnStart:
		current.liveness.activeTurns.Add(1)
	case provider.EventTurnComplete:
		// Clamp to 0 so an unmatched TurnComplete (e.g. from a replayed
		// envelope) can't drive the counter negative.
		decrementActiveTurnsClamped(&current.liveness.activeTurns)
	case provider.EventSessionStatus:
		if content == "disconnected" {
			current.liveness.activeTurns.Store(0)
		}
	}
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

func (a *App) unregisterSession(threadID, sessionToken string) {
	a.mu.Lock()
	current, ok := a.sessions[threadID]
	if !ok || current.token != sessionToken {
		a.mu.Unlock()
		return
	}
	delete(a.sessions, threadID)
	a.mu.Unlock()

	a.teardownDesignThread(threadID)
}
