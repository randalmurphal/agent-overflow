package app

import (
	"time"

	"agent-overflow/internal/composerdraft"
)

func (a *App) restoreQueuedDraft(threadID string, parts []composerdraft.Part, queueIDs []string) error {
	current, _, err := a.store.GetThreadDraft(threadID)
	if err != nil {
		return err
	}
	merged, err := composerdraft.MergeParts(threadID, current, parts, time.Now().UnixMilli())
	if err != nil {
		return err
	}
	return a.writeRestoredQueueDraft(current, merged, queueIDs)
}
