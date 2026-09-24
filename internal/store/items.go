package store

import (
	"database/sql"
	"errors"
	"fmt"
)

// ErrItemSettled is returned by AppendItemSummary
// when the target row exists but is no longer streaming — i.e. an
// interrupt or settle has already transitioned it to a terminal status
// on a different goroutine. Callers in the streaming hot path treat this
// as "drop the late delta", distinct from sql.ErrNoRows which still
// means the row is genuinely absent.
var ErrItemSettled = errors.New("store: item is no longer streaming")

// itemColumns is the canonical physical-local SELECT projection for
// scanItemRow. Logical timeline reads use itemHydrationColumns so local and
// imported branches can share this same scan order without joining the
// compound timeline_payloads view. Keep both in sync with the Item struct;
// adding a column means updating these lists, insertItemTx, and scanItemRow.
//
// `items.rev` rides last because it is the one column an import arm cannot
// supply from its own row (imported history reads as -1); keeping it at the
// tail means a hand-written arm projection appends one expression instead of
// splicing one into the middle of a positional scan order.
//
// The meta column is the row's meta as a read serves it: a local
// projection merges the row's subagent stamp (servedItemMetaFor), which
// it reads through servedItemJoin.
var itemColumns = `items.id, items.thread_id, items.turn_index, items.item_index,
    items.kind, items.role, items.status, items.summary,
    COALESCE(items.payload_id, ''), COALESCE(payloads.kind, ''), COALESCE(payloads.meta, ''),
    COALESCE(payloads.preview_spans, ''),
    COALESCE(items.input_payload_id, ''),
    items.parent_id, items.is_background, items.completion_of,
    items.tool_name, items.decision, ` + servedItemMetaFor("items.rev") + `, items.created_at, items.updated_at,
    items.rev`

// servedItemMetaFor is the meta column of an item projection on the arm
// whose revision expression is revExpr: a local row's meta as a read
// serves it (subagentServedMetaSQL, read through servedItemJoin).
// Imported rows carry no stamps.
func servedItemMetaFor(revExpr string) string {
	if revExpr == importedItemRevExpr {
		return "items.meta"
	}
	return subagentServedMetaSQL("items.")
}

const itemInsertPrefix = `INSERT INTO items (id, thread_id, turn_index, item_index, kind, role, status, summary,
		payload_id, input_payload_id, parent_id, is_background, completion_of, tool_name, decision, meta,
		created_at, updated_at)`

const itemInsertValues = `(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`
const itemInsertSQL = itemInsertPrefix + ` VALUES ` + itemInsertValues

type sqlExecutor interface {
	Exec(query string, args ...any) (sql.Result, error)
}

// scanItemRow accepts either *sql.Row or *sql.Rows via the common
// Scan(...any) error surface and hydrates one Item. Centralising the
// column order here lets the various list/get paths share a single
// definition instead of duplicating twelve field names five times.
func scanItemRow(scanner interface{ Scan(...any) error }) (Item, error) {
	var it Item
	var isBackground int
	if err := scanner.Scan(
		&it.ID, &it.ThreadID, &it.TurnIndex, &it.ItemIndex,
		&it.Kind, &it.Role, &it.Status, &it.Summary,
		&it.PayloadID, &it.PayloadKind, &it.PayloadMeta,
		&it.PayloadPreviewSpans,
		&it.InputPayloadID,
		&it.ParentID, &isBackground, &it.CompletionOf,
		&it.ToolName, &it.Decision, &it.Meta, &it.CreatedAt, &it.UpdatedAt,
		&it.Rev,
	); err != nil {
		return Item{}, err
	}
	it.IsBackground = isBackground != 0
	return it, nil
}

// defaultStatus coerces an empty Status to "completed" so callers that
// don't explicitly set the field still produce a valid row. The CHECK
// constraint would otherwise reject an empty string — this keeps the
// ergonomics of existing callers (InsertItem/AppendItem with a zero-
// value Status) working without forcing every call site to set Status
// explicitly.
func defaultStatus(s string) string {
	if s == "" {
		return "completed"
	}
	return s
}

func defaultItemMeta(meta string) string {
	if meta == "" {
		return "{}"
	}
	return meta
}

func applyItemDefaults(item *Item) {
	if item == nil {
		return
	}
	item.Status = defaultStatus(item.Status)
	item.Meta = defaultItemMeta(item.Meta)
	if item.CreatedAt == 0 {
		item.CreatedAt = nowMillis()
	}
	if item.UpdatedAt == 0 {
		item.UpdatedAt = item.CreatedAt
	}
}

func nextItemIndexTx(tx *sql.Tx, threadID string, turnIndex int, label string) (int, error) {
	var maxIndex sql.NullInt64
	query, args, err := turnAggregateQuery(tx, threadID, turnIndex, "MAX", "item_index")
	if err != nil {
		return 0, fmt.Errorf("%s: %w", label, err)
	}
	if err := tx.QueryRow(query, args...).Scan(&maxIndex); err != nil {
		return 0, fmt.Errorf("%s: %w", label, err)
	}
	if !maxIndex.Valid {
		return 0, nil
	}
	return int(maxIndex.Int64) + 1, nil
}

