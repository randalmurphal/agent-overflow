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

func (s *Store) appendStreamingItemSummary(
	threadID string,
	id string,
	operation string,
	rereadOperation string,
	runUpdate func(tx *sql.Tx) (sql.Result, error),
) (Item, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return Item{}, fmt.Errorf("store: begin %s tx: %w", operation, err)
	}
	defer tx.Rollback()

	result, err := runUpdate(tx)
	if err != nil {
		return Item{}, fmt.Errorf("store: %s %s/%s: %w", operation, threadID, id, err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return Item{}, fmt.Errorf("store: rows affected %s %s/%s: %w", operation, threadID, id, err)
	}
	if affected == 0 {
		if err := classifyStreamingUpdateMissTx(tx, threadID, id, operation); err != nil {
			return Item{}, err
		}
	}

	updated, err := readBackItemTx(tx, threadID, id)
	if err != nil {
		return Item{}, fmt.Errorf("store: %s %s/%s: %w", rereadOperation, threadID, id, err)
	}
	if err := tx.Commit(); err != nil {
		return Item{}, fmt.Errorf("store: commit %s tx: %w", operation, err)
	}
	return updated, nil
}

func classifyStreamingUpdateMissTx(tx *sql.Tx, threadID string, id string, operation string) error {
	// The UPDATE matched no rows. Because the UPDATE already required
	// status='streaming', any existing row is settled; only absence means
	// callers should fall back to creating the row.
	var exists int
	probeErr := tx.QueryRow(
		`SELECT 1 FROM items WHERE thread_id = ? AND id = ?`,
		threadID, id,
	).Scan(&exists)
	if errors.Is(probeErr, sql.ErrNoRows) {
		return sql.ErrNoRows
	}
	if probeErr != nil {
		return fmt.Errorf("store: probe item existence for %s %s/%s: %w", operation, threadID, id, probeErr)
	}
	return ErrItemSettled
}

func readBackItemTx(tx *sql.Tx, threadID string, id string) (Item, error) {
	row := tx.QueryRow(
		`SELECT `+itemColumns+`
		   FROM items
		   LEFT JOIN payloads ON payloads.id = items.payload_id
		  WHERE items.thread_id = ? AND items.id = ?`,
		threadID, id,
	)
	return scanItemRow(row)
}

func (s *Store) InsertItem(item Item) error {
	applyItemDefaults(&item)
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("store: begin insert item tx: %w", err)
	}
	defer tx.Rollback()

	if err := insertItemTx(tx, item, "store: insert item"); err != nil {
		return err
	}
	// Thread activity is bumped explicitly via Store.MarkThreadActivity by
	// the triage paths that count as a meaningful interaction (user_text
	// persist, turn settle, approval / user-input request creation). Item
	// inserts on their own do not advance the sidebar timestamp.
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit insert item tx: %w", err)
	}
	return nil
}

// AppendItem inserts an item at the next available item_index for
// (thread, turn), computed atomically inside the transaction. Unlike
// InsertItem, the caller does not pass item_index — the store derives it
// as MAX(item_index)+1 within the same transaction as the insert, so two
// concurrent AppendItem calls for the same (thread, turn) cannot land on
// the same slot. Returns the assigned item_index.
//
// Use this when the caller's intent is "add a new timeline entry" and any
// monotonic index is acceptable. Use InsertItem when the caller must
// control the exact index (e.g. CloneThreadItems preserving source
// ordering, migrations replaying a fixed sequence).
func (s *Store) AppendItem(item Item) (int, error) {
	applyItemDefaults(&item)
	tx, err := s.db.Begin()
	if err != nil {
		return 0, fmt.Errorf("store: begin append item tx: %w", err)
	}
	defer tx.Rollback()

	next, err := nextItemIndexTx(tx, item.ThreadID, item.TurnIndex, "store: append item next index")
	if err != nil {
		return 0, err
	}
	item.ItemIndex = next

	if err := insertItemTx(tx, item, "store: append item insert"); err != nil {
		return 0, err
	}
	// Thread activity is bumped explicitly by triage interaction paths,
	// not on every appended item. See InsertItem and MarkThreadActivity.
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("store: commit append item tx: %w", err)
	}
	return next, nil
}

// AppendItemSummary appends delta to the item's summary column in-place
// without round-tripping the full existing summary through Go memory. The
// caller passes only the newly-arrived text; SQLite's `||` operator does the
// concatenation against the already-stored column value. updatedAt is
// written in the same UPDATE so the hot streaming path does not need two
// statements. Returns the re-read Item (including the joined payload_kind
// and payload_meta) so the caller can emit the updated row without
// performing its own GetThreadItem round-trip.
//
// This is the dedicated hot-path fix for the former O(N²) streaming
// behavior in triage: handleTextDelta / handleThinking used to
// GetThreadItem → existing.Summary+delta in Go → UpsertItem (which
// re-reads via LEFT JOIN), producing 3 round-trips and a full-summary
// allocation per delta. AppendItemSummary collapses all three into one
// UPDATE + one SELECT, keeps the quadratic string concatenation inside
// SQLite's blob-append, and preserves the existing item_index, role,
// kind, status, and payload_id (none of which change across deltas).
//
// Returns sql.ErrNoRows (wrapped) if no item matches id. Callers that
// need to create the item on the first delta should call UpsertItem for
// the initial delta and AppendItemSummary for every subsequent delta.
//
// Does NOT bump threads.updated_at — sidebar activity is bumped only at
// interaction points (user_text persist, turn settle, approval/user-input
// requests) via Store.MarkThreadActivity.
func (s *Store) AppendItemSummary(threadID, id, delta string, updatedAt int64) (Item, error) {
	return s.appendStreamingItemSummary(
		threadID,
		id,
		"append item summary",
		"re-read appended item",
		func(tx *sql.Tx) (sql.Result, error) {
			return tx.Exec(
				`UPDATE items SET summary = summary || ?, updated_at = ? WHERE thread_id = ? AND id = ? AND status = 'streaming'`,
				delta, updatedAt, threadID, id,
			)
		},
	)
}

