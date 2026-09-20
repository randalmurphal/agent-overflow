package store

import (
	"strings"
	"testing"
	"time"
)

// v104 adds the poller's retirement gate and the wake retry's clock to
// thread_requests. The rows a store already holds decide their own gate: a
// settled cross-computer request whose late-reply window has closed has
// nothing left to ask for, and one still inside it keeps being asked.
func TestThreadRequestPollingMigrationRetiresSpentRows(t *testing.T) {
	db := migrateThrough(t, 103)
	now := time.Now().UnixMilli()
	day := int64(24 * time.Hour / time.Millisecond)

	mustExec(t, db, `INSERT INTO projects (id, path, name, created_at, updated_at)
		VALUES ('p-poll', '/tmp/poll', 'Poll', 1, 1)`)
	mustExec(t, db, `
		INSERT INTO threads (id, project_id, title, provider, workspace_path, model,
			created_at, updated_at, archived, mode)
		VALUES ('t-poll', 'p-poll', 'Caller', 'claude', '/tmp/poll', 'test-model', 1, 1, 0, 'chat')`)

	seed := func(token, computer, state string, settledAt, expiresAt int64, lateReply any) {
		t.Helper()
		mustExec(t, db, `
			INSERT INTO thread_requests (token, caller_thread_id, kind, target_computer_id,
				state, settled_at, expires_at, late_reply, next_check, created_at, updated_at)
			VALUES (?, 't-poll', 'send', ?, ?, ?, ?, ?, 0, 1, 1)`,
			token, computer, state, nilIfZero(settledAt), nilIfZero(expiresAt), lateReply)
	}
	seed("tok-open", "backend-1", "running", 0, 0, nil)
	seed("tok-local", "", "finished", now-2*day, now-day, nil)
	seed("tok-inside-window", "backend-1", "finished", now, now+day, nil)
	seed("tok-window-closed", "backend-1", "finished", now-2*day, now-day, nil)
	// A destination that reported no deadline still held the answer for a
	// day, which is the fallback the poller assumes.
	seed("tok-no-deadline", "backend-1", "finished", now-2*day, 0, nil)
	seed("tok-late-collected", "backend-1", "finished", now, now+day, []byte("one more thing"))

	migrateFrom(t, db, 103)

	for token, want := range map[string]bool{
		"tok-open":           true,
		"tok-local":          true,
		"tok-inside-window":  true,
		"tok-window-closed":  false,
		"tok-no-deadline":    false,
		"tok-late-collected": false,
	} {
		var polling int
		if err := db.QueryRow(`SELECT polling FROM thread_requests WHERE token = ?`, token).Scan(&polling); err != nil {
			t.Fatalf("read polling for %s: %v", token, err)
		}
		if (polling != 0) != want {
			t.Errorf("%s polling = %d, want %v", token, polling, want)
		}
	}

	// The new columns carry the wake retry's own clock and the answering
	// thread's name, both defaulted for every row the store already held.
	var title, issue string
	var attempts, nextCheck int64
	if err := db.QueryRow(`SELECT target_thread_title, wake_attempts, wake_next_check, wake_issue
		FROM thread_requests WHERE token = 'tok-open'`).Scan(&title, &attempts, &nextCheck, &issue); err != nil {
		t.Fatalf("read the new columns: %v", err)
	}
	if title != "" || attempts != 0 || nextCheck != 0 || issue != "" {
		t.Errorf("defaults = %q/%d/%d/%q, want empty", title, attempts, nextCheck, issue)
	}

	// The due-row scan is gated on the new column, so a retired row leaves
	// the index rather than filling the poller's batch.
	if _, err := db.Exec(`UPDATE thread_requests SET polling = 2 WHERE token = 'tok-open'`); err == nil {
		t.Error("polling must be constrained to 0 or 1")
	}
	for _, index := range []string{"idx_thread_requests_poll", "idx_thread_requests_wake"} {
		var exists int
		if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'index' AND name = ?`,
			index).Scan(&exists); err != nil {
			t.Fatalf("probe %s: %v", index, err)
		}
		if exists == 0 {
			t.Errorf("index %s missing after migration", index)
		}
	}
}

// The poller's due query must use its partial index: a scan of the whole
// ledger every few seconds is what the index exists to prevent.
func TestDueThreadRequestPollsUsesItsPartialIndex(t *testing.T) {
	s := newTestStore(t)
	var plan string
	rows, err := s.reader().Query(`EXPLAIN QUERY PLAN
		SELECT token FROM thread_requests
		 WHERE polling = 1 AND target_computer_id <> ''
		   AND state IN ('unconfirmed','accepted','running','finished')
		   AND next_check <= ? ORDER BY next_check LIMIT 1`, nowMillis())
	if err != nil {
		t.Fatalf("explain: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id, parent, aux int
		var detail string
		if err := rows.Scan(&id, &parent, &aux, &detail); err != nil {
			t.Fatalf("scan plan: %v", err)
		}
		plan += detail + "\n"
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate plan: %v", err)
	}
	if !strings.Contains(plan, "idx_thread_requests_poll") {
		t.Fatalf("due poll query plan = %q", plan)
	}
}
