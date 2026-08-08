package sessionimport

import (
	"fmt"
	"strings"

	"agent-overflow/internal/importir"
	"agent-overflow/internal/store"
)

// Cursor is where one import stopped: the last timeline row it wrote, and
// the last source line it consumed.
//
// The two halves answer different questions and both are needed.
//
//   - (TurnIndex, ItemIndex) is a position in the THREAD. It is a pair
//     because `items.item_index` restarts at 0 in every turn, so a lone item
//     index names nothing — item 2 of turn 1 and item 2 of turn 9 share it.
//     A refresh compares the thread's real contents against this pair to
//     find out whether the user resumed the thread inside AO after importing
//     it, in which case appending the source's tail would interleave
//     duplicate history.
//   - (SourceUUID, SourceOffset) is a position in the SOURCE FILE. Both
//     providers fill SourceUUID — it is the provenance stamp of the last
//     event consumed — but which half a refresh ANCHORS on is
//     provider-shaped: Claude's transcript is a uuid DAG, so it walks the
//     branch for events after that uuid, while Codex's rollout is
//     append-only JSONL, so it tails from the byte offset instead (and
//     Claude leaves that offset at 0).
//
// A zero Cursor means "nothing was written". Its (-1, -1) form is what
// Apply stamps, so the divergence guard sees any row at all as growth.
type Cursor struct {
	TurnIndex    int
	ItemIndex    int
	SourceUUID   string
	SourceOffset int64
}

// EmptyCursor is the position of an import that produced no rows.
var EmptyCursor = Cursor{TurnIndex: -1, ItemIndex: -1}

// NewCursor derives the cursor of one built batch.
//
// The row position is the LAST one in the batch's ordering, not the last
// one appended: a tool_call is upserted in place across its whole life, so
// the writer's slice order is launch order, and a launch early in a turn can
// be settled after rows that follow it. The maximum is the position the
// thread actually ends at.
//
// The source coordinate comes from the last EVENT that carried one rather
// than from the batch, because events that produce no row (a turn boundary,
// a skipped reasoning block) still advance the file position a refresh must
// resume from.
func NewCursor(batch store.ImportBatch, events []importir.Event) Cursor {
	cursor := EmptyCursor
	for _, row := range batch.Rows {
		if row.Item.TurnIndex > cursor.TurnIndex ||
			(row.Item.TurnIndex == cursor.TurnIndex && row.Item.ItemIndex > cursor.ItemIndex) {
			cursor.TurnIndex = row.Item.TurnIndex
			cursor.ItemIndex = row.Item.ItemIndex
		}
	}
	for i := len(events) - 1; i >= 0; i-- {
		if uuid := strings.TrimSpace(events[i].SourceUUID); uuid != "" {
			cursor.SourceUUID = uuid
			break
		}
	}
	for i := len(events) - 1; i >= 0; i-- {
		if events[i].SourceOffset > 0 {
			cursor.SourceOffset = events[i].SourceOffset
			break
		}
	}
	return cursor
}

// Apply stamps the cursor onto an import-state row.
func (c Cursor) Apply(state *store.ThreadImportState) {
	if state == nil {
		return
	}
	state.LastTurnIndex = c.TurnIndex
	state.LastItemIndex = c.ItemIndex
	state.LastSourceUUID = c.SourceUUID
	state.LastSourceOffset = c.SourceOffset
}

// Advance folds a refresh's cursor onto the one already recorded, so a
// refresh can only ever move forward.
//
// Every field needs the guard for its own reason: a tail that produced no
// rows leaves the row position at EmptyCursor's (-1, -1), a Codex tail
// carries no transcript uuid at all, and a Claude tail carries no byte
// offset. Taking the later of each is what keeps the un-advanced halves
// from erasing the position the import wrote.
func (c Cursor) Advance(prev Cursor) Cursor {
	if prev.TurnIndex > c.TurnIndex ||
		(prev.TurnIndex == c.TurnIndex && prev.ItemIndex > c.ItemIndex) {
		c.TurnIndex = prev.TurnIndex
		c.ItemIndex = prev.ItemIndex
	}
	if c.SourceUUID == "" {
		c.SourceUUID = prev.SourceUUID
	}
	if prev.SourceOffset > c.SourceOffset {
		c.SourceOffset = prev.SourceOffset
	}
	return c
}

// CursorOf reads the row position back off a recorded import state.
func CursorOf(state store.ThreadImportState) Cursor {
	return Cursor{
		TurnIndex:    state.LastTurnIndex,
		ItemIndex:    state.LastItemIndex,
		SourceUUID:   state.LastSourceUUID,
		SourceOffset: state.LastSourceOffset,
	}
}

// Diverged reports whether the thread has grown past what its import wrote.
//
// True means the user resumed the imported session inside AO: the thread now
// holds rows at (turn_index, item_index) positions the import never
// allocated, and appending the source's tail would interleave a second copy
// of history under indices the live session already claimed. A refresh must
// refuse rather than repair — the two histories diverged at the provider,
// not in AO, and only the user knows which one they want.
func Diverged(s *store.Store, state store.ThreadImportState) (bool, error) {
	if s == nil {
		return false, fmt.Errorf("sessionimport: divergence check has no store")
	}
	if strings.TrimSpace(state.ThreadID) == "" {
		return false, fmt.Errorf("sessionimport: divergence check has no thread id")
	}
	return s.HasItemsAfterCursor(state.ThreadID, state.LastTurnIndex, state.LastItemIndex)
}
