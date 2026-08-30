package main

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// snapshotDatabase copies the store with VACUUM INTO rather than copying
// the file, because the real app may be RUNNING while this happens: a
// file copy of a WAL database is a torn read of the main file plus
// whatever the -wal happened to hold, while VACUUM INTO writes one
// consistent single-file snapshot of the reader's own transaction,
// committed WAL content included.
//
// The source connection is mode=ro. It is NOT query_only, and that is a
// measured divergence rather than an oversight: query_only(1) refuses
// VACUUM INTO outright ("attempt to write a readonly database"), even
// though the statement writes only to the output file. mode=ro is the
// layer that actually enforces read-only against the source and permits
// the copy.
func snapshotDatabase(sourceDB, targetDB string) error {
	db, err := openSourceForSnapshot(sourceDB)
	if err != nil {
		return err
	}
	defer db.Close()
	if _, err := db.Exec("VACUUM INTO '" + strings.ReplaceAll(targetDB, "'", "''") + "'"); err != nil {
		return fmt.Errorf("snapshot %s into %s: %w", sourceDB, targetDB, err)
	}
	return nil
}

func openSourceForSnapshot(path string) (*sql.DB, error) {
	escaped := strings.NewReplacer("%", "%25", "?", "%3F", "#", "%23").Replace(path)
	dsn := "file:" + escaped + "?mode=ro&immutable=0&_pragma=busy_timeout(10000)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open %s read-only: %w", path, err)
	}
	db.SetMaxOpenConns(1)
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("open %s read-only: %w", path, err)
	}
	return db, nil
}

// scrubResult is one scrub statement's receipt, printed so an operator
// sees that the neutralization ran rather than assuming it.
type scrubResult struct {
	What   string `json:"what"`
	Detail string `json:"detail"`
	Rows   int64  `json:"rows"`
}

// scrubStatement names its table so a database predating the table can be
// skipped with a note rather than failing the clone.
type scrubStatement struct {
	what   string
	table  string
	sql    string
	detail func(rows int64) string
}

// scrubStatements is the neutralization, written as SQL because this
// binary links no store code. Each entry is a fact about the schema, so
// each is asserted by a test rather than trusted.
func scrubStatements() []scrubStatement {
	return []scrubStatement{
		{
			what:  "threads",
			table: "threads",
			sql: `UPDATE threads
		         SET session_ref = NULL,
		             pending_fork_session_ref = NULL,
		             pending_fork_resume_at = ''`,
			detail: func(rows int64) string {
				return fmt.Sprintf("%d row(s), session refs cleared", rows)
			},
		},
		{
			what:  "thread_import_state",
			table: "thread_import_state",
			sql: `UPDATE thread_import_state
		         SET source_session_id = '',
		             leaf_uuid = '',
		             source_parent_session_id = ''`,
			detail: func(rows int64) string {
				return fmt.Sprintf("%d row(s), source session ids neutralized", rows)
			},
		},
		{
			what:  "ui_state",
			table: "ui_state",
			sql:   `DELETE FROM ui_state`,
			detail: func(rows int64) string {
				return fmt.Sprintf("%d row(s) deleted", rows)
			},
		},
	}
}

// stashedTrigger is one trigger's identity plus the DDL sqlite_master
// holds for it, so the restore cannot drift from the source schema.
type stashedTrigger struct {
	name string
	sql  string
}

