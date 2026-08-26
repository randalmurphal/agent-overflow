package main

import (
	"database/sql"
	"encoding/json"
	"errors"
	"os"
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
//
// The pragma rows are the interesting ones, because the verb whitelist
// ADMITS `PRAGMA` — so a caller can reach the two pragmas that aim at the
// guard itself. Both are ACCEPTED by SQLite (`query_only=0` genuinely
// clears the statement-layer flag on this handle), and the write still
// fails: mode=ro is the layer that actually holds, exactly as
// openReadOnly's comment claims. That asymmetry is worth a test of its
// own, because a future change that dropped mode=ro and kept query_only
// would look identically safe until this ran.
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

	// Loosen everything the statement layer offers, then try again.
	for _, pragma := range []string{`PRAGMA query_only=0`, `PRAGMA journal_mode=DELETE`} {
		if _, err := db.Exec(pragma); err != nil {
			t.Logf("%s was refused outright (%v); the write below is the assertion either way", pragma, err)
		}
	}
	if _, err := db.Exec(`INSERT INTO t VALUES ('y')`); err == nil {
		t.Fatal("a write was accepted after query_only was cleared; mode=ro is no longer holding")
	}
	if _, err := db.Exec(`DELETE FROM t`); err == nil {
		t.Fatal("a delete was accepted after query_only was cleared")
	}
}

// `--file` is the one door around instance resolution, and the harness
// boot's own refusal (main_harness.go) never sees it. Without this check
// `ao-harness db --file ~/.config/agent-overflow/agent-overflow.db` reads
// the developer's real threads through a tool whose contract is "test
// instances only".
func TestDBRefusesTheRealAppDataDir(t *testing.T) {
	configRoot, err := os.UserConfigDir()
	if err != nil {
		t.Skip("no OS config dir on this machine; nothing to protect")
	}
	realDir := filepath.Join(configRoot, "agent-overflow")

	e, _, _ := testEnv(t.TempDir())
	for _, file := range []string{
		filepath.Join(realDir, "agent-overflow.db"),
		filepath.Join(realDir, "nested", "agent-overflow.db"),
		realDir,
	} {
		err := runDB(e, []string{"--file", file, "SELECT 1"})
		if err == nil {
			t.Errorf("db --file %s was accepted", file)
			continue
		}
		var usage usageErr
		if !errors.As(err, &usage) {
			t.Errorf("db --file %s = %v, want a usage error", file, err)
		}
		if !strings.Contains(err.Error(), "real app data dir") {
			t.Errorf("db --file %s error does not say why: %v", file, err)
		}
	}

	// A sibling whose name merely starts the same way is not inside it.
	if err := refuseRealAppDatabase(realDir + "-harness/agent-overflow.db"); err != nil {
		t.Errorf("a sibling directory must not be refused: %v", err)
	}
	// And an ordinary scratch path is untouched.
	if err := refuseRealAppDatabase(filepath.Join(t.TempDir(), "agent-overflow.db")); err != nil {
		t.Errorf("a scratch path must not be refused: %v", err)
	}
}

// The refusal must survive the obvious way past a string comparison.
func TestDBRefusesASymlinkAimedAtTheRealAppDataDir(t *testing.T) {
	configRoot, err := os.UserConfigDir()
	if err != nil {
		t.Skip("no OS config dir on this machine; nothing to protect")
	}
	realDir := filepath.Join(configRoot, "agent-overflow")
	if _, err := os.Stat(realDir); err != nil {
		t.Skip("no real app data dir on this machine to link at")
	}
	link := filepath.Join(t.TempDir(), "innocent")
	if err := os.Symlink(realDir, link); err != nil {
		t.Skip("cannot create symlinks here")
	}
	if err := refuseRealAppDatabase(filepath.Join(link, "agent-overflow.db")); err == nil {
		t.Fatal("a symlinked path into the real app data dir was accepted")
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

// A silently cut value is how a reader concludes a column holds something
// it does not, so the default truncation says so once and names both
// recourses.
func TestDBTruncatesWideCellsAndSaysSo(t *testing.T) {
	path := seedWideCellDB(t)

	e, stdout, _ := testEnv(t.TempDir())
	if err := runDB(e, []string{"--file", path, "SELECT body FROM notes"}); err != nil {
		t.Fatal(err)
	}
	out := stdout.String()
	if strings.Contains(out, strings.Repeat("x", defaultDBColWidth+1)) {
		t.Fatalf("cell was not truncated at the default width:\n%s", out)
	}
	if !strings.Contains(out, "--max-col-width 0") || !strings.Contains(out, "-o json") {
		t.Fatalf("truncation note does not name both recourses:\n%s", out)
	}
}

func TestDBMaxColWidthZeroPrintsTheWholeValue(t *testing.T) {
	path := seedWideCellDB(t)

	e, stdout, _ := testEnv(t.TempDir())
	if err := runDB(e, []string{"--file", path, "--max-col-width", "0", "SELECT body FROM notes"}); err != nil {
		t.Fatal(err)
	}
	out := stdout.String()
	if !strings.Contains(out, strings.Repeat("x", 200)) {
		t.Fatalf("--max-col-width 0 still truncated:\n%s", out)
	}
	if strings.Contains(out, "truncated") {
		t.Fatalf("an untruncated table claimed truncation:\n%s", out)
	}
	// A multi-line cell must still be flattened: newlines and tabs are the
	// tabwriter's own delimiters and would shear the table.
	if strings.Count(out, "\n") != 2 {
		t.Fatalf("an embedded newline reached the table:\n%q", out)
	}
}

func TestDBMaxColWidthRefusesANegativeWidth(t *testing.T) {
	e, _, _ := testEnv(t.TempDir())
	err := runDB(e, []string{"--file", seedWideCellDB(t), "--max-col-width", "-1", "SELECT 1"})
	if err == nil {
		t.Fatal("a negative width was accepted")
	}
	if code := exitCodeOf(t, err); code != exitUsage {
		t.Fatalf("exit code = %d, want %d", code, exitUsage)
	}
}

func seedWideCellDB(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "wide.db")
	seed, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	body := strings.Repeat("x", 200) + "\nsecond line"
	if _, err := seed.Exec(`CREATE TABLE notes (body TEXT)`); err != nil {
		t.Fatal(err)
	}
	if _, err := seed.Exec(`INSERT INTO notes VALUES (?)`, body); err != nil {
		t.Fatal(err)
	}
	if err := seed.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}
