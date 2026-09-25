package store

import (
	"database/sql"
	"fmt"
	"slices"
	"strings"
)

// Migrations v126 and v127 make a parked agent stop (agent_stops.go) a
// row of its own: a completion-shaped sibling with status 'parked'.
//
//   - v126 admits the status and reinstalls the background settle
//     triggers, which from then on settle a launch only on an ending
//     sibling (background_settle_triggers.go). The items CHECK is widened
//     in place (widenItemsStatusCheck): a loosened CHECK holds for every
//     stored row, so the table is not rebuilt.
//   - v127 converts the bells earlier builds rang at a parked stop into
//     parked siblings at their positions, and deletes the agent bells a
//     completion sibling covers, which the frontend used to hide. It
//     reinstalls the pointer-fork triggers, whose revive-on-move trigger
//     passed over a parked stop like the settle triggers do, until v133
//     dropped it.
const parkedStopMigrationVersion = 126

const parkedAgentStopsV126SQL = dropBackgroundSettleTriggersV126SQL + "\n" + backgroundSettleTriggersV126SQL

// itemsStatusCheckOpen opens the items status CHECK in the table's DDL.
const itemsStatusCheckOpen = "CHECK(status IN ("

// itemsStatusesBeforeV126 is the status set the CHECK admits before v126.
var itemsStatusesBeforeV126 = []string{"'streaming'", "'running'", "'completed'", "'errored'", "'declined'", "'killed'"}

// widenItemsStatusCheck adds 'parked' to the items status CHECK by
// rewriting the table's stored DDL (SQLite's writable_schema procedure
// for loosening a constraint) and bumping schema_version, so every
// connection reloads the schema. It fails unless the CHECK lists exactly
// the statuses before v126.
func widenItemsStatusCheck(tx *sql.Tx) error {
	var ddl string
	if err := tx.QueryRow(`SELECT sql FROM sqlite_master WHERE type = 'table' AND name = 'items'`).Scan(&ddl); err != nil {
		return fmt.Errorf("read the items DDL: %w", err)
	}
	widened, err := widenStatusCheckDDL(ddl)
	if err != nil {
		return err
	}
	var version int64
	if err := tx.QueryRow(`PRAGMA schema_version`).Scan(&version); err != nil {
		return fmt.Errorf("read schema_version: %w", err)
	}
	if _, err := tx.Exec(`PRAGMA writable_schema = ON`); err != nil {
		return fmt.Errorf("enable writable_schema: %w", err)
	}
	_, updateErr := tx.Exec(`UPDATE sqlite_master SET sql = ? WHERE type = 'table' AND name = 'items'`, widened)
	var bumpErr error
	if updateErr == nil {
		_, bumpErr = tx.Exec(fmt.Sprintf(`PRAGMA schema_version = %d`, version+1))
	}
	if _, err := tx.Exec(`PRAGMA writable_schema = OFF`); err != nil {
		return fmt.Errorf("disable writable_schema: %w", err)
	}
	if updateErr != nil {
		return fmt.Errorf("rewrite the items DDL: %w", updateErr)
	}
	if bumpErr != nil {
		return fmt.Errorf("bump schema_version: %w", bumpErr)
	}
	return nil
}

// widenStatusCheckDDL returns the items DDL with 'parked' appended to its
// status CHECK list.
func widenStatusCheckDDL(ddl string) (string, error) {
	if n := strings.Count(ddl, itemsStatusCheckOpen); n != 1 {
		return "", fmt.Errorf("the items DDL has %d status CHECKs, want 1", n)
	}
	start := strings.Index(ddl, itemsStatusCheckOpen) + len(itemsStatusCheckOpen)
	end := strings.Index(ddl[start:], ")")
	if end < 0 {
		return "", fmt.Errorf("the items status CHECK is not closed")
	}
	list := ddl[start : start+end]
	var statuses []string
	for _, status := range strings.Split(list, ",") {
		statuses = append(statuses, strings.TrimSpace(status))
	}
	if !slices.Equal(statuses, itemsStatusesBeforeV126) {
		return "", fmt.Errorf("the items status CHECK admits %v, want %v", statuses, itemsStatusesBeforeV126)
	}
	body := strings.TrimRight(list, " \t\r\n")
	return ddl[:start] + body + ",\n        '" + ItemStatusParked + "'" + list[len(body):] + ddl[start+end:], nil
}

