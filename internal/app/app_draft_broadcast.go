package app

import (
	"context"
	"time"

	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/store"
	"agent-overflow/internal/transport"
)

// The `draft:updated` broadcast chokepoint.
//
// Every persisted composer-draft write goes through writeThreadDraft /
// removeThreadDraft, and nothing else may touch store.UpsertThreadDraft or
// store.DeleteThreadDraft — TestOnlyTheDraftHelpersWriteDrafts holds that.
// Threading the emit through one pair of helpers rather than a dozen call
// sites is the difference between "drafts converge" and "drafts converge
// except after a revert, a queue flush, or a fork".
//
// Three rules:
//
//   - The emit rides the PERSIST, never the keystroke. The composer debounces
//     its saves; this layer never sees a character.
//   - A write that changed nothing emits nothing. That matters more here than
//     anywhere else in the app: the composer autosaves, so a buffer nobody
//     touched would otherwise wake every attached client on a timer.
//   - The frame carries the thread, the timestamp and WHO wrote it — never the
//     draft text. Receivers re-read through GetDraft, which takes
//     `threads:operate` for the disclosure reason; a push carrying the text
//     would be the way around the grant that read enforces.
//
// Last write wins, and the losing client finds out: there is no merge here and
// no lock. Two screens typing into one composer is a state the app can report
// honestly (the newer save is what the thread has) but cannot reconcile,
// because the text has no structure to merge on.

// DraftUpdatedEvent is the wire shape for draft:updated: "this thread's draft
// row moved at this time, and here is who moved it".
//
// A deletion is not distinguished from an edit. Both mean "re-read", and the
// re-read answers both the same way: a thread whose draft was cleared and a
// thread that never had one are the same state, and the composer renders both
// as empty. A discriminator would be a field with nothing to decide.
//
// DeviceID and ConnectionID name the screen that wrote it, and are empty when
// the backend wrote it itself — a saga restoring a draft, a queue dispatch
// consuming one. ConnectionID is the echo-suppression key, because it is
// unique per page load; DeviceID is the durable one, carried for a future
// "edited on <device>" affordance and deliberately NOT used for suppression
// (two tabs of one browser share it, and each would then sit on the other's
// stale text).
type DraftUpdatedEvent struct {
	ThreadID     string `json:"threadId"`
	UpdatedAt    int64  `json:"updatedAt"`
	DeviceID     string `json:"deviceId,omitempty"`
	ConnectionID string `json:"connectionId,omitempty"`
}

// clientOf is the identity of the screen an RPC came from. The zero value is
// a normal answer: in-process bindings, background sagas and tests have no
// screen behind them, and the frames they produce are applied by everyone.
func clientOf(ctx context.Context) transport.ClientIdentity {
	return transport.ClientFromContext(ctx)
}

// writeThreadDraft persists a draft and announces it if it moved.
func (a *App) writeThreadDraft(who transport.ClientIdentity, draft store.ThreadDraft) error {
	changed, err := a.store.UpsertThreadDraft(draft)
	if err != nil {
		return err
	}
	if !changed {
		return nil
	}
	a.broadcastDraft(who, DraftUpdatedEvent{
		ThreadID:  draft.ThreadID,
		UpdatedAt: draft.UpdatedAt,
	})
	return nil
}

// removeThreadDraft deletes a draft and announces it if there was one.
func (a *App) removeThreadDraft(who transport.ClientIdentity, threadID string) error {
	deleted, err := a.store.DeleteThreadDraft(threadID)
	if err != nil {
		return err
	}
	if !deleted {
		return nil
	}
	// The row is gone and its stored timestamp went with it, so the frame
	// carries the moment of the delete.
	a.broadcastDraft(who, DraftUpdatedEvent{ThreadID: threadID, UpdatedAt: time.Now().UnixMilli()})
	return nil
}

func (a *App) broadcastDraft(who transport.ClientIdentity, evt DraftUpdatedEvent) {
	evt.DeviceID = who.DeviceID
	evt.ConnectionID = who.ConnectionID
	a.emitEvent(eventchan.DraftUpdated, evt)
}
