package main

import (
	"fmt"
	"log"
	"strings"

	"agent-overflow/internal/store"
)

// ThreadModeChangedEvent is emitted whenever a thread's mode is updated
// while a provider session is active. The frontend uses NeedsReconnect
// to show a toast that prompts the user to reconnect — the running
// session was started under the previous mode and will keep using it
// until a new session is spawned.
type ThreadModeChangedEvent struct {
	ThreadID       string `json:"threadId"`
	Mode           string `json:"mode"`
	NeedsReconnect bool   `json:"needsReconnect"`
}

// Valid modes that the UI / new-thread flow is allowed to set.
// "discussion" is intentionally excluded — those threads are created
// by StartDiscussion because they require a deliberation channel and
// participant child threads. Letting the UI set "discussion" directly
// would produce orphan threads that the discussion runtime never knows
// about.
var modesForManualSelection = map[string]struct{}{
	"chat":   {},
	"plan":   {},
	"design": {},
}

// modesForPostCreation is a superset: once a thread exists the user may
// also switch back into discussion mode if desired (the backend treats
// "discussion" as an informational marker when no channel is attached).
var modesForPostCreation = map[string]struct{}{
	"chat":       {},
	"plan":       {},
	"design":     {},
	"discussion": {},
}

// validateCreateThreadMode normalizes the mode for CreateThread. An empty
// string is accepted and normalized to "chat" so existing callers that
// don't care about the mode keep working. "discussion" is rejected to keep
// StartDiscussion as the only path that produces discussion-mode threads.
func validateCreateThreadMode(mode string) (string, error) {
	trimmed := strings.TrimSpace(mode)
	if trimmed == "" {
		return "chat", nil
	}
	if _, ok := modesForManualSelection[trimmed]; !ok {
		return "", fmt.Errorf("invalid mode %q (allowed: chat, plan, design)", trimmed)
	}
	return trimmed, nil
}

// validateSetMode validates a mode for UpdateThreadMode. Unlike
// create-time, "discussion" is accepted here because the caller may be
// restoring a previously-demoted discussion thread. The store layer
// keeps the field in sync; the deliberation runtime keys off
// DiscussionID, not this column.
func validateSetMode(mode string) (string, error) {
	trimmed := strings.TrimSpace(mode)
	if _, ok := modesForPostCreation[trimmed]; !ok {
		return "", fmt.Errorf("invalid mode %q (allowed: chat, plan, design, discussion)", trimmed)
	}
	return trimmed, nil
}

// UpdateThreadMode updates the thread's mode and returns the refreshed
// row. Rejects unknown modes with a clear error message so the frontend
// can surface it verbatim.
//
// When the thread has an active provider session at the time of the
// change we emit a "thread:mode_changed" event with NeedsReconnect=true
// so the frontend can prompt the user to reconnect — the running
// session was started under the previous mode and will not pick up the
// new one until a new session is spawned. The persisted row is always
// updated regardless.
func (a *App) UpdateThreadMode(threadID string, mode string) (store.Thread, error) {
	normalized, err := validateSetMode(mode)
	if err != nil {
		return store.Thread{}, err
	}
	if _, err := a.store.GetThread(threadID); err != nil {
		return store.Thread{}, err
	}
	if err := a.store.UpdateMode(threadID, normalized); err != nil {
		return store.Thread{}, err
	}

	a.mu.Lock()
	_, sessionActive := a.sessions[threadID]
	a.mu.Unlock()
	if sessionActive {
		log.Printf("thread %s: mode changed to %q while session is active; reconnect required", threadID, normalized)
	}
	a.emitEvent("thread:mode_changed", ThreadModeChangedEvent{
		ThreadID:       threadID,
		Mode:           normalized,
		NeedsReconnect: sessionActive,
	})

	return a.store.GetThread(threadID)
}
