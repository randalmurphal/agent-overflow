package store

import (
	"database/sql"
	"fmt"
	"time"

	"github.com/google/uuid"

	"agent-overflow/internal/entityid"
	"agent-overflow/internal/itemmeta"
)

// BuildForkedThread returns a Thread row populated from source plus the
// fork-only fields: a fresh UUID, a "(fork)"-suffixed title, the
// `ForkedFromThreadID` linkage, and a `created_at` / `updated_at` pair
// at the current millisecond. The session-state fields
// (`SessionRef`, `PendingForkRef`) are left empty — the app-side fork
// saga sets them once the provider-specific resume reference is known.
// AutoCompactStandard/Extended Percent are intentionally NOT copied —
// a fork starts with zero overrides so it picks up the live Settings
// value on the first session start (the same default-resolution path a
// brand-new thread follows).
//
// `LastTokenUsage` IS copied so the meter reflects the inherited
// conversation history from frame 0. The new resumed session emits a
// fresh `thread/tokenUsage/updated` on its first turn which overwrites
// this seed with the live measurement. Without the copy the meter
// would render 0% for forked threads even though the cloned items
// occupy meaningful context.
//
// Pure: this only builds the row. The caller persists it (CreateThread)
// and pairs it with the side-effecting clone steps.
func BuildForkedThread(source Thread) Thread {
	now := time.Now().UnixMilli()
	return Thread{
		ID:                 entityid.New(),
		ProjectID:          source.ProjectID,
		Title:              source.Title + " (fork)",
		Provider:           source.Provider,
		WorkspacePath:      source.WorkspacePath,
		Model:              source.Model,
		WorktreePath:       source.WorktreePath,
		Branch:             source.Branch,
		Mode:               source.Mode,
		ReasoningEffort:    source.ReasoningEffort,
		FastMode:           source.FastMode,
		ContextWindow:      source.ContextWindow,
		RuntimeMode:        source.RuntimeMode,
		LastTokenUsage:     source.LastTokenUsage,
		ForkedFromThreadID: source.ID,
		CreatedAt:          now,
		UpdatedAt:          now,
	}
}

// CloneThreadItems copies the visible timeline items from sourceThreadID into
// targetThreadID, preserving turn ordering while assigning new item IDs. The
// returned map is source item id -> cloned item id for every copied row.
//
// When throughTurnIndex is non-nil, only items whose turn_index is <= *throughTurnIndex
// are copied — used for fork-at-point so the forked thread starts truncated
// at the chosen turn. nil means clone every turn (existing fork-at-tail
// behavior).
//
// Rows that are LIVE-backgrounded (`is_background=1`, status `running`
// or `streaming`, and NO completion sibling inside the cut) are
// SKIPPED, and so is everything hanging off one — see
// `cloneThreadItemsTx`. Those rows point at PTYs / subagents owned by
// the source session's provider subprocess, and the fork gets its own
// subprocess that can never reach them. Copying them would strand the
// forked thread with ghost rows that can never complete: the fork
// settle (SettleForkedThreadAsInterrupted) exempts background
// tool_calls in BOTH live statuses, so a copied one would be
// permanently unsettleable. The parent thread is untouched — its
// backgrounded launches keep running under its own session.
//
// A running background launch WITH a completion sibling is not live —
// it is the permanent shape of every finished background task
// (invariant 24: the sibling is the terminal, the launch's status
// never flips) — and clones normally, subtree included. So do
// non-background live rows; the filter is deliberately narrow.
//
// All inserts run in a single transaction so a 200-row clone takes one
// fsync instead of 200. Per-row InsertItem would commit individually
// and dominate the fork wall-clock for large threads.
//
// Forks call CloneThreadHistoryThroughTurn instead, which folds this
// and the turns clone into ONE transaction. This entry point stands
// alone for callers that want only the item half.
func (s *Store) CloneThreadItems(sourceThreadID, targetThreadID string, throughTurnIndex *int) (map[string]string, error) {
	return s.inCloneTx(func(tx *sql.Tx) (map[string]string, error) {
		return cloneThreadItemsTx(tx, sourceThreadID, targetThreadID, throughTurnKeep(throughTurnIndex))
	})
}

