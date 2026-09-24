package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
)

// FindStreamItemByProviderItemID resolves a streamed assistant row from the
// provider's item id stored in items.meta. It is intentionally a narrow
// fallback lookup for late completion events; the hot delta path keeps the
// in-memory item id and never pays this JSON predicate.
func (s *Store) FindStreamItemByProviderItemID(threadID string, turnIndex int, kind, parentID, providerItemID string) (Item, bool, error) {
	selection, args := timelineIDSelection(threadID, timelineSelection{
		Turn: "?", TurnArgs: []any{turnIndex},
		Where: `items.kind = ?
		    AND items.parent_id = ?
		    AND json_extract(items.meta, '$.provider_item_id') = ?`,
		WhereArgs: []any{kind, parentID, providerItemID},
		OrderBy:   "item_index ASC",
		Limit:     1,
	})
	item, found, err := queryOneHydratedTimelineItem(s.reader(), threadID, selection, args...)
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
	selection, args := turnIDSelection(threadID, turnIndex)
	items, err := queryHydratedTimelineItems(s.reader(), threadID, selection, args...)
	if err != nil {
		return nil, fmt.Errorf("store: list items for thread %s turn %d: %w", threadID, turnIndex, err)
	}
	return items, nil
}

func (s *Store) LastTurnIndex(threadID string) (int, error) {
	index, err := s.lastTurnIndex(threadID)
	return int(index.Int64), err
}

// NextTurnIndex reserves known turns even when they have no cached items yet.
// An empty thread starts at zero; unlike LastTurnIndex, zero is not ambiguous.
func (s *Store) NextTurnIndex(threadID string) (int, error) {
	index, err := s.lastTurnIndex(threadID)
	if err != nil || !index.Valid {
		return 0, err
	}
	return int(index.Int64) + 1, nil
}

func (s *Store) lastTurnIndex(threadID string) (sql.NullInt64, error) {
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
		return sql.NullInt64{}, fmt.Errorf("store: last turn index: %w", err)
	}
	return maxIndex, nil
}

func (s *Store) FindTurnItem(threadID string, turnIndex int, kind string) (Item, bool, error) {
	selection, args := timelineIDSelection(threadID, timelineSelection{
		Turn: "?", TurnArgs: []any{turnIndex},
		Where: "items.kind = ?", WhereArgs: []any{kind},
		OrderBy: "item_index DESC", Limit: 1,
	})
	item, found, err := queryOneHydratedTimelineItem(s.reader(), threadID, selection, args...)
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
// The query is O(log N) thanks to the partial expression indexes
// idx_items_meta_task_id and idx_import_history_items_task_lookup, which
// materialise json_extract(meta, '$.task_id') for the narrow subset of
// rows that actually carry a task_id. The kind filter stays in Go-space rather
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
	selection, args := timelineKeyedIDSelection(threadID,
		"items.updated_at AS updated_at",
		"json_extract(items.meta, '$.task_id') = ?", []any{taskID},
		"updated_at DESC", 1)
	item, found, err := queryOneHydratedTimelineItem(s.reader(), threadID, selection, args...)
	if err != nil {
		return Item{}, false, fmt.Errorf("store: find tool call by task id %s: %w", taskID, err)
	}
	return item, found, nil
}

// FindOriginalAgentLaunchByTaskID resolves the ORIGINAL launch row a
// Claude task belongs to: the OLDEST tool_call on threadID carrying
// taskID in its persisted meta, excluding excludeItemID (the caller's
// own row). It exists for the resume-carrier identity copy
// (triage's keep-running flip): a §E6 resume rebinds the task onto the
// resuming tool's OWN row and stamps the same task_id there, so
// FindToolCallItemByTaskID's newest-first pick would hand the carrier
// back to itself. Oldest-first with the carrier excluded is the first
// binding — the original Agent/Task launch — even across repeated
// resumes, whose carriers are all younger than the launch.
//
// Same index as FindToolCallItemByTaskID (idx_items_meta_task_id);
// the exclusion and ordering are post-index refinements over the
// handful of rows one task_id can match.
func (s *Store) FindOriginalAgentLaunchByTaskID(threadID, taskID, excludeItemID string) (Item, bool, error) {
	if taskID == "" {
		return Item{}, false, nil
	}
	selection, args := timelineKeyedIDSelection(threadID,
		"items.created_at AS created_at",
		"json_extract(items.meta, '$.task_id') = ? AND items.id <> ?", []any{taskID, excludeItemID},
		"created_at ASC", 1)
	item, found, err := queryOneHydratedTimelineItem(s.reader(), threadID, selection, args...)
	if err != nil {
		return Item{}, false, fmt.Errorf("store: find original agent launch by task id %s: %w", taskID, err)
	}
	return item, found, nil
}

