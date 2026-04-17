package main

import (
	"fmt"
	"log"
	"strings"

	"agent-overflow/internal/store"
)

// ThreadInteractionModeChangedEvent is emitted over the "thread:interaction_mode_changed"
// channel whenever a thread's mode is updated while a provider session is
// active. The frontend uses NeedsReconnect to show a toast that prompts the
// user to reconnect — the running session was started with the previous
// mode and will keep using it until a new session is spawned.
type ThreadInteractionModeChangedEvent struct {
	ThreadID        string `json:"threadId"`
	InteractionMode string `json:"interactionMode"`
	NeedsReconnect  bool   `json:"needsReconnect"`
}

// Valid interaction modes that the UI / new-thread flow is allowed to set.
// "discussion" is intentionally excluded — those threads are created by
// StartDiscussion because they require a deliberation channel and participant
// child threads. Letting the UI set "discussion" directly would produce orphan
// threads that the discussion runtime never knows about.
var interactionModesForManualSelection = map[string]struct{}{
	"default": {},
	"plan":    {},
	"design":  {},
}

// interactionModeIsAllowedForPostCreation is a superset: once a thread exists
// the user may also switch back into discussion mode if desired (the backend
// treats "discussion" as an informational marker when no channel is attached).
var interactionModesForPostCreation = map[string]struct{}{
	"default":    {},
	"plan":       {},
	"design":     {},
	"discussion": {},
}

// validateCreateThreadMode normalizes the mode for CreateThread. An empty
// string is accepted and normalized to "default" so existing callers that
// don't care about the mode keep working. "discussion" is rejected to keep
// StartDiscussion as the only path that produces discussion-mode threads.
func validateCreateThreadMode(mode string) (string, error) {
	trimmed := strings.TrimSpace(mode)
	if trimmed == "" {
		return "default", nil
	}
	if _, ok := interactionModesForManualSelection[trimmed]; !ok {
		return "", fmt.Errorf("invalid interaction mode %q (allowed: default, plan, design)", trimmed)
	}
	return trimmed, nil
}

// validateSetInteractionMode validates a mode for SetThreadInteractionMode.
// Unlike create-time, "discussion" is accepted here because the caller may
// be restoring a previously-demoted discussion thread. The store layer keeps
// the field in sync; the deliberation runtime keys off DiscussionID, not this
// column.
func validateSetInteractionMode(mode string) (string, error) {
	trimmed := strings.TrimSpace(mode)
	if _, ok := interactionModesForPostCreation[trimmed]; !ok {
		return "", fmt.Errorf("invalid interaction mode %q (allowed: default, plan, design, discussion)", trimmed)
	}
	return trimmed, nil
}

// SetThreadInteractionMode updates the thread's interaction mode and returns
// the refreshed row. Rejects unknown modes with a clear error message so the
// frontend can surface it verbatim.
//
// When the thread has an active provider session at the time of the change we
// emit a "thread:interaction_mode_changed" event with NeedsReconnect=true so
// the frontend can prompt the user to reconnect — the running session was
// started under the previous mode and will not pick up the new one until a
// new session is spawned. The persisted row is always updated regardless.
func (a *App) SetThreadInteractionMode(threadID string, mode string) (store.Thread, error) {
	normalized, err := validateSetInteractionMode(mode)
	if err != nil {
		return store.Thread{}, err
	}
	if _, err := a.store.GetThread(threadID); err != nil {
		return store.Thread{}, err
	}
	if err := a.store.UpdateInteractionMode(threadID, normalized); err != nil {
		return store.Thread{}, err
	}

	a.mu.Lock()
	_, sessionActive := a.sessions[threadID]
	a.mu.Unlock()
	if sessionActive {
		log.Printf("thread %s: interaction mode changed to %q while session is active; reconnect required", threadID, normalized)
	}
	a.emitEvent("thread:interaction_mode_changed", ThreadInteractionModeChangedEvent{
		ThreadID:        threadID,
		InteractionMode: normalized,
		NeedsReconnect:  sessionActive,
	})

	return a.store.GetThread(threadID)
}