// AppendItemSummaryTail appends delta to a streaming item's summary while
// keeping only the LAST maxRunes characters of the accumulated text. Use
// this for timeline rows whose full content lives in payloads.data and
// whose collapsed-row preview should reflect the END of the content (the
// frontend renders a sliding-tail viewport for thinking rows). Storing
// the tail directly means triage's settle path doesn't need to re-read
// payloads.data to derive the right preview.
func (s *Store) AppendItemSummaryTail(threadID, id, delta string, maxRunes int, updatedAt int64) (Item, error) {
	if maxRunes < 0 {
		maxRunes = 0
	}

	return s.appendStreamingItemSummary(
		threadID,
		id,
		"append item summary tail",
		"re-read tail-appended item",
		func(tx *sql.Tx) (sql.Result, error) {
			return tx.Exec(
				`UPDATE items
				    SET summary = CASE
				            WHEN length(summary || ?) <= ? THEN summary || ?
				            ELSE substr(summary || ?, length(summary || ?) - ? + 1)
				        END,
				        updated_at = ?
				  WHERE thread_id = ? AND id = ? AND status = 'streaming'`,
				delta, maxRunes, delta, delta, delta, maxRunes,
				updatedAt, threadID, id,
			)
		},
	)
}

// UpsertItem persists `item` (inserting or updating depending on whether a
// row with the same (thread_id, id) already exists) together with an
// optional `payload` and returns the re-read row (joined with its payload
// meta/kind) so the caller can emit the canonical persisted state without a
// separate round-trip. Thread activity is bumped explicitly via
// Store.MarkThreadActivity from triage; this helper does not touch
// threads.updated_at.
//
// The method is split into three small helpers that run inside one
// transaction:
//
//   - upsertPayload stores the payload blob first and links its id onto
//     the item so the subsequent item write carries the right foreign key.
//   - writeItem looks up an existing row and dispatches to either
//     updateExistingItem or insertNewItem; both preserve the caller's
//     intent about which fields change.
//   - readBackUpsertedItem re-reads the row through the same JOIN used by
//     ListItems so the returned Item matches what ListItems would surface.
func (s *Store) UpsertItem(item Item, payload *Payload) (Item, error) {
	return s.UpsertItemWithInputPayload(item, payload, nil)
}

// UpsertItemWithInputPayload is the two-payload sibling of UpsertItem:
// it accepts an optional `inputPayload` whose id is linked into
// `items.input_payload_id`. Triage uses it to promote heavy tool-call
// inputs (Edit `old_string`/`new_string`, MultiEdit `edits`, Write
// `content`, etc.) out of `items.meta` and into a lazy-loaded payload
// row of kind "tool_call_input". Both payloads land in the same
// transaction as the item upsert so the FK link can never point at a
// missing row.
//
// Pass nil for either payload to skip it. Passing nil for both is
// equivalent to UpsertItem(item, nil).
func (s *Store) UpsertItemWithInputPayload(item Item, resultPayload, inputPayload *Payload) (Item, error) {
	applyItemDefaults(&item)
	tx, err := s.db.Begin()
	if err != nil {
		return Item{}, fmt.Errorf("store: begin upsert item tx: %w", err)
	}
	defer tx.Rollback()

	if err := upsertPayload(tx, resultPayload, &item); err != nil {
		return Item{}, err
	}
	if err := upsertInputPayload(tx, inputPayload, &item); err != nil {
		return Item{}, err
	}
	if err := writeItem(tx, &item); err != nil {
		return Item{}, err
	}

	persisted, err := readBackUpsertedItem(tx, item.ThreadID, item.ID)
	if err != nil {
		return Item{}, err
	}
	if err := tx.Commit(); err != nil {
		return Item{}, fmt.Errorf("store: commit upsert item tx: %w", err)
	}
	return persisted, nil
}

// upsertPayload writes the optional payload blob and links its id onto
// `item`. When payload is nil the function is a no-op; otherwise it runs
// the same INSERT OR REPLACE semantics UpsertItem has always used so
// repeated upserts against the same payload id refresh the blob.
func upsertPayload(tx *sql.Tx, payload *Payload, item *Item) error {
	if payload == nil {
		return nil
	}
	if _, err := tx.Exec(
		`INSERT OR REPLACE INTO payloads (id, kind, meta, data, created_at)
		 VALUES (?, ?, ?, ?, ?)`,
		payload.ID, payload.Kind, payload.Meta, payload.Data, payload.CreatedAt,
	); err != nil {
		return fmt.Errorf("store: upsert item payload %s: %w", payload.ID, err)
	}
	item.PayloadID = payload.ID
	return nil
}

// upsertInputPayload mirrors upsertPayload but writes the
// "tool_call_input" sibling payload and links its id onto
// `item.InputPayloadID` instead of `item.PayloadID`. Used by
// UpsertItemWithInputPayload to keep promoted heavy tool inputs (Edit
// `old_string` / `new_string`, MultiEdit `edits`, Write `content`, etc.)
// out of `items.meta`.
func upsertInputPayload(tx *sql.Tx, payload *Payload, item *Item) error {
	if payload == nil {
		return nil
	}
	if _, err := tx.Exec(
		`INSERT OR REPLACE INTO payloads (id, kind, meta, data, created_at)
		 VALUES (?, ?, ?, ?, ?)`,
		payload.ID, payload.Kind, payload.Meta, payload.Data, payload.CreatedAt,
	); err != nil {
		return fmt.Errorf("store: upsert item input payload %s: %w", payload.ID, err)
	}
	item.InputPayloadID = payload.ID
	return nil
}

// writeItem resolves whether `item` already exists on its thread and
// dispatches to the matching update/insert helper. The lookup query runs
// inside the same transaction so concurrent upserts can't both see
// "absent" and race to insert.
func writeItem(tx *sql.Tx, item *Item) error {
	var existingItemIndex int
	var existingCreatedAt int64
	row := tx.QueryRow(
		`SELECT item_index, created_at FROM items WHERE thread_id = ? AND id = ?`,
		item.ThreadID, item.ID,
	)
	switch err := row.Scan(&existingItemIndex, &existingCreatedAt); err {
	case nil:
		item.ItemIndex = existingItemIndex
		item.CreatedAt = existingCreatedAt
		return updateExistingItem(tx, *item)
	case sql.ErrNoRows:
		return insertNewItem(tx, item)
	default:
		return fmt.Errorf("store: upsert item lookup %s: %w", item.ID, err)
	}
}