// FindUserTextItemBySendID returns the thread's reader-authored `user_text`
// row carrying the client-minted send id, anywhere in retained history.
//
// The ONE caller is the send-idempotency check: a client re-sends the same
// frame after a socket died under it, and the backend has to tell "this
// arrived twice" from "this is a new message". It runs on EVERY send, so the
// match is made through sparse send-id indexes on both physical timeline arms;
// at most one row is hydrated. Newer messages cannot age out an accepted send.
//
// Every AO send is a reader-authored row, and
// the wire-only injections and subagent child prompts it excludes carry no
// send id to match.
//
// Keep json_valid alongside the lookup and its partial indexes: malformed
// neighbouring metadata must never fail every send. Existing task-id indexes
// currently reject such writes (TestBothTimelineArmsRefuseMetaTheLookupCouldNotRead).
//
// An empty send id is not found, without a query:
// every app-internal injector leaves the id unset, and matching those against
// each other would collapse unrelated messages into one.
func (s *Store) FindUserTextItemBySendID(threadID, sendID string) (Item, bool, error) {
	if sendID == "" {
		return Item{}, false, nil
	}
	query, args := sendIdentityQuery(threadID, sendID)
	item, found, err := queryOneHydratedTimelineItem(s.reader(), threadID, query, args...)
	if err != nil {
		return Item{}, false, fmt.Errorf("store: find user text item by send id for thread %s: %w", threadID, err)
	}
	if found {
		return item, true, nil
	}
	query, args = joinedSendIdentityQuery(threadID, sendID)
	item, found, err = queryOneHydratedTimelineItem(s.reader(), threadID, query, args...)
	if err != nil {
		return Item{}, false, fmt.Errorf("store: find joined user text item by send id for thread %s: %w", threadID, err)
	}
	return item, found, nil
}

func sendIdentityQuery(threadID, sendID string) (string, []any) {
	return timelineIDSelection(threadID, timelineSelection{
		KeyFirst: true,
		Where: readerAuthoredUserTextFilterFor("items.") +
			` AND json_valid(items.meta) AND json_extract(items.meta, '$.sendId') IS NOT NULL
			  AND json_extract(items.meta, '$.sendId') = ?`,
		WhereArgs: []any{sendID},
		Limit:     1,
	})
}

// joinedSendIdentityQuery is the second arm of the send-identity lookup:
// the row a MERGED outbound message left behind answers for every send id
// it folded in, and only its first member sits on `$.sendId`
// (usermessage.Meta.JoinedSendIDs). A multi-valued key cannot live in an
// expression index, so the sparse
// idx_*_joined_send_ids indexes narrow the candidate set to the joined rows
// of this thread — normally none, single digits at worst — and `json_each`
// compares the array inside SQL rather than carrying metas back into Go.
//
// `json_extract(... '$.joinedSendIds') IS NOT NULL` is repeated verbatim from
// the index predicate: SQLite applies a partial index only when the query
// textually implies its WHERE clause.
func joinedSendIdentityQuery(threadID, sendID string) (string, []any) {
	return timelineIDSelection(threadID, timelineSelection{
		Where: readerAuthoredUserTextFilterFor("items.") +
			` AND json_valid(items.meta)
			  AND json_extract(items.meta, '$.joinedSendIds') IS NOT NULL
			  AND EXISTS (SELECT 1 FROM json_each(items.meta, '$.joinedSendIds') WHERE json_each.value = ?)`,
		WhereArgs: []any{sendID},
		Limit:     1,
	})
}

