package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strconv"
)

// FindStreamItemByProviderItemID resolves a streamed assistant row from the
// provider's item id stored in items.meta. It is intentionally a narrow
// fallback lookup for late completion events; the hot delta path keeps the
// in-memory item id and never pays this JSON predicate.
func (s *Store) FindStreamItemByProviderItemID(threadID string, turnIndex int, kind, parentID, providerItemID string) (Item, bool, error) {
	item, found, err := queryOneHydratedTimelineItem(
		s.reader(), threadID,
		`SELECT id FROM timeline_items
		  WHERE thread_id = ?
		    AND turn_index = ?
		    AND kind = ?
		    AND parent_id = ?
		    AND json_extract(meta, '$.provider_item_id') = ?
		  ORDER BY item_index ASC
		  LIMIT 1`,
		threadID, turnIndex, kind, parentID, providerItemID,
	)
	if err != nil {
		return Item{}, false, fmt.Errorf("store: find stream item by provider item id: %w", err)
	}
	return item, found, nil
}

func (s *Store) ListItems(threadID string) ([]Item, error) {
	items, err := queryHydratedTimelineItems(
		s.reader(), threadID,
		`SELECT id FROM timeline_items WHERE thread_id = ?`,
		threadID,
	)
	if err != nil {
		return nil, fmt.Errorf("store: list items for thread %s: %w", threadID, err)
	}
	return items, nil
}

func (s *Store) ListItemsForTurn(threadID string, turnIndex int) ([]Item, error) {
	items, err := queryHydratedTimelineItems(
		s.reader(), threadID,
		`SELECT id FROM timeline_items WHERE thread_id = ? AND turn_index = ?`,
		threadID, turnIndex,
	)
	if err != nil {
		return nil, fmt.Errorf("store: list items for thread %s turn %d: %w", threadID, turnIndex, err)
	}
	return items, nil
}

