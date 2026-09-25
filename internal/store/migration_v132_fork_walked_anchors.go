package store

// forkWalkedAnchorsV132SQL adds thread_fork_walked, the anchors a thread's
// readers walk rather than serve from the stamp of the level that holds
// them (fork_walked.go), and records the anchors every existing thread
// would have recorded had the writers run when its rows were written:
//
//   - for each hidden row, the anchorable ancestors of its parent when the
//     hiding thread does not hide that parent too: fork creation, a copy,
//     a hide and a late row the source placed below the cut each leave one;
//   - for each row a card counts, local or imported, of a fork or a holder
//     (writers) whose parent the thread does not hold, the anchorable
//     ancestors of that parent: a fork's own rows under an inherited
//     anchor, and a holder's rows whose parent stayed with the thread it
//     took them from;
//   - for a fork that reads an ancestor through a lower cut than a thread
//     that reads the fork, or not at all, since a revert lowered its cuts,
//     the stamped copies it holds, each a stamped row whose id it hides.
//     A holder has no stamp before this migration.
//
// A thread resolves a parent through the threads whose rows it reads
// (scope): its own, its lineage's and, for a thread its readers read
// before other levels, those deeper levels. A row is found by id in each
// of them, a local row by key and an imported one through
// idx_import_history_items_id: no index leads with an item id alone. A
// row that more than one of them holds under the same id is the same
// history, so each gives the same parent. The walk goes up through rows a
// card counts, as markWalkedAncestorsTx does, and records each anchorable
// row it reaches, local or imported, but for a thread's own anchors, whose
// stamps the thread keeps.
const forkWalkedAnchorsV132SQL = `
CREATE TABLE thread_fork_walked (
    thread_id TEXT NOT NULL REFERENCES threads(id) ON DELETE CASCADE,
    item_id   TEXT NOT NULL,
    PRIMARY KEY (thread_id, item_id)
) WITHOUT ROWID;

INSERT OR IGNORE INTO thread_fork_walked (thread_id, item_id)
WITH RECURSIVE
scope(marker, tid) AS (
    SELECT thread_id, thread_id FROM thread_fork_lineage
    UNION SELECT thread_id, ancestor_id FROM thread_fork_lineage
    UNION SELECT r.ancestor_id, r.ancestor_id
            FROM thread_fork_lineage r CROSS JOIN threads t ON t.id = r.ancestor_id
           WHERE t.mode = 'holder'
    UNION SELECT r.ancestor_id, deeper.ancestor_id
            FROM thread_fork_lineage r
            CROSS JOIN thread_fork_lineage deeper ON deeper.thread_id = r.thread_id AND deeper.depth > r.depth
    UNION SELECT thread_id, thread_id FROM thread_fork_hidden
),
writers(marker) AS (
    SELECT thread_id FROM thread_fork_lineage
    UNION SELECT id FROM threads WHERE mode = 'holder'
),
start(marker, parent) AS (
    SELECT h.thread_id, i.parent_id
      FROM thread_fork_hidden h
      CROSS JOIN scope s ON s.marker = h.thread_id
      CROSS JOIN items i ON i.thread_id = s.tid AND i.id = h.item_id
     WHERE i.parent_id <> ''
       AND NOT EXISTS (SELECT 1 FROM thread_fork_hidden ph WHERE ph.thread_id = h.thread_id AND ph.item_id = i.parent_id)
    UNION
    SELECT h.thread_id, i.parent_id
      FROM thread_fork_hidden h
      CROSS JOIN scope s ON s.marker = h.thread_id
      CROSS JOIN import_history_items i ON i.id = h.item_id
     WHERE i.parent_id <> ''
       AND EXISTS (SELECT 1 FROM thread_import_chunks refs WHERE refs.chunk_id = i.chunk_id AND refs.thread_id = s.tid)
       AND NOT EXISTS (SELECT 1 FROM thread_import_item_overrides o WHERE o.thread_id = s.tid AND o.item_id = i.id)
       AND NOT EXISTS (SELECT 1 FROM thread_fork_hidden ph WHERE ph.thread_id = h.thread_id AND ph.item_id = i.parent_id)
    UNION
    SELECT own.marker, own.parent
      FROM (SELECT i.thread_id AS marker, i.parent_id AS parent, i.kind, i.tool_name
              FROM writers w
              CROSS JOIN items i ON i.thread_id = w.marker
             WHERE i.parent_id <> ''
            UNION ALL
            SELECT refs.thread_id, i.parent_id, i.kind, i.tool_name
              FROM writers w
              CROSS JOIN thread_import_chunks refs ON refs.thread_id = w.marker
              CROSS JOIN import_history_items i ON i.chunk_id = refs.chunk_id
             WHERE i.parent_id <> ''
               AND NOT EXISTS (SELECT 1 FROM thread_import_item_overrides o WHERE o.thread_id = refs.thread_id AND o.item_id = i.id)) own
     WHERE NOT (own.kind = 'notification' AND own.tool_name = 'plan_update')
       AND NOT EXISTS (SELECT 1 FROM items p WHERE p.thread_id = own.marker AND p.id = own.parent)
       AND NOT EXISTS (SELECT 1 FROM import_history_items p
                        WHERE p.id = own.parent
                          AND EXISTS (SELECT 1 FROM thread_import_chunks pr WHERE pr.chunk_id = p.chunk_id AND pr.thread_id = own.marker)
                          AND NOT EXISTS (SELECT 1 FROM thread_import_item_overrides o WHERE o.thread_id = own.marker AND o.item_id = p.id))
),
up(marker, id) AS (
    SELECT marker, parent FROM start
    UNION
    SELECT up.marker, i.parent_id
      FROM up
      CROSS JOIN scope s ON s.marker = up.marker
      CROSS JOIN items i ON i.thread_id = s.tid AND i.id = up.id
     WHERE i.parent_id <> '' AND NOT (i.kind = 'notification' AND i.tool_name = 'plan_update')
    UNION
    SELECT up.marker, i.parent_id
      FROM up
      CROSS JOIN scope s ON s.marker = up.marker
      CROSS JOIN import_history_items i ON i.id = up.id
     WHERE i.parent_id <> '' AND NOT (i.kind = 'notification' AND i.tool_name = 'plan_update')
       AND EXISTS (SELECT 1 FROM thread_import_chunks refs WHERE refs.chunk_id = i.chunk_id AND refs.thread_id = s.tid)
       AND NOT EXISTS (SELECT 1 FROM thread_import_item_overrides o WHERE o.thread_id = s.tid AND o.item_id = i.id)
)
SELECT up.marker, up.id
  FROM up
 WHERE NOT EXISTS (SELECT 1 FROM items own WHERE own.thread_id = up.marker AND own.id = up.id)
   AND (EXISTS (SELECT 1 FROM scope s CROSS JOIN items i ON i.thread_id = s.tid AND i.id = up.id
                WHERE s.marker = up.marker AND i.kind = 'tool_call' AND i.tool_name <> 'collab_agent')
    OR EXISTS (SELECT 1 FROM scope s CROSS JOIN import_history_items i ON i.id = up.id
                WHERE s.marker = up.marker AND i.kind = 'tool_call' AND i.tool_name <> 'collab_agent'
                  AND EXISTS (SELECT 1 FROM thread_import_chunks refs WHERE refs.chunk_id = i.chunk_id AND refs.thread_id = s.tid)
                  AND NOT EXISTS (SELECT 1 FROM thread_import_item_overrides o WHERE o.thread_id = s.tid AND o.item_id = i.id)))
UNION
SELECT s.thread_id, s.item_id
  FROM (SELECT DISTINCT r.ancestor_id AS id
          FROM thread_fork_lineage r
          CROSS JOIN threads t ON t.id = r.ancestor_id
          CROSS JOIN thread_fork_lineage behind ON behind.thread_id = r.thread_id AND behind.depth > r.depth
         WHERE t.mode <> 'holder'
           AND NOT EXISTS (SELECT 1 FROM thread_fork_lineage own
                            WHERE own.thread_id = r.ancestor_id AND own.ancestor_id = behind.ancestor_id
                              AND (own.cut_turn_index, own.cut_item_index) >= (behind.cut_turn_index, behind.cut_item_index))) narrowed
  CROSS JOIN subagent_aggregates s ON s.thread_id = narrowed.id
 WHERE EXISTS (SELECT 1 FROM thread_fork_hidden h WHERE h.thread_id = s.thread_id AND h.item_id = s.item_id);
`
