package store

// Migration v128 fits the reads and the row stamps of an agent's stops
// (agent_stops.go) to the stops being rows:
//
//   - idx_items_completion_of adds the order of a launch's stops, so its
//     newest stop is one index step however many runs it had
//     (newestStopIDSQL: the tray's run state and CurrentParkedStop).
//   - idx_items_subagent_wake holds the wake rows under each transcript
//     root by creation time (agentWakesSinceSQL). Its predicate is
//     wakeFlagSQL's.
//   - the history triggers and the subagent_aggregates stamp triggers are
//     reinstalled with sibling legs that pass over a parked sibling, whose
//     read no write to its launch or under it changes (stampedRowIDsSQL,
//     subagentAggregateStampSiblingsSQL).
const agentStopReadsMigrationVersion = 128

var agentStopReadsV128SQL = `
DROP INDEX idx_items_completion_of;
CREATE INDEX idx_items_completion_of
    ON items(thread_id, completion_of, created_at, turn_index, item_index) WHERE completion_of <> '';

CREATE INDEX idx_items_subagent_wake
    ON items(thread_id, parent_id, created_at)
 WHERE kind = 'user_text' AND parent_id <> '' AND ` + wakeFlagSQL("") + `;

` + dropHistoryRevTriggersSQL + historyRevTriggersSQL + `
` + dropSubagentAggregateTriggersSQL + subagentAggregateTriggersSQL
