package store

import (
	"database/sql"
	"fmt"
)

// BackgroundTaskRetentionMillis is how long a settled launch and its
// completion sibling stay in a tray read: the retention cutoff a caller
// passes is now minus this. The frontend prunes a settled pair sooner
// (COMPLETION_RETENTION_MS in activityRailBackground.svelte.ts); the
// window here only has to outlast the delivery of the settle.
const BackgroundTaskRetentionMillis = 2000

// ListLiveBackgroundTasks returns the tray's item set: live background
// launches plus their completion siblings whose `created_at` is inside
// the retention window.
//
// The tray lists by BACKGROUNDED ANCESTRY, not by top-level-ness
// (docs/specs/agent-visibility.md Q8). Three row classes qualify:
//
//  1. every live launch that is `is_background = 1`, at ANY depth: a
//     nested agent that backgrounded a Bash is running work the user
//     needs a handle on, and hiding it because its parent happened to
//     be an agent is how backgrounded work went invisible;
//  2. every live AGENT LAUNCH that descends from a background launch
//     (subagentLaunchFilterFor: structural, never a tool-name list).
//     Foreground PLAIN tool calls under a background agent stay out:
//     they are the agent's own work, rendered inside its card. Because
//     only a launch can be a parent, this class also supplies every
//     intermediate ancestor between a background root and a nested
//     launch, so the frontend indents by walking `parentId` WITHIN the
//     result instead of asking for rows it was not given;
//  3. the recent completion siblings of that same anchor set.
//
// A background process launch stays `status='running'` forever (spec
// invariant: the sibling completion row carries the final state). The
// launch and its completion must age out together: returning an orphan
// launch whose completion was pruned would re-render it as "running"
// indefinitely. A launch with no completion yet still surfaces unless
// it is marked inactive with `live_background_active=false`, which,
// since migration v74, the schema itself stamps the moment a completion
// sibling exists (background_settle_triggers.go), on top of the
// teardown/projection writers that always did.
//
// That is what lets the SEED be cheap. It is two index reads, never a
// walk of the thread's background history:
//
//   - every live launch (`idx_items_running_bg_tool_calls`: running,
//     background, flag set), at ANY depth, which post-v74 contains only
//     genuinely live rows;
//   - every launch named by a completion sibling inside the retention
//     window (`idx_items_completion_created`), which is how a
//     just-settled launch and its completion still leave together.
//
// Class 2 is found from the other end. Its candidates are the thread's
// running foreground tool calls below the top level
// (idx_items_running_nested_fg_tool_calls; a foreground call is
// transient, so the index holds the calls in flight) and the launches
// named by a completion inside the window. Each candidate walks up its
// parent chain by primary key until it meets the seed. The walk costs
// the candidates times their depth, never the size of a background
// agent's subtree.
//
// The outer SELECT is driven FROM the resulting id set (`CROSS JOIN
// items`, so the planner cannot flip it back into a whole-thread scan)
// rather than filtering the thread. Before v74 the seed was "every
// background tool_call in the thread" and the walk covered every
// descendant they ever had: 75k page reads / 309MB / 120-200ms on a
// 38k-item thread to return between zero and eight rows, on every thread
// switch and after every background tool completion.
//
// This is the DISPLAY query only. The reaper and queue gates in
// items_lifecycle.go (HasRunningTopLevelForegroundToolCall,
// HasLiveBackgroundToolCall, HasQueueBlockingBackgroundToolCall,
// MarkLiveBackgroundToolCallsInactive) and paging.go's
// topLevelItemsFilter KEEP the empty-`parent_id` term: whether the tray SHOWS a
// nested background Bash and whether that Bash blocks the flush queue
// are different questions, and the second one is still answered at the
// top level only. CountLiveRunningBackgroundToolCalls counts every depth:
// a session stop kills a nested launch too.
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
// completion: an orphan "-> done" row under nothing. The two forms
// differ on exactly one state: a launch a teardown marked inactive
// that LATER acquired a completion inside the window. The only writer
// of that mark (`markConfirmedBackgroundTasksInactiveAfterProviderCleanup`)
// truncates the thread immediately afterwards, so the state is not
// reachable in the app; where it did occur, showing the pair together
// is what the "age out together" rule above asks for anyway.
//
// A parked stop (agent_stops.go) is not one of the completions: it does
// not settle its launch, which stays in the set as a live row serving the
// pause as its run state.
//
// Thread-scoped. Live launches surface regardless of turn_index.
// Ordering is (turn_index, item_index) so launches precede completions.
//
// Every row is served as ListLiveBackgroundTasks returns it, with a
// Claude background agent's run state (serveAgentRunStates) read by the
// same statement. ListBackgroundTrayRows reads the rows of named launches
// by the same rules at a cost independent of the thread's other tasks: it
// is what a tray delta carries.
func (s *Store) ListLiveBackgroundTasks(threadID string, retentionCutoffMillis int64) ([]Item, error) {
	items, err := s.readTrayRows(threadID, liveBackgroundTasksSQL, threadID, retentionCutoffMillis)
	if err != nil {
		return nil, fmt.Errorf("store: list live background tasks for %s: %w", threadID, err)
	}
	return items, nil
}

