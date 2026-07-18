package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"agent-overflow/internal/itemmeta"
)

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
	if err := upsertPayloadTx(tx, *payload, fmt.Sprintf("store: upsert item payload %s", payload.ID)); err != nil {
		return err
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
	if err := upsertPayloadTx(tx, *payload, fmt.Sprintf("store: upsert item input payload %s", payload.ID)); err != nil {
		return err
	}
	item.InputPayloadID = payload.ID
	return nil
}

// writeItem resolves whether `item` already exists on its thread and
// dispatches to the matching update/insert helper. The lookup query runs
// inside the same transaction so concurrent upserts can't both see
// "absent" and race to insert.
func writeItem(tx *sql.Tx, item *Item) error {
	return writeItemWithIndexFn(tx, item, nextItemIndexTx)
}

// writeItemWithIndexFn is writeItem with the new-row index allocator
// injected: nextItemIndexTx appends (the default), headItemIndexTx
// prepends (UpsertItemAtTurnHead). Existing rows update in place either
// way — placement only applies to the insert.
func writeItemWithIndexFn(tx *sql.Tx, item *Item, indexFn func(*sql.Tx, string, int, string) (int, error)) error {
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
		return insertNewItem(tx, item, indexFn)
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

// insertNewItem allocates the row's index through indexFn within the
// transaction to keep concurrent upserts from colliding on the same
// slot, then inserts the row. The computed ItemIndex is written back
// onto `item` so the re-read step (readBackUpsertedItem) returns the
// persisted value.
func insertNewItem(tx *sql.Tx, item *Item, indexFn func(*sql.Tx, string, int, string) (int, error)) error {
	next, err := indexFn(tx, item.ThreadID, item.TurnIndex, "store: upsert item next index")
	if err != nil {
		return err
	}
	item.ItemIndex = next
	if err := insertItemWithIDTx(tx, *item, "store: insert item"); err != nil {
		return err
	}
	return nil
}

// UpsertItemAtTurnHead is UpsertItem with HEAD placement for a new row:
// a missing row inserts at MIN(item_index)-1 for its turn (0 when the
// turn is empty — identical to the append path there); an existing row
// updates in place with its index preserved, exactly like UpsertItem.
// For a row that owns the FIRST slot of its turn — a deferred flush
// prompt whose turn was empty at its first echo — this makes the
// persist retryable after a failure: response rows that took 0..n
// while the prompt's first persist failed no longer push a MAX+1 retry
// below the prompt's own response (round-7, R7-4). The caller decides
// turn ownership; rows steered into an occupied turn must append. No
// payload variant: the deferred-prompt path persists bare user rows.
func (s *Store) UpsertItemAtTurnHead(item Item) (Item, error) {
	applyItemDefaults(&item)
	tx, err := s.db.Begin()
	if err != nil {
		return Item{}, fmt.Errorf("store: begin upsert item at head tx: %w", err)
	}
	defer tx.Rollback()

	if err := writeItemWithIndexFn(tx, &item, headItemIndexTx); err != nil {
		return Item{}, err
	}
	persisted, err := readBackUpsertedItem(tx, item.ThreadID, item.ID)
	if err != nil {
		return Item{}, err
	}
	if err := tx.Commit(); err != nil {
		return Item{}, fmt.Errorf("store: commit upsert item at head tx: %w", err)
	}
	return persisted, nil
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

// BumpItemToTurnEnd atomically moves an item to MAX(item_index)+1 for
// its turn, placing it after all items currently at that turn, and —
// when transformMeta is non-nil — rewrites the row's meta through it in
// the SAME transaction (reading the current meta inside the tx, so a
// concurrent meta merge on the row is never lost). Returns the updated
// item. Used by the interrupt promote (bump + promotion marker) and the
// echo-time flush reposition (bump + provider_item_id stamp): both
// pairings are load-bearing together, and committing the bump without
// its meta would leave truncation predicates reading a repositioned row
// with stale ordering metadata. updatedAt stamps the mutation time.
func (s *Store) BumpItemToTurnEnd(threadID, itemID string, transformMeta func(string) (string, error), updatedAt int64) (Item, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return Item{}, fmt.Errorf("store: begin bump item index tx: %w", err)
	}
	defer tx.Rollback()

	var turnIndex int
	var meta string
	if err := tx.QueryRow(
		`SELECT turn_index, meta FROM items WHERE thread_id = ? AND id = ?`,
		threadID, itemID,
	).Scan(&turnIndex, &meta); err != nil {
		return Item{}, fmt.Errorf("store: bump item index lookup %s: %w", itemID, err)
	}

	next, err := nextItemIndexTx(tx, threadID, turnIndex, "store: bump item index")
	if err != nil {
		return Item{}, err
	}
	if transformMeta != nil {
		if meta, err = transformMeta(meta); err != nil {
			return Item{}, fmt.Errorf("store: bump item index transform meta %s: %w", itemID, err)
		}
	}
	if _, err := tx.Exec(
		`UPDATE items SET item_index = ?, meta = ?, updated_at = ? WHERE thread_id = ? AND id = ?`,
		next, meta, updatedAt, threadID, itemID,
	); err != nil {
		return Item{}, fmt.Errorf("store: bump item index update %s: %w", itemID, err)
	}

	item, err := readBackItemTx(tx, threadID, itemID)
	if err != nil {
		return Item{}, fmt.Errorf("store: bump item index re-read %s: %w", itemID, err)
	}
	if err := tx.Commit(); err != nil {
		return Item{}, fmt.Errorf("store: commit bump item index tx: %w", err)
	}
	return item, nil
}

// UpdateItemMetaMerge atomically rewrites a row's meta through
// transform inside one transaction: the current meta is read under the
// tx, transformed, and written back, so two concurrent merges (the
// interrupt promote and the wire-echo stamp run on different
// goroutines) compose instead of one overwriting the other. Returns the
// updated row and whether the meta actually changed — callers use the
// flag to skip redundant frontend emissions on duplicate echoes.
// updated_at is stamped only when the meta changed.
func (s *Store) UpdateItemMetaMerge(threadID, id string, transform func(string) (string, error), updatedAt int64) (Item, bool, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return Item{}, false, fmt.Errorf("store: begin meta merge tx: %w", err)
	}
	defer tx.Rollback()

	var meta string
	if err := tx.QueryRow(
		`SELECT meta FROM items WHERE thread_id = ? AND id = ?`,
		threadID, id,
	).Scan(&meta); err != nil {
		return Item{}, false, fmt.Errorf("store: meta merge lookup %s/%s: %w", threadID, id, err)
	}
	merged, err := transform(meta)
	if err != nil {
		return Item{}, false, fmt.Errorf("store: meta merge transform %s/%s: %w", threadID, id, err)
	}
	changed := merged != meta
	if changed {
		if _, err := tx.Exec(
			`UPDATE items SET meta = ?, updated_at = ? WHERE thread_id = ? AND id = ?`,
			merged, updatedAt, threadID, id,
		); err != nil {
			return Item{}, false, fmt.Errorf("store: meta merge update %s/%s: %w", threadID, id, err)
		}
	}
	item, err := readBackItemTx(tx, threadID, id)
	if err != nil {
		return Item{}, false, fmt.Errorf("store: meta merge re-read %s/%s: %w", threadID, id, err)
	}
	if err := tx.Commit(); err != nil {
		return Item{}, false, fmt.Errorf("store: commit meta merge tx: %w", err)
	}
	return item, changed, nil
}

