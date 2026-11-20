package store

import "testing"

// v161 adds a link's nudge run columns and the index of the open ones. A
// receipt from before it keeps every column it had and has no nudge.
func TestReceiptNudgeRunsMigration(t *testing.T) {
	db := migrateThrough(t, receiptNudgeRunsMigrationVersion-1)
	mustExec(t, db, `INSERT INTO thread_request_receipts (token, owner_device_id, source_computer_id, source_thread_id, kind, state, created_at, updated_at)
		VALUES ('old-link', 'dev', 'pc', 'lead', 'spawn', 'finished', 5, 5)`)

	migrateFrom(t, db, receiptNudgeRunsMigrationVersion-1)

	var state string
	var token, open any
	if err := db.QueryRow(`SELECT state, nudge_token, nudge_open FROM thread_request_receipts WHERE token = 'old-link'`).Scan(&state, &token, &open); err != nil {
		t.Fatal(err)
	}
	if state != "finished" || token != nil || open != nil {
		t.Fatalf("the old receipt reads %s, nudge %v open %v; want it as it was, with no nudge", state, token, open)
	}
	var columns int
	if err := db.QueryRow(`SELECT count(*) FROM pragma_table_info('thread_request_receipts') WHERE name LIKE 'nudge\_%' ESCAPE '\'`).Scan(&columns); err != nil || columns != 9 {
		t.Fatalf("thread_request_receipts has %d nudge columns, %v; want 9", columns, err)
	}
	var index string
	if err := db.QueryRow(`SELECT sql FROM sqlite_master WHERE type = 'index' AND name = 'idx_thread_request_receipts_nudge_open'`).Scan(&index); err != nil || index == "" {
		t.Fatalf("idx_thread_request_receipts_nudge_open: %q, %v", index, err)
	}
}

// peerRestedWorker is w, owned through a paired computer's link "link" whose
// spawn's run ended with the worker at 3 calls, 30 tokens and 300 ms.
func peerRestedWorker(t *testing.T, s *Store) {
	t.Helper()
	mustCreateThread(t, s, "w")
	acceptLink(t, s, "link", "dev", "w", 10)
	if _, err := s.SetThreadPlacement([]string{"w"}, PlacementOwned); err != nil {
		t.Fatal(err)
	}
	mustExec(t, s.db, `INSERT INTO thread_tallies (thread_id, tool_calls, tokens, turn_ms) VALUES ('w', 3, 30, 300)
		ON CONFLICT(thread_id) DO UPDATE SET tool_calls = 3, tokens = 30, turn_ms = 300`)
	mustExec(t, s.db, `UPDATE thread_request_receipts SET state = 'finished', settled_at = 20,
		run_open = 0, run_tool_calls = 3, run_tokens = 30, run_turn_ms = 300 WHERE token = 'link'`)
}

// peerNudge reads the link's latest nudge run as its owner's answer does.
func peerNudge(t *testing.T, s *Store) *NudgeRun {
	t.Helper()
	run, err := s.ReceiptRunOf("dev", "lead-link", "w")
	if err != nil {
		t.Fatal(err)
	}
	return run.Nudge
}