// updateExistingItem writes every mutable column on the existing row.
// item_index / created_at are preserved (the caller already copied them
// from the lookup) so the upsert is logically "update-in-place".
//
// input_payload_id is preserved when the caller passes an empty value:
// completion-merge upserts (tool_lifecycle.go) reuse the launch row's
// input payload and would otherwise null it out. The COALESCE+NULLIF
// pair "use the new value if non-empty, else keep the existing column"
// keeps that contract in a single UPDATE.
func updateExistingItem(tx *sql.Tx, item Item) error {
	if _, err := tx.Exec(
		`UPDATE items
		 SET turn_index = ?, kind = ?, role = ?, status = ?, summary = ?,
		     payload_id = ?,
		     input_payload_id = COALESCE(NULLIF(?, ''), input_payload_id),
		     parent_id = ?, is_background = ?, completion_of = ?,
		     tool_name = ?, decision = ?, meta = ?, updated_at = ?
		 WHERE thread_id = ? AND id = ?`,
		item.TurnIndex, item.Kind, item.Role, item.Status, item.Summary,
		nilIfEmpty(item.PayloadID), item.InputPayloadID,
		item.ParentID, boolToInt(item.IsBackground), item.CompletionOf,
		item.ToolName, item.Decision, item.Meta, item.UpdatedAt, item.ThreadID, item.ID,
	); err != nil {
		return fmt.Errorf("store: update item %s: %w", item.ID, err)
	}
	return nil
}

// insertNewItem computes MAX(item_index)+1 within the transaction to
// keep concurrent upserts from colliding on the same slot, then inserts
// the row. The computed ItemIndex is written back onto `item` so the
// re-read step (readBackUpsertedItem) returns the persisted value.
func insertNewItem(tx *sql.Tx, item *Item) error {
	next, err := nextItemIndexTx(tx, item.ThreadID, item.TurnIndex, "store: upsert item next index")
	if err != nil {
		return err
	}
	item.ItemIndex = next
	if err := insertItemWithIDTx(tx, *item, "store: insert item"); err != nil {
		return err
	}
	return nil
}

// readBackUpsertedItem re-reads the just-written row through the same
// LEFT JOIN ListItems uses so the returned Item carries the current
// payload kind/meta. This lives inside the upsert transaction so
// callers observe their own write even with WAL-reader snapshots in play.
func readBackUpsertedItem(tx *sql.Tx, threadID, id string) (Item, error) {
	persisted, err := readBackItemTx(tx, threadID, id)
	if err != nil {
		return Item{}, fmt.Errorf("store: re-read upserted item %s: %w", id, err)
	}
	return persisted, nil
}

func (s *Store) ListItems(threadID string) ([]Item, error) {
	rows, err := s.db.Query(
		`SELECT `+itemColumns+`
		   FROM items
		   LEFT JOIN payloads ON payloads.id = items.payload_id
		  WHERE items.thread_id = ?
		  ORDER BY items.turn_index, items.item_index`,
		threadID,
	)
	if err != nil {
		return nil, fmt.Errorf("store: list items for thread %s: %w", threadID, err)
	}
	defer rows.Close()

	var items []Item
	for rows.Next() {
		it, err := scanItemRow(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scan item row: %w", err)
		}
		items = append(items, it)
	}
	return items, rows.Err()
}

func (s *Store) ListItemsForTurn(threadID string, turnIndex int) ([]Item, error) {
	rows, err := s.db.Query(
		`SELECT `+itemColumns+`
		   FROM items
		   LEFT JOIN payloads ON payloads.id = items.payload_id
		  WHERE items.thread_id = ? AND items.turn_index = ?
		  ORDER BY items.item_index`,
		threadID, turnIndex,
	)
	if err != nil {
		return nil, fmt.Errorf("store: list items for thread %s turn %d: %w", threadID, turnIndex, err)
	}
	defer rows.Close()

	var items []Item
	for rows.Next() {
		it, err := scanItemRow(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scan item row: %w", err)
		}
		items = append(items, it)
	}
	return items, rows.Err()
}

func (s *Store) LatestAssistantTextSummaryForParent(threadID, parentID string) (string, bool, error) {
	var summary string
	err := s.db.QueryRow(
		`SELECT summary
		   FROM items
		  WHERE thread_id = ?
		    AND parent_id = ?
		    AND kind = 'assistant_text'
		    AND summary <> ''
		  ORDER BY turn_index DESC, item_index DESC
		  LIMIT 1`,
		threadID,
		parentID,
	).Scan(&summary)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("store: latest assistant text summary for parent %s/%s: %w", threadID, parentID, err)
	}
	return summary, true, nil
}

func (s *Store) LastTurnIndex(threadID string) (int, error) {
	var maxIndex sql.NullInt64
	err := s.db.QueryRow(
		`SELECT MAX(turn_index)
		   FROM (
		         SELECT turn_index FROM items WHERE thread_id = ?
		         UNION ALL
		         SELECT turn_index FROM turns WHERE thread_id = ?
		        )`,
		threadID, threadID,
	).Scan(&maxIndex)
	if err != nil {
		return 0, fmt.Errorf("store: last turn index: %w", err)
	}
	if !maxIndex.Valid {
		return 0, nil
	}
	return int(maxIndex.Int64), nil
}

func (s *Store) HasItems(threadID string) (bool, error) {
	var exists int
	if err := s.db.QueryRow(
		`SELECT EXISTS(SELECT 1 FROM items WHERE thread_id = ? LIMIT 1)`,
		threadID,
	).Scan(&exists); err != nil {
		return false, fmt.Errorf("store: has items for thread %s: %w", threadID, err)
	}
	return exists != 0, nil
}

func (s *Store) HasRunningTopLevelForegroundToolCall(threadID string) (bool, error) {
	var exists int
	if err := s.db.QueryRow(
		`SELECT EXISTS(
		    SELECT 1 FROM items
		     WHERE thread_id = ?
		       AND kind = 'tool_call'
		       AND status = 'running'
		       AND is_background = 0
		       AND parent_id = ''
		     LIMIT 1
		)`,
		threadID,
	).Scan(&exists); err != nil {
		return false, fmt.Errorf("store: has running top-level foreground tool call for thread %s: %w", threadID, err)
	}
	return exists != 0, nil
}

