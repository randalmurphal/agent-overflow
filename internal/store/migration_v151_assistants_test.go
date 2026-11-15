package store

import (
	"database/sql"
	"errors"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"
)

// openFileDBThrough opens a file database migrated through target on one
// connection.
func openFileDBThrough(t *testing.T, target int) (*sql.DB, string) {
	t.Helper()
	path := fileDBThrough(t, target)
	db, err := sql.Open("sqlite", poolDSN(path, writerConnPragmas))
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	return db, path
}

func applyAssistantsMigration(t *testing.T, db *sql.DB) {
	t.Helper()
	if err := applyMigration(db, migrationByVersion(t, assistantsMigrationVersion)); err != nil {
		t.Fatalf("apply v%d: %v", assistantsMigrationVersion, err)
	}
}

// v151 over a populated database: every existing row and every schema
// object but the two ledger tables is unchanged; the new tables have exactly
// their columns, constraints and indexes and cascade as the spec says; the
// ledger gains its columns at the end, its indexes and its triggers; and the
// placement trigger ends ownership with the last link, except for a thread
// outside every project, an assistant's helper.
func TestMigrationV151Schema(t *testing.T) {
	db, _ := openFileDBThrough(t, assistantsMigrationVersion-1)
	mustExec(t, db, `INSERT INTO projects (id, path, name, created_at, updated_at) VALUES ('p', '/p', 'p', 1, 1)`)
	mustExec(t, db, `INSERT INTO threads (id, project_id, title, provider, workspace_path, model, created_at, updated_at, archived) VALUES
		('lead', 'p', 'Lead', 'claude', '/p', '', 1, 1, 0),
		('worker', 'p', 'Worker', 'codex', '/p', '', 1, 2, 0),
		('next', 'p', 'Next', 'claude', '/p', '', 1, 3, 0),
		('helper', NULL, 'Helper', 'claude', '/h/lead', '', 1, 4, 0)`)
	mustExec(t, db, `UPDATE threads SET placement = 'owned' WHERE id IN ('worker', 'helper')`)
	mustExec(t, db, `INSERT INTO thread_requests (token, caller_thread_id, kind, target_thread_id, state, created_at, updated_at) VALUES
		('spawn', 'lead', 'spawn', 'worker', 'finished', 5, 5),
		('spawn-helper', 'lead', 'spawn', 'helper', 'finished', 6, 6)`)
	threadsBefore := migrationThreadRows(t, db)
	ledger := []string{"thread_requests", "thread_request_receipts"}
	columnsBefore := map[string][]string{}
	for _, table := range ledger {
		columns, err := queryIDs(db, `SELECT name FROM pragma_table_info(?) ORDER BY cid`, table)
		if err != nil {
			t.Fatal(err)
		}
		columnsBefore[table] = columns
	}
	schemaBefore := migrationSchema(t, db)

	applyAssistantsMigration(t, db)

	if got := migrationThreadRows(t, db); !slices.Equal(got, threadsBefore) {
		t.Errorf("thread rows changed:\n got %q\nwant %q", got, threadsBefore)
	}
	schemaAfter := migrationSchema(t, db)
	for name, sqlText := range schemaBefore {
		if slices.Contains(ledger, name) {
			continue
		}
		if schemaAfter[name] != sqlText {
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
	if want := []string{
		"agent_helpers", "agent_past_threads", "agent_projects", "agent_wakes", "agents",
		"idx_agent_helpers_agent", "idx_agent_past_threads_agent", "idx_agent_projects_project", "idx_agent_wakes_due", "idx_agent_wakes_thread",
		"idx_thread_request_receipts_age", "idx_thread_request_receipts_lineage", "idx_thread_requests_age", "idx_thread_requests_caller_settled", "idx_thread_requests_lineage",
		"idx_thread_requests_nested_caller", "idx_thread_requests_nested_open", "idx_thread_requests_open_below", "idx_thread_requests_open_created", "idx_thread_requests_open_deep", "idx_thread_requests_remote_rest",
		"idx_thread_requests_spawn_link", "idx_thread_requests_spawn_target",
		"session_start_failures",
		"sqlite_autoindex_agent_helpers_1", "sqlite_autoindex_agent_past_threads_1",
		"sqlite_autoindex_agent_wakes_1", "sqlite_autoindex_agents_1", "sqlite_autoindex_agents_2", "sqlite_autoindex_session_start_failures_1",
		"trg_thread_placement_spawn_gone", "trg_thread_requests_open_below_delete", "trg_thread_requests_open_below_insert",
		"trg_thread_requests_open_below_update", "trg_thread_requests_spawn_cycle_insert", "trg_thread_requests_spawn_cycle_update",
	}; !slices.Equal(added, want) {
		t.Errorf("v%d added\n %v\nwant\n %v", assistantsMigrationVersion, added, want)
	}

	// name|type|not null|default|primary key, in column order.
	columns := func(table string) []string {
		t.Helper()
		got, err := queryIDs(db, `SELECT name || '|' || type || '|' || "notnull" || '|' || COALESCE(dflt_value, 'NULL') || '|' || pk
			FROM pragma_table_info(?) ORDER BY cid`, table)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	for table, want := range map[string][]string{
		"agents": {
			"id|TEXT|0|NULL|1", "thread_id|TEXT|1|NULL|0", "home|TEXT|1|NULL|0", "creator_agent_id|TEXT|1|''|0",
			"created_at|INTEGER|1|NULL|0", "all_projects|INTEGER|1|0|0",
		},
		"agent_projects":     {"agent_id|TEXT|1|NULL|1", "project_id|TEXT|1|NULL|2"},
		"agent_helpers":      {"thread_id|TEXT|0|NULL|1", "agent_id|TEXT|1|NULL|0"},
		"agent_past_threads": {"thread_id|TEXT|0|NULL|1", "agent_id|TEXT|1|NULL|0", "superseded_at|INTEGER|1|NULL|0", "moved_draft|TEXT|1|''|0", "moved_draft_attachments|TEXT|1|'[]'|0"},
		"agent_wakes": {
			"id|TEXT|0|NULL|1", "thread_id|TEXT|1|NULL|0", "kind|TEXT|1|NULL|0", "label|TEXT|1|NULL|0", "note|TEXT|1|''|0",
			"command|TEXT|1|''|0", "cwd|TEXT|1|''|0", "interval_seconds|INTEGER|0|NULL|0", "timeout_seconds|INTEGER|0|NULL|0",
			"long_running|INTEGER|1|0|0", "next_at|INTEGER|0|NULL|0", "last_run_at|INTEGER|0|NULL|0", "last_outcome|TEXT|1|''|0",
			"last_output|TEXT|1|''|0", "undelivered|TEXT|1|''|0", "missed|INTEGER|1|0|0", "state|TEXT|1|NULL|0",
			"created_at|INTEGER|1|NULL|0", "last_stderr|TEXT|1|''|0",
		},
		"session_start_failures": {"thread_id|TEXT|0|NULL|1", "failed_at|INTEGER|1|NULL|0"},
	} {
		if got := columns(table); !slices.Equal(got, want) {
			t.Errorf("%s columns:\n got %q\nwant %q", table, got, want)
		}
	}
	addedColumns := map[string][]string{
		"thread_requests": {
			"lineage_only|INTEGER|1|0|0", "superseded|INTEGER|1|0|0", "confirmed_at|INTEGER|0|NULL|0", "late_confirmed_at|INTEGER|0|NULL|0",
			"answer_agent|INTEGER|1|0|0", "answer_agent_role|TEXT|1|''|0", "late_reply_agent|INTEGER|1|0|0",
			"late_reply_agent_role|TEXT|1|''|0", "nested_open|INTEGER|1|0|0", "target_provider|TEXT|1|''|0",
			"late_delivered_how|TEXT|1|''|0", "made_in_thread_id|TEXT|1|''|0", "wake_item_id|TEXT|1|''|0",
			"late_wake_item_id|TEXT|1|''|0", "open_below_0|INTEGER|1|0|0", "open_below_1|INTEGER|1|0|0",
			"open_below_2|INTEGER|1|0|0", "open_below_3|INTEGER|1|0|0",
		},
		"thread_request_receipts": {
			"answer_agent|INTEGER|1|0|0", "answer_agent_role|TEXT|1|''|0", "late_reply_agent|INTEGER|1|0|0",
			"late_reply_agent_role|TEXT|1|''|0", "own_turn|INTEGER|1|0|0", "lineage_only|INTEGER|1|0|0",
		},
	}
	for _, table := range ledger {
		got := columns(table)
		if len(got) < len(columnsBefore[table]) {
			t.Fatalf("%s lost columns: %q", table, got)
		}
		for i, name := range columnsBefore[table] {
			if !strings.HasPrefix(got[i], name+"|") {
				t.Errorf("%s column %d = %q, want %s kept in place", table, i, got[i], name)
			}
		}
		if tail := got[len(columnsBefore[table]):]; !slices.Equal(tail, addedColumns[table]) {
			t.Errorf("%s added columns:\n got %q\nwant %q", table, tail, addedColumns[table])
		}
	}
	for table, want := range map[string][]string{
		"agents":                 {"threads.id|CASCADE"},
		"agent_projects":         {"agents.id|CASCADE", "projects.id|CASCADE"},
		"agent_helpers":          {"agents.id|CASCADE", "threads.id|CASCADE"},
		"agent_past_threads":     {"agents.id|CASCADE", "threads.id|CASCADE"},
		"agent_wakes":            {"threads.id|CASCADE"},
		"session_start_failures": {"threads.id|CASCADE"},
	} {
		got, err := queryIDs(db, `SELECT "table" || '.' || "to" || '|' || on_delete FROM pragma_foreign_key_list(?) ORDER BY 1`, table)
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(got, want) {
			t.Errorf("%s foreign keys = %q, want %q", table, got, want)
		}
	}
	for index, want := range map[string][]string{
		"idx_agent_past_threads_agent": {"agent_id", "superseded_at", "thread_id"},
		"idx_agent_wakes_due":          {"state", "next_at"},
		"idx_agent_wakes_thread":       {"thread_id", "state"},
	} {
		if got, err := queryIDs(db, `SELECT name FROM pragma_index_info(?) ORDER BY seqno`, index); err != nil || !slices.Equal(got, want) {
			t.Errorf("%s = %q %v, want %q", index, got, err, want)
		}
	}

	for _, bad := range []string{
		`INSERT INTO agents (id, thread_id, home, created_at) VALUES ('x', 'missing', '/h', 1)`,
		`INSERT INTO agents (id, thread_id, home, all_projects, created_at) VALUES ('x', 'worker', '/h', 2, 1)`,
		`INSERT INTO agent_wakes (id, thread_id, kind, label, state, created_at) VALUES ('x', 'lead', 'cron', 'l', 'armed', 1)`,
		`INSERT INTO agent_wakes (id, thread_id, kind, label, state, created_at) VALUES ('x', 'lead', 'loop', 'l', 'paused', 1)`,
		`INSERT INTO agent_wakes (id, thread_id, kind, label, long_running, state, created_at) VALUES ('x', 'lead', 'watch', 'l', 2, 'armed', 1)`,
		`INSERT INTO agent_wakes (id, thread_id, kind, label, state, created_at) VALUES ('x', 'missing', 'loop', 'l', 'armed', 1)`,
		`UPDATE thread_requests SET lineage_only = 2 WHERE token = 'spawn'`,
		`UPDATE thread_requests SET superseded = 2 WHERE token = 'spawn'`,
		`UPDATE thread_requests SET answer_agent = NULL WHERE token = 'spawn'`,
		`UPDATE thread_requests SET late_reply_agent = 2 WHERE token = 'spawn'`,
		`UPDATE thread_requests SET nested_open = 2 WHERE token = 'spawn'`,
		`UPDATE thread_requests SET late_delivered_how = 'mail' WHERE token = 'spawn'`,
	} {
		if _, err := db.Exec(bad); err == nil {
			t.Errorf("accepted %s", bad)
		}
	}
	mustExec(t, db, `INSERT INTO agents (id, thread_id, home, created_at) VALUES ('lead', 'lead', '/h/lead', 1)`)
	if _, err := db.Exec(`INSERT INTO agents (id, thread_id, home, created_at) VALUES ('dup', 'lead', '/h', 1)`); err == nil {
		t.Error("two assistants took one current thread")
	}
	mustExec(t, db, `INSERT INTO agent_projects (agent_id, project_id) VALUES ('lead', 'p')`)
	mustExec(t, db, `INSERT INTO agent_wakes (id, thread_id, kind, label, state, created_at) VALUES ('w', 'lead', 'loop', 'board', 'armed', 1)`)
	mustExec(t, db, `INSERT INTO session_start_failures (thread_id, failed_at) VALUES ('lead', 1)`)

	// The spawner's delete takes its spawn rows, which ends the worker's
	// ownership and leaves the helper's.
	mustExec(t, db, `DELETE FROM thread_requests WHERE token IN ('spawn', 'spawn-helper')`)
	for id, want := range map[string]string{"worker": PlacementStandalone, "helper": PlacementOwned} {
		var placement string
		if err := db.QueryRow(`SELECT placement FROM threads WHERE id = ?`, id).Scan(&placement); err != nil || placement != want {
			t.Errorf("%s without its spawn = %q %v, want %s", id, placement, err, want)
		}
	}

	// A past thread's delete leaves the assistant; the current thread's
	// delete takes the assistant, its projects, wakes and mark, and leaves
	// its other past threads as ordinary threads.
	mustExec(t, db, `UPDATE agents SET thread_id = 'next' WHERE id = 'lead'`)
	mustExec(t, db, `INSERT INTO agent_past_threads (thread_id, agent_id, superseded_at) VALUES ('lead', 'lead', 30), ('worker', 'lead', 31)`)
	mustExec(t, db, `DELETE FROM threads WHERE id = 'worker'`)
	if got, err := queryIDs(db, `SELECT thread_id FROM agent_past_threads ORDER BY thread_id`); err != nil || !slices.Equal(got, []string{"lead"}) {
		t.Errorf("past threads after deleting one = %q %v", got, err)
	}
	mustExec(t, db, `DELETE FROM threads WHERE id = 'next'`)
	var left int
	if err := db.QueryRow(`SELECT (SELECT COUNT(*) FROM agents) + (SELECT COUNT(*) FROM agent_projects) + (SELECT COUNT(*) FROM agent_past_threads)`).Scan(&left); err != nil || left != 0 {
		t.Errorf("after deleting the current thread %d assistant rows are left, %v", left, err)
	}
	mustExec(t, db, `DELETE FROM threads WHERE id = 'lead'`)
	if err := db.QueryRow(`SELECT (SELECT COUNT(*) FROM agent_wakes) + (SELECT COUNT(*) FROM session_start_failures)`).Scan(&left); err != nil || left != 0 {
		t.Errorf("after deleting their thread %d wake or start-failure rows are left, %v", left, err)
	}
}

// v151 over a populated ledger: every answer and late reply delivered
// before it, by any route, reads confirmed at its delivery time; a late
// reply delivered before it carries the route its delivery wrote; every
// request and receipt reads as answered by no agent, made by its caller,
// with nothing open under a remote target of unknown provider, and every
// receipt as delivered into a turn it did not open; no existing column
// changes.
func TestMigrationV151Deliveries(t *testing.T) {
	db, _ := openFileDBThrough(t, assistantsMigrationVersion-1)
	mustExec(t, db, `INSERT INTO projects (id, path, name, created_at, updated_at) VALUES ('p', '/p', 'p', 1, 1)`)
	mustExec(t, db, `INSERT INTO threads (id, project_id, title, provider, workspace_path, model, created_at, updated_at, archived) VALUES
		('caller', 'p', 'Caller', 'claude', '/p', '', 1, 1, 0)`)
	mustExec(t, db, `INSERT INTO thread_requests (token, caller_thread_id, kind, target_computer_id, target_thread_id, target_thread_title, state, answer,
		settled_at, delivered_at, delivered_how, late_reply, late_delivered_at, created_at, updated_at) VALUES
		('queued', 'caller', 'send', '', 't', '', 'finished', 'a', 10, 20, 'queued', NULL, NULL, 1, 1),
		('inline', 'caller', 'send', '', 't', '', 'finished', 'a', 10, 30, 'inline', NULL, NULL, 1, 1),
		('draft', 'caller', 'send', '', 't', '', 'finished', 'a', 10, 40, 'draft', NULL, NULL, 1, 1),
		('late-inline', 'caller', 'send', '', 't', '', 'finished', 'a', 10, 50, 'inline', 'b', 60, 1, 1),
		('late-owed', 'caller', 'send', '', 't', '', 'finished', 'a', 10, 50, 'queued', 'b', NULL, 1, 1),
		('owed', 'caller', 'send', '', 't', '', 'finished', 'a', 10, NULL, '', NULL, NULL, 1, 1),
		('remote', 'caller', 'spawn', 'c', 't', 'Their thread', 'running', NULL, NULL, NULL, '', NULL, NULL, 1, 1),
		('open', 'caller', 'ask', '', '', '', 'running', NULL, NULL, NULL, '', NULL, NULL, 1, 1)`)
	mustExec(t, db, `INSERT INTO thread_request_receipts (token, owner_device_id, kind, target_thread_id, message_item_id, turn_id, state, answer, settled_at, created_at, updated_at) VALUES
		('bound', 'local', 'send', 'caller', 'user:0', 'caller:0', 'running', NULL, NULL, 1, 1),
		('settled', 'local', 'send', 'caller', 'user:1', 'caller:1', 'finished', 'a', 10, 1, 1),
		('unbound', 'local', 'send', 'caller', '', '', 'accepted', NULL, NULL, 1, 1)`)
	render := func(table, key string) []string {
		t.Helper()
		columns, err := queryIDs(db, `SELECT name FROM pragma_table_info(?) ORDER BY cid`, table)
		if err != nil {
			t.Fatal(err)
		}
		return renderRows(t, db, table, columns, key)
	}
	requestsBefore, receiptsBefore := render("thread_requests", "token"), render("thread_request_receipts", "token")
	requestColumns, err := queryIDs(db, `SELECT name FROM pragma_table_info('thread_requests') ORDER BY cid`)
	if err != nil {
		t.Fatal(err)
	}
	receiptColumns, err := queryIDs(db, `SELECT name FROM pragma_table_info('thread_request_receipts') ORDER BY cid`)
	if err != nil {
		t.Fatal(err)
	}

	applyAssistantsMigration(t, db)

	if got := renderRows(t, db, "thread_requests", requestColumns, "token"); !slices.Equal(got, requestsBefore) {
		t.Errorf("thread_requests changed:\n got %q\nwant %q", got, requestsBefore)
	}
	if got := renderRows(t, db, "thread_request_receipts", receiptColumns, "token"); !slices.Equal(got, receiptsBefore) {
		t.Errorf("thread_request_receipts changed:\n got %q\nwant %q", got, receiptsBefore)
	}
	got, err := queryIDs(db, `SELECT token || '|' || COALESCE(confirmed_at, 'NULL') || '|' || COALESCE(late_confirmed_at, 'NULL') || '|' || late_delivered_how
		FROM thread_requests ORDER BY token`)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{
		"draft|40|NULL|", "inline|30|NULL|", "late-inline|50|60|inline", "late-owed|50|NULL|",
		"open|NULL|NULL|", "owed|NULL|NULL|", "queued|20|NULL|", "remote|NULL|NULL|",
	}; !slices.Equal(got, want) {
		t.Errorf("confirmations and late routes:\n got %q\nwant %q", got, want)
	}
	got, err = queryIDs(db, `SELECT DISTINCT lineage_only || '|' || answer_agent || '|' || answer_agent_role || '|' || late_reply_agent || '|' ||
		late_reply_agent_role || '|' || nested_open || '|' || target_provider || '|' || made_in_thread_id || '|' || wake_item_id || '|' ||
		late_wake_item_id || '|' || open_below_0 || open_below_1 || open_below_2 || open_below_3 FROM thread_requests`)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"0|0||0||0||||" + "|0000"}; !slices.Equal(got, want) {
		t.Errorf("new request columns = %q, want %q", got, want)
	}
	got, err = queryIDs(db, `SELECT DISTINCT answer_agent || '|' || answer_agent_role || '|' || late_reply_agent || '|' || late_reply_agent_role || '|' || own_turn
		FROM thread_request_receipts`)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"0||0||0"}; !slices.Equal(got, want) {
		t.Errorf("new receipt columns = %q, want %q", got, want)
	}
}

// v151 over a populated database: a confirmed queued answer or late reply
// gets the row the send-id lookup finds, a joined row included, and a
// request that rode with it gets the same row; an undelivered, inline or
// row-less one gets none.
func TestMigrationV151WakeItems(t *testing.T) {
	db, _ := openFileDBThrough(t, assistantsMigrationVersion-1)
	mustExec(t, db, `INSERT INTO projects (id, path, name, created_at, updated_at) VALUES ('p', '/p', 'p', 1, 1)`)
	mustExec(t, db, `INSERT INTO threads (id, project_id, title, provider, workspace_path, model, created_at, updated_at, archived) VALUES
		('caller', 'p', 'Caller', 'claude', '/p', '', 1, 1, 0), ('bulk', 'p', 'Bulk', 'claude', '/p', '', 1, 1, 0)`)
	userRow := func(threadID, id string, turn int, meta string) {
		t.Helper()
		mustExec(t, db, `INSERT INTO items (id, thread_id, turn_index, item_index, kind, role, status, summary, meta, created_at, updated_at)
			VALUES (?, ?, ?, 0, 'user_text', 'user', 'completed', 'answer', ?, 1, 1)`, id, threadID, turn, meta)
	}
	userRow("caller", "row-first", 1, `{"sendId":"thread-wake:first"}`)
	userRow("caller", "row-joined", 2, `{"sendId":"person","joinedSendIds":["person","thread-wake:joined"]}`)
	userRow("caller", "row-late", 3, `{"sendId":"thread-wake-late:late"}`)
	userRow("caller", "row-late-first", 4, `{"sendId":"thread-wake:late"}`)
	userRow("caller", "row-undelivered", 5, `{"sendId":"thread-wake:undelivered"}`)
	userRow("caller", "row-inline", 6, `{"sendId":"thread-wake:inline"}`)
	mustExec(t, db, `INSERT INTO thread_requests (token, caller_thread_id, kind, target_computer_id, target_thread_id, target_thread_title, state, answer,
		settled_at, delivered_at, delivered_how, late_reply, late_delivered_at, created_at, updated_at) VALUES
		('first', 'caller', 'send', '', 't', '', 'finished', 'a', 10, 20, 'queued', NULL, NULL, 1, 1),
		('joined', 'caller', 'send', '', 't', '', 'finished', 'a', 10, 20, 'queued', NULL, NULL, 1, 1),
		('late', 'caller', 'send', '', 't', '', 'finished', 'a', 10, 20, 'queued', 'b', 30, 1, 1),
		('undelivered', 'caller', 'send', '', 't', '', 'finished', 'a', 10, NULL, '', NULL, NULL, 1, 1),
		('inline', 'caller', 'send', '', 't', '', 'finished', 'a', 10, 20, 'inline', NULL, NULL, 1, 1),
		('reverted', 'caller', 'send', '', 't', '', 'finished', 'a', 10, 20, 'queued', NULL, NULL, 1, 1),
		('open', 'caller', 'ask', '', '', '', 'running', NULL, NULL, NULL, '', NULL, NULL, 1, 1)`)
	// Requests that rode with another's first wake (v149's delivered_with)
	// have no send id of their own: each takes its carrier's row, confirmed
	// with it, and one whose carrier's row was not found takes none.
	mustExec(t, db, `INSERT INTO thread_requests (token, caller_thread_id, kind, target_computer_id, target_thread_id, target_thread_title, state, answer,
		settled_at, delivered_at, delivered_how, delivered_with, created_at, updated_at) VALUES
		('rider', 'caller', 'send', '', 't2', '', 'finished', 'a', 10, 20, 'queued', 'first', 2, 2),
		('rider-joined', 'caller', 'send', '', 't2', '', 'finished', 'a', 10, 20, 'queued', 'joined', 2, 2),
		('rider-reverted', 'caller', 'send', '', 't2', '', 'finished', 'a', 10, 20, 'queued', 'reverted', 2, 2)`)
	seedConfirmedWakes(t, db, []string{"bulk"}, 3000)

	applyAssistantsMigration(t, db)

	got, err := queryIDs(db, `SELECT token || '|' || wake_item_id || '|' || late_wake_item_id FROM thread_requests
		WHERE caller_thread_id = 'caller' ORDER BY token`)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"first|row-first|", "inline||", "joined|row-joined|", "late|row-late-first|row-late", "open||", "reverted||",
		"rider|row-first|", "rider-joined|row-joined|", "rider-reverted||", "undelivered||"}
	if !slices.Equal(got, want) {
		t.Errorf("wake rows = %q, want %q", got, want)
	}
	riders, err := queryIDs(db, `SELECT token || '|' || COALESCE(confirmed_at, 'NULL') || '|' || delivered_with FROM thread_requests
		WHERE delivered_with <> '' ORDER BY token`)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"rider|20|first", "rider-joined|20|joined", "rider-reverted|20|reverted"}; !slices.Equal(riders, want) {
		t.Errorf("riders = %q, want each confirmed with its carrier recorded", riders)
	}
	var missing int
	if err := db.QueryRow(`SELECT COUNT(*) FROM thread_requests WHERE caller_thread_id = 'bulk' AND wake_item_id <> 'row-' || token`).Scan(&missing); err != nil || missing != 0 {
		t.Errorf("%d bulk answers without their row, %v", missing, err)
	}
}

