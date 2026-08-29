package compare

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	_ "modernc.org/sqlite"
)

func snapshotAndScrub(source, target string) (int64, error) {
	if _, err := os.Stat(source); err != nil {
		return 0, fmt.Errorf("open source database %s: %w", source, err)
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		return 0, err
	}
	escaped := strings.NewReplacer("%", "%25", "?", "%3F", "#", "%23").Replace(source)
	db, err := sql.Open("sqlite", "file:"+escaped+"?mode=ro&_pragma=busy_timeout(10000)")
	if err != nil {
		return 0, fmt.Errorf("open source database read-only: %w", err)
	}
	db.SetMaxOpenConns(1)
	defer db.Close()
	if err := db.Ping(); err != nil {
		return 0, fmt.Errorf("open source database read-only: %w", err)
	}
	quoted := strings.ReplaceAll(target, "'", "''")
	if _, err := db.Exec("VACUUM INTO '" + quoted + "'"); err != nil {
		return 0, fmt.Errorf("snapshot source database: %w", err)
	}
	if err := scrubDatabase(target); err != nil {
		return 0, err
	}
	info, err := os.Stat(target)
	if err != nil {
		return 0, err
	}
	return info.Size(), nil
}

func scrubDatabase(path string) error {
	escaped := strings.NewReplacer("%", "%25", "?", "%3F", "#", "%23").Replace(path)
	db, err := sql.Open("sqlite", "file:"+escaped+"?_pragma=busy_timeout(10000)")
	if err != nil {
		return fmt.Errorf("open snapshot for scrub: %w", err)
	}
	db.SetMaxOpenConns(1)
	defer db.Close()
	for _, statement := range []struct{ table, sql string }{
		{"threads", `UPDATE threads SET session_ref = NULL, pending_fork_session_ref = NULL, pending_fork_resume_at = ''`},
		{"thread_import_state", `UPDATE thread_import_state SET source_session_id = '', leaf_uuid = '', source_parent_session_id = ''`},
		{"ui_state", `DELETE FROM ui_state`},
	} {
		present, err := tableExists(db, statement.table)
		if err != nil {
			return err
		}
		if !present {
			continue
		}
		triggers, err := stashTriggers(db, statement.table)
		if err != nil {
			return err
		}
		if _, err := db.Exec(statement.sql); err != nil {
			return fmt.Errorf("scrub %s: %w", statement.table, err)
		}
		if err := restoreTriggers(db, triggers); err != nil {
			return err
		}
	}
	return nil
}

type trigger struct{ name, ddl string }

func tableExists(db *sql.DB, table string) (bool, error) {
	var name string
	err := db.QueryRow(`SELECT name FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&name)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("inspect table %s: %w", table, err)
	}
	return true, nil
}

func stashTriggers(db *sql.DB, table string) ([]trigger, error) {
	rows, err := db.Query(`SELECT name, sql FROM sqlite_master WHERE type='trigger' AND tbl_name=?`, table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []trigger
	for rows.Next() {
		var t trigger
		var ddl sql.NullString
		if err := rows.Scan(&t.name, &ddl); err != nil {
			return nil, err
		}
		if !ddl.Valid || strings.TrimSpace(ddl.String) == "" {
			return nil, fmt.Errorf("trigger %s has no restorable DDL", t.name)
		}
		t.ddl = ddl.String
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, t := range out {
		if _, err := db.Exec(`DROP TRIGGER "` + strings.ReplaceAll(t.name, `"`, `""`) + `"`); err != nil {
			return nil, fmt.Errorf("drop trigger %s: %w", t.name, err)
		}
	}
	return out, nil
}

func restoreTriggers(db *sql.DB, triggers []trigger) error {
	for _, t := range triggers {
		if _, err := db.Exec(t.ddl); err != nil {
			return fmt.Errorf("restore trigger %s: %w", t.name, err)
		}
	}
	return nil
}
