package store

import (
	"strings"
	"testing"
)

// v153 refills the candidate text of every user row with a shell output,
// local, imported and shown by a pointer fork, to what a fresh write gives
// it, without moving a revision; other rows keep theirs. It reads the rows
// with attachments through their partial indexes.
func TestFindTextShellCommandsMigrationRefillsShellRows(t *testing.T) {
	db := migrateThrough(t, findTextShellCommandsMigrationVersion-1)
	shell := `{"attachments":[{"id":"att","threadId":"t","filename":"output.txt","mimeType":"text/plain","size":3,"kind":"file","shell":{"command":"make needle-target","exitCode":2}}]}`
	image := `{"attachments":[{"id":"img","threadId":"t","filename":"a.png","mimeType":"image/png","size":3}]}`
	mustExec(t, db, `INSERT INTO threads(id,title,provider,workspace_path,created_at,updated_at) VALUES
		('t','T','claude','/p',10,10),('source','S','claude','/p',10,10),('fork','F','claude','/p',10,10)`)
	mustExec(t, db, `INSERT INTO items(thread_id,id,turn_index,item_index,kind,role,status,summary,parent_id,meta,created_at,updated_at) VALUES
		('t','edited',1,0,'user_text','user','completed','run it again','',?,20,20),
		('t','joined',2,0,'user_text','user','completed',?,'',?,21,21),
		('t','image',3,0,'user_text','user','completed','look','',?,22,22),
		('source','shown',1,0,'user_text','user','completed','again','',?,23,23)`,
		shell, "a\n\n---\n\n!make needle-target\n\n---\n\nb", shell, image, shell)
	mustExec(t, db, `INSERT INTO thread_fork_lineage(thread_id,depth,ancestor_id,cut_turn_index,cut_item_index) VALUES('fork',1,'source',3,0)`)
	mustExec(t, db, `INSERT INTO import_history_chunks(id,item_count,min_turn_index,max_turn_index) VALUES('chunk',1,0,0)`)
	mustExec(t, db, `INSERT INTO import_history_items(chunk_id,id,turn_index,item_index,kind,role,status,summary,meta,created_at,updated_at)
		VALUES('chunk','imported',0,0,'user_text','user','completed','imported text',?,1,1)`, shell)
	// What v150 and v152 left: candidate text without the meta commands.
	mustExec(t, db, `UPDATE items SET find_text = 'stale'`)
	mustExec(t, db, `UPDATE import_history_items SET find_text = 'stale'`)
	revs := func() string {
		var out string
		if err := db.QueryRow(`SELECT group_concat(id || ':' || history_rev, ',') FROM (SELECT id, history_rev FROM threads ORDER BY id)`).Scan(&out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	before := revs()

	assertPlanUses(t, db, "idx_items_attachment_refs", `EXPLAIN QUERY PLAN SELECT rowid FROM items
		WHERE instr(meta, '"attachments"') > 0 AND kind = 'user_text' AND instr(meta, '"shell"') > 0`)
	assertPlanUses(t, db, "idx_import_history_items_attachment_refs", `EXPLAIN QUERY PLAN SELECT rowid FROM import_history_items
		WHERE instr(meta, '"attachments"') > 0 AND kind = 'user_text' AND instr(meta, '"shell"') > 0`)
	migrateFromThrough(t, db, findTextShellCommandsMigrationVersion-1, findTextShellCommandsMigrationVersion)

	for _, table := range []string{"items", "import_history_items"} {
		rows, err := db.Query(`SELECT id, kind, summary, meta, tool_name, find_text FROM ` + table)
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var id, kind, summary, meta, toolName, findText string
			if err := rows.Scan(&id, &kind, &summary, &meta, &toolName, &findText); err != nil {
				t.Fatal(err)
			}
			want := findCandidateText(kind, summary, meta, toolName, nil)
			if !strings.Contains(meta, `"shell"`) {
				want = "stale"
			}
			if findText != want {
				t.Errorf("%s %s find_text = %q, want %q", table, id, findText, want)
			}
			if want != "stale" && !strings.Contains(findText, findNeedle("needle-target")) {
				t.Errorf("%s %s find_text = %q misses its command", table, id, findText)
			}
		}
		if err := rows.Close(); err != nil {
			t.Fatal(err)
		}
	}
	if after := revs(); after != before {
		t.Errorf("history revisions %s -> %s, want unchanged", before, after)
	}
}
