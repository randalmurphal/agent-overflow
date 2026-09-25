package store

import (
	"slices"
	"strings"
	"testing"
)

// TestMigrationV132ForkWalkedAnchors: the migration records, for each
// thread written before it, the anchors above its hidden rows and above
// the rows it holds under a parent it does not hold, each found by id in
// the threads it reads, and nothing for a thread's own anchors, a
// top-level row, a row under a hidden parent or a row no card counts.
func TestMigrationV132ForkWalkedAnchors(t *testing.T) {
	db := migrateThrough(t, 131)
	mustExec(t, db, `INSERT INTO threads(id,title,provider,workspace_path,mode,created_at,updated_at) VALUES
		('S','S','claude','/p','chat',1,1),('F','F','claude','/p','chat',1,1),('G','G','claude','/p','chat',1,1),
		('F2','F2','claude','/p','chat',1,1),('R','R','claude','/p','chat',1,1),('H','H','claude','','holder',1,1)`)
	type row struct {
		thread, id, parent, kind, tool, status string
		turn, index                            int
	}
	for _, r := range []row{
		{"S", "u0", "", "user_text", "", "completed", 0, 0},
		{"S", "A", "", "tool_call", "Agent", "completed", 1, 0},
		{"S", "A-c1", "A", "tool_call", "Bash", "completed", 1, 1},
		{"S", "B", "A", "tool_call", "Agent", "completed", 1, 2},
		{"S", "B-c1", "B", "assistant_text", "", "completed", 1, 3},
		{"S", "L", "A", "tool_call", "Agent", "running", 1, 4},
		{"S", "L-c1", "L", "assistant_text", "", "completed", 1, 5},
		{"S", "C", "", "tool_call", "Agent", "completed", 2, 0},
		{"S", "C-c1", "C", "tool_call", "Read", "completed", 2, 1},
		{"S", "T", "", "tool_call", "Agent", "completed", 2, 2},
		{"S", "T-c1", "T", "assistant_text", "", "completed", 2, 3},
		{"S", "Q", "", "tool_call", "Agent", "completed", 2, 4},
		{"S", "N", "Q", "notification", "plan_update", "completed", 2, 5},
		{"S", "N-c1", "N", "assistant_text", "", "completed", 2, 6},
		{"S", "D", "", "tool_call", "Agent", "completed", 3, 0},
		// The holder took D's child and all of E.
		{"H", "D-c1", "D", "tool_call", "Bash", "completed", 3, 1},
		{"H", "E", "", "tool_call", "Agent", "completed", 3, 2},
		{"H", "E-c1", "E", "assistant_text", "", "completed", 3, 3},
		// A fork's own row under an anchor it reads, and under its own.
		{"F", "F-c1", "B", "tool_call", "Bash", "completed", 5, 0},
		{"F", "M", "", "tool_call", "Agent", "completed", 5, 1},
		{"F", "M-c1", "M", "assistant_text", "", "completed", 5, 2},
	} {
		role := "assistant"
		if r.kind == "user_text" {
			role = "user"
		} else if r.kind == "notification" {
			role = "system"
		}
		mustExec(t, db, `INSERT INTO items(thread_id,id,turn_index,item_index,kind,role,status,summary,parent_id,tool_name,meta,created_at,updated_at)
			VALUES(?,?,?,?,?,?,?,?,?,?,'{}',1,1)`, r.thread, r.id, r.turn, r.index, r.kind, role, r.status, r.id, r.parent, r.tool)
	}
	mustExec(t, db, `INSERT INTO thread_fork_lineage(thread_id,depth,ancestor_id,cut_turn_index,cut_item_index) VALUES
		('F',1,'S',4,0),('G',1,'F',6,0),('G',2,'S',4,0),('F2',1,'S',4,0),('R',1,'H',4,0),('R',2,'S',3,1)`)
	mustExec(t, db, `INSERT INTO thread_fork_hidden(thread_id,item_id) VALUES
		('F','L'),('F','L-c1'),('G','B-c1'),('G','C-c1'),('G','N-c1'),('F2','T'),('F2','T-c1'),
		('H','D-c1'),('H','E'),('H','E-c1')`)

	migrateFromThrough(t, db, 131, 132)

	for thread, want := range map[string][]string{
		// L's parent A; F-c1's parent B and B's parent A. M is F's own.
		"F": {"A", "B"},
		// B-c1 read through S two levels down; N-c1 hangs off a row no
		// card counts.
		"G": {"A", "B", "C"},
		// A hidden top-level row, and a row whose parent is hidden too.
		"F2": {},
		// D-c1's parent stayed with S; E went whole.
		"H": {"D"},
		"R": {},
		"S": {},
	} {
		got, err := queryIDs(db, `SELECT item_id FROM thread_fork_walked WHERE thread_id = ? ORDER BY item_id`, thread)
		if err != nil {
			t.Fatal(err)
		}
		if got == nil {
			got = []string{}
		}
		if !slices.Equal(got, want) {
			t.Errorf("%s marks %v, want %v", thread, got, want)
		}
	}

	var table string
	if err := db.QueryRow(`SELECT sql FROM sqlite_master WHERE type = 'table' AND name = 'thread_fork_walked'`).Scan(&table); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"PRIMARY KEY (thread_id, item_id)", "WITHOUT ROWID", "REFERENCES threads(id) ON DELETE CASCADE"} {
		if !strings.Contains(table, want) {
			t.Errorf("thread_fork_walked lacks %q:\n%s", want, table)
		}
	}
	mustExec(t, db, `DELETE FROM threads WHERE id = 'G'`)
	if left, err := queryIDs(db, `SELECT item_id FROM thread_fork_walked WHERE thread_id = 'G'`); err != nil || len(left) > 0 {
		t.Errorf("a deleted thread's markers = %v, %v", left, err)
	}
}
