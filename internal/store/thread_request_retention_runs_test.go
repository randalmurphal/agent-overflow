package store

import "testing"

// rowExists reports whether table holds a row keyed token.
func rowExists(t *testing.T, s *Store, table, token string) bool {
	t.Helper()
	var found bool
	if err := s.db.QueryRow(`SELECT EXISTS (SELECT 1 FROM `+table+` WHERE token = ?)`, token).Scan(&found); err != nil {
		t.Fatal(err)
	}
	return found
}

// sweepAt runs the retention sweep at floor and reports how many
// statements and transactions it wrote.
func sweepAt(t *testing.T, s *Store, floor int64) (writes, commits uint64) {
	t.Helper()
	writes, commits = s.WriteCount(), s.CommitCount()
	if _, _, err := s.DeleteThreadRequestsBefore(floor, everySpawnRests(t, s, floor)); err != nil {
		t.Fatal(err)
	}
	return s.WriteCount() - writes, s.CommitCount() - commits
}

// A request row whose run is open is still work: retention deletes neither
// a nudge's row, settled when written, nor a settled send of the
// spawner's, however old, and with only those past the floor the sweep
// writes nothing. Once each run has ended the row is past the floor like
// any other and goes.
func TestRetentionKeepsARequestWhoseRunIsOpen(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	for _, id := range []string{"lead", "w", "w2"} {
		mustCreateThread(t, s, id)
	}
	for _, spawn := range []struct{ token, thread string }{{"spawn", "w"}, {"spawn2", "w2"}} {
		seedAgentRequest(t, s, spawn.token, "lead", ThreadRequestSpawn, spawn.thread, false, 100)
		settleAgentRequest(t, s, spawn.token, 200)
		mustExec(t, s.db, `UPDATE thread_requests SET run_tool_calls = 0, run_tokens = 0, run_turn_ms = 0 WHERE token = ?`, spawn.token)
	}
	if _, err := s.SetThreadPlacement([]string{"w", "w2"}, PlacementOwned); err != nil {
		t.Fatal(err)
	}
	if !startNudge(t, s, "nudge", "") {
		t.Fatal("the nudge recorded no run")
	}
	insertRun(t, s, "send", ThreadRequestSend, "", "w2", &RunFigures{}, 120)
	settleRun(t, s, "send")
	if !rawThreadRequest(t, s, "send").RunOpen() {
		t.Fatal("the settled send's run is not open")
	}
	const floor = 150
	mustExec(t, s.db, `UPDATE thread_requests SET created_at = 1, updated_at = 1, settled_at = 1 WHERE token IN ('nudge', 'send')`)

	if w, c := sweepAt(t, s, floor); w != 0 || c != 0 {
		t.Fatalf("a sweep with only open runs past the floor wrote %d statements in %d transactions", w, c)
	}
	for _, token := range []string{"nudge", "send"} {
		if !rowExists(t, s, "thread_requests", token) {
			t.Fatalf("the sweep deleted %s while its run is open", token)
		}
	}

	for _, thread := range []string{"w", "w2"} {
		rest := &RestGuard{Roots: []string{thread}, AtRest: func(SubtreeRest) []string { return []string{thread} }}
		if ended, err := s.EndThreadRequestRunsAtRest(thread, rest); err != nil || !ended {
			t.Fatalf("EndThreadRequestRunsAtRest(%s) = %v, %v", thread, ended, err)
		}
	}
	sweepAt(t, s, floor)
	for _, token := range []string{"nudge", "send"} {
		if rowExists(t, s, "thread_requests", token) {
			t.Fatalf("the sweep kept %s past the floor once its run ended", token)
		}
	}
}

// A paired computer's receipt whose run is open here is kept past the
// floor, and goes once its run has ended.
func TestRetentionKeepsAReceiptWhoseRunIsOpen(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	mustCreateThread(t, s, "w")
	if _, created, err := s.AcceptThreadRequestReceipt(ThreadRequestReceipt{
		Token: "send", OwnerDeviceID: "dev", SourceComputerID: "pc", SourceThreadID: "lead",
		Kind: ThreadRequestSend, TargetThreadID: "w", CreatedAt: 1,
	}); err != nil || !created {
		t.Fatalf("accept: %v %v", created, err)
	}
	const floor = 150
	mustExec(t, s.db, `UPDATE thread_request_receipts SET state = 'finished', settled_at = 1, updated_at = 1,
		run_open = 1 WHERE token = 'send'`)
	if w, c := sweepAt(t, s, floor); w != 0 || c != 0 {
		t.Fatalf("a sweep with only an open run past the floor wrote %d statements in %d transactions", w, c)
	}
	if !rowExists(t, s, "thread_request_receipts", "send") {
		t.Fatal("the sweep deleted a receipt whose run is open")
	}
	mustExec(t, s.db, `UPDATE thread_request_receipts SET run_open = 0, run_tool_calls = 1, run_tokens = 1, run_turn_ms = 1 WHERE token = 'send'`)
	sweepAt(t, s, floor)
	if rowExists(t, s, "thread_request_receipts", "send") {
		t.Fatal("the sweep kept the receipt past the floor once its run ended")
	}
}

// A paired computer's open nudge run is recorded here however long ago it
// started on that computer's clock, and kept while it is open; once that
// computer reports its end, the row is past the floor and goes.
func TestARemoteOpenNudgeRunOlderThanTheFloorIsRecordedAndKept(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	mustCreateThread(t, s, "lead")
	insertRun(t, s, "spawn", ThreadRequestSpawn, "pc", "w", &RunFigures{}, 10)
	settleAgentRequest(t, s, "spawn", 20)
	if err := s.EndRemoteThreadRequestRuns("pc", "w", RunFigures{}); err != nil {
		t.Fatal(err)
	}
	day := int64(24 * 60 * 60 * 1000)
	floor := nowMillis() - int64(ThreadRequestRetentionDays)*day
	run := NudgeRun{Token: "nudge", StartedAt: floor - day}
	if wrote, err := s.RecordRemoteNudgeRun("spawn", run); err != nil || !wrote {
		t.Fatalf("RecordRemoteNudgeRun = %v, %v; want the open run recorded", wrote, err)
	}
	sweepAt(t, s, floor)
	if !rowExists(t, s, "thread_requests", "nudge") {
		t.Fatal("the sweep deleted the open remote nudge run")
	}
	run.End = &RunFigures{ToolCalls: 2}
	if wrote, err := s.RecordRemoteNudgeRun("spawn", run); err != nil || !wrote {
		t.Fatalf("RecordRemoteNudgeRun = %v, %v; want the end recorded", wrote, err)
	}
	sweepAt(t, s, floor)
	if rowExists(t, s, "thread_requests", "nudge") {
		t.Fatal("the sweep kept the ended remote nudge run past the floor")
	}
}