// seedConfirmedWakes writes n queued answers delivered before v151 spread
// over callers, each with the user row its wake left, every tenth joined
// with another message.
func seedConfirmedWakes(t *testing.T, db *sql.DB, callers []string, n int) {
	t.Helper()
	list, err := jsonList(callers)
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, db, `BEGIN`)
	mustExec(t, db, `WITH RECURSIVE seq(i) AS (SELECT 0 UNION ALL SELECT i + 1 FROM seq WHERE i + 1 < ?1),
		wakes AS (SELECT i, printf('bulk-%06d', i) AS token,
		                 (SELECT value FROM json_each(?2) WHERE key = i % json_array_length(?2)) AS caller FROM seq)
		INSERT INTO items (id, thread_id, turn_index, item_index, kind, role, status, summary, meta, created_at, updated_at)
		SELECT 'row-' || token, caller, i + 1, 0, 'user_text', 'user', 'completed', 'answer',
		       CASE WHEN i % 10 = 0
		            THEN json_object('sendId', 'person-' || token, 'joinedSendIds', json_array('person-' || token, 'thread-wake:' || token))
		            ELSE json_object('sendId', 'thread-wake:' || token) END, 1, 1
		  FROM wakes`, n, list)
	mustExec(t, db, `WITH RECURSIVE seq(i) AS (SELECT 0 UNION ALL SELECT i + 1 FROM seq WHERE i + 1 < ?1)
		INSERT INTO thread_requests (token, caller_thread_id, kind, target_computer_id, target_thread_id, target_thread_title, state, answer,
			settled_at, delivered_at, delivered_how, created_at, updated_at)
		SELECT printf('bulk-%06d', i), (SELECT value FROM json_each(?2) WHERE key = i % json_array_length(?2)),
		       'send', '', 't', '', 'finished', 'a', 10, 20, 'queued', 1, 1 FROM seq`, n, list)
	mustExec(t, db, `COMMIT`)
}