// CloneThreadHistoryThroughTurn is the fork pipeline's clone: the item
// rows and the turn rows of the same cut, read and written inside ONE
// transaction.
//
// The single transaction is the point. Splitting the two halves lets a
// turn complete between them, so the fork gets a turn row stamped
// `completed_at`/`end_turn` over items that were snapshotted mid-stream
// and then flipped to interrupted — a fork whose own two tables
// disagree about whether its last turn finished. On the single writer
// connection a transaction is also the only thing that makes the two
// reads one snapshot.
func (s *Store) CloneThreadHistoryThroughTurn(sourceThreadID, targetThreadID string, throughTurnIndex *int) (map[string]string, error) {
	return s.inCloneTx(func(tx *sql.Tx) (map[string]string, error) {
		idMap, err := cloneThreadItemsTx(tx, sourceThreadID, targetThreadID, throughTurnKeep(throughTurnIndex))
		if err != nil {
			return nil, err
		}
		if err := cloneThreadTurnsTx(tx, sourceThreadID, targetThreadID, throughTurnIndex); err != nil {
			return nil, err
		}
		return idMap, nil
	})
}

// throughTurnKeep renders the turn-granular cut as a cloneThreadItemsTx
// keep predicate. nil keeps every turn.
func throughTurnKeep(throughTurnIndex *int) func(Item) bool {
	return func(item Item) bool {
		return throughTurnIndex == nil || item.TurnIndex <= *throughTurnIndex
	}
}

// inCloneTx runs one clone body inside a writer transaction, rolling
// back on any error. Every clone entry point in this file shares it so
// none of them can accidentally ship a half-cloned fork.
func (s *Store) inCloneTx(body func(*sql.Tx) (map[string]string, error)) (map[string]string, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, fmt.Errorf("store: begin clone tx: %w", err)
	}
	defer tx.Rollback()

	idMap, err := body(tx)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("store: commit clone tx: %w", err)
	}
	return idMap, nil
}

