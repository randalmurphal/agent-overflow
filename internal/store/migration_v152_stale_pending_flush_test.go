package store

import (
	"strings"
	"testing"
)

// v152 clears pendingFlush from every marked user row the residue rule calls
// history, a row a pointer fork shows included, and leaves the mark on the
// residue the boot restore takes back. Each changed row's thread moves to a
// new revision, and the item guard still refuses an ordinary write to a row
// a fork shows.
func TestStalePendingFlushMigrationClearsHistoryRowsOnly(t *testing.T) {
	db := migrateThrough(t, stalePendingFlushMigrationVersion-1)
	mustExec(t, db, `INSERT INTO threads(id,title,provider,workspace_path,created_at,updated_at) VALUES
		('history','H','claude','/p',10,10),('tail','T','claude','/p',10,10),('both','B','claude','/p',10,10),
		('turn-only','O','claude','/p',10,10),('copied','C','claude','/p',10,10),
		('source','S','claude','/p',10,10),('fork','F','claude','/p',10,10)`)
	mustExec(t, db, `INSERT INTO items(thread_id,id,turn_index,item_index,kind,role,status,summary,parent_id,meta,created_at,updated_at) VALUES
		('history','user:flush:a',9,20,'user_text','user','completed','old','','{"sendId":"s-a","pendingFlush":true}',20,20),
		('history','assistant:10',10,0,'assistant_text','assistant','completed','later','','{}',30,30),
		('tail','user:flush:b',3,4,'user_text','user','completed','residue','','{"pendingFlush":true}',20,20),
		('tail','assistant:3',3,5,'assistant_text','assistant','completed','same turn','','{}',21,21),
		('both','user:flush:c',1,1,'user_text','user','completed','old','','{"pendingFlush":true}',20,20),
		('both','assistant:2',2,0,'assistant_text','assistant','completed','later','','{}',30,30),
		('both','user:flush:d',2,1,'user_text','user','completed','residue','','{"pendingFlush":true}',31,31),
		('both','child',2,2,'user_text','user','completed','agent','toolu','{"pendingFlush":true}',32,32),
		('turn-only','user:flush:e',4,0,'user_text','user','completed','old','','{"pendingFlush":true}',20,20),
		('copied','user:flush:f',4,0,'user_text','user','completed','copy','','{"pendingFlush":true}',5,5),
		('source','user:flush:g',1,0,'user_text','user','completed','shown','','{"pendingFlush":true}',20,20),
		('source','assistant:s2',2,0,'assistant_text','assistant','completed','later','','{}',30,30)`)
	mustExec(t, db, `INSERT INTO turns(turn_id,thread_id,turn_index,started_at) VALUES('turn-5','turn-only',5,40)`)
	mustExec(t, db, `INSERT INTO thread_fork_lineage(thread_id,depth,ancestor_id,cut_turn_index,cut_item_index) VALUES('fork',1,'source',3,0)`)
	revBefore := map[string]int64{}
	for _, id := range []string{"history", "tail", "source"} {
		var rev int64
		if err := db.QueryRow(`SELECT history_rev FROM threads WHERE id = ?`, id).Scan(&rev); err != nil {
			t.Fatal(err)
		}
		revBefore[id] = rev
	}

	migrateFrom(t, db, stalePendingFlushMigrationVersion-1)

	for id, want := range map[string]string{
		"user:flush:a": `{"sendId":"s-a"}`,
		"user:flush:b": `{"pendingFlush":true}`,
		"user:flush:c": `{}`,
		"user:flush:d": `{"pendingFlush":true}`,
		"child":        `{"pendingFlush":true}`,
		"user:flush:e": `{}`,
		"user:flush:f": `{}`,
		"user:flush:g": `{}`,
	} {
		var meta string
		if err := db.QueryRow(`SELECT meta FROM items WHERE id = ?`, id).Scan(&meta); err != nil {
			t.Fatalf("read %s: %v", id, err)
		}
		if meta != want {
			t.Errorf("%s meta = %q, want %q", id, meta, want)
		}
	}
	for id, moved := range map[string]bool{"history": true, "tail": false, "source": true} {
		var rev int64
		if err := db.QueryRow(`SELECT history_rev FROM threads WHERE id = ?`, id).Scan(&rev); err != nil {
			t.Fatal(err)
		}
		if (rev > revBefore[id]) != moved {
			t.Errorf("%s history_rev %d -> %d, want moved=%v", id, revBefore[id], rev, moved)
		}
	}
	if _, err := db.Exec(`UPDATE items SET meta = '{"edited":true}' WHERE id = 'user:flush:g'`); err == nil || !strings.Contains(err.Error(), shownHistoryImmutable) {
		t.Fatalf("write to a shown row after v152 = %v, want refused", err)
	}
	var fixRows int
	if err := db.QueryRow(`SELECT count(*) FROM shown_history_fix`).Scan(&fixRows); err != nil || fixRows != 0 {
		t.Fatalf("shown_history_fix rows = %d (%v), want none left", fixRows, err)
	}
}
