package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"unicode"
)

// TestMigrationV154RetireFeatures applies v154 over a v149 store holding what
// the discussion feature and the workflow scheduler wrote, beside spawns,
// groups and remote watches:
//
//   - discussion threads with channels and messages, parent_thread_id
//     children (one pair inside a group), a definition, a discussion
//     favorite beside a model favorite, and a scratch thread forked out of a
//     discussion;
//   - an automation with a cursor, a run it started, and a manual, an agent
//     and a called run beside it;
//   - local spawns of a grouped and an ungrouped thread, a remote spawn and
//     a send;
//   - a queued, a pending and a dismissed remote watch, and one whose thread
//     is gone.
//
// Discussion threads and the scratch return mode become chat, the automation
// run becomes a manual run with no source_ref, the ungrouped spawned thread
// is standalone (no thread from before keeps out of the sidebar) and the
// grouped ones grouped, every other value survives, the
// queued watch reads confirmed at its creation and every kept watch launched
// by its own thread, the orphan watch is gone, the retired columns, tables
// and indexes are gone, the rebuilt tables restate every other declaration,
// the CHECKs refuse the retired values and enforce placement, and the view,
// trigger and foreign keys that name threads still work.
func TestMigrationV154RetireFeatures(t *testing.T) {
	db := migrateThrough(t, retireFeaturesMigrationVersion-1)
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
	mustExec(t, db, `UPDATE threads SET pinned_at = 5, pin_group = 1 WHERE id = 'grp-child'`)
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
	mustExec(t, db, `INSERT INTO thread_requests(token,caller_thread_id,kind,target_computer_id,target_thread_id,state,created_at,updated_at) VALUES
		('sp-b','disc-parent','spawn','','disc-child-b','finished',1,1),
		('sp-grp','disc-parent','spawn','','grp-child','finished',1,1),
		('sp-far','disc-parent','spawn','laptop','fork','finished',1,1),
		('send-a','disc-parent','send','','disc-child-a','finished',1,1)`)
	mustExec(t, db, `INSERT INTO automations(id,project_id,workflow_id,workflow_scope,name,enabled,trigger,notes,created_at,updated_at)
		VALUES('auto','p','build','project','Nightly',1,'{"kind":"cron","expr":"0 3 * * *"}','carry on',1,1)`)
	mustExec(t, db, `INSERT INTO automation_cursors(automation_id,source_key,cursor,updated_at) VALUES('auto','jira','42',1)`)
	mustExec(t, db, `INSERT INTO work_items(id,project_id,goal,workflow_id,workflow_scope,state,source,source_ref,created_at) VALUES
		('run-auto','p','Nightly','build','project','done','automation','auto',1),
		('run-manual','p','By hand','build','project','running','manual','',2),
		('run-agent','p','By agent','build','project','done','agent','item/phase/key',3)`)
	mustExec(t, db, `INSERT INTO work_items(id,project_id,goal,workflow_id,workflow_scope,state,source,source_ref,parent_item_id,parent_phase_id,parent_attempt,call_depth,created_at)
		VALUES('run-call','p','Called','build','project','done','call','site','run-manual','plan',1,1,4)`)
	mustExec(t, db, `INSERT INTO remote_watches(computer_id,request_id,thread_id,fingerprint,label,receipt,error,notification,next_check,created_at,command) VALUES
		('pc','queued','disc-child-a','f','job','{"id":"queued"}','e','queued',5,6,'cmd'),
		('pc','pending','fork','f','job','{}','','pending',7,8,''),
		('pc','dismissed','fork','f','job','{}','','dismissed',9,10,''),
		('pc','orphan','deleted-thread','f','job','{}','','pending',0,1,'')`)

	removedColumns := []string{"discussion_id", "parent_thread_id"}
	removedTables := []string{"channels", "channel_messages", "discussion_definitions", "automations", "automation_cursors"}
	removedIndexes := []string{"idx_channels_thread", "idx_threads_parent", "idx_work_items_automation_source_ref"}
	rebuiltTables := []string{"threads", "scratch_threads", "chat_bar_favorites", "work_items", "remote_watches"}
	addedObjects := []string{"idx_remote_watches_done"}
	addedColumns := map[string][]columnShape{
		"threads": {
			{Name: "placement", Type: "TEXT", NotNull: true, Default: sql.NullString{String: "'standalone'", Valid: true}},
			{Name: "attention_kind", Type: "TEXT", NotNull: true, Default: sql.NullString{String: "''", Valid: true}},
			{Name: "attention_reason", Type: "TEXT", NotNull: true, Default: sql.NullString{String: "''", Valid: true}},
			{Name: "attention_at", Type: "INTEGER"},
		},
		"remote_watches": {
			{Name: "confirmed_at", Type: "INTEGER"},
			{Name: "wake_attempts", Type: "INTEGER", NotNull: true, Default: sql.NullString{String: "0", Valid: true}},
			{Name: "wake_issue", Type: "TEXT", NotNull: true, Default: sql.NullString{String: "''", Valid: true}},
			{Name: "launched_in_thread_id", Type: "TEXT", NotNull: true, Default: sql.NullString{String: "''", Valid: true}},
		},
	}
	addedChecks := map[string][]string{
		"threads": {
			"placementIN('owned','grouped','standalone')AND(placement='grouped')=(group_idISNOTNULL)",
			"attention_kindIN('','decision','review','problem')",
		},
	}
	addedFKs := map[string][]string{
		"remote_watches": {"thread_id -> threads(id) ON UPDATE NO ACTION ON DELETE CASCADE"},
	}

	columnsBefore, err := queryIDs(db, `SELECT name FROM pragma_table_info('threads') ORDER BY cid`)
	if err != nil {
		t.Fatal(err)
	}
	for _, column := range removedColumns {
		if !slices.Contains(columnsBefore, column) {
			t.Fatalf("threads lacks %s before v154; the fixture does not exercise its drop", column)
		}
	}
	kept := slices.DeleteFunc(slices.Clone(columnsBefore), func(c string) bool { return slices.Contains(removedColumns, c) })
	// The rows v154 must produce: every kept value as it was, with
	// discussion mode read as chat.
	wantRows := renderThreadRows(t, db, kept, map[string]string{
		"mode": "CASE mode WHEN 'discussion' THEN 'chat' ELSE mode END",
	})
	workColumns, err := queryIDs(db, `SELECT name FROM pragma_table_info('work_items') ORDER BY cid`)
	if err != nil {
		t.Fatal(err)
	}
	renderWork := func() []string {
		t.Helper()
		return renderRows(t, db, "work_items", workColumns, "id")
	}
	// The automation run reads as a manual run with no source_ref.
	mustExec(t, db, `UPDATE work_items SET source = 'manual', source_ref = '' WHERE id = 'run-auto'`)
	wantWork := renderWork()
	mustExec(t, db, `UPDATE work_items SET source = 'automation', source_ref = 'auto' WHERE id = 'run-auto'`)
	if slices.Equal(renderWork(), wantWork) {
		t.Fatal("restoring the automation run left the rows unchanged; the fixture does not exercise the mapping")
	}
	watchColumns, err := queryIDs(db, `SELECT name FROM pragma_table_info('remote_watches') ORDER BY cid`)
	if err != nil {
		t.Fatal(err)
	}
	renderWatches := func() []string {
		t.Helper()
		return renderRows(t, db, "remote_watches", watchColumns, "computer_id, request_id")
	}
	mustExec(t, db, `DELETE FROM remote_watches WHERE request_id = 'orphan'`)
	wantWatches := renderWatches()
	mustExec(t, db, `INSERT INTO remote_watches(computer_id,request_id,thread_id,fingerprint,label,notification,created_at)
		VALUES('pc','orphan','deleted-thread','f','job','pending',1)`)

	schemaBefore := migrationSchema(t, db)
	for _, name := range slices.Concat(removedTables, removedIndexes) {
		if _, ok := schemaBefore[name]; !ok {
			t.Fatalf("%s is missing before v154; the fixture does not exercise its drop", name)
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

	migrateFromThrough(t, db, retireFeaturesMigrationVersion-1, retireFeaturesMigrationVersion)

	if got := renderThreadRows(t, db, kept, nil); !slices.Equal(got, wantRows) {
		t.Errorf("thread rows after v154:\n got %q\nwant %q", got, wantRows)
	}
	modes, err := queryIDs(db, `SELECT id || '=' || mode FROM threads WHERE id IN ('disc-parent','grp-parent') ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"disc-parent=chat", "grp-parent=chat"}; !slices.Equal(modes, want) {
		t.Errorf("discussion thread modes after v154 = %v, want %v", modes, want)
	}
	placements, err := queryIDs(db, `SELECT id || '=' || placement || '|' || attention_kind || '|' || attention_reason || '|' || COALESCE(attention_at, 'NULL')
		FROM threads ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{
		"disc-child-a=standalone|||NULL", "disc-child-b=standalone|||NULL", "disc-parent=standalone|||NULL", "fork=standalone|||NULL",
		"grp-child=grouped|||NULL", "grp-parent=grouped|||NULL", "scratch=standalone|||NULL",
	}; !slices.Equal(placements, want) {
		t.Errorf("placements after v154:\n got %q\nwant %q", placements, want)
	}
	scratch, err := queryIDs(db, `SELECT thread_id || '|' || source_thread_id || '|' || return_mode || '|' || created_at || '|' || request_token FROM scratch_threads`)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"scratch|disc-parent|chat|3|tok"}; !slices.Equal(scratch, want) {
		t.Errorf("scratch_threads after v154 = %v, want %v", scratch, want)
	}
	favorites, err := queryIDs(db, `SELECT kind || '|' || provider || '|' || value || '|' || label || '|' || created_at FROM chat_bar_favorites`)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"model|claude|opus|Opus|2"}; !slices.Equal(favorites, want) {
		t.Errorf("chat_bar_favorites after v154 = %v, want %v", favorites, want)
	}
	if got := renderWork(); !slices.Equal(got, wantWork) {
		t.Errorf("work_items rows after v154:\n got %q\nwant %q", got, wantWork)
	}
	if got := renderWatches(); !slices.Equal(got, wantWatches) {
		t.Errorf("remote_watches rows after v154:\n got %q\nwant %q", got, wantWatches)
	}
	watches, err := queryIDs(db, `SELECT request_id || '|' || COALESCE(confirmed_at, 'NULL') || '|' || wake_attempts || '|' || wake_issue || '|' || launched_in_thread_id
		FROM remote_watches ORDER BY request_id`)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"dismissed|NULL|0||fork", "pending|NULL|0||fork", "queued|6|0||disc-child-a"}; !slices.Equal(watches, want) {
		t.Errorf("remote_watches new columns after v154 = %q, want %q", watches, want)
	}

	// The retired objects are gone and nothing else is: every schema object
	// other than the rebuilt tables keeps its SQL, the rebuilt tables are
	// compared declaration by declaration below, and the one new object is
	// the finished-watch index.
	schemaAfter := migrationSchema(t, db)
	for name, sqlText := range schemaBefore {
		if removed(name) {
			if _, ok := schemaAfter[name]; ok {
				t.Errorf("%s survived v154", name)
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
	var added []string
	for name := range schemaAfter {
		if _, ok := schemaBefore[name]; !ok {
			added = append(added, name)
		}
	}
	slices.Sort(added)
	if !slices.Equal(added, addedObjects) {
		t.Errorf("v154 added %v, want %v", added, addedObjects)
	}
	if got := readIndexSQL(t, db, "idx_remote_watches_done"); !strings.Contains(got, "notification = 'dismissed' OR (notification = 'queued' AND confirmed_at IS NOT NULL)") {
		t.Errorf("idx_remote_watches_done = %q", got)
	}

	// Each rebuilt table restates v149's declarations less the retired
	// parts: the removed columns and their foreign keys, 'discussion' and
	// 'automation' in each IN list, the composite favorites CHECK, whose
	// model arm becomes a provider CHECK, and provider's DEFAULT '', which
	// only discussion favorites used. threads and remote_watches append the
	// columns, CHECKs and foreign key the standing agents need.
	retired := strings.NewReplacer(",'discussion'", "", "'discussion',", "", ",'automation'", "")
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
		wantColumns = append(wantColumns, addedColumns[table]...)
		if !slices.Equal(after.columns, wantColumns) {
			t.Errorf("%s columns after v154:\n got %+v\nwant %+v", table, after.columns, wantColumns)
		}

		wantChecks := make([]string, 0, len(before.checks))
		for _, check := range before.checks {
			if table == "chat_bar_favorites" && strings.Contains(check, "kind='discussion'") {
				check = "providerIN('claude','codex','claude-tui')"
			}
			wantChecks = append(wantChecks, retired.Replace(check))
		}
		if table != "remote_watches" && slices.Equal(wantChecks, before.checks) {
			t.Fatalf("%s CHECKs before v154 never name a retired value; the fixture does not exercise the change: %q", table, before.checks)
		}
		wantChecks = append(wantChecks, addedChecks[table]...)
		slices.Sort(wantChecks)
		if !slices.Equal(after.checks, wantChecks) {
			t.Errorf("%s CHECKs after v154:\n got %q\nwant %q", table, after.checks, wantChecks)
		}

		wantFKs := slices.DeleteFunc(slices.Clone(before.fks), func(fk string) bool {
			return slices.ContainsFunc(removedColumns, func(column string) bool {
				return strings.HasPrefix(fk, column+" ")
			})
		})
		wantFKs = append(wantFKs, addedFKs[table]...)
		slices.Sort(wantFKs)
		if !slices.Equal(after.fks, wantFKs) {
			t.Errorf("%s foreign keys after v154 = %q, want %q", table, after.fks, wantFKs)
		}
	}

	// Each CHECK that named a retired value refuses it now. The fixture's
	// own inserts show each accepted it at v149.
	for _, refused := range []struct{ name, query string }{
		{"threads.mode", `UPDATE threads SET mode = 'discussion' WHERE id = 'disc-child-a'`},
		{"scratch_threads.return_mode", `UPDATE scratch_threads SET return_mode = 'discussion' WHERE thread_id = 'scratch'`},
		{"chat_bar_favorites.kind", `INSERT INTO chat_bar_favorites(kind,provider,value,label,created_at) VALUES('discussion','claude','def','Discussion favorite',4)`},
		{"work_items.source", `UPDATE work_items SET source = 'automation' WHERE id = 'run-auto'`},
		{"grouped without a group", `UPDATE threads SET placement = 'grouped' WHERE id = 'fork'`},
		{"a group while standalone", `UPDATE threads SET group_id = 'g' WHERE id = 'fork'`},
		{"a group while owned", `UPDATE threads SET placement = 'owned' WHERE id = 'grp-child'`},
		{"the retired nested value", `UPDATE threads SET placement = 'nested' WHERE id = 'fork'`},
		{"an unknown placement", `UPDATE threads SET placement = 'floating' WHERE id = 'fork'`},
		{"an unknown attention kind", `UPDATE threads SET attention_kind = 'urgent' WHERE id = 'fork'`},
		{"a half-linked call", `UPDATE work_items SET parent_phase_id = '' WHERE id = 'run-call'`},
	} {
		if _, err := db.Exec(refused.query); err == nil || !strings.Contains(err.Error(), "CHECK constraint failed") {
			t.Errorf("%s after v154: err = %v, want a CHECK violation", refused.name, err)
		}
	}
	mustExec(t, db, `UPDATE threads SET mode = 'plan' WHERE id = 'disc-child-a'`)
	mustExec(t, db, `UPDATE scratch_threads SET return_mode = 'plan' WHERE thread_id = 'scratch'`)
	mustExec(t, db, `INSERT INTO chat_bar_favorites(kind,provider,value,label,created_at) VALUES('model','codex','gpt','GPT',4)`)
	for _, kind := range AttentionKinds() {
		mustExec(t, db, `UPDATE threads SET attention_kind = ?, attention_at = 5 WHERE id = 'fork'`, kind)
	}
	mustExec(t, db, `UPDATE threads SET group_id = 'g', placement = 'grouped' WHERE id = 'fork'`)
	mustExec(t, db, `UPDATE threads SET group_id = NULL, placement = 'standalone' WHERE id = 'fork'`)
	mustExec(t, db, `UPDATE work_items SET source = 'agent', source_ref = 'item/phase/other' WHERE id = 'run-auto'`)
	if _, err := db.Exec(`UPDATE work_items SET source_ref = 'item/phase/key' WHERE id = 'run-auto'`); err == nil || !strings.Contains(err.Error(), "UNIQUE constraint failed") {
		t.Errorf("a duplicate agent source_ref after v154: err = %v, want the unique index to refuse it", err)
	}
	var pinnedAt, pinGroup sql.NullInt64
	if err := db.QueryRow(`SELECT pinned_at, pin_group FROM threads WHERE id = 'grp-child'`).Scan(&pinnedAt, &pinGroup); err != nil {
		t.Fatal(err)
	}
	if pinnedAt.Int64 != 5 || pinGroup.Int64 != 1 {
		t.Errorf("a grouped member's pin = %v/%v, want it kept", pinnedAt, pinGroup)
	}

	// Foreign keys into threads: the same set less the two v154 removed and
	// with the watches' owner, clean, and enforced on the rebuilt tables.
	wantFKs := slices.DeleteFunc(slices.Clone(fksBefore), func(fk string) bool {
		return fk == "channels.thread_id" || fk == "threads.parent_thread_id"
	})
	if len(wantFKs) != len(fksBefore)-2 {
		t.Fatalf("foreign keys into threads before v154 = %v, want channels.thread_id and threads.parent_thread_id among them", fksBefore)
	}
	wantFKs = append(wantFKs, "remote_watches.thread_id")
	slices.Sort(wantFKs)
	if got := foreignKeysInto(t, db, "threads"); !slices.Equal(got, wantFKs) {
		t.Errorf("foreign keys into threads after v154 = %v, want %v", got, wantFKs)
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
	if _, err := db.Exec(`UPDATE threads SET group_id = 'missing', placement = 'grouped' WHERE id = 'disc-child-b'`); err == nil {
		t.Error("a thread joined a missing group; the group_id foreign key is missing")
	}
	if _, err := db.Exec(`INSERT INTO remote_watches(computer_id,request_id,thread_id,fingerprint,label,notification,created_at)
		VALUES('pc','late','deleted-thread','f','job','pending',1)`); err == nil {
		t.Error("a watch named a missing thread; the thread_id foreign key is missing")
	}
	if _, err := db.Exec(`DELETE FROM threads WHERE id = 'disc-child-a'`); err == nil || !strings.Contains(err.Error(), "kept as a holder") {
		t.Errorf("deleting a thread a fork reads = %v, want the fork source trigger's refusal", err)
	}
	mustExec(t, db, `DELETE FROM threads WHERE id = 'scratch'`)
	mustExec(t, db, `DELETE FROM threads WHERE id = 'fork'`)
	var orphans int
	if err := db.QueryRow(`SELECT (SELECT COUNT(*) FROM scratch_threads WHERE thread_id = 'scratch') + (SELECT COUNT(*) FROM items WHERE thread_id = 'scratch')
		+ (SELECT COUNT(*) FROM remote_watches WHERE thread_id = 'fork')`).Scan(&orphans); err != nil {
		t.Fatal(err)
	}
	if orphans != 0 {
		t.Errorf("deleting a thread left %d scratch, item or watch rows; the cascades into threads are missing", orphans)
	}

	var owned int
	if err := db.QueryRow(`SELECT COUNT(*) FROM owned_threads`).Scan(&owned); err != nil {
		t.Fatalf("read owned_threads after v154: %v", err)
	}
	if want := len(wantRows) - 2; owned != want {
		t.Errorf("owned_threads = %d rows, want %d", owned, want)
	}
}

// seedV152DeliveryRows writes, on a store at v152 or v154, the rows by which
// main's boot told a queued message delivered (app.queuedSendDelivered and
// store.ListUnconfirmedUserRows at v152) for the send ids sendOf(name)
// names, on threads "caller" and "crashed":
//
//   - delivered: no queue row and no marked row; delivered.
//   - queued-row: its queue row is left and no row carries it; undelivered.
//   - queued-echoed: its queue row is left beside a row carrying the
//     provider's id; delivered.
//   - queued-unechoed: its queue row is left beside a row without the
//     provider's id (an interrupt revealed it); undelivered.
//   - residue: a marked row in the last turn of "crashed" carries it;
//     undelivered.
//   - residue-joined: a member of a marked joined row there; undelivered.
//   - history: a marked row a later turn follows carries it; delivered.
//   - elsewhere: a queue row with its send id is on another thread; delivered.
func seedV152DeliveryRows(t *testing.T, db *sql.DB, sendOf func(name string) string) {
	t.Helper()
	mustExec(t, db, `INSERT INTO projects(id,path,name,created_at,updated_at) VALUES('p','/p','p',1,1)`)
	mustExec(t, db, `INSERT INTO threads(id,project_id,title,provider,model,workspace_path,created_at,updated_at,archived) VALUES
		('caller','p','Caller','claude','opus','/p',1,1,0), ('crashed','p','Crashed','claude','opus','/p',1,1,0)`)
	index := 0
	row := func(thread, id string, turn int, meta map[string]any) {
		t.Helper()
		encoded, err := json.Marshal(meta)
		if err != nil {
			t.Fatal(err)
		}
		index++
		mustExec(t, db, `INSERT INTO items(id,thread_id,turn_index,item_index,kind,role,status,summary,meta,created_at,updated_at)
			VALUES(?,?,?,?,'user_text','user','completed','text',?,1,1)`, id, thread, turn, index, string(encoded))
	}
	queued := func(thread, name string) {
		t.Helper()
		mustExec(t, db, `INSERT INTO flush_queue_items(id,thread_id,send_id,message,enqueued_at) VALUES(?,?,?,'text',1)`,
			"queue-"+name+"-"+thread, thread, sendOf(name))
	}
	row("caller", "row-delivered", 1, map[string]any{"sendId": sendOf("delivered"), "provider_item_id": "wire-1"})
	queued("caller", "queued-row")
	queued("caller", "queued-echoed")
	row("caller", "row-queued-echoed", 2, map[string]any{"sendId": sendOf("queued-echoed"), "provider_item_id": "wire-2"})
	queued("caller", "queued-unechoed")
	row("caller", "row-queued-unechoed", 3, map[string]any{"sendId": sendOf("queued-unechoed"), "promoted_at_interrupt": true})
	row("caller", "row-history", 4, map[string]any{"sendId": sendOf("history"), "pendingFlush": true})
	row("caller", "row-later", 5, map[string]any{"sendId": "person-later"})
	queued("crashed", "elsewhere")
	row("crashed", "row-residue", 7, map[string]any{"sendId": sendOf("residue"), "pendingFlush": true})
	row("crashed", "row-residue-joined", 7, map[string]any{"sendId": "person", "pendingFlush": true,
		"joinedSendIds": []string{"person", sendOf("residue-joined")}})
}

// v154 records a completion queued before it as confirmed only when main
// read it delivered: its queue row gone, or a row carrying the provider's id
// beside it, and no row main restores as unconfirmed carrying it. Any other
// is left unconfirmed, for the boot to give back to its watch.
func TestMigrationV154ConfirmsOnlyTheCompletionsMainDelivered(t *testing.T) {
	db := migrateThrough(t, retireFeaturesMigrationVersion-1)
	sendOf := func(name string) string { return "remote-completion:pc:" + name }
	seedV152DeliveryRows(t, db, sendOf)
	watch := func(name, thread, notification string) {
		t.Helper()
		mustExec(t, db, `INSERT INTO remote_watches(computer_id,request_id,thread_id,fingerprint,label,notification,created_at)
			VALUES('pc',?,?,'f','job',?,9)`, name, thread, notification)
	}
	for _, name := range []string{"delivered", "queued-row", "queued-echoed", "queued-unechoed", "history", "elsewhere"} {
		watch(name, "caller", "queued")
	}
	watch("residue", "crashed", "queued")
	watch("residue-joined", "crashed", "queued")
	watch("pending", "caller", "pending")

	migrateFromThrough(t, db, retireFeaturesMigrationVersion-1, retireFeaturesMigrationVersion)

	got, err := queryIDs(db, `SELECT request_id || '|' || notification || '|' || COALESCE(confirmed_at, 'NULL') FROM remote_watches ORDER BY request_id`)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"delivered|queued|9", "elsewhere|queued|9", "history|queued|9", "pending|pending|NULL",
		"queued-echoed|queued|9", "queued-row|queued|NULL", "queued-unechoed|queued|NULL", "residue|queued|NULL", "residue-joined|queued|NULL"}
	if !slices.Equal(got, want) {
		t.Errorf("watches after v154:\n got %q\nwant %q", got, want)
	}
}

// renderRows renders the named columns of every row of table, ordered by
// orderBy.
func renderRows(t *testing.T, db sqlQueryer, table string, columns []string, orderBy string) []string {
	t.Helper()
	parts := make([]string, len(columns))
	for i, column := range columns {
		parts[i] = "'" + column + "=' || quote(" + column + ")"
	}
	rows, err := queryIDs(db, `SELECT `+strings.Join(parts, ` || '|' || `)+` FROM `+table+` ORDER BY `+orderBy)
	if err != nil {
		t.Fatal(err)
	}
	return rows
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
