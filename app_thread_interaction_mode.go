package main

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"agent-overflow/internal/provider"
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

// modesForPostCreation is the set the UI is allowed to mutate into via the
// chat/plan agent-mode toggle. Thread *type* (design / discussion) is
// determined at creation and is immutable thereafter — switching the type of
// a live thread would orphan its associated runtime state (design artifacts,
// deliberation channel) and confuse the UI shell. Internal callers that need
// to flip a chat thread's interaction mode (sendMessage's plan→chat saga,
// proposed-plan revisions) only ever move between chat and plan.
var modesForPostCreation = map[string]struct{}{
	"chat": {},
	"plan": {},
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

// validateSetMode validates a mode for UpdateThreadMode. Only chat and plan
// are accepted: design and discussion are immutable thread types set at
// creation time. The frontend's agent-mode toggle (chat ↔ plan) is the only
// caller that should hit UpdateThreadMode at user-facing scope; internal
// callsites (proposed-plan saga) only ever pass chat or plan.
func validateSetMode(mode string) (string, error) {
	trimmed := strings.TrimSpace(mode)
	if _, ok := modesForPostCreation[trimmed]; !ok {
		return "", fmt.Errorf("invalid mode %q (allowed: chat, plan)", trimmed)
	}
	return trimmed, nil
}

// UpdateThreadMode updates the thread's mode and returns the refreshed
// row. Rejects unknown modes with a clear error message so the frontend
// can surface it verbatim.
//
// Active Claude sessions apply chat/plan changes immediately through
// set_permission_mode. Codex reads the persisted mode on the next turn/start.
// Modes that cannot be applied to the live session emit NeedsReconnect=true.
// The persisted row is always updated regardless.
func (a *App) UpdateThreadMode(threadID string, mode string) (store.Thread, error) {
	normalized, err := validateSetMode(mode)
	if err != nil {
		return store.Thread{}, err
	}
	current, err := a.store.GetThread(threadID)
	if err != nil {
		return store.Thread{}, err
	}
	// Thread *type* is immutable: design and discussion threads stay in
	// their respective UX shells for life. The agent-mode toggle only swaps
	// chat ↔ plan within a chat thread.
	if _, ok := modesForPostCreation[strings.TrimSpace(current.Mode)]; !ok {
		return store.Thread{}, fmt.Errorf("cannot change mode of %q thread (immutable thread type)", current.Mode)
	}
	if err := a.store.UpdateMode(threadID, normalized); err != nil {
		return store.Thread{}, err
	}

	needsReconnect := false
	a.mu.Lock()
	sess, sessionActive := a.sessions[threadID]
	a.mu.Unlock()
	if sessionActive {
		needsReconnect = a.applyActiveModeChange(threadID, sess, provider.NormalizeInteractionMode(normalized))
	}
	a.emitEvent("thread:mode_changed", ThreadModeChangedEvent{
		ThreadID:       threadID,
		Mode:           normalized,
		NeedsReconnect: needsReconnect,
	})

	return a.store.GetThread(threadID)
}

func (a *App) applyActiveModeChange(threadID string, sess session, mode provider.InteractionMode) bool {
	switch mode {
	case provider.ModeChat, provider.ModePlan:
		if sess.claude != nil {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := sess.claude.SetInteractionMode(ctx, mode); err != nil {
				log.Printf("thread %s: apply active Claude mode %q failed: %v", threadID, mode, err)
				return true
			}
		}
		// Codex cannot mutate the active turn's collaboration mode, but the
		// persisted mode is read on the next turn/start. No reconnect is needed.
		return false
	default:
		log.Printf("thread %s: mode changed to %q while session is active; reconnect required", threadID, mode)
		return true
	}
}
