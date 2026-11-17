package store

import (
	"database/sql"
	"errors"
	"fmt"
)

// Migration v154 adds the assistants (standing agents), their wakes and the
// request ledger's agent records (docs/specs/standing-agents.md).
//
// `agents` is the assistant: its own id, its current thread, its home and
// the assistant that created it. The unique `thread_id` keeps one current
// thread per assistant, and it cascades from that thread, so deleting the
// current thread deletes the assistant. `agent_past_threads` lists the
// threads an assistant had before, with when each was superseded; it
// cascades from the thread and from the assistant, so deleting a past
// thread leaves the assistant alone and deleting the assistant leaves its
// past threads as ordinary threads. `moved_draft` and
// `moved_draft_attachments` are the text and attachment ids of the composer
// draft the handoff moved from the past thread to the next one, so a send of
// that draft from the past thread can be told where it went; empty when none
// moved. An assistant serves every project when `all_projects` is set,
// otherwise the projects `agent_projects` names (ruling 37), each row going
// with its assistant or its project. `agent_helpers` names the threads an
// assistant spawned without a project, helpers to its own work that live
// under it; a row goes with its thread or its assistant.
//
// `agent_wakes` holds the loops and watches a thread registered, and the
// sweep rebuilds its schedule from them at every boot. `last_output` keeps
// the body of a watch's most recent wake that never reached the agent,
// `undelivered` that wake's outcome until a later wake tells the agent, and
// `last_stderr` the tail of the watch's last stderr for the activity rail.
// `session_start_failures` marks a thread whose session, started for a
// message the app queued, failed to start and raised the start-failure
// notification, so the first start that succeeds withdraws it even after a
// restart. Each cascades from its thread.
//
// threads gains the phases of a family delete (thread_delete.go): a
// thread's `delete_claim` is the id of the delete that claimed it, empty
// for none. A claimed thread stays listed and refuses execution and every
// move; idx_threads_delete_claim holds the claimed ones, for the boot that
// releases the claims no delete committed. `spawn_instructions` is the text
// a thread_spawn appended to the thread's system prompt for its life, and
// `spawn_level` the sub-thread level a paired computer's spawn or ask
// carried here while that delegation lasts, zero for every other thread.
//
// thread_requests and thread_request_receipts gain:
//
//   - `lineage_only`: a spawn row the retention sweep kept past the floor
//     with its payload cleared, as the record of which thread spawned its
//     target, here or on another computer. The ledger reads skip it: its token is unknown
//     and its answer gone. It still links its target into the tree. On a
//     receipt it is the same for a spawn's receipt: kept while the thread
//     it made exists, as the record that the source thread there owns it,
//     which authorizes that computer's agent_rows and agent_stop.
//   - `superseded`: a local spawn of a thread that a newer local spawn
//     also names, which only data from before this migration holds. It is
//     not its target's link and names no owner: every read of a thread's
//     spawner, its owned workers and its placement reads the link alone.
//   - `confirmed_at` and `late_confirmed_at`: the provider's echo of a
//     queued answer and late reply. A row delivered before this migration
//     is recorded as confirmed at its delivery unless the previous version
//     read its queued message undelivered: a row of the caller marked
//     pendingFlush that the residue rule calls unconfirmed
//     (store.ListUnconfirmedUserRows) carries its send id, or its queue row
//     is still there and no row carrying the provider's id answers for it
//     (app.queuedSendDelivered). A request that rode with another's wake
//     reads its carrier's send id. Those are left unconfirmed, and the boot
//     gives them back to their requests. v154_undelivered_wakes, a
//     temporary table dropped before the migration ends, holds those send
//     ids by thread so each request's check is one keyed probe.
//   - `answer_agent` and `answer_agent_role`, `late_reply_agent` and
//     `late_reply_agent_role`: whether the thread that wrote each answer is
//     a standing agent, and its role line then. An earlier row was answered
//     by no agent, as none existed.
//   - `nested_open` and `target_provider`: whether a remote spawn's computer
//     last listed open work under it, and the remote target's provider.
//   - `late_delivered_how`: how a late reply reached the caller apart from
//     the first answer. Before this migration a late delivery overwrote
//     `delivered_how`, so a row with one delivered gets that route copied;
//     its first answer's route stays as it reads.
//   - `made_in_thread_id`: the past thread that made a request a handoff
//     moved to its assistant's current thread.
//   - `wake_item_id` and `late_wake_item_id`: the caller's timeline row that
//     carried a queued answer and late reply. recordConfirmedWakeItems fills
//     them for the deliveries confirmed here from the send ids the wakes
//     carried, and a request that rode with another's wake gets its
//     carrier's.
//   - `open_below_0` to `open_below_3` on a local spawn, and on a receipt
//     `own_turn`, whether the turn its message was delivered into opened
//     for it (a cancel interrupts only such a turn).
//   - `spawn_instructions` on both: a spawn's thread's instructions
//     (threads.spawn_instructions), recorded once when the spawn names its
//     thread, on the receipt and, for a local spawn, the source row; a
//     remote spawn's source row records what its computer's answer
//     carried. A spawn's result reads them from here, whatever became of
//     the thread since.
//
// A thread is reached by at most one local spawn, its link
// (idx_thread_requests_spawn_link): every writer that names a local spawn's
// target names a thread it just made, or the successor New Thread, Same
// Assistant made, so a thread's place in the tree is the one chain of spawns
// above it. The link outlives the retention floor: clearing a spawn's
// payload (lineage_only) leaves it the link, so a worker reused after the
// sweep nests its new work under its owner. A database that holds two such
// rows for one thread keeps the newest as the link, by creation time then
// token, and marks the older ones superseded lineage rows.
//
// open_below_0 to open_below_3 count, on each such spawn, the requests that
// count as open work (agentOpenWorkSQL) made by the threads d spawns below
// its target, d from 0 to 3: the depth within which open work keeps a
// settled spawn listed. Kept by depth, a spawn that is removed or moved
// takes exactly what it carried away from the spawns above it. The triggers
// keep them in the transaction of every write that adds, removes or changes
// a request, a thread's delete and its cascade included, climbing at most
// four spawns. The climb counts a path once only while no spawn repeats on
// it, so a local spawn may not reach its caller or a thread within four
// spawns above it; no writer makes one, as each names a thread with nothing
// above it. What the triggers add to a write under a chain four spawns deep,
// measured: a settle 70 us, an open send's insert 30 us, a spawn's insert,
// receipt and naming 200 us, and a thread delete 51 us per open local spawn
// its cascade removes, 208 ms for 5,000 (TestAgentTreeMarksCascadeAtScale
// holds that under 1 s). fillAgentTreeMarks counts them for the rows here.
//
// The Agents segment's read (threadAgentTreeSQL) searches each thread on a
// spawn walk by caller through an index that leaves its settled history
// out, or a range of one that does not reach it:
// idx_thread_requests_open_created for open rows in tree order,
// idx_thread_requests_caller_settled for rows settled inside the window,
// idx_thread_requests_open_below for the local spawns kept for open work
// under them, idx_thread_requests_nested_caller for remote spawns with open
// work. idx_thread_requests_open_deep holds the local spawns with open work
// three spawns below their target, from which the reads climb to the spawns
// above that no mark reaches. idx_thread_requests_caller_settled holds
// every row, lineage rows
// included, so the segment's Past list pages a caller's settled rows newest
// first through it (ListThreadPastAgentRequests).
// idx_thread_requests_nested_open holds the settled remote spawns
// with open work there, their lineage included, in the order the sweep
// pages through them, and idx_thread_requests_remote_rest the others,
// which it pages through at a slower pace to find a thread there that left
// rest.
// idx_thread_requests_spawn_target serves the lineage reads by spawned
// thread. idx_thread_requests_age and idx_thread_request_receipts_age index
// the age of the rows the retention floor can remove, and
// idx_thread_requests_lineage the local lineage rows and
// idx_thread_request_receipts_lineage the receipts' lineage, which the
// sweep deletes once their spawned thread is gone; a remote spawn's goes
// when its computer says so.
//
// trg_thread_placement_spawn_gone makes an owned thread of a project
// standalone when its link is deleted, as a spawner's delete does, so the
// thread gains a sidebar row rather than being left owned by nothing; a
// superseded row left behind owns nothing. An assistant's helper, outside every
// project, stays owned: its assistant's delete deletes it.
//
// idx_agent_past_threads_agent orders an assistant's past threads newest
// superseded first, ties by thread id, the page order of
// ListAssistantPastThreads.
//
// async_questions gains `revision`. The highest revision of an item's rows
// is the item's, and async_question_revision raises it on every change of
// state of any of them (an answer, a dismissal, a reopen, a restore, a
// delivery, a give-back), in the transaction of that change. Two readings
// of an item with one revision ask the same questions: a worker's question
// is put to its spawner under one, and an answer written for an older one
// is refused where it is claimed (QueueAsyncAnswers).
const assistantsMigrationVersion = 154