// DeleteThreadItem removes one item scoped by thread and id. Intended for
// rows that were reserved internally but never became visible history, such as
// quietly-persisted queued flush rows whose provider session died before echo.
func (s *Store) DeleteThreadItem(threadID, itemID string) error {
	result, err := s.db.Exec(
		`DELETE FROM items WHERE thread_id = ? AND id = ?`,
		threadID, itemID,
	)
	if err != nil {
		return fmt.Errorf("store: delete item %s/%s: %w", threadID, itemID, err)
	}
	return requireRowsAffected(
		result,
		fmt.Sprintf("store: delete item %s/%s", threadID, itemID),
	)
}

// DeleteConversationFromTurn removes items, turn rows, tracked files,
// and message checkpoints with turn_index >= fromTurnIndex, returning
// the git refs of the deleted checkpoints so the caller can drop them
// from the workspace. Reverting to a user-message checkpoint deletes
// that selected prompt too, so the predicate is inclusive.
//
// Everything runs in ONE transaction. The old shape — a separate
// checkpoint delete committed before this call — could succeed and
// then have the conversation delete fail, stranding timeline rows
// whose checkpoints (the revert/fork anchors and the retry key) were
// already gone; a retry could no longer resolve the revert point
// (round-5 review, R5-5). A failure now rolls back both.
func (s *Store) DeleteConversationFromTurn(threadID string, fromTurnIndex int) ([]CheckpointRef, int, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, 0, fmt.Errorf("store: begin delete conversation from turn tx: %w", err)
	}
	defer tx.Rollback()

	refs, err := checkpointRefsFromTurn(tx, threadID, fromTurnIndex)
	if err != nil {
		return nil, 0, err
	}
	// Explicit, though the items delete below cascades the checkpoint
	// rows via their items FK: the turn_index predicate also covers a
	// checkpoint whose recorded turn drifted from its user item's.
	if _, err := tx.Exec(
		`DELETE FROM thread_checkpoints WHERE thread_id = ? AND turn_index >= ?`,
		threadID, fromTurnIndex,
	); err != nil {
		return nil, 0, fmt.Errorf("store: delete checkpoints from turn for thread %s: %w", threadID, err)
	}
	result, err := tx.Exec(
		`DELETE FROM items WHERE thread_id = ? AND turn_index >= ?`,
		threadID, fromTurnIndex,
	)
	if err != nil {
		return nil, 0, fmt.Errorf("store: delete items from turn for thread %s: %w", threadID, err)
	}
	n, err := result.RowsAffected()
	if err != nil {
		return nil, 0, fmt.Errorf("store: delete items from turn rows affected: %w", err)
	}
	if _, err := tx.Exec(
		`DELETE FROM turns WHERE thread_id = ? AND turn_index >= ?`,
		threadID, fromTurnIndex,
	); err != nil {
		return nil, 0, fmt.Errorf("store: delete turns from turn for thread %s: %w", threadID, err)
	}
	if _, err := tx.Exec(
		`DELETE FROM thread_tracked_files WHERE thread_id = ? AND turn_index >= ?`,
		threadID, fromTurnIndex,
	); err != nil {
		return nil, 0, fmt.Errorf("store: delete tracked files from turn for thread %s: %w", threadID, err)
	}
	// Truncating the conversation is a structural change, not a fresh
	// interaction. The next user_text persist (or a turn settle that
	// follows the resume) bumps activity through MarkThreadActivity.
	if err := tx.Commit(); err != nil {
		return nil, 0, fmt.Errorf("store: commit delete conversation from turn tx: %w", err)
	}
	return refs, int(n), nil
}

