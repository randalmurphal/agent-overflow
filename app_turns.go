package main

import (
	"agent-overflow/internal/store"
)

// ListRecentTurns returns the N most recent turn records for the given
// thread, newest first. Used by the frontend on thread-switch to rehydrate
// the latest settled-turn projection.
//
// The frontend MUST NOT light up the working indicator from these rows —
// an in-flight (completed_at=NULL) row from a prior session/crash is
// historical, not live. Only a fresh `provider:turn_started` push can set
// `pane.activeTurn`. See docs/architecture/invariants.md #22 and
// docs/architecture/turn-lifecycle.md §Frontend state shape.
func (a *App) ListRecentTurns(threadID string, limit int) ([]store.Turn, error) {
	return a.store.ListRecentTurns(threadID, limit)
}