// ListBackgroundTrayRows returns the rows ListLiveBackgroundTasks would
// return for the named launches and for the resume carriers stamped with
// a named launch as their transcript root: each launch that qualifies,
// and its completion sibling inside the retention window. A launch that
// does not qualify returns no row. A carrier comes with its root because
// its run state reads the wakes filed under its root (trayProjectionSQL),
// and a wake names the root. Its statement starts from the named ids
// and walks up their parent chains, so its cost does not grow with the
// thread's other background tasks.
func (s *Store) ListBackgroundTrayRows(threadID string, retentionCutoffMillis int64, launchIDs []string) ([]Item, error) {
	if len(launchIDs) == 0 {
		return []Item{}, nil
	}
	ids, err := jsonList(launchIDs)
	if err != nil {
		return nil, err
	}
	items, err := s.readTrayRows(threadID, backgroundTrayRowsSQL, threadID, retentionCutoffMillis, ids)
	if err != nil {
		return nil, fmt.Errorf("store: list background tray rows for %s: %w", threadID, err)
	}
	return items, nil
}

// readTrayRows runs one tray statement and serves its rows: the run state
// from the stop the statement read, then the latest-tool line a clean
// stamped launch already carries and the rest read.
func (s *Store) readTrayRows(threadID, query string, args ...any) ([]Item, error) {
	rows, err := s.reader().Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []Item{}
	stops := []parkedRun{}
	for rows.Next() {
		var stop parkedRun
		var commands sql.NullInt64
		var reportID, preview sql.NullString
		it, err := scanItemRow(trayRowScanner{rows: rows, extra: []any{&stop.parked, &commands, &reportID, &preview}})
		if err != nil {
			return nil, fmt.Errorf("scan background task row: %w", err)
		}
		stop.commands, stop.reportID, stop.preview = commands.Int64, reportID.String, preview.String
		out = append(out, it)
		stops = append(stops, stop)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate background task rows: %w", err)
	}
	if err := serveAgentRunStates(out, stops); err != nil {
		return nil, err
	}
	// A clean stamped launch carries its tray activity; the rest read it.
	return s.decorateLatestDirectSubagentTools(s.reader(), threadID, out)
}

// trayRowScanner scans a tray row: the item's columns, then the parked
// stop the statement appends to them.
type trayRowScanner struct {
	rows  *sql.Rows
	extra []any
}

func (r trayRowScanner) Scan(dest ...any) error {
	return r.rows.Scan(append(dest, r.extra...)...)
}

