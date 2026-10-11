package store

import (
	"database/sql"
	"errors"
	"slices"
	"strings"
	"testing"
	"unicode"
)

// TestMigrationV142RemoveDiscussions applies v142 over a v141 store holding
// what the discussion feature wrote: discussion threads with channels and
// messages, parent_thread_id children (one pair inside a group), a
// definition, a discussion favorite beside a model favorite, and a scratch
// thread forked out of a discussion. Discussion threads and the scratch
// return mode become chat, every other thread value survives, the feature's
// columns, tables and indexes are gone, the rebuilt tables restate every
// other declaration, the three CHECKs refuse 'discussion', and the view,
// trigger and foreign keys that name threads still work.
func TestMigrationV142RemoveDiscussions(t *testing.T) {
	db := migrateThrough(t, removeDiscussionsMigrationVersion-1)
	mustExec(t, db, `INSERT INTO projects(id,path,name,created_at,updated_at) VALUES('p','/p','p',1,1)`)
	mustExec(t, db, `INSERT INTO thread_groups(id,project_id,name,created_at,updated_at) VALUES('g','p','Group',1,1)`)
	mustExec(t, db, `INSERT INTO threads(id,project_id,title,provider,model,workspace_path,mode,created_at,updated_at,archived,group_id) VALUES
		('disc-parent','p','Discussion','claude','opus','/p','discussion',1,6,0,NULL),
		('disc-child-a','p','Participant A','claude','opus','/p','chat',1,5,0,NULL),
		('disc-child-b','p','Participant B','codex','gpt','/p','chat',1,4,0,NULL),
		('grp-parent','p','Grouped discussion','claude','opus','/p','discussion',1,3,0,'g'),
		('grp-child','p','Grouped participant','claude','opus','/p','chat',1,2,0,'g'),
		('scratch','p','Scratch','claude','opus','/p','scratch',1,1,0,NULL),
		('fork','p','Fork','claude','opus','/p','chat',1,1,0,NULL)`)
	mustExec(t, db, `INSERT INTO channels(id,thread_id,type,status,created_at,updated_at) VALUES
		('chan-1','disc-parent','deliberation','open',1,1),
		('chan-2','grp-parent','deliberation','concluded',1,1)`)
	mustExec(t, db, `INSERT INTO channel_messages(id,channel_id,sequence,from_type,from_id,from_role,content,created_at) VALUES
		('msg-0','chan-1',0,'user','user',NULL,'Topic',1),
		('msg-1','chan-1',1,'agent','disc-child-a','architect','Reply',1)`)
	mustExec(t, db, `UPDATE threads SET discussion_id='chan-1' WHERE id='disc-parent'`)
	mustExec(t, db, `UPDATE threads SET discussion_id='chan-2' WHERE id='grp-parent'`)
	mustExec(t, db, `UPDATE threads SET parent_thread_id='disc-parent' WHERE id IN ('disc-child-a','disc-child-b')`)
	mustExec(t, db, `UPDATE threads SET parent_thread_id='grp-parent' WHERE id='grp-child'`)
	mustExec(t, db, `INSERT INTO discussion_definitions(id,name,description,scope,definition,created_at,updated_at)
		VALUES('def','Architects','','global','{"participants":[]}',1,1)`)
	mustExec(t, db, `INSERT INTO chat_bar_favorites(kind,provider,value,label,created_at) VALUES
		('discussion','','def','Discussion favorite',1),
		('model','claude','opus','Opus',2)`)
	mustExec(t, db, `INSERT INTO scratch_threads(thread_id,source_thread_id,return_mode,created_at,request_token)
		VALUES('scratch','disc-parent','discussion',3,'tok')`)
	mustExec(t, db, `INSERT INTO items(id,thread_id,turn_index,item_index,kind,role,status,summary,meta,created_at,updated_at) VALUES
		('i-parent','disc-parent',0,0,'user_text','user','completed','Topic','{}',1,1),
		('i-scratch','scratch',0,0,'user_text','user','completed','Aside','{}',1,1)`)
	mustExec(t, db, `UPDATE threads SET fork_source_thread_id='disc-child-a', fork_cut_turn_index=1 WHERE id='fork'`)
	mustExec(t, db, `INSERT INTO thread_fork_lineage(thread_id,depth,ancestor_id,cut_turn_index,cut_item_index) VALUES('fork',1,'disc-child-a',1,0)`)

	removedColumns := []string{"discussion_id", "parent_thread_id"}
	removedTables := []string{"channels", "channel_messages", "discussion_definitions"}
	removedIndexes := []string{"idx_channels_thread", "idx_threads_parent"}
	rebuiltTables := []string{"threads", "scratch_threads", "chat_bar_favorites"}

	columnsBefore, err := queryIDs(db, `SELECT name FROM pragma_table_info('threads') ORDER BY cid`)
	if err != nil {
		t.Fatal(err)
	}
	for _, column := range removedColumns {
		if !slices.Contains(columnsBefore, column) {
			t.Fatalf("threads lacks %s before v142; the fixture does not exercise its drop", column)
		}
	}
	kept := slices.DeleteFunc(slices.Clone(columnsBefore), func(c string) bool { return slices.Contains(removedColumns, c) })
	// The rows v142 must produce: every kept value as it was, with
	// discussion mode read as chat.
	wantRows := renderThreadRows(t, db, kept, map[string]string{
		"mode": "CASE mode WHEN 'discussion' THEN 'chat' ELSE mode END",
	})
	schemaBefore := migrationSchema(t, db)
	for _, name := range slices.Concat(removedTables, removedIndexes) {
		if _, ok := schemaBefore[name]; !ok {
			t.Fatalf("%s is missing before v142; the fixture does not exercise its drop", name)
		}
	}
	// A removed object is a removed table, one of its indexes (declared or
	// automatic) or a removed index.
	removed := func(name string) bool {
		if slices.Contains(removedIndexes, name) {
			return true
		}
		return slices.ContainsFunc(removedTables, func(table string) bool {
			return strings.Contains(schemaBefore[name], " ON "+table+":")
		})
	}
	fksBefore := foreignKeysInto(t, db, "threads")
	shapesBefore := map[string]tableShape{}
	for _, table := range rebuiltTables {
		shapesBefore[table] = readTableShape(t, db, table)
	}

	migrateFrom(t, db, removeDiscussionsMigrationVersion-1)

	if got := renderThreadRows(t, db, kept, nil); !slices.Equal(got, wantRows) {
		t.Errorf("thread rows after v142:\n got %q\nwant %q", got, wantRows)
	}
	modes, err := queryIDs(db, `SELECT id || '=' || mode FROM threads WHERE id IN ('disc-parent','grp-parent') ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"disc-parent=chat", "grp-parent=chat"}; !slices.Equal(modes, want) {
		t.Errorf("discussion thread modes after v142 = %v, want %v", modes, want)
	}
	scratch, err := queryIDs(db, `SELECT thread_id || '|' || source_thread_id || '|' || return_mode || '|' || created_at || '|' || request_token FROM scratch_threads`)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"scratch|disc-parent|chat|3|tok"}; !slices.Equal(scratch, want) {
		t.Errorf("scratch_threads after v142 = %v, want %v", scratch, want)
	}
	favorites, err := queryIDs(db, `SELECT kind || '|' || provider || '|' || value || '|' || label || '|' || created_at FROM chat_bar_favorites`)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"model|claude|opus|Opus|2"}; !slices.Equal(favorites, want) {
		t.Errorf("chat_bar_favorites after v142 = %v, want %v", favorites, want)
	}

	// The discussion objects are gone and nothing else is: every schema
	// object other than the three rebuilt tables keeps its SQL, and the
	// rebuilt tables are compared declaration by declaration below.
	schemaAfter := migrationSchema(t, db)
	for name, sqlText := range schemaBefore {
		if removed(name) {
			if _, ok := schemaAfter[name]; ok {
				t.Errorf("%s survived v142", name)
			}
			continue
		}
		if slices.Contains(rebuiltTables, name) {
			continue
		}
		if strings.Join(strings.Fields(schemaAfter[name]), " ") != strings.Join(strings.Fields(sqlText), " ") {
			t.Errorf("schema object %s changed:\n got %q\nwant %q", name, schemaAfter[name], sqlText)
		}
	}
	for name := range schemaAfter {
		if _, ok := schemaBefore[name]; !ok {
			t.Errorf("schema object %s is new after v142", name)
		}
	}

	// Each rebuilt table restates v141's declarations less the discussion
	// parts: the removed columns and their foreign keys, 'discussion' in
	// each IN list, the composite favorites CHECK, whose model arm becomes
	// a provider CHECK, and provider's DEFAULT '', which only discussion
	// favorites used.
	withoutDiscussion := strings.NewReplacer(",'discussion'", "", "'discussion',", "")
	for _, table := range rebuiltTables {
		before, after := shapesBefore[table], readTableShape(t, db, table)

		var wantColumns []columnShape
		for _, column := range before.columns {
			if slices.Contains(removedColumns, column.Name) {
				continue
			}
			if table == "chat_bar_favorites" && column.Name == "provider" {
				column.Default = sql.NullString{}
			}
			wantColumns = append(wantColumns, column)
		}
		if !slices.Equal(after.columns, wantColumns) {
			t.Errorf("%s columns after v142:\n got %+v\nwant %+v", table, after.columns, wantColumns)
		}

		wantChecks := make([]string, 0, len(before.checks))
		for _, check := range before.checks {
			if table == "chat_bar_favorites" && strings.Contains(check, "kind='discussion'") {
				check = "providerIN('claude','codex','claude-tui')"
			}
			wantChecks = append(wantChecks, withoutDiscussion.Replace(check))
		}
		slices.Sort(wantChecks)
		if slices.Equal(wantChecks, before.checks) {
			t.Fatalf("%s CHECKs before v142 never name 'discussion'; the fixture does not exercise the change: %q", table, before.checks)
		}
		if !slices.Equal(after.checks, wantChecks) {
			t.Errorf("%s CHECKs after v142:\n got %q\nwant %q", table, after.checks, wantChecks)
		}

		wantFKs := slices.DeleteFunc(slices.Clone(before.fks), func(fk string) bool {
			return slices.ContainsFunc(removedColumns, func(column string) bool {
				return strings.HasPrefix(fk, column+" ")
			})
		})
		if !slices.Equal(after.fks, wantFKs) {
			t.Errorf("%s foreign keys after v142 = %q, want %q", table, after.fks, wantFKs)
		}
	}

	// Each CHECK that named the mode refuses it now. The fixture's own
	// inserts show all three accepted it at v141.
	for _, refused := range []struct{ name, query string }{
		{"threads.mode", `UPDATE threads SET mode = 'discussion' WHERE id = 'disc-child-a'`},
		{"scratch_threads.return_mode", `UPDATE scratch_threads SET return_mode = 'discussion' WHERE thread_id = 'scratch'`},
		{"chat_bar_favorites.kind", `INSERT INTO chat_bar_favorites(kind,provider,value,label,created_at) VALUES('discussion','claude','def','Discussion favorite',4)`},
	} {
		if _, err := db.Exec(refused.query); err == nil || !strings.Contains(err.Error(), "CHECK constraint failed") {
			t.Errorf("%s = 'discussion' after v142: err = %v, want a CHECK violation", refused.name, err)
		}
	}
	mustExec(t, db, `UPDATE threads SET mode = 'plan' WHERE id = 'disc-child-a'`)
	mustExec(t, db, `UPDATE scratch_threads SET return_mode = 'plan' WHERE thread_id = 'scratch'`)
	mustExec(t, db, `INSERT INTO chat_bar_favorites(kind,provider,value,label,created_at) VALUES('model','codex','gpt','GPT',4)`)

	// Foreign keys into threads: the same set less the two v142 removed,
	// clean, and enforced on the rebuilt tables.
	wantFKs := slices.DeleteFunc(slices.Clone(fksBefore), func(fk string) bool {
		return fk == "channels.thread_id" || fk == "threads.parent_thread_id"
	})
	if len(wantFKs) != len(fksBefore)-2 {
		t.Fatalf("foreign keys into threads before v142 = %v, want channels.thread_id and threads.parent_thread_id among them", fksBefore)
	}
	if got := foreignKeysInto(t, db, "threads"); !slices.Equal(got, wantFKs) {
		t.Errorf("foreign keys into threads after v142 = %v, want %v", got, wantFKs)
	}
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
	if _, err := db.Exec(`UPDATE threads SET group_id = 'missing' WHERE id = 'grp-child'`); err == nil {
		t.Error("a thread joined a missing group; the group_id foreign key is missing")
	}
	mustExec(t, db, `DELETE FROM threads WHERE id = 'scratch'`)
	var orphans int
	if err := db.QueryRow(`SELECT (SELECT COUNT(*) FROM scratch_threads WHERE thread_id = 'scratch') + (SELECT COUNT(*) FROM items WHERE thread_id = 'scratch')`).Scan(&orphans); err != nil {
		t.Fatal(err)
	}
	if orphans != 0 {
		t.Errorf("deleting a thread left %d scratch or item rows; the cascades into threads are missing", orphans)
	}

	if _, err := db.Exec(`DELETE FROM threads WHERE id = 'disc-child-a'`); err == nil || !strings.Contains(err.Error(), "kept as a holder") {
		t.Errorf("deleting a thread a fork reads = %v, want the fork source trigger's refusal", err)
	}
	var owned int
	if err := db.QueryRow(`SELECT COUNT(*) FROM owned_threads`).Scan(&owned); err != nil {
		t.Fatalf("read owned_threads after v142: %v", err)
	}
	if want := len(wantRows) - 1; owned != want {
		t.Errorf("owned_threads = %d rows, want %d", owned, want)
	}
}

// renderThreadRows renders the named columns of every thread row, ordered
// by id. exprs replaces a column's value with an SQL expression.
func renderThreadRows(t *testing.T, db sqlQueryer, columns []string, exprs map[string]string) []string {
	t.Helper()
	parts := make([]string, len(columns))
	for i, column := range columns {
		expr := column
		if e, ok := exprs[column]; ok {
			expr = e
		}
		parts[i] = "'" + column + "=' || quote(" + expr + ")"
	}
	rows, err := queryIDs(db, `SELECT `+strings.Join(parts, ` || '|' || `)+` FROM threads ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

// foreignKeysInto lists every table.column that references parent.
func foreignKeysInto(t *testing.T, db sqlQueryer, parent string) []string {
	t.Helper()
	fks, err := queryIDs(db, `SELECT m.name || '.' || f."from"
		FROM sqlite_master m, pragma_foreign_key_list(m.name) f
		WHERE m.type = 'table' AND f."table" = ? ORDER BY 1`, parent)
	if err != nil {
		t.Fatal(err)
	}
	return fks
}

// tableShape is what a table rebuild has to restate: the columns as
// pragma_table_info reports them, the CHECK clauses and the outgoing
// foreign keys.
type tableShape struct {
	columns []columnShape
	checks  []string
	fks     []string
}

// columnShape is one pragma_table_info row.
type columnShape struct {
	Name    string
	Type    string
	NotNull bool
	Default sql.NullString
	PK      int
}

func readTableShape(t *testing.T, db sqlQueryer, table string) tableShape {
	t.Helper()
	var shape tableShape
	rows, err := db.Query(`SELECT name, type, "notnull", dflt_value, pk FROM pragma_table_info(?) ORDER BY cid`, table)
	if err != nil {
		t.Fatal(err)
	}
	var scanErr error
	for scanErr == nil && rows.Next() {
		var column columnShape
		scanErr = rows.Scan(&column.Name, &column.Type, &column.NotNull, &column.Default, &column.PK)
		shape.columns = append(shape.columns, column)
	}
	if err := errors.Join(scanErr, rows.Err(), rows.Close()); err != nil {
		t.Fatalf("read %s columns: %v", table, err)
	}
	var create string
	if err := db.QueryRow(`SELECT sql FROM sqlite_master WHERE type = 'table' AND name = ?`, table).Scan(&create); err != nil {
		t.Fatal(err)
	}
	shape.checks = checkClauses(create)
	shape.fks, err = queryIDs(db, `SELECT "from" || ' -> ' || "table" || '(' || COALESCE("to", '') || ') ON UPDATE ' || on_update || ' ON DELETE ' || on_delete
		FROM pragma_foreign_key_list(?) ORDER BY 1`, table)
	if err != nil {
		t.Fatal(err)
	}
	return shape
}

// checkClauses returns the body of every CHECK constraint in a CREATE TABLE
// statement, sorted, with whitespace outside string literals removed so the
// comparison does not depend on layout.
func checkClauses(create string) []string {
	var checks []string
	upper := strings.ToUpper(create)
	for from := 0; ; {
		at := strings.Index(upper[from:], "CHECK")
		if at < 0 {
			break
		}
		at += from
		from = at + len("CHECK")
		if at > 0 && (create[at-1] == '_' || unicode.IsLetter(rune(create[at-1])) || unicode.IsDigit(rune(create[at-1]))) {
			continue
		}
		open := from
		for open < len(create) && unicode.IsSpace(rune(create[open])) {
			open++
		}
		if open == len(create) || create[open] != '(' {
			continue
		}
		var body strings.Builder
		depth, quoted := 0, false
		for i := open; i < len(create); i++ {
			c := create[i]
			switch {
			case c == '\'':
				quoted = !quoted
			case quoted:
			case c == '(':
				depth++
			case c == ')':
				depth--
			case unicode.IsSpace(rune(c)):
				continue
			}
			if depth == 0 {
				from = i + 1
				break
			}
			if i > open {
				body.WriteByte(c)
			}
		}
		checks = append(checks, body.String())
	}
	slices.Sort(checks)
	return checks
}