// FindProvisionalSubagentPrompt returns the oldest launch-scoped prompt
// row under parentID that still carries the provisional marker and whose
// summary is exactly `content`.
//
// It is the reconciliation half of the §E6 resume prompt: that row is
// minted from the rebind `system/task_started` (which has no provider
// uuid to give) and the session mirror later delivers the agent's copy of
// the same text WITH a uuid. Without this lookup the transcript row lands as
// a second `user:wire:<uuid>` duplicate below the answer it asked for.
//
// The non-empty `parent_id` term is load-bearing: it is the predicate of the partial
// idx_items_parent (thread_id, parent_id) index, and SQLite cannot prove
// it from a bound parameter. The meta LIKE is a cheap post-index filter
// over the handful of rows one launch scope holds; the exact comparison
// is the summary equality, in SQL rather than in Go so a long agent
// prompt is never carried back across the boundary to be discarded.
func (s *Store) FindProvisionalSubagentPrompt(threadID, parentID, content string) (Item, bool, error) {
	if strings.TrimSpace(threadID) == "" || strings.TrimSpace(parentID) == "" {
		return Item{}, false, nil
	}
	item, found, err := queryOneHydratedTimelineItem(
		s.reader(), threadID,
		`SELECT id FROM items
		  WHERE thread_id = ?
		    AND parent_id = ?
		    AND parent_id <> ''
		    AND kind = 'user_text'
		    AND summary = ?
		    AND meta LIKE '%subagent_prompt_provisional%'
		    AND COALESCE(json_extract(meta, '$.provider_item_id'), '') = ''
		  ORDER BY turn_index, item_index
		  LIMIT 1`,
		threadID, parentID, content,
	)
	if err != nil {
		return Item{}, false, fmt.Errorf("store: find provisional subagent prompt %s/%s: %w", threadID, parentID, err)
	}
	return item, found, nil
}

// GetThreadItemByPayloadID returns the newest item on threadID whose
// payload_id OR input_payload_id matches payloadID, so a payload id is
// not usable outside the thread that references it. The two partial
// indexes (idx_items_payload_id, idx_items_input_payload_id) cover the
// two local columns; UNION keeps each branch index-friendly. A single
// OR-clause forces SQLite onto the broad thread_id index instead, which
// would scan every row in the thread on every lazy-load click. An
// imported row's payload lives in its own chunk, so the imported branch
// starts from the payload's chunk rows and scans only those chunks.
func (s *Store) GetThreadItemByPayloadID(threadID, payloadID string) (Item, bool, error) {
	item, found, err := queryOneHydratedTimelineItem(
		s.reader(), threadID,
		`SELECT id FROM (
		     SELECT id, updated_at FROM items
		      WHERE thread_id = ? AND payload_id = ?
		     UNION
		     SELECT id, updated_at FROM items
		      WHERE thread_id = ? AND input_payload_id = ?
		     UNION
		     SELECT items.id, items.updated_at
		       FROM import_history_payloads payload
		       CROSS JOIN thread_import_chunks refs ON refs.chunk_id = payload.chunk_id
		       CROSS JOIN import_history_items items ON items.chunk_id = payload.chunk_id
		      WHERE payload.id = ? AND refs.thread_id = ?
		        AND (items.payload_id = payload.id OR items.input_payload_id = payload.id)
		        AND `+importedNotOverridden+`
		 )
		 ORDER BY updated_at DESC
		 LIMIT 1`,
		threadID, payloadID, threadID, payloadID, payloadID, threadID,
	)
	if err != nil {
		return Item{}, false, fmt.Errorf("store: get item by payload id %s on thread %s: %w", payloadID, threadID, err)
	}
	return item, found, nil
}

// readerAuthoredUserTextFilterFor matches the user_text rows the reader
// actually typed: top-level (subagent child prompts excluded) and not
// wire-only (context injections the send path marks in meta). It is the
// SQL counterpart of the frontend's `isReaderAuthoredUserText`; the
// json_valid guard keeps one corrupt meta blob from failing the whole
// read (the lifecycle queries guard the same way).
//
// The empty-`parent_id` plus `kind = 'user_text'` prefix is also what makes
// the partial index idx_items_user_text (migration v73) apply: SQLite
// uses a partial index only when the query's predicates TEXTUALLY imply
// the index's WHERE clause, so neither term may be dropped or reordered
// into a form that no longer states both.
func readerAuthoredUserTextFilterFor(alias string) string {
	return topLevelItemsFilterFor(alias) + " AND " + userMessageTickFilterFor(alias)
}