const assistantsV154SQL = `
CREATE TABLE agents (
    id               TEXT PRIMARY KEY,
    thread_id        TEXT NOT NULL UNIQUE REFERENCES threads(id) ON DELETE CASCADE,
    home             TEXT NOT NULL,
    creator_agent_id TEXT NOT NULL DEFAULT '',
    created_at       INTEGER NOT NULL,
    all_projects     INTEGER NOT NULL DEFAULT 0 CHECK(all_projects IN (0,1))
);
CREATE TABLE agent_projects (
    agent_id   TEXT NOT NULL REFERENCES agents(id) ON DELETE CASCADE,
    project_id TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    PRIMARY KEY (agent_id, project_id)
) WITHOUT ROWID;
CREATE INDEX idx_agent_projects_project ON agent_projects(project_id);
CREATE TABLE agent_helpers (
    thread_id TEXT PRIMARY KEY REFERENCES threads(id) ON DELETE CASCADE,
    agent_id  TEXT NOT NULL REFERENCES agents(id) ON DELETE CASCADE
);
CREATE INDEX idx_agent_helpers_agent ON agent_helpers(agent_id);
CREATE TABLE agent_past_threads (
    thread_id        TEXT PRIMARY KEY REFERENCES threads(id) ON DELETE CASCADE,
    agent_id         TEXT NOT NULL REFERENCES agents(id) ON DELETE CASCADE,
    superseded_at    INTEGER NOT NULL,
    moved_draft      TEXT NOT NULL DEFAULT '',
    moved_draft_attachments TEXT NOT NULL DEFAULT '[]'
);
CREATE INDEX idx_agent_past_threads_agent ON agent_past_threads(agent_id, superseded_at DESC, thread_id DESC);
CREATE TABLE agent_wakes (
    id               TEXT PRIMARY KEY,
    thread_id        TEXT NOT NULL REFERENCES threads(id) ON DELETE CASCADE,
    kind             TEXT NOT NULL CHECK(kind IN ('loop','watch')),
    label            TEXT NOT NULL,
    note             TEXT NOT NULL DEFAULT '',
    command          TEXT NOT NULL DEFAULT '',
    cwd              TEXT NOT NULL DEFAULT '',
    interval_seconds INTEGER,
    timeout_seconds  INTEGER,
    long_running     INTEGER NOT NULL DEFAULT 0 CHECK(long_running IN (0,1)),
    next_at          INTEGER,
    last_run_at      INTEGER,
    last_outcome     TEXT NOT NULL DEFAULT '',
    last_output      TEXT NOT NULL DEFAULT '',
    undelivered      TEXT NOT NULL DEFAULT '',
    missed           INTEGER NOT NULL DEFAULT 0,
    state            TEXT NOT NULL CHECK(state IN ('armed','running','cancelled')),
    created_at       INTEGER NOT NULL,
    last_stderr      TEXT NOT NULL DEFAULT ''
);
CREATE INDEX idx_agent_wakes_due ON agent_wakes(state, next_at);
CREATE INDEX idx_agent_wakes_thread ON agent_wakes(thread_id, state);
CREATE TABLE session_start_failures (
    thread_id        TEXT PRIMARY KEY REFERENCES threads(id) ON DELETE CASCADE,
    failed_at        INTEGER NOT NULL
);

ALTER TABLE threads ADD COLUMN delete_claim TEXT NOT NULL DEFAULT '';
CREATE INDEX idx_threads_delete_claim ON threads(delete_claim) WHERE delete_claim <> '';
ALTER TABLE threads ADD COLUMN spawn_instructions TEXT NOT NULL DEFAULT '';
ALTER TABLE threads ADD COLUMN spawn_level INTEGER NOT NULL DEFAULT 0 CHECK(spawn_level >= 0);

ALTER TABLE thread_requests ADD COLUMN lineage_only INTEGER NOT NULL DEFAULT 0 CHECK(lineage_only IN (0,1));
ALTER TABLE thread_requests ADD COLUMN superseded INTEGER NOT NULL DEFAULT 0 CHECK(superseded IN (0,1));
ALTER TABLE thread_requests ADD COLUMN confirmed_at INTEGER;
ALTER TABLE thread_requests ADD COLUMN late_confirmed_at INTEGER;
ALTER TABLE thread_requests ADD COLUMN answer_agent INTEGER NOT NULL DEFAULT 0 CHECK(answer_agent IN (0,1));
ALTER TABLE thread_requests ADD COLUMN answer_agent_role TEXT NOT NULL DEFAULT '';
ALTER TABLE thread_requests ADD COLUMN late_reply_agent INTEGER NOT NULL DEFAULT 0 CHECK(late_reply_agent IN (0,1));
ALTER TABLE thread_requests ADD COLUMN late_reply_agent_role TEXT NOT NULL DEFAULT '';
ALTER TABLE thread_requests ADD COLUMN nested_open INTEGER NOT NULL DEFAULT 0 CHECK(nested_open IN (0,1));
ALTER TABLE thread_requests ADD COLUMN target_provider TEXT NOT NULL DEFAULT '';
ALTER TABLE thread_requests ADD COLUMN late_delivered_how TEXT NOT NULL DEFAULT ''
    CHECK(late_delivered_how IN ('','inline','queued','draft'));
ALTER TABLE thread_requests ADD COLUMN made_in_thread_id TEXT NOT NULL DEFAULT '';
ALTER TABLE thread_requests ADD COLUMN wake_item_id TEXT NOT NULL DEFAULT '';
ALTER TABLE thread_requests ADD COLUMN late_wake_item_id TEXT NOT NULL DEFAULT '';
ALTER TABLE thread_requests ADD COLUMN open_below_0 INTEGER NOT NULL DEFAULT 0;
ALTER TABLE thread_requests ADD COLUMN open_below_1 INTEGER NOT NULL DEFAULT 0;
ALTER TABLE thread_requests ADD COLUMN open_below_2 INTEGER NOT NULL DEFAULT 0;
ALTER TABLE thread_requests ADD COLUMN open_below_3 INTEGER NOT NULL DEFAULT 0;
ALTER TABLE thread_requests ADD COLUMN spawn_instructions TEXT NOT NULL DEFAULT '';
ALTER TABLE thread_request_receipts ADD COLUMN answer_agent INTEGER NOT NULL DEFAULT 0 CHECK(answer_agent IN (0,1));
ALTER TABLE thread_request_receipts ADD COLUMN answer_agent_role TEXT NOT NULL DEFAULT '';
ALTER TABLE thread_request_receipts ADD COLUMN late_reply_agent INTEGER NOT NULL DEFAULT 0 CHECK(late_reply_agent IN (0,1));
ALTER TABLE thread_request_receipts ADD COLUMN late_reply_agent_role TEXT NOT NULL DEFAULT '';
ALTER TABLE thread_request_receipts ADD COLUMN own_turn INTEGER NOT NULL DEFAULT 0 CHECK(own_turn IN (0,1));
ALTER TABLE thread_request_receipts ADD COLUMN lineage_only INTEGER NOT NULL DEFAULT 0 CHECK(lineage_only IN (0,1));
ALTER TABLE thread_request_receipts ADD COLUMN spawn_instructions TEXT NOT NULL DEFAULT '';

CREATE TEMP TABLE v154_undelivered_wakes (
    thread_id TEXT NOT NULL,
    send_id   TEXT NOT NULL,
    PRIMARY KEY (thread_id, send_id)
) WITHOUT ROWID;
WITH residue(thread_id, send_id, joined) AS (
  SELECT u.thread_id,
         CASE WHEN json_valid(u.meta) THEN json_extract(u.meta, '$.sendId') END,
         COALESCE(CASE WHEN json_valid(u.meta) THEN
           CASE WHEN json_type(u.meta, '$.joinedSendIds') = 'array' THEN json_extract(u.meta, '$.joinedSendIds') END END, '[]')
    FROM items u
   WHERE u.kind = 'user_text' AND u.parent_id = ''
     AND (CASE WHEN json_valid(u.meta) THEN json_extract(u.meta, '$.pendingFlush') END) = 1
     AND u.created_at >= (SELECT t.created_at FROM threads t WHERE t.id = u.thread_id)
     AND NOT EXISTS (SELECT 1 FROM turns later WHERE later.thread_id = u.thread_id AND later.turn_index > u.turn_index)
     AND NOT EXISTS (SELECT 1 FROM items later
                      WHERE later.thread_id = u.thread_id AND later.turn_index > u.turn_index AND later.parent_id = ''))
INSERT OR IGNORE INTO v154_undelivered_wakes (thread_id, send_id)
SELECT thread_id, send_id FROM residue WHERE send_id GLOB 'thread-wake*'
UNION
SELECT r.thread_id, j.value FROM residue r JOIN json_each(r.joined) j WHERE j.type = 'text' AND j.value GLOB 'thread-wake*';
INSERT OR IGNORE INTO v154_undelivered_wakes (thread_id, send_id)
SELECT q.thread_id, q.send_id
  FROM flush_queue_items q
 WHERE q.send_id GLOB 'thread-wake*'
   AND NOT EXISTS (SELECT 1 FROM items d
        WHERE d.thread_id = q.thread_id AND d.kind = 'user_text' AND d.parent_id = ''
          AND (CASE WHEN json_valid(d.meta) THEN COALESCE(json_extract(d.meta, '$.provider_item_id'), '') END) <> ''
          AND (CASE WHEN json_valid(d.meta) THEN COALESCE(json_extract(d.meta, '$.wire_only'), 0) END) != 1
          AND ((CASE WHEN json_valid(d.meta) THEN json_extract(d.meta, '$.sendId') END) = q.send_id
            OR q.send_id IN (SELECT j.value FROM json_each(CASE WHEN json_valid(d.meta) THEN d.meta ELSE '{}' END, '$.joinedSendIds') j)));

UPDATE thread_requests SET confirmed_at = delivered_at
 WHERE delivered_at IS NOT NULL
   AND NOT (delivered_how = 'queued' AND EXISTS (SELECT 1 FROM v154_undelivered_wakes x
        WHERE x.thread_id = thread_requests.caller_thread_id
          AND x.send_id = 'thread-wake:' || CASE WHEN thread_requests.delivered_with <> '' THEN thread_requests.delivered_with ELSE thread_requests.token END));
UPDATE thread_requests SET late_delivered_how = delivered_how,
       late_confirmed_at = CASE WHEN delivered_how = 'queued' AND EXISTS (SELECT 1 FROM v154_undelivered_wakes x
        WHERE x.thread_id = thread_requests.caller_thread_id AND x.send_id = 'thread-wake-late:' || thread_requests.token)
       THEN NULL ELSE late_delivered_at END
 WHERE late_delivered_at IS NOT NULL;
DROP TABLE v154_undelivered_wakes;

UPDATE thread_requests SET superseded = 1, lineage_only = 1
 WHERE kind = 'spawn' AND target_computer_id = '' AND target_thread_id <> ''
   AND EXISTS (SELECT 1 FROM thread_requests n
                WHERE n.kind = 'spawn' AND n.target_computer_id = ''
                  AND n.target_thread_id = thread_requests.target_thread_id
                  AND (n.created_at > thread_requests.created_at
                    OR (n.created_at = thread_requests.created_at AND n.token > thread_requests.token)));

CREATE INDEX idx_thread_requests_spawn_target
    ON thread_requests(target_thread_id)
 WHERE kind = 'spawn' AND target_computer_id = '';
CREATE INDEX idx_thread_requests_nested_open ON thread_requests(settled_at, token)
  WHERE nested_open = 1 AND settled_at IS NOT NULL AND kind = 'spawn'
    AND target_computer_id <> '';
CREATE INDEX idx_thread_requests_remote_rest ON thread_requests(settled_at, token)
  WHERE nested_open = 0 AND settled_at IS NOT NULL AND kind = 'spawn'
    AND target_computer_id <> '' AND target_thread_id <> '';
CREATE INDEX idx_thread_requests_caller_settled
    ON thread_requests(caller_thread_id, settled_at, token);
CREATE INDEX idx_thread_requests_nested_caller
    ON thread_requests(caller_thread_id)
 WHERE nested_open = 1 AND kind = 'spawn' AND target_computer_id <> '';
CREATE INDEX idx_thread_requests_age
    ON thread_requests(COALESCE(settled_at, due_at, created_at))
 WHERE lineage_only = 0 AND state NOT IN ('unconfirmed','accepted','running');
CREATE INDEX idx_thread_requests_lineage
    ON thread_requests(target_thread_id)
 WHERE lineage_only = 1 AND target_computer_id = '';
CREATE INDEX idx_thread_request_receipts_age
    ON thread_request_receipts(COALESCE(settled_at, created_at))
 WHERE lineage_only = 0 AND state NOT IN ('accepted','running');
CREATE INDEX idx_thread_request_receipts_lineage
    ON thread_request_receipts(target_thread_id)
 WHERE lineage_only = 1;
CREATE UNIQUE INDEX idx_thread_requests_spawn_link
    ON thread_requests(target_thread_id)
 WHERE kind = 'spawn' AND target_computer_id = '' AND superseded = 0 AND target_thread_id <> '';
CREATE INDEX idx_thread_requests_open_below
    ON thread_requests(caller_thread_id)
 WHERE kind = 'spawn' AND target_computer_id = '' AND superseded = 0 AND target_thread_id <> ''
   AND (open_below_0 + open_below_1 + open_below_2 + open_below_3) > 0;
CREATE INDEX idx_thread_requests_open_deep
    ON thread_requests(caller_thread_id)
 WHERE kind = 'spawn' AND target_computer_id = '' AND superseded = 0 AND target_thread_id <> ''
   AND open_below_3 > 0;
CREATE INDEX idx_thread_requests_open_created
    ON thread_requests(caller_thread_id, created_at, token)
 WHERE lineage_only = 0 AND settled_at IS NULL;

CREATE TRIGGER trg_thread_requests_open_below_insert AFTER INSERT ON thread_requests
WHEN ((NEW.lineage_only = 0 AND NEW.settled_at IS NULL AND (NEW.kind = 'spawn' OR (NEW.kind IN ('send','ask') AND NEW.notify = 1)))
        OR (NEW.kind = 'spawn' AND NEW.target_computer_id <> '' AND NEW.nested_open = 1)) OR (NEW.kind = 'spawn' AND NEW.target_computer_id = '' AND NEW.superseded = 0 AND NEW.target_thread_id <> '')
BEGIN
  UPDATE thread_requests SET open_below_0 = open_below_0 + 1
   WHERE ((NEW.lineage_only = 0 AND NEW.settled_at IS NULL AND (NEW.kind = 'spawn' OR (NEW.kind IN ('send','ask') AND NEW.notify = 1)))
        OR (NEW.kind = 'spawn' AND NEW.target_computer_id <> '' AND NEW.nested_open = 1)) AND kind = 'spawn' AND target_computer_id = '' AND superseded = 0 AND target_thread_id <> ''
     AND target_thread_id = NEW.caller_thread_id;
  UPDATE thread_requests SET open_below_1 = open_below_1 + 1
   WHERE ((NEW.lineage_only = 0 AND NEW.settled_at IS NULL AND (NEW.kind = 'spawn' OR (NEW.kind IN ('send','ask') AND NEW.notify = 1)))
        OR (NEW.kind = 'spawn' AND NEW.target_computer_id <> '' AND NEW.nested_open = 1)) AND kind = 'spawn' AND target_computer_id = '' AND superseded = 0 AND target_thread_id <> ''
     AND target_thread_id = (SELECT p.caller_thread_id FROM thread_requests p
          WHERE p.kind = 'spawn' AND p.target_computer_id = '' AND p.superseded = 0 AND p.target_thread_id = NEW.caller_thread_id);
  UPDATE thread_requests SET open_below_2 = open_below_2 + 1
   WHERE ((NEW.lineage_only = 0 AND NEW.settled_at IS NULL AND (NEW.kind = 'spawn' OR (NEW.kind IN ('send','ask') AND NEW.notify = 1)))
        OR (NEW.kind = 'spawn' AND NEW.target_computer_id <> '' AND NEW.nested_open = 1)) AND kind = 'spawn' AND target_computer_id = '' AND superseded = 0 AND target_thread_id <> ''
     AND target_thread_id = (SELECT p.caller_thread_id FROM thread_requests p
          WHERE p.kind = 'spawn' AND p.target_computer_id = '' AND p.superseded = 0 AND p.target_thread_id = (SELECT p.caller_thread_id FROM thread_requests p
          WHERE p.kind = 'spawn' AND p.target_computer_id = '' AND p.superseded = 0 AND p.target_thread_id = NEW.caller_thread_id));
  UPDATE thread_requests SET open_below_3 = open_below_3 + 1
   WHERE ((NEW.lineage_only = 0 AND NEW.settled_at IS NULL AND (NEW.kind = 'spawn' OR (NEW.kind IN ('send','ask') AND NEW.notify = 1)))
        OR (NEW.kind = 'spawn' AND NEW.target_computer_id <> '' AND NEW.nested_open = 1)) AND kind = 'spawn' AND target_computer_id = '' AND superseded = 0 AND target_thread_id <> ''
     AND target_thread_id = (SELECT p.caller_thread_id FROM thread_requests p
          WHERE p.kind = 'spawn' AND p.target_computer_id = '' AND p.superseded = 0 AND p.target_thread_id = (SELECT p.caller_thread_id FROM thread_requests p
          WHERE p.kind = 'spawn' AND p.target_computer_id = '' AND p.superseded = 0 AND p.target_thread_id = (SELECT p.caller_thread_id FROM thread_requests p
          WHERE p.kind = 'spawn' AND p.target_computer_id = '' AND p.superseded = 0 AND p.target_thread_id = NEW.caller_thread_id)));
  UPDATE thread_requests SET open_below_0 = (SELECT COUNT(*) FROM thread_requests c WHERE c.caller_thread_id = NEW.target_thread_id
        AND c.lineage_only = 0 AND c.settled_at IS NULL AND (c.kind = 'spawn' OR (c.kind IN ('send','ask') AND c.notify = 1)))
      + (SELECT COUNT(*) FROM thread_requests c WHERE c.caller_thread_id = NEW.target_thread_id
        AND c.nested_open = 1 AND c.kind = 'spawn' AND c.target_computer_id <> '' AND +c.settled_at IS NOT NULL),
    open_below_1 = (SELECT COALESCE(SUM(c.open_below_0), 0) FROM thread_requests c WHERE c.caller_thread_id = NEW.target_thread_id
        AND c.kind = 'spawn' AND c.target_computer_id = '' AND c.superseded = 0 AND c.target_thread_id <> ''
        AND (c.open_below_0 + c.open_below_1 + c.open_below_2 + c.open_below_3) > 0),
    open_below_2 = (SELECT COALESCE(SUM(c.open_below_1), 0) FROM thread_requests c WHERE c.caller_thread_id = NEW.target_thread_id
        AND c.kind = 'spawn' AND c.target_computer_id = '' AND c.superseded = 0 AND c.target_thread_id <> ''
        AND (c.open_below_0 + c.open_below_1 + c.open_below_2 + c.open_below_3) > 0),
    open_below_3 = (SELECT COALESCE(SUM(c.open_below_2), 0) FROM thread_requests c WHERE c.caller_thread_id = NEW.target_thread_id
        AND c.kind = 'spawn' AND c.target_computer_id = '' AND c.superseded = 0 AND c.target_thread_id <> ''
        AND (c.open_below_0 + c.open_below_1 + c.open_below_2 + c.open_below_3) > 0)
   WHERE (NEW.kind = 'spawn' AND NEW.target_computer_id = '' AND NEW.superseded = 0 AND NEW.target_thread_id <> '') AND token = NEW.token;
  UPDATE thread_requests SET open_below_1 = open_below_1 + (SELECT s.open_below_0 FROM thread_requests s WHERE s.token = NEW.token), open_below_2 = open_below_2 + (SELECT s.open_below_1 FROM thread_requests s WHERE s.token = NEW.token), open_below_3 = open_below_3 + (SELECT s.open_below_2 FROM thread_requests s WHERE s.token = NEW.token)
   WHERE (NEW.kind = 'spawn' AND NEW.target_computer_id = '' AND NEW.superseded = 0 AND NEW.target_thread_id <> '') AND kind = 'spawn' AND target_computer_id = '' AND superseded = 0 AND target_thread_id <> ''
     AND target_thread_id = NEW.caller_thread_id;
  UPDATE thread_requests SET open_below_2 = open_below_2 + (SELECT s.open_below_0 FROM thread_requests s WHERE s.token = NEW.token), open_below_3 = open_below_3 + (SELECT s.open_below_1 FROM thread_requests s WHERE s.token = NEW.token)
   WHERE (NEW.kind = 'spawn' AND NEW.target_computer_id = '' AND NEW.superseded = 0 AND NEW.target_thread_id <> '') AND kind = 'spawn' AND target_computer_id = '' AND superseded = 0 AND target_thread_id <> ''
     AND target_thread_id = (SELECT p.caller_thread_id FROM thread_requests p
          WHERE p.kind = 'spawn' AND p.target_computer_id = '' AND p.superseded = 0 AND p.target_thread_id = NEW.caller_thread_id);
  UPDATE thread_requests SET open_below_3 = open_below_3 + (SELECT s.open_below_0 FROM thread_requests s WHERE s.token = NEW.token)
   WHERE (NEW.kind = 'spawn' AND NEW.target_computer_id = '' AND NEW.superseded = 0 AND NEW.target_thread_id <> '') AND kind = 'spawn' AND target_computer_id = '' AND superseded = 0 AND target_thread_id <> ''
     AND target_thread_id = (SELECT p.caller_thread_id FROM thread_requests p
          WHERE p.kind = 'spawn' AND p.target_computer_id = '' AND p.superseded = 0 AND p.target_thread_id = (SELECT p.caller_thread_id FROM thread_requests p
          WHERE p.kind = 'spawn' AND p.target_computer_id = '' AND p.superseded = 0 AND p.target_thread_id = NEW.caller_thread_id));
END;

CREATE TRIGGER trg_thread_requests_open_below_delete AFTER DELETE ON thread_requests
WHEN ((OLD.lineage_only = 0 AND OLD.settled_at IS NULL AND (OLD.kind = 'spawn' OR (OLD.kind IN ('send','ask') AND OLD.notify = 1)))
        OR (OLD.kind = 'spawn' AND OLD.target_computer_id <> '' AND OLD.nested_open = 1)) OR (OLD.kind = 'spawn' AND OLD.target_computer_id = '' AND OLD.superseded = 0 AND OLD.target_thread_id <> '')
BEGIN
  UPDATE thread_requests SET open_below_0 = open_below_0 + -1
   WHERE ((OLD.lineage_only = 0 AND OLD.settled_at IS NULL AND (OLD.kind = 'spawn' OR (OLD.kind IN ('send','ask') AND OLD.notify = 1)))
        OR (OLD.kind = 'spawn' AND OLD.target_computer_id <> '' AND OLD.nested_open = 1)) AND kind = 'spawn' AND target_computer_id = '' AND superseded = 0 AND target_thread_id <> ''
     AND target_thread_id = OLD.caller_thread_id;
  UPDATE thread_requests SET open_below_1 = open_below_1 + -1
   WHERE ((OLD.lineage_only = 0 AND OLD.settled_at IS NULL AND (OLD.kind = 'spawn' OR (OLD.kind IN ('send','ask') AND OLD.notify = 1)))
        OR (OLD.kind = 'spawn' AND OLD.target_computer_id <> '' AND OLD.nested_open = 1)) AND kind = 'spawn' AND target_computer_id = '' AND superseded = 0 AND target_thread_id <> ''
     AND target_thread_id = (SELECT p.caller_thread_id FROM thread_requests p
          WHERE p.kind = 'spawn' AND p.target_computer_id = '' AND p.superseded = 0 AND p.target_thread_id = OLD.caller_thread_id);
  UPDATE thread_requests SET open_below_2 = open_below_2 + -1
   WHERE ((OLD.lineage_only = 0 AND OLD.settled_at IS NULL AND (OLD.kind = 'spawn' OR (OLD.kind IN ('send','ask') AND OLD.notify = 1)))
        OR (OLD.kind = 'spawn' AND OLD.target_computer_id <> '' AND OLD.nested_open = 1)) AND kind = 'spawn' AND target_computer_id = '' AND superseded = 0 AND target_thread_id <> ''
     AND target_thread_id = (SELECT p.caller_thread_id FROM thread_requests p
          WHERE p.kind = 'spawn' AND p.target_computer_id = '' AND p.superseded = 0 AND p.target_thread_id = (SELECT p.caller_thread_id FROM thread_requests p
          WHERE p.kind = 'spawn' AND p.target_computer_id = '' AND p.superseded = 0 AND p.target_thread_id = OLD.caller_thread_id));
  UPDATE thread_requests SET open_below_3 = open_below_3 + -1
   WHERE ((OLD.lineage_only = 0 AND OLD.settled_at IS NULL AND (OLD.kind = 'spawn' OR (OLD.kind IN ('send','ask') AND OLD.notify = 1)))
        OR (OLD.kind = 'spawn' AND OLD.target_computer_id <> '' AND OLD.nested_open = 1)) AND kind = 'spawn' AND target_computer_id = '' AND superseded = 0 AND target_thread_id <> ''
     AND target_thread_id = (SELECT p.caller_thread_id FROM thread_requests p
          WHERE p.kind = 'spawn' AND p.target_computer_id = '' AND p.superseded = 0 AND p.target_thread_id = (SELECT p.caller_thread_id FROM thread_requests p
          WHERE p.kind = 'spawn' AND p.target_computer_id = '' AND p.superseded = 0 AND p.target_thread_id = (SELECT p.caller_thread_id FROM thread_requests p
          WHERE p.kind = 'spawn' AND p.target_computer_id = '' AND p.superseded = 0 AND p.target_thread_id = OLD.caller_thread_id)));
  UPDATE thread_requests SET open_below_1 = open_below_1 + -OLD.open_below_0, open_below_2 = open_below_2 + -OLD.open_below_1, open_below_3 = open_below_3 + -OLD.open_below_2
   WHERE (OLD.kind = 'spawn' AND OLD.target_computer_id = '' AND OLD.superseded = 0 AND OLD.target_thread_id <> '') AND kind = 'spawn' AND target_computer_id = '' AND superseded = 0 AND target_thread_id <> ''
     AND target_thread_id = OLD.caller_thread_id;
  UPDATE thread_requests SET open_below_2 = open_below_2 + -OLD.open_below_0, open_below_3 = open_below_3 + -OLD.open_below_1
   WHERE (OLD.kind = 'spawn' AND OLD.target_computer_id = '' AND OLD.superseded = 0 AND OLD.target_thread_id <> '') AND kind = 'spawn' AND target_computer_id = '' AND superseded = 0 AND target_thread_id <> ''
     AND target_thread_id = (SELECT p.caller_thread_id FROM thread_requests p
          WHERE p.kind = 'spawn' AND p.target_computer_id = '' AND p.superseded = 0 AND p.target_thread_id = OLD.caller_thread_id);
  UPDATE thread_requests SET open_below_3 = open_below_3 + -OLD.open_below_0
   WHERE (OLD.kind = 'spawn' AND OLD.target_computer_id = '' AND OLD.superseded = 0 AND OLD.target_thread_id <> '') AND kind = 'spawn' AND target_computer_id = '' AND superseded = 0 AND target_thread_id <> ''
     AND target_thread_id = (SELECT p.caller_thread_id FROM thread_requests p
          WHERE p.kind = 'spawn' AND p.target_computer_id = '' AND p.superseded = 0 AND p.target_thread_id = (SELECT p.caller_thread_id FROM thread_requests p
          WHERE p.kind = 'spawn' AND p.target_computer_id = '' AND p.superseded = 0 AND p.target_thread_id = OLD.caller_thread_id));
END;

CREATE TRIGGER trg_thread_requests_open_below_update AFTER UPDATE OF caller_thread_id, kind, notify, settled_at, nested_open, lineage_only, superseded, target_thread_id, target_computer_id ON thread_requests
WHEN ((OLD.lineage_only = 0 AND OLD.settled_at IS NULL AND (OLD.kind = 'spawn' OR (OLD.kind IN ('send','ask') AND OLD.notify = 1)))
        OR (OLD.kind = 'spawn' AND OLD.target_computer_id <> '' AND OLD.nested_open = 1)) IS NOT ((NEW.lineage_only = 0 AND NEW.settled_at IS NULL AND (NEW.kind = 'spawn' OR (NEW.kind IN ('send','ask') AND NEW.notify = 1)))
        OR (NEW.kind = 'spawn' AND NEW.target_computer_id <> '' AND NEW.nested_open = 1)) OR (OLD.kind = 'spawn' AND OLD.target_computer_id = '' AND OLD.superseded = 0 AND OLD.target_thread_id <> '') IS NOT (NEW.kind = 'spawn' AND NEW.target_computer_id = '' AND NEW.superseded = 0 AND NEW.target_thread_id <> '') OR OLD.caller_thread_id IS NOT NEW.caller_thread_id
  OR ((NEW.kind = 'spawn' AND NEW.target_computer_id = '' AND NEW.superseded = 0 AND NEW.target_thread_id <> '') AND OLD.target_thread_id IS NOT NEW.target_thread_id)
BEGIN
  UPDATE thread_requests SET open_below_0 = open_below_0 + -1
   WHERE ((OLD.lineage_only = 0 AND OLD.settled_at IS NULL AND (OLD.kind = 'spawn' OR (OLD.kind IN ('send','ask') AND OLD.notify = 1)))
        OR (OLD.kind = 'spawn' AND OLD.target_computer_id <> '' AND OLD.nested_open = 1)) AND (OLD.caller_thread_id IS NOT NEW.caller_thread_id OR NOT ((NEW.lineage_only = 0 AND NEW.settled_at IS NULL AND (NEW.kind = 'spawn' OR (NEW.kind IN ('send','ask') AND NEW.notify = 1)))
        OR (NEW.kind = 'spawn' AND NEW.target_computer_id <> '' AND NEW.nested_open = 1))) AND kind = 'spawn' AND target_computer_id = '' AND superseded = 0 AND target_thread_id <> ''
     AND target_thread_id = OLD.caller_thread_id;
  UPDATE thread_requests SET open_below_1 = open_below_1 + -1
   WHERE ((OLD.lineage_only = 0 AND OLD.settled_at IS NULL AND (OLD.kind = 'spawn' OR (OLD.kind IN ('send','ask') AND OLD.notify = 1)))
        OR (OLD.kind = 'spawn' AND OLD.target_computer_id <> '' AND OLD.nested_open = 1)) AND (OLD.caller_thread_id IS NOT NEW.caller_thread_id OR NOT ((NEW.lineage_only = 0 AND NEW.settled_at IS NULL AND (NEW.kind = 'spawn' OR (NEW.kind IN ('send','ask') AND NEW.notify = 1)))
        OR (NEW.kind = 'spawn' AND NEW.target_computer_id <> '' AND NEW.nested_open = 1))) AND kind = 'spawn' AND target_computer_id = '' AND superseded = 0 AND target_thread_id <> ''
     AND target_thread_id = (SELECT p.caller_thread_id FROM thread_requests p
          WHERE p.kind = 'spawn' AND p.target_computer_id = '' AND p.superseded = 0 AND p.target_thread_id = OLD.caller_thread_id);
  UPDATE thread_requests SET open_below_2 = open_below_2 + -1
   WHERE ((OLD.lineage_only = 0 AND OLD.settled_at IS NULL AND (OLD.kind = 'spawn' OR (OLD.kind IN ('send','ask') AND OLD.notify = 1)))
        OR (OLD.kind = 'spawn' AND OLD.target_computer_id <> '' AND OLD.nested_open = 1)) AND (OLD.caller_thread_id IS NOT NEW.caller_thread_id OR NOT ((NEW.lineage_only = 0 AND NEW.settled_at IS NULL AND (NEW.kind = 'spawn' OR (NEW.kind IN ('send','ask') AND NEW.notify = 1)))
        OR (NEW.kind = 'spawn' AND NEW.target_computer_id <> '' AND NEW.nested_open = 1))) AND kind = 'spawn' AND target_computer_id = '' AND superseded = 0 AND target_thread_id <> ''
     AND target_thread_id = (SELECT p.caller_thread_id FROM thread_requests p
          WHERE p.kind = 'spawn' AND p.target_computer_id = '' AND p.superseded = 0 AND p.target_thread_id = (SELECT p.caller_thread_id FROM thread_requests p
          WHERE p.kind = 'spawn' AND p.target_computer_id = '' AND p.superseded = 0 AND p.target_thread_id = OLD.caller_thread_id));
  UPDATE thread_requests SET open_below_3 = open_below_3 + -1
   WHERE ((OLD.lineage_only = 0 AND OLD.settled_at IS NULL AND (OLD.kind = 'spawn' OR (OLD.kind IN ('send','ask') AND OLD.notify = 1)))
        OR (OLD.kind = 'spawn' AND OLD.target_computer_id <> '' AND OLD.nested_open = 1)) AND (OLD.caller_thread_id IS NOT NEW.caller_thread_id OR NOT ((NEW.lineage_only = 0 AND NEW.settled_at IS NULL AND (NEW.kind = 'spawn' OR (NEW.kind IN ('send','ask') AND NEW.notify = 1)))
        OR (NEW.kind = 'spawn' AND NEW.target_computer_id <> '' AND NEW.nested_open = 1))) AND kind = 'spawn' AND target_computer_id = '' AND superseded = 0 AND target_thread_id <> ''
     AND target_thread_id = (SELECT p.caller_thread_id FROM thread_requests p
          WHERE p.kind = 'spawn' AND p.target_computer_id = '' AND p.superseded = 0 AND p.target_thread_id = (SELECT p.caller_thread_id FROM thread_requests p
          WHERE p.kind = 'spawn' AND p.target_computer_id = '' AND p.superseded = 0 AND p.target_thread_id = (SELECT p.caller_thread_id FROM thread_requests p
          WHERE p.kind = 'spawn' AND p.target_computer_id = '' AND p.superseded = 0 AND p.target_thread_id = OLD.caller_thread_id)));
  UPDATE thread_requests SET open_below_1 = open_below_1 + -OLD.open_below_0, open_below_2 = open_below_2 + -OLD.open_below_1, open_below_3 = open_below_3 + -OLD.open_below_2
   WHERE (OLD.kind = 'spawn' AND OLD.target_computer_id = '' AND OLD.superseded = 0 AND OLD.target_thread_id <> '') AND (OLD.caller_thread_id IS NOT NEW.caller_thread_id OR NOT ((OLD.kind = 'spawn' AND OLD.target_computer_id = '' AND OLD.superseded = 0 AND OLD.target_thread_id <> '') AND (NEW.kind = 'spawn' AND NEW.target_computer_id = '' AND NEW.superseded = 0 AND NEW.target_thread_id <> '') AND OLD.target_thread_id = NEW.target_thread_id)) AND kind = 'spawn' AND target_computer_id = '' AND superseded = 0 AND target_thread_id <> ''
     AND target_thread_id = OLD.caller_thread_id;
  UPDATE thread_requests SET open_below_2 = open_below_2 + -OLD.open_below_0, open_below_3 = open_below_3 + -OLD.open_below_1
   WHERE (OLD.kind = 'spawn' AND OLD.target_computer_id = '' AND OLD.superseded = 0 AND OLD.target_thread_id <> '') AND (OLD.caller_thread_id IS NOT NEW.caller_thread_id OR NOT ((OLD.kind = 'spawn' AND OLD.target_computer_id = '' AND OLD.superseded = 0 AND OLD.target_thread_id <> '') AND (NEW.kind = 'spawn' AND NEW.target_computer_id = '' AND NEW.superseded = 0 AND NEW.target_thread_id <> '') AND OLD.target_thread_id = NEW.target_thread_id)) AND kind = 'spawn' AND target_computer_id = '' AND superseded = 0 AND target_thread_id <> ''
     AND target_thread_id = (SELECT p.caller_thread_id FROM thread_requests p
          WHERE p.kind = 'spawn' AND p.target_computer_id = '' AND p.superseded = 0 AND p.target_thread_id = OLD.caller_thread_id);
  UPDATE thread_requests SET open_below_3 = open_below_3 + -OLD.open_below_0
   WHERE (OLD.kind = 'spawn' AND OLD.target_computer_id = '' AND OLD.superseded = 0 AND OLD.target_thread_id <> '') AND (OLD.caller_thread_id IS NOT NEW.caller_thread_id OR NOT ((OLD.kind = 'spawn' AND OLD.target_computer_id = '' AND OLD.superseded = 0 AND OLD.target_thread_id <> '') AND (NEW.kind = 'spawn' AND NEW.target_computer_id = '' AND NEW.superseded = 0 AND NEW.target_thread_id <> '') AND OLD.target_thread_id = NEW.target_thread_id)) AND kind = 'spawn' AND target_computer_id = '' AND superseded = 0 AND target_thread_id <> ''
     AND target_thread_id = (SELECT p.caller_thread_id FROM thread_requests p
          WHERE p.kind = 'spawn' AND p.target_computer_id = '' AND p.superseded = 0 AND p.target_thread_id = (SELECT p.caller_thread_id FROM thread_requests p
          WHERE p.kind = 'spawn' AND p.target_computer_id = '' AND p.superseded = 0 AND p.target_thread_id = OLD.caller_thread_id));
  UPDATE thread_requests SET open_below_0 = (SELECT COUNT(*) FROM thread_requests c WHERE c.caller_thread_id = NEW.target_thread_id
        AND c.lineage_only = 0 AND c.settled_at IS NULL AND (c.kind = 'spawn' OR (c.kind IN ('send','ask') AND c.notify = 1)))
      + (SELECT COUNT(*) FROM thread_requests c WHERE c.caller_thread_id = NEW.target_thread_id
        AND c.nested_open = 1 AND c.kind = 'spawn' AND c.target_computer_id <> '' AND +c.settled_at IS NOT NULL),
    open_below_1 = (SELECT COALESCE(SUM(c.open_below_0), 0) FROM thread_requests c WHERE c.caller_thread_id = NEW.target_thread_id
        AND c.kind = 'spawn' AND c.target_computer_id = '' AND c.superseded = 0 AND c.target_thread_id <> ''
        AND (c.open_below_0 + c.open_below_1 + c.open_below_2 + c.open_below_3) > 0),
    open_below_2 = (SELECT COALESCE(SUM(c.open_below_1), 0) FROM thread_requests c WHERE c.caller_thread_id = NEW.target_thread_id
        AND c.kind = 'spawn' AND c.target_computer_id = '' AND c.superseded = 0 AND c.target_thread_id <> ''
        AND (c.open_below_0 + c.open_below_1 + c.open_below_2 + c.open_below_3) > 0),
    open_below_3 = (SELECT COALESCE(SUM(c.open_below_2), 0) FROM thread_requests c WHERE c.caller_thread_id = NEW.target_thread_id
        AND c.kind = 'spawn' AND c.target_computer_id = '' AND c.superseded = 0 AND c.target_thread_id <> ''
        AND (c.open_below_0 + c.open_below_1 + c.open_below_2 + c.open_below_3) > 0)
   WHERE (NEW.kind = 'spawn' AND NEW.target_computer_id = '' AND NEW.superseded = 0 AND NEW.target_thread_id <> '') AND NOT ((OLD.kind = 'spawn' AND OLD.target_computer_id = '' AND OLD.superseded = 0 AND OLD.target_thread_id <> '') AND (NEW.kind = 'spawn' AND NEW.target_computer_id = '' AND NEW.superseded = 0 AND NEW.target_thread_id <> '') AND OLD.target_thread_id = NEW.target_thread_id) AND token = NEW.token;
  UPDATE thread_requests SET open_below_0 = 0, open_below_1 = 0, open_below_2 = 0, open_below_3 = 0
   WHERE (OLD.kind = 'spawn' AND OLD.target_computer_id = '' AND OLD.superseded = 0 AND OLD.target_thread_id <> '') AND NOT (NEW.kind = 'spawn' AND NEW.target_computer_id = '' AND NEW.superseded = 0 AND NEW.target_thread_id <> '') AND token = NEW.token;
  UPDATE thread_requests SET open_below_0 = open_below_0 + 1
   WHERE ((NEW.lineage_only = 0 AND NEW.settled_at IS NULL AND (NEW.kind = 'spawn' OR (NEW.kind IN ('send','ask') AND NEW.notify = 1)))
        OR (NEW.kind = 'spawn' AND NEW.target_computer_id <> '' AND NEW.nested_open = 1)) AND (OLD.caller_thread_id IS NOT NEW.caller_thread_id OR NOT ((OLD.lineage_only = 0 AND OLD.settled_at IS NULL AND (OLD.kind = 'spawn' OR (OLD.kind IN ('send','ask') AND OLD.notify = 1)))
        OR (OLD.kind = 'spawn' AND OLD.target_computer_id <> '' AND OLD.nested_open = 1))) AND kind = 'spawn' AND target_computer_id = '' AND superseded = 0 AND target_thread_id <> ''
     AND target_thread_id = NEW.caller_thread_id;
  UPDATE thread_requests SET open_below_1 = open_below_1 + 1
   WHERE ((NEW.lineage_only = 0 AND NEW.settled_at IS NULL AND (NEW.kind = 'spawn' OR (NEW.kind IN ('send','ask') AND NEW.notify = 1)))
        OR (NEW.kind = 'spawn' AND NEW.target_computer_id <> '' AND NEW.nested_open = 1)) AND (OLD.caller_thread_id IS NOT NEW.caller_thread_id OR NOT ((OLD.lineage_only = 0 AND OLD.settled_at IS NULL AND (OLD.kind = 'spawn' OR (OLD.kind IN ('send','ask') AND OLD.notify = 1)))
        OR (OLD.kind = 'spawn' AND OLD.target_computer_id <> '' AND OLD.nested_open = 1))) AND kind = 'spawn' AND target_computer_id = '' AND superseded = 0 AND target_thread_id <> ''
     AND target_thread_id = (SELECT p.caller_thread_id FROM thread_requests p
          WHERE p.kind = 'spawn' AND p.target_computer_id = '' AND p.superseded = 0 AND p.target_thread_id = NEW.caller_thread_id);
  UPDATE thread_requests SET open_below_2 = open_below_2 + 1
   WHERE ((NEW.lineage_only = 0 AND NEW.settled_at IS NULL AND (NEW.kind = 'spawn' OR (NEW.kind IN ('send','ask') AND NEW.notify = 1)))
        OR (NEW.kind = 'spawn' AND NEW.target_computer_id <> '' AND NEW.nested_open = 1)) AND (OLD.caller_thread_id IS NOT NEW.caller_thread_id OR NOT ((OLD.lineage_only = 0 AND OLD.settled_at IS NULL AND (OLD.kind = 'spawn' OR (OLD.kind IN ('send','ask') AND OLD.notify = 1)))
        OR (OLD.kind = 'spawn' AND OLD.target_computer_id <> '' AND OLD.nested_open = 1))) AND kind = 'spawn' AND target_computer_id = '' AND superseded = 0 AND target_thread_id <> ''
     AND target_thread_id = (SELECT p.caller_thread_id FROM thread_requests p
          WHERE p.kind = 'spawn' AND p.target_computer_id = '' AND p.superseded = 0 AND p.target_thread_id = (SELECT p.caller_thread_id FROM thread_requests p
          WHERE p.kind = 'spawn' AND p.target_computer_id = '' AND p.superseded = 0 AND p.target_thread_id = NEW.caller_thread_id));
  UPDATE thread_requests SET open_below_3 = open_below_3 + 1
   WHERE ((NEW.lineage_only = 0 AND NEW.settled_at IS NULL AND (NEW.kind = 'spawn' OR (NEW.kind IN ('send','ask') AND NEW.notify = 1)))
        OR (NEW.kind = 'spawn' AND NEW.target_computer_id <> '' AND NEW.nested_open = 1)) AND (OLD.caller_thread_id IS NOT NEW.caller_thread_id OR NOT ((OLD.lineage_only = 0 AND OLD.settled_at IS NULL AND (OLD.kind = 'spawn' OR (OLD.kind IN ('send','ask') AND OLD.notify = 1)))
        OR (OLD.kind = 'spawn' AND OLD.target_computer_id <> '' AND OLD.nested_open = 1))) AND kind = 'spawn' AND target_computer_id = '' AND superseded = 0 AND target_thread_id <> ''
     AND target_thread_id = (SELECT p.caller_thread_id FROM thread_requests p
          WHERE p.kind = 'spawn' AND p.target_computer_id = '' AND p.superseded = 0 AND p.target_thread_id = (SELECT p.caller_thread_id FROM thread_requests p
          WHERE p.kind = 'spawn' AND p.target_computer_id = '' AND p.superseded = 0 AND p.target_thread_id = (SELECT p.caller_thread_id FROM thread_requests p
          WHERE p.kind = 'spawn' AND p.target_computer_id = '' AND p.superseded = 0 AND p.target_thread_id = NEW.caller_thread_id)));
  UPDATE thread_requests SET open_below_1 = open_below_1 + (SELECT s.open_below_0 FROM thread_requests s WHERE s.token = NEW.token), open_below_2 = open_below_2 + (SELECT s.open_below_1 FROM thread_requests s WHERE s.token = NEW.token), open_below_3 = open_below_3 + (SELECT s.open_below_2 FROM thread_requests s WHERE s.token = NEW.token)
   WHERE (NEW.kind = 'spawn' AND NEW.target_computer_id = '' AND NEW.superseded = 0 AND NEW.target_thread_id <> '') AND (OLD.caller_thread_id IS NOT NEW.caller_thread_id OR NOT ((OLD.kind = 'spawn' AND OLD.target_computer_id = '' AND OLD.superseded = 0 AND OLD.target_thread_id <> '') AND (NEW.kind = 'spawn' AND NEW.target_computer_id = '' AND NEW.superseded = 0 AND NEW.target_thread_id <> '') AND OLD.target_thread_id = NEW.target_thread_id)) AND kind = 'spawn' AND target_computer_id = '' AND superseded = 0 AND target_thread_id <> ''
     AND target_thread_id = NEW.caller_thread_id;
  UPDATE thread_requests SET open_below_2 = open_below_2 + (SELECT s.open_below_0 FROM thread_requests s WHERE s.token = NEW.token), open_below_3 = open_below_3 + (SELECT s.open_below_1 FROM thread_requests s WHERE s.token = NEW.token)
   WHERE (NEW.kind = 'spawn' AND NEW.target_computer_id = '' AND NEW.superseded = 0 AND NEW.target_thread_id <> '') AND (OLD.caller_thread_id IS NOT NEW.caller_thread_id OR NOT ((OLD.kind = 'spawn' AND OLD.target_computer_id = '' AND OLD.superseded = 0 AND OLD.target_thread_id <> '') AND (NEW.kind = 'spawn' AND NEW.target_computer_id = '' AND NEW.superseded = 0 AND NEW.target_thread_id <> '') AND OLD.target_thread_id = NEW.target_thread_id)) AND kind = 'spawn' AND target_computer_id = '' AND superseded = 0 AND target_thread_id <> ''
     AND target_thread_id = (SELECT p.caller_thread_id FROM thread_requests p
          WHERE p.kind = 'spawn' AND p.target_computer_id = '' AND p.superseded = 0 AND p.target_thread_id = NEW.caller_thread_id);
  UPDATE thread_requests SET open_below_3 = open_below_3 + (SELECT s.open_below_0 FROM thread_requests s WHERE s.token = NEW.token)
   WHERE (NEW.kind = 'spawn' AND NEW.target_computer_id = '' AND NEW.superseded = 0 AND NEW.target_thread_id <> '') AND (OLD.caller_thread_id IS NOT NEW.caller_thread_id OR NOT ((OLD.kind = 'spawn' AND OLD.target_computer_id = '' AND OLD.superseded = 0 AND OLD.target_thread_id <> '') AND (NEW.kind = 'spawn' AND NEW.target_computer_id = '' AND NEW.superseded = 0 AND NEW.target_thread_id <> '') AND OLD.target_thread_id = NEW.target_thread_id)) AND kind = 'spawn' AND target_computer_id = '' AND superseded = 0 AND target_thread_id <> ''
     AND target_thread_id = (SELECT p.caller_thread_id FROM thread_requests p
          WHERE p.kind = 'spawn' AND p.target_computer_id = '' AND p.superseded = 0 AND p.target_thread_id = (SELECT p.caller_thread_id FROM thread_requests p
          WHERE p.kind = 'spawn' AND p.target_computer_id = '' AND p.superseded = 0 AND p.target_thread_id = NEW.caller_thread_id));
END;


CREATE TRIGGER trg_thread_requests_spawn_cycle_insert BEFORE INSERT ON thread_requests
WHEN (NEW.kind = 'spawn' AND NEW.target_computer_id = '' AND NEW.superseded = 0 AND NEW.target_thread_id <> '') AND (NEW.target_thread_id = NEW.caller_thread_id OR NEW.target_thread_id = (SELECT p.caller_thread_id FROM thread_requests p
          WHERE p.kind = 'spawn' AND p.target_computer_id = '' AND p.superseded = 0 AND p.target_thread_id = NEW.caller_thread_id) OR NEW.target_thread_id = (SELECT p.caller_thread_id FROM thread_requests p
          WHERE p.kind = 'spawn' AND p.target_computer_id = '' AND p.superseded = 0 AND p.target_thread_id = (SELECT p.caller_thread_id FROM thread_requests p
          WHERE p.kind = 'spawn' AND p.target_computer_id = '' AND p.superseded = 0 AND p.target_thread_id = NEW.caller_thread_id)) OR NEW.target_thread_id = (SELECT p.caller_thread_id FROM thread_requests p
          WHERE p.kind = 'spawn' AND p.target_computer_id = '' AND p.superseded = 0 AND p.target_thread_id = (SELECT p.caller_thread_id FROM thread_requests p
          WHERE p.kind = 'spawn' AND p.target_computer_id = '' AND p.superseded = 0 AND p.target_thread_id = (SELECT p.caller_thread_id FROM thread_requests p
          WHERE p.kind = 'spawn' AND p.target_computer_id = '' AND p.superseded = 0 AND p.target_thread_id = NEW.caller_thread_id))))
BEGIN
  SELECT RAISE(ABORT, 'thread_requests: a local spawn reaches a thread above its caller');
END;

CREATE TRIGGER trg_thread_requests_spawn_cycle_update BEFORE UPDATE OF caller_thread_id, kind, superseded, target_thread_id, target_computer_id ON thread_requests
WHEN (NEW.kind = 'spawn' AND NEW.target_computer_id = '' AND NEW.superseded = 0 AND NEW.target_thread_id <> '') AND (NEW.target_thread_id = NEW.caller_thread_id OR NEW.target_thread_id = (SELECT p.caller_thread_id FROM thread_requests p
          WHERE p.kind = 'spawn' AND p.target_computer_id = '' AND p.superseded = 0 AND p.target_thread_id = NEW.caller_thread_id) OR NEW.target_thread_id = (SELECT p.caller_thread_id FROM thread_requests p
          WHERE p.kind = 'spawn' AND p.target_computer_id = '' AND p.superseded = 0 AND p.target_thread_id = (SELECT p.caller_thread_id FROM thread_requests p
          WHERE p.kind = 'spawn' AND p.target_computer_id = '' AND p.superseded = 0 AND p.target_thread_id = NEW.caller_thread_id)) OR NEW.target_thread_id = (SELECT p.caller_thread_id FROM thread_requests p
          WHERE p.kind = 'spawn' AND p.target_computer_id = '' AND p.superseded = 0 AND p.target_thread_id = (SELECT p.caller_thread_id FROM thread_requests p
          WHERE p.kind = 'spawn' AND p.target_computer_id = '' AND p.superseded = 0 AND p.target_thread_id = (SELECT p.caller_thread_id FROM thread_requests p
          WHERE p.kind = 'spawn' AND p.target_computer_id = '' AND p.superseded = 0 AND p.target_thread_id = NEW.caller_thread_id))))
BEGIN
  SELECT RAISE(ABORT, 'thread_requests: a local spawn reaches a thread above its caller');
END;

CREATE TRIGGER trg_thread_placement_spawn_gone AFTER DELETE ON thread_requests
WHEN OLD.kind = 'spawn' AND OLD.target_computer_id = '' AND OLD.superseded = 0 AND OLD.target_thread_id <> ''
BEGIN
  UPDATE threads SET placement = 'standalone'
   WHERE id = OLD.target_thread_id AND placement = 'owned' AND project_id IS NOT NULL
     AND NOT EXISTS (SELECT 1 FROM thread_requests
                      WHERE kind = 'spawn' AND target_computer_id = '' AND superseded = 0 AND target_thread_id = OLD.target_thread_id);
END;

ALTER TABLE async_questions ADD COLUMN revision INTEGER NOT NULL DEFAULT 1;
CREATE TRIGGER async_question_revision AFTER UPDATE OF state ON async_questions
WHEN OLD.state IS NOT NEW.state
BEGIN
  UPDATE async_questions SET revision = (SELECT MAX(revision) FROM async_questions
                                          WHERE thread_id = NEW.thread_id AND item_id = NEW.item_id) + 1
   WHERE thread_id = NEW.thread_id AND item_id = NEW.item_id AND question_index = NEW.question_index;
END;
`

