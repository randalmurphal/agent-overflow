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
// docs/architecture/development.md, rather than with SQLite's error for
// the object that already exists.
const (
	// A database from before main's v142: personal-assistant's chain
	// through receipt_own_turn recorded two below its current numbers, no
	// thread_pull_requests. Its first missing migration is v152, or v153
	// once it took only the v143 repair.
	preSyncBeforeV142 = "docs/architecture/development.md#before-mains-v142"
	// A database from before main's v143: personal-assistant's chain up to
	// one of receipt_own_turn, late_delivery_route, agent_records,
	// ledger_indexes or agent_tree_marks recorded one below, no
	// codex_developer_instructions. Its first missing migration is v153,
	// v154, v155, v156 or v158.
	preSyncBeforeV143 = "docs/architecture/development.md#before-mains-v143"
)

// refusePreSyncColumn is the Check of a migration that adds table.column.
func refusePreSyncColumn(table, column string) func(*sql.Tx) error {
	return refusePreSync(table+"."+column, func(tx *sql.Tx) (bool, error) {
		columns, err := tableColumns(tx, table)
		return columns[column], err
	})
}

// refusePreSyncObject is the Check of a migration that creates the table
// or index name.
func refusePreSyncObject(kind, name string) func(*sql.Tx) error {
	return refusePreSync(kind+" "+name, func(tx *sql.Tx) (bool, error) {
		return schemaObjectExists(tx, kind, name)
	})
}

// refusePreSync refuses a database that already holds what the migration
// makes: a pre-sync personal-assistant build migrated it. The repair it
// names follows from thread_pull_requests: a database without it starts
// with main's v142 repair.
func refusePreSync(what string, exists func(*sql.Tx) (bool, error)) func(*sql.Tx) error {
	return func(tx *sql.Tx) error {
		held, err := exists(tx)
		if err != nil || !held {
			return err
		}
		pullRequests, err := schemaObjectExists(tx, "table", "thread_pull_requests")
		if err != nil {
			return err
		}
		repair := "see " + preSyncBeforeV143
		if !pullRequests {
			repair = "it has no thread_pull_requests, so see " + preSyncBeforeV142
		}
		return fmt.Errorf("%s already exists: this database was migrated by a pre-sync personal-assistant build; %s", what, repair)
	}
}

func schemaObjectExists(tx *sql.Tx, kind, name string) (bool, error) {
	var n int
	if err := tx.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type = ? AND name = ?`, kind, name).Scan(&n); err != nil {
		return false, fmt.Errorf("read schema for %s %s: %w", kind, name, err)
	}
	return n > 0, nil
}
