// Package triage — handler for EventBlocksAbandoned, Claude's statement
// that the streamed content blocks of one API response from an index on
// were discarded after the CLI stopped reading a stalled or failed stream.
//
// The rows those blocks streamed into are completed history and stay as
// written. The abandonment is recorded as its own `notification` row of
// kind abandonedBlocksNotificationKind at its own timeline position, right
// after the last discarded row, naming the discarded rows in its meta
// (`item_ids`). The UI derives the discarded marking from that row, so the
// marking survives reload and history paging without any write to the
// rows it describes (docs/specs/agent-visibility.md §Immutable agent
// history).
//
// Rows are tied to their response through the content-block lifecycle:
// content_block_start carries the block's api message id and index
// (provider.MetaAPIMessageIDKey), noteStreamBlockStart parks them per
// scope, and the first delta's row creation binds them to the row id
// (bindStreamBlockRow) and stamps them onto the row's meta. The binding
// lives for the turn and is dropped with the turn's streams.

package triage

import (
	"encoding/json"
	"fmt"
	"sort"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
)

// abandonedBlocksNotificationKind is the `meta.kind` / tool_name of the
// notification row recording an abandonment. The frontend dispatches its
// renderer on it (TimelineLeaf.svelte); the store keeps it out of activity
// runs (store.AbandonedBlocksNotificationKind).
const abandonedBlocksNotificationKind = store.AbandonedBlocksNotificationKind

// abandonedBlocksSummary is the row's text. The renderer adds what
// followed when it can see it.
const abandonedBlocksSummary = "Discarded by Claude after a stalled response"

// streamBlockIdentity names a streamed content block within its API
// response.
type streamBlockIdentity struct {
	apiMessageID string
	index        int
}

// streamBlockRow is one streamed block's row, keyed by its block index.
type streamBlockRow struct {
	index  int
	itemID string
}

// streamBlockRowSet is every row one API response streamed into on one
// scope of one turn.
type streamBlockRowSet struct {
	turnIndex int
	scope     string
	rows      []streamBlockRow
}

func streamBlockRowsKey(turnIndex int, scope, apiMessageID string) string {
	return fmt.Sprintf("%d|%s|%s", turnIndex, scope, apiMessageID)
}

// contentBlockStartMeta is the slice of a content_block_start's Meta the
// binding reads.
type contentBlockStartMeta struct {
	Index        int    `json:"index"`
	APIMessageID string `json:"apiMessageId"`
}

// noteStreamBlockStart parks the block a content_block_start opened on its
// scope until the first delta creates the block's row. A start without an
// api message id (an older CLI, Codex) clears the scope's parked block so
// a stale identity never binds to a later row.
func (r *Router) noteStreamBlockStart(threadID, scope string, meta json.RawMessage) {
	var decoded contentBlockStartMeta
	if len(meta) > 0 {
		_ = json.Unmarshal(meta, &decoded)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	st := r.state(threadID)
	if decoded.APIMessageID == "" {
		delete(st.openStreamBlock, scope)
		return
	}
	if st.openStreamBlock == nil {
		st.openStreamBlock = make(map[string]streamBlockIdentity)
	}
	st.openStreamBlock[scope] = streamBlockIdentity{apiMessageID: decoded.APIMessageID, index: decoded.Index}
}

// bindStreamBlockRow consumes the scope's parked block and records itemID
// as its row. It reports false when no block was parked, which is every
// stream whose start carried no api message id.
func (r *Router) bindStreamBlockRow(threadID string, turnIndex int, scope, itemID string) (streamBlockIdentity, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	st := r.threadStateIfPresent(threadID)
	if st == nil {
		return streamBlockIdentity{}, false
	}
	id, ok := st.openStreamBlock[scope]
	if !ok {
		return streamBlockIdentity{}, false
	}
	delete(st.openStreamBlock, scope)
	if st.streamBlockRows == nil {
		st.streamBlockRows = make(map[string]*streamBlockRowSet)
	}
	key := streamBlockRowsKey(turnIndex, scope, id.apiMessageID)
	set := st.streamBlockRows[key]
	if set == nil {
		set = &streamBlockRowSet{turnIndex: turnIndex, scope: scope}
		st.streamBlockRows[key] = set
	}
	set.rows = append(set.rows, streamBlockRow{index: id.index, itemID: itemID})
	return id, true
}

// streamBlockRowMeta is the row meta recording which response and block a
// streamed row holds, merged onto the row at creation.
func streamBlockRowMeta(id streamBlockIdentity) json.RawMessage {
	meta, _ := json.Marshal(map[string]any{
		"api_message_id": id.apiMessageID,
		"block_index":    id.index,
	})
	return meta
}

// dropStreamBlockRowsLocked forgets the block bindings a turn boundary
// ends: those of turnIndex, or of every turn when allTurns is set, except
// the ones an agent owns. The parked starts of the dropped scopes go with
// them. Caller holds r.mu.
func dropStreamBlockRowsLocked(st *threadState, turnIndex int, allTurns bool, agentScopes map[string]bool) {
	for key, set := range st.streamBlockRows {
		if (!allTurns && set.turnIndex != turnIndex) || agentScopes[set.scope] {
			continue
		}
		delete(st.streamBlockRows, key)
	}
	for scope := range st.openStreamBlock {
		if agentScopes[scope] {
			continue
		}
		delete(st.openStreamBlock, scope)
	}
}

// abandonedStreamRowIDs returns, in block order, the ids of the rows the
// response streamed into at fromBlockIndex and above.
func (r *Router) abandonedStreamRowIDs(threadID string, turnIndex int, scope string, meta provider.BlocksAbandonedMeta) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	st := r.threadStateIfPresent(threadID)
	if st == nil {
		return nil
	}
	set := st.streamBlockRows[streamBlockRowsKey(turnIndex, scope, meta.APIMessageID)]
	if set == nil {
		return nil
	}
	rows := make([]streamBlockRow, 0, len(set.rows))
	for _, row := range set.rows {
		if row.index >= meta.FromBlockIndex {
			rows = append(rows, row)
		}
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].index < rows[j].index })
	ids := make([]string, 0, len(rows))
	for _, row := range rows {
		ids = append(ids, row.itemID)
	}
	return ids
}

