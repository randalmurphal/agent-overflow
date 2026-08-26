package main

import (
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

func TestDBGuardAcceptsReads(t *testing.T) {
	cases := []string{
		"SELECT count(*) FROM threads",
		"select 1",
		"  select 1  ",
		"SELECT 1;",
		"PRAGMA table_info(threads)",
		"EXPLAIN QUERY PLAN SELECT 1",
		"-- what is in here\nSELECT 1",
		"/* a comment */ SELECT 1",
		"SELECT ';' AS semicolon",
		"SELECT 'it''s fine; really'",
	}
	for _, statement := range cases {
		if _, err := checkReadOnly(statement); err != nil {
			t.Errorf("checkReadOnly(%q) = %v, want accepted", statement, err)
		}
	}
}

func TestDBGuardRefusesAnythingThatIsNotARead(t *testing.T) {
	cases := []string{
		"DELETE FROM threads",
		"delete from threads",
		"INSERT INTO threads (id) VALUES ('x')",
		"UPDATE threads SET title = 'x'",
		"DROP TABLE threads",
		"VACUUM",
		"ATTACH DATABASE '/tmp/x.db' AS x",
		// WITH is refused because its first keyword says nothing about
		// whether the statement writes.
		"WITH doomed AS (SELECT id FROM threads) DELETE FROM threads",
	}
	for _, statement := range cases {
		if _, err := checkReadOnly(statement); err == nil {
			t.Errorf("checkReadOnly(%q) was accepted, want refused", statement)
		}
	}
}

func TestDBGuardRefusesASecondStatement(t *testing.T) {
	_, err := checkReadOnly("SELECT 1; DELETE FROM threads")
	if err == nil {
		t.Fatal("a piggybacked statement must be refused")
	}
	if !strings.Contains(err.Error(), "exactly one statement") {
		t.Fatalf("error = %v", err)
	}
}

func TestDBGuardStripsOneTrailingSemicolon(t *testing.T) {
	got, err := checkReadOnly("SELECT 1 ;  ")
	if err != nil {
		t.Fatal(err)
	}
	if got != "SELECT 1" {
		t.Fatalf("statement = %q", got)
	}
}

// A semicolon inside a literal is data, not a separator. Getting this
// wrong the other way would refuse legitimate queries.
func TestDBGuardDoesNotSplitOnSemicolonsInsideLiterals(t *testing.T) {
	for _, statement := range []string{
		`SELECT * FROM items WHERE summary = 'a;b'`,
		"SELECT * FROM items WHERE \"weird;column\" = 1",
		"SELECT * FROM items WHERE [weird;column] = 1",
		"SELECT 1 -- trailing; comment",
		"SELECT 1 /* inline; comment */",
	} {
		if _, err := checkReadOnly(statement); err != nil {
			t.Errorf("checkReadOnly(%q) = %v", statement, err)
		}
	}
}

func TestDBGuardRefusesUnterminatedQuoting(t *testing.T) {
	for _, statement := range []string{
		"SELECT 'unterminated",
		"SELECT [unterminated",
		"SELECT 1 /* unterminated",
	} {
		if _, err := checkReadOnly(statement); err == nil {
			t.Errorf("checkReadOnly(%q) was accepted", statement)
		}
	}
}

// The end-to-end leg: a real SQLite file, opened through the same DSN
// production uses, queried through the command.
func TestDBQueriesAFileReadOnly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	seed, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := seed.Exec(`CREATE TABLE threads (id TEXT, title TEXT); INSERT INTO threads VALUES ('a', 'one'), ('b', 'two')`); err != nil {
		t.Fatal(err)
	}
	if err := seed.Close(); err != nil {
		t.Fatal(err)
	}

	e, stdout, _ := testEnv(t.TempDir())
	e.format = "json"
	if err := runDB(e, []string{"--file", path, "SELECT id, title FROM threads ORDER BY id"}); err != nil {
		t.Fatal(err)
	}
	var rows []map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &rows); err != nil {
		t.Fatalf("output was not JSON: %v\n%s", err, stdout.String())
	}
	if len(rows) != 2 || rows[0]["id"] != "a" || rows[1]["title"] != "two" {
		t.Fatalf("rows = %+v", rows)
	}
}

// mode=ro is the second half of the guard: even if the statement check
// were bypassed, the connection cannot write.
func TestDBConnectionCannotWrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	seed, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := seed.Exec(`CREATE TABLE t (id TEXT)`); err != nil {
		t.Fatal(err)
	}
	seed.Close()

	db, err := openReadOnly(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`INSERT INTO t VALUES ('x')`); err == nil {
		t.Fatal("a read-only connection accepted a write")
	}
}

func TestDBNeedsExactlyOneStatementArgument(t *testing.T) {
	e, _, _ := testEnv(t.TempDir())
	for _, args := range [][]string{{}, {"SELECT 1", "SELECT 2"}, {"   "}} {
		err := runDB(e, args)
		if err == nil {
			t.Errorf("runDB(%v) was accepted", args)
			continue
		}
		var usage usageErr
		if !errors.As(err, &usage) {
			t.Errorf("runDB(%v) = %v, want a usage error", args, err)
		}
	}
}