// DeleteConversationFromItem removes the anchor item and everything after it
// in PROVIDER order, plus the checkpoints of deleted user rows and the
// turns / tracked-file rows of turns left without any items. Returns the git
// refs of the deleted checkpoints so the caller can drop them from the
// workspace.
//
// Provider order is timeline order — (turn_index, item_index) — for every row
// except interrupt-promoted queued messages (itemmeta promotion marker):
// those were bumped over their turn's not-yet-persisted tail, so their
// same-turn NON-USER successors precede them in the provider transcript and
// survive the cut; same-turn user successors are later-queued messages and go.
// When the promoted row's echo stamped a provider-order boundary (the CLI
// consumed it mid-loop and its response persisted in the same turn), non-user
// successors PAST the boundary are that response — provider-order AFTER the
// message — and are deleted with it. Whenever the cut removes same-turn
// non-user content, the surviving turn row's settle metadata described that
// deleted content — completed_at is trimmed back to the last surviving row
// and the assistant_message_id cleared; token usage stays, the spend was real
// and the ledger already has it.
//
// This is the item-granular twin of DeleteConversationFromTurn, for providers
// whose conversation revert cuts at the message itself (Claude's session-file
// slice anchors on the message uuid). Queued flush messages can share a turn
// with the prompt that was running when they were enqueued; deleting the whole
// turn would take that original prompt — and the agent work before the queued
// message — down with them. When the anchor opens its turn the predicate
// degenerates to DeleteConversationFromTurn's. Codex reverts keep the
// turn-granular delete: thread/fork cuts provider history at a turn boundary,
// and SQLite must match it.
func (s *Store) DeleteConversationFromItem(threadID, itemID string) ([]CheckpointRef, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, fmt.Errorf("store: begin delete conversation from item tx: %w", err)
	}
	defer tx.Rollback()

	var turnIndex, itemIndex int
	var meta string
	if err := tx.QueryRow(
		`SELECT turn_index, item_index, meta FROM items WHERE thread_id = ? AND id = ?`,
		threadID, itemID,
	).Scan(&turnIndex, &itemIndex, &meta); err != nil {
		return nil, fmt.Errorf("store: delete conversation from item lookup %s/%s: %w", threadID, itemID, err)
	}
	promotion, err := itemmeta.DecodePromotionState(meta)
	if err != nil {
		// Corrupt anchor meta means the provider-order cut is undecidable;
		// failing beats silently degrading to a display-order cut that the
		// session slice would disagree with.
		return nil, fmt.Errorf("store: delete conversation from item %s/%s: %w", threadID, itemID, err)
	}

	// Checkpoints anchored on any item being deleted, derived from the
	// items themselves rather than the checkpoint's cached turn_index —
	// the FK cascade follows the item delete, so ref collection must
	// track the item cut exactly or a checkpoint whose cached turn
	// drifted from its item's actual position cascades away with its
	// git ref never returned. The subquery over-matches checkpoint-less
	// rows (non-user, parented, promoted-cut survivors) harmlessly:
	// among checkpoint-bearing rows (top-level user), position >= the
	// anchor means deleted in both the plain and promoted-anchor item
	// predicates below. Must run before the item delete removes the rows.
	const checkpointPredicate = `thread_id = ?
		 AND user_item_id IN (SELECT id FROM items
		                      WHERE thread_id = ?
		                        AND (turn_index > ?
		                             OR (turn_index = ? AND item_index >= ?)))`
	rows, err := tx.Query(
		`SELECT ref_name, workspace_path FROM thread_checkpoints WHERE `+checkpointPredicate+
			` ORDER BY turn_index`,
		threadID, threadID, turnIndex, turnIndex, itemIndex,
	)
	if err != nil {
		return nil, fmt.Errorf("store: list checkpoints from item for thread %s: %w", threadID, err)
	}
	var refs []CheckpointRef
	for rows.Next() {
		var ref CheckpointRef
		if err := rows.Scan(&ref.RefName, &ref.WorkspacePath); err != nil {
			rows.Close()
			return nil, fmt.Errorf("store: scan checkpoint ref from item: %w", err)
		}
		refs = append(refs, ref)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, fmt.Errorf("store: iterate checkpoints from item: %w", err)
	}
	rows.Close()
	if _, err := tx.Exec(
		`DELETE FROM thread_checkpoints WHERE `+checkpointPredicate,
		threadID, threadID, turnIndex, turnIndex, itemIndex,
	); err != nil {
		return nil, fmt.Errorf("store: delete checkpoints from item for thread %s: %w", threadID, err)
	}

	// deletedTurnContent: does this cut remove same-turn NON-USER rows?
	// Those are the rows the anchor turn's settle metadata describes
	// (streamed content, the response), so their deletion triggers the
	// trim below. Computed BEFORE the delete removes the evidence.
	contentPredicate := ""
	contentArgs := []any{}
	itemPredicate := `turn_index > ? OR (turn_index = ? AND item_index >= ?)`
	itemArgs := []any{threadID, turnIndex, turnIndex, itemIndex}
	if promotion.Promoted {
		// Same-turn successors up to the echo boundary that are not
		// top-level user rows — streamed assistant/tool content AND
		// parented wire-only user rows (subagent prompts nested under
		// their launching tool_call) — are the interrupted round's tail:
		// they precede the promoted message in the provider transcript
		// and stay. Same-turn TOP-LEVEL user successors are later-promoted
		// queued rows; only those carry checkpoints, so the checkpoint
		// predicate above already matched exactly the user rows this
		// delete removes. Past the boundary (stamped when the CLI consumed
		// the message mid-loop), everything else is the response —
		// provider-order AFTER the message — and goes with it.
		itemPredicate = `turn_index > ? OR (turn_index = ? AND item_index >= ? AND role = 'user' AND parent_id = '')`
		if promotion.HasEchoBoundary {
			itemPredicate += ` OR (turn_index = ? AND item_index > ? AND (role != 'user' OR parent_id != ''))`
			itemArgs = append(itemArgs, turnIndex, promotion.EchoBoundary)
			contentPredicate = `item_index > ?`
			contentArgs = []any{threadID, turnIndex, promotion.EchoBoundary}
		}
	} else {
		contentPredicate = `item_index > ?`
		contentArgs = []any{threadID, turnIndex, itemIndex}
	}
	deletedTurnContent := false
	if contentPredicate != "" {
		if err := tx.QueryRow(
			`SELECT EXISTS(SELECT 1 FROM items
			  WHERE thread_id = ? AND turn_index = ? AND (role != 'user' OR parent_id != '') AND `+contentPredicate+`)`,
			contentArgs...,
		).Scan(&deletedTurnContent); err != nil {
			return nil, fmt.Errorf("store: probe deleted turn content for thread %s: %w", threadID, err)
		}
	}
	if _, err := tx.Exec(
		`DELETE FROM items WHERE thread_id = ? AND (`+itemPredicate+`)`,
		itemArgs...,
	); err != nil {
		return nil, fmt.Errorf("store: delete items from item for thread %s: %w", threadID, err)
	}

	// The anchor turn keeps its turns / tracked-file rows while any items
	// survive in it: the remaining prefix still happened, and its tracked
	// paths must stay visible to a later files-revert of the whole turn.
	for _, table := range []string{"turns", "thread_tracked_files"} {
		if _, err := tx.Exec(
			`DELETE FROM `+table+` WHERE thread_id = ?
			 AND turn_index >= ?
			 AND NOT EXISTS (SELECT 1 FROM items
			                 WHERE thread_id = ? AND turn_index = `+table+`.turn_index)`,
			threadID, turnIndex, threadID,
		); err != nil {
			return nil, fmt.Errorf("store: delete %s from item for thread %s: %w", table, threadID, err)
		}
	}

	// A surviving anchor turn that just lost streamed content ends at its
	// last surviving row, not at the settle the deleted response produced:
	// trim completed_at back (never forward — MIN) and drop the
	// assistant_message_id that now points at a deleted message. Gated on
	// deletedTurnContent, NOT on the anchor being mid-turn: an anchor that
	// is the LAST row of its turn (an at-pickup bumped quiet flush) deletes
	// nothing the settle metadata describes, and rewriting it would corrupt
	// accurate history. The completed_at IS NOT NULL guard leaves a
	// (guard-violating) active turn alone rather than fabricating a
	// settlement.
	if deletedTurnContent {
		if err := trimTurnSettleToSurvivorsTx(tx, threadID, turnIndex); err != nil {
			return nil, err
		}
	}

	// Like DeleteConversationFromTurn: truncation is a structural change,
	// not a fresh interaction — no MarkThreadActivity bump.
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("store: commit delete conversation from item tx: %w", err)
	}
	return refs, nil
}

