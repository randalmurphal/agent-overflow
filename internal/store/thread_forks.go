package store

import (
	"database/sql"
	"fmt"
	"time"

	"github.com/google/uuid"

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
		ID:                 uuid.NewString(),
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
		DisabledMcpServers: source.DisabledMcpServers,
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
// Rows with `is_background=1 AND status='running'` are SKIPPED: those
// point at PTYs / subagents owned by the source session's provider
// subprocess, and the fork gets its own subprocess that can never reach
// them. Copying them would strand the forked thread with ghost rows
// that can never complete. The parent thread is untouched — its
// backgrounded launches keep running under its own session.
//
// Completed backgrounded rows and non-background running rows copy
// normally; the filter is deliberately narrow.
//
// All inserts run in a single transaction so a 200-row clone takes one
// fsync instead of 200. Per-row InsertItem would commit individually
// and dominate the fork wall-clock for large threads.
func (s *Store) CloneThreadItems(sourceThreadID, targetThreadID string, throughTurnIndex *int) (map[string]string, error) {
	return s.cloneThreadItems(sourceThreadID, targetThreadID, func(item Item) bool {
		return throughTurnIndex == nil || item.TurnIndex <= *throughTurnIndex
	})
}

// cloneThreadItems is the shared clone body behind CloneThreadItems and
// CloneThreadHistoryBeforeItem: keep decides which source rows copy;
// the background-running skip applies on top of it unconditionally.
func (s *Store) cloneThreadItems(sourceThreadID, targetThreadID string, keep func(Item) bool) (map[string]string, error) {
	items, err := s.ListItems(sourceThreadID)
	if err != nil {
		return nil, fmt.Errorf("store: list source items for fork %s: %w", sourceThreadID, err)
	}

	clonedItems := make([]Item, 0, len(items))
	idMap := make(map[string]string, len(items))
	for _, item := range items {
		if item.IsBackground && item.Status == "running" {
			continue
		}
		if !keep(item) {
			continue
		}
		oldID := item.ID
		item.ID = uuid.NewString()
		item.ThreadID = targetThreadID
		idMap[oldID] = item.ID
		clonedItems = append(clonedItems, item)
	}
	for i := range clonedItems {
		if next, ok := idMap[clonedItems[i].ParentID]; ok {
			clonedItems[i].ParentID = next
		}
		if next, ok := idMap[clonedItems[i].CompletionOf]; ok {
			clonedItems[i].CompletionOf = next
		}
	}

	tx, err := s.db.Begin()
	if err != nil {
		return nil, fmt.Errorf("store: begin clone items tx: %w", err)
	}
	defer tx.Rollback()

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

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("store: commit clone items tx: %w", err)
	}
	return idMap, nil
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
func (s *Store) CloneThreadTurns(sourceThreadID, targetThreadID string, throughTurnIndex *int) error {
	cut := -1
	if throughTurnIndex != nil {
		cut = *throughTurnIndex
	}
	_, err := s.db.Exec(
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

// CloneThreadHistoryBeforeItem copies into targetThreadID everything that
// precedes the anchor item in PROVIDER order — the fork-side twin of
// DeleteConversationFromItem's kept-set, for providers whose fork cuts
// provider history at the message itself (Claude's session-file slice).
// Codex forks stay on the turn-granular CloneThreadItems/CloneThreadTurns
// pair, matching thread/fork's turn-boundary cut.
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
// Like the CloneThreadItems + CloneThreadTurns pair, the steps are not one
// SQL transaction: every caller wraps the clone in the fork saga's rollback
// stack, which deletes the fork thread (and cascades its rows) on failure.
func (s *Store) CloneThreadHistoryBeforeItem(sourceThreadID, targetThreadID, anchorItemID string) (map[string]string, error) {
	var turnIndex, itemIndex int
	var meta string
	if err := s.reader().QueryRow(
		`SELECT turn_index, item_index, meta FROM items WHERE thread_id = ? AND id = ?`,
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

	idMap, err := s.cloneThreadItems(sourceThreadID, targetThreadID, func(item Item) bool {
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

	if _, err := s.db.Exec(
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
	switch {
	case !promotion.Promoted:
		if err := s.reader().QueryRow(
			`SELECT EXISTS(SELECT 1 FROM items
			  WHERE thread_id = ? AND turn_index = ? AND (role != 'user' OR parent_id != '') AND item_index > ?)`,
			sourceThreadID, turnIndex, itemIndex,
		).Scan(&excludedContent); err != nil {
			return nil, fmt.Errorf("store: probe excluded turn content for fork %s: %w", targetThreadID, err)
		}
	case promotion.HasEchoBoundary:
		if err := s.reader().QueryRow(
			`SELECT EXISTS(SELECT 1 FROM items
			  WHERE thread_id = ? AND turn_index = ? AND (role != 'user' OR parent_id != '') AND item_index > ?)`,
			sourceThreadID, turnIndex, promotion.EchoBoundary,
		).Scan(&excludedContent); err != nil {
			return nil, fmt.Errorf("store: probe excluded turn content for fork %s: %w", targetThreadID, err)
		}
	}
	if excludedContent {
		var lastKept sql.NullInt64
		if err := s.reader().QueryRow(
			`SELECT MAX(created_at) FROM items WHERE thread_id = ? AND turn_index = ?`,
			targetThreadID, turnIndex,
		).Scan(&lastKept); err != nil {
			return nil, fmt.Errorf("store: cloned turn survivors lookup for fork %s: %w", targetThreadID, err)
		}
		if lastKept.Valid {
			if _, err := s.db.Exec(
				`UPDATE turns SET completed_at = MIN(completed_at, ?), assistant_message_id = ''
				 WHERE thread_id = ? AND turn_index = ? AND completed_at IS NOT NULL`,
				lastKept.Int64, targetThreadID, turnIndex,
			); err != nil {
				return nil, fmt.Errorf("store: trim cloned anchor turn settle for thread %s: %w", targetThreadID, err)
			}
		}
	}
	return idMap, nil
}
