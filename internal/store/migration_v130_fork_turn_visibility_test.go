package store

import (
	"database/sql"
	"slices"
	"strings"
	"testing"
)

// TestMigrationV130ForkTurnVisibility: a reader whose cut on a nearer level
// a split lowered reads a deeper level's turn row past that cut, where the
// nearer level's own row is not the reader's: the view shows it, and the
// guards keep it as the reader read it.
func TestMigrationV130ForkTurnVisibility(t *testing.T) {
	db := migrateThrough(t, 129)
	mustExec(t, db, `INSERT INTO threads(id,title,provider,workspace_path,created_at,updated_at) VALUES
		('S','S','claude','/p',1,1),('F','F','claude','/p',1,1),('G','G','claude','/p',1,1)`)
	for turn := range 4 {
		mustExec(t, db, `INSERT INTO turns(turn_id,thread_id,turn_index,started_at,completed_at,stop_reason) VALUES(?, 'S', ?, 1, 2, 'end_turn')`,
			"S:"+string(rune('0'+turn)), turn)
	}
	// F reverted to turn 2 and owns its row there; G read F through turn 3
	// before that and reads S there still.
	mustExec(t, db, `INSERT INTO turns(turn_id,thread_id,turn_index,started_at,completed_at,stop_reason) VALUES('F:2','F',2,1,5,'reverted')`)
	mustExec(t, db, `INSERT INTO thread_fork_lineage(thread_id,depth,ancestor_id,cut_turn_index,cut_item_index) VALUES
		('F',1,'S',2,1),('G',1,'F',2,1),('G',2,'S',4,0)`)
	turns := func() []string {
		t.Helper()
		ids, err := queryIDs(db, `SELECT turn_id FROM timeline_turns WHERE thread_id = 'G' ORDER BY turn_index`)
		if err != nil {
			t.Fatal(err)
		}
		return ids
	}
	if got, want := turns(), []string{"S:0", "S:1", "S:3"}; !slices.Equal(got, want) {
		t.Fatalf("G's turns before v130 = %v, want %v", got, want)
	}
	migrateFromThrough(t, db, 129, 130)
	if got, want := turns(), []string{"S:0", "S:1", "S:2", "S:3"}; !slices.Equal(got, want) {
		t.Fatalf("G's turns = %v, want %v", got, want)
	}
	for _, write := range []string{
		`UPDATE turns SET stop_reason = 'late' WHERE turn_id = 'S:2'`,
		`DELETE FROM turns WHERE turn_id = 'S:2'`,
	} {
		if _, err := db.Exec(write); err == nil || !strings.Contains(err.Error(), shownHistoryImmutable) {
			t.Fatalf("%s = %v, want refused", write, err)
		}
	}
	// F's own row past G's cut on F is F's to change.
	mustExec(t, db, `UPDATE turns SET stop_reason = 'late' WHERE turn_id = 'F:2'`)
}

// TestForkTriggersMatchTheMigrations: the fork triggers a restore installs
// (forkTriggersSQL) are the ones the migrations leave.
func TestForkTriggersMatchTheMigrations(t *testing.T) {
	s := newTestStore(t)
	read := func() map[string]string {
		t.Helper()
		rows, err := s.db.Query(`SELECT name, sql FROM sqlite_master WHERE type = 'trigger'`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		out := map[string]string{}
		for rows.Next() {
			var name string
			var text sql.NullString
			if err := rows.Scan(&name, &text); err != nil {
				t.Fatal(err)
			}
			out[name] = text.String
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		return out
	}
	migrated := read()
	mustExec(t, s.db, dropForkTriggersSQL)
	mustExec(t, s.db, forkTriggersSQL)
	restored := read()
	for name, text := range restored {
		if migrated[name] != text {
			t.Errorf("trigger %s: migrated\n%s\nrestore installs\n%s", name, migrated[name], text)
		}
	}
	if len(restored) != len(migrated) {
		t.Fatalf("migrated %d triggers, restore leaves %d", len(migrated), len(restored))
	}
}