func (s *Store) HasLiveBackgroundToolCall(threadID string) (bool, error) {
	var exists int
	if err := s.db.QueryRow(
		`SELECT EXISTS(
		    SELECT 1 FROM items
		     WHERE thread_id = ?
		       AND kind = 'tool_call'
		       AND status = 'running'
		       AND is_background = 1
		       AND COALESCE(json_extract(meta, '$.live_background_active'), 1) != 0
		       AND NOT EXISTS (
		         SELECT 1 FROM pending_background_task_terminals p
		          WHERE p.thread_id = items.thread_id
		            AND p.tool_use_id = items.id
		       )
		       AND NOT EXISTS (
		         SELECT 1 FROM items c
		          WHERE c.thread_id = items.thread_id
		            AND c.completion_of = items.id
		       )
		     LIMIT 1
		)`,
		threadID,
	).Scan(&exists); err != nil {
		return false, fmt.Errorf("store: has live background tool call for thread %s: %w", threadID, err)
	}
	return exists != 0, nil
}

func (s *Store) FindTurnItem(threadID string, turnIndex int, kind string) (Item, bool, error) {
	row := s.db.QueryRow(
		`SELECT `+itemColumns+`
		   FROM items
		   LEFT JOIN payloads ON payloads.id = items.payload_id
		  WHERE items.thread_id = ? AND items.turn_index = ? AND items.kind = ?
		 ORDER BY items.item_index DESC
		 LIMIT 1`,
		threadID, turnIndex, kind,
	)

	item, err := scanItemRow(row)
	if err == sql.ErrNoRows {
		return Item{}, false, nil
	}
	if err != nil {
		return Item{}, false, fmt.Errorf("store: find turn item: %w", err)
	}
	return item, true, nil
}

func (s *Store) GetItem(id string) (Item, bool, error) {
	row := s.db.QueryRow(
		`SELECT `+itemColumns+`
		   FROM items
		   LEFT JOIN payloads ON payloads.id = items.payload_id
		  WHERE items.id = ?
		  ORDER BY items.created_at DESC
		  LIMIT 1`,
		id,
	)

	item, err := scanItemRow(row)
	if err == sql.ErrNoRows {
		return Item{}, false, nil
	}
	if err != nil {
		return Item{}, false, fmt.Errorf("store: get item %s: %w", id, err)
	}
	return item, true, nil
}

// FindToolCallItemByTaskID resolves a thread's tool_call row whose persisted
// items.meta JSON carries a top-level task_id matching taskID. Used by the
// background completion router when a Claude task_updated/task_notification
// event arrives without an inline tool_use_id — most commonly after a
// reconnect with a fresh parser, when the adapter's in-memory
// task_id ↔ tool_use_id map has been dropped.
//
// The query is O(log N) thanks to the partial expression index
// idx_items_meta_task_id (migration v17) which materialises
// json_extract(meta, '$.task_id') for the narrow subset of rows that
// actually carry a task_id. The kind filter stays in Go-space rather
// than the index because every row this function cares about is a
// tool_call by construction (only that kind sets task_id in meta), and
// adding kind to the index would bloat it for no planner benefit.
//
// Empty taskID returns (Item{}, false, nil) so callers can short-circuit
// without a DB round-trip.
func (s *Store) FindToolCallItemByTaskID(threadID, taskID string) (Item, bool, error) {
	if taskID == "" {
		return Item{}, false, nil
	}
	row := s.db.QueryRow(
		`SELECT `+itemColumns+`
		   FROM items
		   LEFT JOIN payloads ON payloads.id = items.payload_id
		  WHERE items.thread_id = ?
		    AND json_extract(items.meta, '$.task_id') = ?
		  ORDER BY items.updated_at DESC
		  LIMIT 1`,
		threadID, taskID,
	)
	item, err := scanItemRow(row)
	if err == sql.ErrNoRows {
		return Item{}, false, nil
	}
	if err != nil {
		return Item{}, false, fmt.Errorf("store: find tool call by task id %s: %w", taskID, err)
	}
	return item, true, nil
}

// GetThreadItemByPayloadID returns the newest item on threadID whose
// payload_id OR input_payload_id matches payloadID, so a payload id is
// not usable outside the thread that references it. The two partial
// indexes (idx_items_payload_id, idx_items_input_payload_id) cover the
// two columns; UNION ALL keeps each branch index-friendly. A single
// OR-clause forces SQLite onto the broad thread_id index instead, which
// would scan every row in the thread on every lazy-load click.
func (s *Store) GetThreadItemByPayloadID(threadID, payloadID string) (Item, bool, error) {
	row := s.db.QueryRow(
		`SELECT `+itemColumns+`
		   FROM (
		         SELECT items.* FROM items
		          WHERE items.thread_id = ? AND items.payload_id = ?
		         UNION ALL
		         SELECT items.* FROM items
		          WHERE items.thread_id = ? AND items.input_payload_id = ?
		   ) AS items
		   LEFT JOIN payloads ON payloads.id = items.payload_id
		  ORDER BY items.updated_at DESC
		  LIMIT 1`,
		threadID, payloadID, threadID, payloadID,
	)
	item, err := scanItemRow(row)
	if err == sql.ErrNoRows {
		return Item{}, false, nil
	}
	if err != nil {
		return Item{}, false, fmt.Errorf("store: get item by payload id %s on thread %s: %w", payloadID, threadID, err)
	}
	return item, true, nil
}

func (s *Store) GetThreadItem(threadID, id string) (Item, bool, error) {
	row := s.db.QueryRow(
		`SELECT `+itemColumns+`
		   FROM items
		   LEFT JOIN payloads ON payloads.id = items.payload_id
		  WHERE items.thread_id = ? AND items.id = ?`,
		threadID, id,
	)

	item, err := scanItemRow(row)
	if err == sql.ErrNoRows {
		return Item{}, false, nil
	}
	if err != nil {
		return Item{}, false, fmt.Errorf("store: get item %s on thread %s: %w", id, threadID, err)
	}
	return item, true, nil
}

