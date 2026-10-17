package store

import (
	"database/sql"
	"fmt"
)

// A personal-assistant build migrated its own migrations under the version
// main later gave one of its own, and a sync moved them up one. A database
// that build migrated records them under the old numbers, so the first
// migration this build runs on it is one the database already holds. Its
// Check refuses it by name with the repair's section in
// docs/architecture/development.md, rather than with SQLite's
// duplicate-column error.
const (
	// A database from before main's v142: personal-assistant's chain
	// through receipt_own_turn, recorded two below its current numbers, no
	// thread_pull_requests. The first migration it lacks is v152.
	preSyncBeforeV142 = "docs/architecture/development.md#before-mains-v142"
	// A database from before main's v143: personal-assistant's chain
	// through agent_tree_marks, recorded one below, no
	// codex_developer_instructions. The first migration it lacks is v158;
	// one that took only the v142 repair lacks v153.
	preSyncBeforeV143 = "docs/architecture/development.md#before-mains-v143"
)

// refusePreSyncColumn is the Check of a migration that adds table.column:
// a database that already has the column was migrated by a pre-sync
// personal-assistant build, and repair names the section that repairs it.
func refusePreSyncColumn(table, column, repair string) func(*sql.Tx) error {
	return func(tx *sql.Tx) error {
		columns, err := tableColumns(tx, table)
		if err != nil {
			return err
		}
		if columns[column] {
			return fmt.Errorf("%s.%s already exists: this database was migrated by a pre-sync personal-assistant build; see %s", table, column, repair)
		}
		return nil
	}
}