// AbandonedBlocksItemID is the id of the notification row recording that
// one API response's blocks were abandoned: one row per response, so a
// repeated signal upserts in place.
func AbandonedBlocksItemID(turnIndex int, scope, apiMessageID string) string {
	if scope == "" {
		return fmt.Sprintf("notification:%d:abandoned:%s", turnIndex, apiMessageID)
	}
	return fmt.Sprintf("notification:%d:%s:abandoned:%s", turnIndex, scope, apiMessageID)
}

// handleBlocksAbandoned records the abandonment when it names rows this
// turn streamed. A from index above every block seen means nothing
// visible was discarded (the stall fell between blocks) and writes
// nothing.
func (r *Router) handleBlocksAbandoned(evt provider.ProviderEvent) error {
	var meta provider.BlocksAbandonedMeta
	if len(evt.Meta) == 0 || json.Unmarshal(evt.Meta, &meta) != nil || meta.APIMessageID == "" {
		return fmt.Errorf("blocks abandoned: meta %q names no response", string(evt.Meta))
	}
	turnIndex, err := r.turnIndexForEvent(evt)
	if err != nil {
		return fmt.Errorf("blocks abandoned turn index: %w", err)
	}
	scope := eventParentID(evt)
	ids := r.abandonedStreamRowIDs(evt.ThreadID, turnIndex, scope, meta)
	if len(ids) == 0 {
		return nil
	}
	rowMeta, err := json.Marshal(abandonedBlocksRowMeta{
		APIMessageID:   meta.APIMessageID,
		FromBlockIndex: meta.FromBlockIndex,
		ItemIDs:        ids,
	})
	if err != nil {
		return fmt.Errorf("blocks abandoned meta: %w", err)
	}
	evt.Meta = rowMeta
	_, err = r.persistTimelineNotificationWithID(evt, AbandonedBlocksItemID(turnIndex, scope, meta.APIMessageID), abandonedBlocksNotificationKind, abandonedBlocksSummary)
	return err
}

// abandonedBlocksRowMeta is the notification row's record of the
// abandonment, beside the shared `kind` and `title`. ItemIDs are the
// discarded rows in block order; FromBlockIndex 0 means the whole
// response was discarded.
type abandonedBlocksRowMeta struct {
	APIMessageID   string   `json:"api_message_id"`
	FromBlockIndex int      `json:"from_block_index"`
	ItemIDs        []string `json:"item_ids"`
}

func addAbandonedBlocksNotificationMeta(meta map[string]any, raw json.RawMessage) {
	var decoded abandonedBlocksRowMeta
	if len(raw) == 0 || json.Unmarshal(raw, &decoded) != nil {
		return
	}
	meta["api_message_id"] = decoded.APIMessageID
	meta["from_block_index"] = decoded.FromBlockIndex
	meta["item_ids"] = decoded.ItemIDs
}

// withStreamBlockMeta binds a new streaming row to the block parked for
// its scope and merges that block's response id and index onto the row's
// meta; a row with no parked block keeps meta as it was.
func (r *Router) withStreamBlockMeta(meta, threadID string, turnIndex int, scope, itemID string) string {
	id, ok := r.bindStreamBlockRow(threadID, turnIndex, scope, itemID)
	if !ok {
		return meta
	}
	return mergeItemMetaJSON(meta, streamBlockRowMeta(id))
}
