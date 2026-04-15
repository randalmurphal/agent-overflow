package main

import (
	"fmt"

	"agent-overflow/internal/provider"
)

// RespondToApproval forwards an interactive response to the active provider session.
func (a *App) RespondToApproval(threadID string, response provider.ApprovalResponse) error {
	a.mu.Lock()
	sess, ok := a.sessions[threadID]
	a.mu.Unlock()
	if !ok {
		return fmt.Errorf("no active session for thread %s", threadID)
	}

	switch {
	case sess.claude != nil:
		return sess.claude.RespondToApproval(a.ctx, response)
	case sess.codex != nil:
		return sess.codex.RespondToApproval(a.ctx, response)
	default:
		return fmt.Errorf("session has no provider")
	}
}
