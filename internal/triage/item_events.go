package triage

import "agent-overflow/internal/store"

const (
	itemStreamActionUpsert = "upsert"
	itemStreamActionDelta  = "delta"
)

type ItemStreamEvent struct {
	Action    string      `json:"action"`
	ThreadID  string      `json:"threadId"`
	Item      *store.Item `json:"item,omitempty"`
	ItemID    string      `json:"itemId,omitempty"`
	Kind      string      `json:"kind,omitempty"`
	Delta     string      `json:"delta,omitempty"`
	UpdatedAt int64       `json:"updatedAt,omitempty"`
}

func NewItemStreamUpsert(item store.Item) ItemStreamEvent {
	return ItemStreamEvent{
		Action:   itemStreamActionUpsert,
		ThreadID: item.ThreadID,
		Item:     &item,
	}
}

func newItemStreamDelta(evt ItemDeltaEvent) ItemStreamEvent {
	return ItemStreamEvent{
		Action:    itemStreamActionDelta,
		ThreadID:  evt.ThreadID,
		ItemID:    evt.ItemID,
		Kind:      evt.Kind,
		Delta:     evt.Delta,
		UpdatedAt: evt.UpdatedAt,
	}
}