// fillAssistantLedger records the wake rows of the confirmed deliveries,
// then counts the agent tree marks the triggers keep from here on.
func fillAssistantLedger(tx *sql.Tx) error {
	if err := recordConfirmedWakeItems(tx); err != nil {
		return err
	}
	return fillAgentTreeMarks(tx)
}

// recordConfirmedWakeItems fills wake_item_id and late_wake_item_id for the
// queued answers and late replies already confirmed: the caller's own user
// row carrying the wake's send id, alone or joined with other messages.
// Only the caller's own rows are looked in, which are the only rows the
// reads accept (wakeItemSQL): a request never moves to a fork, so no
// inherited row is its caller's, and imported history carries no send ids.
// One the lookup finds nothing for keeps none. Each request probes its
// caller's row by the send id (idx_items_send_id), then every joined row is
// read once and names the requests it carries (idx_items_joined_send_ids),
// which a probe per request would read once per request of its thread.
func recordConfirmedWakeItems(tx *sql.Tx) error {
	for _, wake := range []struct{ column, route, confirmed, sendID string }{
		{"wake_item_id", "delivered_how", "confirmed_at", "thread-wake:"},
		{"late_wake_item_id", "late_delivered_how", "late_confirmed_at", "thread-wake-late:"},
	} {
		prefix := sqlTextLiteral(wake.sendID)
		confirmed := `r.lineage_only = 0 AND r.` + wake.route + ` = 'queued' AND r.` + wake.confirmed + ` IS NOT NULL`
		if _, err := tx.Exec(`UPDATE thread_requests AS r SET ` + wake.column + ` = COALESCE((
			SELECT i.id FROM items i
			 WHERE i.thread_id = r.caller_thread_id AND i.kind = 'user_text' AND i.parent_id = '' AND json_valid(i.meta)
			   AND json_extract(i.meta, '$.sendId') IS NOT NULL AND json_extract(i.meta, '$.sendId') = ` + prefix + ` || r.token
			   AND COALESCE(json_extract(i.meta, '$.wire_only'), 0) != 1
			 LIMIT 1), '')
			WHERE ` + confirmed); err != nil {
			return fmt.Errorf("record the rows of the confirmed wakes: %w", err)
		}
		if err := recordJoinedWakeItems(tx, wake.column, prefix, confirmed); err != nil {
			return err
		}
	}
	// A request that rode with another's first wake (delivered_with, v149)
	// has no send id of its own: its row is its carrier's. The read of the
	// riders is idx_thread_requests_delivered_with's.
	if _, err := tx.Exec(`UPDATE thread_requests AS r SET wake_item_id = COALESCE((
		SELECT c.wake_item_id FROM thread_requests c
		 WHERE c.token = r.delivered_with AND c.caller_thread_id = r.caller_thread_id), '')
		WHERE r.delivered_with <> '' AND r.lineage_only = 0 AND r.delivered_how = 'queued' AND r.confirmed_at IS NOT NULL`); err != nil {
		return fmt.Errorf("record the rows of the confirmed wakes' riders: %w", err)
	}
	return nil
}

