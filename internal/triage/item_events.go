package triage

import (
	"agent-overflow/internal/itemwire"
	"agent-overflow/internal/store"
)

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
	Action    string           `json:"action"`
	ThreadID  string           `json:"threadId"`
	Item      *store.Item      `json:"item,omitempty"`
	ItemID    string           `json:"itemId,omitempty"`
	Kind      string           `json:"kind,omitempty"`
	Delta     string           `json:"delta,omitempty"`
	Meta      string           `json:"meta,omitempty"`
	Patch     *ItemPatchFields `json:"patch,omitempty"`
	UpdatedAt int64            `json:"updatedAt,omitempty"`
}

// NewItemStreamUpsert is the single constructor every
// item-upsert emit site goes through, which is why the wire projection
// sits here rather than at the eleven call sites: a new emitter cannot
// forget it.
//
// The bus encodes one payload and broadcasts the bytes to every
// subscriber (transport.EventBus.Emit), so a push frame cannot carry a
// per-client preference the way an RPC result can. It therefore takes
// the preference-independent half of the projection — the byte budgets —
// with inline previews left ON: a row that arrives with its preview
// intact renders, where a row that arrived without one a client wanted
// would have to fetch. The other direction is where correctness lives,
// and it cannot happen: no row is ever elided without its marker.
//
// Under budget this is one length check per item event, and item events
// are per persisted row, not per delta (newItemStreamDelta is untouched
// and stays the streaming hot path).
func NewItemStreamUpsert(item store.Item) ItemStreamEvent {
	projected := itemwire.Project(item, true)
	return ItemStreamEvent{
		Action:   itemStreamActionUpsert,
		ThreadID: projected.ThreadID,
		Item:     &projected,
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

// newItemStreamPatch carries the settle-time field update for a row the
// client already holds. Its `meta` is the same value an upsert would
// have carried, so it takes the same projection — a row must not be able
// to arrive projected and then be patched back to its unprojected shape.
// The `payloadMeta` context the command-retention rule wants is not on a
// patch, so the rule reads as "no second copy", which is the safe
// direction: the leaf is kept.
func newItemStreamPatch(threadID, itemID, kind string, patch ItemPatchFields) ItemStreamEvent {
	if patch.Meta != nil {
		projected := itemwire.ProjectMeta(*patch.Meta, "")
		patch.Meta = &projected
	}
	return ItemStreamEvent{
		Action:   itemStreamActionPatch,
		ThreadID: threadID,
		ItemID:   itemID,
		Kind:     kind,
		Patch:    &patch,
	}
}