// parkedAgentStopsV127SQL converts the bells builds before v127 rang for
// agents into the rows the stops are now.
//
//   - A parked bell (meta kind 'parked_agent') becomes, in place, the
//     parked sibling of the row its stop belonged to: the tool_use its
//     notification named, else the newest tool_call bound to the task at
//     the stop (a woken run's notification names none). It keeps its id
//     and position. Its report head, the stored preview cut to the 240
//     characters a completion preview holds, becomes its payload, and its
//     run facts are read from the wake rows under the agent's transcript
//     root. Only top-level agents rang one.
//   - An agent's other bells repeat the ending sibling of its task, which
//     renders the report; the frontend no longer hides them, so they go.
//
// A pointer fork reads its ancestors' rows in place, and the fork triggers
// refuse a change to a row a fork shows. This one is a change of
// representation, not of history, and applies to every thread that shows
// the row alike, so the triggers are dropped for it and reinstalled after,
// and each fork that showed a converted or deleted row takes a new epoch.
//
// Every bell carries a task_id, so both scans read idx_items_meta_task_id.
var parkedAgentStopsV127SQL = dropForkTriggersV127SQL + `
CREATE TEMP TABLE v127_parked_stops (
    thread_id      TEXT    NOT NULL,
    id             TEXT    NOT NULL,
    created_at     INTEGER NOT NULL,
    bell_meta      TEXT    NOT NULL,
    launch_id      TEXT    NOT NULL,
    launch_created INTEGER NOT NULL,
    launch_summary TEXT    NOT NULL,
    launch_tool    TEXT    NOT NULL,
    root_id        TEXT    NOT NULL,
    preview        TEXT    NOT NULL,
    woke_at        INTEGER,
    PRIMARY KEY (thread_id, id)
);

INSERT INTO v127_parked_stops (thread_id, id, created_at, bell_meta, launch_id, launch_created,
                               launch_summary, launch_tool, root_id, preview)
SELECT b.thread_id, b.id, b.created_at, b.meta, l.id, l.created_at, l.summary, l.tool_name,
       COALESCE(NULLIF(json_extract(l.meta, '$.transcript_root_id'), ''), l.id),
       trim(replace(replace(COALESCE(json_extract(b.meta, '$.parked_report_preview'), ''),
                            char(13), ' '), char(10), ' '))
  FROM items b INDEXED BY idx_items_meta_task_id
  JOIN items l
    ON l.thread_id = b.thread_id
   AND l.kind = 'tool_call'
   AND l.id = COALESCE(
         (SELECT x.id FROM items x
           WHERE x.thread_id = b.thread_id AND x.id = json_extract(b.meta, '$.tool_use_id') AND x.kind = 'tool_call'),
         (SELECT x.id FROM items x
           WHERE x.thread_id = b.thread_id
             AND json_extract(x.meta, '$.task_id') = json_extract(b.meta, '$.task_id')
             AND x.kind = 'tool_call'
             AND x.created_at <= b.created_at
           ORDER BY x.created_at DESC, x.turn_index DESC, x.item_index DESC
           LIMIT 1))
 WHERE json_extract(b.meta, '$.task_id') IS NOT NULL
   AND b.kind = 'notification'
   AND b.parent_id = ''
   AND json_extract(b.meta, '$.kind') = 'parked_agent';

UPDATE v127_parked_stops
   SET preview = rtrim(substr(preview, 1, 239)) || '…'
 WHERE length(preview) > 240;

-- A run a wake started began at the wake: the newest wake row under the
-- agent's root since the previous stop of the same row (or the row's
-- start), up to this stop.
UPDATE v127_parked_stops AS s
   SET woke_at = (
     SELECT MAX(w.created_at) FROM items w
      WHERE w.thread_id = s.thread_id
        AND w.parent_id = s.root_id
        AND w.parent_id <> ''
        AND w.kind = 'user_text'
        AND w.created_at >= COALESCE(
              (SELECT MAX(prev.created_at) FROM v127_parked_stops prev
                WHERE prev.thread_id = s.thread_id AND prev.launch_id = s.launch_id
                  AND prev.created_at < s.created_at),
              s.launch_created)
        AND w.created_at <= s.created_at
        AND CASE WHEN json_valid(w.meta) THEN json_extract(w.meta, '$.subagent_wake_prompt') END = 1);

INSERT INTO payloads (thread_id, id, kind, meta, data, created_at)
SELECT s.thread_id, 'tool-call-result:' || s.id, 'tool_call_result',
       json_patch(json_object('outputFileState', 'loaded', 'preview', s.preview),
                  json_object('outputFile', json_extract(s.bell_meta, '$.output_file'))),
       X'', s.created_at
  FROM v127_parked_stops s
 WHERE s.preview <> '';

UPDATE items
   SET kind          = 'tool_completion',
       role          = 'assistant',
       status        = 'parked',
       summary       = CASE WHEN trim(s.launch_summary) = '' THEN 'parked'
                            ELSE trim(s.launch_summary) || ' -> parked' END,
       is_background = 1,
       completion_of = s.launch_id,
       tool_name     = s.launch_tool,
       payload_id    = CASE WHEN s.preview <> '' THEN 'tool-call-result:' || s.id END,
       meta          = json_patch(
         json_object(
           'task_id', json_extract(s.bell_meta, '$.task_id'),
           'status_source', 'task_notification',
           'summary_is_rich', json('true'),
           'notification_source', 'task_notification',
           'notification_output_loaded', json(CASE WHEN s.preview <> '' THEN 'true' ELSE 'false' END),
           'notification_output_state', 'loaded',
           'run_started_at', COALESCE(s.woke_at, s.launch_created)),
         json_object(
           'tool_use_id', json_extract(s.bell_meta, '$.tool_use_id'),
           'output_file', json_extract(s.bell_meta, '$.output_file'),
           'notification_output_file', json_extract(s.bell_meta, '$.output_file'),
           'notification_terminal_state', json_extract(s.bell_meta, '$.status'),
           'parked_commands', json_extract(s.bell_meta, '$.parked_commands'),
           'parked_report_item_id', json_extract(s.bell_meta, '$.parked_report_item_id'),
           'run_woke', json(CASE WHEN s.woke_at IS NOT NULL THEN 'true' END)))
  FROM v127_parked_stops s
 WHERE items.thread_id = s.thread_id AND items.id = s.id;

CREATE TEMP TABLE v127_changed_rows AS
SELECT s.thread_id, s.id, i.turn_index, i.item_index
  FROM v127_parked_stops s
  JOIN items i ON i.thread_id = s.thread_id AND i.id = s.id;

INSERT INTO v127_changed_rows (thread_id, id, turn_index, item_index)
SELECT b.thread_id, b.id, b.turn_index, b.item_index
  FROM items b INDEXED BY idx_items_meta_task_id
 WHERE json_extract(b.meta, '$.task_id') IS NOT NULL
   AND b.kind = 'notification'
   AND b.parent_id = ''
   AND b.tool_name IN ('Agent', 'Task', 'SendMessage')
   AND COALESCE(json_extract(b.meta, '$.watch_task'), 0) = 0
   AND EXISTS (
     SELECT 1 FROM items c
      WHERE c.thread_id = b.thread_id
        AND json_extract(c.meta, '$.task_id') = json_extract(b.meta, '$.task_id')
        AND ((c.kind = 'tool_completion' AND c.status <> 'parked')
             OR (c.kind = 'tool_call' AND c.status = 'completed')));

DELETE FROM items
 WHERE (thread_id, id) IN (SELECT thread_id, id FROM v127_changed_rows)
   AND kind = 'notification';

UPDATE threads
   SET history_rev = history_rev + 1,
       history_epoch = history_epoch + 1
 WHERE id IN (
   SELECT l.thread_id FROM thread_fork_lineage l
     JOIN v127_changed_rows d ON d.thread_id = l.ancestor_id
    WHERE (l.cut_turn_index, l.cut_item_index) > (d.turn_index, d.item_index));

DROP TABLE v127_parked_stops;
DROP TABLE v127_changed_rows;
` + forkGuardTriggersV125SQL + reviveBgLaunchOnCompletionMoveV127SQL + forkLineageReleaseTriggerSQL
