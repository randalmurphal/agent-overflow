package store

import (
	"database/sql"
	"errors"
	"fmt"
)

// Migration v157 lets the Agents segment's tree cost what it lists and how
// deep it nests, not the threads' history or the open work elsewhere on
// the computer (thread_agent_tree.go).
//
// A thread is reached by at most one local spawn
// (idx_thread_requests_spawn_link): every writer that names a local spawn's
// target names a thread it just made, or the successor New Thread, Same
// Assistant made, so a thread's place in the tree is the one chain of
// spawns above it. A database that holds two such rows for one thread
// keeps the newest, which the sidebar's lineage already reads, and keeps
// the older ones as lineage rows only, which no ledger read lists.
//
// open_below_0 to open_below_3 count, on each such spawn, the requests
// that count as open work (agentOpenWorkSQL) made by the threads d spawns
// below its target, d from 0 to 3: the depth within which open work keeps
// a settled spawn listed. Kept by depth, a spawn that is removed or moved
// takes exactly what it carried away from the spawns above it. The
// triggers keep them in the transaction of every write that adds, removes
// or changes a request, a thread's delete and its cascade included,
// climbing at most four spawns. The climb counts a path once only while no
// spawn repeats on it, so a local spawn may not reach its caller or a
// thread within four spawns above it; no writer makes one, as each names a
// thread with nothing above it. idx_thread_requests_open_below holds the
// spawns with open work under them, idx_thread_requests_open_created a
// caller's open requests in tree order. They replace
// idx_thread_requests_spawn_caller, which nothing reads.
const agentTreeMarksMigrationVersion = 157

