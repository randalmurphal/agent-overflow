package main

import (
	"context"
	"database/sql"
	"errors"
	"log"

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
	if a.terminals != nil {
		if err := a.terminals.CloseThread(threadID); err != nil {
			return err
		}
	}
	a.clearThreadSystemPrompt(threadID)
	a.removeDeliberation(thread)
	a.cleanupThreadCheckpoints(threadID, thread, threadFound)
	a.cleanupThreadAttachmentFiles(threadID)

	// Thread was already gone (e.g. partially deleted earlier). Children and
	// session cleanup above are still necessary, but there is nothing left to
	// delete from the store.
	if !threadFound {
		return nil
	}

	return a.store.DeleteThread(threadID)
}

// cleanupThreadAttachmentFiles removes the on-disk attachment directory for a
// thread. The SQLite FK cascade already drops the metadata rows; this sweeps
// the bytes. Errors are logged, not returned, because attachment cleanup must
// not block thread deletion — a residual directory is cosmetic, not a data
// integrity problem.
func (a *App) cleanupThreadAttachmentFiles(threadID string) {
	if a.attachments == nil {
		return
	}
	if err := a.attachments.DeleteThreadDir(threadID); err != nil {
		log.Printf("delete thread: remove attachment files for %s: %v", threadID, err)
	}
}

// cleanupThreadCheckpoints removes both the Git refs and the SQLite
// bookkeeping rows for a thread's checkpoints. Errors are logged but NOT
// returned so a checkpoint-cleanup failure doesn't block thread deletion;
// the orphan refs will be garbage collected eventually via git gc.
func (a *App) cleanupThreadCheckpoints(threadID string, thread store.Thread, threadFound bool) {
	if threadFound {
		workspace := checkpointWorkspaceForThread(thread)
		if workspace != "" {
			if err := a.checkpointStore().CleanupThread(context.Background(), workspace, threadID); err != nil {
				log.Printf("delete thread: cleanup checkpoint refs for %s: %v", threadID, err)
			}
		}
	}
	if a.store != nil {
		if err := a.store.DeleteCheckpointsForThread(threadID); err != nil {
			log.Printf("delete thread: drop checkpoint rows for %s: %v", threadID, err)
		}
	}
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
