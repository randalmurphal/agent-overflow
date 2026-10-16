package store

import (
	"database/sql"
	"fmt"
	"slices"
	"testing"
	"time"
)

// v156 over a populated database: a confirmed queued answer or late reply
// gets the row the send-id lookup finds, a joined row included; an
// unconfirmed, inline or row-less one gets none; no other column changes.
// A few thousand confirmed answers run inside the open's budget.
func TestMigrationV156WakeItems(t *testing.T) {
	path := fileDBThrough(t, wakeItemsMigrationVersion-1)
	db, err := sql.Open("sqlite", poolDSN(path, writerConnPragmas))
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
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
	userRow("caller", "row-unconfirmed", 5, `{"sendId":"thread-wake:unconfirmed"}`)
	mustExec(t, db, `INSERT INTO thread_requests (token, caller_thread_id, kind, target_computer_id, target_thread_id, target_thread_title, state, answer,
		settled_at, delivered_at, delivered_how, late_reply, late_delivered_at, late_delivered_how, confirmed_at, late_confirmed_at, created_at, updated_at) VALUES
		('first', 'caller', 'send', '', 't', '', 'finished', 'a', 10, 20, 'queued', NULL, NULL, '', 25, NULL, 1, 1),
		('joined', 'caller', 'send', '', 't', '', 'finished', 'a', 10, 20, 'queued', NULL, NULL, '', 25, NULL, 1, 1),
		('late', 'caller', 'send', '', 't', '', 'finished', 'a', 10, 20, 'queued', 'b', 30, 'queued', 25, 35, 1, 1),
		('unconfirmed', 'caller', 'send', '', 't', '', 'finished', 'a', 10, 20, 'queued', NULL, NULL, '', NULL, NULL, 1, 1),
		('inline', 'caller', 'send', '', 't', '', 'finished', 'a', 10, 20, 'inline', NULL, NULL, '', 20, NULL, 1, 1),
		('reverted', 'caller', 'send', '', 't', '', 'finished', 'a', 10, 20, 'queued', NULL, NULL, '', 25, NULL, 1, 1),
		('open', 'caller', 'ask', '', '', '', 'running', NULL, NULL, NULL, '', NULL, NULL, '', NULL, NULL, 1, 1)`)
	const bulk = 3000
	mustExec(t, db, `BEGIN`)
	for i := range bulk {
		token := fmt.Sprintf("bulk-%04d", i)
		userRow("bulk", "row-"+token, i+1, `{"sendId":"thread-wake:`+token+`"}`)
		mustExec(t, db, `INSERT INTO thread_requests (token, caller_thread_id, kind, target_computer_id, target_thread_id, target_thread_title, state, answer,
			settled_at, delivered_at, delivered_how, confirmed_at, created_at, updated_at)
			VALUES (?, 'bulk', 'send', '', 't', '', 'finished', 'a', 10, 20, 'queued', 25, 1, 1)`, token)
	}
	mustExec(t, db, `COMMIT`)
	before, err := queryIDs(db, `SELECT `+threadRequestColumnsBeforeV150+` FROM thread_requests ORDER BY token`)
	if err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	if err := applyMigration(db, migrationByVersion(t, wakeItemsMigrationVersion)); err != nil {
		t.Fatalf("apply v%d: %v", wakeItemsMigrationVersion, err)
	}
	elapsed := time.Since(start)
	t.Logf("v%d over %d confirmed answers: %v", wakeItemsMigrationVersion, bulk+3, elapsed)
	if elapsed > 10*time.Second {
		t.Errorf("v%d took %v over %d confirmed answers; a deferred phase would be due", wakeItemsMigrationVersion, elapsed, bulk)
	}

	if got, err := queryIDs(db, `SELECT `+threadRequestColumnsBeforeV150+` FROM thread_requests ORDER BY token`); err != nil {
		t.Fatal(err)
	} else if !slices.Equal(got, before) {
		t.Error("thread_requests changed beyond the new columns")
	}
	got, err := queryIDs(db, `SELECT token || '|' || wake_item_id || '|' || late_wake_item_id FROM thread_requests
		WHERE caller_thread_id = 'caller' ORDER BY token`)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"first|row-first|", "inline||", "joined|row-joined|", "late|row-late-first|row-late", "open||", "reverted||", "unconfirmed||"}
	if !slices.Equal(got, want) {
		t.Errorf("wake rows = %q, want %q", got, want)
	}
	var missing int
	if err := db.QueryRow(`SELECT COUNT(*) FROM thread_requests WHERE caller_thread_id = 'bulk' AND wake_item_id <> 'row-' || token`).Scan(&missing); err != nil || missing != 0 {
		t.Errorf("%d bulk answers without their row, %v", missing, err)
	}
}
