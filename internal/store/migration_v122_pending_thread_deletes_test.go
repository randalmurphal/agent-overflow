package store

import (
	"slices"
	"testing"
)

// TestMigrationV122PendingThreadDeletes: existing threads upgrade unmarked
// and stay owned, the mark takes only 0 or 1, a marked thread leaves
// owned_threads while the transfer rule the view already applied still
// holds, and the boot listing reads the partial index.
func TestMigrationV122PendingThreadDeletes(t *testing.T) {
	db := migrateThrough(t, 121)
	mustExec(t, db, `INSERT INTO projects(id,path,name,slug,created_at,updated_at) VALUES('p','/p','p','p',1,1)`)
	for _, id := range []string{"kept", "marked", "moved"} {
		mustExec(t, db, `INSERT INTO threads(id,project_id,title,provider,workspace_path,created_at,updated_at) VALUES(?,'p',?,'claude','/p',1,1)`, id, id)
	}
	mustExec(t, db, `INSERT INTO thread_transfers (id, thread_id, target_thread_id, peer_backend_id, kind,
		     direction, phase, activation_hash, private_state, archive_size, created_at, updated_at)
		 VALUES ('op', 'moved', '', 'peer', 'move', 'outgoing', 'complete', '', '{}', 0, 1, 1)`)
	migrateFrom(t, db, 121)

	owned := func() []string {
		t.Helper()
		ids, err := queryIDs(db, `SELECT id FROM owned_threads ORDER BY id`)
		if err != nil {
			t.Fatal(err)
		}
		return ids
	}
	var marked int
	if err := db.QueryRow(`SELECT COUNT(*) FROM threads WHERE deleting <> 0`).Scan(&marked); err != nil || marked != 0 {
		t.Fatalf("upgraded threads marked = %d, err %v; want none", marked, err)
	}
	if got, want := owned(), []string{"kept", "marked"}; !slices.Equal(got, want) {
		t.Fatalf("owned_threads after the upgrade = %v, want %v", got, want)
	}
	if _, err := db.Exec(`UPDATE threads SET deleting = 2 WHERE id = 'marked'`); err == nil {
		t.Fatal("deleting accepted 2")
	}
	mustExec(t, db, `UPDATE threads SET deleting = 1 WHERE id = 'marked'`)
	if got, want := owned(), []string{"kept"}; !slices.Equal(got, want) {
		t.Fatalf("owned_threads with a marked thread = %v, want %v", got, want)
	}
	var epoch int64
	if err := db.QueryRow(`SELECT ownership_epoch FROM owned_threads WHERE id = 'kept'`).Scan(&epoch); err != nil || epoch != 0 {
		t.Fatalf("kept ownership_epoch = %d, err %v", epoch, err)
	}
	pending, err := queryIDs(db, `SELECT id FROM threads WHERE deleting = 1`)
	if err != nil || !slices.Equal(pending, []string{"marked"}) {
		t.Fatalf("pending deletes = %v, err %v", pending, err)
	}
	assertPlanUses(t, db, "idx_threads_deleting", `EXPLAIN QUERY PLAN SELECT id FROM threads WHERE deleting = 1`)
}
