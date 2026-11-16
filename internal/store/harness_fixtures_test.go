package store

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"agent-overflow/internal/appdirs"
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
	// fixtureStaleAgentRow is a row the fixture's finished background agent
	// left running, for v124's phase to settle.
	fixtureStaleAgentRow     = "agent-read"
	fixtureStaleAgentSummary = "Read: README.md"
)

// TestHistoryRepairHarnessFixture writes an upgrade's database for the
// harness: v118's schema, three sealed chunks, an empty payload inside one
// of them, a payload nothing references, a background agent's legacy
// transcript copy beside a Monitor output that keeps its data, and a row
// that finished agent left running. It runs only when
// historyRepairFixtureEnv names an absolute path whose directory exists and
// which does not exist yet.
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

// migrateDatabaseEnv names an existing database TestMigrateHarnessDatabase
// opens and closes. The release upgrade check
// (e2e/tests/upgrade-corpus-helpers.ts) migrates a copy of the launcher's
// snapshot with it, so what the migrations did to old rows can be told
// from what the candidate's boot did after them.
const migrateDatabaseEnv = "AO_TEST_MIGRATE_DATABASE"

// migrateThroughEnv, when set, stops the migration at that version, so the
// release upgrade check's recovery spec can build a store at an older
// schema than this build's (e2e/tests/upgrade-corpus-recovery.spec.ts).
const migrateThroughEnv = "AO_TEST_MIGRATE_THROUGH"

// TestMigrateHarnessDatabase opens the database migrateDatabaseEnv names
// with New, which runs the migration chain and writes nothing else, and
// closes it; with migrateThroughEnv it applies the chain only through that
// version. It never creates a database and refuses the app's own.
func TestMigrateHarnessDatabase(t *testing.T) {
	path := os.Getenv(migrateDatabaseEnv)
	if path == "" {
		t.Skip(migrateDatabaseEnv + " is not set")
	}
	through := 0
	if raw := os.Getenv(migrateThroughEnv); raw != "" {
		var err error
		if through, err = strconv.Atoi(raw); err != nil || through < 1 {
			t.Fatalf("%s=%q is not a migration version", migrateThroughEnv, raw)
		}
	}
	if err := migrateHarnessDatabase(path, through); err != nil {
		t.Fatal(err)
	}
}

// migrateHarnessDatabase migrates the existing database at path, which
// must be absolute and outside the app's own data directory: through the
// whole chain when through is 0, otherwise through that version.
func migrateHarnessDatabase(path string, through int) error {
	if !filepath.IsAbs(path) {
		return fmt.Errorf("%q is not an absolute path", path)
	}
	if info, err := os.Stat(path); err != nil || !info.Mode().IsRegular() {
		return fmt.Errorf("%s: not an existing database file (%v)", path, err)
	}
	if real, err := appdirs.Root(); err == nil && (path == real || strings.HasPrefix(path, real+string(filepath.Separator))) {
		return fmt.Errorf("%s is the app's own database", path)
	}
	if through > 0 {
		return migrateHarnessDatabaseThrough(path, through)
	}
	s, err := New(path)
	if err != nil {
		return fmt.Errorf("migrate %s: %w", path, err)
	}
	if err := s.Close(); err != nil {
		return fmt.Errorf("close %s: %w", path, err)
	}
	return nil
}

// migrateHarnessDatabaseThrough applies the chain to the database at path
// from its version through version through, as migrateThrough does for an
// in-memory pre-image.
func migrateHarnessDatabaseThrough(path string, through int) (err error) {
	if latest := latestMigrationVersionForTest(); through > latest {
		return fmt.Errorf("v%d is past this build's latest migration v%d", through, latest)
	}
	db, err := sql.Open("sqlite", poolDSN(path, writerConnPragmas))
	if err != nil {
		return fmt.Errorf("open %s: %w", path, err)
	}
	defer func() {
		if closeErr := db.Close(); closeErr != nil && err == nil {
			err = fmt.Errorf("close %s: %w", path, closeErr)
		}
	}()
	db.SetMaxOpenConns(1)
	if err := configureDatabase(db, writerConnPragmas); err != nil {
		return fmt.Errorf("configure %s: %w", path, err)
	}
	if err := ensureMigrationTable(db); err != nil {
		return fmt.Errorf("migration table of %s: %w", path, err)
	}
	var current int
	if err := db.QueryRow(`SELECT COALESCE(MAX(version), 0) FROM migration_versions`).Scan(&current); err != nil {
		return fmt.Errorf("read the version of %s: %w", path, err)
	}
	if current > through {
		return fmt.Errorf("%s is at v%d, past v%d", path, current, through)
	}
	for _, m := range migrations {
		if m.Version <= current || m.Version > through {
			continue
		}
		apply := applyMigration
		if m.Rebuild {
			apply = applyRebuildMigration
		}
		if err := apply(db, m); err != nil {
			return fmt.Errorf("apply migration v%d to %s: %w", m.Version, path, err)
		}
	}
	return nil
}