// recordJoinedWakeItems names, for each joined row, the confirmed requests
// still without a row whose wakes it carries: a read of the joined rows,
// then one keyed write per wake found.
func recordJoinedWakeItems(tx *sql.Tx, column, prefix, confirmed string) (err error) {
	type carried struct{ id, threadID, token string }
	rows, err := tx.Query(`SELECT i.id, i.thread_id, substr(j.value, length(` + prefix + `) + 1)
		  FROM items i CROSS JOIN json_each(i.meta, '$.joinedSendIds') j
		 WHERE i.kind = 'user_text' AND i.parent_id = '' AND json_valid(i.meta)
		   AND json_extract(i.meta, '$.joinedSendIds') IS NOT NULL
		   AND COALESCE(json_extract(i.meta, '$.wire_only'), 0) != 1
		   AND substr(j.value, 1, length(` + prefix + `)) = ` + prefix)
	if err != nil {
		return fmt.Errorf("list the joined rows of the confirmed wakes: %w", err)
	}
	var found []carried
	for rows.Next() {
		var c carried
		if err := rows.Scan(&c.id, &c.threadID, &c.token); err != nil {
			return errors.Join(fmt.Errorf("scan a joined row: %w", err), rows.Close())
		}
		found = append(found, c)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return fmt.Errorf("list the joined rows of the confirmed wakes: %w", err)
	}
	update, err := tx.Prepare(`UPDATE thread_requests AS r SET ` + column + ` = ?1
		WHERE r.token = ?2 AND r.caller_thread_id = ?3 AND r.` + column + ` = '' AND ` + confirmed)
	if err != nil {
		return fmt.Errorf("prepare the joined wake write: %w", err)
	}
	defer func() { err = errors.Join(err, update.Close()) }()
	for _, c := range found {
		if _, err := update.Exec(c.id, c.token, c.threadID); err != nil {
			return fmt.Errorf("record the joined row of %s: %w", c.token, err)
		}
	}
	return nil
}

