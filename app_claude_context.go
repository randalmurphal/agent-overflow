package main

import (
	"context"
	"fmt"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
	"agent-overflow/internal/triage"
)

func (a *App) ensureClaudeContextReadyForUserSendLocked(thread store.Thread) error {
	if thread.Provider != string(provider.Claude) {
		return nil
	}
	sess, ok := a.sessionManager().get(thread.ID)
	if !ok || sess.claude == nil {
		return nil
	}
	if !sess.claude.RequiresResumeAtBeforeUserSend() {
		return nil
	}
	resumeAt := sess.claude.CanonicalLeafUUID()
	if resumeAt == "" {
		return fmt.Errorf("Claude session context needs repair before sending, but the canonical transcript leaf is not known")
	}
	if hasBackground, err := a.store.HasLiveBackgroundToolCall(thread.ID); err != nil {
		return fmt.Errorf("check Claude background work before context repair: %w", err)
	} else if hasBackground {
		return fmt.Errorf("Claude session context needs repair before sending, but live background work is still running")
	}

	if thread.SessionRef == "" {
		sessionID := sess.claude.SessionID()
		if sessionID == "" {
			return fmt.Errorf("Claude session context needs repair before sending, but the live session id is not known")
		}
		changed, err := a.store.UpdateSessionRef(thread.ID, sessionID)
		if err != nil {
			return fmt.Errorf("record Claude session ref before context repair: %w", err)
		}
		if changed {
			a.emitEvent("thread:updated", triage.ThreadUpdateEvent{
				Action:     "patch",
				ID:         thread.ID,
				SessionRef: &sessionID,
			})
		}
	}
	if err := a.runSessionStart(context.Background(), thread.ID, func() error {
		return a.startSessionNowWithClaudeResumeAt(thread.ID, resumeAt)
	}); err != nil {
		return fmt.Errorf("repair Claude session context: %w", err)
	}
	return nil
}
