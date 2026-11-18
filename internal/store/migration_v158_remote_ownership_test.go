package store

import (
	"slices"
	"testing"
)

// v158 adds the link a paired computer's owned spawn makes and what its
// owner owes it, and a remote request's admission deadline. Rows from
// before it are not links, owe nothing and have no deadline left; the new
// objects exist and enforce what they declare.
func TestRemoteOwnershipMigration(t *testing.T) {
	db := migrateThrough(t, remoteOwnershipMigrationVersion-1)
	mustExec(t, db, `INSERT INTO projects (id, path, name, created_at, updated_at) VALUES ('p', '/tmp/p', 'P', 1, 1)`)
	for _, id := range []string{"lead", "worker"} {
		mustExec(t, db, `INSERT INTO threads (id, project_id, title, provider, workspace_path, model, created_at, updated_at, archived, mode)
			VALUES (?, 'p', ?, 'claude', '/tmp/p', 'test-model', 1, 1, 0, 'chat')`, id, id)
	}
	mustExec(t, db, `INSERT INTO thread_request_receipts (token, owner_device_id, source_computer_id, source_thread_id, kind, target_thread_id, state, created_at, updated_at)
		VALUES ('old', 'dev', 'pc', 'lead', 'spawn', 'worker', 'finished', 5, 5)`)
	mustExec(t, db, `INSERT INTO thread_requests (token, caller_thread_id, kind, target_computer_id, target_thread_id, state, created_at, updated_at)
		VALUES ('old-spawn', 'lead', 'spawn', 'pc', 'w', 'finished', 5, 5)`)

	migrateFrom(t, db, remoteOwnershipMigrationVersion-1)

	var link, seen, receiptSeq, requestSeq, admitUntil int64
	if err := db.QueryRow(`SELECT spawn_link, owner_seen_at, owner_op_seq,
		(SELECT owner_op_seq FROM thread_requests WHERE token = 'old-spawn'),
		(SELECT admit_until FROM thread_requests WHERE token = 'old-spawn')
		FROM thread_request_receipts WHERE token = 'old'`).Scan(&link, &seen, &receiptSeq, &requestSeq, &admitUntil); err != nil {
		t.Fatal(err)
	}
	// An old request's deadline is past: v0.0.19 fenced its poll by
	// next_check alone, so an unknown answer once it is polled is final.
	if link != 0 || seen != 0 || receiptSeq != 0 || requestSeq != 0 || admitUntil != 0 {
		t.Fatalf("old rows read link %d seen %d seq %d/%d admit_until %d, want all 0", link, seen, receiptSeq, requestSeq, admitUntil)
	}
	var placement string
	if err := db.QueryRow(`SELECT placement FROM threads WHERE id = 'worker'`).Scan(&placement); err != nil || placement != PlacementStandalone {
		t.Fatalf("an old remote spawn's thread = %s, %v; want standalone", placement, err)
	}

	objects := map[string][]string{}
	rows, err := db.Query(`SELECT type, name FROM sqlite_master WHERE name IN (
		'idx_thread_request_receipts_spawn_link', 'idx_thread_request_receipts_owner_seen',
		'idx_thread_requests_remote_spawn_target', 'thread_owner_ops', 'idx_thread_owner_ops_due', 'idx_thread_owner_ops_computer',
		'trg_thread_placement_link_gone', 'trg_thread_placement_link_deleted')`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var kind, name string
		if err := rows.Scan(&kind, &name); err != nil {
			t.Fatal(err)
		}
		objects[kind] = append(objects[kind], name)
	}
	if err := rows.Close(); err != nil {
		t.Fatal(err)
	}
	for kind, want := range map[string][]string{
		"index":   {"idx_thread_owner_ops_computer", "idx_thread_owner_ops_due", "idx_thread_request_receipts_owner_seen", "idx_thread_request_receipts_spawn_link", "idx_thread_requests_remote_spawn_target"},
		"table":   {"thread_owner_ops"},
		"trigger": {"trg_thread_placement_link_deleted", "trg_thread_placement_link_gone"},
	} {
		got := objects[kind]
		slices.Sort(got)
		if !slices.Equal(got, want) {
			t.Errorf("%s objects = %v, want %v", kind, got, want)
		}
	}

	for name, write := range map[string]string{
		"a flag outside 0 and 1": `UPDATE thread_request_receipts SET spawn_link = 2 WHERE token = 'old'`,
		"an unknown operation":   `INSERT INTO thread_owner_ops (token, computer_id, source_thread_id, op, seq, owed_at, next_at) VALUES ('t', 'pc', 'lead', 'pause', 1, 1, 1)`,
		"a sent mark of 2":       `INSERT INTO thread_owner_ops (token, computer_id, source_thread_id, op, seq, sent, owed_at, next_at) VALUES ('t', 'pc', 'lead', 'archive', 1, 2, 1, 1)`,
	} {
		if _, err := db.Exec(write); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
	mustExec(t, db, `UPDATE thread_request_receipts SET spawn_link = 1 WHERE token = 'old'`)
	mustExec(t, db, `INSERT INTO thread_request_receipts (token, owner_device_id, source_computer_id, source_thread_id, kind, target_thread_id, state, created_at, updated_at, spawn_link)
		VALUES ('unlinked', 'dev', 'pc', 'lead', 'spawn', 'worker', 'finished', 6, 6, 0)`)
	if _, err := db.Exec(`INSERT INTO thread_request_receipts (token, owner_device_id, source_computer_id, source_thread_id, kind, target_thread_id, state, created_at, updated_at, spawn_link)
		VALUES ('second', 'dev', 'pc', 'lead', 'spawn', 'worker', 'finished', 6, 6, 1)`); err == nil {
		t.Fatal("a second link for one thread was accepted")
	}
}