// liveBackgroundTasksSQL is the tray query. The two `INDEXED BY` hints
// on the seed are planner directives, not optimism. Both partial indexes
// are the whole point of the rewrite, and an empty or freshly-migrated
// `items` gives the planner no row stats to prefer them with (see the index-ordering note in
// schema_v1.go). `TestListLiveBackgroundTasksSeedUsesPartialIndexes`
// fails the moment a plan stops using either, or starts scanning the
// thread through `idx_items_thread_turn_item_unique`.
//
// The final `CROSS JOIN` is the same kind of directive: the candidate
// set is a co-routine of unknown size, and left to itself the planner
// drives from `items` and probes the candidates with an automatic
// index, which is exactly the whole-thread scan this query stopped
// doing. CROSS JOIN pins the loop order.
//
// The statements bind ?1 to the thread id and ?2 to the retention cutoff;
// the keyed form binds ?3 to the JSON list of named launches. They bind
// nothing else (TestTrayStatementsBindNoLimit).
var liveBackgroundTasksSQL = `WITH RECURSIVE bg(id) AS (
		    SELECT id FROM items INDEXED BY idx_items_running_bg_tool_calls
		     WHERE thread_id = ?1
		       AND kind = 'tool_call'
		       AND status = 'running'
		       AND is_background = 1
		       AND COALESCE(json_extract(meta, '$.live_background_active'), 1) != 0
		    UNION
		    SELECT c.completion_of
		      FROM items c INDEXED BY idx_items_completion_created
		     WHERE c.thread_id = ?1
		       AND c.completion_of <> ''
		       AND c.created_at >= ?2
		       AND c.status <> 'parked'
		       AND EXISTS (
		         SELECT 1 FROM items l
		          WHERE l.thread_id = c.thread_id
		            AND l.id = c.completion_of
		            AND l.kind = 'tool_call'
		            AND l.is_background = 1
		       )
		),
		-- Class 2 candidates: nested foreground calls in flight, and the
		-- launches recent completions name.
		nested(id) AS (
		    SELECT id FROM items INDEXED BY idx_items_running_nested_fg_tool_calls
		     WHERE thread_id = ?1
		       AND kind = 'tool_call'
		       AND status = 'running'
		       AND is_background = 0
		       AND parent_id <> ''
		    UNION
		    SELECT c.completion_of
		      FROM items c INDEXED BY idx_items_completion_created
		     WHERE c.thread_id = ?1
		       AND c.completion_of <> ''
		       AND c.created_at >= ?2
		),
		-- Each candidate's chain upward through visible rows, stopping at
		-- the first seed row: the rows the old descendant walk would have
		-- passed through to reach it.
		up(start, id, parent_id) AS (
		    SELECT i.id, i.id, i.parent_id
		      FROM nested
		      CROSS JOIN items i ON i.thread_id = ?1 AND i.id = nested.id
		     WHERE i.parent_id <> '' AND ` + visibleItemsFilterFor("i.") + `
		    UNION
		    SELECT up.start, p.id, p.parent_id
		      FROM up
		      CROSS JOIN items p ON p.thread_id = ?1 AND p.id = up.parent_id
		     WHERE up.parent_id NOT IN (SELECT id FROM bg)
		       AND p.parent_id <> '' AND ` + visibleItemsFilterFor("p.") + `
		),
		anchors(id) AS (
		    SELECT id FROM bg
		    UNION
		    SELECT i.id
		      FROM up
		      CROSS JOIN items i ON i.thread_id = ?1 AND i.id = up.start
		     WHERE up.parent_id IN (SELECT id FROM bg)
		       AND ` + subagentLaunchFilterFor("i.") + `
		),
		cand(id) AS (
		    SELECT id FROM anchors
		    UNION
		    SELECT c.id FROM items c INDEXED BY idx_items_completion_created
		     WHERE c.thread_id = ?1
		       AND c.completion_of <> ''
		       AND c.created_at >= ?2
		),
		` + trayProjectionSQL

// backgroundTraySeedSQL is a row the whole read seeds its walk from (the
// bg CTE there): a live background launch, or a background launch a
// completion inside the window names.
func backgroundTraySeedSQL(idExpr string) string {
	return `EXISTS (
		      SELECT 1 FROM items b
		       WHERE b.thread_id = ?1
		         AND b.id = ` + idExpr + `
		         AND b.kind = 'tool_call'
		         AND b.is_background = 1
		         AND (
		           (b.status = 'running' AND COALESCE(json_extract(b.meta, '$.live_background_active'), 1) != 0)
		           OR EXISTS (
		             SELECT 1 FROM items c INDEXED BY idx_items_completion_of
		              WHERE c.thread_id = b.thread_id
		                AND c.completion_of = b.id
		                AND c.completion_of <> ''
		                AND c.created_at >= ?2
		                AND c.status <> 'parked'
		           )
		         )
		    )`
}

