package store

import (
	"database/sql"
	"os"
	"path/filepath"
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
	db, err := sql.Open("sqlite", poolDSN(path, writerConnPragmas))
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
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
// left: main's chain through v142, then personal-assistant's
// remove_discussions through agent_tree_marks recorded as v143 to v157.
func beforeV143DB(t *testing.T) string {
	return preSyncDB(t, threadPullRequestsMigrationVersion, codexDeveloperInstructionsMigrationVersion, agentTreeMarksMigrationVersion, 1)
}

// repairSQL is the SQL block of development.md's section heading, as a
// reader would paste it.
func repairSQL(t *testing.T, heading string) string {
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

const (
	repairBeforeV142 = "#### Before main's v142"
	repairBeforeV143 = "#### Before main's v143"
)

func applyRepair(t *testing.T, path, heading string) {
	t.Helper()
	db, err := sql.Open("sqlite", poolDSN(path, writerConnPragmas))
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	mustExec(t, db, repairSQL(t, heading))
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
}

// requireRefused opens path and wants the named refusal at version, with
// nothing applied.
func requireRefused(t *testing.T, path string, version int, name, column, repair string) {
	t.Helper()
	before := recordedVersion(t, path)
	_, err := New(path)
	if err == nil {
		t.Fatal("New opened a pre-sync personal-assistant database")
	}
	want := "migration v" + itoa(version) + " (" + name + ") refused: " + column + " already exists: " +
		"this database was migrated by a pre-sync personal-assistant build; see " + repair
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("New error = %q, want it to contain %q", err, want)
	}
	if got := recordedVersion(t, path); got != before {
		t.Fatalf("recorded version = %d after the refusal, want %d", got, before)
	}
}

// requireOpens opens path, which migrates it to this build's last version
// with main's tables shaped as this build makes them.
func requireOpens(t *testing.T, path string) {
	t.Helper()
	s, err := New(path)
	if err != nil {
		t.Fatalf("New after the repair: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if got := recordedVersion(t, path); got != latestMigrationVersionForTest() {
		t.Fatalf("recorded version = %d, want %d", got, latestMigrationVersionForTest())
	}
	fresh := fileDBThrough(t, latestMigrationVersionForTest())
	for _, table := range []string{"thread_pull_requests", "codex_developer_instructions"} {
		if got, want := tableSQL(t, path, table), tableSQL(t, fresh, table); got != want {
			t.Errorf("%s after the repair:\n got %q\nwant %q", table, got, want)
		}
	}
}

// tableSQL is the stored declaration of table and its indexes.
func tableSQL(t *testing.T, path, table string) string {
	t.Helper()
	db, err := sql.Open("sqlite", poolDSN(path, readerConnPragmas))
	if err != nil {
		t.Fatal(err)
	}
	rows, err := queryIDs(db, `SELECT type || ' ' || name || ': ' || sql FROM sqlite_master WHERE tbl_name = '`+table+`' AND sql IS NOT NULL ORDER BY type, name`)
	if closeErr := db.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) == 0 {
		t.Fatalf("%s is missing", table)
	}
	return strings.Join(rows, "\n")
}

// An unrepaired pre-sync database is refused at the first migration it
// already holds, naming the section that repairs it, and changes nothing.
func TestPreSyncDatabasesAreRefusedByName(t *testing.T) {
	t.Run("before v142", func(t *testing.T) {
		requireRefused(t, beforeV142DB(t), agentRowsRemoteMigrationVersion, "agent_rows_remote",
			"thread_requests.nested_open", preSyncBeforeV142)
	})
	t.Run("before v142, repaired for v142 only", func(t *testing.T) {
		path := beforeV142DB(t)
		applyRepair(t, path, repairBeforeV142)
		requireRefused(t, path, receiptOwnTurnMigrationVersion, "receipt_own_turn",
			"thread_request_receipts.own_turn", preSyncBeforeV143)
	})
	t.Run("before v142, repaired out of order", func(t *testing.T) {
		path := beforeV142DB(t)
		applyRepair(t, path, repairBeforeV143)
		requireRefused(t, path, receiptOwnTurnMigrationVersion, "receipt_own_turn",
			"thread_request_receipts.own_turn", preSyncBeforeV143)
	})
	t.Run("before v143", func(t *testing.T) {
		requireRefused(t, beforeV143DB(t), agentTreeMarksMigrationVersion, "agent_tree_marks",
			"thread_requests.open_below_0", preSyncBeforeV143)
	})
}

// The repairs development.md gives, applied as written in the order it
// states, or the first after the second with the change it states, leave a database this build opens and migrates to its last
// version, from each state a pre-sync build left.
func TestThePreSyncRepairsOpen(t *testing.T) {
	t.Run("before v142, both repairs", func(t *testing.T) {
		path := beforeV142DB(t)
		applyRepair(t, path, repairBeforeV142)
		applyRepair(t, path, repairBeforeV143)
		requireOpens(t, path)
	})
	t.Run("before v142, repaired for v142 and migrated by a build before v143", func(t *testing.T) {
		path := beforeV142DB(t)
		applyRepair(t, path, repairBeforeV142)
		// That build ran late_delivery_route through agent_tree_marks as
		// v153 to v157.
		applyShifted(t, path, receiptOwnTurnMigrationVersion, agentTreeMarksMigrationVersion, 1)
		requireRefused(t, path, agentTreeMarksMigrationVersion, "agent_tree_marks",
			"thread_requests.open_below_0", preSyncBeforeV143)
		applyRepair(t, path, repairBeforeV143)
		requireOpens(t, path)
	})
	t.Run("before v142, repaired out of order", func(t *testing.T) {
		path := beforeV142DB(t)
		applyRepair(t, path, repairBeforeV143)
		block := repairSQL(t, repairBeforeV142)
		const insert = "VALUES (152, 'receipt_own_turn')"
		if !strings.Contains(block, insert) {
			t.Fatalf("%s has no %q to change", repairBeforeV142, insert)
		}
		db, err := sql.Open("sqlite", poolDSN(path, writerConnPragmas))
		if err != nil {
			t.Fatal(err)
		}
		db.SetMaxOpenConns(1)
		mustExec(t, db, strings.Replace(block, insert, "VALUES (153, 'receipt_own_turn')", 1))
		if err := db.Close(); err != nil {
			t.Fatal(err)
		}
		requireOpens(t, path)
	})
	t.Run("before v143", func(t *testing.T) {
		path := beforeV143DB(t)
		applyRepair(t, path, repairBeforeV143)
		requireOpens(t, path)
	})
}

// Each block development.md gives creates main's table as its migration
// does.
func TestThePreSyncRepairsCreateMainsTables(t *testing.T) {
	for heading, want := range map[string]string{
		repairBeforeV142: threadPullRequestsV142SQL,
		repairBeforeV143: codexDeveloperInstructionsV143SQL,
	} {
		if got := repairSQL(t, heading); !strings.Contains(collapseSpace(got), collapseSpace(want)) {
			t.Errorf("%s does not create the table as its migration does:\n block %q\n want %q", heading, got, want)
		}
	}
}

func collapseSpace(text string) string { return strings.Join(strings.Fields(text), " ") }
