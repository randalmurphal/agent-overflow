package store

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// historyRepairFixtureEnv names the database file TestHistoryRepairHarnessFixture
// writes. e2e/tests/history-repair-upgrade.spec.ts sets it when it runs
// bin/ao-store-test, the test binary `make harness-build` compiles from this
// package, and then boots the harness on the file.
const historyRepairFixtureEnv = "AO_TEST_HISTORY_REPAIR_FIXTURE"

// The fixture's rows, named here so the spec and TestHistoryRepairFixtureRepairs
// assert the same outcome.
const (
	fixtureSealedThread     = "fixture-sealed"
	fixtureSealedRows       = 30
	fixtureTranscriptThread = "fixture-transcript"
)

// TestHistoryRepairHarnessFixture writes an upgrade's database for the
// harness: v118's schema, three sealed chunks, an empty payload inside one
// of them, a payload nothing references, and a background agent's legacy
// transcript copy beside a Monitor output that keeps its data. It runs only
// when historyRepairFixtureEnv names an absolute path whose directory exists
// and which does not exist yet.
func TestHistoryRepairHarnessFixture(t *testing.T) {
	writeHistoryRepairFixture(t, harnessFixturePath(t, historyRepairFixtureEnv))
}

// schemaAheadFixtureEnv names the database file TestSchemaAheadHarnessFixture
// writes for e2e/tests/store-schema-ahead.spec.ts.
const schemaAheadFixtureEnv = "AO_TEST_SCHEMA_AHEAD_FIXTURE"

// TestSchemaAheadHarnessFixture writes a database a build two migrations
// newer than this one left behind. It runs only when schemaAheadFixtureEnv
// names the file to write.
func TestSchemaAheadHarnessFixture(t *testing.T) {
	stampAhead(t, harnessFixturePath(t, schemaAheadFixtureEnv), latestMigrationVersionForTest()+2, 0)
}

// harnessFixturePath returns the database path env names for a harness
// fixture, and skips the test when it is unset. The path must be absolute,
// its directory must exist, and nothing may be there yet.
func harnessFixturePath(t *testing.T, env string) string {
	t.Helper()
	path := os.Getenv(env)
	if path == "" {
		t.Skip(env + " is not set")
	}
	if !filepath.IsAbs(path) {
		t.Fatalf("%s=%q is not an absolute path", env, path)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("%s: refusing to write over an existing path (%v)", path, err)
	}
	return path
}