// A paired computer's owned worker here is nudged by the rule a local one
// is: a message from the person or a thread other than its spawner there,
// with no run of the spawner's open on it, records a run on the spawn's
// link, its base the worker's figures then; one while it is open joins it.
// The spawner's own message, a run of the spawner's open on a receipt, and
// a worker with a sidebar row record none. The owner's read reports the
// nudge apart from the spawner's own runs.
func TestAPeerSpawnsLinkRecordsANudgeOfItsWorker(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	peerRestedWorker(t, s)
	start := func(token, by string) bool {
		t.Helper()
		started, err := s.StartNudgeRun(token, "w", by)
		if err != nil {
			t.Fatalf("StartNudgeRun(%s): %v", token, err)
		}
		return started
	}
	if start("by-spawner", "lead-link") {
		t.Fatal("the spawner's own message recorded a nudge")
	}
	mustExec(t, s.db, `UPDATE thread_request_receipts SET run_open = 1 WHERE token = 'link'`)
	if start("in-run", "") {
		t.Fatal("a nudge during the spawner's run recorded a run of its own")
	}
	mustExec(t, s.db, `UPDATE thread_request_receipts SET run_open = 0 WHERE token = 'link'`)
	if _, err := s.SetThreadPlacement([]string{"w"}, PlacementStandalone); err != nil {
		t.Fatal(err)
	}
	if start("sidebar", "") {
		t.Fatal("a worker with a sidebar row recorded a nudge")
	}
	if _, err := s.SetThreadPlacement([]string{"w"}, PlacementOwned); err != nil {
		t.Fatal(err)
	}
	if nudge := peerNudge(t, s); nudge != nil {
		t.Fatalf("the link reports nudge %+v before any", nudge)
	}

	if !start("nudge", "") {
		t.Fatal("the person's message recorded no nudge run")
	}
	nudge := peerNudge(t, s)
	if nudge == nil || nudge.Token != "nudge" || nudge.End != nil || nudge.StartedAt == 0 ||
		nudge.Base != (RunFigures{ToolCalls: 3, Tokens: 30, TurnMs: 300}) {
		t.Fatalf("nudge %+v; want it open from the worker's figures then", nudge)
	}
	if run, err := s.ReceiptRunOf("dev", "lead-link", "w"); err != nil || run.Open || run.End == nil {
		t.Fatalf("the spawner's own runs %+v, %v; want them as they were, ended", run, err)
	}
	if start("joined", "another") {
		t.Fatal("a second nudge while one is open recorded a run of its own")
	}
	if threads, any, err := s.OpenRunThreads(); err != nil || !any || len(threads) != 1 || threads[0] != "w" {
		t.Fatalf("OpenRunThreads = %v %v, %v; want the nudged worker", threads, any, err)
	}
	if ending, err := s.ThreadRunsEnding("w"); err != nil || !ending {
		t.Fatalf("ThreadRunsEnding = %v, %v; want the nudge's run", ending, err)
	}
	if listed, err := s.ListThreadRequestReceiptsForThread("w"); err != nil || len(listed) != 1 {
		t.Fatalf("receipts %+v, %v; want the link alone, a nudge being no request", listed, err)
	}

	if err := s.DropNudgeRun("nudge"); err != nil {
		t.Fatal(err)
	}
	if nudge := peerNudge(t, s); nudge != nil {
		t.Fatalf("the dropped nudge is still reported: %+v", nudge)
	}
}

// A link's nudge run ends at its worker's rest, with the worker's figures
// over its life then, and so do the spawner's runs on its receipts.
func TestAPeerNudgeRunEndsAtItsWorkersRest(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	peerRestedWorker(t, s)
	if started, err := s.StartNudgeRun("nudge", "w", ""); err != nil || !started {
		t.Fatalf("StartNudgeRun = %v, %v", started, err)
	}
	mustExec(t, s.db, `UPDATE thread_tallies SET tool_calls = 5, tokens = 90, turn_ms = 700 WHERE thread_id = 'w'`)
	rest := &RestGuard{Roots: []string{"w"}, AtRest: func(SubtreeRest) []string { return []string{"w"} }}
	if ended, err := s.EndThreadRequestRunsAtRest("w", rest); err != nil || !ended {
		t.Fatalf("EndThreadRequestRunsAtRest = %v, %v", ended, err)
	}
	nudge := peerNudge(t, s)
	if nudge == nil || nudge.End == nil || *nudge.End != (RunFigures{ToolCalls: 5, Tokens: 90, TurnMs: 700}) {
		t.Fatalf("nudge %+v; want it ended with the worker's figures at its rest", nudge)
	}
	if ending, err := s.ThreadRunsEnding("w"); err != nil || ending {
		t.Fatalf("ThreadRunsEnding = %v, %v; want none open", ending, err)
	}
}

// A link's open nudge run ends, with the worker's figures then, when the
// worker gets a sidebar row here or a group, and when the link ends.
func TestAPeerNudgeRunEndsWithTheWorkersOwnership(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name string
		end  func(t *testing.T, s *Store)
	}{
		{"shown in the sidebar", func(t *testing.T, s *Store) {
			if _, err := s.SetThreadPlacement([]string{"w"}, PlacementStandalone); err != nil {
				t.Fatal(err)
			}
		}},
		{"grouped", func(t *testing.T, s *Store) {
			thread, err := s.GetThread("w")
			if err != nil {
				t.Fatal(err)
			}
			group, err := s.CreateThreadGroup(thread.ProjectID, "Sweep")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.SetThreadGroup([]string{"w"}, group.ID); err != nil {
				t.Fatal(err)
			}
		}},
		{"its link released", func(t *testing.T, s *Store) {
			if released, err := s.ReleaseSpawnLink("link", "dev", "lead-link"); err != nil || len(released) != 1 {
				t.Fatalf("ReleaseSpawnLink = %v, %v", released, err)
			}
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			s := newTestStore(t)
			peerRestedWorker(t, s)
			if started, err := s.StartNudgeRun("nudge", "w", ""); err != nil || !started {
				t.Fatalf("StartNudgeRun = %v, %v", started, err)
			}
			mustExec(t, s.db, `UPDATE thread_tallies SET tool_calls = 4 WHERE thread_id = 'w'`)
			c.end(t, s)
			nudge := peerNudge(t, s)
			if nudge == nil || nudge.End == nil || nudge.End.ToolCalls != 4 {
				t.Fatalf("nudge %+v; want it ended with the worker's figures then", nudge)
			}
			if ending, err := s.ThreadRunsEnding("w"); err != nil || ending {
				t.Fatalf("ThreadRunsEnding = %v, %v; want none open", ending, err)
			}
		})
	}
}