// backgroundTrayRowsSQL is liveBackgroundTasksSQL for named launches. The
// seed, the class 2 candidates and the anchors are the whole read's,
// asked of each named launch and of the rows on its chain instead of
// enumerated over the thread (TestListBackgroundTrayRowsMatchesTheWholeRead).
// The named set adds each named launch's carriers through
// idx_items_transcript_root.
var backgroundTrayRowsSQL = `WITH RECURSIVE asked(id) AS (
		    SELECT DISTINCT value FROM json_each(?3) WHERE type = 'text' AND value <> ''
		),
		named(id) AS (
		    SELECT id FROM asked
		    UNION
		    SELECT carrier.id
		      FROM asked
		      CROSS JOIN items carrier
		        ON carrier.thread_id = ?1
		       AND json_extract(carrier.meta, '$.` + metaKeyTranscriptRootID + `') = asked.id
		),
		up(start, id, parent_id) AS (
		    SELECT i.id, i.id, i.parent_id
		      FROM named
		      CROSS JOIN items i ON i.thread_id = ?1 AND i.id = named.id
		     WHERE i.parent_id <> '' AND ` + visibleItemsFilterFor("i.") + `
		       AND (
		         (i.kind = 'tool_call' AND i.status = 'running' AND i.is_background = 0)
		         OR EXISTS (
		           SELECT 1 FROM items c INDEXED BY idx_items_completion_of
		            WHERE c.thread_id = i.thread_id
		              AND c.completion_of = i.id
		              AND c.completion_of <> ''
		              AND c.created_at >= ?2
		         )
		       )
		    UNION
		    SELECT up.start, p.id, p.parent_id
		      FROM up
		      CROSS JOIN items p ON p.thread_id = ?1 AND p.id = up.parent_id
		     WHERE NOT ` + backgroundTraySeedSQL("up.parent_id") + `
		       AND p.parent_id <> '' AND ` + visibleItemsFilterFor("p.") + `
		),
		anchors(id) AS (
		    SELECT named.id FROM named WHERE ` + backgroundTraySeedSQL("named.id") + `
		    UNION
		    SELECT i.id
		      FROM up
		      CROSS JOIN items i ON i.thread_id = ?1 AND i.id = up.start
		     WHERE ` + backgroundTraySeedSQL("up.parent_id") + `
		       AND ` + subagentLaunchFilterFor("i.") + `
		),
		cand(id) AS (
		    SELECT id FROM named
		    UNION
		    SELECT c.id
		      FROM named
		      CROSS JOIN items c INDEXED BY idx_items_completion_of
		         ON c.thread_id = ?1 AND c.completion_of = named.id AND c.completion_of <> ''
		     WHERE c.created_at >= ?2
		),
		` + trayProjectionSQL

// trayProjectionSQL selects the tray rows from the candidates, with the
// stop a served run state reads (serveAgentRunStates): a background
// launch's newest stop, and when that stop is parked and no wake under the
// launch's transcript root has followed it (Store.CurrentParkedStop), what
// it recorded. A carrier's root is the transcript_root_id the parser or
// its backgrounding flip (resumeCarrierIdentity) stamped on it; any other
// launch is its own root. The newest stop is one index step per launch
// and the wake probe runs only for a parked one; the rows are
// materialized so each launch costs it once. A launch's completion
// sibling is probed by its key (idx_items_completion_of), never by
// scanning the retention window. A parked sibling is not a completion
// (agent_stops.go): it neither seeds nor ages out the launch, and is not
// a row of the tray.
var trayProjectionSQL = `tray(id, thread_id, root, stop) AS MATERIALIZED (
		    SELECT items.id, items.thread_id,
		           COALESCE(NULLIF(trim(json_extract(items.meta, '$.transcript_root_id')), ''), items.id),
		           CASE WHEN items.kind = 'tool_call' AND items.is_background = 1 AND items.completion_of = ''
		                THEN (` + newestStopIDSQL("items.thread_id", "items.id") + `)
		                END
		      FROM cand
		      CROSS JOIN items ON items.thread_id = ?1 AND items.id = cand.id
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
		               SELECT 1 FROM items c INDEXED BY idx_items_completion_of
		                WHERE c.thread_id = items.thread_id
		                  AND c.completion_of = items.id
		                  AND c.completion_of <> ''
		                  AND c.created_at >= ?2
		                  AND c.status <> 'parked'
		             )
		           )
		         )
		         OR (
		           items.completion_of <> ''
		           AND items.created_at >= ?2
		           AND items.status <> 'parked'
		           AND items.completion_of IN (SELECT id FROM anchors)
		         )
		       )
		)
		SELECT ` + itemColumns + `,
		       parked.id IS NOT NULL,
		       json_extract(parked.meta, '$.` + MetaKeyParkedCommands + `'),
		       json_extract(parked.meta, '$.` + MetaKeyParkedReportItemID + `'),
		       CASE WHEN json_valid(parked_payload.meta) THEN json_extract(parked_payload.meta, '$.preview') END
		   FROM tray
		   CROSS JOIN items ON items.thread_id = tray.thread_id AND items.id = tray.id
		   LEFT JOIN payloads ON payloads.thread_id = items.thread_id AND payloads.id = items.payload_id` + servedItemJoin + `
		   LEFT JOIN items parked
		     ON parked.thread_id = tray.thread_id
		    AND parked.id = tray.stop
		    AND parked.status = 'parked'
		    AND NOT EXISTS (` + agentWakesSinceSQL("tray.thread_id", "tray.root", "parked.created_at") + `)
		   LEFT JOIN payloads parked_payload
		     ON parked_payload.thread_id = parked.thread_id AND parked_payload.id = parked.payload_id
		  ORDER BY items.turn_index, items.item_index`