const agentTreeMarksV157SQL = `
ALTER TABLE thread_requests ADD COLUMN open_below_0 INTEGER NOT NULL DEFAULT 0;
ALTER TABLE thread_requests ADD COLUMN open_below_1 INTEGER NOT NULL DEFAULT 0;
ALTER TABLE thread_requests ADD COLUMN open_below_2 INTEGER NOT NULL DEFAULT 0;
ALTER TABLE thread_requests ADD COLUMN open_below_3 INTEGER NOT NULL DEFAULT 0;

UPDATE thread_requests SET lineage_only = 1
 WHERE kind = 'spawn' AND target_computer_id = '' AND lineage_only = 0 AND target_thread_id <> ''
   AND EXISTS (SELECT 1 FROM thread_requests n
                WHERE n.kind = 'spawn' AND n.target_computer_id = '' AND n.lineage_only = 0
                  AND n.target_thread_id = thread_requests.target_thread_id
                  AND (n.created_at > thread_requests.created_at
                    OR (n.created_at = thread_requests.created_at AND n.token > thread_requests.token)));

CREATE UNIQUE INDEX idx_thread_requests_spawn_link
    ON thread_requests(target_thread_id)
 WHERE kind = 'spawn' AND target_computer_id = '' AND lineage_only = 0 AND target_thread_id <> '';
CREATE INDEX idx_thread_requests_open_below
    ON thread_requests(caller_thread_id)
 WHERE kind = 'spawn' AND target_computer_id = '' AND lineage_only = 0 AND target_thread_id <> ''
   AND (open_below_0 + open_below_1 + open_below_2 + open_below_3) > 0;
CREATE INDEX idx_thread_requests_open_created
    ON thread_requests(caller_thread_id, created_at, token)
 WHERE lineage_only = 0 AND settled_at IS NULL;
DROP INDEX idx_thread_requests_spawn_caller;

CREATE TRIGGER trg_thread_requests_open_below_insert AFTER INSERT ON thread_requests
WHEN (NEW.lineage_only = 0 AND ((NEW.settled_at IS NULL AND (NEW.kind = 'spawn' OR (NEW.kind IN ('send','ask') AND NEW.notify = 1)))
        OR (NEW.kind = 'spawn' AND NEW.target_computer_id <> '' AND NEW.nested_open = 1))) OR (NEW.kind = 'spawn' AND NEW.target_computer_id = '' AND NEW.lineage_only = 0 AND NEW.target_thread_id <> '')
BEGIN
  UPDATE thread_requests SET open_below_0 = open_below_0 + 1
   WHERE (NEW.lineage_only = 0 AND ((NEW.settled_at IS NULL AND (NEW.kind = 'spawn' OR (NEW.kind IN ('send','ask') AND NEW.notify = 1)))
        OR (NEW.kind = 'spawn' AND NEW.target_computer_id <> '' AND NEW.nested_open = 1))) AND kind = 'spawn' AND target_computer_id = '' AND lineage_only = 0 AND target_thread_id <> ''
     AND target_thread_id = NEW.caller_thread_id;
  UPDATE thread_requests SET open_below_1 = open_below_1 + 1
   WHERE (NEW.lineage_only = 0 AND ((NEW.settled_at IS NULL AND (NEW.kind = 'spawn' OR (NEW.kind IN ('send','ask') AND NEW.notify = 1)))
        OR (NEW.kind = 'spawn' AND NEW.target_computer_id <> '' AND NEW.nested_open = 1))) AND kind = 'spawn' AND target_computer_id = '' AND lineage_only = 0 AND target_thread_id <> ''
     AND target_thread_id = (SELECT p.caller_thread_id FROM thread_requests p
          WHERE p.kind = 'spawn' AND p.target_computer_id = '' AND p.lineage_only = 0 AND p.target_thread_id = NEW.caller_thread_id);
  UPDATE thread_requests SET open_below_2 = open_below_2 + 1
   WHERE (NEW.lineage_only = 0 AND ((NEW.settled_at IS NULL AND (NEW.kind = 'spawn' OR (NEW.kind IN ('send','ask') AND NEW.notify = 1)))
        OR (NEW.kind = 'spawn' AND NEW.target_computer_id <> '' AND NEW.nested_open = 1))) AND kind = 'spawn' AND target_computer_id = '' AND lineage_only = 0 AND target_thread_id <> ''
     AND target_thread_id = (SELECT p.caller_thread_id FROM thread_requests p
          WHERE p.kind = 'spawn' AND p.target_computer_id = '' AND p.lineage_only = 0 AND p.target_thread_id = (SELECT p.caller_thread_id FROM thread_requests p
          WHERE p.kind = 'spawn' AND p.target_computer_id = '' AND p.lineage_only = 0 AND p.target_thread_id = NEW.caller_thread_id));
  UPDATE thread_requests SET open_below_3 = open_below_3 + 1
   WHERE (NEW.lineage_only = 0 AND ((NEW.settled_at IS NULL AND (NEW.kind = 'spawn' OR (NEW.kind IN ('send','ask') AND NEW.notify = 1)))
        OR (NEW.kind = 'spawn' AND NEW.target_computer_id <> '' AND NEW.nested_open = 1))) AND kind = 'spawn' AND target_computer_id = '' AND lineage_only = 0 AND target_thread_id <> ''
     AND target_thread_id = (SELECT p.caller_thread_id FROM thread_requests p
          WHERE p.kind = 'spawn' AND p.target_computer_id = '' AND p.lineage_only = 0 AND p.target_thread_id = (SELECT p.caller_thread_id FROM thread_requests p
          WHERE p.kind = 'spawn' AND p.target_computer_id = '' AND p.lineage_only = 0 AND p.target_thread_id = (SELECT p.caller_thread_id FROM thread_requests p
          WHERE p.kind = 'spawn' AND p.target_computer_id = '' AND p.lineage_only = 0 AND p.target_thread_id = NEW.caller_thread_id)));
  UPDATE thread_requests SET open_below_0 = (SELECT COUNT(*) FROM thread_requests c WHERE c.caller_thread_id = NEW.target_thread_id
        AND c.lineage_only = 0 AND c.settled_at IS NULL AND (c.kind = 'spawn' OR (c.kind IN ('send','ask') AND c.notify = 1)))
      + (SELECT COUNT(*) FROM thread_requests c WHERE c.caller_thread_id = NEW.target_thread_id
        AND c.nested_open = 1 AND c.kind = 'spawn' AND c.target_computer_id <> '' AND c.lineage_only = 0 AND +c.settled_at IS NOT NULL),
    open_below_1 = (SELECT COALESCE(SUM(c.open_below_0), 0) FROM thread_requests c WHERE c.caller_thread_id = NEW.target_thread_id
        AND c.kind = 'spawn' AND c.target_computer_id = '' AND c.lineage_only = 0 AND c.target_thread_id <> ''
        AND (c.open_below_0 + c.open_below_1 + c.open_below_2 + c.open_below_3) > 0),
    open_below_2 = (SELECT COALESCE(SUM(c.open_below_1), 0) FROM thread_requests c WHERE c.caller_thread_id = NEW.target_thread_id
        AND c.kind = 'spawn' AND c.target_computer_id = '' AND c.lineage_only = 0 AND c.target_thread_id <> ''
        AND (c.open_below_0 + c.open_below_1 + c.open_below_2 + c.open_below_3) > 0),
    open_below_3 = (SELECT COALESCE(SUM(c.open_below_2), 0) FROM thread_requests c WHERE c.caller_thread_id = NEW.target_thread_id
        AND c.kind = 'spawn' AND c.target_computer_id = '' AND c.lineage_only = 0 AND c.target_thread_id <> ''
        AND (c.open_below_0 + c.open_below_1 + c.open_below_2 + c.open_below_3) > 0)
   WHERE (NEW.kind = 'spawn' AND NEW.target_computer_id = '' AND NEW.lineage_only = 0 AND NEW.target_thread_id <> '') AND token = NEW.token;
  UPDATE thread_requests SET open_below_1 = open_below_1 + (SELECT s.open_below_0 FROM thread_requests s WHERE s.token = NEW.token), open_below_2 = open_below_2 + (SELECT s.open_below_1 FROM thread_requests s WHERE s.token = NEW.token), open_below_3 = open_below_3 + (SELECT s.open_below_2 FROM thread_requests s WHERE s.token = NEW.token)
   WHERE (NEW.kind = 'spawn' AND NEW.target_computer_id = '' AND NEW.lineage_only = 0 AND NEW.target_thread_id <> '') AND kind = 'spawn' AND target_computer_id = '' AND lineage_only = 0 AND target_thread_id <> ''
     AND target_thread_id = NEW.caller_thread_id;
  UPDATE thread_requests SET open_below_2 = open_below_2 + (SELECT s.open_below_0 FROM thread_requests s WHERE s.token = NEW.token), open_below_3 = open_below_3 + (SELECT s.open_below_1 FROM thread_requests s WHERE s.token = NEW.token)
   WHERE (NEW.kind = 'spawn' AND NEW.target_computer_id = '' AND NEW.lineage_only = 0 AND NEW.target_thread_id <> '') AND kind = 'spawn' AND target_computer_id = '' AND lineage_only = 0 AND target_thread_id <> ''
     AND target_thread_id = (SELECT p.caller_thread_id FROM thread_requests p
          WHERE p.kind = 'spawn' AND p.target_computer_id = '' AND p.lineage_only = 0 AND p.target_thread_id = NEW.caller_thread_id);
  UPDATE thread_requests SET open_below_3 = open_below_3 + (SELECT s.open_below_0 FROM thread_requests s WHERE s.token = NEW.token)
   WHERE (NEW.kind = 'spawn' AND NEW.target_computer_id = '' AND NEW.lineage_only = 0 AND NEW.target_thread_id <> '') AND kind = 'spawn' AND target_computer_id = '' AND lineage_only = 0 AND target_thread_id <> ''
     AND target_thread_id = (SELECT p.caller_thread_id FROM thread_requests p
          WHERE p.kind = 'spawn' AND p.target_computer_id = '' AND p.lineage_only = 0 AND p.target_thread_id = (SELECT p.caller_thread_id FROM thread_requests p
          WHERE p.kind = 'spawn' AND p.target_computer_id = '' AND p.lineage_only = 0 AND p.target_thread_id = NEW.caller_thread_id));
END;

CREATE TRIGGER trg_thread_requests_open_below_delete AFTER DELETE ON thread_requests
WHEN (OLD.lineage_only = 0 AND ((OLD.settled_at IS NULL AND (OLD.kind = 'spawn' OR (OLD.kind IN ('send','ask') AND OLD.notify = 1)))
        OR (OLD.kind = 'spawn' AND OLD.target_computer_id <> '' AND OLD.nested_open = 1))) OR (OLD.kind = 'spawn' AND OLD.target_computer_id = '' AND OLD.lineage_only = 0 AND OLD.target_thread_id <> '')
BEGIN
  UPDATE thread_requests SET open_below_0 = open_below_0 + -1
   WHERE (OLD.lineage_only = 0 AND ((OLD.settled_at IS NULL AND (OLD.kind = 'spawn' OR (OLD.kind IN ('send','ask') AND OLD.notify = 1)))
        OR (OLD.kind = 'spawn' AND OLD.target_computer_id <> '' AND OLD.nested_open = 1))) AND kind = 'spawn' AND target_computer_id = '' AND lineage_only = 0 AND target_thread_id <> ''
     AND target_thread_id = OLD.caller_thread_id;
  UPDATE thread_requests SET open_below_1 = open_below_1 + -1
   WHERE (OLD.lineage_only = 0 AND ((OLD.settled_at IS NULL AND (OLD.kind = 'spawn' OR (OLD.kind IN ('send','ask') AND OLD.notify = 1)))
        OR (OLD.kind = 'spawn' AND OLD.target_computer_id <> '' AND OLD.nested_open = 1))) AND kind = 'spawn' AND target_computer_id = '' AND lineage_only = 0 AND target_thread_id <> ''
     AND target_thread_id = (SELECT p.caller_thread_id FROM thread_requests p
          WHERE p.kind = 'spawn' AND p.target_computer_id = '' AND p.lineage_only = 0 AND p.target_thread_id = OLD.caller_thread_id);
  UPDATE thread_requests SET open_below_2 = open_below_2 + -1
   WHERE (OLD.lineage_only = 0 AND ((OLD.settled_at IS NULL AND (OLD.kind = 'spawn' OR (OLD.kind IN ('send','ask') AND OLD.notify = 1)))
        OR (OLD.kind = 'spawn' AND OLD.target_computer_id <> '' AND OLD.nested_open = 1))) AND kind = 'spawn' AND target_computer_id = '' AND lineage_only = 0 AND target_thread_id <> ''
     AND target_thread_id = (SELECT p.caller_thread_id FROM thread_requests p
          WHERE p.kind = 'spawn' AND p.target_computer_id = '' AND p.lineage_only = 0 AND p.target_thread_id = (SELECT p.caller_thread_id FROM thread_requests p
          WHERE p.kind = 'spawn' AND p.target_computer_id = '' AND p.lineage_only = 0 AND p.target_thread_id = OLD.caller_thread_id));
  UPDATE thread_requests SET open_below_3 = open_below_3 + -1
   WHERE (OLD.lineage_only = 0 AND ((OLD.settled_at IS NULL AND (OLD.kind = 'spawn' OR (OLD.kind IN ('send','ask') AND OLD.notify = 1)))
        OR (OLD.kind = 'spawn' AND OLD.target_computer_id <> '' AND OLD.nested_open = 1))) AND kind = 'spawn' AND target_computer_id = '' AND lineage_only = 0 AND target_thread_id <> ''
     AND target_thread_id = (SELECT p.caller_thread_id FROM thread_requests p
          WHERE p.kind = 'spawn' AND p.target_computer_id = '' AND p.lineage_only = 0 AND p.target_thread_id = (SELECT p.caller_thread_id FROM thread_requests p
          WHERE p.kind = 'spawn' AND p.target_computer_id = '' AND p.lineage_only = 0 AND p.target_thread_id = (SELECT p.caller_thread_id FROM thread_requests p
          WHERE p.kind = 'spawn' AND p.target_computer_id = '' AND p.lineage_only = 0 AND p.target_thread_id = OLD.caller_thread_id)));
  UPDATE thread_requests SET open_below_1 = open_below_1 + -OLD.open_below_0, open_below_2 = open_below_2 + -OLD.open_below_1, open_below_3 = open_below_3 + -OLD.open_below_2
   WHERE (OLD.kind = 'spawn' AND OLD.target_computer_id = '' AND OLD.lineage_only = 0 AND OLD.target_thread_id <> '') AND kind = 'spawn' AND target_computer_id = '' AND lineage_only = 0 AND target_thread_id <> ''
     AND target_thread_id = OLD.caller_thread_id;
  UPDATE thread_requests SET open_below_2 = open_below_2 + -OLD.open_below_0, open_below_3 = open_below_3 + -OLD.open_below_1
   WHERE (OLD.kind = 'spawn' AND OLD.target_computer_id = '' AND OLD.lineage_only = 0 AND OLD.target_thread_id <> '') AND kind = 'spawn' AND target_computer_id = '' AND lineage_only = 0 AND target_thread_id <> ''
     AND target_thread_id = (SELECT p.caller_thread_id FROM thread_requests p
          WHERE p.kind = 'spawn' AND p.target_computer_id = '' AND p.lineage_only = 0 AND p.target_thread_id = OLD.caller_thread_id);
  UPDATE thread_requests SET open_below_3 = open_below_3 + -OLD.open_below_0
   WHERE (OLD.kind = 'spawn' AND OLD.target_computer_id = '' AND OLD.lineage_only = 0 AND OLD.target_thread_id <> '') AND kind = 'spawn' AND target_computer_id = '' AND lineage_only = 0 AND target_thread_id <> ''
     AND target_thread_id = (SELECT p.caller_thread_id FROM thread_requests p
          WHERE p.kind = 'spawn' AND p.target_computer_id = '' AND p.lineage_only = 0 AND p.target_thread_id = (SELECT p.caller_thread_id FROM thread_requests p
          WHERE p.kind = 'spawn' AND p.target_computer_id = '' AND p.lineage_only = 0 AND p.target_thread_id = OLD.caller_thread_id));
END;

CREATE TRIGGER trg_thread_requests_open_below_update AFTER UPDATE OF caller_thread_id, kind, notify, settled_at, nested_open, lineage_only, target_thread_id, target_computer_id ON thread_requests
WHEN (OLD.lineage_only = 0 AND ((OLD.settled_at IS NULL AND (OLD.kind = 'spawn' OR (OLD.kind IN ('send','ask') AND OLD.notify = 1)))
        OR (OLD.kind = 'spawn' AND OLD.target_computer_id <> '' AND OLD.nested_open = 1))) IS NOT (NEW.lineage_only = 0 AND ((NEW.settled_at IS NULL AND (NEW.kind = 'spawn' OR (NEW.kind IN ('send','ask') AND NEW.notify = 1)))
        OR (NEW.kind = 'spawn' AND NEW.target_computer_id <> '' AND NEW.nested_open = 1))) OR (OLD.kind = 'spawn' AND OLD.target_computer_id = '' AND OLD.lineage_only = 0 AND OLD.target_thread_id <> '') IS NOT (NEW.kind = 'spawn' AND NEW.target_computer_id = '' AND NEW.lineage_only = 0 AND NEW.target_thread_id <> '') OR OLD.caller_thread_id IS NOT NEW.caller_thread_id
  OR ((NEW.kind = 'spawn' AND NEW.target_computer_id = '' AND NEW.lineage_only = 0 AND NEW.target_thread_id <> '') AND OLD.target_thread_id IS NOT NEW.target_thread_id)
BEGIN
  UPDATE thread_requests SET open_below_0 = open_below_0 + -1
   WHERE (OLD.lineage_only = 0 AND ((OLD.settled_at IS NULL AND (OLD.kind = 'spawn' OR (OLD.kind IN ('send','ask') AND OLD.notify = 1)))
        OR (OLD.kind = 'spawn' AND OLD.target_computer_id <> '' AND OLD.nested_open = 1))) AND (OLD.caller_thread_id IS NOT NEW.caller_thread_id OR NOT (NEW.lineage_only = 0 AND ((NEW.settled_at IS NULL AND (NEW.kind = 'spawn' OR (NEW.kind IN ('send','ask') AND NEW.notify = 1)))
        OR (NEW.kind = 'spawn' AND NEW.target_computer_id <> '' AND NEW.nested_open = 1)))) AND kind = 'spawn' AND target_computer_id = '' AND lineage_only = 0 AND target_thread_id <> ''
     AND target_thread_id = OLD.caller_thread_id;
  UPDATE thread_requests SET open_below_1 = open_below_1 + -1
   WHERE (OLD.lineage_only = 0 AND ((OLD.settled_at IS NULL AND (OLD.kind = 'spawn' OR (OLD.kind IN ('send','ask') AND OLD.notify = 1)))
        OR (OLD.kind = 'spawn' AND OLD.target_computer_id <> '' AND OLD.nested_open = 1))) AND (OLD.caller_thread_id IS NOT NEW.caller_thread_id OR NOT (NEW.lineage_only = 0 AND ((NEW.settled_at IS NULL AND (NEW.kind = 'spawn' OR (NEW.kind IN ('send','ask') AND NEW.notify = 1)))
        OR (NEW.kind = 'spawn' AND NEW.target_computer_id <> '' AND NEW.nested_open = 1)))) AND kind = 'spawn' AND target_computer_id = '' AND lineage_only = 0 AND target_thread_id <> ''
     AND target_thread_id = (SELECT p.caller_thread_id FROM thread_requests p
          WHERE p.kind = 'spawn' AND p.target_computer_id = '' AND p.lineage_only = 0 AND p.target_thread_id = OLD.caller_thread_id);
  UPDATE thread_requests SET open_below_2 = open_below_2 + -1
   WHERE (OLD.lineage_only = 0 AND ((OLD.settled_at IS NULL AND (OLD.kind = 'spawn' OR (OLD.kind IN ('send','ask') AND OLD.notify = 1)))
        OR (OLD.kind = 'spawn' AND OLD.target_computer_id <> '' AND OLD.nested_open = 1))) AND (OLD.caller_thread_id IS NOT NEW.caller_thread_id OR NOT (NEW.lineage_only = 0 AND ((NEW.settled_at IS NULL AND (NEW.kind = 'spawn' OR (NEW.kind IN ('send','ask') AND NEW.notify = 1)))
        OR (NEW.kind = 'spawn' AND NEW.target_computer_id <> '' AND NEW.nested_open = 1)))) AND kind = 'spawn' AND target_computer_id = '' AND lineage_only = 0 AND target_thread_id <> ''
     AND target_thread_id = (SELECT p.caller_thread_id FROM thread_requests p
          WHERE p.kind = 'spawn' AND p.target_computer_id = '' AND p.lineage_only = 0 AND p.target_thread_id = (SELECT p.caller_thread_id FROM thread_requests p
          WHERE p.kind = 'spawn' AND p.target_computer_id = '' AND p.lineage_only = 0 AND p.target_thread_id = OLD.caller_thread_id));
  UPDATE thread_requests SET open_below_3 = open_below_3 + -1
   WHERE (OLD.lineage_only = 0 AND ((OLD.settled_at IS NULL AND (OLD.kind = 'spawn' OR (OLD.kind IN ('send','ask') AND OLD.notify = 1)))
        OR (OLD.kind = 'spawn' AND OLD.target_computer_id <> '' AND OLD.nested_open = 1))) AND (OLD.caller_thread_id IS NOT NEW.caller_thread_id OR NOT (NEW.lineage_only = 0 AND ((NEW.settled_at IS NULL AND (NEW.kind = 'spawn' OR (NEW.kind IN ('send','ask') AND NEW.notify = 1)))
        OR (NEW.kind = 'spawn' AND NEW.target_computer_id <> '' AND NEW.nested_open = 1)))) AND kind = 'spawn' AND target_computer_id = '' AND lineage_only = 0 AND target_thread_id <> ''
     AND target_thread_id = (SELECT p.caller_thread_id FROM thread_requests p
          WHERE p.kind = 'spawn' AND p.target_computer_id = '' AND p.lineage_only = 0 AND p.target_thread_id = (SELECT p.caller_thread_id FROM thread_requests p
          WHERE p.kind = 'spawn' AND p.target_computer_id = '' AND p.lineage_only = 0 AND p.target_thread_id = (SELECT p.caller_thread_id FROM thread_requests p
          WHERE p.kind = 'spawn' AND p.target_computer_id = '' AND p.lineage_only = 0 AND p.target_thread_id = OLD.caller_thread_id)));
  UPDATE thread_requests SET open_below_1 = open_below_1 + -OLD.open_below_0, open_below_2 = open_below_2 + -OLD.open_below_1, open_below_3 = open_below_3 + -OLD.open_below_2
   WHERE (OLD.kind = 'spawn' AND OLD.target_computer_id = '' AND OLD.lineage_only = 0 AND OLD.target_thread_id <> '') AND (OLD.caller_thread_id IS NOT NEW.caller_thread_id OR NOT ((OLD.kind = 'spawn' AND OLD.target_computer_id = '' AND OLD.lineage_only = 0 AND OLD.target_thread_id <> '') AND (NEW.kind = 'spawn' AND NEW.target_computer_id = '' AND NEW.lineage_only = 0 AND NEW.target_thread_id <> '') AND OLD.target_thread_id = NEW.target_thread_id)) AND kind = 'spawn' AND target_computer_id = '' AND lineage_only = 0 AND target_thread_id <> ''
     AND target_thread_id = OLD.caller_thread_id;
  UPDATE thread_requests SET open_below_2 = open_below_2 + -OLD.open_below_0, open_below_3 = open_below_3 + -OLD.open_below_1
   WHERE (OLD.kind = 'spawn' AND OLD.target_computer_id = '' AND OLD.lineage_only = 0 AND OLD.target_thread_id <> '') AND (OLD.caller_thread_id IS NOT NEW.caller_thread_id OR NOT ((OLD.kind = 'spawn' AND OLD.target_computer_id = '' AND OLD.lineage_only = 0 AND OLD.target_thread_id <> '') AND (NEW.kind = 'spawn' AND NEW.target_computer_id = '' AND NEW.lineage_only = 0 AND NEW.target_thread_id <> '') AND OLD.target_thread_id = NEW.target_thread_id)) AND kind = 'spawn' AND target_computer_id = '' AND lineage_only = 0 AND target_thread_id <> ''
     AND target_thread_id = (SELECT p.caller_thread_id FROM thread_requests p
          WHERE p.kind = 'spawn' AND p.target_computer_id = '' AND p.lineage_only = 0 AND p.target_thread_id = OLD.caller_thread_id);
  UPDATE thread_requests SET open_below_3 = open_below_3 + -OLD.open_below_0
   WHERE (OLD.kind = 'spawn' AND OLD.target_computer_id = '' AND OLD.lineage_only = 0 AND OLD.target_thread_id <> '') AND (OLD.caller_thread_id IS NOT NEW.caller_thread_id OR NOT ((OLD.kind = 'spawn' AND OLD.target_computer_id = '' AND OLD.lineage_only = 0 AND OLD.target_thread_id <> '') AND (NEW.kind = 'spawn' AND NEW.target_computer_id = '' AND NEW.lineage_only = 0 AND NEW.target_thread_id <> '') AND OLD.target_thread_id = NEW.target_thread_id)) AND kind = 'spawn' AND target_computer_id = '' AND lineage_only = 0 AND target_thread_id <> ''
     AND target_thread_id = (SELECT p.caller_thread_id FROM thread_requests p
          WHERE p.kind = 'spawn' AND p.target_computer_id = '' AND p.lineage_only = 0 AND p.target_thread_id = (SELECT p.caller_thread_id FROM thread_requests p
          WHERE p.kind = 'spawn' AND p.target_computer_id = '' AND p.lineage_only = 0 AND p.target_thread_id = OLD.caller_thread_id));
  UPDATE thread_requests SET open_below_0 = (SELECT COUNT(*) FROM thread_requests c WHERE c.caller_thread_id = NEW.target_thread_id
        AND c.lineage_only = 0 AND c.settled_at IS NULL AND (c.kind = 'spawn' OR (c.kind IN ('send','ask') AND c.notify = 1)))
      + (SELECT COUNT(*) FROM thread_requests c WHERE c.caller_thread_id = NEW.target_thread_id
        AND c.nested_open = 1 AND c.kind = 'spawn' AND c.target_computer_id <> '' AND c.lineage_only = 0 AND +c.settled_at IS NOT NULL),
    open_below_1 = (SELECT COALESCE(SUM(c.open_below_0), 0) FROM thread_requests c WHERE c.caller_thread_id = NEW.target_thread_id
        AND c.kind = 'spawn' AND c.target_computer_id = '' AND c.lineage_only = 0 AND c.target_thread_id <> ''
        AND (c.open_below_0 + c.open_below_1 + c.open_below_2 + c.open_below_3) > 0),
    open_below_2 = (SELECT COALESCE(SUM(c.open_below_1), 0) FROM thread_requests c WHERE c.caller_thread_id = NEW.target_thread_id
        AND c.kind = 'spawn' AND c.target_computer_id = '' AND c.lineage_only = 0 AND c.target_thread_id <> ''
        AND (c.open_below_0 + c.open_below_1 + c.open_below_2 + c.open_below_3) > 0),
    open_below_3 = (SELECT COALESCE(SUM(c.open_below_2), 0) FROM thread_requests c WHERE c.caller_thread_id = NEW.target_thread_id
        AND c.kind = 'spawn' AND c.target_computer_id = '' AND c.lineage_only = 0 AND c.target_thread_id <> ''
        AND (c.open_below_0 + c.open_below_1 + c.open_below_2 + c.open_below_3) > 0)
   WHERE (NEW.kind = 'spawn' AND NEW.target_computer_id = '' AND NEW.lineage_only = 0 AND NEW.target_thread_id <> '') AND NOT ((OLD.kind = 'spawn' AND OLD.target_computer_id = '' AND OLD.lineage_only = 0 AND OLD.target_thread_id <> '') AND (NEW.kind = 'spawn' AND NEW.target_computer_id = '' AND NEW.lineage_only = 0 AND NEW.target_thread_id <> '') AND OLD.target_thread_id = NEW.target_thread_id) AND token = NEW.token;
  UPDATE thread_requests SET open_below_0 = 0, open_below_1 = 0, open_below_2 = 0, open_below_3 = 0
   WHERE (OLD.kind = 'spawn' AND OLD.target_computer_id = '' AND OLD.lineage_only = 0 AND OLD.target_thread_id <> '') AND NOT (NEW.kind = 'spawn' AND NEW.target_computer_id = '' AND NEW.lineage_only = 0 AND NEW.target_thread_id <> '') AND token = NEW.token;
  UPDATE thread_requests SET open_below_0 = open_below_0 + 1
   WHERE (NEW.lineage_only = 0 AND ((NEW.settled_at IS NULL AND (NEW.kind = 'spawn' OR (NEW.kind IN ('send','ask') AND NEW.notify = 1)))
        OR (NEW.kind = 'spawn' AND NEW.target_computer_id <> '' AND NEW.nested_open = 1))) AND (OLD.caller_thread_id IS NOT NEW.caller_thread_id OR NOT (OLD.lineage_only = 0 AND ((OLD.settled_at IS NULL AND (OLD.kind = 'spawn' OR (OLD.kind IN ('send','ask') AND OLD.notify = 1)))
        OR (OLD.kind = 'spawn' AND OLD.target_computer_id <> '' AND OLD.nested_open = 1)))) AND kind = 'spawn' AND target_computer_id = '' AND lineage_only = 0 AND target_thread_id <> ''
     AND target_thread_id = NEW.caller_thread_id;
  UPDATE thread_requests SET open_below_1 = open_below_1 + 1
   WHERE (NEW.lineage_only = 0 AND ((NEW.settled_at IS NULL AND (NEW.kind = 'spawn' OR (NEW.kind IN ('send','ask') AND NEW.notify = 1)))
        OR (NEW.kind = 'spawn' AND NEW.target_computer_id <> '' AND NEW.nested_open = 1))) AND (OLD.caller_thread_id IS NOT NEW.caller_thread_id OR NOT (OLD.lineage_only = 0 AND ((OLD.settled_at IS NULL AND (OLD.kind = 'spawn' OR (OLD.kind IN ('send','ask') AND OLD.notify = 1)))
        OR (OLD.kind = 'spawn' AND OLD.target_computer_id <> '' AND OLD.nested_open = 1)))) AND kind = 'spawn' AND target_computer_id = '' AND lineage_only = 0 AND target_thread_id <> ''
     AND target_thread_id = (SELECT p.caller_thread_id FROM thread_requests p
          WHERE p.kind = 'spawn' AND p.target_computer_id = '' AND p.lineage_only = 0 AND p.target_thread_id = NEW.caller_thread_id);
  UPDATE thread_requests SET open_below_2 = open_below_2 + 1
   WHERE (NEW.lineage_only = 0 AND ((NEW.settled_at IS NULL AND (NEW.kind = 'spawn' OR (NEW.kind IN ('send','ask') AND NEW.notify = 1)))
        OR (NEW.kind = 'spawn' AND NEW.target_computer_id <> '' AND NEW.nested_open = 1))) AND (OLD.caller_thread_id IS NOT NEW.caller_thread_id OR NOT (OLD.lineage_only = 0 AND ((OLD.settled_at IS NULL AND (OLD.kind = 'spawn' OR (OLD.kind IN ('send','ask') AND OLD.notify = 1)))
        OR (OLD.kind = 'spawn' AND OLD.target_computer_id <> '' AND OLD.nested_open = 1)))) AND kind = 'spawn' AND target_computer_id = '' AND lineage_only = 0 AND target_thread_id <> ''
     AND target_thread_id = (SELECT p.caller_thread_id FROM thread_requests p
          WHERE p.kind = 'spawn' AND p.target_computer_id = '' AND p.lineage_only = 0 AND p.target_thread_id = (SELECT p.caller_thread_id FROM thread_requests p
          WHERE p.kind = 'spawn' AND p.target_computer_id = '' AND p.lineage_only = 0 AND p.target_thread_id = NEW.caller_thread_id));
  UPDATE thread_requests SET open_below_3 = open_below_3 + 1
   WHERE (NEW.lineage_only = 0 AND ((NEW.settled_at IS NULL AND (NEW.kind = 'spawn' OR (NEW.kind IN ('send','ask') AND NEW.notify = 1)))
        OR (NEW.kind = 'spawn' AND NEW.target_computer_id <> '' AND NEW.nested_open = 1))) AND (OLD.caller_thread_id IS NOT NEW.caller_thread_id OR NOT (OLD.lineage_only = 0 AND ((OLD.settled_at IS NULL AND (OLD.kind = 'spawn' OR (OLD.kind IN ('send','ask') AND OLD.notify = 1)))
        OR (OLD.kind = 'spawn' AND OLD.target_computer_id <> '' AND OLD.nested_open = 1)))) AND kind = 'spawn' AND target_computer_id = '' AND lineage_only = 0 AND target_thread_id <> ''
     AND target_thread_id = (SELECT p.caller_thread_id FROM thread_requests p
          WHERE p.kind = 'spawn' AND p.target_computer_id = '' AND p.lineage_only = 0 AND p.target_thread_id = (SELECT p.caller_thread_id FROM thread_requests p
          WHERE p.kind = 'spawn' AND p.target_computer_id = '' AND p.lineage_only = 0 AND p.target_thread_id = (SELECT p.caller_thread_id FROM thread_requests p
          WHERE p.kind = 'spawn' AND p.target_computer_id = '' AND p.lineage_only = 0 AND p.target_thread_id = NEW.caller_thread_id)));
  UPDATE thread_requests SET open_below_1 = open_below_1 + (SELECT s.open_below_0 FROM thread_requests s WHERE s.token = NEW.token), open_below_2 = open_below_2 + (SELECT s.open_below_1 FROM thread_requests s WHERE s.token = NEW.token), open_below_3 = open_below_3 + (SELECT s.open_below_2 FROM thread_requests s WHERE s.token = NEW.token)
   WHERE (NEW.kind = 'spawn' AND NEW.target_computer_id = '' AND NEW.lineage_only = 0 AND NEW.target_thread_id <> '') AND (OLD.caller_thread_id IS NOT NEW.caller_thread_id OR NOT ((OLD.kind = 'spawn' AND OLD.target_computer_id = '' AND OLD.lineage_only = 0 AND OLD.target_thread_id <> '') AND (NEW.kind = 'spawn' AND NEW.target_computer_id = '' AND NEW.lineage_only = 0 AND NEW.target_thread_id <> '') AND OLD.target_thread_id = NEW.target_thread_id)) AND kind = 'spawn' AND target_computer_id = '' AND lineage_only = 0 AND target_thread_id <> ''
     AND target_thread_id = NEW.caller_thread_id;
  UPDATE thread_requests SET open_below_2 = open_below_2 + (SELECT s.open_below_0 FROM thread_requests s WHERE s.token = NEW.token), open_below_3 = open_below_3 + (SELECT s.open_below_1 FROM thread_requests s WHERE s.token = NEW.token)
   WHERE (NEW.kind = 'spawn' AND NEW.target_computer_id = '' AND NEW.lineage_only = 0 AND NEW.target_thread_id <> '') AND (OLD.caller_thread_id IS NOT NEW.caller_thread_id OR NOT ((OLD.kind = 'spawn' AND OLD.target_computer_id = '' AND OLD.lineage_only = 0 AND OLD.target_thread_id <> '') AND (NEW.kind = 'spawn' AND NEW.target_computer_id = '' AND NEW.lineage_only = 0 AND NEW.target_thread_id <> '') AND OLD.target_thread_id = NEW.target_thread_id)) AND kind = 'spawn' AND target_computer_id = '' AND lineage_only = 0 AND target_thread_id <> ''
     AND target_thread_id = (SELECT p.caller_thread_id FROM thread_requests p
          WHERE p.kind = 'spawn' AND p.target_computer_id = '' AND p.lineage_only = 0 AND p.target_thread_id = NEW.caller_thread_id);
  UPDATE thread_requests SET open_below_3 = open_below_3 + (SELECT s.open_below_0 FROM thread_requests s WHERE s.token = NEW.token)
   WHERE (NEW.kind = 'spawn' AND NEW.target_computer_id = '' AND NEW.lineage_only = 0 AND NEW.target_thread_id <> '') AND (OLD.caller_thread_id IS NOT NEW.caller_thread_id OR NOT ((OLD.kind = 'spawn' AND OLD.target_computer_id = '' AND OLD.lineage_only = 0 AND OLD.target_thread_id <> '') AND (NEW.kind = 'spawn' AND NEW.target_computer_id = '' AND NEW.lineage_only = 0 AND NEW.target_thread_id <> '') AND OLD.target_thread_id = NEW.target_thread_id)) AND kind = 'spawn' AND target_computer_id = '' AND lineage_only = 0 AND target_thread_id <> ''
     AND target_thread_id = (SELECT p.caller_thread_id FROM thread_requests p
          WHERE p.kind = 'spawn' AND p.target_computer_id = '' AND p.lineage_only = 0 AND p.target_thread_id = (SELECT p.caller_thread_id FROM thread_requests p
          WHERE p.kind = 'spawn' AND p.target_computer_id = '' AND p.lineage_only = 0 AND p.target_thread_id = NEW.caller_thread_id));
END;


CREATE TRIGGER trg_thread_requests_spawn_cycle_insert BEFORE INSERT ON thread_requests
WHEN (NEW.kind = 'spawn' AND NEW.target_computer_id = '' AND NEW.lineage_only = 0 AND NEW.target_thread_id <> '') AND (NEW.target_thread_id = NEW.caller_thread_id OR NEW.target_thread_id = (SELECT p.caller_thread_id FROM thread_requests p
          WHERE p.kind = 'spawn' AND p.target_computer_id = '' AND p.lineage_only = 0 AND p.target_thread_id = NEW.caller_thread_id) OR NEW.target_thread_id = (SELECT p.caller_thread_id FROM thread_requests p
          WHERE p.kind = 'spawn' AND p.target_computer_id = '' AND p.lineage_only = 0 AND p.target_thread_id = (SELECT p.caller_thread_id FROM thread_requests p
          WHERE p.kind = 'spawn' AND p.target_computer_id = '' AND p.lineage_only = 0 AND p.target_thread_id = NEW.caller_thread_id)) OR NEW.target_thread_id = (SELECT p.caller_thread_id FROM thread_requests p
          WHERE p.kind = 'spawn' AND p.target_computer_id = '' AND p.lineage_only = 0 AND p.target_thread_id = (SELECT p.caller_thread_id FROM thread_requests p
          WHERE p.kind = 'spawn' AND p.target_computer_id = '' AND p.lineage_only = 0 AND p.target_thread_id = (SELECT p.caller_thread_id FROM thread_requests p
          WHERE p.kind = 'spawn' AND p.target_computer_id = '' AND p.lineage_only = 0 AND p.target_thread_id = NEW.caller_thread_id))))
BEGIN
  SELECT RAISE(ABORT, 'thread_requests: a local spawn reaches a thread above its caller');
END;

CREATE TRIGGER trg_thread_requests_spawn_cycle_update BEFORE UPDATE OF caller_thread_id, kind, lineage_only, target_thread_id, target_computer_id ON thread_requests
WHEN (NEW.kind = 'spawn' AND NEW.target_computer_id = '' AND NEW.lineage_only = 0 AND NEW.target_thread_id <> '') AND (NEW.target_thread_id = NEW.caller_thread_id OR NEW.target_thread_id = (SELECT p.caller_thread_id FROM thread_requests p
          WHERE p.kind = 'spawn' AND p.target_computer_id = '' AND p.lineage_only = 0 AND p.target_thread_id = NEW.caller_thread_id) OR NEW.target_thread_id = (SELECT p.caller_thread_id FROM thread_requests p
          WHERE p.kind = 'spawn' AND p.target_computer_id = '' AND p.lineage_only = 0 AND p.target_thread_id = (SELECT p.caller_thread_id FROM thread_requests p
          WHERE p.kind = 'spawn' AND p.target_computer_id = '' AND p.lineage_only = 0 AND p.target_thread_id = NEW.caller_thread_id)) OR NEW.target_thread_id = (SELECT p.caller_thread_id FROM thread_requests p
          WHERE p.kind = 'spawn' AND p.target_computer_id = '' AND p.lineage_only = 0 AND p.target_thread_id = (SELECT p.caller_thread_id FROM thread_requests p
          WHERE p.kind = 'spawn' AND p.target_computer_id = '' AND p.lineage_only = 0 AND p.target_thread_id = (SELECT p.caller_thread_id FROM thread_requests p
          WHERE p.kind = 'spawn' AND p.target_computer_id = '' AND p.lineage_only = 0 AND p.target_thread_id = NEW.caller_thread_id))))
BEGIN
  SELECT RAISE(ABORT, 'thread_requests: a local spawn reaches a thread above its caller');
END;
`

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
		WHERE kind = 'spawn' AND target_computer_id = '' AND lineage_only = 0 AND target_thread_id = ?`)
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
