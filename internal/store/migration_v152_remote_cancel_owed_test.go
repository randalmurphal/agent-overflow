package store

import (
	"strings"
	"testing"
)

// v152 adds the cancel a remote command is owed and the mark of a stopped
// completion. A watch from before it owes none and was stopped by nothing,
// and the watcher's due list reads the owed ones through their index.
func TestRemoteCancelOwedMigrationOwesNothingForOldWatches(t *testing.T) {
	db := migrateThrough(t, remoteCancelOwedMigrationVersion-1)
	mustExec(t, db, `INSERT INTO projects (id, path, name, created_at, updated_at)
		VALUES ('p-owed', '/tmp/owed', 'Owed', 1, 1)`)
	mustExec(t, db, `
		INSERT INTO threads (id, project_id, title, provider, workspace_path, model,
			created_at, updated_at, archived, mode)
		VALUES ('t-owed', 'p-owed', 'Owed', 'claude', '/tmp/owed', 'test-model', 1, 1, 0, 'chat')`)
	mustExec(t, db, `
		INSERT INTO remote_watches (computer_id, request_id, thread_id, launched_in_thread_id, fingerprint, label, created_at)
		VALUES ('c-owed', 'r-owed', 't-owed', 't-owed', 'f', 'job', 1)`)

	migrateFrom(t, db, remoteCancelOwedMigrationVersion-1)

	var owed, next, stopped int64
	if err := db.QueryRow(`SELECT cancel_owed_at, cancel_next_at, stopped_at FROM remote_watches WHERE request_id = 'r-owed'`).Scan(&owed, &next, &stopped); err != nil {
		t.Fatalf("read the owed cancel: %v", err)
	}
	if owed != 0 || next != 0 || stopped != 0 {
		t.Fatalf("owed cancel = %d, %d, stopped at %d on a watch from before the columns, want none", owed, next, stopped)
	}
	plan := explainWakeCarrierPlan(t, db, `SELECT `+remoteWatchColumns+` FROM remote_watches `+owedRemoteCancelsWhere, int64(5))
	if want := "idx_remote_watches_cancel_owed"; !strings.Contains(plan, want) {
		t.Fatalf("due list plan = %q, want %s", plan, want)
	}
}
