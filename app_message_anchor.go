package main

import (
	"log"
	"time"

	"agent-overflow/internal/store"
	"agent-overflow/internal/usermessage"
)

// recordMessageAnchor persists the provider-correlation anchor for a
// just-persisted real user message. Runs synchronously at the send /
// flush-confirmed sites — it is one SQLite upsert. Failure is logged,
// not surfaced: the message itself is already durable, and a missing
// anchor only degrades the later fork / revert-on-interrupt slice to
// the item-meta fallback candidates (resolveRollbackAnchor and
// ForkThreadFromMessage both synthesize from the item row when no
// anchor exists).
func (a *App) recordMessageAnchor(userItem store.Item) {
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
