package harnessrpc

import (
	"fmt"

	"agent-overflow/internal/store"
)

// HarnessStartSession brings a thread's provider session up without sending
// anything, for a spec that needs a live session before its first message.
// No page starts a session this way: a send starts one by itself. It runs
// the production start (Host.StartSession) and answers its error.
func (h *Harness) HarnessStartSession(threadID string) error {
	if h.config.Host == nil {
		return fmt.Errorf("harness host unavailable")
	}
	return h.config.Host.StartSession(threadID)
}

// HarnessListItems answers a thread's items in order, the assertion read
// for a spec or `ao-harness items`: the newest rows that fit the response
// budget, projected as the paged loads project them, so a long thread
// answers its tail only. A page reads bounded slices
// (ListThreadSliceAround, ListItemsBeforeCursor, ListItemsAfterCursor)
// instead. inlinePreviews keeps payload previews inline where the
// projection's bounds allow.
func (h *Harness) HarnessListItems(threadID string, inlinePreviews bool) ([]store.Item, error) {
	if h.config.Host == nil {
		return nil, fmt.Errorf("harness host unavailable")
	}
	return h.config.Host.ListItems(threadID, inlinePreviews)
}