// trimTurnSettleToSurvivorsTx rewrites a surviving turn row whose settle
// metadata described just-deleted content: completed_at trims back to the
// last surviving row's created_at (bumped anchors carry dispatch-time
// created_at OLDER than the kept tail, so the anchor is the wrong target)
// and assistant_message_id clears — the message it referenced is gone.
// No-op when the turn kept no rows (its turn row was already deleted) or
// was never settled.
func trimTurnSettleToSurvivorsTx(tx *sql.Tx, threadID string, turnIndex int) error {
	var lastKept sql.NullInt64
	if err := tx.QueryRow(
		`SELECT MAX(created_at) FROM items WHERE thread_id = ? AND turn_index = ?`,
		threadID, turnIndex,
	).Scan(&lastKept); err != nil {
		return fmt.Errorf("store: trim turn settle survivors lookup for thread %s: %w", threadID, err)
	}
	if !lastKept.Valid {
		return nil
	}
	if _, err := tx.Exec(
		`UPDATE turns SET completed_at = MIN(completed_at, ?), assistant_message_id = ''
		 WHERE thread_id = ? AND turn_index = ? AND completed_at IS NOT NULL`,
		lastKept.Int64, threadID, turnIndex,
	); err != nil {
		return fmt.Errorf("store: trim anchor turn settle for thread %s: %w", threadID, err)
	}
	return nil
}