// fillAgentTreeMarks counts, for every local spawn, the open work below
// its target, climbing from each thread with open work: the rows the
// triggers keep from here on.
func fillAgentTreeMarks(tx *sql.Tx) (err error) {
	rows, err := tx.Query(`SELECT caller_thread_id, COUNT(*) FROM thread_requests WHERE ` + agentOpenWorkSQL("thread_requests") + `
		GROUP BY caller_thread_id`)
	if err != nil {
		return fmt.Errorf("list the threads with open work: %w", err)
	}
	busy := map[string]int64{}
	for rows.Next() {
		var thread string
		var n int64
		if err := rows.Scan(&thread, &n); err != nil {
			return errors.Join(fmt.Errorf("scan a thread with open work: %w", err), rows.Close())
		}
		busy[thread] = n
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return fmt.Errorf("list the threads with open work: %w", err)
	}
	parent, err := tx.Prepare(`SELECT token, caller_thread_id FROM thread_requests
		WHERE kind = 'spawn' AND target_computer_id = '' AND superseded = 0 AND target_thread_id = ?`)
	if err != nil {
		return fmt.Errorf("prepare the spawn climb: %w", err)
	}
	defer func() { err = errors.Join(err, parent.Close()) }()
	marks := map[string]*[agentOpenDepth]int64{}
	for thread, n := range busy {
		at := thread
		for d := range agentOpenDepth {
			var token, caller string
			if err := parent.QueryRow(at).Scan(&token, &caller); errors.Is(err, sql.ErrNoRows) {
				break
			} else if err != nil {
				return fmt.Errorf("climb from %s: %w", thread, err)
			}
			if marks[token] == nil {
				marks[token] = &[agentOpenDepth]int64{}
			}
			marks[token][d] += n
			at = caller
		}
	}
	write, err := tx.Prepare(`UPDATE thread_requests SET open_below_0 = ?, open_below_1 = ?, open_below_2 = ?, open_below_3 = ?
		WHERE token = ?`)
	if err != nil {
		return fmt.Errorf("prepare the mark write: %w", err)
	}
	defer func() { err = errors.Join(err, write.Close()) }()
	for token, mark := range marks {
		if _, err := write.Exec(mark[0], mark[1], mark[2], mark[3], token); err != nil {
			return fmt.Errorf("write the mark of %s: %w", token, err)
		}
	}
	return nil
}
