package store

import (
	"database/sql"
	"fmt"
)

// Thread-wide read queries that back dedicated frontend bindings. These
// exist so surfaces like PlanSidebar, DiffPanelDrawer, and
// BackgroundTaskTray — which need to see the whole thread, not just the
// currently-loaded paging window — don't re-derive from `pane.items`
// after the timeline moved to a windowed model. Each query is
// thread-scoped and returns the minimum item set the frontend needs to
// compute its own views; expensive per-item payload bodies are never
// loaded here (payload meta is carried via the standard LEFT JOIN, but
// `data` stays in the on-demand expansion path).

// ListThreadProposedPlans returns the current assistant-authored plan item for
// the thread whose joined payload_kind equals "proposed_plan". The result is a
// 0-or-1 item slice so the JSON response always carries a stable shape while
// avoiding history payloads the UI no longer presents.
//
// The `role = 'assistant'` filter keeps a user-authored item whose
// payload_kind happens to collide with 'proposed_plan' (possible via
// forks / imports) out of plan UI — only plans the agent actually
// proposed should appear.
func (s *Store) ListThreadProposedPlans(threadID string) ([]Item, error) {
	rows, err := s.reader().Query(
		`SELECT `+itemColumns+`
		   FROM proposed_plans
		   JOIN items
		     ON items.thread_id = proposed_plans.thread_id
		    AND items.id = proposed_plans.item_id
		   JOIN payloads ON payloads.thread_id = items.thread_id AND payloads.id = items.payload_id
		  WHERE proposed_plans.thread_id = ?
		    AND items.role = 'assistant'
		    AND payloads.kind = 'proposed_plan'
		  ORDER BY proposed_plans.version DESC
		  LIMIT 1`,
		threadID,
	)
	if err != nil {
		return nil, fmt.Errorf("store: list thread proposed plans for %s: %w", threadID, err)
	}
	defer rows.Close()

	out := []Item{}
	for rows.Next() {
		it, err := scanItemRow(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scan proposed plan row: %w", err)
		}
		out = append(out, it)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate proposed plans for %s: %w", threadID, err)
	}
	decorated, err := s.decorateProposedPlanItems(s.reader(), threadID, out)
	if err != nil {
		return nil, fmt.Errorf("store: decorate proposed plans for %s: %w", threadID, err)
	}
	return decorated, nil
}

func (s *Store) GetThreadProposedPlanItem(threadID, itemID string) (Item, bool, error) {
	row := s.reader().QueryRow(
		`SELECT `+itemColumns+`
		   FROM items
		   JOIN payloads ON payloads.thread_id = items.thread_id AND payloads.id = items.payload_id
		  WHERE items.thread_id = ?
		    AND items.id = ?
		    AND items.role = 'assistant'
		    AND payloads.kind = 'proposed_plan'
		  LIMIT 1`,
		threadID, itemID,
	)
	item, err := scanItemRow(row)
	if err == sql.ErrNoRows {
		return Item{}, false, nil
	}
	if err != nil {
		return Item{}, false, fmt.Errorf("store: get proposed plan item %s/%s: %w", threadID, itemID, err)
	}
	decorated, err := s.decorateProposedPlanItems(s.reader(), threadID, []Item{item})
	if err != nil {
		return Item{}, false, fmt.Errorf("store: decorate proposed plan item %s/%s: %w", threadID, itemID, err)
	}
	return decorated[0], true, nil
}