// UpdateItemMeta rewrites only the `meta` column on a single item
// row, scoped to the owning thread. Used by the fork-time UUID remap
// in `app_thread_fork.go::remapClaudeProviderIDs` to refresh a
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

// ItemPartialUpdate describes a subset of mutable Item fields for a targeted
// UPDATE. Non-nil pointer fields are written; nil fields are left unchanged.
type ItemPartialUpdate struct {
	Status    *string
	Summary   *string
	Meta      *string
	Decision  *string
	UpdatedAt *int64
}

// UpdateItemFields writes only the non-nil fields from update onto the
// existing row identified by (threadID, id). Returns an error if the
// row does not exist or no fields were specified.
func (s *Store) UpdateItemFields(threadID, id string, update ItemPartialUpdate) error {
	setClauses := make([]string, 0, 5)
	args := make([]any, 0, 7)
	if update.Status != nil {
		setClauses = append(setClauses, "status = ?")
		args = append(args, *update.Status)
	}
	if update.Summary != nil {
		setClauses = append(setClauses, "summary = ?")
		args = append(args, *update.Summary)
	}
	if update.Meta != nil {
		setClauses = append(setClauses, "meta = ?")
		args = append(args, *update.Meta)
	}
	if update.Decision != nil {
		setClauses = append(setClauses, "decision = ?")
		args = append(args, *update.Decision)
	}
	if update.UpdatedAt != nil {
		setClauses = append(setClauses, "updated_at = ?")
		args = append(args, *update.UpdatedAt)
	}
	if len(setClauses) == 0 {
		return fmt.Errorf("store: update item fields %s/%s: no fields specified", threadID, id)
	}
	args = append(args, threadID, id)
	query := "UPDATE items SET " + strings.Join(setClauses, ", ") + " WHERE thread_id = ? AND id = ?"
	result, err := s.db.Exec(query, args...)
	if err != nil {
		return fmt.Errorf("store: update item fields %s/%s: %w", threadID, id, err)
	}
	return requireRowsAffected(
		result,
		fmt.Sprintf("store: update item fields %s/%s", threadID, id),
	)
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
