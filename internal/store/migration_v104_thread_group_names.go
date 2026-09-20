package store

// threadGroupNamesV104SQL makes a group name unique within its project.
//
// A group is resolved by name: `thread_update` and `thread_spawn` take a
// group NAME and create the group when the project has none of that name.
// Without a constraint two concurrent calls both read "no such group" and
// both insert, leaving two identically named sidebar rows a reader cannot
// tell apart. The index is what makes the resolve-or-insert inside the
// organize transaction (ensureThreadGroupTx) a decision rather than a race.
//
// The match ignores case for the same reason the name lookup does: two rows
// differing only in case are two rows nobody can tell apart.
//
// Existing duplicates are merged rather than refused. The survivor is the
// oldest row, ties broken by id, so every database that runs this picks the
// same one; its members gain the other rows' threads and the other rows go.
// A merged-away group's pin is not carried over: the survivor keeps its own,
// and a pin is a sidebar preference the user can set again.
const threadGroupNamesV104SQL = `
UPDATE threads
   SET group_id = (
        SELECT keep.id
          FROM thread_groups keep
          JOIN thread_groups cur ON cur.id = threads.group_id
         WHERE keep.project_id = cur.project_id
           AND keep.name = cur.name COLLATE NOCASE
         ORDER BY keep.created_at, keep.id
         LIMIT 1)
 WHERE group_id IS NOT NULL
   AND EXISTS (SELECT 1 FROM thread_groups g WHERE g.id = threads.group_id);

DELETE FROM thread_groups AS dup
 WHERE EXISTS (
        SELECT 1
          FROM thread_groups keep
         WHERE keep.project_id = dup.project_id
           AND keep.name = dup.name COLLATE NOCASE
           AND (keep.created_at < dup.created_at
                OR (keep.created_at = dup.created_at AND keep.id < dup.id)));

CREATE UNIQUE INDEX idx_thread_groups_project_name
    ON thread_groups (project_id, name COLLATE NOCASE);
`
