package app

import (
	"time"

	"agent-overflow/internal/composerdraft"
)

// restoreQueuedDraft merges parts ahead of the composer and retires the queue
// rows queueIDs in one write. sendIDs are the send ids of the messages the
// parts carry; a thread request whose wake is among them reads as delivered
// to the draft from that same write. Every path that returns a queued or
// in-flight message to the composer goes through here.
func (a *App) restoreQueuedDraft(threadID string, parts []composerdraft.Part, queueIDs, sendIDs []string) error {
	current, _, err := a.store.GetThreadDraft(threadID)
	if err != nil {
		return err
	}
	merged, err := composerdraft.MergeParts(threadID, current, parts, time.Now().UnixMilli())
	if err != nil {
		return err
	}
	return a.writeRestoredQueueDraft(current, merged, queueIDs, sendIDs)
}
