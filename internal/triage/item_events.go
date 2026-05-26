package triage

import "agent-overflow/internal/store"

const (
	itemStreamActionUpsert = "upsert"
	itemStreamActionDelta  = "delta"
	// itemStreamActionMeta carries a re-validated `meta` blob for a
	// row that already exists on the frontend. Used today to push
	// fresh `pathRefs` allowlists onto in-flight assistant_text rows
	// so links can render mid-stream instead of only at settle.
	// Frontend consumers preserve delta ordering: any pending deltas
	// for the same row flush BEFORE the meta replaces the row, so
	// the meta lands against text the user has already seen.
	itemStreamActionMeta  = "meta"
	itemStreamActionPatch = "patch"
)

// ItemPatchFields carries the mutable subset of an Item for a patch event.
// Non-nil pointer fields mean "set to this value"; nil means "unchanged".
type ItemPatchFields struct {
	Status    *string `json:"status,omitempty"`
	Summary   *string `json:"summary,omitempty"`
	Meta      *string `json:"meta,omitempty"`
	Decision  *string `json:"decision,omitempty"`
	UpdatedAt *int64  `json:"updatedAt,omitempty"`
}

type ItemStreamEvent struct {
	Action           string           `json:"action"`
	ThreadID         string           `json:"threadId"`
	Item             *store.Item      `json:"item,omitempty"`
	CountsAsActivity *bool            `json:"countsAsActivity,omitempty"`
	ItemID           string           `json:"itemId,omitempty"`
	Kind             string           `json:"kind,omitempty"`
	Delta            string           `json:"delta,omitempty"`
	Meta             string           `json:"meta,omitempty"`
	Patch            *ItemPatchFields `json:"patch,omitempty"`
	UpdatedAt        int64            `json:"updatedAt,omitempty"`
}

func NewItemStreamUpsert(item store.Item) ItemStreamEvent {
	return NewItemStreamUpsertWithActivity(item, nil)
}

func NewItemStreamUpsertWithActivity(item store.Item, countsAsActivity *bool) ItemStreamEvent {
	return ItemStreamEvent{
		Action:           itemStreamActionUpsert,
		ThreadID:         item.ThreadID,
		Item:             &item,
		CountsAsActivity: countsAsActivity,
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

func newItemStreamMeta(threadID, itemID, kind, meta string, updatedAt int64) ItemStreamEvent {
	return ItemStreamEvent{
		Action:    itemStreamActionMeta,
		ThreadID:  threadID,
		ItemID:    itemID,
		Kind:      kind,
		Meta:      meta,
		UpdatedAt: updatedAt,
	}
}

func newItemStreamPatch(threadID, itemID, kind string, patch ItemPatchFields) ItemStreamEvent {
	return ItemStreamEvent{
		Action:   itemStreamActionPatch,
		ThreadID: threadID,
		ItemID:   itemID,
		Kind:     kind,
		Patch:    &patch,
	}
}
