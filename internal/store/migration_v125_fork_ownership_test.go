package store

import (
	"slices"
	"strings"
	"testing"
)

// TestMigrationV125ForkOwnership: the migration removes every fork divider
// row, wherever a hand-off or transfer left it, and moves the stamps of the
// thread that held it and of the forks that read that thread; it drops the
// copy records and the fork source index, rebuilds threads with every row,
// index and trigger and the holder mode, leaves holders out of
// owned_threads and installs the guards that keep a row a fork shows as it
// is.
func TestMigrationV125ForkOwnership(t *testing.T) {
	db := migrateThrough(t, 124)
	mustExec(t, db, `INSERT INTO projects(id,path,name,slug,created_at,updated_at) VALUES('p','/p','p','p',1,1)`)
	mustExec(t, db, `INSERT INTO threads(id,project_id,title,provider,workspace_path,created_at,updated_at,pinned_at,fork_source_thread_id,fork_cut_turn_index,fork_cut_item_index,fork_source_title,deleting) VALUES
		('S','p','Source','claude','/p',1,2,5,'',0,0,'',0),
		('F','p','Fork','claude','/p',1,3,NULL,'S',1,0,'Source',0),
		('G','p','Grandchild','codex','/p',1,4,NULL,'F',1,1,'Fork',0),
		('K','p','Keeper','claude','/p',1,5,NULL,'',0,0,'',0),
		('X','p','Leaving','claude','/p',1,6,NULL,'',0,0,'',1)`)
	mustExec(t, db, `INSERT INTO items(thread_id,id,turn_index,item_index,kind,role,status,summary,tool_name,meta,created_at,updated_at) VALUES
		('S','s0',0,0,'user_text','user','completed','ask','','{}',1,1),
		('S','s1',0,1,'assistant_text','assistant','completed','reply','','{}',1,1),
		('S','s2',1,5,'assistant_text','assistant','completed','later','','{}',1,1),
		('F','fork-origin-F',1,0,'notification','system','completed','Forked from Source','fork_origin','{}',1,1),
		('G','fork-origin-G',1,1,'notification','system','completed','Forked from Fork','fork_origin','{}',1,1),
		('G','g-copy',1,2,'assistant_text','assistant','completed','copy','','{}',1,1),
		('K','fork-origin-F',0,0,'notification','system','completed','Forked from Source','fork_origin','{}',1,1),
		('K','fork-origin-note',0,1,'user_text','user','completed','not a divider','','{}',1,1)`)
	mustExec(t, db, `INSERT INTO thread_fork_copied(thread_id,item_id,source_id) VALUES('G','g-copy','S')`)
	// The forks read the rows their sources held when they were made.
	mustExec(t, db, `INSERT INTO thread_fork_lineage(thread_id,depth,ancestor_id,cut_turn_index,cut_item_index) VALUES
		('F',1,'S',1,0),('G',1,'F',1,1),('G',2,'S',1,0)`)

	type stamp struct{ rev, epoch int64 }
	stamps := func() map[string]stamp {
		t.Helper()
		rows, err := db.Query(`SELECT id, history_rev, history_epoch FROM threads`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		out := map[string]stamp{}
		for rows.Next() {
			var id string
			var s stamp
			if err := rows.Scan(&id, &s.rev, &s.epoch); err != nil {
				t.Fatal(err)
			}
			out[id] = s
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		return out
	}
	threadRows := func() []string {
		t.Helper()
		ids, err := queryIDs(db, `SELECT id || '|' || COALESCE(project_id, '') || '|' || title || '|' || provider || '|' || mode || '|' ||
			COALESCE(pinned_at, '') || '|' || updated_at || '|' || fork_source_thread_id || '|' || fork_cut_turn_index || ':' || fork_cut_item_index || '|' ||
			fork_source_title || '|' || deleting FROM threads ORDER BY id`)
		if err != nil {
			t.Fatal(err)
		}
		return ids
	}
	schemaNames := func(kind, table string) []string {
		t.Helper()
		names, err := queryIDs(db, `SELECT name FROM sqlite_master WHERE type = ? AND tbl_name = ? ORDER BY name`, kind, table)
		if err != nil {
			t.Fatal(err)
		}
		return names
	}
	before, rowsBefore := stamps(), threadRows()
	indexesBefore := slices.DeleteFunc(schemaNames("index", "threads"), func(name string) bool { return name == "idx_threads_fork_source" })
	threadTriggersBefore := schemaNames("trigger", "threads")

	migrateFromThrough(t, db, 124, 125)

	dividers, err := queryIDs(db, `SELECT thread_id || '/' || id FROM items WHERE tool_name = 'fork_origin' ORDER BY 1`)
	if err != nil || len(dividers) != 0 {
		t.Fatalf("dividers left: %v, %v", dividers, err)
	}
	if kept, err := queryIDs(db, `SELECT thread_id || '/' || id FROM items WHERE id >= 'fork-origin-' AND id < 'fork-origin.' ORDER BY 1`); err != nil || !slices.Equal(kept, []string{"K/fork-origin-note"}) {
		t.Fatalf("rows under the divider prefix = %v, %v; want only the one that is no divider", kept, err)
	}
	after := stamps()
	for id, moved := range map[string]int64{"S": 0, "F": 1, "G": 2, "K": 1, "X": 0} {
		if got := after[id]; got.rev-before[id].rev != moved || got.epoch-before[id].epoch != moved {
			t.Errorf("%s stamps %+v -> %+v, want rev and epoch up %d", id, before[id], got, moved)
		}
	}
	requireIDs(t, "thread rows", threadRows(), rowsBefore)
	requireIDs(t, "threads indexes", schemaNames("index", "threads"), indexesBefore)
	requireIDs(t, "threads triggers", schemaNames("trigger", "threads"), threadTriggersBefore)
	for _, gone := range []string{"thread_fork_copied", "idx_thread_fork_copied_source", "idx_threads_fork_source", "trg_items_fork_reader_stamp"} {
		if n := countRowsDB(t, db, `SELECT count(*) FROM sqlite_master WHERE name = ?`, gone); n != 0 {
			t.Errorf("%s survived", gone)
		}
	}
	for _, trigger := range []string{"trg_threads_fork_source_delete", "trg_items_fork_position", "trg_items_fork_position_update",
		"trg_items_fork_snapshot", "trg_items_fork_snapshot_move", "trg_items_shown_update", "trg_items_shown_delete",
		"trg_payloads_shown_update", "trg_payload_chunks_shown_insert", "trg_payload_chunks_shown_update",
		"trg_payload_chunks_shown_delete", "trg_turns_shown_update", "trg_turns_shown_delete",
		"trg_items_revive_bg_launch_on_completion_move", "trg_thread_fork_lineage_release"} {
		if n := countRowsDB(t, db, `SELECT count(*) FROM sqlite_master WHERE type = 'trigger' AND name = ?`, trigger); n != 1 {
			t.Errorf("%s missing", trigger)
		}
	}

	mustExec(t, db, `INSERT INTO threads(id,title,provider,workspace_path,mode,created_at,updated_at) VALUES('H','Source','claude','','holder',1,1)`)
	if _, err := db.Exec(`INSERT INTO threads(id,provider,workspace_path,mode,created_at,updated_at) VALUES('bad','claude','/p','bogus',1,1)`); err == nil {
		t.Fatal("the rebuilt threads accepted an unknown mode")
	}
	owned, err := queryIDs(db, `SELECT id FROM owned_threads ORDER BY id`)
	if err != nil || !slices.Equal(owned, []string{"F", "G", "K", "S"}) {
		t.Fatalf("owned threads = %v, %v; want neither the holder nor the deleting thread", owned, err)
	}
	if _, err := db.Exec(`UPDATE items SET summary = 'rewritten' WHERE thread_id = 'S' AND id = 's1'`); err == nil || !strings.Contains(err.Error(), shownHistoryImmutable) {
		t.Fatalf("a rewrite of a row the forks show = %v, want refused", err)
	}
	mustExec(t, db, `UPDATE items SET summary = 'rewritten' WHERE thread_id = 'S' AND id = 's2'`)
	mustExec(t, db, `DELETE FROM thread_fork_lineage WHERE thread_id = 'G'`)
	mustExec(t, db, `INSERT INTO thread_fork_lineage(thread_id,depth,ancestor_id,cut_turn_index,cut_item_index) VALUES('F',2,'H',0,0)`)
	mustExec(t, db, `DELETE FROM thread_fork_lineage WHERE thread_id = 'F' AND ancestor_id = 'H'`)
	var deleting bool
	if err := db.QueryRow(`SELECT deleting FROM threads WHERE id = 'H'`).Scan(&deleting); err != nil || !deleting {
		t.Fatalf("a holder no lineage row names: deleting=%v, %v", deleting, err)
	}
	if n := countRowsDB(t, db, `SELECT count(*) FROM pragma_foreign_key_check`); n != 0 {
		t.Fatalf("%d foreign key violations", n)
	}
	var check string
	if err := db.QueryRow(`PRAGMA integrity_check`).Scan(&check); err != nil || check != "ok" {
		t.Fatalf("integrity_check = %q, %v", check, err)
	}
}
