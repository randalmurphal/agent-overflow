package store

import (
	"database/sql"
	"encoding/json"
	"strings"
	"testing"
)

// seedMigrationThread writes a project and a Claude thread on a raw
// migration database.
func seedMigrationThread(t *testing.T, db *sql.DB, threadIDs ...string) {
	t.Helper()
	mustExec(t, db, `INSERT INTO projects (id, path, name, created_at, updated_at) VALUES ('p', '/p', 'p', 1, 1)`)
	for _, id := range threadIDs {
		mustExec(t, db, `INSERT INTO threads (id, project_id, title, provider, workspace_path, model, created_at, updated_at, archived)
			VALUES (?, 'p', 'T', 'claude', '/p', '', 1, 1, 0)`, id)
	}
}

// migrationRow is an items row a raw migration test writes.
type migrationRow struct {
	id, kind, role, status, summary, parent, tool, completionOf, payload, meta string
	background, turn, item                                                     int
	created                                                                    int64
}

func insertMigrationRow(db *sql.DB, threadID string, r migrationRow) error {
	if r.role == "" {
		r.role = "assistant"
	}
	if r.status == "" {
		r.status = "completed"
	}
	if r.meta == "" {
		r.meta = "{}"
	}
	var payload any
	if r.payload != "" {
		payload = r.payload
	}
	_, err := db.Exec(`INSERT INTO items (id, thread_id, turn_index, item_index, kind, role, status, summary,
		payload_id, parent_id, is_background, completion_of, tool_name, meta, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		r.id, threadID, r.turn, r.item, r.kind, r.role, r.status, r.summary,
		payload, r.parent, r.background, r.completionOf, r.tool, r.meta, r.created, r.created)
	return err
}

func itemsDDL(t *testing.T, db *sql.DB) string {
	t.Helper()
	var ddl string
	if err := db.QueryRow(`SELECT sql FROM sqlite_master WHERE type = 'table' AND name = 'items'`).Scan(&ddl); err != nil {
		t.Fatalf("read the items DDL: %v", err)
	}
	return ddl
}

// v126 admits 'parked' and nothing else new, in place: the DDL is v125's
// with one status added, the rows are intact, and the settle triggers are
// the parked-aware ones.
func TestParkedStopMigrationWidensTheItemsStatusCheck(t *testing.T) {
	db := migrateThrough(t, parkedStopMigrationVersion-1)
	seedMigrationThread(t, db, "t")
	parked := migrationRow{id: "stop", kind: "tool_completion", status: ItemStatusParked, completionOf: "launch", background: 1, item: 1, created: 2}
	if err := insertMigrationRow(db, "t", migrationRow{id: "launch", kind: "tool_call", status: "running", tool: "Agent", background: 1, created: 1}); err != nil {
		t.Fatalf("seed the launch: %v", err)
	}
	if err := insertMigrationRow(db, "t", parked); err == nil || !strings.Contains(err.Error(), "CHECK") {
		t.Fatalf("v125 took a parked row: %v", err)
	}
	before := itemsDDL(t, db)

	if err := applyMigration(db, migrationByVersion(t, parkedStopMigrationVersion)); err != nil {
		t.Fatalf("apply v%d: %v", parkedStopMigrationVersion, err)
	}
	after := itemsDDL(t, db)
	want, err := widenStatusCheckDDL(before)
	if err != nil {
		t.Fatal(err)
	}
	if after != want || strings.Count(after, "'parked'") != 1 {
		t.Fatalf("items DDL after v126:\n%s\nwant:\n%s", after, want)
	}
	if err := insertMigrationRow(db, "t", parked); err != nil {
		t.Fatalf("v126 refused a parked row: %v", err)
	}
	if err := insertMigrationRow(db, "t", migrationRow{id: "bogus", kind: "tool_completion", status: "paused", item: 2, created: 3}); err == nil {
		t.Fatal("v126 took a status it does not declare")
	}
	var check string
	if err := db.QueryRow(`PRAGMA integrity_check`).Scan(&check); err != nil || check != "ok" {
		t.Fatalf("integrity_check = %q, %v", check, err)
	}
	// The parked row settled nothing.
	var flag sql.NullInt64
	if err := db.QueryRow(`SELECT json_extract(meta, '$.live_background_active') FROM items WHERE id = 'launch'`).Scan(&flag); err != nil {
		t.Fatal(err)
	}
	if flag.Valid {
		t.Fatalf("a parked sibling set the launch's live flag to %d", flag.Int64)
	}
}

func TestWidenStatusCheckDDLRefusesAnUnexpectedList(t *testing.T) {
	good := "CREATE TABLE items (status TEXT CHECK(status IN (\n 'streaming',\n 'running',\n 'completed',\n 'errored',\n 'declined',\n 'killed'\n )))"
	if widened, err := widenStatusCheckDDL(good); err != nil || !strings.Contains(widened, "'killed',\n        'parked'\n )") {
		t.Fatalf("widen = %q, %v", widened, err)
	}
	for name, ddl := range map[string]string{
		"already parked": strings.Replace(good, "'killed'", "'killed', 'parked'", 1),
		"missing status": strings.Replace(good, "'declined',", "", 1),
		"no check":       "CREATE TABLE items (status TEXT)",
		"two checks":     good + " CHECK(status IN ('x'))",
	} {
		if widened, err := widenStatusCheckDDL(ddl); err == nil {
			t.Errorf("%s: widened to %q", name, widened)
		}
	}
}

// migrationItem is the part of a row the v127 assertions read.
type migrationItem struct {
	kind, role, status, summary, completionOf, tool, payload string
	background, turn, item                                   int
	created                                                  int64
	meta                                                     map[string]any
}

func readMigrationItem(t *testing.T, db *sql.DB, threadID, id string) (migrationItem, bool) {
	t.Helper()
	var it migrationItem
	var payload sql.NullString
	var meta string
	err := db.QueryRow(`SELECT kind, role, status, summary, completion_of, tool_name, payload_id, is_background,
		turn_index, item_index, created_at, meta FROM items WHERE thread_id = ? AND id = ?`, threadID, id).
		Scan(&it.kind, &it.role, &it.status, &it.summary, &it.completionOf, &it.tool, &payload, &it.background,
			&it.turn, &it.item, &it.created, &meta)
	if err == sql.ErrNoRows {
		return it, false
	}
	if err != nil {
		t.Fatalf("read %s/%s: %v", threadID, id, err)
	}
	it.payload = payload.String
	if err := json.Unmarshal([]byte(meta), &it.meta); err != nil {
		t.Fatalf("meta of %s: %v", id, err)
	}
	return it, true
}

func threadEpoch(t *testing.T, db *sql.DB, threadID string) int {
	t.Helper()
	var epoch int
	if err := db.QueryRow(`SELECT history_epoch FROM threads WHERE id = ?`, threadID).Scan(&epoch); err != nil {
		t.Fatal(err)
	}
	return epoch
}

// v127 turns every parked bell into the parked sibling of the row its stop
// belonged to, at its own position, with the report head as its payload
// and the run's start read from the wake rows, and deletes the agent bells
// an ending sibling covers. A command's bell, an agent bell nothing covers
// and a nested row are left as they are.
func TestParkedBellMigrationConvertsBellsInPlace(t *testing.T) {
	db := migrateThrough(t, 126)
	seedMigrationThread(t, db, "t", "fork-shows", "fork-before")
	mustExec(t, db, `INSERT INTO payloads (thread_id, id, kind, meta, data, created_at)
		VALUES ('t', 'tool-call-result:L', 'tool_call_result', '{"preview":"Both passed."}', X'', 1600)`)

	longReport := "First line of the report.\n" + strings.Repeat("x", 300)
	bellMeta := func(fields map[string]any) string {
		base := map[string]any{"task_id": "T", "source": "task_notification", "output_file_state": "ready", "status": "completed", "output_file": "/tmp/T.output"}
		for k, v := range fields {
			base[k] = v
		}
		encoded, err := json.Marshal(base)
		if err != nil {
			t.Fatal(err)
		}
		return string(encoded)
	}
	parkedBell := `Agent "gate watcher" reported and is waiting on 1 background command`
	rows := []migrationRow{
		{id: "L", kind: "tool_call", status: "running", summary: "Agent: gate watcher", tool: "Agent", background: 1, meta: `{"task_id":"T"}`, turn: 0, item: 0, created: 1000},
		{id: "R1", kind: "assistant_text", summary: "Report one", parent: "L", turn: 0, item: 1, created: 1100},
		{id: "B1", kind: "notification", role: "system", summary: parkedBell, tool: "Agent", turn: 0, item: 2, created: 1200,
			meta: bellMeta(map[string]any{"uuid": "u1", "tool_use_id": "L", "kind": "parked_agent", "parked_commands": 1, "parked_report_item_id": "R1", "parked_report_preview": longReport})},
		{id: "W1", kind: "user_text", role: "user", summary: "shell done", parent: "L", turn: 0, item: 3, created: 1300, meta: `{"subagent_wake_prompt":true,"task_id":"T"}`},
		{id: "B2", kind: "notification", role: "system", summary: parkedBell, tool: "Agent", turn: 1, item: 0, created: 1500,
			meta: bellMeta(map[string]any{"uuid": "u2", "kind": "parked_agent", "parked_commands": 2})},
		{id: "F", kind: "notification", role: "system", summary: "Both passed.", tool: "Agent", payload: "tool-call-result:L", turn: 1, item: 1, created: 1600,
			meta: bellMeta(map[string]any{"uuid": "u3", "output_file_state": "loaded"})},
		{id: "complete:L", kind: "tool_completion", summary: "Agent: gate watcher -> done", tool: "Agent", completionOf: "L", payload: "tool-call-result:L", background: 1, turn: 1, item: 2, created: 1600,
			meta: `{"task_id":"T","status_source":"task_updated"}`},
		{id: "S", kind: "tool_call", status: "running", summary: "Bash: make", tool: "Bash", background: 1, meta: `{"task_id":"S"}`, turn: 1, item: 3, created: 1700},
		{id: "complete:S", kind: "tool_completion", summary: "Bash: make -> exit 0", tool: "Bash", completionOf: "S", background: 1, turn: 1, item: 4, created: 1800, meta: `{"task_id":"S"}`},
		{id: "SB", kind: "notification", role: "system", summary: `Background command "make" completed (exit code 0)`, tool: "Bash", turn: 1, item: 5, created: 1800, meta: `{"task_id":"S"}`},
		{id: "K", kind: "tool_call", status: "running", summary: "Agent: open", tool: "Agent", background: 1, meta: `{"task_id":"K"}`, turn: 2, item: 0, created: 1900},
		{id: "KB", kind: "notification", role: "system", summary: "Open report", tool: "Agent", turn: 2, item: 1, created: 1950, meta: `{"task_id":"K"}`},
		{id: "CR", kind: "tool_call", status: "running", summary: "Agent: gate watcher", tool: "SendMessage", background: 1, meta: `{"task_id":"T","transcript_root_id":"L"}`, turn: 2, item: 2, created: 2000},
		{id: "B3", kind: "notification", role: "system", summary: parkedBell, tool: "SendMessage", turn: 2, item: 3, created: 2100,
			meta: bellMeta(map[string]any{"uuid": "u4", "tool_use_id": "CR", "kind": "parked_agent", "parked_commands": 1, "parked_report_preview": "Short\r\nreport"})},
		{id: "W3", kind: "user_text", role: "user", summary: "shell done", parent: "L", turn: 0, item: 4, created: 2200, meta: `{"subagent_wake_prompt":true,"task_id":"T"}`},
		{id: "B4", kind: "notification", role: "system", summary: parkedBell, tool: "SendMessage", turn: 2, item: 4, created: 2300,
			meta: bellMeta(map[string]any{"uuid": "u5", "kind": "parked_agent", "parked_commands": 1})},
		{id: "NB", kind: "notification", role: "system", summary: parkedBell, parent: "L", tool: "Agent", turn: 2, item: 5, created: 2400,
			meta: bellMeta(map[string]any{"uuid": "u6", "kind": "parked_agent", "parked_commands": 1})},
	}
	for _, row := range rows {
		if err := insertMigrationRow(db, "t", row); err != nil {
			t.Fatalf("seed %s: %v", row.id, err)
		}
	}
	// Pointer forks of t made after its rows: one shows the bells, one is
	// cut before them. The fork triggers refuse a change to a row a fork
	// shows, which the migration's conversion is.
	mustExec(t, db, `INSERT INTO thread_fork_lineage (thread_id, depth, ancestor_id, cut_turn_index, cut_item_index)
		VALUES ('fork-shows', 1, 't', 1, 5), ('fork-before', 1, 't', 0, 1)`)
	if _, err := db.Exec(`UPDATE items SET summary = 'x' WHERE thread_id = 't' AND id = 'B2'`); err == nil || !strings.Contains(err.Error(), shownHistoryImmutable) {
		t.Fatalf("a change to a bell the fork shows = %v, want refused", err)
	}
	epochs := map[string]int{"t": threadEpoch(t, db, "t"), "fork-shows": threadEpoch(t, db, "fork-shows"), "fork-before": threadEpoch(t, db, "fork-before")}

	if err := applyMigration(db, migrationByVersion(t, 127)); err != nil {
		t.Fatalf("apply v127: %v", err)
	}
	// The fork triggers are back, with the parked-aware revive.
	if _, err := db.Exec(`UPDATE items SET summary = 'x' WHERE thread_id = 't' AND id = 'B2'`); err == nil || !strings.Contains(err.Error(), shownHistoryImmutable) {
		t.Fatalf("a change to a row the fork shows after v127 = %v, want refused", err)
	}
	var revive string
	if err := db.QueryRow(`SELECT sql FROM sqlite_master WHERE type = 'trigger' AND name = 'trg_items_revive_bg_launch_on_completion_move'`).Scan(&revive); err != nil {
		t.Fatal(err)
	}
	if revive+";" != strings.TrimSpace(reviveBgLaunchOnCompletionMoveV127SQL) {
		t.Fatalf("revive-on-move trigger after v127:\n%s\nwant:\n%s", revive, reviveBgLaunchOnCompletionMoveV127SQL)
	}

	type want struct {
		launch, summary, tool string
		turn, item            int
		created, runStart     int64
		woke                  bool
		commands              float64
		report, preview       string
		toolUse               string
	}
	head := strings.Repeat("x", 239-len("First line of the report. "))
	for id, w := range map[string]want{
		"B1": {launch: "L", summary: "Agent: gate watcher -> parked", tool: "Agent", turn: 0, item: 2, created: 1200, runStart: 1000,
			commands: 1, report: "R1", preview: "First line of the report. " + head + "…", toolUse: "L"},
		"B2": {launch: "L", summary: "Agent: gate watcher -> parked", tool: "Agent", turn: 1, item: 0, created: 1500, runStart: 1300, woke: true, commands: 2},
		"B3": {launch: "CR", summary: "Agent: gate watcher -> parked", tool: "SendMessage", turn: 2, item: 3, created: 2100, runStart: 2000,
			commands: 1, preview: "Short  report", toolUse: "CR"},
		"B4": {launch: "CR", summary: "Agent: gate watcher -> parked", tool: "SendMessage", turn: 2, item: 4, created: 2300, runStart: 2200, woke: true, commands: 1},
	} {
		got, found := readMigrationItem(t, db, "t", id)
		if !found {
			t.Fatalf("%s is gone", id)
		}
		if got.kind != "tool_completion" || got.role != "assistant" || got.status != ItemStatusParked || got.completionOf != w.launch ||
			got.background != 1 || got.summary != w.summary || got.tool != w.tool {
			t.Errorf("%s = %+v, want the parked sibling of %s", id, got, w.launch)
		}
		if got.turn != w.turn || got.item != w.item || got.created != w.created {
			t.Errorf("%s moved to (%d,%d)@%d, want (%d,%d)@%d", id, got.turn, got.item, got.created, w.turn, w.item, w.created)
		}
		m := got.meta
		if m["task_id"] != "T" || m["parked_commands"] != w.commands || m["run_started_at"] != float64(w.runStart) {
			t.Errorf("%s meta = %v, want task T, %v commands, run from %d", id, m, w.commands, w.runStart)
		}
		if woke, _ := m["run_woke"].(bool); woke != w.woke {
			t.Errorf("%s run_woke = %v, want %v", id, m["run_woke"], w.woke)
		}
		if report, _ := m["parked_report_item_id"].(string); report != w.report {
			t.Errorf("%s report = %q, want %q", id, report, w.report)
		}
		if toolUse, _ := m["tool_use_id"].(string); toolUse != w.toolUse {
			t.Errorf("%s tool_use_id = %q, want %q", id, toolUse, w.toolUse)
		}
		for _, gone := range []string{"kind", "parked_report_preview", "uuid", "output_file_state"} {
			if _, has := m[gone]; has {
				t.Errorf("%s keeps the bell's %s: %v", id, gone, m)
			}
		}
		if m["notification_output_loaded"] != (w.preview != "") || m["notification_terminal_state"] != "completed" || m["output_file"] != "/tmp/T.output" {
			t.Errorf("%s notification meta = %v", id, m)
		}
		if w.preview == "" {
			if got.payload != "" {
				t.Errorf("%s has payload %q with no report", id, got.payload)
			}
			continue
		}
		var payloadMeta string
		if err := db.QueryRow(`SELECT meta FROM payloads WHERE thread_id = 't' AND id = ?`, got.payload).Scan(&payloadMeta); err != nil {
			t.Fatalf("%s payload %q: %v", id, got.payload, err)
		}
		var pm map[string]any
		if err := json.Unmarshal([]byte(payloadMeta), &pm); err != nil {
			t.Fatal(err)
		}
		if got.payload != "tool-call-result:"+id || pm["preview"] != w.preview || pm["outputFile"] != "/tmp/T.output" || pm["outputFileState"] != "loaded" {
			t.Errorf("%s payload %s = %v, want preview %q", id, got.payload, pm, w.preview)
		}
	}
	if _, found := readMigrationItem(t, db, "t", "F"); found {
		t.Error("the agent's final bell the ending sibling covers is still there")
	}
	var payloads int
	if err := db.QueryRow(`SELECT count(*) FROM payloads WHERE thread_id = 't' AND id = 'tool-call-result:L'`).Scan(&payloads); err != nil || payloads != 1 {
		t.Errorf("the ending sibling's payload = %d rows (%v), want kept", payloads, err)
	}
	for _, id := range []string{"SB", "KB", "NB"} {
		if got, found := readMigrationItem(t, db, "t", id); !found || got.kind != "notification" {
			t.Errorf("%s = %+v found=%v, want left a notification", id, got, found)
		}
	}
	var flag sql.NullInt64
	if err := db.QueryRow(`SELECT json_extract(meta, '$.live_background_active') FROM items WHERE thread_id = 't' AND id = 'CR'`).Scan(&flag); err != nil {
		t.Fatal(err)
	}
	if flag.Valid {
		t.Errorf("the parked carrier's live flag = %d, want unset", flag.Int64)
	}
	if got := threadEpoch(t, db, "fork-shows"); got != epochs["fork-shows"]+1 {
		t.Errorf("the fork showing the deleted bell has epoch %d, want %d", got, epochs["fork-shows"]+1)
	}
	if got := threadEpoch(t, db, "fork-before"); got != epochs["fork-before"] {
		t.Errorf("the fork cut before the deleted bell has epoch %d, want %d", got, epochs["fork-before"])
	}
	if got := threadEpoch(t, db, "t"); got <= epochs["t"] {
		t.Errorf("the source thread's epoch %d did not move from %d", got, epochs["t"])
	}
	var check string
	if err := db.QueryRow(`PRAGMA integrity_check`).Scan(&check); err != nil || check != "ok" {
		t.Fatalf("integrity_check = %q, %v", check, err)
	}
	if n := countRowsDB(t, db, `SELECT count(*) FROM pragma_foreign_key_check`); n != 0 {
		t.Fatalf("%d foreign key violations", n)
	}
	migrateFrom(t, db, 127)
}
