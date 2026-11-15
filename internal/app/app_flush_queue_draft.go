package app

import (
	"time"

	"agent-overflow/internal/composerdraft"
)

// restoreQueuedDraft returns undelivered messages to the composer: parts
// merged ahead of what it holds, the queue rows queueIDs deleted, and the
// rows retire the messages already have taken back with the review comments
// they claimed, all in one store transaction. A failure changes nothing.
func (a *App) restoreQueuedDraft(threadID string, parts []composerdraft.Part, queueIDs, retire []string) error {
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
	return a.writeRestoredQueueDraft(current, merged, queueIDs, retire)
}