// headItemIndexTx mirrors nextItemIndexTx at the turn's HEAD:
// MIN(item_index)-1, or 0 for an empty turn (identical to
// nextItemIndexTx's empty-turn result, so head and tail placement only
// diverge once the turn has rows). Negative indexes are valid — every
// ordering read sorts by (turn_index, item_index), and the mid-turn
// anchor predicate (ItemIndex > 0) correctly treats a head-inserted
// row as turn-initial.
func headItemIndexTx(tx *sql.Tx, threadID string, turnIndex int, label string) (int, error) {
	var minIndex sql.NullInt64
	query, args, err := turnAggregateQuery(tx, threadID, turnIndex, "MIN", "item_index")
	if err != nil {
		return 0, fmt.Errorf("%s: %w", label, err)
	}
	if err := tx.QueryRow(query, args...).Scan(&minIndex); err != nil {
		return 0, fmt.Errorf("%s: %w", label, err)
	}
	if !minIndex.Valid {
		return 0, nil
	}
	return int(minIndex.Int64) - 1, nil
}

// itemInsertArgs is the bind list itemInsertSQL takes, in column order.
// It exists so the one-off inserts and the prepared-statement bulk path
// (ApplyImportBatch) cannot drift into two different column orders.
func itemInsertArgs(item Item) []any {
	return []any{
		item.ID, item.ThreadID, item.TurnIndex, item.ItemIndex, item.Kind, item.Role, item.Status, item.Summary,
		nilIfEmpty(item.PayloadID), nilIfEmpty(item.InputPayloadID), item.ParentID,
		boolToInt(item.IsBackground), item.CompletionOf, item.ToolName, item.Decision, item.Meta,
		item.CreatedAt, item.UpdatedAt,
	}
}

// itemInsertAdoptingSQL is itemInsertSQL that reports whether the new row
// has a visible child already, in either arm: rows written before their
// parent, which it adopts (cardWrite.inserted).
var itemInsertAdoptingSQL = itemInsertSQL + ` RETURNING ` + aggHasChildSQL("?2", "?1", "")

// insertItemTx inserts one row and indexes its text when the row arrives
// settled (a user message, a fork's copy or a transferred row). A row that
// arrives streaming is indexed later by the write that settles it. The
// index write shares this transaction, so a rolled-back insert leaves
// nothing searchable. w records the row for the subagent cards; a row
// that may anchor a card or count toward one reports its children in the
// same statement. A row under a pointer fork's materialized copies makes
// them the fork's own (settleForkCopiesTx).
func insertItemTx(tx *sql.Tx, w *cardWrite, item Item, label string) error {
	row := subagentRowOf(item)
	if err := w.check(row); err != nil {
		return err
	}
	if row.parentID != "" {
		if err := settleForkCopiesTx(tx, item.ThreadID, row.parentID); err != nil {
			return err
		}
	}
	hasChild := false
	if row.anchorable() || row.parentID != "" {
		if err := tx.QueryRow(itemInsertAdoptingSQL, itemInsertArgs(item)...).Scan(&hasChild); err != nil {
			return fmt.Errorf("%s: %w", label, err)
		}
	} else if _, err := tx.Exec(itemInsertSQL, itemInsertArgs(item)...); err != nil {
		return fmt.Errorf("%s: %w", label, err)
	}
	w.inserted(row, hasChild)
	return indexSettledItemTx(tx, item.ThreadID, item.ID, item.Kind, item.Status, item.Summary)
}

func insertItemWithIDTx(tx *sql.Tx, w *cardWrite, item Item, label string) error {
	return insertItemTx(tx, w, item, label+" "+item.ID)
}

// itemColumnsSansPayload mirrors itemColumns but without the
// payloads.kind / payloads.meta projection. Used on the narrow paths
// that only need status/summary/kind/role (force-close safety net, the
// turn-complete flip loop) so we skip the LEFT JOIN and the two string
// scans. Column order in scanItemRowSansPayload must match exactly.
var itemColumnsSansPayload = itemColumnsSansPayloadFor("items.thread_id", "items.rev")

// itemColumnsSansPayloadFor is itemColumnsSansPayload as a timelineArms
// projection: the arm supplies the thread id and revision expressions,
// and the ordering keys carry their names for the compound's ORDER BY.
func itemColumnsSansPayloadFor(threadIDExpr, revExpr string) string {
	return `items.id, ` + threadIDExpr + ` AS thread_id, items.turn_index AS turn_index, items.item_index AS item_index,
    items.kind, items.role, items.status, items.summary,
    COALESCE(items.payload_id, ''),
    items.parent_id, items.is_background, items.completion_of,
    items.tool_name, items.decision, ` + servedItemMetaFor(revExpr) + `, items.created_at, items.updated_at,
    ` + revExpr
}

// scanItemRowSansPayload hydrates an Item without the joined payload
// kind / meta columns. PayloadKind and PayloadMeta are left empty on
// the returned row — callers that need those must use scanItemRow.
func scanItemRowSansPayload(scanner interface{ Scan(...any) error }) (Item, error) {
	var it Item
	var isBackground int
	if err := scanner.Scan(
		&it.ID, &it.ThreadID, &it.TurnIndex, &it.ItemIndex,
		&it.Kind, &it.Role, &it.Status, &it.Summary,
		&it.PayloadID,
		&it.ParentID, &isBackground, &it.CompletionOf,
		&it.ToolName, &it.Decision, &it.Meta, &it.CreatedAt, &it.UpdatedAt,
		&it.Rev,
	); err != nil {
		return Item{}, err
	}
	it.IsBackground = isBackground != 0
	return it, nil
}