func userMessageTickFilterFor(alias string) string {
	return alias + `kind = 'user_text'
	  AND COALESCE(CASE WHEN json_valid(` + alias + `meta) THEN json_extract(` + alias + `meta, '$.wire_only') END, 0) != 1`
}

var readerAuthoredUserTextFilter = readerAuthoredUserTextFilterFor("")

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
// columns per row, so even a very long thread's list is a few KB.
//
// It runs on every thread switch, so it walks the partial index
// idx_items_user_text (v73) through the physical timeline arms rather
// than sorting the thread's whole row set behind the view: 17,816 pages
// / 17 ms became 736 / 1-3 ms on a 67k-item thread.
func (s *Store) ListThreadUserMessageTicks(threadID string, selection TimelineSelection) ([]UserMessageTick, error) {
	return readSnapshot(s.reader(), "user message ticks", func(q sqlQueryer) ([]UserMessageTick, error) {
		scope, err := s.resolveTimelineScope(q, threadID, selection)
		if err != nil {
			return nil, err
		}
		filter := readerAuthoredUserTextFilterFor("items.")
		var filterArgs []any
		if scope.selection.ScopeRootID != "" {
			filter, filterArgs = scope.filter("items.")
			filter += " AND " + userMessageTickFilterFor("items.")
		}

		sql, args := timelineArms(threadID, timelineSelection{
			Columns: func(string, string) string {
				return `items.id AS id, items.turn_index AS turn_index, items.item_index AS item_index`
			},
			Where: filter, WhereArgs: filterArgs,
			OrderBy: "turn_index ASC, item_index ASC",
		})
		rows, err := q.Query(sql, args...)
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
	})
}

// UserMessageHistoryEntry is one composer history-recall entry: a
// reader-authored user message's full text plus its position, so the
// frontend can merge the loaded window's live rows (optimistic sends
// included) over the store's baseline the way the nav rail merges ticks.
type UserMessageHistoryEntry struct {
	ID        string `json:"id"`
	TurnIndex int    `json:"turnIndex"`
	ItemIndex int    `json:"itemIndex"`
	Summary   string `json:"summary"`
}

// ListThreadUserMessageHistory returns the thread's newest reader-authored
// user messages, newest first, capped at limit. Backs the composer's
// ArrowUp history recall, which needs the FULL text — a recalled message
// is re-sent verbatim, so unlike the turn-preview read nothing here is
// rune-capped. Wire-only injections and subagent child prompts are
// excluded by the shared predicate. Like the ticks read it walks
// idx_items_user_text through the physical timeline arms, so the LIMIT
// stops the read instead of trimming a fully sorted thread.
//
// A non-positive limit returns no rows.
func (s *Store) ListThreadUserMessageHistory(threadID string, limit int) ([]UserMessageHistoryEntry, error) {
	if limit <= 0 {
		return []UserMessageHistoryEntry{}, nil
	}
	sql, args := timelineArms(threadID, timelineSelection{
		Columns: func(string, string) string {
			return `items.id AS id, items.turn_index AS turn_index,
			        items.item_index AS item_index, items.summary AS summary`
		},
		Where:   readerAuthoredUserTextFilterFor("items."),
		OrderBy: "turn_index DESC, item_index DESC",
		Limit:   limit,
	})
	rows, err := s.reader().Query(sql, args...)
	if err != nil {
		return nil, fmt.Errorf("store: list user message history on thread %s: %w", threadID, err)
	}
	defer rows.Close()
	entries := []UserMessageHistoryEntry{}
	for rows.Next() {
		var e UserMessageHistoryEntry
		if err := rows.Scan(&e.ID, &e.TurnIndex, &e.ItemIndex, &e.Summary); err != nil {
			return nil, fmt.Errorf("store: scan user message history row on thread %s: %w", threadID, err)
		}
		entries = append(entries, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list user message history on thread %s: %w", threadID, err)
	}
	return entries, nil
}

// LatestHumanUserText returns the newest user message of a thread that no
// agent wrote, and false when the thread has none.
//
// It is what the agent thread tools quote in a request footer, so the
// receiving thread can see what the person behind the sender last asked
// for. Rows an agent wrote are excluded by `meta.origin`, the same key the
// attribution chip renders from, so a chain of agent-to-agent messages
// never quotes another agent back at itself. Wire-only injections and
// subagent prompts are already excluded by the shared predicate.
func (s *Store) LatestHumanUserText(threadID string) (string, bool, error) {
	query, args := timelineArms(threadID, timelineSelection{
		Columns: func(string, string) string {
			return `items.summary AS summary, items.turn_index AS turn_index, items.item_index AS item_index`
		},
		Where: readerAuthoredUserTextFilterFor("items.") +
			` AND COALESCE(CASE WHEN json_valid(items.meta) THEN json_extract(items.meta, '$.origin') END, '') = ''`,
		OrderBy: "turn_index DESC, item_index DESC",
		Limit:   1,
	})
	var summary string
	var turnIndex, itemIndex int
	err := s.reader().QueryRow(query, args...).Scan(&summary, &turnIndex, &itemIndex)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("store: latest human user text on thread %s: %w", threadID, err)
	}
	return summary, true, nil
}