// FindNotificationItemByTaskID returns the newest notification row whose
// meta.task_id matches taskID. Claude task_notification rows use this to let
// later task terminals attach the durable output_file payload without treating
// the notification itself as lifecycle state.
func (s *Store) FindNotificationItemByTaskID(threadID, taskID string) (Item, bool, error) {
	if taskID == "" {
		return Item{}, false, nil
	}
	row := s.db.QueryRow(
		`SELECT `+itemColumns+`
		   FROM items
		   LEFT JOIN payloads ON payloads.id = items.payload_id
		  WHERE items.thread_id = ?
		    AND items.kind = 'notification'
		    AND json_extract(items.meta, '$.task_id') = ?
		  ORDER BY items.turn_index DESC, items.item_index DESC
		  LIMIT 1`,
		threadID, taskID,
	)
	item, err := scanItemRow(row)
	if err == sql.ErrNoRows {
		return Item{}, false, nil
	}
	if err != nil {
		return Item{}, false, fmt.Errorf("store: find notification by task_id %s: %w", taskID, err)
	}
	return item, true, nil
}

func (s *Store) ListTurnItems(threadID string, turnIndex int) ([]Item, error) {
	rows, err := s.db.Query(
		`SELECT `+itemColumns+`
		   FROM items
		   LEFT JOIN payloads ON payloads.id = items.payload_id
		  WHERE items.thread_id = ? AND items.turn_index = ?
		 ORDER BY items.item_index`,
		threadID, turnIndex,
	)
	if err != nil {
		return nil, fmt.Errorf("store: list turn items for thread %s turn %d: %w", threadID, turnIndex, err)
	}
	defer rows.Close()

	var items []Item
	for rows.Next() {
		it, err := scanItemRow(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scan turn item row: %w", err)
		}
		items = append(items, it)
	}
	return items, rows.Err()
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

// ListTurnItemsSansPayload is a lighter sibling of ListTurnItems that
// skips the payloads LEFT JOIN. Use it on paths that read only the
// item-table columns (status, summary, kind, role, is_background) —
// the force-close safety net and the truncated-turn flip loop both
// qualify. For any caller that inspects PayloadKind / PayloadMeta
// (e.g. tool_result_diff_upgrade.loadSummaryOnlyToolResultCandidate)
// keep ListTurnItems, which hydrates them.
func (s *Store) ListTurnItemsSansPayload(threadID string, turnIndex int) ([]Item, error) {
	rows, err := s.db.Query(
		`SELECT `+itemColumnsSansPayload+`
		   FROM items
		  WHERE items.thread_id = ? AND items.turn_index = ?
		 ORDER BY items.item_index`,
		threadID, turnIndex,
	)
	if err != nil {
		return nil, fmt.Errorf("store: list turn items (sans payload) for thread %s turn %d: %w", threadID, turnIndex, err)
	}
	defer rows.Close()

	var items []Item
	for rows.Next() {
		it, err := scanItemRowSansPayload(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scan turn item row (sans payload): %w", err)
		}
		items = append(items, it)
	}
	return items, rows.Err()
}

// ForceCloseRunningToolCallsInTurn flips every status=running +
// is_background=0 tool_call row in (threadID, turnIndex) to
// status=errored with the caller-provided summary. The UPDATEs and the
// thread's updated_at bump all run inside a single transaction so an
// N-orphan force-close pays one fsync (WAL commit) instead of N.
//
// Returns the flipped rows (with status/summary/updated_at already
// reflecting the post-write state) so the caller can fan out one
// `provider:item_event` upsert per row — the store handles the write, the
// caller handles the emit, matching the existing persistItem
// contract.
//
// summarise is called with the row's prior summary so callers can
// preserve idempotency of their suffix convention (the force-close
// summariser returns the same string when the suffix is already
// present). updatedAt is stamped on every flipped row.
//
// Backgrounded tool_call rows (is_background=1) are exempt — they
// legitimately outlive the turn per invariant 24. Rows in other
// statuses (streaming text/thinking, already-settled tool_calls) are
// left alone — this accessor is the narrow force-close path, not the
// broader flip-everything-to-errored path owned by
// flipTurnItemsErrored.
func (s *Store) ForceCloseRunningToolCallsInTurn(
	threadID string,
	turnIndex int,
	summarise func(string) string,
	updatedAt int64,
) ([]Item, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, fmt.Errorf("store: begin force-close tx: %w", err)
	}
	defer tx.Rollback()

	rows, err := tx.Query(
		`SELECT `+itemColumnsSansPayload+`
		   FROM items
		  WHERE items.thread_id = ?
		    AND items.turn_index = ?
		    AND items.kind = 'tool_call'
		    AND items.status = 'running'
		    AND items.is_background = 0
		 ORDER BY items.item_index`,
		threadID, turnIndex,
	)
	if err != nil {
		return nil, fmt.Errorf("store: force-close select for thread %s turn %d: %w", threadID, turnIndex, err)
	}

	var flipped []Item
	for rows.Next() {
		it, err := scanItemRowSansPayload(rows)
		if err != nil {
			rows.Close()
			return nil, fmt.Errorf("store: force-close scan: %w", err)
		}
		flipped = append(flipped, it)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, fmt.Errorf("store: force-close rows err: %w", err)
	}
	rows.Close()

	if len(flipped) == 0 {
		// Commit the no-op TX — cheaper than holding it open and lets
		// WAL recycle. The thread-touch below runs only when at least
		// one row actually flipped, matching the pre-refactor
		// persistItem-per-row behaviour.
		if err := tx.Commit(); err != nil {
			return nil, fmt.Errorf("store: commit force-close (no rows): %w", err)
		}
		return nil, nil
	}

	for i := range flipped {
		flipped[i].Status = "errored"
		flipped[i].Summary = summarise(flipped[i].Summary)
		flipped[i].UpdatedAt = updatedAt

		if _, err := tx.Exec(
			`UPDATE items
			    SET status = ?, summary = ?, updated_at = ?
			  WHERE thread_id = ? AND id = ?`,
			flipped[i].Status, flipped[i].Summary, flipped[i].UpdatedAt,
			flipped[i].ThreadID, flipped[i].ID,
		); err != nil {
			return nil, fmt.Errorf("store: force-close update %s: %w", flipped[i].ID, err)
		}
	}

	// Thread activity is bumped at the turn-settle path (via
	// MarkThreadActivity in triage), not here. Force-closing orphan
	// tool_calls is part of that same boundary.
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("store: commit force-close tx: %w", err)
	}
	return flipped, nil
}