// cloneThreadItemsTx is the shared clone body behind CloneThreadItems,
// CloneThreadHistoryThroughTurn, and CloneThreadHistoryBeforeItem: keep
// decides which source rows copy; the live-background skip applies on
// top of it unconditionally.
//
// The source read runs on the caller's transaction, not on the read
// pool. That is what makes the items and the turns of one clone a
// single snapshot: the writer is a single connection, so nothing else
// can commit while this transaction is open.
//
// A skip is TRANSITIVE. A row is dropped when it is a LIVE background
// launch (running/streaming with no completion sibling in the cut),
// when keep rejects it, or when the row its `parent_id` /
// `completion_of` names was itself dropped. Without that closure a
// child of a dropped launch cloned with its parent_id unremapped — a
// reference into the SOURCE thread, invisible to every window read and
// permanent (fork thread d1166194 carried 5901 such rows, all pointing
// at background-running Agent launches left behind in b44a738d).
//
// One forward pass suffices: ListItems orders by (turn_index,
// item_index), and invariants 10 / 11 put a parent before its children
// and a tool_call before the completion row that settles it in exactly
// that order, so every id a row references has already been decided by
// the time the row is examined.
//
// A reference to an id that is not in the source list at all is
// pre-existing corruption in the SOURCE and copies verbatim — only ids
// this pass deliberately dropped propagate.
func cloneThreadItemsTx(tx *sql.Tx, sourceThreadID, targetThreadID string, keep func(Item) bool) (map[string]string, error) {
	items, err := queryHydratedTimelineItems(
		tx, sourceThreadID,
		`SELECT id FROM timeline_items WHERE thread_id = ?`,
		sourceThreadID,
	)
	if err != nil {
		return nil, fmt.Errorf("store: list source items for fork %s: %w", sourceThreadID, err)
	}

	// A background launch's terminal is its completion SIBLING — the
	// launch row itself stays `running` forever (invariant 24), so
	// "live" cannot be read off the launch's own status. This pre-pass
	// collects the launches settled by a sibling INSIDE the cut; keep
	// filters it too, so a sibling beyond a through-turn cut cannot
	// vouch for a launch the fork would then hold unsettleable.
	settled := make(map[string]struct{})
	for _, item := range items {
		if item.CompletionOf != "" && keep(item) {
			settled[item.CompletionOf] = struct{}{}
		}
	}

	clonedItems := make([]Item, 0, len(items))
	idMap := make(map[string]string, len(items))
	skipped := make(map[string]struct{})
	dropped := func(id string) bool {
		if id == "" {
			return false
		}
		_, ok := skipped[id]
		return ok
	}
	for _, item := range items {
		if isLiveBackgroundRow(item, settled) ||
			!keep(item) ||
			dropped(item.ParentID) ||
			dropped(item.CompletionOf) {
			skipped[item.ID] = struct{}{}
			continue
		}
		oldID := item.ID
		item.ID = uuid.NewString()
		item.ThreadID = targetThreadID
		idMap[oldID] = item.ID
		clonedItems = append(clonedItems, item)
	}
	// A reference to a dropped id surviving to this pass means a writer
	// broke the parents-precede-children ordering the forward pass rests
	// on (a hand-authored InsertItem, an import batch) — the transitive
	// skip above never saw it. Refuse the fork rather than mint the
	// invisible cross-thread reference this function exists to prevent;
	// the fork saga rolls the target thread back on error. Unknown ids
	// (never in the source list) still copy verbatim.
	for i := range clonedItems {
		if next, ok := idMap[clonedItems[i].ParentID]; ok {
			clonedItems[i].ParentID = next
		} else if dropped(clonedItems[i].ParentID) {
			return nil, fmt.Errorf("store: clone from thread %s: row %s references dropped parent %s out of source order",
				sourceThreadID, clonedItems[i].ID, clonedItems[i].ParentID)
		}
		if next, ok := idMap[clonedItems[i].CompletionOf]; ok {
			clonedItems[i].CompletionOf = next
		} else if dropped(clonedItems[i].CompletionOf) {
			return nil, fmt.Errorf("store: clone from thread %s: row %s completes dropped call %s out of source order",
				sourceThreadID, clonedItems[i].ID, clonedItems[i].CompletionOf)
		}
	}

	if err := cloneThreadPayloadsTx(tx, sourceThreadID, targetThreadID, clonedItems); err != nil {
		return nil, err
	}

	stmt, err := tx.Prepare(
		`INSERT INTO items (id, thread_id, turn_index, item_index, kind, role, status, summary,
		    payload_id, input_payload_id, parent_id, is_background, completion_of, tool_name, decision, meta,
		    created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
	)
	if err != nil {
		return nil, fmt.Errorf("store: prepare clone insert: %w", err)
	}
	defer stmt.Close()

	var maxUpdatedAt int64
	cloned := 0
	for _, item := range clonedItems {
		if _, err := stmt.Exec(
			item.ID, item.ThreadID, item.TurnIndex, item.ItemIndex,
			item.Kind, item.Role, item.Status, item.Summary,
			nilIfEmpty(item.PayloadID), nilIfEmpty(item.InputPayloadID), item.ParentID,
			boolToInt(item.IsBackground), item.CompletionOf, item.ToolName, item.Decision, item.Meta,
			item.CreatedAt, item.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("store: clone item into thread %s: %w", targetThreadID, err)
		}
		if item.UpdatedAt > maxUpdatedAt {
			maxUpdatedAt = item.UpdatedAt
		}
		cloned++
	}

	// Touch the destination thread's updated_at once at the end (mirrors
	// per-row InsertItem's touch semantics, batched).
	if cloned > 0 {
		if _, err := tx.Exec(`UPDATE threads SET updated_at = ? WHERE id = ?`, maxUpdatedAt, targetThreadID); err != nil {
			return nil, fmt.Errorf("store: touch fork thread %s updated_at: %w", targetThreadID, err)
		}
	}

	return idMap, nil
}

// isLiveBackgroundRow is the clone's "owned by the source's provider
// subprocess" test. A background launch's terminal is its completion
// sibling, never its own status — the launch row stays `running`
// forever (invariant 24) — so a launch named in `settled` is finished
// history and clones as-is, sibling and subtree included (thread
// c65dfb09's fork silently lost 1631 completed subagent rows to a
// status-only version of this test). BOTH live statuses count for a
// siblingless launch: a background tool_call is exempt from the fork
// settle in either one, so a truly-live row cloned into a fork would
// be permanently unsettleable — the ghost-row failure the skip exists
// to prevent (fork d1166194, 5901 rows).
func isLiveBackgroundRow(item Item, settled map[string]struct{}) bool {
	if !item.IsBackground || (item.Status != "running" && item.Status != "streaming") {
		return false
	}
	_, ok := settled[item.ID]
	return !ok
}

// cloneThreadPayloadsTx gives a fork its own copy of every result and input
// payload referenced by the items it will receive. Payload IDs are local to a
// thread, so they stay unchanged while their owning thread key changes. The
// chunk and edit-snapshot children copy with the same scope. Keeping all four
// tables in cloneThreadItems' transaction prevents a partial payload graph or
// an item that points back into mutable source history.
func cloneThreadPayloadsTx(tx *sql.Tx, sourceThreadID, targetThreadID string, items []Item) error {
	payloadIDs := make(map[string]struct{})
	for _, item := range items {
		if item.PayloadID != "" {
			payloadIDs[item.PayloadID] = struct{}{}
		}
		if item.InputPayloadID != "" {
			payloadIDs[item.InputPayloadID] = struct{}{}
		}
	}
	if len(payloadIDs) == 0 {
		return nil
	}

	payloadStmt, err := tx.Prepare(
		`INSERT INTO payloads (
		    thread_id, id, kind, meta, data, created_at, preview_spans, spans
		 )
		 SELECT ?, id, kind, meta, data, created_at, preview_spans, spans
		   FROM timeline_payloads
		  WHERE thread_id = ? AND id = ?`,
	)
	if err != nil {
		return fmt.Errorf("store: prepare fork payload clone: %w", err)
	}
	defer payloadStmt.Close()

	chunkStmt, err := tx.Prepare(
		`INSERT INTO payload_chunks (
		    thread_id, payload_id, chunk_index, start_offset, data, created_at
		 )
		 SELECT ?, payload_id, chunk_index, start_offset, data, created_at
		   FROM payload_chunks
		  WHERE thread_id = ? AND payload_id = ?`,
	)
	if err != nil {
		return fmt.Errorf("store: prepare fork payload chunk clone: %w", err)
	}
	defer chunkStmt.Close()

	snapshotStmt, err := tx.Prepare(
		`INSERT INTO edit_file_snapshots (
		    thread_id, payload_id, path, content, created_at
		 )
		 SELECT ?, payload_id, path, content, created_at
		   FROM edit_file_snapshots
		  WHERE thread_id = ? AND payload_id = ?`,
	)
	if err != nil {
		return fmt.Errorf("store: prepare fork edit snapshot clone: %w", err)
	}
	defer snapshotStmt.Close()

	for payloadID := range payloadIDs {
		result, err := payloadStmt.Exec(targetThreadID, sourceThreadID, payloadID)
		if err != nil {
			return fmt.Errorf("store: clone payload %s into thread %s: %w", payloadID, targetThreadID, err)
		}
		if err := requireRowsAffected(
			result, fmt.Sprintf("store: clone payload %s into thread %s", payloadID, targetThreadID),
		); err != nil {
			return err
		}
		if _, err := chunkStmt.Exec(targetThreadID, sourceThreadID, payloadID); err != nil {
			return fmt.Errorf("store: clone payload chunks %s into thread %s: %w", payloadID, targetThreadID, err)
		}
		if _, err := snapshotStmt.Exec(targetThreadID, sourceThreadID, payloadID); err != nil {
			return fmt.Errorf("store: clone edit snapshots %s into thread %s: %w", payloadID, targetThreadID, err)
		}
	}
	return nil
}

// CloneThreadTurns copies the source thread's turns rows (<=
// *throughTurnIndex when non-nil, everything when nil) into the target
// thread. turn_id is a global PRIMARY KEY, so cloned rows get a fresh
// synthesized `<targetThreadID>:<turn_index>` id — the same convention
// Claude turns use — while provider_turn_id is preserved verbatim.
// That preserved wire id is the point of the clone: a Codex
// `thread/fork` keeps the source's turn ids, so the cloned row is what
// lets a later revert/fork inside the forked thread resolve its
// `lastTurnId` anchor without reaching back to the (possibly deleted)
// source thread.
//
// Forks call CloneThreadHistoryThroughTurn instead, which runs this and
// the item clone in one transaction; this entry point stands alone for
// callers that want only the turn half.
func (s *Store) CloneThreadTurns(sourceThreadID, targetThreadID string, throughTurnIndex *int) error {
	_, err := s.inCloneTx(func(tx *sql.Tx) (map[string]string, error) {
		return nil, cloneThreadTurnsTx(tx, sourceThreadID, targetThreadID, throughTurnIndex)
	})
	return err
}

func cloneThreadTurnsTx(tx *sql.Tx, sourceThreadID, targetThreadID string, throughTurnIndex *int) error {
	cut := -1
	if throughTurnIndex != nil {
		cut = *throughTurnIndex
	}
	_, err := tx.Exec(
		`INSERT INTO turns (turn_id, thread_id, turn_index, started_at, completed_at,
		    stop_reason, assistant_message_id, token_usage_json, error_message, provider_turn_id)
		 SELECT ? || ':' || turn_index, ?, turn_index, started_at, completed_at,
		    stop_reason, assistant_message_id, token_usage_json, error_message, provider_turn_id
		 FROM turns
		 WHERE thread_id = ? AND (? < 0 OR turn_index <= ?)`,
		targetThreadID, targetThreadID, sourceThreadID, cut, cut,
	)
	if err != nil {
		return fmt.Errorf("store: clone turns into thread %s: %w", targetThreadID, err)
	}
	return nil
}

// SettleForkedThreadAsInterrupted applies the standard interrupted
// treatment to a freshly-cloned fork: every stranded running/streaming
// item flips to `errored` with summarise(summary), and every turn row
// still open (`completed_at IS NULL`) closes with
// stop_reason='interrupted'. Both halves are exactly what
// RecoverCrashedTurns writes — they share settleStrandedItemsTx and the
// same stop_reason string — because the fork is in the same position a
// crash leftover is: its rows describe work no process will ever finish.
// A mid-turn fork is a snapshot "as if interrupted right now", so it
// settles the same way a real interrupt or the boot sweep would.
//
// Applied to the FORK ONLY, after the clone. The source thread is never
// touched — it keeps streaming under its own live session, which is the
// whole point of forking mid-turn.
//
// Backgrounded tool_call rows are exempt from the item flip (invariant
// 24), and on the fork path that exemption is LOAD-BEARING: the clone
// keeps every settled background launch — permanently `running` with
// its completion sibling, the designed terminal shape — and flipping
// one to errored here would rewrite finished work as interrupted. The
// truly-live (siblingless) background rows the exemption would
// otherwise strand never reach this settle: the clone drops them
// transitively.
//
// Safe to run unconditionally: an idle source clones no open rows, so
// both statements match nothing and the transaction is a no-op. Emits
// nothing — the fork is returned through the RPC response and rendered
// fresh, so there is no client holding a window of it to invalidate.
func (s *Store) SettleForkedThreadAsInterrupted(threadID string, summarise func(string) string, now int64) error {
	if threadID == "" {
		return fmt.Errorf("store: settle forked thread: thread id is required")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("store: begin fork settle tx for %s: %w", threadID, err)
	}
	defer tx.Rollback()

	if err := settleStrandedItemsTx(tx, threadID, nil, summarise, now); err != nil {
		return err
	}
	if _, err := tx.Exec(
		`UPDATE turns
		    SET completed_at = ?, stop_reason = 'interrupted'
		  WHERE thread_id = ? AND completed_at IS NULL`,
		now, threadID,
	); err != nil {
		return fmt.Errorf("store: fork settle turns for %s: %w", threadID, err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit fork settle tx for %s: %w", threadID, err)
	}
	return nil
}

// CloneThreadHistoryBeforeItem copies into targetThreadID everything that
// precedes the anchor item in PROVIDER order — the fork-side twin of
// DeleteConversationFromItem's kept-set, for providers whose fork cuts
// provider history at the message itself (Claude's session-file slice).
// Codex forks stay on the turn-granular CloneThreadHistoryThroughTurn,
// matching thread/fork's turn-boundary cut.
//
// Kept items: earlier turns, plus the anchor turn's rows before the anchor.
// An interrupt-promoted anchor (itemmeta promotion marker) additionally
// keeps its turn's content successors — everything but TOP-LEVEL user rows,
// i.e. the interrupted round's streamed tail including parented wire-only
// subagent prompts, which precede the promoted message in the provider
// transcript; same-turn top-level user successors are later-queued messages
// and stay behind. When the promoted row's echo stamped a provider-order
// boundary (mid-loop consumption whose response persisted in the same
// turn), content successors past it are that response and stay behind too. Turns rows clone where kept items exist;
// whenever the cut excludes same-turn non-user content the cloned turn row's
// settle metadata described it, so completed_at trims back to the last
// cloned row and assistant_message_id clears (token usage kept) — the same
// trim DeleteConversationFromItem applies.
//
// Every step — the anchor read, the item clone, the turn clone, the
// excluded-content probe and the settle trim — runs in ONE transaction,
// for the same reason CloneThreadHistoryThroughTurn does: a turn
// completing between the item read and the turn clone would give the
// fork a settled turn row over items snapshotted mid-stream. The fork
// saga's rollback stack still wraps the call and deletes the fork thread
// on any error.
func (s *Store) CloneThreadHistoryBeforeItem(sourceThreadID, targetThreadID, anchorItemID string) (map[string]string, error) {
	return s.inCloneTx(func(tx *sql.Tx) (map[string]string, error) {
		var turnIndex, itemIndex int
		var meta string
		if err := tx.QueryRow(
			`SELECT turn_index, item_index, meta FROM timeline_items WHERE thread_id = ? AND id = ?`,
			sourceThreadID, anchorItemID,
		).Scan(&turnIndex, &itemIndex, &meta); err != nil {
			return nil, fmt.Errorf("store: clone history anchor lookup %s/%s: %w", sourceThreadID, anchorItemID, err)
		}
		promotion, err := itemmeta.DecodePromotionState(meta)
		if err != nil {
			// Corrupt anchor meta means the provider-order cut is undecidable;
			// failing the fork beats silently cloning a set the session slice
			// would disagree with.
			return nil, fmt.Errorf("store: clone history anchor %s/%s: %w", sourceThreadID, anchorItemID, err)
		}

		idMap, err := cloneThreadItemsTx(tx, sourceThreadID, targetThreadID, func(item Item) bool {
			if item.TurnIndex != turnIndex {
				return item.TurnIndex < turnIndex
			}
			if item.ItemIndex < itemIndex {
				return true
			}
			// Only TOP-LEVEL user successors are later-queued messages that
			// stay behind; a parented wire-only user row (subagent prompt
			// nested under its tool_call) is interrupted-tail content like any
			// assistant row — the session slice retains it.
			if !promotion.Promoted || (item.Role == "user" && item.ParentID == "") {
				return false
			}
			return !promotion.HasEchoBoundary || item.ItemIndex <= promotion.EchoBoundary
		})
		if err != nil {
			return nil, err
		}

		if _, err := tx.Exec(
			`INSERT INTO turns (turn_id, thread_id, turn_index, started_at, completed_at,
			    stop_reason, assistant_message_id, token_usage_json, error_message, provider_turn_id)
			 SELECT ? || ':' || turn_index, ?, turn_index, started_at, completed_at,
			    stop_reason, assistant_message_id, token_usage_json, error_message, provider_turn_id
			 FROM turns
			 WHERE thread_id = ?
			   AND turn_index IN (SELECT DISTINCT turn_index FROM items WHERE thread_id = ?)`,
			targetThreadID, targetThreadID, sourceThreadID, targetThreadID,
		); err != nil {
			return nil, fmt.Errorf("store: clone turns before item into thread %s: %w", targetThreadID, err)
		}

		// Same content probe as DeleteConversationFromItem's, evaluated on the
		// SOURCE rows the keep predicate excluded: content rows (anything but
		// top-level user rows) after the anchor (plain) or past the echo
		// boundary (promoted).
		excludedContent := false
		probeAfter := -1
		switch {
		case !promotion.Promoted:
			probeAfter = itemIndex
		case promotion.HasEchoBoundary:
			probeAfter = promotion.EchoBoundary
		}
		if probeAfter >= 0 {
			if err := tx.QueryRow(
				`SELECT EXISTS(SELECT 1 FROM timeline_items
				  WHERE thread_id = ? AND turn_index = ? AND (role != 'user' OR parent_id != '') AND item_index > ?)`,
				sourceThreadID, turnIndex, probeAfter,
			).Scan(&excludedContent); err != nil {
				return nil, fmt.Errorf("store: probe excluded turn content for fork %s: %w", targetThreadID, err)
			}
		}
		if excludedContent {
			var lastKept sql.NullInt64
			if err := tx.QueryRow(
				`SELECT MAX(created_at) FROM items WHERE thread_id = ? AND turn_index = ?`,
				targetThreadID, turnIndex,
			).Scan(&lastKept); err != nil {
				return nil, fmt.Errorf("store: cloned turn survivors lookup for fork %s: %w", targetThreadID, err)
			}
			if lastKept.Valid {
				if _, err := tx.Exec(
					`UPDATE turns SET completed_at = MIN(completed_at, ?), assistant_message_id = ''
					 WHERE thread_id = ? AND turn_index = ? AND completed_at IS NOT NULL`,
					lastKept.Int64, targetThreadID, turnIndex,
				); err != nil {
					return nil, fmt.Errorf("store: trim cloned anchor turn settle for thread %s: %w", targetThreadID, err)
				}
			}
		}
		return idMap, nil
	})
}
