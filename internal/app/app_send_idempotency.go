package app

import (
	"fmt"

	"agent-overflow/internal/store"
)

// --- Send idempotency ---
//
// A socket that dies after the send frame reached this backend looks, from
// the page, exactly like one that died before: the RPC never answers, the
// composer puts the text back, and the person presses Send again. That is
// how one message becomes two turns.
//
// So one composer send carries one client-minted id
// (`frontend/src/lib/utils/sendOptions.ts`), identically on
// SendMessageWithOptions and RegisterQueueItem, and a repeated arrival is
// answered from what the FIRST one left behind instead of starting anything.
//
// THE MESSAGE ITSELF IS THE RECORD. There is no id table, and that is the
// whole design: the id lands on the dispatched `user_text` row's meta and on
// the durable queue row while the message waits, so there is exactly one
// source of truth, it survives a backend restart with the message it
// describes, and nothing has to expire — the record ages out when the
// message it belongs to does. The meta half is matched with
// `json_extract` inside the store, not by decoding rows here.

// recentSendIDWindow bounds the lookup: the newest N user messages of the
// thread. A retry follows its own failed frame by seconds, so anything
// further back cannot be the frame being retried, and a bounded window is
// what keeps this off the send path's critical section without an index over
// a column whose common value is empty. The MATCH inside the window is made
// in SQL (store.FindUserTextItemBySendID), so the common answer — no repeat —
// hydrates and decodes nothing.
const recentSendIDWindow = 64

// recordedSend is where a repeated send id was found. Exactly one half is
// set: the message is either still waiting on the queue, or it has been
// dispatched and persisted.
type recordedSend struct {
	// item is the persisted `user_text` row the first frame produced.
	item store.Item
	// queued is the durable queue row the first frame produced.
	queued store.FlushQueueItem
	// dispatched says which half to read. It is a field rather than an
	// emptiness test on the two rows so a caller cannot mistake a
	// zero-valued row for the other case.
	dispatched bool
}

// findRecordedSend answers whether this thread has already accepted the given
// client-minted send id, and what it did with it.
//
// An empty id is never a match: every app-internal injector leaves it unset,
// as does any client bundle older than the field, and matching those against
// each other would collapse unrelated messages into one.
//
// A store failure is returned rather than read as "no record". The two reads
// are local SQLite over the same thread the caller is about to write to, so a
// failure here is a failure the send would hit a step later anyway — and
// treating it as "no record" would turn the one condition this function
// exists for into a duplicate turn.
func (a *App) findRecordedSend(threadID, sendID string) (recordedSend, bool, error) {
	if sendID == "" {
		return recordedSend{}, false, nil
	}
	item, found, err := a.store.FindUserTextItemBySendID(threadID, sendID, recentSendIDWindow)
	if err != nil {
		return recordedSend{}, false, fmt.Errorf("recent user messages: %w", err)
	}
	if found {
		return recordedSend{item: item, dispatched: true}, true, nil
	}
	rows, err := a.store.ListFlushQueueItems(threadID)
	if err != nil {
		return recordedSend{}, false, fmt.Errorf("queued messages: %w", err)
	}
	for _, row := range rows {
		if row.SendID == sendID {
			return recordedSend{queued: row}, true, nil
		}
	}
	return recordedSend{}, false, nil
}
