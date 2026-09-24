package store

import (
	"fmt"
	"math/rand/v2"
	"slices"
	"strings"
	"testing"

	"modernc.org/libc"
	sqlite3 "modernc.org/sqlite/lib"
)

// sqliteStatementEnds is the reference split: the offset just past each
// semicolon at which SQLite's own sqlite3_complete first reports the text
// since the previous end complete.
func sqliteStatementEnds(t *testing.T, tls *libc.TLS, script string) []int {
	t.Helper()
	complete := func(s string) bool {
		p, err := libc.CString(s)
		if err != nil {
			t.Fatalf("CString: %v", err)
		}
		defer libc.Xfree(tls, p)
		return sqlite3.Xsqlite3_complete(tls, p) != 0
	}
	var ends []int
	start := 0
	for p := 0; p < len(script); p++ {
		if script[p] == ';' && complete(script[start:p+1]) {
			ends = append(ends, p+1)
			start = p + 1
		}
	}
	return ends
}

// adversarialSQL are scripts whose semicolons SQLite does not all treat as
// statement ends.
var adversarialSQL = []string{
	`INSERT INTO t VALUES(';', 'a;b', 'it''s; fine'); SELECT "x;y", [z;w], ` + "`q;r`" + `;`,
	"-- lead; comment\nCREATE TABLE a(x); /* block; comment */ INSERT INTO a VALUES(1); -- trailing; comment\n",
	"CREATE TABLE t(x TEXT DEFAULT 'a;b') -- c;\n;",
	`CREATE TRIGGER t1 AFTER INSERT ON a BEGIN
 INSERT INTO b SELECT CASE WHEN new.x > 0 THEN 'begin;' ELSE 'end;' END;
 UPDATE b SET y = (SELECT CASE x WHEN 1 THEN 2 END FROM a WHERE x = new.x);
 SELECT RAISE(ABORT, 'end; begin;') WHERE new.x IS NULL;
END; SELECT 1;`,
	`CREATE TRIGGER IF NOT EXISTS t2 BEFORE UPDATE OF x ON a FOR EACH ROW
 WHEN new.x IS NOT old.x AND new.x <> ';' AND (SELECT count(*) FROM b WHERE y = 'end;') = 0
BEGIN
 SELECT RAISE(ABORT, 'no;');
 DELETE FROM b WHERE y = old.x;
END;
CREATE INDEX i ON a(x);`,
	"create temp trigger t3 after delete on a begin delete from b; End; create temporary trigger t4 instead of insert on v begin select 1; end;",
	"EXPLAIN CREATE TRIGGER t5 AFTER INSERT ON a BEGIN SELECT 1; END; EXPLAIN SELECT 1;",
	"CREATE TRIGGER trigger_end AFTER DELETE ON end_table BEGIN DELETE FROM createtrigger; END; CREATE TABLE end_date(temp_x);",
	// Each identifier byte class glued to a keyword makes a plain identifier.
	"CREATE TRIGGER_1 AFTER INSERT ON a; EXPLAIN$ CREATE TRIGGER t2; CREATE TRIGGERé x; EXPLAIN9 CREATE TRIGGER y; SELECT 1;",
	"CREATE\vTRIGGER t7 AFTER INSERT ON a; CREATE\rTRIGGER t8 AFTER INSERT ON a BEGIN SELECT 1;\fEND; SELECT 2;",
	`CREATE TABLE "ü;" (é INT); CREATE TABLE tëst(x); SELECT 'ß;';`,
	";; SELECT 1;; ;",
	"CREATE TABLE z(x); SELECT 1",
	"SELECT 1; SELECT 'unterminated;",
	"SELECT 1; /* unterminated; comment",
	"SELECT 1; -- unterminated; line comment",
	"SELECT 1; [unterminated; identifier",
	"CREATE TRIGGER t6 AFTER INSERT ON a BEGIN SELECT 1; SELECT 2",
}

// TestSQLStatementEndsMatchSQLite: the port ends statements exactly where
// SQLite's own sqlite3_complete does, for every migration's SQL, the
// adversarial scripts above, and random scripts built from the tokens the
// state machine distinguishes.
func TestSQLStatementEndsMatchSQLite(t *testing.T) {
	tls := libc.NewTLS()
	defer tls.Close()
	check := func(what, script string) {
		t.Helper()
		if got, want := sqlStatementEnds(script), sqliteStatementEnds(t, tls, script); !slices.Equal(got, want) {
			t.Fatalf("%s: statement ends %v, SQLite ends %v in %q", what, got, want, script)
		}
	}

	for _, m := range migrations {
		if m.SQL != "" {
			check(m.Name, m.SQL)
		}
	}
	for _, script := range adversarialSQL {
		check("adversarial", script)
	}

	fragments := []string{
		";", ";", ";", " ", "\n", "\t", "\r", "\f", "\v", "x", "t1", "_", "9", "(", ")", ",", ".", "=", "*", "$v", "ü",
		"'a;b'", "'it''s'", `"q;"`, "[i;]", "`b;`", "-- c;\n", "/* d; */", "/", "-", "--", "/*", "'", `"`, "[", "`",
		"CREATE", "create", "TEMP", "temporary", "TRIGGER", "trigger", "END", "end", "EXPLAIN", "BEGIN", "begin",
		"CASE", "WHEN", "THEN", "SELECT", "INSERT", "INTO", "ON", "AFTER", "INDEX", "createx", "endx", "tempo", "triggers",
	}
	rng := rand.New(rand.NewPCG(1, 2))
	for n := range 20_000 {
		var b strings.Builder
		for range 1 + rng.IntN(40) {
			f := fragments[rng.IntN(len(fragments))]
			b.WriteString(f)
			if rng.IntN(3) > 0 {
				b.WriteByte(' ')
			}
		}
		check(fmt.Sprintf("random script %d", n), b.String())
	}
}
