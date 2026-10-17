package store

import (
	"database/sql"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// preSyncDB is a database a personal-assistant build migrated before a
// sync moved its migrations up: this build's chain through base, then the
// migrations in (from, through] recorded shift versions lower.
func preSyncDB(t *testing.T, base, from, through, shift int) string {
	t.Helper()
	path := fileDBThrough(t, base)
	applyShifted(t, path, from, through, shift)
	return path
}

// applyShifted applies this build's migrations in (from, through] to the
// database at path, each recorded shift versions lower.
func applyShifted(t *testing.T, path string, from, through, shift int) {
	t.Helper()
	db := openWriter(t, path)
	for _, m := range migrations {
		if m.Version <= from || m.Version > through {
			continue
		}
		m.Version -= shift
		apply := applyMigration
		if m.Rebuild {
			apply = applyRebuildMigration
		}
		if err := apply(db, m); err != nil {
			t.Fatalf("apply %s as v%d: %v", m.Name, m.Version, err)
		}
	}
	closeDB(t, db)
}

func openWriter(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", poolDSN(path, writerConnPragmas))
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	return db
}

func closeDB(t *testing.T, db *sql.DB) {
	t.Helper()
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
}

// beforeV142DB is what a personal-assistant build from before main's v142
// left: main's chain through v141, then personal-assistant's
// remove_discussions through receipt_own_turn recorded as v142 to v151.
func beforeV142DB(t *testing.T) string {
	return preSyncDB(t, threadPullRequestsMigrationVersion-1, codexDeveloperInstructionsMigrationVersion, receiptOwnTurnMigrationVersion, 2)
}

// beforeV143DB is what a personal-assistant build from before main's v143
// left after running personal-assistant's migrations through the one now
// numbered through, each recorded one lower, over main's chain through v142.
func beforeV143DB(t *testing.T, through int) string {
	return preSyncDB(t, threadPullRequestsMigrationVersion, codexDeveloperInstructionsMigrationVersion, through, 1)
}

const (
	repairBeforeV142 = "#### Before main's v142"
	repairBeforeV143 = "#### Before main's v143"
)

// repairBlock is the SQL block of development.md's section heading, as a
// reader would paste it into sqlite3.
func repairBlock(t *testing.T, heading string) string {
	t.Helper()
	doc, err := os.ReadFile(filepath.Join("..", "..", "docs", "architecture", "development.md"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(doc)
	at := strings.Index(text, "\n"+heading+"\n")
	if at < 0 {
		t.Fatalf("development.md has no %q", heading)
	}
	text = text[at:]
	if next := strings.Index(text[1:], "\n#"); next >= 0 {
		text = text[:next+1]
	}
	open := strings.Index(text, "```sql\n")
	if open < 0 {
		t.Fatalf("%q has no sql block", heading)
	}
	text = text[open+len("```sql\n"):]
	end := strings.Index(text, "```")
	if end < 0 {
		t.Fatalf("%q has an unterminated sql block", heading)
	}
	return text[:end]
}

// repairSQL is the block without its sqlite3 dot commands, which open the
// block: a reader's sqlite3 must stop at the block's first error rather
// than run the statements after it.
func repairSQL(t *testing.T, heading string) string {
	t.Helper()
	block := repairBlock(t, heading)
	if !strings.HasPrefix(block, ".bail on\n") {
		t.Fatalf("%s does not open with .bail on:\n%s", heading, block)
	}
	var statements []string
	for _, line := range strings.Split(block, "\n") {
		if !strings.HasPrefix(line, ".") {
			statements = append(statements, line)
		}
	}
	return strings.Join(statements, "\n")
}

func applyRepair(t *testing.T, path, heading string) {
	t.Helper()
	db := openWriter(t, path)
	mustExec(t, db, repairSQL(t, heading))
	closeDB(t, db)
}

// dumpDB is the database's schema and migration records.
func dumpDB(t *testing.T, path string) string {
	t.Helper()
	return schemaDump(t, path) + "\n--\n" + strings.Join(recordedVersions(t, path), "\n")
}

func schemaDump(t *testing.T, path string) string {
	t.Helper()
	return strings.Join(readRows(t, path, `SELECT type || ' ' || name || ': ' || COALESCE(sql, '') FROM sqlite_master ORDER BY type, name`), "\n")
}

func recordedVersions(t *testing.T, path string) []string {
	t.Helper()
	return readRows(t, path, `SELECT version FROM migration_versions ORDER BY version`)
}

func readRows(t *testing.T, path, query string) []string {
	t.Helper()
	db, err := sql.Open("sqlite", poolDSN(path, readerConnPragmas))
	if err != nil {
		t.Fatal(err)
	}
	rows, err := queryIDs(db, query)
	if closeErr := db.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

// requireRefused opens path and wants the named refusal at version, with
// the database unchanged.
func requireRefused(t *testing.T, path string, version int, name, object, repair string) {
	t.Helper()
	before := dumpDB(t, path)
	_, err := New(path)
	if err == nil {
		t.Fatal("New opened a pre-sync personal-assistant database")
	}
	want := "migration v" + itoa(version) + " (" + name + ") refused: " + object + " already exists: " +
		"this database was migrated by a pre-sync personal-assistant build; " + repair
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("New error = %q, want it to contain %q", err, want)
	}
	if after := dumpDB(t, path); after != before {
		t.Fatalf("the refusal changed the database:\nbefore %s\nafter  %s", before, after)
	}
}

const (
	seeBeforeV143 = "see " + preSyncBeforeV143
	seeBeforeV142 = "it has no thread_pull_requests, so see " + preSyncBeforeV142
)

// requireOpens opens path, which migrates it to this build's last version
// with the schema and one migration record per version that a fresh
// database has.
func requireOpens(t *testing.T, path string) {
	t.Helper()
	s, err := New(path)
	if err != nil {
		t.Fatalf("New after the repair: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	fresh := fileDBThrough(t, latestMigrationVersionForTest())
	if got, want := schemaDump(t, path), schemaDump(t, fresh); got != want {
		t.Errorf("schema after the repair differs from a fresh database:\n got %s\nwant %s", got, want)
	}
	if got, want := recordedVersions(t, path), recordedVersions(t, fresh); !slices.Equal(got, want) {
		t.Errorf("recorded versions after the repair = %v, want %v", got, want)
	}
}

// An unrepaired pre-sync database is refused at the first migration it
// already holds, naming the section that repairs it, and changes nothing.
func TestPreSyncDatabasesAreRefusedByName(t *testing.T) {
	t.Run("before v142", func(t *testing.T) {
		requireRefused(t, beforeV142DB(t), agentRowsRemoteMigrationVersion, "agent_rows_remote",
			"thread_requests.nested_open", seeBeforeV142)
	})
	t.Run("before v142, repaired for v142 only", func(t *testing.T) {
		path := beforeV142DB(t)
		applyRepair(t, path, repairBeforeV142)
		requireRefused(t, path, receiptOwnTurnMigrationVersion, "receipt_own_turn",
			"thread_request_receipts.own_turn", seeBeforeV143)
	})
	t.Run("before v142, repaired for v143 only", func(t *testing.T) {
		path := beforeV142DB(t)
		applyRepair(t, path, repairBeforeV143)
		requireRefused(t, path, receiptOwnTurnMigrationVersion, "receipt_own_turn",
			"thread_request_receipts.own_turn", seeBeforeV142)
	})
	// Each build before main's v143 that stopped at an earlier migration
	// than agent_tree_marks.
	for _, era := range []struct {
		name, object string
		version      int
	}{
		{"late_delivery_route", "thread_requests.late_delivered_how", lateDeliveryRouteMigrationVersion},
		{"agent_records", "table agents", agentRecordsMigrationVersion},
		{"ledger_indexes", "index idx_thread_requests_caller_settled", ledgerIndexesMigrationVersion},
		{"agent_tree_marks", "thread_requests.open_below_0", agentTreeMarksMigrationVersion},
	} {
		t.Run("before v143, through "+era.name, func(t *testing.T) {
			requireRefused(t, beforeV143DB(t, era.version), era.version, era.name, era.object, seeBeforeV143)
		})
	}
}

// The repairs development.md gives, applied as written, leave a database
// this build opens and migrates to a fresh database's schema and records,
// from each state a pre-sync build left and in either order.
func TestThePreSyncRepairsOpen(t *testing.T) {
	t.Run("before v142, both repairs", func(t *testing.T) {
		path := beforeV142DB(t)
		applyRepair(t, path, repairBeforeV142)
		applyRepair(t, path, repairBeforeV143)
		requireOpens(t, path)
	})
	t.Run("before v142, both repairs in the other order", func(t *testing.T) {
		path := beforeV142DB(t)
		applyRepair(t, path, repairBeforeV143)
		applyRepair(t, path, repairBeforeV142)
		requireOpens(t, path)
	})
	t.Run("before v142, repaired for v142 and migrated by a build before v143", func(t *testing.T) {
		path := beforeV142DB(t)
		applyRepair(t, path, repairBeforeV142)
		// That build ran late_delivery_route through agent_tree_marks as
		// v153 to v157.
		applyShifted(t, path, receiptOwnTurnMigrationVersion, agentTreeMarksMigrationVersion, 1)
		requireRefused(t, path, agentTreeMarksMigrationVersion, "agent_tree_marks",
			"thread_requests.open_below_0", seeBeforeV143)
		applyRepair(t, path, repairBeforeV143)
		requireOpens(t, path)
	})
	for _, through := range []int{receiptOwnTurnMigrationVersion, lateDeliveryRouteMigrationVersion, agentRecordsMigrationVersion, ledgerIndexesMigrationVersion, agentTreeMarksMigrationVersion} {
		t.Run("before v143, through v"+itoa(through), func(t *testing.T) {
			path := beforeV143DB(t, through)
			applyRepair(t, path, repairBeforeV143)
			requireOpens(t, path)
		})
	}
}

// Running a block again, on a database it or both blocks repaired or on
// one a fresh build made, changes nothing.
func TestThePreSyncRepairsRunAgainChangeNothing(t *testing.T) {
	repairedV142 := beforeV142DB(t)
	applyRepair(t, repairedV142, repairBeforeV142)
	repairedBoth := beforeV142DB(t)
	applyRepair(t, repairedBoth, repairBeforeV142)
	applyRepair(t, repairedBoth, repairBeforeV143)
	repairedV143 := beforeV143DB(t, agentTreeMarksMigrationVersion)
	applyRepair(t, repairedV143, repairBeforeV143)
	fresh := fileDBThrough(t, latestMigrationVersionForTest())
	for _, tc := range []struct {
		name, path string
		blocks     []string
	}{
		{"v142 repaired", repairedV142, []string{repairBeforeV142}},
		{"both repaired", repairedBoth, []string{repairBeforeV142, repairBeforeV143}},
		{"v143 repaired", repairedV143, []string{repairBeforeV143}},
		{"fresh", fresh, []string{repairBeforeV142, repairBeforeV143}},
	} {
		for _, heading := range tc.blocks {
			before := dumpDB(t, tc.path)
			applyRepair(t, tc.path, heading)
			if after := dumpDB(t, tc.path); after != before {
				t.Errorf("%s: %s again changed the database:\nbefore %s\nafter  %s", tc.name, heading, before, after)
			}
		}
	}
}

// Each block creates main's table as its migration does.
func TestThePreSyncRepairsCreateMainsTables(t *testing.T) {
	for heading, want := range map[string]string{
		repairBeforeV142: threadPullRequestsV142SQL,
		repairBeforeV143: codexDeveloperInstructionsV143SQL,
	} {
		got := strings.ReplaceAll(repairSQL(t, heading), " IF NOT EXISTS", "")
		if !strings.Contains(collapseSpace(got), collapseSpace(want)) {
			t.Errorf("%s does not create the table as its migration does:\n block %q\n want %q", heading, got, want)
		}
	}
}

func collapseSpace(text string) string { return strings.Join(strings.Fields(text), " ") }
