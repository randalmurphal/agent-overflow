package store

import (
	"database/sql"
	"slices"
	"testing"
)

// v148 over a populated database: every request answer and late reply
// delivered before it, by any route, reads confirmed at its delivery time,
// and only a queued remote completion reads confirmed; an undelivered
// answer and a pending watch stay unconfirmed, and the watches start with
// no redelivery attempts or issue. No other column of either table changes.
func TestMigrationV148DeliveryConfirmations(t *testing.T) {
	path := fileDBThrough(t, deliveryConfirmationsMigrationVersion-1)
	db, err := sql.Open("sqlite", poolDSN(path, writerConnPragmas))
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	mustExec(t, db, `INSERT INTO projects (id, path, name, created_at, updated_at) VALUES ('p', '/p', 'p', 1, 1)`)
	mustExec(t, db, `INSERT INTO threads (id, project_id, title, provider, workspace_path, model, created_at, updated_at, archived) VALUES
		('caller', 'p', 'Caller', 'claude', '/p', '', 1, 1, 0)`)
	mustExec(t, db, `INSERT INTO thread_requests (token, caller_thread_id, kind, state, answer, settled_at, delivered_at, delivered_how, late_reply, late_delivered_at, created_at, updated_at) VALUES
		('queued', 'caller', 'send', 'finished', 'a', 10, 20, 'queued', NULL, NULL, 1, 1),
		('inline', 'caller', 'send', 'finished', 'a', 10, 30, 'inline', NULL, NULL, 1, 1),
		('draft', 'caller', 'send', 'finished', 'a', 10, 40, 'draft', NULL, NULL, 1, 1),
		('late', 'caller', 'send', 'finished', 'a', 10, 50, 'queued', 'b', 60, 1, 1),
		('owed', 'caller', 'send', 'finished', 'a', 10, NULL, '', NULL, NULL, 1, 1)`)
	mustExec(t, db, `INSERT INTO remote_watches (computer_id, request_id, thread_id, fingerprint, label, notification, created_at) VALUES
		('c', 'queued', 'caller', 'f', 'l', 'queued', 70),
		('c', 'pending', 'caller', 'f', 'l', 'pending', 80),
		('c', 'dismissed', 'caller', 'f', 'l', 'dismissed', 90)`)
	requestsBefore, err := queryIDs(db, `SELECT `+threadRequestColumnsBefore+` FROM thread_requests ORDER BY token`)
	if err != nil {
		t.Fatal(err)
	}
	watchesBefore, err := queryIDs(db, `SELECT `+remoteWatchColumnsBefore+` FROM remote_watches ORDER BY request_id`)
	if err != nil {
		t.Fatal(err)
	}

	if err := applyMigration(db, migrationByVersion(t, deliveryConfirmationsMigrationVersion)); err != nil {
		t.Fatalf("apply v%d: %v", deliveryConfirmationsMigrationVersion, err)
	}

	if got, err := queryIDs(db, `SELECT `+threadRequestColumnsBefore+` FROM thread_requests ORDER BY token`); err != nil {
		t.Fatal(err)
	} else if !slices.Equal(got, requestsBefore) {
		t.Errorf("thread_requests changed:\n got %q\nwant %q", got, requestsBefore)
	}
	if got, err := queryIDs(db, `SELECT `+remoteWatchColumnsBefore+` FROM remote_watches ORDER BY request_id`); err != nil {
		t.Fatal(err)
	} else if !slices.Equal(got, watchesBefore) {
		t.Errorf("remote_watches changed:\n got %q\nwant %q", got, watchesBefore)
	}
	got, err := queryIDs(db, `SELECT token || '|' || COALESCE(confirmed_at, 'NULL') || '|' || COALESCE(late_confirmed_at, 'NULL') FROM thread_requests ORDER BY token`)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"draft|40|NULL", "inline|30|NULL", "late|50|60", "owed|NULL|NULL", "queued|20|NULL"}; !slices.Equal(got, want) {
		t.Errorf("request confirmations = %q, want %q", got, want)
	}
	got, err = queryIDs(db, `SELECT request_id || '|' || COALESCE(confirmed_at, 'NULL') || '|' || wake_attempts || '|' || wake_issue FROM remote_watches ORDER BY request_id`)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"dismissed|NULL|0|", "pending|NULL|0|", "queued|70|0|"}; !slices.Equal(got, want) {
		t.Errorf("watch confirmations = %q, want %q", got, want)
	}
}

// The columns each table had before v148, rendered as one string per row.
const (
	threadRequestColumnsBefore = `token || '|' || caller_thread_id || '|' || state || '|' || COALESCE(settled_at, 'NULL') || '|' ||
		COALESCE(delivered_at, 'NULL') || '|' || delivered_how || '|' || COALESCE(late_delivered_at, 'NULL') || '|' || notify || '|' ||
		wake_attempts || '|' || wake_next_check || '|' || wake_issue || '|' || lineage_only || '|' || updated_at`
	remoteWatchColumnsBefore = `computer_id || '|' || request_id || '|' || thread_id || '|' || notification || '|' || error || '|' ||
		next_check || '|' || receipt || '|' || created_at`
)
