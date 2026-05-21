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

// itemColumns is the canonical SELECT projection for scanItemRow. Keep in sync
// with the Item struct; adding a column means updating this list,
// insertItemTx, and scanItemRow together.
const itemColumns = `items.id, items.thread_id, items.turn_index, items.item_index,
    items.kind, items.role, items.status, items.summary,
    COALESCE(items.payload_id, ''), COALESCE(payloads.kind, ''), COALESCE(payloads.meta, ''),
    COALESCE(items.input_payload_id, ''),
    items.parent_id, items.is_background, items.completion_of,
    items.tool_name, items.decision, items.meta, items.created_at, items.updated_at`

const itemInsertSQL = `INSERT INTO items (id, thread_id, turn_index, item_index, kind, role, status, summary,
    payload_id, input_payload_id, parent_id, is_background, completion_of, tool_name, decision, meta,
    created_at, updated_at)
 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`

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
		&it.InputPayloadID,
		&it.ParentID, &isBackground, &it.CompletionOf,
		&it.ToolName, &it.Decision, &it.Meta, &it.CreatedAt, &it.UpdatedAt,
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
	if err := tx.QueryRow(
		`SELECT MAX(item_index) FROM items WHERE thread_id = ? AND turn_index = ?`,
		threadID, turnIndex,
	).Scan(&maxIndex); err != nil {
		return 0, fmt.Errorf("%s: %w", label, err)
	}
	if !maxIndex.Valid {
		return 0, nil
	}
	return int(maxIndex.Int64) + 1, nil
}

func insertItemTx(exec sqlExecutor, item Item, label string) error {
	if _, err := exec.Exec(
		itemInsertSQL,
		item.ID, item.ThreadID, item.TurnIndex, item.ItemIndex, item.Kind, item.Role, item.Status, item.Summary,
		nilIfEmpty(item.PayloadID), nilIfEmpty(item.InputPayloadID), item.ParentID,
		boolToInt(item.IsBackground), item.CompletionOf, item.ToolName, item.Decision, item.Meta,
		item.CreatedAt, item.UpdatedAt,
	); err != nil {
		return fmt.Errorf("%s: %w", label, err)
	}
	return nil
}

func insertItemWithIDTx(exec sqlExecutor, item Item, label string) error {
	if _, err := exec.Exec(
		itemInsertSQL,
		item.ID, item.ThreadID, item.TurnIndex, item.ItemIndex, item.Kind, item.Role, item.Status, item.Summary,
		nilIfEmpty(item.PayloadID), nilIfEmpty(item.InputPayloadID), item.ParentID,
		boolToInt(item.IsBackground), item.CompletionOf, item.ToolName, item.Decision, item.Meta,
		item.CreatedAt, item.UpdatedAt,
	); err != nil {
		return fmt.Errorf("%s %s: %w", label, item.ID, err)
	}
	return nil
}

// itemColumnsSansPayload mirrors itemColumns but without the
// payloads.kind / payloads.meta projection. Used on the narrow paths
// that only need status/summary/kind/role (force-close safety net, the
// turn-complete flip loop) so we skip the LEFT JOIN and the two string
// scans. Column order in scanItemRowSansPayload must match exactly.
const itemColumnsSansPayload = `items.id, items.thread_id, items.turn_index, items.item_index,
    items.kind, items.role, items.status, items.summary,
    COALESCE(items.payload_id, ''),
    items.parent_id, items.is_background, items.completion_of,
    items.tool_name, items.decision, items.meta, items.created_at, items.updated_at`

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
	); err != nil {
		return Item{}, err
	}
	it.IsBackground = isBackground != 0
	return it, nil
}
