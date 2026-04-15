package main

import (
	"database/sql"
	"errors"

	"agent-overflow/internal/store"
)

func (a *App) deleteThreadTree(threadID string) error {
	thread, threadErr := a.store.GetThread(threadID)
	if threadErr != nil && !errors.Is(threadErr, sql.ErrNoRows) {
		return threadErr
	}

	children, err := a.store.ListChildThreads(threadID)
	if err != nil {
		return err
	}
	for _, child := range children {
		if err := a.deleteThreadTree(child.ID); err != nil {
			return err
		}
	}

	if err := a.stopSession(threadID); err != nil {
		return err
	}
	a.clearThreadSystemPrompt(threadID)
	a.removeDeliberation(thread)

	return a.store.DeleteThread(threadID)
}

func (a *App) removeDeliberation(thread store.Thread) {
	if thread.DiscussionID == "" || thread.ParentThreadID != "" {
		return
	}

	a.mu.Lock()
	delete(a.deliberations, thread.DiscussionID)
	a.mu.Unlock()
}