func (s *Store) GetThreadItem(threadID, id string) (Item, bool, error) {
	return s.getThreadItem(s.reader(), threadID, id)
}

// getThreadItem is GetThreadItem against a caller-chosen queryer, so a
// window read that must be attested by stamps from the same transaction
// can resolve its anchor inside that transaction too.
//
// The selection is the id itself: the hydrator resolves it through each
// arm's id index and returns nothing when the thread has no such row.
func (s *Store) getThreadItem(q sqlQueryer, threadID, id string) (Item, bool, error) {
	item, found, err := queryOneHydratedTimelineItem(q, threadID, `SELECT ? AS id`, id)
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
	// The order is applied after the keyed arms: ordered arms would let the
	// planner walk the thread's ordering index newest-first probing meta
	// per row instead of using the narrow partial expression index (13ms
	// vs 0.04ms on a 38k-item thread).
	selection, args := timelineKeyedIDSelection(threadID,
		"items.turn_index AS turn_index, items.item_index AS item_index",
		"items.kind = 'notification' AND json_extract(items.meta, '$.task_id') = ?", []any{taskID},
		"turn_index DESC, item_index DESC", 1)
	item, found, err := queryOneHydratedTimelineItem(s.reader(), threadID, selection, args...)
	if err != nil {
		return Item{}, false, fmt.Errorf("store: find notification by task_id %s: %w", taskID, err)
	}
	return item, found, nil
}

