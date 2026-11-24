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

// HarnessListItems answers every item of a thread in order: the assertion
// read for a spec or `ao-harness items`. A page reads bounded slices
// (ListThreadSliceAround, ListItemsBeforeCursor, ListItemsAfterCursor)
// instead. inlinePreviews asks for complete payload metadata rather than
// the wire projection's elided copy.
func (h *Harness) HarnessListItems(threadID string, inlinePreviews bool) ([]store.Item, error) {
	if h.config.Host == nil {
		return nil, fmt.Errorf("harness host unavailable")
	}
	return h.config.Host.ListItems(threadID, inlinePreviews)
}
