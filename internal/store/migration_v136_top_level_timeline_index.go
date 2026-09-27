package store

// Migration v136 indexes the visible rows with no parent, the rows the
// main timeline shows, in a thread's local and imported history. A
// main-timeline read (mainTimelineFilterFor) walks these indexes instead
// of the ordering index, which also reads every subagent child row
// between two top-level rows: in a thread with a dozen agents, most of the
// rows a page, run expansion or held-window check passes over. Each index
// carries the visibility filter's columns and the id, so an arm that
// selects ids and coordinates reads no table row. A read that states
// topLevelItemsFilterFor without the visibility term cannot use them.
//
// The predicate is mainTimelineFilterFor's, visibility term first. With no
// statistics SQLite cannot tell two usable partial indexes apart; the
// visibility term keeps a read that is not a main-timeline window, such as
// the boot pass over running agents, off these indexes.
const topLevelTimelineIndexMigrationVersion = 136

const topLevelTimelineIndexV136SQL = `CREATE INDEX idx_items_top_level
    ON items(thread_id, turn_index, item_index, kind, tool_name, id)
 WHERE NOT (kind = 'notification' AND tool_name = 'plan_update') AND parent_id = '';
CREATE INDEX idx_import_history_items_top_level
    ON import_history_items(chunk_id, turn_index, item_index, kind, tool_name, id)
 WHERE NOT (kind = 'notification' AND tool_name = 'plan_update') AND parent_id = ''`