func (s *Store) ListTurnItems(threadID string, turnIndex int) ([]Item, error) {
	selection, args := turnIDSelection(threadID, turnIndex)
	items, err := queryHydratedTimelineItems(s.reader(), threadID, selection, args...)
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
	query, args := timelineArms(threadID, timelineSelection{
		Columns: itemColumnsSansPayloadFor,
		Turn:    "?", TurnArgs: []any{turnIndex},
		OrderBy: "item_index",
	})
	rows, err := s.reader().Query(query, args...)
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
// regeneration reads: top-level (empty `parent_id`) `user_text` and
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
	//
	// One row past the window: its arrival is what proves rows were
	// dropped, and it is discarded immediately after.
	windowSQL, windowArgs := timelineArms(threadID, timelineSelection{
		Columns: func(threadIDExpr, revExpr string) string {
			return `items.id, ` + threadIDExpr + ` AS thread_id,
			        items.turn_index AS turn_index, items.item_index AS item_index,
			        items.kind, items.role, items.status,
			        substr(items.summary, -` + strconv.Itoa(threadTitleContextSummaryTail) + `),
			        COALESCE(items.payload_id, ''),
			        items.parent_id, items.is_background, items.completion_of,
			        items.tool_name, items.decision,
			        CASE WHEN items.kind = 'user_text' THEN items.meta ELSE '' END,
			        items.created_at, items.updated_at,
			        ` + revExpr
		},
		Where: topLevelItemsFilterFor("items.") + `
		   AND items.kind IN ('user_text', 'assistant_text')`,
		OrderBy: "turn_index DESC, item_index DESC",
		Limit:   limit + 1,
	})
	windowRows, err := tx.Query(windowSQL, windowArgs...)
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
	earliestSQL, earliestArgs := timelineArms(threadID, timelineSelection{
		Columns: func(threadIDExpr, revExpr string) string {
			return `items.id, ` + threadIDExpr + ` AS thread_id,
			        items.turn_index AS turn_index, items.item_index AS item_index,
			        items.kind, items.role, items.status,
			        substr(items.summary, 1, ` + strconv.Itoa(threadTitleContextSummaryHead) + `),
			        COALESCE(items.payload_id, ''),
			        items.parent_id, items.is_background, items.completion_of,
			        items.tool_name, items.decision, items.meta,
			        items.created_at, items.updated_at,
			        ` + revExpr
		},
		// `parent_id = '' AND kind = 'user_text'` is also what lets this
		// one use the partial idx_items_user_text.
		Where: topLevelItemsFilterFor("items.") + `
		   AND items.kind = 'user_text'`,
		OrderBy: "turn_index ASC, item_index ASC",
		Limit:   1,
	})
	earliestRows, err := tx.Query(earliestSQL, earliestArgs...)
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
	query, args := timelineArms(threadID, timelineSelection{
		Columns: func(string, string) string { return "1" },
		Turn:    "?", TurnArgs: []any{turnIndex},
		Where: `items.kind = ?
			   AND items.role = 'system'
			   AND items.parent_id = ?
			   AND items.summary = ?`,
		WhereArgs: []any{kind, parentID, summary},
	})
	err := s.reader().QueryRow(`SELECT EXISTS(`+query+`)`, args...).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("store: matching system item for thread %s turn %d: %w", threadID, turnIndex, err)
	}
	return exists != 0, nil
}

// MaxItemIndexForTurn returns the highest item_index currently persisted
// for (threadID, turnIndex), with ok=false when the turn holds no items.
// The echo handler uses it to stamp a promoted row's provider-order
// boundary: every row at or below this index existed before the echo, so
// it precedes the queued message in the provider transcript.
func (s *Store) MaxItemIndexForTurn(threadID string, turnIndex int) (int, bool, error) {
	var maxIndex sql.NullInt64
	query, args := turnAggregateQuery(threadID, turnIndex, "MAX", "item_index")
	if err := s.reader().QueryRow(query, args...).Scan(&maxIndex); err != nil {
		return 0, false, fmt.Errorf("store: max item index for %s/%d: %w", threadID, turnIndex, err)
	}
	if !maxIndex.Valid {
		return 0, false, nil
	}
	return int(maxIndex.Int64), true, nil
}

// ListUnclaimedProviderQueuedUserItemIDs returns the ids of this thread's
// `user_text` rows whose message the PROVIDER's own queue took ownership of
// (`itemmeta.MarkProviderQueued`, a Codex `thread/queue/add` on an app-server
// >= 0.148) and that no provider echo has claimed yet.
//
// The pair of conditions is the whole definition of "still with the provider":
// the marker is permanent by design (nothing clears it when the message
// eventually runs), so the row is only outstanding while it also carries no
// `provider_item_id` — the key triage stamps when the dispatched turn's
// `userMessage` echo lands on it.
//
// Ordered by timeline position so a caller reporting them names them in the
// order the provider will run them.
func (s *Store) ListUnclaimedProviderQueuedUserItemIDs(threadID string) ([]string, error) {
	rows, err := s.reader().Query(
		`SELECT id FROM items
		  WHERE thread_id = ?
		    AND kind = 'user_text'
		    AND COALESCE(json_extract(meta, '$.providerQueued'), 0) != 0
		    AND COALESCE(json_extract(meta, '$.provider_item_id'), '') = ''
		  ORDER BY turn_index, item_index`,
		threadID,
	)
	if err != nil {
		return nil, fmt.Errorf("store: list provider-queued user rows for thread %s: %w", threadID, err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("store: scan provider-queued user row for thread %s: %w", threadID, err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate provider-queued user rows for thread %s: %w", threadID, err)
	}
	return ids, nil
}
