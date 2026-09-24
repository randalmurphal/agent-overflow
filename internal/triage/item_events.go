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
	// itemStreamActionRemove retires a row the backend deleted from a
	// thread whose history is otherwise immutable. The one producer is the
	// Claude queue-boundary merge fold (claude_merge_fold.go): the provider
	// merged several dispatched messages into one transcript entry, so AO
	// rebuilds the surviving row as their join and removes the others.
	//
	// It rides the same ordered channel as the survivor's upsert, which is
	// what keeps a client from rendering the fold half-applied — the join
	// and the removals apply in one flush.
	itemStreamActionRemove = "remove"
	// itemStreamActionResync tells a client showing the thread to re-sync
	// its window: a write to another thread moved the thread's stamps, a
	// pointer fork's source changing or losing a row the fork shows, and
	// no row frame of the thread's own describes the change. See
	// NewItemStreamResync.
	itemStreamActionResync = "resync"
)

// ItemPatchFields carries the mutable subset of an Item for a patch event.
// Non-nil pointer fields mean "set to this value"; nil means "unchanged".
// It is what an emitter CHOOSES to change; the revision that write
// produced is not a choice, so it is not here (see ItemPatch).
type ItemPatchFields struct {
	Status    *string `json:"status,omitempty"`
	Summary   *string `json:"summary,omitempty"`
	Meta      *string `json:"meta,omitempty"`
	Decision  *string `json:"decision,omitempty"`
	UpdatedAt *int64  `json:"updatedAt,omitempty"`
}

// ItemPatch is the wire patch: the fields an emitter changed plus the
// revision the write that changed them produced. One flat JSON object, so
// the client reads `patch.rev` beside `patch.status`.
type ItemPatch struct {
	ItemPatchFields
	// Rev is not optional and is not a field of the row's content: it is
	// the row revision the patched content belongs to, read inside the
	// write's own transaction. A client folds the patch into the row it
	// holds, so without it the row would keep the revision its last
	// upsert carried while SQLite moved on, and every held window
	// containing a settled row would fail verification and pay a page
	// (docs/architecture/thread-replica-sync.md §3.1). Emitters receive it
	// from the writer rather than choosing it; see Router.emitItemPatch.
	Rev int64 `json:"rev"`
}

// ItemStreamEvent is one `provider:item_event` frame. ParentID rides
// deltas, metas and patches, which carry no row: a client whose window does
// not hold the row reads it to tell another scope's row from a missing one,
// and the transport reads it (with Item.ParentID on an upsert) to withhold a
// subagent's rows from connections not viewing that agent
// (eventscope.ScopeRootIDFromEvent). A frame describes one row, so it has
// exactly one scope.
type ItemStreamEvent struct {
	Action    string      `json:"action"`
	ThreadID  string      `json:"threadId"`
	Item      *store.Item `json:"item,omitempty"`
	ItemID    string      `json:"itemId,omitempty"`
	ParentID  string      `json:"parentId,omitempty"`
	Kind      string      `json:"kind,omitempty"`
	Delta     string      `json:"delta,omitempty"`
	Meta      string      `json:"meta,omitempty"`
	Patch     *ItemPatch  `json:"patch,omitempty"`
	UpdatedAt int64       `json:"updatedAt,omitempty"`
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

// NewItemStreamResync is the frame the app pushes for a pointer fork whose
// stamps another thread's write moved (store.OnForkStampsMoved). It
// carries the thread only: the client re-reads its window, which the
// store's stamps now describe.
func NewItemStreamResync(threadID string) ItemStreamEvent {
	return ItemStreamEvent{Action: itemStreamActionResync, ThreadID: threadID}
}

// newItemStreamRemove announces that a row no longer exists. Carries the id
// and kind only: there is no row left to project.
func newItemStreamRemove(threadID, itemID, kind string) ItemStreamEvent {
	return ItemStreamEvent{
		Action:   itemStreamActionRemove,
		ThreadID: threadID,
		ItemID:   itemID,
		Kind:     kind,
	}
}

func newItemStreamDelta(evt ItemDeltaEvent) ItemStreamEvent {
	return ItemStreamEvent{
		Action:    itemStreamActionDelta,
		ThreadID:  evt.ThreadID,
		ItemID:    evt.ItemID,
		ParentID:  evt.ParentID,
		Kind:      evt.Kind,
		Delta:     evt.Delta,
		UpdatedAt: evt.UpdatedAt,
	}
}

func newItemStreamMeta(threadID, itemID, parentID, kind, meta string, updatedAt int64) ItemStreamEvent {
	return ItemStreamEvent{
		Action:    itemStreamActionMeta,
		ThreadID:  threadID,
		ItemID:    itemID,
		ParentID:  parentID,
		Kind:      kind,
		Meta:      meta,
		UpdatedAt: updatedAt,
	}
}

// newItemStreamPatch carries the settle-time field update for a row the
// client already holds, at the revision that update produced. Its `meta`
// is the same value an upsert would have carried, so it takes the same
// projection — a row must not be able to arrive projected and then be
// patched back to its unprojected shape.
// The `payloadMeta` context the command-retention rule wants is not on a
// patch, so the rule reads as "no second copy", which is the safe
// direction: the leaf is kept.
func newItemStreamPatch(threadID, itemID, parentID, kind string, rev int64, patch ItemPatchFields) ItemStreamEvent {
	if patch.Meta != nil {
		projected := itemwire.ProjectMeta(*patch.Meta, "")
		patch.Meta = &projected
	}
	return ItemStreamEvent{
		Action:   itemStreamActionPatch,
		ThreadID: threadID,
		ItemID:   itemID,
		ParentID: parentID,
		Kind:     kind,
		Patch:    &ItemPatch{ItemPatchFields: patch, Rev: rev},
	}
}