// ListRunningBackgroundToolCalls returns every still-`running` +
// `is_background=1` `tool_call` row with no completion sibling for the
// given thread. The on-reopen Codex reconciler uses it to scope its flip
// when the probe reports a systemError — those are the only rows whose
// disposition is uncertain after a session restart (inline tool calls
// complete or error in the same turn; background rows with completion
// siblings are already settled).
//
// The filter pushes down into SQLite (vs. fetching ListItems and
// filtering in Go) so threads with deep history don't pay the
// deserialization cost on every reopen. Reopen is a cold path today but
// the query is narrow enough that a dedicated method is cheaper than a
// full table hydration.
func (s *Store) ListRunningBackgroundToolCalls(threadID string) ([]Item, error) {
	rows, err := s.db.Query(
		`SELECT `+itemColumns+`
		   FROM items
		   LEFT JOIN payloads ON payloads.id = items.payload_id
		  WHERE items.thread_id = ?
		    AND items.kind = 'tool_call'
		    AND items.status = 'running'
		    AND items.is_background = 1
		    AND NOT EXISTS (
		      SELECT 1 FROM items c
		       WHERE c.thread_id = items.thread_id
		         AND c.completion_of = items.id
		    )
		  ORDER BY items.turn_index, items.item_index`,
		threadID,
	)
	if err != nil {
		return nil, fmt.Errorf("store: list running background tool calls for thread %s: %w", threadID, err)
	}
	defer rows.Close()

	var items []Item
	for rows.Next() {
		it, err := scanItemRow(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scan running bg tool call row: %w", err)
		}
		items = append(items, it)
	}
	return items, rows.Err()
}

func (s *Store) ListIncompleteCodexSubagentLaunches(threadID string) ([]Item, error) {
	rows, err := s.db.Query(
		`SELECT `+itemColumns+`
		   FROM items
		   LEFT JOIN payloads ON payloads.id = items.payload_id
		  WHERE items.thread_id = ?
		    AND items.kind = 'tool_call'
		    AND items.tool_name = 'collab_agent'
		    AND items.is_background = 1
		    AND NOT EXISTS (
		      SELECT 1 FROM items c
		       WHERE c.thread_id = items.thread_id
		         AND c.completion_of = items.id
		    )
		  ORDER BY items.turn_index, items.item_index`,
		threadID,
	)
	if err != nil {
		return nil, fmt.Errorf("store: list incomplete Codex subagent launches for thread %s: %w", threadID, err)
	}
	defer rows.Close()

	var items []Item
	for rows.Next() {
		it, err := scanItemRow(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scan incomplete Codex subagent launch row: %w", err)
		}
		items = append(items, it)
	}
	return items, rows.Err()
}

func (s *Store) GetIncompleteCodexSubagentLaunch(threadID, itemID string) (Item, bool, error) {
	row := s.db.QueryRow(
		`SELECT `+itemColumns+`
		   FROM items
		   LEFT JOIN payloads ON payloads.id = items.payload_id
		  WHERE items.thread_id = ?
		    AND items.id = ?
		    AND items.kind = 'tool_call'
		    AND items.tool_name = 'collab_agent'
		    AND items.is_background = 1
		    AND NOT EXISTS (
		      SELECT 1 FROM items c
		       WHERE c.thread_id = items.thread_id
		         AND c.completion_of = items.id
		    )`,
		threadID,
		itemID,
	)
	it, err := scanItemRow(row)
	if err == nil {
		return it, true, nil
	}
	if errors.Is(err, sql.ErrNoRows) {
		return Item{}, false, nil
	}
	return Item{}, false, fmt.Errorf("store: get incomplete Codex subagent launch %s on thread %s: %w", itemID, threadID, err)
}

// ListOrphanedBackgroundLaunches returns every backgrounded tool_call
// row across all threads that is still running and has no completion
// sibling. Used by app startup to detect launches whose owning provider
// session died with the previous app instance — the agent will never
// observe completion, so the launch would otherwise hang forever in
// the tray and chat.
//
// Launches with a `pending_background_task_terminals` stash entry are
// included: at boot time no provider session is alive yet, so any
// stash row is by definition orphaned (the observer that would drain
// it is dead). The recovery path drains the stash and uses its data
// when synthesising the completion sibling, so the user sees the real
// exit state rather than a generic session_died/killed badge.
func (s *Store) ListOrphanedBackgroundLaunches() ([]Item, error) {
	rows, err := s.db.Query(
		`SELECT ` + itemColumns + `
		   FROM items
		   LEFT JOIN payloads ON payloads.id = items.payload_id
		  WHERE items.kind = 'tool_call'
		    AND items.status = 'running'
		    AND items.is_background = 1
		    AND NOT EXISTS (
		      SELECT 1 FROM items c
		       WHERE c.thread_id = items.thread_id
		         AND c.completion_of = items.id
		    )
		  ORDER BY items.thread_id, items.turn_index, items.item_index`,
	)
	if err != nil {
		return nil, fmt.Errorf("store: list orphaned background launches: %w", err)
	}
	defer rows.Close()

	var items []Item
	for rows.Next() {
		it, err := scanItemRow(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scan orphaned bg launch row: %w", err)
		}
		items = append(items, it)
	}
	return items, rows.Err()
}

