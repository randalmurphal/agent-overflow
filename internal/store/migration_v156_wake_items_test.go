package store

import (
	"database/sql"
	"slices"
	"testing"
	"time"
)

// v156 over a populated database: a confirmed queued answer or late reply
// gets the row the send-id lookup finds, a joined row included; an
// unconfirmed, inline or row-less one gets none; no other column changes.
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
	seedConfirmedWakes(t, db, []string{"bulk"}, 3000)
	before, err := queryIDs(db, `SELECT `+threadRequestColumnsBeforeV150+` FROM thread_requests ORDER BY token`)
	if err != nil {
		t.Fatal(err)
	}

	if err := applyMigration(db, migrationByVersion(t, wakeItemsMigrationVersion)); err != nil {
		t.Fatalf("apply v%d: %v", wakeItemsMigrationVersion, err)
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

// seedConfirmedWakes writes n confirmed queued answers spread over callers,
// each with the user row its wake left, every tenth joined with another
// message.
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
			settled_at, delivered_at, delivered_how, confirmed_at, created_at, updated_at)
		SELECT printf('bulk-%06d', i), (SELECT value FROM json_each(?2) WHERE key = i % json_array_length(?2)),
		       'send', '', 't', '', 'finished', 'a', 10, 20, 'queued', 25, 1, 1 FROM seq`, n, list)
	mustExec(t, db, `COMMIT`)
}

// v156's fill is one statement per column and a keyed write per joined
// wake, so it runs at open: 50,000 confirmed answers (30 days of retention
// at 1,700 wakes a day) take well under the open's budget, and each lookup
// probes its index by the caller's key.
func TestMigrationV156WakeItemsAtScale(t *testing.T) {
	if testing.Short() {
		t.Skip("seeds 50,000 requests")
	}
	for _, n := range []int{3_000, 50_000} {
		path := fileDBThrough(t, wakeItemsMigrationVersion-1)
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
		if err := applyMigration(db, migrationByVersion(t, wakeItemsMigrationVersion)); err != nil {
			t.Fatalf("apply v%d: %v", wakeItemsMigrationVersion, err)
		}
		elapsed := time.Since(start)
		t.Logf("v%d over %d confirmed answers: %v", wakeItemsMigrationVersion, n, elapsed)
		var missing int
		if err := db.QueryRow(`SELECT COUNT(*) FROM thread_requests WHERE wake_item_id <> 'row-' || token`).Scan(&missing); err != nil || missing != 0 {
			t.Errorf("%d of %d answers without their row, %v", missing, n, err)
		}
		if elapsed > 5*time.Second {
			t.Errorf("v%d took %v over %d confirmed answers; its fill belongs in a deferred phase", wakeItemsMigrationVersion, elapsed, n)
		}
		if err := db.Close(); err != nil {
			t.Fatal(err)
		}
	}
}
