package main

import (
	"context"
	"fmt"
	"log"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
	"agent-overflow/internal/threadmode"
)

// applyActiveModeTimeout bounds the synchronous Claude
// `set_permission_mode` RPC we send when the user flips chat/plan
// while a session is live. Short — the RPC is a state nudge with no
// per-server fan-out — but generous enough to survive a momentary
// stall before we mark the session reconnect-required.
const applyActiveModeTimeout = 5 * time.Second

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

// UpdateThreadMode updates the thread's mode and returns the refreshed
// row. Rejects unknown modes with a clear error message so the frontend
// can surface it verbatim.
//
// Active Claude sessions apply chat/plan changes immediately through
// set_permission_mode. Codex reads the persisted mode on the next turn/start.
// Modes that cannot be applied to the live session emit NeedsReconnect=true.
// The persisted row is always updated regardless.
func (a *App) UpdateThreadMode(threadID string, mode string) (store.Thread, error) {
	normalized, err := threadmode.ValidateSet(mode)
	if err != nil {
		return store.Thread{}, err
	}
	current, err := a.store.GetThread(threadID)
	if err != nil {
		return store.Thread{}, err
	}
	// Thread *type* is immutable: design, discussion, and workflow threads
	// stay in their respective owning UX/engine paths for life. The agent-mode
	// toggle only swaps chat ↔ plan within a chat thread.
	if !threadmode.IsPostCreationMode(current.Mode) {
		return store.Thread{}, fmt.Errorf("cannot change mode of %q thread (immutable thread type)", current.Mode)
	}
	if err := a.store.UpdateMode(threadID, normalized); err != nil {
		return store.Thread{}, err
	}

	needsReconnect := false
	sess, sessionActive := a.sessionManager().get(threadID)
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
			ctx, cancel := context.WithTimeout(context.Background(), applyActiveModeTimeout)
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
