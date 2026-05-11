package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"

	"agent-overflow/internal/provider"
)

func (a *App) sessionEventHandler(threadID, sessionToken, providerType string) func(provider.ProviderEvent) {
	return func(evt provider.ProviderEvent) {
		// Design-mode tools used to be wired through Claude event
		// interception (handleClaudeDesignTool); after the v42 rewrite
		// Claude consumes the design MCP tools the same way Codex does
		// — via the HTTP MCP server registered through --mcp-config.
		// No event-side dispatch is required.

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
