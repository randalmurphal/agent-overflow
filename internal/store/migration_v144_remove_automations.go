package store

import (
	"database/sql"
	"fmt"
	"log"
)

// Migration v144 removes the workflow scheduler's schema
// (docs/specs/standing-agents.md, ruling 13: the wake supervisor is the
// app's only clock).
//
//   - automation_cursors and automations are dropped, with the partial
//     index idx_work_items_automation_source_ref that only the scheduler's
//     overlap check read.
//   - A run an automation started becomes a manual run, and its source_ref,
//     which named the dropped automation, is cleared. The fixup logs how many
//     runs it mapped. v145 then removes 'automation' from the source CHECK.
const removeAutomationsMigrationVersion = 144

const removeAutomationsV144SQL = `DROP INDEX idx_work_items_automation_source_ref;

DROP TABLE automation_cursors;

DROP TABLE automations;
`

// mapAutomationRunsToManual rewrites every automation-started run as a
// manual run with no source reference.
func mapAutomationRunsToManual(tx *sql.Tx) error {
	result, err := tx.Exec(`UPDATE work_items SET source = 'manual', source_ref = '' WHERE source = 'automation'`)
	if err != nil {
		return fmt.Errorf("map automation runs to manual: %w", err)
	}
	mapped, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("map automation runs to manual: rows affected: %w", err)
	}
	log.Printf("store: migration v%d mapped %d automation-started run(s) to manual", removeAutomationsMigrationVersion, mapped)
	return nil
}
