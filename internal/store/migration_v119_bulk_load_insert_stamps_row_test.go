package store

import (
	"database/sql"
	"fmt"
	"testing"
)

// An insert under history_bulk_load stamps the inserted row alone; outside
// the flag it still stamps the launch, its carrier and its completion
// sibling. v118 stamped them in both cases.
func TestMigrationV119BulkLoadInsertStampsOnlyTheRow(t *testing.T) {
	db := migrateThrough(t, 118)
	mustExec(t, db, `INSERT INTO projects(id,path,name,slug,created_at,updated_at) VALUES('p','/p','p','p',1,1)`)
	mustExec(t, db, `INSERT INTO threads(id,project_id,title,provider,workspace_path,created_at,updated_at) VALUES('t','p','t','claude','/p',1,1)`)
	mustExec(t, db, `INSERT INTO items(id,thread_id,turn_index,item_index,kind,role,status,summary,tool_name,completion_of,meta,created_at,updated_at)
 VALUES('launch','t',0,0,'tool_call','assistant','completed','launch','Agent','','{}',1,1),
       ('carrier','t',0,1,'tool_call','assistant','completed','resume','Agent','','{"transcript_root_id":"launch"}',1,1),
       ('done','t',0,2,'tool_completion','assistant','completed','done','','launch','{}',1,1)`)
	anchors := []string{"launch", "carrier", "done"}
	revs := func() (map[string]int64, int64) {
		t.Helper()
		got := map[string]int64{}
		for _, id := range append([]string{"c118", "c119", "live"}, anchors...) {
			var rev int64
			err := db.QueryRow(`SELECT rev FROM items WHERE thread_id='t' AND id=?`, id).Scan(&rev)
			if err == sql.ErrNoRows {
				continue
			}
			if err != nil {
				t.Fatal(err)
			}
			got[id] = rev
		}
		var historyRev int64
		if err := db.QueryRow(`SELECT history_rev FROM threads WHERE id='t'`).Scan(&historyRev); err != nil {
			t.Fatal(err)
		}
		return got, historyRev
	}
	bulkInsertChild := func(id string, index int) int64 {
		t.Helper()
		// Move the stamp past every anchor so a re-stamp is visible.
		mustExec(t, db, `UPDATE threads SET history_rev = history_rev + 10 WHERE id='t'`)
		_, frozen := revs()
		tx, err := db.Begin()
		if err != nil {
			t.Fatal(err)
		}
		for _, statement := range []string{
			`UPDATE threads SET history_bulk_load = 1 WHERE id='t'`,
			fmt.Sprintf(`INSERT INTO items(id,thread_id,turn_index,item_index,kind,role,status,summary,parent_id,meta,created_at,updated_at)
 VALUES('%s','t',0,%d,'assistant_text','assistant','completed','child','launch','{}',1,1)`, id, index),
			`UPDATE threads SET history_rev = history_rev + 1, history_bulk_load = 0 WHERE id='t'`,
		} {
			if _, err := tx.Exec(statement); err != nil {
				t.Fatal(errJoinRollback(err, tx))
			}
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
		return frozen
	}

	frozen := bulkInsertChild("c118", 3)
	got, _ := revs()
	for _, id := range anchors {
		if got[id] != frozen {
			t.Fatalf("v118 bulk-load insert left %s at rev %d, want the re-stamp %d", id, got[id], frozen)
		}
	}

	migrateFrom(t, db, 118)
	before, _ := revs()
	frozen = bulkInsertChild("c119", 4)
	got, historyRev := revs()
	if got["c119"] != frozen || historyRev != frozen+1 {
		t.Fatalf("bulk-load child rev=%d history_rev=%d, want %d and %d", got["c119"], historyRev, frozen, frozen+1)
	}
	for _, id := range anchors {
		if got[id] != before[id] {
			t.Errorf("bulk-load insert re-stamped %s: %d -> %d", id, before[id], got[id])
		}
	}

	// Outside the flag the insert trigger stamps the whole row set.
	mustExec(t, db, `INSERT INTO items(id,thread_id,turn_index,item_index,kind,role,status,summary,parent_id,meta,created_at,updated_at)
 VALUES('live','t',0,5,'assistant_text','assistant','completed','child','launch','{}',1,1)`)
	got, historyRev = revs()
	for _, id := range append([]string{"live"}, anchors...) {
		if got[id] != historyRev {
			t.Errorf("live insert left %s at rev %d, want %d", id, got[id], historyRev)
		}
	}
}

func errJoinRollback(err error, tx *sql.Tx) error {
	if rollbackErr := tx.Rollback(); rollbackErr != nil {
		return fmt.Errorf("%w (rollback: %v)", err, rollbackErr)
	}
	return err
}
