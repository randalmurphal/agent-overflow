package main

import (
	"fmt"
	"log"

	"agent-overflow/internal/provider"
)

func (a *App) sessionEventHandler(threadID, sessionToken string) func(provider.ProviderEvent) {
	return func(evt provider.ProviderEvent) {
		a.handleClaudeDesignTool(evt)

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
		}

		if evt.Kind == provider.EventSessionStatus && evt.Content == "disconnected" {
			a.unregisterSession(threadID, sessionToken)
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