// stashTableTriggers reads and drops every trigger attached to the named
// table, returning the DDL to recreate each one. Offline scrubbing must
// temporarily bypass integrity triggers that reject neutralized values.
func stashTableTriggers(db *sql.DB, table string) ([]stashedTrigger, error) {
	rows, err := db.Query(
		`SELECT name, sql FROM sqlite_master WHERE type = 'trigger' AND tbl_name = ?`, table)
	if err != nil {
		return nil, fmt.Errorf("list triggers on %s: %w", table, err)
	}
	defer rows.Close()
	var stashed []stashedTrigger
	for rows.Next() {
		var t stashedTrigger
		var ddl sql.NullString
		if err := rows.Scan(&t.name, &ddl); err != nil {
			return nil, fmt.Errorf("scan trigger on %s: %w", table, err)
		}
		if !ddl.Valid || strings.TrimSpace(ddl.String) == "" {
			return nil, fmt.Errorf("trigger %s on %s has no DDL in sqlite_master; refusing to drop what cannot be restored", t.name, table)
		}
		t.sql = ddl.String
		stashed = append(stashed, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list triggers on %s: %w", table, err)
	}
	for _, t := range stashed {
		if _, err := db.Exec(`DROP TRIGGER "` + strings.ReplaceAll(t.name, `"`, `""`) + `"`); err != nil {
			return nil, fmt.Errorf("drop trigger %s on %s: %w", t.name, table, err)
		}
	}
	return stashed, nil
}

func restoreTableTriggers(db *sql.DB, table string, stashed []stashedTrigger) error {
	for _, t := range stashed {
		if _, err := db.Exec(t.sql); err != nil {
			return fmt.Errorf("restore trigger %s on %s: %w", t.name, table, err)
		}
	}
	return nil
}

// scrubClonedDatabase runs neutralization against the COPY, opened
// read-write. The source is never opened this way.
func scrubClonedDatabase(e *env, targetDB string) ([]scrubResult, error) {
	escaped := strings.NewReplacer("%", "%25", "?", "%3F", "#", "%23").Replace(targetDB)
	db, err := sql.Open("sqlite", "file:"+escaped+"?_pragma=busy_timeout(10000)")
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", targetDB, err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	if err := db.Ping(); err != nil {
		return nil, fmt.Errorf("open %s: %w", targetDB, err)
	}

	results := make([]scrubResult, 0, len(scrubStatements()))
	for _, statement := range scrubStatements() {
		present, err := tableExists(db, statement.table)
		if err != nil {
			return nil, err
		}
		if !present {
			fmt.Fprintf(e.stderr, "ao-harness: %s has no %s table; nothing to scrub there\n", targetDB, statement.table)
			results = append(results, scrubResult{What: statement.what, Detail: "absent in this database"})
			continue
		}
		triggers, err := stashTableTriggers(db, statement.table)
		if err != nil {
			return nil, fmt.Errorf("scrub %s in %s: %w", statement.table, targetDB, err)
		}
		result, err := db.Exec(statement.sql)
		if err != nil {
			return nil, fmt.Errorf("scrub %s in %s: %w", statement.table, targetDB, err)
		}
		if err := restoreTableTriggers(db, statement.table, triggers); err != nil {
			return nil, fmt.Errorf("scrub %s in %s: %w", statement.table, targetDB, err)
		}
		rows, err := result.RowsAffected()
		if err != nil {
			return nil, fmt.Errorf("scrub %s in %s: %w", statement.table, targetDB, err)
		}
		results = append(results, scrubResult{What: statement.what, Detail: statement.detail(rows), Rows: rows})
	}
	return results, nil
}

func readSchemaVersion(targetDB string) (int64, bool) {
	escaped := strings.NewReplacer("%", "%25", "?", "%3F", "#", "%23").Replace(targetDB)
	db, err := sql.Open("sqlite", "file:"+escaped+"?mode=ro")
	if err != nil {
		return 0, false
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	var version sql.NullInt64
	if err := db.QueryRow(`SELECT MAX(version) FROM migration_versions`).Scan(&version); err != nil || !version.Valid {
		return 0, false
	}
	return version.Int64, true
}

func tableExists(db *sql.DB, name string) (bool, error) {
	var found string
	err := db.QueryRow(
		`SELECT name FROM sqlite_master WHERE type = 'table' AND name = ?`, name,
	).Scan(&found)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("look for table %s: %w", name, err)
	}
	return true, nil
}
