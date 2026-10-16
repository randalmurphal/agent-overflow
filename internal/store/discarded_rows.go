package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
)

// Rows Claude discarded after a stalled response.
//
// Triage records an abandonment as its own notification row
// (internal/triage/abandoned_blocks.go): tool_name
// AbandonedBlocksNotificationKind, `meta.item_ids` naming the streamed rows
// Claude dropped from its transcript. The rows themselves are completed
// history and never change. A read marks them instead: decorateDiscardedRows
// sets Item.DiscardedBy on every row a record names, so a page that holds
// the row without the record (the record paged, trimmed or past the slice
// edge) still serves it marked. The record's id is a function of the row's
// own coordinates (AbandonedBlocksRecordID), so the lookup is one keyed
// probe per distinct response on the page, never a scan of the turn.

// AbandonedBlocksNotificationKind is the tool_name and `meta.kind` of the
// notification row recording that Claude discarded the streamed rows before
// it after a stalled response. It is about the prose it follows, not about
// activity, so a run never absorbs it: it breaks the run like the prose it
// describes would, on both sides of the paging contract
// (activityRunGrouping.ts mirrors this).
const AbandonedBlocksNotificationKind = "abandoned_blocks"

// AbandonedBlocksRecordID is the id of the notification row recording that
// one API response's blocks were abandoned: one row per response in its
// turn and scope, so a repeated signal upserts in place and a row that
// names its response (metaKeyAPIMessageID) names its record.
func AbandonedBlocksRecordID(turnIndex int, parentID, apiMessageID string) string {
	if parentID == "" {
		return fmt.Sprintf("notification:%d:abandoned:%s", turnIndex, apiMessageID)
	}
	return fmt.Sprintf("notification:%d:%s:abandoned:%s", turnIndex, parentID, apiMessageID)
}

// metaKeyAPIMessageID is the meta key a streamed assistant_text or thinking
// row carries naming the API response that produced it. Only Claude rows
// since 2.1.300 carry it (docs/references/claude-wire.md).
const metaKeyAPIMessageID = "api_message_id"

// discardableRowResponseID is the response id of a row a record could name:
// a streamed text or thinking row stamped with its response. Every other
// row returns "". The substring pre-check keeps rows without the stamp off
// a JSON parse.
func discardableRowResponseID(item Item) string {
	if item.Kind != "assistant_text" && item.Kind != "thinking" {
		return ""
	}
	if !strings.Contains(item.Meta, metaKeyAPIMessageID) {
		return ""
	}
	var decoded struct {
		APIMessageID string `json:"api_message_id"`
	}
	if json.Unmarshal([]byte(item.Meta), &decoded) != nil {
		return ""
	}
	return decoded.APIMessageID
}

func isAbandonedBlocksRecord(item Item) bool {
	return item.Kind == notificationKind && item.ToolName == AbandonedBlocksNotificationKind
}

// abandonedBlocksRecordItemIDs reads the discarded row ids a record names.
func abandonedBlocksRecordItemIDs(meta string) []string {
	var decoded struct {
		ItemIDs []string `json:"item_ids"`
	}
	if json.Unmarshal([]byte(meta), &decoded) != nil {
		return nil
	}
	return decoded.ItemIDs
}

// decorateDiscardedRows sets DiscardedBy on each page row an abandonment
// record names, reading the records by the ids the rows imply. A row whose
// record exists but does not name it (an earlier block of the same
// response, kept in the transcript) stays unmarked.
func (s *Store) decorateDiscardedRows(q sqlQueryer, threadID string, items []Item) ([]Item, error) {
	var recordIDs []string
	seen := make(map[string]bool)
	for i := range items {
		items[i].DiscardedBy = ""
		apiMessageID := discardableRowResponseID(items[i])
		if apiMessageID == "" {
			continue
		}
		recordID := AbandonedBlocksRecordID(items[i].TurnIndex, items[i].ParentID, apiMessageID)
		if !seen[recordID] {
			seen[recordID] = true
			recordIDs = append(recordIDs, recordID)
		}
	}
	if len(recordIDs) == 0 {
		return items, nil
	}
	selection, args, err := idListSelection(recordIDs)
	if err != nil {
		return nil, fmt.Errorf("store: read abandonment records for %s: %w", threadID, err)
	}
	records, err := queryHydratedTimelineItems(q, threadID, selection, args...)
	if err != nil {
		return nil, fmt.Errorf("store: read abandonment records for %s: %w", threadID, err)
	}
	discardedBy := make(map[string]string)
	for _, record := range records {
		if !isAbandonedBlocksRecord(record) {
			continue
		}
		for _, id := range abandonedBlocksRecordItemIDs(record.Meta) {
			discardedBy[id] = record.ID
		}
	}
	for i := range items {
		if recordID, ok := discardedBy[items[i].ID]; ok {
			items[i].DiscardedBy = recordID
		}
	}
	return items, nil
}

// abandonmentRecordExistsTx reports whether the record a row's coordinates
// name exists, by key on every arm: ProbeWireItem's narrowing for a
// response-stamped row, run once per row write on the provider event path.
func abandonmentRecordExistsTx(q sqlQueryer, threadID, recordID string) (bool, error) {
	probe, args, err := timelineArms(q, threadID, timelineSelection{
		Columns:  func(string, string) string { return "1" },
		KeyFirst: true,
		Where:    "items.id = ?", WhereArgs: []any{recordID},
	})
	if err != nil {
		return false, err
	}
	var exists bool
	if err := q.QueryRow(`SELECT EXISTS(`+probe+`)`, args...).Scan(&exists); err != nil {
		return false, fmt.Errorf("store: probe abandonment record %s/%s: %w", threadID, recordID, err)
	}
	return exists, nil
}

// touchDiscardedRowsSQL is stampDiscardedRowsTx's touch: the same
// `updated_at = updated_at` as bumpHistoryRevForItemTx, over the rows a
// record names, so the item UPDATE trigger stamps each one.
const touchDiscardedRowsSQL = `UPDATE items SET updated_at = updated_at
		  WHERE thread_id = ? AND id IN (SELECT value FROM json_each(?))`

// stampDiscardedRowsTx advances the `rev` of every row a written
// abandonment record names, in the record's transaction. The record
// changes what a read of those rows returns (DiscardedBy) without writing
// them, and a client holding them at their old revision must not prove a
// window fresh (docs/architecture/thread-replica-sync.md §3.1). Any other
// row writes nothing here.
func stampDiscardedRowsTx(tx *sql.Tx, item Item) error {
	if !isAbandonedBlocksRecord(item) {
		return nil
	}
	ids := abandonedBlocksRecordItemIDs(item.Meta)
	if len(ids) == 0 {
		return nil
	}
	list, err := jsonList(ids)
	if err != nil {
		return fmt.Errorf("store: stamp rows discarded by %s/%s: %w", item.ThreadID, item.ID, err)
	}
	if _, err := tx.Exec(touchDiscardedRowsSQL, item.ThreadID, list); err != nil {
		return fmt.Errorf("store: stamp rows discarded by %s/%s: %w", item.ThreadID, item.ID, err)
	}
	return nil
}
