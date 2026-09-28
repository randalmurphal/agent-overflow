package store

import (
	"database/sql"
	"slices"
	"strings"
	"testing"
)

// TestMigrationV139GroupedThreadPins applies v139 over a populated store
// holding pinned ungrouped threads, grouped threads and pinned groups. The
// rows survive unchanged, the schema is the old one less the grouped-pin
// CHECK and the two group pin columns, a grouped thread now takes a pin,
// and the foreign keys, trigger and view that name threads still work.
func TestMigrationV139GroupedThreadPins(t *testing.T) {
	db := migrateThrough(t, groupedThreadPinsMigrationVersion-1)
	mustExec(t, db, `INSERT INTO projects(id,path,name,created_at,updated_at) VALUES('p','/p','p',1,1)`)
	mustExec(t, db, `INSERT INTO thread_groups(id,project_id,name,pinned_at,pin_group,created_at,updated_at) VALUES
		('g-front','p','Front',7,0,1,1),
		('g-back','p','Back',8,1,1,1),
		('g-none','p','None',NULL,NULL,1,1)`)
	mustExec(t, db, `INSERT INTO threads(id,project_id,title,provider,workspace_path,created_at,updated_at,pinned_at,pin_group,group_id,parent_thread_id,archived) VALUES
		('front','p','Front pin','claude','/p',1,2,5,0,NULL,NULL,0),
		('back','p','Back pin','codex','/p',1,3,6,1,NULL,NULL,0),
		('member','p','Member','claude','/p',1,4,NULL,NULL,'g-front',NULL,0),
		('reply','p','Reply','claude','/p',1,5,NULL,NULL,'g-front','member',0),
		('archived','p','Archived member','claude','/p',1,6,NULL,NULL,'g-back',NULL,1),
		('fork','p','Fork','claude','/p',1,7,NULL,NULL,'g-none',NULL,0)`)
	mustExec(t, db, `INSERT INTO items(thread_id,id,turn_index,item_index,kind,role,status,summary,tool_name,meta,created_at,updated_at) VALUES
		('member','m0',0,0,'user_text','user','completed','ask','','{}',1,1)`)
	mustExec(t, db, `UPDATE threads SET fork_source_thread_id='member', fork_cut_turn_index=1 WHERE id='fork'`)
	mustExec(t, db, `INSERT INTO thread_fork_lineage(thread_id,depth,ancestor_id,cut_turn_index,cut_item_index) VALUES('fork',1,'member',1,0)`)

	rowsBefore := migrationThreadRows(t, db)
	schemaBefore := migrationSchema(t, db)
	if _, err := db.Exec(`UPDATE threads SET pinned_at = 9 WHERE id = 'member'`); err == nil {
		t.Fatal("a grouped+pinned write succeeded before v139; the fixture does not exercise the CHECK")
	}

	if err := applyRebuildMigration(db, migrationByVersion(t, groupedThreadPinsMigrationVersion)); err != nil {
		t.Fatalf("apply v%d: %v", groupedThreadPinsMigrationVersion, err)
	}

	if got := migrationThreadRows(t, db); !slices.Equal(got, rowsBefore) {
		t.Errorf("thread rows changed:\n got %q\nwant %q", got, rowsBefore)
	}

	// Every schema object is the one it was, except the threads CHECK and
	// the thread_groups pin columns.
	schemaAfter := migrationSchema(t, db)
	const check = "\n        CHECK(group_id IS NULL OR pinned_at IS NULL)"
	wantThreads := schemaBefore["threads"]
	if !strings.Contains(wantThreads, check) {
		t.Fatalf("threads schema before v139 lacks the grouped-pin CHECK: %s", wantThreads)
	}
	wantThreads = strings.Replace(wantThreads, check, "", 1)
	if schemaAfter["threads"] != wantThreads {
		t.Errorf("threads schema after v139:\n%s\nwant:\n%s", schemaAfter["threads"], wantThreads)
	}
	for name, sqlText := range schemaBefore {
		if name == "threads" || name == "thread_groups" {
			continue
		}
		if schemaAfter[name] != sqlText {
			t.Errorf("schema object %s changed:\n got %q\nwant %q", name, schemaAfter[name], sqlText)
		}
	}
	for name := range schemaAfter {
		if _, ok := schemaBefore[name]; !ok {
			t.Errorf("schema object %s is new after v139", name)
		}
	}
	groupColumns, err := queryIDs(db, `SELECT name FROM pragma_table_info('thread_groups') ORDER BY cid`)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"id", "project_id", "name", "created_at", "updated_at"}; !slices.Equal(groupColumns, want) {
		t.Errorf("thread_groups columns = %v, want %v", groupColumns, want)
	}
	groups, err := queryIDs(db, `SELECT id || '|' || project_id || '|' || name FROM thread_groups ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"g-back|p|Back", "g-front|p|Front", "g-none|p|None"}; !slices.Equal(groups, want) {
		t.Errorf("thread_groups rows = %v, want %v", groups, want)
	}

	// A grouped thread now holds its own pin; the burner CHECK stands.
	mustExec(t, db, `UPDATE threads SET pinned_at = 9, pin_group = 1 WHERE id = 'member'`)
	if _, err := db.Exec(`UPDATE threads SET pinned_at = NULL WHERE id = 'back'`); err == nil {
		t.Error("a burner on an unpinned thread succeeded; the pin_group CHECK is missing")
	}

	// Foreign keys: clean, enforced, and the group FK still ungroups.
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	fkErr := assertForeignKeysIntact(t.Context(), tx)
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	if fkErr != nil {
		t.Fatal(fkErr)
	}
	if _, err := db.Exec(`UPDATE threads SET group_id = 'missing' WHERE id = 'front'`); err == nil {
		t.Error("a thread joined a missing group; the group_id foreign key is missing")
	}
	mustExec(t, db, `DELETE FROM thread_groups WHERE id = 'g-back'`)
	var archivedGroup sql.NullString
	if err := db.QueryRow(`SELECT group_id FROM threads WHERE id = 'archived'`).Scan(&archivedGroup); err != nil {
		t.Fatal(err)
	}
	if archivedGroup.Valid {
		t.Errorf("deleting a group left its archived member in it: %q", archivedGroup.String)
	}
	if _, err := db.Exec(`DELETE FROM threads WHERE id = 'member'`); err == nil || !strings.Contains(err.Error(), "kept as a holder") {
		t.Errorf("deleting a thread a fork reads = %v, want the fork source trigger's refusal", err)
	}
	var owned int
	if err := db.QueryRow(`SELECT COUNT(*) FROM owned_threads`).Scan(&owned); err != nil {
		t.Fatalf("read owned_threads after v139: %v", err)
	}
	if owned != len(rowsBefore) {
		t.Errorf("owned_threads = %d rows, want %d", owned, len(rowsBefore))
	}
}

// migrationSchema maps every named schema object to its SQL.
func migrationSchema(t *testing.T, db *sql.DB) map[string]string {
	t.Helper()
	rows, err := db.Query(`SELECT name, type || ' ON ' || tbl_name || ': ' || COALESCE(sql, '') FROM sqlite_master WHERE name NOT LIKE 'sqlite_stat%'`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var name, sqlText string
		if err := rows.Scan(&name, &sqlText); err != nil {
			t.Fatal(err)
		}
		out[name] = sqlText
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// migrationThreadRows renders every column of every thread row, in the
// table's column order, so a rebuild that drops or mangles a value shows.
func migrationThreadRows(t *testing.T, db *sql.DB) []string {
	t.Helper()
	columns, err := queryIDs(db, `SELECT name FROM pragma_table_info('threads') ORDER BY cid`)
	if err != nil {
		t.Fatal(err)
	}
	quoted := make([]string, len(columns))
	for i, column := range columns {
		quoted[i] = "'" + column + "=' || quote(" + column + ")"
	}
	rendered, err := queryIDs(db, `SELECT `+strings.Join(quoted, ` || '|' || `)+` FROM threads ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	return rendered
}
