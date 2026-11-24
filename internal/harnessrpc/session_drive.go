package harnessrpc

import (
	"context"
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

// HarnessSendResult is what one HarnessSend did.
type HarnessSendResult struct {
	// Queued is whether the message waits behind the thread's work. It is
	// never set for a send admitted only at rest.
	Queued bool `json:"queued"`
	// TurnIndex is the turn the message's row was placed in. Zero when
	// Queued.
	TurnIndex int `json:"turnIndex"`
	// CompletionsAfter is set only for a send admitted only at rest: the
	// provider:turn_completed sequence read under the thread's action lock
	// once the send was admitted (harnessHost.Send in internal/app holds the
	// contract). A wait for the turn's completion skips anything at or
	// below it.
	CompletionsAfter uint64 `json:"completionsAfter,omitempty"`
}

// HarnessSend sends text as the page's default send does, for the harness
// CLI and the specs' drivers. With atRestOnly a thread that is working (a
// turn open, a message waiting for its turn to start, or a queued message
// not yet written) refuses the send and nothing is recorded, decided under
// the thread's action lock with the admission every send passes; the
// answer then names the turn the message's row was placed in. Without it
// the message queues behind that work, as the composer's does.
func (h *Harness) HarnessSend(ctx context.Context, threadID, text string, atRestOnly bool) (HarnessSendResult, error) {
	if h.config.Host == nil {
		return HarnessSendResult{}, fmt.Errorf("harness host unavailable")
	}
	return h.config.Host.Send(ctx, threadID, text, atRestOnly)
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
