package store

// Migration v172 adds tracker_cache_parents: each parent's name (key,
// title and type level) as the newest read that named it left it, one row
// per parent a card names, whether or not the mirror keeps the parent. It
// is the one source of a parent's name for every view (storeMirrorCardsTx
// writes it; FinishMirrorCleanup deletes a row no stored issue names). The
// backfill names each parent the mirror keeps from its stored card, which
// is what a view named it from before; an unkept parent stays unnamed.
// Every board's cursor is forgotten, so its next poll reads every row
// once and names the parents it does not keep.
const trackerParentsV172SQL = `
CREATE TABLE tracker_cache_parents (
 root_id INTEGER NOT NULL REFERENCES tracker_cache_roots(id) ON DELETE CASCADE,
 issue_id TEXT NOT NULL CHECK(issue_id <> ''),
 issue_key TEXT NOT NULL CHECK(issue_key <> ''),
 title TEXT NOT NULL,
 type_level INTEGER NOT NULL,
 read_at INTEGER NOT NULL CHECK(read_at > 0),
 PRIMARY KEY (root_id, issue_id)
) WITHOUT ROWID;

INSERT INTO tracker_cache_parents(root_id, issue_id, issue_key, title, type_level, read_at)
SELECT p.root_id, p.issue_id, p.issue_key, p.title, p.type_level, p.read_at FROM tracker_cache_issues p
 WHERE p.read_at > 0
   AND EXISTS (SELECT 1 FROM tracker_cache_issues c WHERE c.root_id = p.root_id AND c.parent_id = p.issue_id AND c.parent_id <> '')
   AND (EXISTS (SELECT 1 FROM tracker_cache_board_issues kept_row
          WHERE kept_row.root_id = p.root_id AND kept_row.issue_id = p.issue_id
            AND EXISTS (SELECT 1 FROM tracker_cache_boards shown_board
              JOIN tracker_cache_roots shown_root ON shown_root.id = shown_board.root_id
              JOIN project_tracker_boards shown_listing ON shown_listing.connection_id = shown_root.connection_id
               AND shown_listing.board_id = shown_board.board_id
             WHERE shown_board.root_id = kept_row.root_id AND shown_board.board_id = kept_row.board_id
               AND shown_board.retired = 0 AND shown_board.read_at > 0))
     OR EXISTS (SELECT 1 FROM tracker_cache_roots link_root
          JOIN tracker_connections link_conn ON link_conn.id = link_root.connection_id
          JOIN thread_tickets link_ticket ON link_ticket.kind = link_conn.kind AND link_ticket.site = link_conn.site
          JOIN owned_threads link_thread ON link_thread.id = link_ticket.thread_id
         WHERE link_root.id = p.root_id AND link_conn.id = (SELECT MIN(reader.id) FROM tracker_connections reader
              WHERE reader.kind = link_conn.kind AND reader.site = link_conn.site AND reader.parent_id IS NULL AND reader.enabled = 1)
           AND link_ticket.issue_id = p.issue_id)
     OR EXISTS (SELECT 1 FROM tracker_cache_open_tickets kept_open
          JOIN tracker_cache_roots open_root ON open_root.id = kept_open.root_id AND open_root.connection_id IS NOT NULL
         WHERE kept_open.root_id = p.root_id AND kept_open.issue_id = p.issue_id));

UPDATE tracker_cache_sync SET cursor = NULL WHERE cursor IS NOT NULL;
`

const trackerParentsMigrationVersion = 172