func (s *Store) LastTurnIndex(threadID string) (int, error) {
	var maxIndex sql.NullInt64
	err := s.reader().QueryRow(
		`SELECT MAX(turn_index)
		   FROM (
		         SELECT turn_index FROM timeline_items WHERE thread_id = ?
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

func (s *Store) FindTurnItem(threadID string, turnIndex int, kind string) (Item, bool, error) {
	item, found, err := queryOneHydratedTimelineItem(
		s.reader(), threadID,
		`SELECT id FROM timeline_items
		  WHERE thread_id = ? AND turn_index = ? AND kind = ?
		  ORDER BY item_index DESC
		  LIMIT 1`,
		threadID, turnIndex, kind,
	)
	if err != nil {
		return Item{}, false, fmt.Errorf("store: find turn item: %w", err)
	}
	return item, found, nil
}

// FindToolCallItemByTaskID resolves a thread's tool_call row whose persisted
// items.meta JSON carries a top-level task_id matching taskID. Used by the
// background completion router when a Claude task_updated/task_notification
// event arrives without an inline tool_use_id — most commonly after a
// reconnect with a fresh parser, when the adapter's in-memory
// task_id ↔ tool_use_id map has been dropped.
//
// The query is O(log N) thanks to the partial expression index
// idx_items_meta_task_id which materialises
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
	item, found, err := queryOneHydratedTimelineItem(
		s.reader(), threadID,
		`SELECT id FROM timeline_items
		  WHERE thread_id = ?
		    AND json_extract(meta, '$.task_id') = ?
		  ORDER BY updated_at DESC
		  LIMIT 1`,
		threadID, taskID,
	)
	if err != nil {
		return Item{}, false, fmt.Errorf("store: find tool call by task id %s: %w", taskID, err)
	}
	return item, found, nil
}

// GetThreadItemByPayloadID returns the newest item on threadID whose
// payload_id OR input_payload_id matches payloadID, so a payload id is
// not usable outside the thread that references it. The two partial
// indexes (idx_items_payload_id, idx_items_input_payload_id) cover the
// two columns; UNION ALL keeps each branch index-friendly. A single
// OR-clause forces SQLite onto the broad thread_id index instead, which
// would scan every row in the thread on every lazy-load click.
func (s *Store) GetThreadItemByPayloadID(threadID, payloadID string) (Item, bool, error) {
	item, found, err := queryOneHydratedTimelineItem(
		s.reader(), threadID,
		`SELECT id FROM (
		     SELECT id, updated_at FROM timeline_items
		      WHERE thread_id = ? AND payload_id = ?
		     UNION
		     SELECT id, updated_at FROM timeline_items
		      WHERE thread_id = ? AND input_payload_id = ?
		 )
		 ORDER BY updated_at DESC
		 LIMIT 1`,
		threadID, payloadID, threadID, payloadID,
	)
	if err != nil {
		return Item{}, false, fmt.Errorf("store: get item by payload id %s on thread %s: %w", payloadID, threadID, err)
	}
	return item, found, nil
}

// readerAuthoredUserTextFilter matches the user_text rows the reader
// actually typed: top-level (subagent child prompts excluded) and not
// wire-only (context injections the send path marks in meta). It is the
// SQL counterpart of the frontend's `isReaderAuthoredUserText`; the
// json_valid guard keeps one corrupt meta blob from failing the whole
// read (the lifecycle queries guard the same way).
const readerAuthoredUserTextFilter = topLevelItemsFilter +
	` AND kind = 'user_text'
	  AND COALESCE(CASE WHEN json_valid(meta) THEN json_extract(meta, '$.wire_only') END, 0) != 1`

// UserMessageTick is one nav-rail tick: a reader-authored user message's
// id plus its position, small enough that a whole thread's list ships in
// one read. The position pair is what lets the frontend splice the
// loaded window's live-derived ticks over the store's baseline.
type UserMessageTick struct {
	ID        string `json:"id"`
	TurnIndex int    `json:"turnIndex"`
	ItemIndex int    `json:"itemIndex"`
}

// ListThreadUserMessageTicks returns every reader-authored user message
// in the thread, oldest first. Backs the message-nav rail, whose ticks
// cover the WHOLE thread rather than the loaded window — three tiny
// columns per row, so even a very long thread's list is a few KB. One
// sorted pass over the thread's user_text rows (timeline_items is a
// UNION ALL view, so the ORDER BY sorts rather than walking an index) —
// a per-thread-switch read, not a hot path.
func (s *Store) ListThreadUserMessageTicks(threadID string) ([]UserMessageTick, error) {
	rows, err := s.reader().Query(
		`SELECT id, turn_index, item_index FROM timeline_items
		  WHERE thread_id = ?
		    AND `+readerAuthoredUserTextFilter+`
		  ORDER BY turn_index ASC, item_index ASC`,
		threadID,
	)
	if err != nil {
		return nil, fmt.Errorf("store: list user message ticks on thread %s: %w", threadID, err)
	}
	defer rows.Close()
	ticks := []UserMessageTick{}
	for rows.Next() {
		var t UserMessageTick
		if err := rows.Scan(&t.ID, &t.TurnIndex, &t.ItemIndex); err != nil {
			return nil, fmt.Errorf("store: scan user message tick on thread %s: %w", threadID, err)
		}
		ticks = append(ticks, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list user message ticks on thread %s: %w", threadID, err)
	}
	return ticks, nil
}

// TurnPreview is the nav rail's hover card for one turn: the reader's
// ask and the turn's final top-level assistant reply.
type TurnPreview struct {
	UserText      string `json:"userText"`
	AssistantText string `json:"assistantText"`
}

// turnPreviewMaxRunes bounds each preview half at the wire. The card
// renders ~400 characters after whitespace collapse; shipping a giant
// message's full body for a hover would be pure waste.
const turnPreviewMaxRunes = 1000

// turnPreviewScanLimit bounds the walk below one turn. A turn with more
// top-level text rows than this is pathological; the preview then
// reflects the first rows, which is still an honest hover hint.
const turnPreviewScanLimit = 400

// ThreadTurnPreview resolves the hover-card content for the turn a
// reader-authored user message opens. The assistant half is the LAST
// top-level assistant_text before the next reader-authored user message
// — how the turn ended — matching the frontend's `turnPreview` walk over
// loaded items, so a loaded and an unloaded tick hover read the same.
// found=false when the item is not a reader-authored user message on
// this thread.
func (s *Store) ThreadTurnPreview(threadID, itemID string) (TurnPreview, bool, error) {
	var userText string
	var turnIndex, itemIndex int
	err := s.reader().QueryRow(
		`SELECT summary, turn_index, item_index FROM timeline_items
		  WHERE thread_id = ? AND id = ?
		    AND `+readerAuthoredUserTextFilter,
		threadID, itemID,
	).Scan(&userText, &turnIndex, &itemIndex)
	if errors.Is(err, sql.ErrNoRows) {
		return TurnPreview{}, false, nil
	}
	if err != nil {
		return TurnPreview{}, false, fmt.Errorf("store: turn preview anchor %s on thread %s: %w", itemID, threadID, err)
	}
	rows, err := s.reader().Query(
		`SELECT kind, summary,
		        COALESCE(CASE WHEN json_valid(meta) THEN json_extract(meta, '$.wire_only') END, 0)
		   FROM timeline_items
		  WHERE thread_id = ?
		    AND `+topLevelItemsFilter+`
		    AND kind IN ('user_text', 'assistant_text')
		    AND (turn_index > ? OR (turn_index = ? AND item_index > ?))
		  ORDER BY turn_index ASC, item_index ASC
		  LIMIT ?`,
		threadID, turnIndex, turnIndex, itemIndex, turnPreviewScanLimit,
	)
	if err != nil {
		return TurnPreview{}, false, fmt.Errorf("store: turn preview walk after %s on thread %s: %w", itemID, threadID, err)
	}
	defer rows.Close()
	assistantText := ""
	for rows.Next() {
		var kind, summary string
		var wireOnly int
		if err := rows.Scan(&kind, &summary, &wireOnly); err != nil {
			return TurnPreview{}, false, fmt.Errorf("store: scan turn preview row on thread %s: %w", threadID, err)
		}
		if kind == "user_text" {
			// A wire-only injection mid-turn is context, not the next
			// ask — same rule as the tick predicate above.
			if wireOnly == 1 {
				continue
			}
			break
		}
		if summary != "" {
			assistantText = summary
		}
	}
	if err := rows.Err(); err != nil {
		return TurnPreview{}, false, fmt.Errorf("store: turn preview walk after %s on thread %s: %w", itemID, threadID, err)
	}
	return TurnPreview{
		UserText:      capRunes(userText, turnPreviewMaxRunes),
		AssistantText: capRunes(assistantText, turnPreviewMaxRunes),
	}, true, nil
}

// capRunes truncates on a rune boundary with an ellipsis marker. Wire
// bound only — display truncation is the frontend's.
func capRunes(s string, maxRunes int) string {
	runes := []rune(s)
	if len(runes) <= maxRunes {
		return s
	}
	return string(runes[:maxRunes]) + "…"
}

func (s *Store) GetThreadItem(threadID, id string) (Item, bool, error) {
	return s.getThreadItem(s.reader(), threadID, id)
}

// getThreadItem is GetThreadItem against a caller-chosen queryer, so a
// window read that must be attested by stamps from the same transaction
// can resolve its anchor inside that transaction too.
func (s *Store) getThreadItem(q sqlQueryer, threadID, id string) (Item, bool, error) {
	item, found, err := queryOneHydratedTimelineItem(
		q, threadID,
		`SELECT id FROM timeline_items WHERE thread_id = ? AND id = ?`,
		threadID, id,
	)
	if err != nil {
		return Item{}, false, fmt.Errorf("store: get item %s on thread %s: %w", id, threadID, err)
	}
	return item, found, nil
}

// FindNotificationItemByTaskID returns the newest notification row whose
// meta.task_id matches taskID. Claude task_notification rows use this to let
// later task terminals attach the durable output_file payload without treating
// the notification itself as lifecycle state.
func (s *Store) FindNotificationItemByTaskID(threadID, taskID string) (Item, bool, error) {
	if taskID == "" {
		return Item{}, false, nil
	}
	// INDEXED BY: without stats the planner walks the thread's ordering
	// index newest-first probing meta per row instead of using the narrow
	// partial expression index (13ms vs 0.04ms on a 38k-item thread).
	item, found, err := queryOneHydratedTimelineItem(
		s.reader(), threadID,
		`SELECT id FROM timeline_items
		  WHERE thread_id = ?
		    AND kind = 'notification'
		    AND json_extract(meta, '$.task_id') = ?
		  ORDER BY turn_index DESC, item_index DESC
		  LIMIT 1`,
		threadID, taskID,
	)
	if err != nil {
		return Item{}, false, fmt.Errorf("store: find notification by task_id %s: %w", taskID, err)
	}
	return item, found, nil
}

func (s *Store) ListTurnItems(threadID string, turnIndex int) ([]Item, error) {
	items, err := queryHydratedTimelineItems(
		s.reader(), threadID,
		`SELECT id FROM timeline_items WHERE thread_id = ? AND turn_index = ?`,
		threadID, turnIndex,
	)
	if err != nil {
		return nil, fmt.Errorf("store: list turn items for thread %s turn %d: %w", threadID, turnIndex, err)
	}
	return items, nil
}

// ListTurnItemsSansPayload is a lighter sibling of ListTurnItems that
// skips the payloads LEFT JOIN. Use it on paths that read only the
// item-table columns (status, summary, kind, role, is_background) —
// the force-close safety net and the truncated-turn flip loop both
// qualify. For any caller that inspects PayloadKind / PayloadMeta
// (e.g. tool_result_diff_upgrade.loadSummaryOnlyToolResultCandidate)
// keep ListTurnItems, which hydrates them.
func (s *Store) ListTurnItemsSansPayload(threadID string, turnIndex int) ([]Item, error) {
	rows, err := s.reader().Query(
		`SELECT `+itemColumnsSansPayload+`
		   FROM timeline_items AS items
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

// threadTitleContextSummaryTail / threadTitleContextSummaryHead bound
// the TEXT the two statements below hydrate, so a thread of 200 rows ×
// 100KB summaries cannot pull tens of megabytes through the read pool
// to render an 8k prompt.
//
// Both are character counts (SQLite's `substr` counts characters, so a
// slice of N characters is at least N bytes) measured against the
// formatter's BYTE budgets, which is what makes them safe over-reads
// rather than silent truncation:
//   - the window rows keep their TAIL, because the formatter windows
//     newest-first inside an 8_000-byte budget, so the last 8192
//     characters always cover everything it can reach;
//   - the earliest-user row keeps its HEAD, because the pin keeps a
//     message's PREFIX under a 2_000-byte cap.
const (
	threadTitleContextSummaryTail = 8192
	threadTitleContextSummaryHead = 2048
)

// ThreadTitleContextItems returns the conversation rows a thread-title
// regeneration reads: top-level (`parent_id = ''`) `user_text` and
// `assistant_text` items, oldest-first, hydrated WITHOUT the payload
// join — Summary carries the text for both kinds, and the caller
// renders nothing else.
//
// The read is bounded in BOTH dimensions. Rows: the window is the
// NEWEST `limit`, because where a thread ended up is what a re-title is
// asking about, and the thread's EARLIEST top-level user row is added
// back when it fell outside that window (a long thread whose first ask
// scrolled out would be re-titled after its latest tangent). Bytes:
// every summary is sliced in SQL to the span the formatter can reach,
// and only USER rows carry their meta — attachment names are the one
// thing read out of it, while an assistant row's meta can carry large
// derived blobs.
//
// The second return reports whether the row window EXCLUDED at least
// one matching row. The formatter needs it: a 201-message thread of
// short messages fits the character budget whole, and rendering it as a
// seamless transcript would tell the model nothing was dropped.
//
// A non-positive limit returns no rows.
func (s *Store) ThreadTitleContextItems(threadID string, limit int) ([]Item, bool, error) {
	if limit <= 0 {
		return nil, false, nil
	}
	// One read-pool transaction for both statements: under WAL a read
	// transaction pins its snapshot at the first statement, so the window
	// and the earliest-user row describe one instant. Two reads could
	// otherwise disagree about which rows exist.
	tx, err := s.reader().BeginTx(context.Background(), nil)
	if err != nil {
		return nil, false, fmt.Errorf("store: begin thread title context for %s: %w", threadID, err)
	}
	// Read-only: the read pool's connections carry query_only(1), and
	// nothing here writes. Rollback is the whole cleanup.
	defer tx.Rollback()

	// The select lists below follow itemColumnsSansPayload's column ORDER
	// because scanItemRowSansPayload scans positionally — a column added
	// there must be added here too, in the same place.
	windowRows, err := tx.Query(
		`SELECT items.id, items.thread_id, items.turn_index, items.item_index,
		        items.kind, items.role, items.status,
		        substr(items.summary, -`+strconv.Itoa(threadTitleContextSummaryTail)+`),
		        COALESCE(items.payload_id, ''),
		        items.parent_id, items.is_background, items.completion_of,
		        items.tool_name, items.decision,
		        CASE WHEN items.kind = 'user_text' THEN items.meta ELSE '' END,
		        items.created_at, items.updated_at
		   FROM timeline_items AS items
		  WHERE items.thread_id = ?
		    AND items.parent_id = ''
		    AND items.kind IN ('user_text', 'assistant_text')
		  ORDER BY items.turn_index DESC, items.item_index DESC
		  LIMIT ?`,
		// One row past the window: its arrival is what proves rows were
		// dropped, and it is discarded immediately after.
		threadID, limit+1,
	)
	if err != nil {
		return nil, false, fmt.Errorf("store: thread title context items for %s: %w", threadID, err)
	}
	items, err := scanThreadTitleContextItems(windowRows)
	if err != nil {
		return nil, false, fmt.Errorf("store: thread title context items for %s: %w", threadID, err)
	}
	dropped := false
	if len(items) > limit {
		dropped = true
		items = items[:limit]
	}
	slices.Reverse(items)

	// Known accepted edge: a FIRST user message longer than the window's
	// summary slice that sits INSIDE the row window is served from its
	// tail rather than its head. The formatter tail-cuts that shape
	// itself once it overruns, so the pin is the only place the
	// difference could show, and it only shows for a thread whose opening
	// message is both enormous and still in the newest-N rows.
	earliestRows, err := tx.Query(
		`SELECT items.id, items.thread_id, items.turn_index, items.item_index,
		        items.kind, items.role, items.status,
		        substr(items.summary, 1, `+strconv.Itoa(threadTitleContextSummaryHead)+`),
		        COALESCE(items.payload_id, ''),
		        items.parent_id, items.is_background, items.completion_of,
		        items.tool_name, items.decision, items.meta,
		        items.created_at, items.updated_at
		   FROM timeline_items AS items
		  WHERE items.thread_id = ?
		    AND items.parent_id = ''
		    AND items.kind = 'user_text'
		  ORDER BY items.turn_index ASC, items.item_index ASC
		  LIMIT 1`,
		threadID,
	)
	if err != nil {
		return nil, false, fmt.Errorf("store: earliest thread title context item for %s: %w", threadID, err)
	}
	earliest, err := scanThreadTitleContextItems(earliestRows)
	if err != nil {
		return nil, false, fmt.Errorf("store: earliest thread title context item for %s: %w", threadID, err)
	}
	if len(earliest) == 0 || threadTitleContextWindowHolds(items, earliest[0]) {
		return items, dropped, nil
	}
	return append(earliest, items...), dropped, nil
}

// scanThreadTitleContextItems drains one of the two statements above.
// The scan loop is all they share — the statements themselves are
// written out in full, because their projections differ.
func scanThreadTitleContextItems(rows *sql.Rows) ([]Item, error) {
	defer rows.Close()

	var items []Item
	for rows.Next() {
		item, err := scanItemRowSansPayload(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

// threadTitleContextWindowHolds reports whether the newest-rows window
// already contains the row at candidate's position. The window is every
// matching row at or after its oldest member, so the position compare
// is exact.
func threadTitleContextWindowHolds(window []Item, candidate Item) bool {
	if len(window) == 0 {
		return false
	}
	oldest := window[0]
	if candidate.TurnIndex != oldest.TurnIndex {
		return candidate.TurnIndex > oldest.TurnIndex
	}
	return candidate.ItemIndex >= oldest.ItemIndex
}

func (s *Store) HasMatchingSystemItem(threadID string, turnIndex int, kind, parentID, summary string) (bool, error) {
	var exists int
	err := s.reader().QueryRow(
		`SELECT EXISTS(
			SELECT 1
			  FROM timeline_items
			 WHERE thread_id = ?
			   AND turn_index = ?
			   AND kind = ?
			   AND role = 'system'
			   AND parent_id = ?
			   AND summary = ?
			 LIMIT 1
		)`,
		threadID, turnIndex, kind, parentID, summary,
	).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("store: matching system item for thread %s turn %d: %w", threadID, turnIndex, err)
	}
	return exists != 0, nil
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

	query := `SELECT id FROM timeline_items
		  WHERE kind = 'tool_call'
		    AND lower(tool_name) IN (` + placeholders + `)
		    AND thread_id = ? AND turn_index = ?
		  ORDER BY item_index DESC
		  LIMIT 1`

	it, found, err := queryOneHydratedTimelineItem(s.reader(), threadID, query, args...)
	if err != nil {
		return Item{}, false, fmt.Errorf("store: latest tool_call thread %s turn %d: %w", threadID, turnIndex, err)
	}
	return it, found, nil
}

// MaxItemIndexForTurn returns the highest item_index currently persisted
// for (threadID, turnIndex), with ok=false when the turn holds no items.
// The echo handler uses it to stamp a promoted row's provider-order
// boundary: every row at or below this index existed before the echo, so
// it precedes the queued message in the provider transcript.
func (s *Store) MaxItemIndexForTurn(threadID string, turnIndex int) (int, bool, error) {
	var maxIndex sql.NullInt64
	if err := s.reader().QueryRow(
		`SELECT MAX(item_index) FROM timeline_items WHERE thread_id = ? AND turn_index = ?`,
		threadID, turnIndex,
	).Scan(&maxIndex); err != nil {
		return 0, false, fmt.Errorf("store: max item index for %s/%d: %w", threadID, turnIndex, err)
	}
	if !maxIndex.Valid {
		return 0, false, nil
	}
	return int(maxIndex.Int64), true, nil
}
