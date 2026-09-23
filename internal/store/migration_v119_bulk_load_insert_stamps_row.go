package store

// An insert under history_bulk_load moves a row a read already showed, or
// rebuilds a thread the same transaction emptied, so it changes no other
// row's read (history_sync.go). The insert trigger nevertheless re-stamped
// the row's parent chain, carriers and completion siblings. The flag freezes
// the stamp, so every re-stamp after the first left rev unchanged and fired
// the update trigger's own walk: moving one 64-row chunk of a launch's
// children made 629 row updates besides its 64 inserts. This migration
// reinstalls the insert trigger so that under the flag it stamps only the
// inserted row.
var bulkLoadInsertStampsRowV119SQL = `DROP TRIGGER trg_items_rev_insert;
` + historyRevInsertTriggerSQL
