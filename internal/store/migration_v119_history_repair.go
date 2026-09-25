package store

// v119 finishes removing background sealing, stops the payload leak and
// repairs the history both left behind.
//
// An insert under history_bulk_load moves a row a read already showed, or
// rebuilds a thread the same transaction emptied, so it changes no other
// row's read (history_rev_triggers.go). The insert trigger nevertheless re-stamped
// the row's parent chain, carriers and completion siblings. The flag freezes
// the stamp, so every re-stamp after the first left rev unchanged and fired
// the update trigger's own walk: moving one 64-row chunk of a launch's
// children made 629 row updates besides its 64 inserts. The migration
// reinstalls the insert trigger so that under the flag it stamps only the
// inserted row.
//
// Repointing an item at another payload left the old payload row behind,
// because the payload GC triggers fired only on DELETE. The update triggers
// apply the delete triggers' rule to the payload an update replaces: it goes
// when no row of the thread references it.
//
// deferred_migration_failures records what the last run of a pending
// deferred phase left unfinished (DeferredMigration).
//
// The deferred phase folds the sealed chunks back into their threads' rows
// and prunes the payload rows the leak left (repairStoredHistory), empties
// the legacy transcript copies (blankLegacyTranscriptCopies), and converts a
// database created before incremental auto-vacuum
// (convertToIncrementalVacuumStep).
var historyRepairV119SQL = `DROP TRIGGER trg_items_rev_insert;
` + historyRevInsertTriggerV119SQL + `
` + payloadReplacementGCTriggersV119SQL + `
` + deferredMigrationFailuresV119SQL

const deferredMigrationFailuresV119SQL = `CREATE TABLE deferred_migration_failures (
    version     INTEGER PRIMARY KEY,
    failures    INTEGER NOT NULL CHECK (failures > 0),
    first_error TEXT    NOT NULL,
    failed_at   INTEGER NOT NULL
);
`

const payloadReplacementGCTriggersV119SQL = `CREATE TRIGGER trg_items_gc_replaced_payload
AFTER UPDATE OF payload_id ON items
WHEN OLD.payload_id IS NOT NULL AND OLD.payload_id IS NOT NEW.payload_id
BEGIN
    DELETE FROM payloads
     WHERE thread_id = OLD.thread_id
       AND id = OLD.payload_id
       AND NOT EXISTS (
           SELECT 1 FROM items
            WHERE thread_id = OLD.thread_id
              AND payload_id = OLD.payload_id
       )
       AND NOT EXISTS (
           SELECT 1 FROM items
            WHERE thread_id = OLD.thread_id
              AND input_payload_id = OLD.payload_id
       );
END;

CREATE TRIGGER trg_items_gc_replaced_input_payload
AFTER UPDATE OF input_payload_id ON items
WHEN OLD.input_payload_id IS NOT NULL AND OLD.input_payload_id IS NOT NEW.input_payload_id
BEGIN
    DELETE FROM payloads
     WHERE thread_id = OLD.thread_id
       AND id = OLD.input_payload_id
       AND NOT EXISTS (
           SELECT 1 FROM items
            WHERE thread_id = OLD.thread_id
              AND payload_id = OLD.input_payload_id
       )
       AND NOT EXISTS (
           SELECT 1 FROM items
            WHERE thread_id = OLD.thread_id
              AND input_payload_id = OLD.input_payload_id
       );
END;
`
