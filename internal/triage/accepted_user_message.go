package triage

import (
	"agent-overflow/internal/store"
	"agent-overflow/internal/usermessage"
)

// FindAcceptedUserMessageBySendID spans persisted history and the bounded
// correlation entries for dispatched input. A successful provider write can
// remove the durable queue before its echo creates the history row. Hold the
// same anchor as echo pop+persist so a retry cannot fall between those homes.
func (r *Router) FindAcceptedUserMessageBySendID(threadID, sendID string) (store.Item, store.FlushQueueItem, bool, error) {
	if sendID == "" {
		return store.Item{}, store.FlushQueueItem{}, false, nil
	}
	anchor := r.flushAnchor(threadID)
	anchor.Lock()
	defer anchor.Unlock()
	item, found, err := r.store.FindUserTextItemBySendID(threadID, sendID)
	if err != nil || found {
		return item, store.FlushQueueItem{}, found, err
	}
	queued, found, err := r.store.FindFlushQueueItemBySendID(threadID, sendID)
	if err != nil || found {
		return store.Item{}, queued, found, err
	}
	r.mu.Lock()
	var retained []store.Item
	for _, pending := range r.pendingSendsLocked(threadID) {
		candidate := pending.DeferredItem
		if candidate == nil {
			candidate = pending.QuietItem
		}
		if candidate != nil {
			retained = append(retained, *candidate)
		}
	}
	r.mu.Unlock()
	for _, candidate := range retained {
		meta, err := usermessage.FromItem(candidate)
		if err != nil {
			return store.Item{}, store.FlushQueueItem{}, false, err
		}
		if meta.SendID == sendID {
			return candidate, store.FlushQueueItem{}, true, nil
		}
	}
	return store.Item{}, store.FlushQueueItem{}, false, nil
}
