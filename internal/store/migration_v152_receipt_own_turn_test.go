package store

import (
	"database/sql"
	"slices"
	"strings"
	"testing"
)

// v152 over a populated database: every receipt, bound or not, reads as
// delivered into a turn it did not open; no other column changes.
func TestMigrationV152ReceiptOwnTurn(t *testing.T) {
	path := fileDBThrough(t, receiptOwnTurnMigrationVersion-1)
	db, err := sql.Open("sqlite", poolDSN(path, writerConnPragmas))
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	mustExec(t, db, `INSERT INTO projects (id, path, name, created_at, updated_at) VALUES ('p', '/p', 'p', 1, 1)`)
	mustExec(t, db, `INSERT INTO threads (id, project_id, title, provider, workspace_path, model, created_at, updated_at, archived) VALUES
		('target', 'p', 'Target', 'claude', '/p', '', 1, 1, 0)`)
	mustExec(t, db, `INSERT INTO thread_request_receipts (token, owner_device_id, kind, target_thread_id, message_item_id, turn_id, state, answer, settled_at, created_at, updated_at) VALUES
		('bound', 'local', 'send', 'target', 'user:0', 'target:0', 'running', NULL, NULL, 1, 1),
		('settled', 'local', 'send', 'target', 'user:1', 'target:1', 'finished', 'a', 10, 1, 1),
		('unbound', 'local', 'send', 'target', '', '', 'accepted', NULL, NULL, 1, 1)`)
	const before = threadReceiptColumnsBeforeV150 + ` || '|' || message_item_id || '|' || turn_id || '|' || answer_agent || '|' || answer_agent_role`
	want, err := queryIDs(db, `SELECT `+before+` FROM thread_request_receipts ORDER BY token`)
	if err != nil {
		t.Fatal(err)
	}

	if err := applyMigration(db, migrationByVersion(t, receiptOwnTurnMigrationVersion)); err != nil {
		t.Fatalf("apply v%d: %v", receiptOwnTurnMigrationVersion, err)
	}

	if got, err := queryIDs(db, `SELECT `+before+` FROM thread_request_receipts ORDER BY token`); err != nil {
		t.Fatal(err)
	} else if !slices.Equal(got, want) {
		t.Errorf("thread_request_receipts changed:\n got %q\nwant %q", got, want)
	}
	got, err := queryIDs(db, `SELECT token || '|' || own_turn FROM thread_request_receipts ORDER BY token`)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"bound|0", "settled|0", "unbound|0"}; !slices.Equal(got, want) {
		t.Errorf("own turns = %q, want %q", got, want)
	}
}

// preSyncPersonalAssistantDB is a database a personal-assistant build from
// before main's v142 migrated: main's chain through v141, then
// personal-assistant's ten migrations recorded one version lower (v142 to
// v151), with no thread_pull_requests.
func preSyncPersonalAssistantDB(t *testing.T) string {
	t.Helper()
	path := fileDBThrough(t, threadPullRequestsMigrationVersion-1)
	db, err := sql.Open("sqlite", poolDSN(path, writerConnPragmas))
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	for _, m := range migrations {
		// Later migrations postdate the sync; no pre-sync build has them.
		if m.Version <= threadPullRequestsMigrationVersion || m.Version > receiptOwnTurnMigrationVersion {
			continue
		}
		m.Version--
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
	return path
}

// A pre-sync personal-assistant database already holds v152's column under
// v151. Opening it refuses v152 by naming that cause and the repair's doc
// section, not with SQLite's duplicate-column error, and changes nothing.
func TestReceiptOwnTurnRefusesAPreSyncPersonalAssistantDatabase(t *testing.T) {
	path := preSyncPersonalAssistantDB(t)
	_, err := New(path)
	if err == nil {
		t.Fatal("New opened a pre-sync personal-assistant database")
	}
	want := "migration v152 (receipt_own_turn) refused: thread_request_receipts.own_turn already exists: " +
		"this database was migrated by a pre-sync personal-assistant build; " +
		"see docs/architecture/development.md#a-database-from-a-pre-sync-personal-assistant-build"
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("New error = %q, want it to contain %q", err, want)
	}
	if strings.Contains(err.Error(), "duplicate column") {
		t.Fatalf("New error = %q, want the cause rather than SQLite's duplicate-column error", err)
	}
	if got := recordedVersion(t, path); got != receiptOwnTurnMigrationVersion-1 {
		t.Fatalf("recorded version = %d, want %d", got, receiptOwnTurnMigrationVersion-1)
	}
}

// The repair docs/architecture/development.md gives, applied to a pre-sync
// database, leaves one the build opens, migrating on from v152, with
// thread_pull_requests.
func TestThePreSyncRepairOpens(t *testing.T) {
	path := preSyncPersonalAssistantDB(t)
	db, err := sql.Open("sqlite", poolDSN(path, writerConnPragmas))
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, db, threadPullRequestsV142SQL)
	mustExec(t, db, `INSERT INTO migration_versions (version, name) VALUES (152, 'receipt_own_turn')`)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	s, err := New(path)
	if err != nil {
		t.Fatalf("New after the repair: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if got := recordedVersion(t, path); got != latestMigrationVersionForTest() {
		t.Fatalf("recorded version = %d, want %d", got, latestMigrationVersionForTest())
	}
}
