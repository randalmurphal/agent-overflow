package store

import (
	"database/sql"
	"errors"
	"fmt"
	"sort"
)

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
		       AND parent_id = ''
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

// HasQueueBlockingBackgroundToolCall is HasLiveBackgroundToolCall minus
// watch tasks (`meta.watch_task`, Claude's Monitor — claude-wire.md
// §E7). A watch observes; it never produces the result a queued user
// send could be waiting on, and a persistent watch runs until session
// end — counting it would starve the flush queue for hours. Only the
// flush-queue drain uses this variant: the reaper, revert gate, and
// context-repair gate all keep the full HasLiveBackgroundToolCall /
// ListRunningBackgroundToolCalls view because closing or restarting the
// session WOULD kill a running watch.
func (s *Store) HasQueueBlockingBackgroundToolCall(threadID string) (bool, error) {
	var exists int
	if err := s.db.QueryRow(
		`SELECT EXISTS(
		    SELECT 1 FROM items
		     WHERE thread_id = ?
		       AND kind = 'tool_call'
		       AND status = 'running'
		       AND is_background = 1
		       AND parent_id = ''
		       AND COALESCE(json_extract(meta, '$.live_background_active'), 1) != 0
		       AND COALESCE(json_extract(meta, '$.watch_task'), 0) = 0
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
		return false, fmt.Errorf("store: has queue-blocking background tool call for thread %s: %w", threadID, err)
	}
	return exists != 0, nil
}

func (s *Store) CountLiveRunningBackgroundToolCalls(threadID string) (int, error) {
	var count int
	if err := s.db.QueryRow(
		`SELECT COUNT(*)
		   FROM items INDEXED BY idx_items_live_background
		  WHERE thread_id = ?
		    AND kind = 'tool_call'
		    AND status = 'running'
		    AND is_background = 1
		    AND parent_id = ''
		    AND COALESCE(json_extract(meta, '$.live_background_active'), 1) != 0
		    AND NOT EXISTS (
		      SELECT 1 FROM items c INDEXED BY idx_items_completion_of
		       WHERE c.thread_id = items.thread_id
		         AND c.completion_of = items.id
		         AND c.completion_of <> ''
		    )`,
		threadID,
	).Scan(&count); err != nil {
		return 0, fmt.Errorf("store: count live running background tool calls for thread %s: %w", threadID, err)
	}
	return count, nil
}

func (s *Store) HasLiveCodexSubagentLaunch(threadID string) (bool, error) {
	var exists int
	if err := s.db.QueryRow(
		`SELECT EXISTS(
		    SELECT 1 FROM items
		    JOIN threads ON threads.id = items.thread_id
		     WHERE items.thread_id = ?
		       AND threads.provider = 'codex'
		       AND items.kind = 'tool_call'
		       AND items.status = 'completed'
		       AND items.tool_name = 'collab_agent'
		       AND items.is_background = 1
		       AND COALESCE(json_extract(items.meta, '$.live_background_active'), 1) != 0
		       AND json_extract(items.meta, '$.input.tool') IN ('spawn_agent', 'spawnAgent')
		       AND NOT EXISTS (
		         SELECT 1 FROM items c
		          WHERE c.thread_id = items.thread_id
		            AND c.completion_of = items.id
		       )
		     LIMIT 1
		)`,
		threadID,
	).Scan(&exists); err != nil {
		return false, fmt.Errorf("store: has live Codex subagent launch for thread %s: %w", threadID, err)
	}
	return exists != 0, nil
}

func (s *Store) CountLiveCodexSubagentLaunches(threadID string) (int, error) {
	var count int
	if err := s.db.QueryRow(
		`SELECT COUNT(*)
		   FROM items INDEXED BY idx_items_live_codex_subagent
		   JOIN threads ON threads.id = items.thread_id
		  WHERE items.thread_id = ?
		    AND threads.provider = 'codex'
		    AND items.kind = 'tool_call'
		    AND items.status = 'completed'
		    AND items.tool_name = 'collab_agent'
		    AND items.is_background = 1
		    AND COALESCE(json_extract(items.meta, '$.live_background_active'), 1) != 0
		    AND json_extract(items.meta, '$.input.tool') IN ('spawn_agent', 'spawnAgent')
		    AND NOT EXISTS (
		      SELECT 1 FROM items c INDEXED BY idx_items_completion_of
		       WHERE c.thread_id = items.thread_id
		         AND c.completion_of = items.id
		         AND c.completion_of <> ''
		    )`,
		threadID,
	).Scan(&count); err != nil {
		return 0, fmt.Errorf("store: count live Codex subagent launches for thread %s: %w", threadID, err)
	}
	return count, nil
}

func (s *Store) MarkLiveCodexSubagentLaunchesInactive(threadID string, updatedAt int64) (int64, error) {
	result, err := s.db.Exec(
		`UPDATE items
		    SET meta = json_set(meta, '$.live_background_active', json('false')),
		        updated_at = ?
		  WHERE thread_id = ?
		    AND kind = 'tool_call'
		    AND status = 'completed'
		    AND tool_name = 'collab_agent'
		    AND is_background = 1
		    AND COALESCE(json_extract(meta, '$.live_background_active'), 1) != 0
		    AND json_extract(meta, '$.input.tool') IN ('spawn_agent', 'spawnAgent')
		    AND NOT EXISTS (
		      SELECT 1 FROM items c
		       WHERE c.thread_id = items.thread_id
		         AND c.completion_of = items.id
		    )`,
		updatedAt,
		threadID,
	)
	if err != nil {
		return 0, fmt.Errorf("store: mark live Codex subagent launches inactive for thread %s: %w", threadID, err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("store: count inactive Codex subagent launches for thread %s: %w", threadID, err)
	}
	return count, nil
}

// MarkLiveBackgroundToolCallsInactive hides top-level running background
// tool_call rows whose owning provider session was intentionally closed.
// The rows are no longer live once the provider process group is gone, and
// the tray must stop advertising them as running. The launch row keeps its
// lifecycle status; terminal states belong to completion siblings.
func (s *Store) MarkLiveBackgroundToolCallsInactive(threadID string, updatedAt int64) (int64, error) {
	result, err := s.db.Exec(
		`UPDATE items
		    SET meta = json_set(
		          CASE WHEN json_valid(meta) THEN meta ELSE '{}' END,
		          '$.live_background_active',
		          json('false')
		        ),
		        updated_at = ?
		  WHERE thread_id = ?
		    AND kind = 'tool_call'
		    AND status = 'running'
		    AND is_background = 1
		    AND parent_id = ''
		    AND COALESCE(json_extract(meta, '$.live_background_active'), 1) != 0
		    AND NOT EXISTS (
		      SELECT 1 FROM items c
		       WHERE c.thread_id = items.thread_id
		         AND c.completion_of = items.id
		    )`,
		updatedAt,
		threadID,
	)
	if err != nil {
		return 0, fmt.Errorf("store: mark live background tool calls inactive for thread %s: %w", threadID, err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("store: count inactive background tool calls for thread %s: %w", threadID, err)
	}
	return count, nil
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

// ListLiveCodexSubagentLaunches returns Codex spawn_agent cards whose child
// threads are still active. The persisted spawn card is completed on the
// upstream wire; callers that render a "live work" surface should project the
// returned copy as running instead of changing the stored timeline row.
func (s *Store) ListLiveCodexSubagentLaunches(threadID string) ([]Item, error) {
	rows, err := s.db.Query(
		`SELECT `+itemColumns+`
		   FROM items
		   JOIN threads ON threads.id = items.thread_id
		   LEFT JOIN payloads ON payloads.id = items.payload_id
		  WHERE items.thread_id = ?
		    AND threads.provider = 'codex'
		    AND items.kind = 'tool_call'
		    AND items.status = 'completed'
		    AND items.tool_name = 'collab_agent'
		    AND items.is_background = 1
		    AND COALESCE(json_extract(items.meta, '$.live_background_active'), 1) != 0
		    AND json_extract(items.meta, '$.input.tool') IN ('spawn_agent', 'spawnAgent')
		    AND (
		      NOT EXISTS (
		        SELECT 1 FROM items c
		         WHERE c.thread_id = items.thread_id
		           AND c.completion_of = items.id
		      )
		      OR json_extract(items.meta, '$.live_background_active') = 1
		    )
		  ORDER BY items.turn_index, items.item_index`,
		threadID,
	)
	if err != nil {
		return nil, fmt.Errorf("store: list live Codex subagent launches for thread %s: %w", threadID, err)
	}
	defer rows.Close()

	var items []Item
	for rows.Next() {
		it, err := scanItemRow(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scan live Codex subagent launch row: %w", err)
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

// ListRecoverableClaudeBackgroundLaunches returns Claude backgrounded
// tool_call rows that startup recovery can safely settle. A recoverable
// launch is still running, still live, and has no completion sibling.
//
// It covers both the headless `claude` and interactive `claude-tui`
// providers — they share the Claude background-task lifecycle. claude-tui
// never reconstructs `system/task_started`, so its launches carry
// `is_background=1` with NO `task_id`; we therefore do NOT require one.
// The completion sibling is keyed off the launch id
// (backgroundCompletionID), so the synthetic completion is idempotent
// with or without a task_id. At boot no provider session from the
// previous app instance survives, so every still-running backgrounded
// launch is provably an orphan whose owning session is dead.
//
// This intentionally excludes Codex background projection rows. Codex
// owns those through the ghost-flip/reconcile path, and inactive Codex
// rows can remain status=running with live_background_active=false after
// every child has stopped. Treating those as Claude task orphans makes
// startup scan and log the same unrecoverable rows forever.
//
// Launches with a `pending_background_task_terminals` stash entry are
// included: at boot time no Claude provider session is alive yet, so any
// stash row is by definition orphaned (the observer that would drain it
// is dead). The recovery path drains the stash and uses its data when
// synthesising the completion sibling, so the user sees the real exit
// state rather than a generic session_died/killed badge.
func (s *Store) ListRecoverableClaudeBackgroundLaunches() ([]Item, error) {
	rows, err := s.db.Query(
		`SELECT ` + itemColumns + `
		   FROM items
		   JOIN threads ON threads.id = items.thread_id
		   LEFT JOIN payloads ON payloads.id = items.payload_id
		  WHERE threads.provider IN ('claude', 'claude-tui')
		    AND items.kind = 'tool_call'
		    AND items.status = 'running'
		    AND items.is_background = 1
		    AND COALESCE(json_extract(items.meta, '$.live_background_active'), 1) != 0
		    AND NOT EXISTS (
		      SELECT 1 FROM items c
		       WHERE c.thread_id = items.thread_id
		         AND c.completion_of = items.id
		    )`,
	)
	if err != nil {
		return nil, fmt.Errorf("store: list recoverable Claude background launches: %w", err)
	}
	defer rows.Close()

	var items []Item
	for rows.Next() {
		it, err := scanItemRow(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scan recoverable Claude bg launch row: %w", err)
		}
		items = append(items, it)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].ThreadID != items[j].ThreadID {
			return items[i].ThreadID < items[j].ThreadID
		}
		if items[i].TurnIndex != items[j].TurnIndex {
			return items[i].TurnIndex < items[j].TurnIndex
		}
		return items[i].ItemIndex < items[j].ItemIndex
	})
	return items, nil
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
