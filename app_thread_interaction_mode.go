package main

import (
	"fmt"
	"strings"

	"agent-overflow/internal/store"
)

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
	return a.store.GetThread(threadID)
}
