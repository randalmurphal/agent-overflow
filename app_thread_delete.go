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
	threadFound := threadErr == nil

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

	// Thread was already gone (e.g. partially deleted earlier). Children and
	// session cleanup above are still necessary, but there is nothing left to
	// delete from the store.
	if !threadFound {
		return nil
	}

	return a.store.DeleteThread(threadID)
}

func (a *App) removeDeliberation(thread store.Thread) {
	if thread.DiscussionID == "" || thread.ParentThreadID != "" {
		return
	}

	a.removeDeliberationByID(thread.DiscussionID)
}

func (a *App) removeDeliberationByID(channelID string) {
	if channelID == "" {
		return
	}
	a.mu.Lock()
	delete(a.deliberations, channelID)
	a.mu.Unlock()
}
