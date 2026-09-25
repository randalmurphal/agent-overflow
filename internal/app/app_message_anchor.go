package app

import (
	"log"
	"time"

	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/store"
	"agent-overflow/internal/usermessage"
)

// recordMessageAnchor persists the provider-correlation anchor for a
// just-persisted real user message. Runs synchronously at the send /
// flush-confirmed sites — it is one SQLite upsert. Failure is logged,
// not surfaced: the message itself is already durable, and a missing
// anchor only degrades the later fork / revert-on-interrupt slice to
// the item-meta fallback candidates (resolveMessageAnchor synthesizes
// one from the item row when no anchor exists).
func (a *App) recordMessageAnchor(userItem store.Item) {
	a.emit(eventchan.ProviderAsyncQuestionsChanged, map[string]string{"threadId": userItem.ThreadID})
	anchor := store.MessageAnchor{
		ThreadID:   userItem.ThreadID,
		UserItemID: userItem.ID,
		TurnIndex:  userItem.TurnIndex,
		// Mirror the row's provider ids onto the anchor at record time.
		// For a direct send the row meta already carries the minted send
		// uuid (app_send.go stamps it before this call), so the anchor
		// is slice-ready before Claude's replay echo. A row confirmed by
		// its echo also carries the parent uuid (stamped in the same tx
		// as the item id, round-5 R5-8). Empty when the meta has none
		// yet (eager-persist-on-interrupt rows); the echo then fills
		// both via UpdateMessageAnchorProviderIDs as before.
		ProviderUserMessageID: usermessage.ReadProviderItemID(userItem.Meta),
		ProviderParentUUID:    usermessage.ReadProviderParentUUID(userItem.Meta),
		CreatedAt:             time.Now().UnixMilli(),
	}
	if err := a.store.UpsertMessageAnchor(anchor); err != nil {
		log.Printf("message anchor: record %s/%s: %v", userItem.ThreadID, userItem.ID, err)
	}
}

// resolveMessageAnchor returns the persisted message anchor for the
// user item, or a synthesized record built from the item row when the
// at-send record didn't land (record error, legacy row) or its turn
// index drifted from the item's. The Claude rollback/fork paths key on
// `ProviderUserMessageID` when available so the slice point is immune
// to synthetic-entry ordinal drift; populating it on the synthesized
// record means an anchor-less row also benefits from the structural
// fix. op labels log lines only.
func (a *App) resolveMessageAnchor(op string, threadID string, userItem store.Item) store.MessageAnchor {
	if anchor, ok, err := a.store.GetMessageAnchor(threadID, userItem.ID); err == nil && ok {
		if anchor.TurnIndex == userItem.TurnIndex {
			return anchor
		}
		log.Printf("app: %s: anchor turn index %d does not match user item turn index %d; synthesizing", op, anchor.TurnIndex, userItem.TurnIndex)
	} else if err != nil {
		log.Printf("app: %s: load message anchor: %v", op, err)
	}
	return store.MessageAnchor{
		ThreadID:              threadID,
		UserItemID:            userItem.ID,
		TurnIndex:             userItem.TurnIndex,
		ProviderUserMessageID: usermessage.ReadProviderItemID(userItem.Meta),
		ProviderParentUUID:    usermessage.ReadProviderParentUUID(userItem.Meta),
	}
}
