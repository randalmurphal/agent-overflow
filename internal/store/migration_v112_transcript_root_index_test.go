package store

import (
	"database/sql"
	"strings"
	"testing"
)

func TestMigrationV112RepairsTranscriptRootIndex(t *testing.T) {
	for _, tc := range []struct {
		name    string
		version int
		missing bool
	}{
		{"earlier_dev_v100", 100, true},
		{"already_upgraded_v111", 111, true},
		{"correct_v111", 111, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := migrateThrough(t, tc.version)
			if tc.missing {
				// Earlier dev v100 lacked this index but was already recorded as applied.
				mustExec(t, db, `DROP INDEX idx_items_transcript_root`)
			}
			mustExec(t, db, `INSERT INTO threads(id,provider,workspace_path,created_at,updated_at)
			 VALUES('t','claude','/tmp',1,1)`)
			mustExec(t, db, `INSERT INTO items(thread_id,id,turn_index,item_index,kind,role,status,summary,parent_id,meta,created_at,updated_at) VALUES
			 ('t','launch',0,0,'tool_call','assistant','completed','launch','','{}',1,1),
			 ('t','carrier',0,1,'tool_call','assistant','completed','resume','','{"transcript_root_id":"launch"}',1,1),
			 ('t','child',0,2,'assistant_text','assistant','completed','preserve me','launch','{}',1,1)`)
			before := transcriptIndexFixtureState(t, db)
			var applied int64
			if err := db.QueryRow(`SELECT applied FROM migration_versions WHERE version=100`).Scan(&applied); err != nil {
				t.Fatal(err)
			}
			for attempt := 0; attempt < 2; attempt++ {
				if err := runMigrations(db); err != nil {
					t.Fatal(err)
				}
				if after := transcriptIndexFixtureState(t, db); after != before {
					t.Fatalf("migration changed history: before=%s after=%s", before, after)
				}
				assertTranscriptRootStampPlan(t, db)
			}
			var preserved int64
			if err := db.QueryRow(`SELECT applied FROM migration_versions WHERE version=100`).Scan(&preserved); err != nil {
				t.Fatal(err)
			}
			if preserved != applied {
				t.Fatal("repair replaced the applied v100 record")
			}
			var beforeRev int64
			if err := db.QueryRow(`SELECT history_rev FROM threads WHERE id='t'`).Scan(&beforeRev); err != nil {
				t.Fatal(err)
			}
			mustExec(t, db, `UPDATE items SET summary='changed' WHERE thread_id='t' AND id='child'`)
			var stamped int
			if err := db.QueryRow(`SELECT count(*) FROM items JOIN threads ON threads.id=items.thread_id
			 WHERE threads.id='t' AND threads.history_rev=? AND items.rev=threads.history_rev`, beforeRev+1).Scan(&stamped); err != nil {
				t.Fatal(err)
			}
			if stamped != 3 {
				t.Fatalf("child update stamped %d rows, want child, launch and resume carrier", stamped)
			}
		})
	}
}

func TestFreshStoreHasTranscriptRootStampIndex(t *testing.T) {
	s := newTestStore(t)
	assertTranscriptRootStampPlan(t, s.db)
}

func assertTranscriptRootStampPlan(t *testing.T, db *sql.DB) {
	t.Helper()
	plan := planText(explainPlan(t, &Store{db: db},
		stampRowsSQL+` WHERE thread_id=?1 AND id IN (`+
			stampedRowIDsFor("?1", "?2", "?3")+`)`, "t", "child", "launch"))
	if !strings.Contains(plan, "USING INDEX idx_items_transcript_root") {
		t.Fatalf("revision stamp does not use the transcript-root index:\n%s", plan)
	}
	lookup := planText(explainPlan(t, &Store{db: db},
		`SELECT id FROM items WHERE thread_id=? AND json_extract(meta,'$.transcript_root_id')=?`, "t", "launch"))
	if !strings.Contains(lookup, "USING INDEX idx_items_transcript_root (thread_id=? AND <expr>=?)") {
		t.Fatalf("carrier lookup does not probe both index keys:\n%s", lookup)
	}
}

func transcriptIndexFixtureState(t *testing.T, db *sql.DB) string {
	t.Helper()
	var state string
	if err := db.QueryRow(`SELECT json_array(history_rev,history_epoch,
	 (SELECT json_group_array(json_array(id,parent_id,meta,summary,rev))
	  FROM (SELECT id,parent_id,meta,summary,rev FROM items WHERE thread_id='t' ORDER BY id)))
	 FROM threads WHERE id='t'`).Scan(&state); err != nil {
		t.Fatal(err)
	}
	return state
}