// The spawner's computer records a paired computer's nudge run as the
// spawn's caller's nudge row, a settled send that owes nothing, its base
// what that computer reported; the row ends with the run's figures less
// its base once that computer says the run ended, and an answer repeating
// either writes nothing. A nudge of an archived caller, or older than the
// retention floor, records nothing. An end of the spawner's runs there
// leaves the nudge's row to the nudge's own word.
func TestTheSpawnersComputerRecordsAPeerNudgeRun(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	mustCreateThread(t, s, "lead")
	insertRun(t, s, "spawn", ThreadRequestSpawn, "pc", "w", &RunFigures{}, 10)
	settleAgentRequest(t, s, "spawn", 20)
	if err := s.EndRemoteThreadRequestRuns("pc", "w", RunFigures{ToolCalls: 3, Tokens: 30, TurnMs: 300}); err != nil {
		t.Fatal(err)
	}
	now := nowMillis()
	run := NudgeRun{Token: "nudge", StartedAt: now, Base: RunFigures{ToolCalls: 3, Tokens: 30, TurnMs: 300}}
	record := func(run NudgeRun) bool {
		t.Helper()
		wrote, err := s.RecordRemoteNudgeRun("spawn", run)
		if err != nil {
			t.Fatal(err)
		}
		return wrote
	}
	if !record(run) {
		t.Fatal("the nudge run was not recorded")
	}
	row := rawThreadRequest(t, s, "nudge")
	if !row.Nudge || row.CallerThreadID != "lead" || row.Kind != ThreadRequestSend || row.TargetComputerID != "pc" ||
		row.TargetThreadID != "w" || row.State != ThreadRequestFinished || row.SettledAt == 0 || row.Notify ||
		!row.RunOpen() || row.RunToken != "nudge" || *row.RunBase != run.Base {
		t.Fatalf("nudge row %+v; want a settled, unowed send of the spawner's with its run open from the reported base", row)
	}
	if listed, err := s.ListThreadRequestsByCaller("lead", 10, 0); err != nil || len(listed) != 1 {
		t.Fatalf("ListThreadRequestsByCaller = %+v, %v; want the spawn alone", listed, err)
	}
	if record(run) {
		t.Fatal("a repeated open nudge wrote again")
	}
	if err := s.EndRemoteThreadRequestRuns("pc", "w", RunFigures{ToolCalls: 3, Tokens: 30, TurnMs: 300}); err != nil {
		t.Fatal(err)
	}
	if rawThreadRequest(t, s, "nudge").RunEnded() {
		t.Fatal("the end of the spawner's own runs ended the nudge's")
	}
	run.End = &RunFigures{ToolCalls: 5, Tokens: 90, TurnMs: 700}
	if !record(run) {
		t.Fatal("the nudge's end was not recorded")
	}
	if total := rawThreadRequest(t, s, "nudge").RunTotal; total == nil || *total != (RunFigures{ToolCalls: 2, Tokens: 60, TurnMs: 400}) {
		t.Fatalf("nudge run %+v; want its figures less its base", total)
	}
	if record(run) {
		t.Fatal("a repeated ended nudge wrote again")
	}

	ended := NudgeRun{Token: "seen-ended", StartedAt: now, Base: RunFigures{ToolCalls: 5}, End: &RunFigures{ToolCalls: 6}}
	if !record(ended) || !rawThreadRequest(t, s, "seen-ended").RunEnded() {
		t.Fatal("a nudge first heard of once ended was not recorded ended")
	}
	old := NudgeRun{Token: "old", StartedAt: now - int64(ThreadRequestRetentionDays+1)*24*60*60*1000, End: &RunFigures{}}
	if record(old) {
		t.Fatal("a nudge older than the retention floor was recorded")
	}
	mustExec(t, s.db, `UPDATE threads SET archived = 1 WHERE id = 'lead'`)
	if record(NudgeRun{Token: "archived", StartedAt: now}) {
		t.Fatal("a nudge of an archived spawner was recorded")
	}
}
