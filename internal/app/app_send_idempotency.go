package app

import (
	"context"
	"fmt"

	"agent-overflow/internal/keyedlock"

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
// durable identity alongside the message. Between dispatch and echo, the
// existing pending correlation entry also answers retries; it is session
// coordination, not crash recovery. Provider transcripts own recovery of
// consumed messages. The retained history half is matched with
// `json_extract` inside the store, not by decoding rows here.

// recordedSend is where a repeated send id was found. Exactly one half is
// set: the message is either still waiting on the queue, or it has been
// dispatched (persisted or awaiting its echo).
type recordedSend struct {
	// item is the persisted or pending `user_text` the first frame produced.
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
	var item store.Item
	var row store.FlushQueueItem
	var found bool
	var err error
	if a.triage != nil {
		item, row, found, err = a.triage.FindAcceptedUserMessageBySendID(threadID, sendID)
	} else {
		item, found, err = a.store.FindUserTextItemBySendID(threadID, sendID)
		if err == nil && !found {
			row, found, err = a.store.FindFlushQueueItemBySendID(threadID, sendID)
		}
	}
	if err != nil {
		return recordedSend{}, false, fmt.Errorf("accepted user message: %w", err)
	}
	if found {
		return recordedSend{item: item, queued: row, dispatched: item.ID != ""}, true, nil
	}
	return recordedSend{}, false, nil
}

// Public send wrappers take this before action/mutation locks. Only attempts
// for the same message wait; typing another queued message remains responsive
// during provider IO. Internal redirects never reacquire the admission lock.
func (a *App) lockSendAdmission(ctx context.Context, threadID, sendID string) (func(), error) {
	if sendID == "" {
		return func() {}, nil
	}
	return a.sendAdmissionLocks().LockCtx(ctx, sendAdmissionKey(threadID, sendID))
}

func sendAdmissionKey(threadID, sendID string) string {
	return fmt.Sprintf("%d:%s%s", len(threadID), threadID, sendID)
}

func (a *App) sendAdmissionLocks() *keyedlock.Registry {
	a.sendAdmissionsOnce.Do(func() { a.sendAdmissions = keyedlock.New() })
	return a.sendAdmissions
}