// TestHistoryRepairFixtureRepairs opens the fixture the way an upgraded
// build does and runs the deferred phase, so the fixture the harness boots
// is known to exercise every step.
func TestHistoryRepairFixtureRepairs(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent-overflow.db")
	writeHistoryRepairFixture(t, path)

	s, err := New(path)
	if err != nil {
		t.Fatalf("open the v118 fixture: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if !deferredPending(t, s) || deferredWatermarkOf(t, s) != 118 {
		t.Fatalf("fixture watermark = %d, want 118 and pending", deferredWatermarkOf(t, s))
	}
	if n := countRows(t, s, `SELECT count(*) FROM import_history_chunks WHERE id LIKE 'sealed:%'`); n != 3 {
		t.Fatalf("fixture has %d sealed chunks, want 3", n)
	}
	if err := s.RunDeferredMigrations(context.Background(), DeferredHost{}); err != nil {
		t.Fatal(err)
	}
	if deferredPending(t, s) || deferredWatermarkOf(t, s) != 119 {
		t.Fatalf("watermark after the phase = %d", deferredWatermarkOf(t, s))
	}
	if failure := deferredFailureOf(t, s); failure != nil {
		t.Fatalf("the phase recorded failures: %+v", failure)
	}
	requireNoImportedHistory(t, s)
	if n := countRows(t, s, `SELECT count(*) FROM items WHERE thread_id = ?`, fixtureSealedThread); n != fixtureSealedRows+1 {
		t.Fatalf("%d folded rows, want %d", n, fixtureSealedRows+1)
	}
	requireEmptyBlob(t, s, "payloads", "thread_id = ? AND id = ?", fixtureSealedThread, "p-empty")
	if n := countRows(t, s, `SELECT count(*) FROM payloads WHERE thread_id = ? AND id = 'orphan'`, fixtureSealedThread); n != 0 {
		t.Fatal("the orphan payload survived")
	}
	requireEmptyBlob(t, s, "payloads", "thread_id = ? AND id = ?", fixtureTranscriptThread, "p-agent")
	if got := storedPayload(t, s, fixtureTranscriptThread, "p-monitor"); got.Data != "monitor output" {
		t.Fatalf("monitor payload = %+v, want its data kept", got)
	}
}

func writeHistoryRepairFixture(t *testing.T, path string) {
	t.Helper()
	s, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateProject(Project{
		ID: defaultTestProjectID, Path: filepath.Dir(path), Name: "History repair fixture",
		CreatedAt: 1, UpdatedAt: 1,
	}); err != nil {
		t.Fatal(err)
	}

	ids := localHistoryFixture(t, s, fixtureSealedThread, fixtureSealedRows)
	empty := emptyPayloadItem(fixtureSealedThread, "empty", 10)
	empty.TurnIndex = fixtureSealedRows/10 - 1
	if _, err := s.UpsertItem(empty, &Payload{ID: "p-empty", Kind: "tool_call_result", Meta: "{}", CreatedAt: 1}); err != nil {
		t.Fatal(err)
	}
	for turn := 0; turn < fixtureSealedRows/10; turn++ {
		chunk := slices.Clone(ids[turn*10 : turn*10+10])
		if turn == empty.TurnIndex {
			chunk = append(chunk, empty.ID)
		}
		sealItemsForTest(t, s, fixtureSealedThread, chunk...)
	}
	mustExec(t, s.db, `INSERT INTO payloads(thread_id,id,kind,meta,data,created_at) VALUES(?,'orphan','text','{}',CAST('leaked' AS BLOB),1)`, fixtureSealedThread)

	if err := s.CreateThread(makeThread(fixtureTranscriptThread, "claude")); err != nil {
		t.Fatal(err)
	}
	writeTranscriptCase(t, s, fixtureTranscriptThread, 0, transcriptCase{id: "agent", tool: "Agent", background: true,
		kind: "tool_call_result", meta: loadedTranscriptMeta, data: "legacy transcript copy"})
	writeTranscriptCase(t, s, fixtureTranscriptThread, 1, transcriptCase{id: "monitor", tool: "Monitor", background: true,
		kind: "tool_call_result", meta: loadedTranscriptMeta, data: "monitor output"})
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	db, err := sql.Open("sqlite", poolDSN(path, writerConnPragmas))
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	defer db.Close()
	downgradeSchema(t, db, migrateThrough(t, 118), 118)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
}

type schemaObject struct {
	Type, Name, Table, SQL string
}

func schemaObjects(t *testing.T, db *sql.DB) []schemaObject {
	t.Helper()
	rows, err := db.Query(`SELECT type, name, tbl_name, COALESCE(sql, '') FROM sqlite_master
 WHERE name NOT LIKE 'sqlite_%' ORDER BY type, name`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var objects []schemaObject
	for rows.Next() {
		var o schemaObject
		if err := rows.Scan(&o.Type, &o.Name, &o.Table, &o.SQL); err != nil {
			t.Fatal(err)
		}
		objects = append(objects, o)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return objects
}

// downgradeSchema gives db the schema of ref, a database the chain migrated
// through version, and the migration record and deferred watermark of that
// version. Rows stay as they are. Only indexes, triggers and views may
// differ in shape, and a table only by columns a later migration added: they
// are dropped with their values. Any other table difference is a failure,
// and a table ref lacks is dropped. It fails unless the two schemas end up
// identical.
func downgradeSchema(t *testing.T, db, ref *sql.DB, version int) {
	t.Helper()
	want := map[string]schemaObject{}
	for _, o := range schemaObjects(t, ref) {
		want[o.Type+" "+o.Name] = o
	}
	have := map[string]schemaObject{}
	for _, o := range schemaObjects(t, db) {
		have[o.Type+" "+o.Name] = o
	}
	drop := func(o schemaObject) {
		mustExec(t, db, fmt.Sprintf(`DROP %s "%s"`, o.Type, o.Name))
	}
	// Dependents first on the way down, tables first on the way up.
	down := []string{"trigger", "view", "index", "table"}
	for _, kind := range down {
		for key, o := range have {
			if o.Type != kind {
				continue
			}
			w, kept := want[key]
			switch {
			case !kept:
				drop(o)
			case w.SQL != o.SQL && kind == "table":
				if got := dropAddedColumns(t, db, ref, o.Name); got != w.SQL {
					t.Fatalf("table %s differs from v%d's; the fixture cannot be downgraded:\n%s\nwant:\n%s", o.Name, version, got, w.SQL)
				}
			case w.SQL != o.SQL:
				drop(o)
				delete(have, key)
			}
		}
	}
	for i := len(down) - 1; i >= 0; i-- {
		for key, w := range want {
			if _, present := have[key]; w.Type != down[i] || present || w.SQL == "" {
				continue
			}
			mustExec(t, db, w.SQL)
		}
	}
	mustExec(t, db, `DELETE FROM migration_versions WHERE version > ?`, version)
	mustExec(t, db, fmt.Sprintf(`PRAGMA user_version = %d`, version))

	if got, wantObjects := schemaObjects(t, db), schemaObjects(t, ref); !slices.Equal(got, wantObjects) {
		t.Fatalf("downgraded schema differs from v%d's:\n got %v\nwant %v", version, got, wantObjects)
	}
	var top int
	if err := db.QueryRow(`SELECT MAX(version) FROM migration_versions`).Scan(&top); err != nil || top != version {
		t.Fatalf("migration record tops out at %d (%v), want %d", top, err, version)
	}
	var check string
	if err := db.QueryRow(`PRAGMA integrity_check`).Scan(&check); err != nil || check != "ok" {
		t.Fatalf("integrity_check = %q, %v", check, err)
	}
	if n := countRowsDB(t, db, `SELECT count(*) FROM pragma_foreign_key_check`); n != 0 {
		t.Fatalf("%d foreign key violations after the downgrade", n)
	}
}

// dropAddedColumns drops the columns of table that ref's table lacks, when
// ref's columns are the leading ones, and returns the table's SQL after.
func dropAddedColumns(t *testing.T, db, ref *sql.DB, table string) string {
	t.Helper()
	columns := func(db *sql.DB) []string {
		rows, err := db.Query(`SELECT name FROM pragma_table_info(?) ORDER BY cid`, table)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var names []string
		for rows.Next() {
			var name string
			if err := rows.Scan(&name); err != nil {
				t.Fatal(err)
			}
			names = append(names, name)
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		return names
	}
	have, want := columns(db), columns(ref)
	if len(have) > len(want) && slices.Equal(have[:len(want)], want) {
		for _, name := range have[len(want):] {
			mustExec(t, db, fmt.Sprintf(`ALTER TABLE "%s" DROP COLUMN "%s"`, table, name))
		}
	}
	var sqlText string
	if err := db.QueryRow(`SELECT sql FROM sqlite_master WHERE type = 'table' AND name = ?`, table).Scan(&sqlText); err != nil {
		t.Fatal(err)
	}
	return sqlText
}

func countRowsDB(t *testing.T, db *sql.DB, query string, args ...any) int {
	t.Helper()
	var n int
	if err := db.QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	return n
}