// TestMigrateHarnessDatabaseRefusals pins what the migration fixture will
// not open: a relative path, a missing file (it never creates one), and
// the app's own database. A database it opens is at the latest version.
func TestMigrateHarnessDatabaseRefusals(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv("HOME", dir)
	t.Setenv("APPDATA", dir)
	real, err := appdirs.Root()
	if err != nil {
		t.Fatal(err)
	}
	own := filepath.Join(real, "agent-overflow.db")
	if err := os.MkdirAll(real, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(own, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, through := range []int{0, 2} {
		for _, path := range []string{"agent-overflow.db", filepath.Join(dir, "missing.db"), own} {
			if err := migrateHarnessDatabase(path, through); err == nil {
				t.Errorf("migrateHarnessDatabase(%s, %d) succeeded, want a refusal", path, through)
			}
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "missing.db")); !os.IsNotExist(err) {
		t.Fatalf("the refused missing database was created (%v)", err)
	}
	copy := filepath.Join(dir, "copy.db")
	if err := os.WriteFile(copy, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	version := func() int {
		t.Helper()
		db, err := sql.Open("sqlite", poolDSN(copy, writerConnPragmas))
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = db.Close() }()
		var v int
		if err := db.QueryRow(`SELECT MAX(version) FROM migration_versions`).Scan(&v); err != nil {
			t.Fatal(err)
		}
		return v
	}
	latest := latestMigrationVersionForTest()
	if err := migrateHarnessDatabase(copy, latest+1); err == nil {
		t.Fatalf("migrating through v%d, past the chain, succeeded", latest+1)
	}
	older := latest - 3
	if err := migrateHarnessDatabase(copy, older); err != nil {
		t.Fatal(err)
	}
	if got := version(); got != older {
		t.Fatalf("migrated through v%d to v%d", older, got)
	}
	if err := migrateHarnessDatabase(copy, older-1); err == nil {
		t.Fatalf("migrating a v%d database through v%d succeeded, want a refusal", older, older-1)
	}
	if err := migrateHarnessDatabase(copy, 0); err != nil {
		t.Fatal(err)
	}
	if got := version(); got != latest {
		t.Fatalf("migrated to v%d, want v%d", got, latest)
	}
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
	path := fixtureCopy(t, "history-repair.db", writeHistoryRepairFixture)

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
	if err := s.RunDeferredMigrations(context.Background(), DeferredHost{AgentEndRule: agentEndRuleForTest}); err != nil {
		t.Fatal(err)
	}
	if deferredPending(t, s) || deferredWatermarkOf(t, s) != latestDeferredVersion {
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
	if got, want := rowStates(t, s, fixtureTranscriptThread)[fixtureStaleAgentRow], "errored:"+unresolvedForTest(fixtureStaleAgentSummary); got != want {
		t.Fatalf("the finished agent's stale row = %q, want %q", got, want)
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
	// A row the finished agent left running, as builds before v124 did.
	if _, err := upsertCarded(s, Item{
		ID: fixtureStaleAgentRow, ThreadID: fixtureTranscriptThread, TurnIndex: 1, Kind: "tool_call", Role: "assistant",
		Status: "running", ToolName: "Read", Summary: fixtureStaleAgentSummary, ParentID: "agent-launch",
		Meta: "{}", CreatedAt: 3, UpdatedAt: 3,
	}, nil); err != nil {
		t.Fatal(err)
	}
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
// version. Rows stay as they are. Indexes, triggers and views may differ in
// shape. A table may differ by trailing columns a later migration added,
// which are dropped with their values, by columns a later migration
// dropped, and in its declaration (a rebuild migration's constraints): it
// is rebuilt to ref's declaration with its rows, which must satisfy it, and
// a dropped column comes back with its declared default (rebuildTableAs).
// A table whose remaining columns are not in ref's order is a failure, and
// a table ref lacks is dropped. It fails unless the two schemas end up
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
					columns := keptColumns(orderedColumnsForTest(t, db, o.Name), orderedColumnsForTest(t, ref, o.Name))
					if !isSubsequence(columns, orderedColumnsForTest(t, ref, o.Name)) {
						t.Fatalf("table %s differs from v%d's; the fixture cannot be downgraded:\n%s\nwant:\n%s", o.Name, version, got, w.SQL)
					}
					rebuildTableAs(t, db, o.Name, w.SQL, columns)
					// The rebuild dropped the table's indexes and triggers;
					// the way up recreates ref's.
					for other, dependent := range have {
						if dependent.Table == o.Name && dependent.Type != "table" {
							delete(have, other)
						}
					}
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

// orderedColumnsForTest lists table's columns in order.
func orderedColumnsForTest(t *testing.T, db *sql.DB, table string) []string {
	t.Helper()
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

// rebuildTableAs replaces table with one declared by create, ref's exact
// statement, holding the same rows' values of columns. db is one
// connection. The old table is
// renamed away under legacy_alter_table, so no reference to the name in
// another table, view or trigger is rewritten, and dropped with its
// indexes and triggers once its rows are copied; foreign keys are off
// meanwhile, as in a rebuild migration.
func rebuildTableAs(t *testing.T, db *sql.DB, table, create string, kept []string) {
	t.Helper()
	columns := `"` + strings.Join(kept, `", "`) + `"`
	old := table + "_downgraded"
	mustExec(t, db, `PRAGMA foreign_keys = OFF`)
	mustExec(t, db, `PRAGMA legacy_alter_table = ON`)
	mustExec(t, db, fmt.Sprintf(`ALTER TABLE "%s" RENAME TO "%s"`, table, old))
	mustExec(t, db, create)
	mustExec(t, db, fmt.Sprintf(`INSERT INTO "%s" (%s) SELECT %s FROM "%s"`, table, columns, columns, old))
	mustExec(t, db, fmt.Sprintf(`DROP TABLE "%s"`, old))
	mustExec(t, db, `PRAGMA legacy_alter_table = OFF`)
	mustExec(t, db, `PRAGMA foreign_keys = ON`)
}

// keptColumns is have without the columns that trail every column of
// want's and want lacks, as a later ADD COLUMN, or a rebuild that appends
// a column, leaves them.
func keptColumns(have, want []string) []string {
	kept := len(have)
	for kept > 0 && !slices.Contains(want, have[kept-1]) {
		kept--
	}
	return have[:kept]
}

// dropAddedColumns drops the columns of table that ref's table lacks, when
// they trail every column ref's has, and returns the table's SQL after. A
// column a constraint of the table names cannot be dropped in place; it
// stays for the caller's rebuild, which copies only keptColumns.
func dropAddedColumns(t *testing.T, db, ref *sql.DB, table string) string {
	t.Helper()
	have, want := orderedColumnsForTest(t, db, table), orderedColumnsForTest(t, ref, table)
	kept := keptColumns(have, want)
	if !slices.ContainsFunc(kept, func(name string) bool { return !slices.Contains(want, name) }) {
		for _, name := range have[len(kept):] {
			if _, err := db.Exec(fmt.Sprintf(`ALTER TABLE "%s" DROP COLUMN "%s"`, table, name)); err != nil {
				break
			}
		}
	}
	var sqlText string
	if err := db.QueryRow(`SELECT sql FROM sqlite_master WHERE type = 'table' AND name = ?`, table).Scan(&sqlText); err != nil {
		t.Fatal(err)
	}
	return sqlText
}

func TestDowngradeRestoresADroppedColumnButRefusesARenamedOne(t *testing.T) {
	open := func(create string) *sql.DB {
		db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "downgrade.db"))
		if err != nil {
			t.Fatal(err)
		}
		db.SetMaxOpenConns(1)
		t.Cleanup(func() { _ = db.Close() })
		mustExec(t, db, create)
		return db
	}
	const declared = `CREATE TABLE x (a TEXT, b TEXT NOT NULL DEFAULT 'kept', c TEXT)`
	ref := open(declared)

	// b was dropped and d added since ref's version.
	dropped := open(`CREATE TABLE x (a TEXT, c TEXT, d TEXT)`)
	mustExec(t, dropped, `INSERT INTO x (a, c, d) VALUES ('1', '3', '4')`)
	dropAddedColumns(t, dropped, ref, "x")
	if got := orderedColumnsForTest(t, dropped, "x"); !isSubsequence(got, orderedColumnsForTest(t, ref, "x")) {
		t.Fatalf("columns after dropping the added one = %v", got)
	}
	rebuildTableAs(t, dropped, "x", declared, orderedColumnsForTest(t, dropped, "x"))
	var a, b, c string
	if err := dropped.QueryRow(`SELECT a, b, c FROM x`).Scan(&a, &b, &c); err != nil || a != "1" || b != "kept" || c != "3" {
		t.Fatalf("rebuilt row = %q %q %q, %v", a, b, c, err)
	}

	// b became b2: its values must not be replaced by b's default.
	renamed := open(`CREATE TABLE x (a TEXT, b2 TEXT, c TEXT, d TEXT)`)
	dropAddedColumns(t, renamed, ref, "x")
	got := orderedColumnsForTest(t, renamed, "x")
	if !slices.Equal(got, []string{"a", "b2", "c", "d"}) || isSubsequence(got, orderedColumnsForTest(t, ref, "x")) {
		t.Fatalf("a renamed column was accepted: %v", got)
	}
}

// isSubsequence reports whether sub is seq with zero or more elements removed.
func isSubsequence(sub, seq []string) bool {
	i := 0
	for _, name := range seq {
		if i < len(sub) && sub[i] == name {
			i++
		}
	}
	return i == len(sub)
}

func countRowsDB(t *testing.T, db *sql.DB, query string, args ...any) int {
	t.Helper()
	var n int
	if err := db.QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	return n
}