// FlipGhostBackgroundRowsOnStart flips every `status='running' +
// is_background=1 + kind='tool_call'` row for the thread to
// `status='errored'`, `decision='lost'`, and rewrites each row's summary
// via summarise. Runs inside a single transaction so an N-ghost flip
// pays one WAL commit (mirrors ForceCloseRunningToolCallsInTurn's
// batching model).
//
// Returns the flipped rows (with status/summary/decision/updated_at
// already reflecting the post-write state) so the caller can fan out
// one `provider:item_event` upsert per row.
//
// Called on EVERY Codex session start — new OR resume — because a prior
// subprocess dying takes its PTYs with it, so any persisted
// `is_background=running` row is a ghost regardless of what the probe
// reports. Claude's analog (`stop_task` / explicit completion) runs on
// a different rail; the caller scopes this method to Codex threads.
//
// summarise is called with each row's prior summary so callers preserve
// idempotency of their suffix convention (the ghost-flip summariser
// returns the same string when the suffix is already present). updatedAt
// is stamped on every flipped row.
//
// Non-background running rows and non-tool_call backgrounded rows
// (spawn_agent subagent rows carry kind='tool_call' too, so those DO
// flip — they're unreachable from a new Codex subprocess just like
// unifiedExec PTYs are) are the narrow target. Rows in other statuses
// (streaming text, already-settled tool_calls) are left alone.
func (s *Store) FlipGhostBackgroundRowsOnStart(
	threadID string,
	summarise func(string) string,
	updatedAt int64,
) ([]Item, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, fmt.Errorf("store: begin ghost-flip tx: %w", err)
	}
	defer tx.Rollback()

	rows, err := tx.Query(
		`SELECT `+itemColumnsSansPayload+`
		   FROM items
		  WHERE items.thread_id = ?
		    AND items.kind = 'tool_call'
		    AND items.status = 'running'
		    AND items.is_background = 1
		 ORDER BY items.turn_index, items.item_index`,
		threadID,
	)
	if err != nil {
		return nil, fmt.Errorf("store: ghost-flip select for thread %s: %w", threadID, err)
	}

	var flipped []Item
	for rows.Next() {
		it, err := scanItemRowSansPayload(rows)
		if err != nil {
			rows.Close()
			return nil, fmt.Errorf("store: ghost-flip scan: %w", err)
		}
		flipped = append(flipped, it)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, fmt.Errorf("store: ghost-flip rows err: %w", err)
	}
	rows.Close()

	if len(flipped) == 0 {
		// Commit the no-op TX — cheaper than holding it open and lets
		// WAL recycle. The thread-touch below runs only when at least
		// one row actually flipped so an empty thread doesn't spuriously
		// bump `threads.updated_at`.
		if err := tx.Commit(); err != nil {
			return nil, fmt.Errorf("store: commit ghost-flip (no rows): %w", err)
		}
		return nil, nil
	}

	for i := range flipped {
		flipped[i].Status = "errored"
		flipped[i].Summary = summarise(flipped[i].Summary)
		flipped[i].Decision = "lost"
		flipped[i].UpdatedAt = updatedAt

		if _, err := tx.Exec(
			`UPDATE items
			    SET status = ?, summary = ?, decision = ?, updated_at = ?
			  WHERE thread_id = ? AND id = ?`,
			flipped[i].Status, flipped[i].Summary, flipped[i].Decision, flipped[i].UpdatedAt,
			flipped[i].ThreadID, flipped[i].ID,
		); err != nil {
			return nil, fmt.Errorf("store: ghost-flip update %s: %w", flipped[i].ID, err)
		}
	}

	// Sweeping crash-recovery cleanup is not a meaningful interaction;
	// thread activity stays where the previous interaction left it.
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("store: commit ghost-flip tx: %w", err)
	}
	return flipped, nil
}

// LatestToolCallByName returns the most-recently-inserted tool_call row
// in (threadID, turnIndex) whose lower(tool_name) equals any of
// toolNames. Matches the iteration pattern in triage.findLatestToolCall
// but pushes the filter into SQLite so we don't deserialize every item
// in turns with a lot of tool calls. Returns (zero Item, false, nil)
// when no match exists.
//
// toolNames must be non-empty and are matched case-insensitively; the
// names are lowercased by the caller (to keep the SQL string short).
func (s *Store) LatestToolCallByName(threadID string, turnIndex int, toolNames []string) (Item, bool, error) {
	if len(toolNames) == 0 {
		return Item{}, false, nil
	}

	// Build a parametrized IN clause. SQLite has no native array type; we
	// use ? placeholders. Thread id + turn index stay as the final two
	// parameters so the SELECT works regardless of the tool-name slice
	// length. Performance-wise we rely on the items.thread_id +
	// turn_index covering index — the LIMIT 1 makes the scan minimal.
	placeholders := ""
	args := make([]any, 0, len(toolNames)+2)
	for i, name := range toolNames {
		if i > 0 {
			placeholders += ", "
		}
		placeholders += "?"
		args = append(args, name)
	}
	args = append(args, threadID, turnIndex)

	query := `SELECT ` + itemColumns + `
		   FROM items
		   LEFT JOIN payloads ON payloads.id = items.payload_id
		  WHERE items.kind = 'tool_call'
		    AND lower(items.tool_name) IN (` + placeholders + `)
		    AND items.thread_id = ? AND items.turn_index = ?
		  ORDER BY items.item_index DESC
		  LIMIT 1`

	row := s.db.QueryRow(query, args...)
	it, err := scanItemRow(row)
	if err != nil {
		if err == sql.ErrNoRows {
			return Item{}, false, nil
		}
		return Item{}, false, fmt.Errorf("store: latest tool_call thread %s turn %d: %w", threadID, turnIndex, err)
	}
	return it, true, nil
}

// DeleteConversationFromTurn removes items and turn rows with turn_index >=
// fromTurnIndex. Reverting to a user-message checkpoint deletes that selected
// prompt too, so the predicate is inclusive.
func (s *Store) DeleteConversationFromTurn(threadID string, fromTurnIndex int) (int, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return 0, fmt.Errorf("store: begin delete conversation from turn tx: %w", err)
	}
	defer tx.Rollback()

	result, err := tx.Exec(
		`DELETE FROM items WHERE thread_id = ? AND turn_index >= ?`,
		threadID, fromTurnIndex,
	)
	if err != nil {
		return 0, fmt.Errorf("store: delete items from turn for thread %s: %w", threadID, err)
	}
	n, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("store: delete items from turn rows affected: %w", err)
	}
	if _, err := tx.Exec(
		`DELETE FROM turns WHERE thread_id = ? AND turn_index >= ?`,
		threadID, fromTurnIndex,
	); err != nil {
		return 0, fmt.Errorf("store: delete turns from turn for thread %s: %w", threadID, err)
	}
	if _, err := tx.Exec(
		`DELETE FROM thread_tracked_files WHERE thread_id = ? AND turn_index >= ?`,
		threadID, fromTurnIndex,
	); err != nil {
		return 0, fmt.Errorf("store: delete tracked files from turn for thread %s: %w", threadID, err)
	}
	// Truncating the conversation is a structural change, not a fresh
	// interaction. The next user_text persist (or a turn settle that
	// follows the resume) bumps activity through MarkThreadActivity.
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("store: commit delete conversation from turn tx: %w", err)
	}
	return int(n), nil
}

