package store

import (
	"database/sql"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// assertPlanSearches fails a plan that scans table or reads it through an
// index outside allowed: every read of table must be a SEARCH through one
// of them.
func assertPlanSearches(t *testing.T, s *Store, what, table string, allowed []string, query string, args ...any) {
	t.Helper()
	plan := explainPlan(t, s, query, args...)
	text := planText(plan)
	reads := 0
	for _, r := range plan {
		fields := strings.Fields(r.detail)
		if len(fields) < 2 || (fields[0] != "SCAN" && fields[0] != "SEARCH") || fields[1] != table {
			continue
		}
		reads++
		if fields[0] == "SCAN" {
			t.Errorf("%s scans %s:\n%s", what, table, text)
			continue
		}
		if !slices.ContainsFunc(allowed, func(index string) bool { return strings.Contains(r.detail, " INDEX "+index+" ") }) {
			t.Errorf("%s reads %s through %q, want one of %v:\n%s", what, table, r.detail, allowed, text)
		}
	}
	if reads == 0 {
		t.Errorf("%s never reads %s:\n%s", what, table, text)
	}
}

// Every read of the Agents segment's tree, the listing's and a read of
// named rows, is a SEARCH through an index that leaves the caller's
// settled history out, each arm through its own, before and after ANALYZE
// has seen a history of 5,000 settled answers and 2,000 settled spawns:
// with the statistics a read the history could serve otherwise becomes a
// SCAN. Every arm is held to its index, so the plans do not change.
// idx_thread_requests_caller, which orders a caller's whole history, is not
// among them.
func TestThreadAgentTreeSearchesItsArms(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	chief, since := agentTreeSpawnHistory(t, s, 5_000, 200, 2_000)
	seedAgentRequest(t, s, "remote-open", chief, ThreadRequestSpawn, "", false, 1)
	mustExec(t, s.db, `UPDATE thread_requests SET target_computer_id = 'pc', target_thread_id = 'far' WHERE token = 'remote-open'`)
	settleAgentRequest(t, s, "remote-open", 1)
	if _, err := s.SetThreadRequestNestedOpen("remote-open", true); err != nil {
		t.Fatal(err)
	}
	arms := []string{
		"SEARCH r USING INDEX idx_thread_requests_caller_settled (caller_thread_id=? AND settled_at=?)",
		"SEARCH r USING INDEX idx_thread_requests_caller_settled (caller_thread_id=? AND settled_at>?)",
		"SEARCH r USING INDEX idx_thread_requests_spawn_caller (caller_thread_id=? AND target_thread_id=?)",
		"SEARCH r USING INDEX idx_thread_requests_nested_caller (caller_thread_id=?)",
	}
	named := []string{
		"SEARCH r USING INDEX sqlite_autoindex_thread_requests_1 (token=?)",
		"SEARCH r USING INDEX idx_thread_requests_spawn_settled (caller_thread_id=? AND settled_at=?)",
		"SEARCH r USING INDEX idx_thread_requests_spawn_settled (caller_thread_id=? AND settled_at>?)",
		"SEARCH r USING INDEX idx_thread_requests_spawn_caller (caller_thread_id=? AND target_thread_id=?)",
	}
	callers := []string{
		"SEARCH c USING INDEX idx_thread_requests_open (caller_thread_id>?)",
		"SEARCH c USING INDEX idx_thread_requests_nested_caller (caller_thread_id>?)",
	}
	arms, named = append(arms, callers...), append(named, callers...)
	for _, analyzed := range []bool{false, true} {
		if analyzed {
			mustExec(t, s.db, `ANALYZE`)
		}
		for _, read := range []struct {
			name  string
			query string
			args  []any
			want  []string
		}{
			{"threadAgentTreeSQL", threadAgentTreeSQL, []any{chief, "[]", since, 4, 4}, arms},
			{"threadAgentNamedTreeSQL", threadAgentNamedTreeSQL, []any{chief, "[]", since, 4, 4, `["recent-000050"]`}, named},
		} {
			name := fmt.Sprintf("%s (analyzed %v)", read.name, analyzed)
			assertPlanSearches(t, s, name, "r", []string{
				"idx_thread_requests_caller_settled", "idx_thread_requests_spawn_caller", "idx_thread_requests_spawn_settled",
				"idx_thread_requests_nested_caller", "idx_thread_requests_spawn_target", "sqlite_autoindex_thread_requests_1",
			}, read.query, read.args...)
			assertPlanSearches(t, s, name, "c", []string{
				"idx_thread_requests_caller_settled", "idx_thread_requests_spawn_caller", "idx_thread_requests_nested_caller",
				"idx_thread_requests_open",
			}, read.query, read.args...)
			text := planText(explainPlan(t, s, read.query, read.args...))
			for _, line := range read.want {
				if !strings.Contains(text, line) {
					t.Errorf("%s does not read %q:\n%s", name, line, text)
				}
			}
		}
	}
	assertPlanSearches(t, s, "HasOpenThreadRequests", "thread_requests", []string{"idx_thread_requests_caller_settled"},
		hasOpenThreadRequestsSQL, "lead")
}

// The four arms list what one predicate would: an open row, one settled
// inside the window, a settled spawn with open work under it locally or on
// another computer. A long settled history adds nothing.
func TestThreadAgentRequestsArmsAgainstAHistory(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	for _, id := range []string{"lead", "child", "grand", "done", "peer"} {
		mustCreateThread(t, s, id)
	}
	seedAgentRequest(t, s, "to-child", "lead", ThreadRequestSpawn, "child", false, 100)
	settleAgentRequest(t, s, "to-child", 200)
	seedAgentRequest(t, s, "to-grand", "child", ThreadRequestSpawn, "grand", false, 110)
	settleAgentRequest(t, s, "to-grand", 210)
	seedAgentRequest(t, s, "grand-open", "grand", ThreadRequestSend, "peer", true, 120)
	seedAgentRequest(t, s, "to-done", "lead", ThreadRequestSpawn, "done", false, 130)
	settleAgentRequest(t, s, "to-done", 230)
	seedAgentRequest(t, s, "remote-open", "lead", ThreadRequestSpawn, "", false, 140)
	mustExec(t, s.db, `UPDATE thread_requests SET target_computer_id = 'pc', target_thread_id = 'far' WHERE token = 'remote-open'`)
	settleAgentRequest(t, s, "remote-open", 240)
	if _, err := s.SetThreadRequestNestedOpen("remote-open", true); err != nil {
		t.Fatal(err)
	}
	seedAgentRequest(t, s, "recent", "lead", ThreadRequestSend, "peer", true, 150)
	settleAgentRequest(t, s, "recent", 5_000)
	seedAgentRequest(t, s, "open-send", "lead", ThreadRequestSend, "peer", true, 160)
	for i := range 300 {
		token := fmt.Sprintf("old-%d", i)
		seedAgentRequest(t, s, token, "lead", ThreadRequestSend, "peer", true, int64(1_000+i))
		settleAgentRequest(t, s, token, int64(2_000+i))
	}
	rows, more, err := s.ListThreadAgentTree(ThreadAgentTreeRead{Root: "lead", SettledSince: 4_000, Depth: 4, OpenDepth: 4, Limit: 64, AnswerHead: 1})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, row := range rows {
		got = append(got, row.Token)
	}
	want := []string{"to-child", "to-grand", "grand-open", "remote-open", "recent", "open-send"}
	if !slices.Equal(got, want) || more {
		t.Fatalf("listed %v more=%v; want %v", got, more, want)
	}
}

// GetThreadRequests binds its tokens as one JSON array: a list of any
// length runs the statement the first list compiled.
func TestGetThreadRequestsSharesOneStatement(t *testing.T) {
	s, counts := openObservedStore(t)
	mustCreateThread(t, s, "lead")
	for i := range 4 {
		seedThreadRequest(t, s, fmt.Sprintf("tok-%d", i), "lead", ThreadRequestSend, int64(i))
	}
	compiled := func() map[string]bool {
		texts := map[string]bool{}
		for key := range counts.snapshot() {
			if key.use == sqlPrepared && strings.Contains(key.query, "FROM thread_requests") {
				texts[key.query] = true
			}
		}
		return texts
	}
	got, err := s.GetThreadRequests([]string{"tok-0"})
	if err != nil || len(got) != 1 {
		t.Fatalf("one token: %d rows, %v", len(got), err)
	}
	first := compiled()
	for n := 2; n <= 4; n++ {
		tokens := []string{"missing"}
		for i := range n {
			tokens = append(tokens, fmt.Sprintf("tok-%d", i))
		}
		got, err := s.GetThreadRequests(tokens)
		if err != nil || len(got) != n {
			t.Fatalf("%d tokens: %d rows, %v", n, len(got), err)
		}
	}
	for text := range compiled() {
		if !first[text] {
			t.Errorf("a longer token list compiled a statement the first did not:\n%s", text)
		}
	}
}

// With nothing past the floor the sweep reads the oldest ages through
// their indexes and writes nothing: no statement, no transaction.
func TestThreadRequestSweepSkipsWithNothingPastTheFloor(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	mustCreateThread(t, s, "lead")
	for i := range 50 {
		token := fmt.Sprintf("tok-%d", i)
		seedAgentRequest(t, s, token, "lead", ThreadRequestSend, "peer", false, 1_000+int64(i))
		settleAgentRequest(t, s, token, 2_000+int64(i))
	}
	writes, commits := s.WriteCount(), s.CommitCount()
	swept, err := s.DeleteThreadRequestsBefore(1_500)
	if err != nil || swept != 0 {
		t.Fatalf("sweep = %d, %v", swept, err)
	}
	if w, c := s.WriteCount()-writes, s.CommitCount()-commits; w != 0 || c != 0 {
		t.Fatalf("a sweep with nothing due wrote %d statements in %d transactions", w, c)
	}
	probe := `SELECT EXISTS (SELECT 1 FROM thread_requests WHERE ` + requestPastFloorSQL + `)`
	assertPlanSearches(t, s, "the request floor probe", "thread_requests", []string{"idx_thread_requests_age"}, probe, int64(1))
	probe = `SELECT EXISTS (SELECT 1 FROM thread_request_receipts WHERE ` + receiptPastFloorSQL + `)`
	assertPlanSearches(t, s, "the receipt floor probe", "thread_request_receipts", []string{"idx_thread_request_receipts_age"}, probe, int64(1))
	// The lineage probe walks the lineage rows alone, one per spawned
	// thread that outlived the floor, in their own index.
	probe = `SELECT EXISTS (SELECT 1 FROM thread_requests WHERE ` + orphanLineageSQL + `)`
	for _, r := range explainPlan(t, s, probe) {
		if strings.Contains(r.detail, " thread_requests") && r.detail != "SCAN thread_requests USING COVERING INDEX idx_thread_requests_lineage" {
			t.Errorf("the lineage probe reads %q, want only idx_thread_requests_lineage", r.detail)
		}
	}
	assertPlanSearches(t, s, "the request delete", "thread_requests", []string{"idx_thread_requests_age"},
		`DELETE FROM thread_requests WHERE `+requestPastFloorSQL+` AND NOT (`+spawnLineageSQL+`)`, int64(1))
}

// Past the floor the sweep still does all it did: a settled request and
// its receipt go, a spawn whose thread exists is kept as lineage, an open
// request stays, and a lineage row whose thread is gone goes.
func TestThreadRequestSweepAfterTheSkip(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	for _, id := range []string{"lead", "child", "gone"} {
		mustCreateThread(t, s, id)
	}
	seedAgentRequest(t, s, "old-send", "lead", ThreadRequestSend, "child", false, 10)
	settleAgentRequest(t, s, "old-send", 20)
	if _, _, err := s.AcceptThreadRequestReceipt(ThreadRequestReceipt{Token: "old-send", OwnerDeviceID: "local", Kind: ThreadRequestSend, TargetThreadID: "child", CreatedAt: 10}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SettleLocalThreadRequest("old-send", ThreadReceiptOpenStates(), ThreadRequestSettlement{State: ThreadReceiptFinished, SettledAt: 20}, ThreadRequestFinished); err != nil {
		t.Fatal(err)
	}
	seedAgentRequest(t, s, "old-spawn", "lead", ThreadRequestSpawn, "child", false, 10)
	settleAgentRequest(t, s, "old-spawn", 20)
	seedAgentRequest(t, s, "orphan-spawn", "lead", ThreadRequestSpawn, "gone", false, 10)
	settleAgentRequest(t, s, "orphan-spawn", 20)
	seedAgentRequest(t, s, "open", "lead", ThreadRequestSend, "child", false, 10)

	swept, err := s.DeleteThreadRequestsBefore(1_000)
	if err != nil {
		t.Fatal(err)
	}
	// old-send, its receipt and orphan-spawn (its thread exists yet, so
	// it is kept as lineage first) plus old-spawn kept.
	if swept != 4 {
		t.Errorf("swept = %d, want old-send, its receipt and the two spawns kept as lineage", swept)
	}
	if err := s.DeleteThread("gone"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DeleteThreadRequestsBefore(1_000); err != nil {
		t.Fatal(err)
	}
	var tokens []string
	rows, err := s.db.Query(`SELECT token || ':' || lineage_only FROM thread_requests ORDER BY token`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var token string
		if err := rows.Scan(&token); err != nil {
			t.Fatal(err)
		}
		tokens = append(tokens, token)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if want := []string{"old-spawn:1", "open:0"}; !slices.Equal(tokens, want) {
		t.Errorf("rows left = %v, want %v", tokens, want)
	}
	if _, found, err := s.GetThreadRequestReceipt("old-send"); err != nil || found {
		t.Errorf("the receipt past the floor stayed: %v %v", found, err)
	}
}

// Every per-thread read and write of agent_wakes goes through
// idx_agent_wakes_thread.
func TestAgentWakesSearchByThread(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	for what, query := range map[string]string{
		"the live count":     `SELECT count(*) FROM agent_wakes WHERE thread_id = ? AND state <> 'cancelled'`,
		"the label lookup":   `SELECT id FROM agent_wakes WHERE thread_id = ? AND label = 'x' ORDER BY created_at LIMIT 1`,
		"the listing":        `SELECT id FROM agent_wakes WHERE thread_id = ? AND state IN ('armed','running') ORDER BY label`,
		"the thread cancel":  `UPDATE agent_wakes SET state = 'cancelled' WHERE thread_id = ? AND state IN ('armed','running')`,
		"the cancelled cap":  `DELETE FROM agent_wakes WHERE thread_id = ? AND state = 'cancelled' AND id NOT IN (SELECT id FROM agent_wakes WHERE thread_id = ? AND state = 'cancelled' ORDER BY created_at DESC, id DESC LIMIT 16)`,
		"the thread cascade": `DELETE FROM agent_wakes WHERE thread_id = ?`,
	} {
		args := []any{"t"}
		if strings.Count(query, "?") == 2 {
			args = append(args, "t")
		}
		assertPlanSearches(t, s, what, "agent_wakes", []string{"idx_agent_wakes_thread"}, query, args...)
	}
}

// A cancel keeps none of what its wakes carried, and a thread keeps its
// newest MaxCancelledAgentWakesPerThread cancelled rows, by label or all at
// once.
func TestCancelledAgentWakesAreBoundedAndEmpty(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	mustCreateThread(t, s, "t")
	register := func(label string, at int64) AgentWake {
		t.Helper()
		row, err := s.RegisterAgentWake(AgentWake{ID: "w-" + label, ThreadID: "t", Kind: AgentWakeWatch, Label: label,
			Command: "true", IntervalSeconds: 60, NextAt: 1, CreatedAt: at})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.NoteAgentWakeUndelivered(row.ID, "not delivered", strings.Repeat("o", 1024)); err != nil {
			t.Fatal(err)
		}
		if _, err := s.NoteAgentWakeStderr(row.ID, "err"); err != nil {
			t.Fatal(err)
		}
		return row
	}
	for i := range MaxCancelledAgentWakesPerThread + 4 {
		label := fmt.Sprintf("l%02d", i)
		register(label, int64(i+1))
		if _, cancelled, err := s.CancelAgentWake("t", label, "", "cancelled"); err != nil || !cancelled {
			t.Fatalf("cancel %s: %v %v", label, cancelled, err)
		}
	}
	cancelledRows := func() int {
		t.Helper()
		var n int
		if err := s.db.QueryRow(`SELECT count(*) FROM agent_wakes WHERE thread_id = 't' AND state = 'cancelled'`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	// Each cancel by label prunes on its own.
	if n := cancelledRows(); n != MaxCancelledAgentWakesPerThread {
		t.Fatalf("after cancels by label the thread keeps %d cancelled rows, want %d", n, MaxCancelledAgentWakesPerThread)
	}
	for i := range 3 {
		register(fmt.Sprintf("bulk%d", i), int64(100+i))
	}
	if ids, err := s.CancelAgentWakesForThread("t", "", "archived"); err != nil || len(ids) != 3 {
		t.Fatalf("thread cancel = %v, %v", ids, err)
	}
	var count, kept int
	if err := s.db.QueryRow(`SELECT count(*), coalesce(sum(length(last_output) + length(last_stderr)), 0) FROM agent_wakes WHERE thread_id = 't'`).Scan(&count, &kept); err != nil {
		t.Fatal(err)
	}
	if count != MaxCancelledAgentWakesPerThread || kept != 0 {
		t.Fatalf("thread keeps %d cancelled rows with %d bytes of output, want %d and none", count, kept, MaxCancelledAgentWakesPerThread)
	}
	// The newest stay: the three cancelled together and the latest by label.
	if _, found, err := s.GetAgentWake("w-bulk2"); err != nil || !found {
		t.Fatalf("the newest cancelled row went: %v %v", found, err)
	}
	if _, found, err := s.GetAgentWake("w-l00"); err != nil || found {
		t.Fatalf("the oldest cancelled row stayed: %v %v", found, err)
	}
	// A kept label still revives with its id.
	if row, err := s.RegisterAgentWake(AgentWake{ID: "fresh", ThreadID: "t", Kind: AgentWakeLoop, Label: "l19", IntervalSeconds: 60, NextAt: 1}); err != nil || row.ID != "w-l19" {
		t.Fatalf("revive = %+v, %v", row, err)
	}
}

// A watch goes with the thread that receives its completion, so a deleted
// thread's pending watch is not in the watcher's due list.
func TestRemoteWatchesGoWithTheirThread(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	threadID := uuid.NewString()
	mustCreateThread(t, s, threadID)
	w := RemoteWatch{ComputerID: uuid.NewString(), RequestID: uuid.NewString(), ThreadID: threadID, Fingerprint: strings.Repeat("a", 64), Label: "job"}
	if _, err := s.RegisterRemoteWatch(w); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteThread(threadID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetRemoteWatch(w.ComputerID, w.RequestID); err != sql.ErrNoRows {
		t.Fatalf("the watch outlived its thread: %v", err)
	}
	due, err := s.ListRemoteWatches("", 1<<62, 32)
	if err != nil || len(due) != 0 {
		t.Fatalf("due = %+v, %v", due, err)
	}
	if _, err := s.RegisterRemoteWatch(RemoteWatch{ComputerID: uuid.NewString(), RequestID: uuid.NewString(), ThreadID: uuid.NewString(), Fingerprint: strings.Repeat("a", 64)}); err == nil {
		t.Fatal("a watch was registered for a thread that does not exist")
	}
}

// The sweep deletes a watch with nothing left to deliver once it is past
// the floor; a pending watch or an unconfirmed completion stays. With
// nothing past it, it writes nothing.
func TestRemoteWatchRetention(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	threadID := uuid.NewString()
	mustCreateThread(t, s, threadID)
	watch := func(notification string, confirmed any, created int64) string {
		t.Helper()
		id := uuid.NewString()
		mustExec(t, s.db, `INSERT INTO remote_watches (computer_id, request_id, thread_id, launched_in_thread_id, fingerprint, label, notification, confirmed_at, created_at)
			VALUES ('pc', ?, ?, ?, ?, 'job', ?, ?, ?)`, id, threadID, threadID, strings.Repeat("a", 64), notification, confirmed, created)
		return id
	}
	old := []string{watch("dismissed", nil, 10), watch("queued", int64(20), 10)}
	kept := []string{watch("pending", nil, 10), watch("queued", nil, 10), watch("dismissed", nil, 5_000)}
	writes, commits := s.WriteCount(), s.CommitCount()
	if swept, err := s.DeleteRemoteWatchesBefore(5); err != nil || swept != 0 {
		t.Fatalf("early sweep = %d, %v", swept, err)
	}
	if w, c := s.WriteCount()-writes, s.CommitCount()-commits; w != 0 || c != 0 {
		t.Fatalf("a sweep with nothing due wrote %d statements in %d transactions", w, c)
	}
	if swept, err := s.DeleteRemoteWatchesBefore(1_000); err != nil || swept != int64(len(old)) {
		t.Fatalf("sweep = %d, %v; want %d", swept, err, len(old))
	}
	for _, id := range old {
		if _, err := s.GetRemoteWatch("pc", id); err != sql.ErrNoRows {
			t.Errorf("finished watch %s stayed: %v", id, err)
		}
	}
	for _, id := range kept {
		if _, err := s.GetRemoteWatch("pc", id); err != nil {
			t.Errorf("watch %s went: %v", id, err)
		}
	}
	assertPlanSearches(t, s, "the watch sweep", "remote_watches", []string{"idx_remote_watches_done"},
		`DELETE FROM remote_watches WHERE `+remoteWatchDoneSQL+` AND created_at < ?`, int64(1))
}
