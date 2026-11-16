package app

import (
	"time"

	"agent-overflow/internal/composerdraft"
	"agent-overflow/internal/store"
)

// restoreQueuedDraft returns undelivered messages to the composer: parts
// merged ahead of what it holds, and everything else restore names (the
// queue rows to delete, the rows to take back) in one store transaction. A
// failure changes nothing. Every path that returns a queued or in-flight
// message to the composer goes through here; restore's Expected and Merged
// are set here. Parts are the person's text only: what the app wrote never
// enters the composer (agentPartsNotTakenIn).
func (a *App) restoreQueuedDraft(threadID string, parts []composerdraft.Part, restore store.QueueDraftRestore) error {
	current, _, err := a.store.GetThreadDraft(threadID)
	if err != nil {
		return err
	}
	merged := current
	if len(parts) > 0 {
		if merged, err = composerdraft.MergeParts(threadID, current, parts, time.Now().UnixMilli()); err != nil {
			return err
		}
	}
	restore.Expected, restore.Merged = current, merged
	return a.writeRestoredQueueDraft(restore)
}
