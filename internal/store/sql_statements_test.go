package store

import (
	"context"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// TestSQLStatementsSplitsWhereSQLiteEndsAStatement: a semicolon ends a
// statement except inside a string, a quoted identifier, a comment or a
// trigger body, as sqlite3_complete decides.
func TestSQLStatementsSplitsWhereSQLiteEndsAStatement(t *testing.T) {
	for _, c := range []struct {
		name, script string
		want         []string
	}{
		{"plain", "CREATE TABLE a(x);\nINSERT INTO a VALUES(1);\n",
			[]string{"CREATE TABLE a(x);", "\nINSERT INTO a VALUES(1);"}},
		{"strings and quoted identifiers", `SELECT ';', "a;b", [c;d], ` + "`e;f`" + `, 'it''s;';`,
			[]string{`SELECT ';', "a;b", [c;d], ` + "`e;f`" + `, 'it''s;';`}},
		{"comments", "-- a; b\nSELECT 1; /* ; */ SELECT 2;",
			[]string{"-- a; b\nSELECT 1;", " /* ; */ SELECT 2;"}},
		{"trigger body", "CREATE TRIGGER t AFTER INSERT ON a BEGIN\n INSERT INTO b VALUES(1);\n UPDATE b SET x = CASE WHEN 1 THEN 2 END;\nEND;\nSELECT 1;",
			[]string{"CREATE TRIGGER t AFTER INSERT ON a BEGIN\n INSERT INTO b VALUES(1);\n UPDATE b SET x = CASE WHEN 1 THEN 2 END;\nEND;", "\nSELECT 1;"}},
		{"temporary trigger", "create temporary trigger t after delete on a begin delete from b; end; select 1;",
			[]string{"create temporary trigger t after delete on a begin delete from b; end;", " select 1;"}},
		{"empty statements and a trailing comment", ";;SELECT 1;; -- done\n",
			[]string{"SELECT 1;"}},
		{"no final semicolon", "SELECT 1; SELECT 2", []string{"SELECT 1;", " SELECT 2"}},
		{"unterminated string", "SELECT 1; SELECT 'x;", []string{"SELECT 1;", " SELECT 'x;"}},
		{"unterminated comment", "SELECT 1; /* x;", []string{"SELECT 1;"}},
	} {
		if got := sqlStatements(c.script); !slices.Equal(got, c.want) {
			t.Errorf("%s: sqlStatements(%q) = %q, want %q", c.name, c.script, got, c.want)
		}
	}
}

// TestSQLStatementsKeepsEveryRebuildMigration: splitting a rebuild loses
// no text but whitespace and comments between statements, and each
// trigger stays one statement.
func TestSQLStatementsKeepsEveryRebuildMigration(t *testing.T) {
	squash := regexp.MustCompile(`\s+`)
	rebuilds := 0
	for _, m := range migrations {
		if !m.Rebuild {
			continue
		}
		rebuilds++
		stmts := sqlStatements(m.SQL)
		if got, want := squash.ReplaceAllString(strings.Join(stmts, ""), ""), squash.ReplaceAllString(m.SQL, ""); got != want {
			t.Errorf("v%d (%s): the statements do not add up to the script", m.Version, m.Name)
		}
		for _, stmt := range stmts {
			if strings.Contains(strings.ToUpper(stmt), "CREATE TRIGGER") && !strings.HasSuffix(strings.ToUpper(strings.TrimSpace(stmt)), "END;") {
				t.Errorf("v%d (%s): a trigger was split: %q", m.Version, m.Name, stmt)
			}
		}
	}
	if rebuilds == 0 {
		t.Fatal("the chain has no rebuild migrations to check")
	}
}

func TestCreateIndexName(t *testing.T) {
	for _, c := range []struct {
		stmt, want string
		ok         bool
	}{
		{"CREATE INDEX idx_a ON t(x);", "idx_a", true},
		{"\n-- why\ncreate unique index if not exists main.idx_b on t(x)", "main.idx_b", true},
		{`CREATE INDEX "q i" ON t(x)`, `"q i"`, true},
		{"CREATE TABLE t(x)", "", false},
		{"CREATE INDEX IF EXISTS idx ON t(x)", "", false},
		{"INSERT INTO t VALUES('CREATE INDEX x')", "", false},
	} {
		got, ok := createIndexName(c.stmt)
		if got != c.want || ok != c.ok {
			t.Errorf("createIndexName(%q) = %q, %v; want %q, %v", c.stmt, got, ok, c.want, c.ok)
		}
	}
}

// TestRebuildReportsEachIndexBuildAndTheForeignKeyCheckBeforeItRuns: the
// activity for an index build or the foreign key check is reported before
// it runs, so it names a step that is running even when that step fails.
func TestRebuildReportsEachIndexBuildAndTheForeignKeyCheckBeforeItRuns(t *testing.T) {
	for _, c := range []struct {
		name, sql string
		want      []string
		fails     bool
	}{
		{"applied", `CREATE TABLE r(x); CREATE INDEX idx_one ON r(x); CREATE UNIQUE INDEX idx_two ON r(x);`,
			[]string{"building index idx_one", "building index idx_two", "checking foreign keys"}, false},
		{"a failing index build", `CREATE TABLE r(x); INSERT INTO r VALUES(1),(1); CREATE UNIQUE INDEX idx_dup ON r(x); CREATE INDEX idx_after ON r(x);`,
			[]string{"building index idx_dup"}, true},
		{"a failing foreign key check", `CREATE TABLE p(id INTEGER PRIMARY KEY); CREATE TABLE c(pid INTEGER REFERENCES p(id)); INSERT INTO c VALUES(7);`,
			[]string{"checking foreign keys"}, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			db := migrateThrough(t, 0)
			var got []string
			err := applyRebuildMigrationSteps(context.Background(), db, Migration{Version: 1_000_001, Name: "probe", SQL: c.sql, Rebuild: true},
				func(what string) { got = append(got, what) })
			if (err != nil) != c.fails {
				t.Fatalf("rebuild error = %v, want failure %v", err, c.fails)
			}
			if !slices.Equal(got, c.want) {
				t.Fatalf("activities = %q, want %q", got, c.want)
			}
		})
	}
}