// ListLiveBackgroundTasks returns the tray's item set: live background
// launches plus their completion siblings whose `created_at` is inside
// the retention window.
//
// The tray lists by BACKGROUNDED ANCESTRY, not by top-level-ness
// (docs/specs/agent-visibility.md Q8). Three row classes qualify:
//
//  1. every live launch that is `is_background = 1`, at ANY depth — a
//     nested agent that backgrounded a Bash is running work the user
//     needs a handle on, and hiding it because its parent happened to
//     be an agent is how backgrounded work went invisible;
//  2. every live AGENT LAUNCH that descends from a background launch
//     (subagentLaunchFilterFor — structural, never a tool-name list).
//     Foreground PLAIN tool calls under a background agent stay out:
//     they are the agent's own work, rendered inside its card. Because
//     only a launch can be a parent, this class also supplies every
//     intermediate ancestor between a background root and a nested
//     launch, so the frontend indents by walking `parentId` WITHIN the
//     result instead of asking for rows it was not given;
//  3. the recent completion siblings of that same anchor set.
//
// A background process launch stays `status='running'` forever (spec
// invariant — the sibling completion row carries the final state). The
// launch and its completion must age out together: returning an orphan
// launch whose completion was pruned would re-render it as "running"
// indefinitely. A launch with no completion yet still surfaces unless
// it is marked inactive with `live_background_active=false` — which,
// since migration v74, the schema itself stamps the moment a completion
// sibling exists (background_settle_triggers.go), on top of the
// teardown/projection writers that always did.
//
// That is what lets the SEED be cheap. It is two index reads, never a
// walk of the thread's background history:
//
//   - every live launch (`idx_items_running_bg_tool_calls` — running,
//     background, flag set), at ANY depth, which post-v74 contains only
//     genuinely live rows;
//   - every launch named by a completion sibling inside the retention
//     window (`idx_items_completion_created`), which is how a
//     just-settled launch and its completion still leave together.
//
// The descendant walk then runs from that seed only, and the outer
// SELECT is driven FROM the resulting id set (`CROSS JOIN items`, so
// the planner cannot flip it back into a whole-thread scan) rather than
// filtering the thread. Before v74 the seed was "every background
// tool_call in the thread" and the walk covered every descendant they
// ever had: 75k page reads / 309MB / 120-200ms on a 38k-item thread to
// return between zero and eight rows, on every thread switch and after
// every background tool completion.
//
// This is the DISPLAY query only. The reaper and queue gates in
// items_lifecycle.go (HasRunningTopLevelForegroundToolCall,
// HasLiveBackgroundToolCall, HasQueueBlockingBackgroundToolCall,
// CountLiveRunningBackgroundToolCalls,
// MarkLiveBackgroundToolCallsInactive) and paging.go's
// topLevelItemsFilter KEEP the empty-`parent_id` term: whether the tray SHOWS a
// nested background Bash and whether that Bash blocks the flush queue
// or survives a session teardown are different questions, and the
// second one is still answered at the top level only.
//
// Codex subagents are different: the chat-history spawn card is completed
// immediately while the child thread keeps running. App.ListLiveBackgroundTasks
// adds those via Store.ListLiveCodexSubagentLaunches and projects the tray copy
// as running without mutating the stored card.
//
// One predicate MOVED with v74 and it is the reason the tray keeps
// looking the same. The launch branch used to read
// `flag != 0 AND (no sibling OR recent sibling)`; it now reads
// `(flag != 0 AND no sibling) OR recent sibling`. Before v74 the flag
// was independent of settlement, so the outer form held; now the
// trigger clears the flag AT settlement, and the outer form would drop
// a just-finished launch on the same read that still returns its
// completion — an orphan "-> done" row under nothing. The two forms
// differ on exactly one state: a launch a teardown marked inactive
// that LATER acquired a completion inside the window. The only writer
// of that mark (`markConfirmedBackgroundTasksInactiveAfterProviderCleanup`)
// truncates the thread immediately afterwards, so the state is not
// reachable in the app; where it did occur, showing the pair together
// is what the "age out together" rule above asks for anyway.
//
// Thread-scoped. Live launches surface regardless of turn_index.
// Ordering is (turn_index, item_index) so launches precede completions.
// liveBackgroundTasksSQL is the tray query. Built once, not per call:
// it is one of the two statements that run on every thread switch AND
// after every background tool completion, and the string concatenation
// is not free at that cadence.
//
// The two `INDEXED BY` hints on the seed are planner directives, not
// optimism. Both partial indexes are the whole point of the rewrite,
// and an empty or freshly-migrated `items` gives the planner no row
// stats to prefer them with (see the index-ordering note in
// schema_v1.go). `TestListLiveBackgroundTasksSeedUsesPartialIndexes`
// fails the moment a plan stops using either, or starts scanning the
// thread through `idx_items_thread_turn_item_unique`.
//
// The final `CROSS JOIN` is the same kind of directive: the candidate
// set is a co-routine of unknown size, and left to itself the planner
// drives from `items` and probes the candidates with an automatic
// index — which is exactly the whole-thread scan this query stopped
// doing. CROSS JOIN pins the loop order.
var liveBackgroundTasksSQL = `WITH RECURSIVE bg(id) AS (
		    SELECT id FROM items INDEXED BY idx_items_running_bg_tool_calls
		     WHERE thread_id = ?
		       AND kind = 'tool_call'
		       AND status = 'running'
		       AND is_background = 1
		       AND COALESCE(json_extract(meta, '$.live_background_active'), 1) != 0
		    UNION
		    SELECT c.completion_of
		      FROM items c INDEXED BY idx_items_completion_created
		     WHERE c.thread_id = ?
		       AND c.completion_of <> ''
		       AND c.created_at >= ?
		       AND EXISTS (
		         SELECT 1 FROM items l
		          WHERE l.thread_id = c.thread_id
		            AND l.id = c.completion_of
		            AND l.kind = 'tool_call'
		            AND l.is_background = 1
		       )
		),
		` + descendantsCTE("items", "SELECT id FROM bg") + `,
		anchors(id) AS (
		    SELECT id FROM bg
		    UNION
		    SELECT i.id
		      FROM rel
		      CROSS JOIN items i ON i.thread_id = ? AND i.id = rel.id
		     WHERE ` + subagentLaunchFilterFor("i.") + `
		),
		cand(id) AS (
		    SELECT id FROM anchors
		    UNION
		    SELECT c.id FROM items c INDEXED BY idx_items_completion_created
		     WHERE c.thread_id = ?
		       AND c.completion_of <> ''
		       AND c.created_at >= ?
		)
		SELECT ` + itemColumns + `
		   FROM cand
		   CROSS JOIN items ON items.thread_id = ? AND items.id = cand.id
		   LEFT JOIN payloads ON payloads.thread_id = items.thread_id AND payloads.id = items.payload_id
		  WHERE (
		      (
		        items.id IN (SELECT id FROM anchors)
		        AND items.kind = 'tool_call'
		        AND items.status = 'running'
		        AND (
		          (
		            COALESCE(json_extract(items.meta, '$.live_background_active'), 1) != 0
		            AND ` + noCompletionSiblingSQL + `
		          )
		          OR EXISTS (
		            SELECT 1 FROM items c
		             WHERE c.thread_id = items.thread_id
		               AND c.completion_of = items.id
		               AND c.completion_of <> ''
		               AND c.created_at >= ?
		          )
		        )
		      )
		      OR (
		        items.completion_of <> ''
		        AND items.created_at >= ?
		        AND items.completion_of IN (SELECT id FROM anchors)
		      )
		    )
		  ORDER BY items.turn_index, items.item_index`

func (s *Store) ListLiveBackgroundTasks(threadID string, retentionCutoffMillis int64) ([]Item, error) {
	rows, err := s.reader().Query(liveBackgroundTasksSQL,
		// bg seed: live launches (threadID), settled-in-window launches
		// (threadID, cutoff).
		threadID, threadID, retentionCutoffMillis,
		// descendants base hop (threadID), recursive hop (threadID),
		// anchor join (threadID).
		threadID, threadID, threadID,
		// candidate completion rows (threadID, cutoff).
		threadID, retentionCutoffMillis,
		// outer scope (threadID), launch completion window (cutoff),
		// sibling window (cutoff).
		threadID, retentionCutoffMillis, retentionCutoffMillis,
	)
	if err != nil {
		return nil, fmt.Errorf("store: list live background tasks for %s: %w", threadID, err)
	}
	defer rows.Close()

	out := []Item{}
	for rows.Next() {
		it, err := scanItemRow(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scan background task row: %w", err)
		}
		out = append(out, it)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate background tasks for %s: %w", threadID, err)
	}
	return out, nil
}