// v151's wake-row fill is one statement per column and a keyed write per
// joined wake, so it runs at open: the whole migration over 50,000
// delivered answers (30 days of retention at 1,700 wakes a day) measured
// about 1.4 s alone, and each lookup probes its index by the caller's key,
// so a fill that scanned per request would exceed the bound many times over.
func TestMigrationV151WakeItemsAtScale(t *testing.T) {
	if testing.Short() {
		t.Skip("seeds 50,000 requests")
	}
	for _, n := range []int{3_000, 50_000} {
		path := fileDBThrough(t, assistantsMigrationVersion-1)
		db, err := sql.Open("sqlite", poolDSN(path, writerConnPragmas))
		if err != nil {
			t.Fatal(err)
		}
		db.SetMaxOpenConns(1)
		mustExec(t, db, `INSERT INTO projects (id, path, name, created_at, updated_at) VALUES ('p', '/p', 'p', 1, 1)`)
		callers := []string{"c0", "c1", "c2", "c3", "c4"}
		for _, id := range callers {
			mustExec(t, db, `INSERT INTO threads (id, project_id, title, provider, workspace_path, model, created_at, updated_at, archived)
				VALUES (?, 'p', ?, 'claude', '/p', '', 1, 1, 0)`, id, id)
		}
		seedConfirmedWakes(t, db, callers, n)
		start := time.Now()
		applyAssistantsMigration(t, db)
		elapsed := time.Since(start)
		t.Logf("v%d over %d delivered answers: %v", assistantsMigrationVersion, n, elapsed)
		var missing int
		if err := db.QueryRow(`SELECT COUNT(*) FROM thread_requests WHERE wake_item_id <> 'row-' || token`).Scan(&missing); err != nil || missing != 0 {
			t.Errorf("%d of %d answers without their row, %v", missing, n, err)
		}
		if elapsed > 5*time.Second {
			t.Errorf("v%d took %v over %d delivered answers; its fill belongs in a deferred phase", assistantsMigrationVersion, elapsed, n)
		}
		if err := db.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

// v151 over a populated ledger: the older of two local spawns of one thread
// becomes a superseded lineage row and the index refuses a second; every spawn's marks
// are filled from the open work below it, four threads down and no further;
// and the triggers keep them from there.
func TestMigrationV151AgentTreeMarks(t *testing.T) {
	db, _ := openFileDBThrough(t, assistantsMigrationVersion-1)
	mustExec(t, db, `INSERT INTO projects (id, path, name, created_at, updated_at) VALUES ('p', '/p', 'p', 1, 1)`)
	chain := []string{"t0", "t1", "t2", "t3", "t4", "t5"}
	for _, id := range append(chain, "side") {
		mustExec(t, db, `INSERT INTO threads (id, project_id, title, provider, workspace_path, model, created_at, updated_at, archived)
			VALUES (?, 'p', ?, 'claude', '/p', '', 1, 1, 0)`, id, id)
	}
	insert := func(token, caller, kind, target string, notify int, settled any, created int) {
		t.Helper()
		mustExec(t, db, `INSERT INTO thread_requests (token, caller_thread_id, kind, target_thread_id, notify, state, settled_at, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, CASE WHEN ? IS NULL THEN 'running' ELSE 'finished' END, ?, ?, ?)`,
			token, caller, kind, target, notify, settled, settled, created, created)
	}
	for i := 0; i+1 < len(chain); i++ {
		insert("spawn-"+chain[i+1], chain[i], ThreadRequestSpawn, chain[i+1], 0, 5, 10+i)
	}
	insert("old-side", "t0", ThreadRequestSpawn, "t3", 0, 5, 1)
	insert("open-t5", "t5", ThreadRequestSend, "side", 1, nil, 30)
	insert("open-t3", "t3", ThreadRequestSend, "side", 1, nil, 31)
	insert("quiet-t3", "t3", ThreadRequestSend, "side", 0, nil, 32)
	insert("done-t2", "t2", ThreadRequestSend, "side", 1, 5, 33)

	applyAssistantsMigration(t, db)

	var marks string
	if err := db.QueryRow(`SELECT group_concat(token || ':' || lineage_only || superseded, ' ') FROM (SELECT token, lineage_only, superseded
		FROM thread_requests WHERE target_thread_id = 't3' ORDER BY token)`).Scan(&marks); err != nil || marks != "old-side:11 spawn-t3:00" {
		t.Fatalf("the spawns of t3 read %q, %v; want the older superseded and lineage only, the newer its link", marks, err)
	}
	if _, err := db.Exec(`INSERT INTO thread_requests (token, caller_thread_id, kind, target_thread_id, state, created_at, updated_at)
		VALUES ('again', 'side', 'spawn', 't4', 'running', 40, 40)`); err == nil || !strings.Contains(err.Error(), "UNIQUE") {
		t.Fatalf("a second local spawn of t4 = %v; want the unique index to refuse it", err)
	}
	want := map[string][agentOpenDepth]int64{
		"spawn-t5": {1, 0, 0, 0}, "spawn-t4": {0, 1, 0, 0}, "spawn-t3": {1, 0, 1, 0}, "spawn-t2": {0, 1, 0, 1}, "spawn-t1": {0, 0, 1, 0},
	}
	if got := agentTreeMarks(t, db); !maps.Equal(got, want) {
		t.Fatalf("filled marks %v; want %v", got, want)
	}
	assertAgentTreeMarks(t, db, "after the fill")
	mustExec(t, db, `UPDATE thread_requests SET settled_at = 50, state = 'finished' WHERE token = 'open-t5'`)
	assertAgentTreeMarks(t, db, "after a settle")
}

// A store from before v151 that holds two local spawns of one thread, by
// time and by token at an equal time, upgrades with the newest its one
// owner on every surface: its spawner, its row's owner, the owner's
// workers, spawns and archive, and the search of either caller. The
// superseded spawn is no owner at all, so deleting the owner leaves the worker
// standalone with no spawner, and owned is refused for it.
func TestUpgradeSupersededSpawnOwnsNothing(t *testing.T) {
	db, path := openFileDBThrough(t, assistantsMigrationVersion-1)
	mustExec(t, db, `INSERT INTO projects (id, path, name, created_at, updated_at) VALUES ('p', '/p', 'p', 1, 1)`)
	mustExec(t, db, `INSERT INTO threads (id, project_id, title, provider, workspace_path, model, created_at, updated_at, archived, placement) VALUES
		('a', 'p', 'A', 'claude', '/p', '', 1, 1, 0, 'standalone'),
		('b', 'p', 'B', 'claude', '/p', '', 1, 1, 0, 'standalone'),
		('w', 'p', 'W', 'claude', '/p', '', 1, 1, 0, 'owned'),
		('tie', 'p', 'Tie', 'claude', '/p', '', 1, 1, 0, 'owned')`)
	mustExec(t, db, `INSERT INTO thread_requests (token, caller_thread_id, kind, target_thread_id, state, settled_at, created_at, updated_at) VALUES
		('w-a', 'a', 'spawn', 'w', 'finished', 5, 1, 1),
		('w-b', 'b', 'spawn', 'w', 'finished', 5, 2, 2),
		('tie-b', 'b', 'spawn', 'tie', 'finished', 5, 3, 3),
		('tie-a', 'a', 'spawn', 'tie', 'finished', 5, 3, 3)`)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	s, err := New(path)
	if err != nil {
		t.Fatalf("open through every migration: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	owners := func(when, want string) {
		t.Helper()
		for _, id := range []string{"w", "tie"} {
			spawner, _, found, err := s.SpawnerOf(id)
			if err != nil || spawner != want || found != (want != "") {
				t.Errorf("%s: SpawnerOf(%s) = %q, %v, %v; want %q", when, id, spawner, found, err, want)
			}
			row, err := s.GetThread(id)
			if err != nil || row.SpawnedByThreadID != want {
				t.Errorf("%s: %s's row names %q, %v as its spawner; want %q", when, id, row.SpawnedByThreadID, err, want)
			}
		}
		for _, caller := range []string{"a", "b"} {
			var wantIDs []string
			if caller == want {
				wantIDs = []string{"tie", "w"}
			}
			workers, err := s.ListOwnedWorkersToArchive(caller)
			var owned []string
			for _, worker := range workers {
				owned = append(owned, worker.ThreadID)
			}
			if err != nil || !slices.Equal(owned, wantIDs) {
				t.Errorf("%s: %s owns %q, %v; want %q", when, caller, owned, err, wantIDs)
			}
			spawned, err := s.ListSpawnedThreadIDs(caller)
			if err != nil || !slices.Equal(spawned, wantIDs) {
				t.Errorf("%s: %s spawned %q, %v; want %q", when, caller, spawned, err, wantIDs)
			}
			found, err := s.ListThreadsByActivity(ThreadSearchFilter{SpawnedBy: caller})
			var foundIDs []string
			for _, row := range found {
				foundIDs = append(foundIDs, row.ID)
			}
			slices.Sort(foundIDs)
			if err != nil || !slices.Equal(foundIDs, wantIDs) {
				t.Errorf("%s: the search spawned by %s finds %q, %v; want %q", when, caller, foundIDs, err, wantIDs)
			}
		}
	}
	owners("after the upgrade", "b")
	if _, _, err := s.ArchiveThread("a"); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"w", "tie"} {
		if _, changed, err := s.ArchiveOwnedWorker(id, "a"); err != nil || changed {
			t.Errorf("%s for the superseded spawner's archive = changed %v, %v; want left alone", id, changed, err)
		}
	}
	if _, _, err := s.UnarchiveThread("a"); err != nil {
		t.Fatal(err)
	}

	if err := s.DeleteThread("b"); err != nil {
		t.Fatal(err)
	}
	owners("after the owner's delete", "")
	placements, err := queryIDs(s.db, `SELECT id || '|' || placement FROM threads WHERE id IN ('w', 'tie') ORDER BY id`)
	if err != nil || !slices.Equal(placements, []string{"tie|standalone", "w|standalone"}) {
		t.Fatalf("after the owner's delete the workers are %q, %v; want both standalone", placements, err)
	}
	if _, err := s.SetThreadPlacement([]string{"w"}, PlacementOwned); !errors.Is(err, ErrThreadNotSpawned) {
		t.Fatalf("owned asked of a thread only a superseded spawn names = %v; want ErrThreadNotSpawned", err)
	}
}

// olderReleaseSchemaVersion is the schema v0.0.16 shipped.
const olderReleaseSchemaVersion = 139

// A store an older release wrote opens through the whole chain with what
// the retired features and the request ledger wrote carried to the final
// design: the discussion becomes a chat, the automation run a manual run,
// the spawned thread nests under its spawner and the grouped one stays
// grouped, the delivered answer and late reply read confirmed with the
// late route and both wake rows, the older of two spawns of one thread is
// lineage only, the open work under a spawn is counted on it, the queued
// watch is confirmed and owned by its thread, and the orphan watch is gone.
func TestUpgradeFromAnOlderReleaseKeepsMainData(t *testing.T) {
	db, path := openFileDBThrough(t, olderReleaseSchemaVersion)
	mustExec(t, db, `INSERT INTO projects (id, path, name, created_at, updated_at) VALUES ('p', '/p', 'p', 1, 1)`)
	mustExec(t, db, `INSERT INTO thread_groups (id, project_id, name, created_at, updated_at) VALUES ('g', 'p', 'Sweep', 1, 1)`)
	mustExec(t, db, `INSERT INTO threads (id, project_id, title, provider, workspace_path, model, mode, created_at, updated_at, archived, group_id) VALUES
		('disc', 'p', 'Discussion', 'claude', '/p', '', 'discussion', 1, 1, 0, NULL),
		('caller', 'p', 'Caller', 'claude', '/p', '', 'chat', 1, 2, 0, NULL),
		('child', 'p', 'Child', 'codex', '/p', '', 'chat', 1, 3, 0, NULL),
		('grouped', 'p', 'Grouped', 'claude', '/p', '', 'chat', 1, 4, 0, 'g')`)
	mustExec(t, db, `INSERT INTO items (id, thread_id, turn_index, item_index, kind, role, status, summary, meta, created_at, updated_at) VALUES
		('row-answer', 'caller', 1, 0, 'user_text', 'user', 'completed', 'answer', '{"sendId":"thread-wake:late"}', 1, 1),
		('row-late', 'caller', 2, 0, 'user_text', 'user', 'completed', 'late', '{"sendId":"thread-wake-late:late"}', 1, 1)`)
	mustExec(t, db, `INSERT INTO thread_requests (token, caller_thread_id, kind, target_computer_id, target_thread_id, notify, state, answer,
		settled_at, delivered_at, delivered_how, late_reply, late_delivered_at, created_at, updated_at) VALUES
		('sp-old', 'caller', 'spawn', '', 'child', 0, 'finished', 'a', 5, NULL, '', NULL, NULL, 1, 1),
		('sp', 'caller', 'spawn', '', 'child', 0, 'finished', 'a', 5, NULL, '', NULL, NULL, 2, 2),
		('sp-grouped', 'caller', 'spawn', '', 'grouped', 0, 'finished', 'a', 5, NULL, '', NULL, NULL, 3, 3),
		('late', 'caller', 'send', '', 'disc', 1, 'finished', 'a', 10, 20, 'queued', 'b', 30, 4, 4),
		('open', 'child', 'send', '', 'disc', 1, 'running', NULL, NULL, NULL, '', NULL, NULL, 5, 5)`)
	mustExec(t, db, `INSERT INTO work_items (id, project_id, goal, workflow_id, workflow_scope, state, source, source_ref, created_at)
		VALUES ('run-auto', 'p', 'Nightly', 'build', 'project', 'done', 'automation', 'auto', 1)`)
	mustExec(t, db, `INSERT INTO remote_watches (computer_id, request_id, thread_id, fingerprint, label, notification, created_at) VALUES
		('pc', 'queued', 'caller', 'f', 'job', 'queued', 6),
		('pc', 'orphan', 'gone', 'f', 'job', 'pending', 7)`)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	s, err := New(path)
	if err != nil {
		t.Fatalf("open through every migration: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	for _, check := range []struct {
		what, query string
		want        []string
	}{
		{"threads", `SELECT id || '|' || mode || '|' || placement || '|' || COALESCE(group_id, '') FROM threads ORDER BY id`,
			[]string{"caller|chat|standalone|", "child|chat|standalone|", "disc|chat|standalone|", "grouped|chat|grouped|g"}},
		{"work items", `SELECT id || '|' || source || '|' || source_ref FROM work_items`, []string{"run-auto|manual|"}},
		{"requests", `SELECT token || '|' || lineage_only || '|' || COALESCE(confirmed_at, '-') || '|' || COALESCE(late_confirmed_at, '-') || '|' ||
			late_delivered_how || '|' || wake_item_id || '|' || late_wake_item_id || '|' || answer_agent || '|' || open_below_0 || open_below_1
			FROM thread_requests ORDER BY token`,
			[]string{"late|0|20|30|queued|row-answer|row-late|0|00", "open|0|-|-||||0|00", "sp|0|-|-||||0|10", "sp-grouped|0|-|-||||0|00", "sp-old|1|-|-||||0|00"}},
		{"watches", `SELECT request_id || '|' || thread_id || '|' || launched_in_thread_id || '|' || COALESCE(confirmed_at, '-') FROM remote_watches`,
			[]string{"queued|caller|caller|6"}},
		{"retired tables", `SELECT name FROM sqlite_master WHERE name IN ('channels', 'channel_messages', 'discussion_definitions', 'automations', 'automation_cursors', 'standing_agents')`,
			nil},
	} {
		got, err := queryIDs(s.db, check.query)
		if err != nil {
			t.Fatalf("%s: %v", check.what, err)
		}
		if !slices.Equal(got, check.want) {
			t.Errorf("%s after the upgrade:\n got %q\nwant %q", check.what, got, check.want)
		}
	}
	tx, err := s.db.Begin()
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
	assertAgentTreeMarks(t, s.db, "after the upgrade")
}