// UpdateItemPayload updates a single item's payload link, summary, and
// timestamp. The parent thread's updated_at is NOT touched — payload
// upgrades are a row mutation, not a sidebar-worthy interaction. Triage
// bumps activity via MarkThreadActivity at the user_text /
// turn-settle / approval-request boundaries.
//
// Returns an error if the item does not exist or its thread has been
// deleted (caught by RowsAffected on the UPDATE), instead of silently
// succeeding on a no-op update.
func (s *Store) UpdateItemPayload(id, payloadID, summary string, createdAt int64) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("store: begin update item payload tx: %w", err)
	}
	defer tx.Rollback()

	result, err := tx.Exec(
		`UPDATE items SET payload_id = ?, summary = ?, updated_at = ? WHERE id = ?`,
		nilIfEmpty(payloadID), summary, createdAt, id,
	)
	if err != nil {
		return fmt.Errorf("store: update item payload %s: %w", id, err)
	}
	if err := requireRowsAffected(
		result,
		fmt.Sprintf("store: update item payload %s", id),
	); err != nil {
		return err
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit update item payload tx: %w", err)
	}
	return nil
}

// UpdateItemMeta rewrites only the `meta` column on a single item
// row, scoped to the owning thread. Used by the fork-time UUID remap
// in `app_thread_fork.go::remapForkedClaudeUUIDs` to refresh a
// cloned `user_text` row's `provider_item_id` after the source
// session JSONL is forked with fresh uuids. Distinct from
// `UpsertItem` because the remap is a back-fill on cloned data, not
// a wire event — it must not bump `updated_at`, must not run the
// payload upsert path, and must not emit a frontend `item:upsert`
// notification (no wire correlation occurred).
//
// Returns sql.ErrNoRows-wrapped error when (threadID, id) does not
// match any row so partial fork cleanups can detect drift before
// committing.
func (s *Store) UpdateItemMeta(threadID, id, meta string) error {
	result, err := s.db.Exec(
		`UPDATE items SET meta = ? WHERE thread_id = ? AND id = ?`,
		meta, threadID, id,
	)
	if err != nil {
		return fmt.Errorf("store: update item meta %s/%s: %w", threadID, id, err)
	}
	return requireRowsAffected(
		result,
		fmt.Sprintf("store: update item meta %s/%s", threadID, id),
	)
}

// UpdateItemStatus transitions an inline tool-call item from its current
// status (typically "running") to the supplied status, replaces its
// summary, and re-links (or clears) its payload_id. status must be one
// of the four values the v14 CHECK constraint allows; an invalid value
// surfaces as a SQLite CHECK error.
//
// Inline tool calls use this method to flip running → completed|errored
// without rewriting any other item. Background launches do NOT go through
// here — their completion is a NEW item appended via AppendCompletionItem,
// keeping the launch row frozen.
//
// Does NOT bump threads.updated_at — sidebar activity is owned by the
// turn-settle / interaction-point paths via MarkThreadActivity. Tool
// completions inside an active turn don't move the sidebar; the bump
// arrives when the turn itself settles.
//
// Returns sql.ErrNoRows (wrapped) if no item matches id.
func (s *Store) UpdateItemStatus(id, status, summary, payloadID string, createdAt int64) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("store: begin update item status tx: %w", err)
	}
	defer tx.Rollback()

	result, err := tx.Exec(
		`UPDATE items
		 SET status = ?, summary = ?, payload_id = ?, updated_at = ?
		 WHERE id = ?`,
		status, summary, nilIfEmpty(payloadID), createdAt, id,
	)
	if err != nil {
		return fmt.Errorf("store: update item status %s: %w", id, err)
	}
	if err := requireRowsAffected(
		result,
		fmt.Sprintf("store: update item status %s", id),
	); err != nil {
		return err
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit update item status tx: %w", err)
	}
	return nil
}

// AppendCompletionItem writes the second row of a backgrounded tool-call
// pair: the caller passes the launch row (already persisted, used only to
// stamp the new row's CompletionOf) and the completion row that
// should land next in the timeline. The completion row always lands with
// IsBackground=true and CompletionOf=launch.ID, overriding whatever
// the caller may have pre-set on those fields — that invariant is the
// whole point of this API.
//
// The item_index for the completion is computed as MAX(item_index)+1 over
// (thread, turn) inside the transaction so concurrent appends can't
// collide. Matching turn_index is the caller's responsibility: the
// completion typically lands on the turn in which the background work
// FINISHED, not the turn in which it launched.
//
// If completionPayload is non-nil it's inserted in the same transaction
// and its id is linked via the completion row's PayloadID, mirroring
// AppendItemWithPayload. Pass nil for payload-less completions.
//
// Returns the assigned item_index.
func (s *Store) AppendCompletionItem(launch Item, completion Item, completionPayload *Payload) (int, error) {
	applyItemDefaults(&completion)
	tx, err := s.db.Begin()
	if err != nil {
		return 0, fmt.Errorf("store: begin append completion item tx: %w", err)
	}
	defer tx.Rollback()

	completion.CompletionOf = launch.ID
	completion.IsBackground = true
	completion.ThreadID = launch.ThreadID

	next, err := nextItemIndexTx(tx, completion.ThreadID, completion.TurnIndex, "store: append completion next index")
	if err != nil {
		return 0, err
	}
	completion.ItemIndex = next

	if completionPayload != nil {
		if err := insertPayloadTx(tx, *completionPayload, "store: append completion payload"); err != nil {
			return 0, err
		}
		completion.PayloadID = completionPayload.ID
	}

	if err := insertItemTx(tx, completion, "store: append completion item insert"); err != nil {
		return 0, err
	}

	// Background-task completion rows are siblings to a running tool_call;
	// they do not represent a fresh interaction. Activity is bumped by
	// the turn-settle path through MarkThreadActivity.
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("store: commit append completion item tx: %w", err)
	}
	return next, nil
}
