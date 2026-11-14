package triage

import (
	"encoding/json"
	"fmt"
	"log"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
)

// narration.go stores a thinking block the provider marked as prose for
// the user (provider.MetaNarrationKey) as assistant text.
//
// The marker arrives with the block's end, so a streamed block is a
// thinking row until its stop and becomes assistant text at its settle;
// a block that arrives complete is written as assistant text directly.
// Either way the row keeps the thinking row's identity: its item id, its
// payload id, its position and its place in the thinking block counter.
// A re-assert of the block therefore names the same row whether or not it
// was converted, and an import of the session produces the same ids.
//
// The row is assistant text in every other respect: the summary is the
// whole text, the payload has the assistant text shape, and the meta
// carries the path refs and code spans of a settled prose row beside
// `"narration": true`.
//
// A block that ends without its stop (an interrupt, an error, a scope
// settle at a timeline boundary, an agent's end) stays a thinking row: the
// marker never arrived.

// narrationItemMeta is the item meta key a narration row carries.
var narrationItemMeta = json.RawMessage(`{"` + provider.MetaNarrationKey + `":true}`)

// itemIsNarration reports whether a stored row carries the narration key.
func itemIsNarration(item store.Item) bool {
	return item.Kind == itemKindAssistantText && eventMetaBool(json.RawMessage(item.Meta), provider.MetaNarrationKey)
}

// narrationRow rewrites a thinking row's fields as the assistant text row
// of the same block. item.Meta must already hold what the row keeps.
func (r *Router) narrationRow(item store.Item, content string, now int64) (store.Item, store.Payload) {
	item.Kind = itemKindAssistantText
	item.Role = "assistant"
	item.Status = statusCompleted
	item.Summary = content
	item.UpdatedAt = now
	r.stampSettledProse(&item, content, narrationItemMeta)
	return item, prosePayload(item.PayloadID, content, now)
}

// settleNarrationRow settles a streaming thinking row whose stop carried
// the narration marker. item is the row as just read, with its stream
// flushed. The whole text is the stop's content when it carried any and
// the flushed payload otherwise: a thinking row's summary is only its
// tail.
//
// The row and its payload are rewritten in one store write that leaves a
// row another write already ended as that write left it (the guarantee
// persistSettle gives a thinking settle), and the stored row goes out as
// one upsert. A patch cannot carry the change: it has no kind.
func (r *Router) settleNarrationRow(item store.Item, finalContent string, finalContentPresent bool) error {
	content := finalContent
	if !finalContentPresent {
		data, err := r.store.GetPayloadData(item.ThreadID, item.PayloadID)
		if err != nil {
			return fmt.Errorf("narration settle read payload %s: %w", item.PayloadID, err)
		}
		content = string(data)
	}
	row, payload := r.narrationRow(item, content, time.Now().UnixMilli())
	var persisted store.Item
	err := r.withSubagentCard(row.ThreadID, row.ParentID, func(card *store.SubagentCard) error {
		row.SubagentCard = card
		var err error
		persisted, err = r.store.UpsertUnsettledItem(row, &payload, nil)
		return err
	})
	if err != nil {
		return fmt.Errorf("narration settle %s: %w", item.ID, err)
	}
	if !itemIsNarration(persisted) {
		// Another write ended the row first; it stays as that write left it.
		return nil
	}
	r.emitItemUpsert(persisted)
	return nil
}

// persistCompletedNarrationItem writes a complete narration block that
// has no row yet, under the thinking item id it would have had. It emits
// as a completed text block does: a top-level block streams its wire
// projection, a scoped block is one settled upsert
// (persistCompletedTextItem).
func (r *Router) persistCompletedNarrationItem(threadID string, turnIndex int, scope, providerItemID, itemID, content string, now int64) error {
	item, payload := r.narrationRow(store.Item{
		ID:        itemID,
		ThreadID:  threadID,
		TurnIndex: turnIndex,
		PayloadID: ThinkingPayloadID(itemID),
		ParentID:  scope,
		Meta:      providerItemMeta(providerItemID),
		CreatedAt: now,
	}, content, now)
	if scope == "" {
		log.Printf("triage: recovered never-streamed narration block %s on thread %s (%d bytes)", itemID, threadID, len(content))
		return r.persistCompletedBlockEmitStreaming(item, &payload, content)
	}
	return r.persistItem(item, &payload)
}

// reassertNarrationItem handles a complete thinking block whose row is
// already assistant text: a repeat of the block names the row by the same
// provider item id. It reports whether such a row exists. The same text
// changes nothing; different text is a completed-history conflict, as it
// is for a thinking row. The check runs for an unmarked block as well, so
// a repeat that lost the marker cannot write a second row or, in a scope,
// overwrite the narration row under their shared item id.
func (r *Router) reassertNarrationItem(threadID string, turnIndex int, scope, providerItemID, content string) (bool, error) {
	item, found, err := r.store.FindStreamItemByProviderItemID(threadID, turnIndex, itemKindAssistantText, scope, providerItemID)
	if err != nil || !found || !itemIsNarration(item) {
		return false, err
	}
	if item.Status == statusCompleted && item.Summary != content {
		return true, fmt.Errorf("provider attempted to replace completed thinking item %s", item.ID)
	}
	return true, nil
}
