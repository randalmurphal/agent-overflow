package store

import (
	"testing"
)

// v159 adds a request's run columns, a receipt's, and the indexes of open runs. A row from
// before it keeps every column it had and has no run: it reads as rows did.
func TestRequestRunsMigration(t *testing.T) {
	db := migrateThrough(t, requestRunsMigrationVersion-1)
	mustExec(t, db, `INSERT INTO projects (id, path, name, created_at, updated_at) VALUES ('p', '/tmp/p', 'P', 1, 1)`)
	mustExec(t, db, `INSERT INTO threads (id, project_id, title, provider, workspace_path, model, created_at, updated_at, archived, mode)
		VALUES ('lead', 'p', 'lead', 'claude', '/tmp/p', 'test-model', 1, 1, 0, 'chat')`)
	mustExec(t, db, `INSERT INTO thread_requests (token, caller_thread_id, kind, target_thread_id, state, answer, answer_kind, settled_at, created_at, updated_at)
		VALUES ('old-spawn', 'lead', 'spawn', 'worker', 'finished', 'done', 'final', 7, 5, 7)`)
	mustExec(t, db, `INSERT INTO thread_request_receipts (token, owner_device_id, source_computer_id, source_thread_id, kind, target_thread_id, state, created_at, updated_at)
		VALUES ('old-receipt', 'dev', 'pc', 'lead', 'spawn', 'lead', 'running', 5, 5)`)

	migrateFrom(t, db, requestRunsMigrationVersion-1)

	var state, answer string
	var settled int64
	var base, total *int64
	if err := db.QueryRow(`SELECT state, answer, settled_at, run_base_tool_calls, run_tool_calls FROM thread_requests WHERE token = 'old-spawn'`).
		Scan(&state, &answer, &settled, &base, &total); err != nil {
		t.Fatal(err)
	}
	if state != "finished" || answer != "done" || settled != 7 || base != nil || total != nil {
		t.Fatalf("the old row reads %s %q settled %d, base %v total %v; want it as it was, with no run", state, answer, settled, base, total)
	}
	var columns int
	if err := db.QueryRow(`SELECT count(*) FROM pragma_table_info('thread_requests') WHERE name LIKE 'run\_%' ESCAPE '\'`).Scan(&columns); err != nil || columns != 6 {
		t.Fatalf("thread_requests has %d run columns, %v; want 6", columns, err)
	}
	for _, name := range []string{"idx_thread_requests_run_open", "idx_thread_request_receipts_run_open"} {
		var index string
		if err := db.QueryRow(`SELECT sql FROM sqlite_master WHERE type = 'index' AND name = ?`, name).Scan(&index); err != nil || index == "" {
			t.Fatalf("%s: %q, %v", name, index, err)
		}
	}
	var receiptRun *int64
	if err := db.QueryRow(`SELECT run_open FROM thread_request_receipts WHERE token = 'old-receipt'`).Scan(&receiptRun); err != nil || receiptRun != nil {
		t.Fatalf("the old receipt's run_open %v, %v; want none", receiptRun, err)
	}
	if err := db.QueryRow(`SELECT count(*) FROM pragma_table_info('thread_request_receipts') WHERE name LIKE 'run\_%' ESCAPE '\'`).Scan(&columns); err != nil || columns != 4 {
		t.Fatalf("thread_request_receipts has %d run columns, %v; want 4", columns, err)
	}
}

// insertRun writes a request to thread on computer with base as its given
// start.
func insertRun(t *testing.T, s *Store, token, kind, computer, thread string, base *RunFigures, at int64) {
	t.Helper()
	if err := s.InsertThreadRequest(ThreadRequest{
		Token: token, CallerThreadID: "lead", Kind: kind, TargetComputerID: computer, TargetThreadID: thread,
		State: ThreadRequestRunning, RunBase: base, CreatedAt: at,
	}); err != nil {
		t.Fatalf("insert %s: %v", token, err)
	}
}

func settleRun(t *testing.T, s *Store, token string) {
	t.Helper()
	if ok, err := s.SettleThreadRequest(token, ThreadRequestOpenStates(), ThreadRequestSettlement{
		State: ThreadRequestFinished, Answer: []byte("done"), AnswerKind: ThreadAnswerFinal, SettledAt: nowMillis(),
	}); err != nil || !ok {
		t.Fatalf("settle %s: %v %v", token, ok, err)
	}
}

// A request made while its caller's run on the worker is open joins that
// run and takes its start; a run ends only once its request settled and the
// worker is at rest, with what the worker did since the start; the next
// request starts a new run where it is given. An ask, and a request to
// another thread, join nothing.
func TestARequestJoinsItsCallersOpenRunAndARunEndsSettled(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	mustCreateThread(t, s, "lead")
	insertRun(t, s, "spawn", ThreadRequestSpawn, "pc", "w", &RunFigures{}, 1)
	insertRun(t, s, "send-a", ThreadRequestSend, "pc", "w", &RunFigures{ToolCalls: 7, Tokens: 70, TurnMs: 700}, 2)
	insertRun(t, s, "ask", ThreadRequestAsk, "pc", "w", nil, 3)
	insertRun(t, s, "elsewhere", ThreadRequestSend, "pc", "other", &RunFigures{ToolCalls: 9, Tokens: 90, TurnMs: 900}, 4)
	if got := mustGetThreadRequest(t, s, "send-a").RunBase; got == nil || *got != (RunFigures{}) {
		t.Fatalf("a send during the spawn's run starts at %+v; want the run's start", got)
	}
	if got := mustGetThreadRequest(t, s, "ask"); got.RunBase != nil {
		t.Fatalf("an ask tracks a run %+v; want none", got.RunBase)
	}
	if got := mustGetThreadRequest(t, s, "elsewhere").RunBase; got == nil || got.ToolCalls != 9 {
		t.Fatalf("a send to another thread starts at %+v; want its given start", got)
	}
	if open, err := s.CallerRunOpen("lead", "pc", "w"); err != nil || !open {
		t.Fatalf("CallerRunOpen = %v, %v; want the run open", open, err)
	}

	// The worker rests before anything settled: no run ends.
	if err := s.EndRemoteThreadRequestRuns("pc", "w", RunFigures{ToolCalls: 1, Tokens: 10, TurnMs: 100}); err != nil {
		t.Fatal(err)
	}
	if mustGetThreadRequest(t, s, "spawn").RunEnded() {
		t.Fatal("an unsettled spawn's run ended")
	}
	settleRun(t, s, "spawn")
	if err := s.EndRemoteThreadRequestRuns("pc", "w", RunFigures{ToolCalls: 3, Tokens: 30, TurnMs: 300}); err != nil {
		t.Fatal(err)
	}
	if got := mustGetThreadRequest(t, s, "spawn").RunTotal; got == nil || *got != (RunFigures{ToolCalls: 3, Tokens: 30, TurnMs: 300}) {
		t.Fatalf("the spawn's run totals %+v; want what the worker did since its start", got)
	}
	if !mustGetThreadRequest(t, s, "send-a").RunOpen() {
		t.Fatal("the unsettled send's run ended with the spawn's")
	}
	settleRun(t, s, "send-a")
	if err := s.EndRemoteThreadRequestRuns("pc", "w", RunFigures{ToolCalls: 4, Tokens: 40, TurnMs: 400}); err != nil {
		t.Fatal(err)
	}
	if got := mustGetThreadRequest(t, s, "send-a").RunTotal; got == nil || *got != (RunFigures{ToolCalls: 4, Tokens: 40, TurnMs: 400}) {
		t.Fatalf("the send's run totals %+v; want the run's from its first request", got)
	}
	// A settled run keeps its totals.
	if err := s.EndRemoteThreadRequestRuns("pc", "w", RunFigures{ToolCalls: 8, Tokens: 80, TurnMs: 800}); err != nil {
		t.Fatal(err)
	}
	if got := mustGetThreadRequest(t, s, "spawn").RunTotal; got.ToolCalls != 3 {
		t.Fatalf("the spawn's ended run moved to %+v", got)
	}
	if open, err := s.CallerRunOpen("lead", "pc", "w"); err != nil || open {
		t.Fatalf("CallerRunOpen = %v, %v; want no run open", open, err)
	}

	insertRun(t, s, "send-b", ThreadRequestSend, "pc", "w", &RunFigures{ToolCalls: 8, Tokens: 80, TurnMs: 800}, 5)
	if got := mustGetThreadRequest(t, s, "send-b").RunBase; got == nil || got.ToolCalls != 8 {
		t.Fatalf("the next send starts at %+v; want its given start", got)
	}
}

// A paired computer's spawn, and a send from its thread to the thread that
// spawn made, start a run on their receipts here; a send from another thread
// does not. The rest pass lists the thread once a receipt with an open run
// settled, and the run ends at the worker's rest with the worker's figures
// over its life then, which ReceiptRunOf reports for that thread alone.
func TestAPairedComputersRunIsKeptOnItsReceipts(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	mustCreateThread(t, s, "w")
	accept := func(token, kind, source string) {
		t.Helper()
		target := "w"
		if kind == ThreadRequestSpawn {
			target = ""
		}
		if _, _, err := s.AcceptThreadRequestReceipt(ThreadRequestReceipt{
			Token: token, OwnerDeviceID: "dev", SourceComputerID: "pc", SourceThreadID: source, Kind: kind, TargetThreadID: target,
		}); err != nil {
			t.Fatalf("accept %s: %v", token, err)
		}
		if kind == ThreadRequestSpawn {
			if named, _, err := s.SetThreadReceiptTarget(token, "w", false); err != nil || !named {
				t.Fatalf("name %s: %v %v", token, named, err)
			}
		}
	}
	accept("spawn", ThreadRequestSpawn, "lead")
	accept("lead-send", ThreadRequestSend, "lead")
	accept("other-send", ThreadRequestSend, "other")
	for token, want := range map[string]any{"spawn": int64(1), "lead-send": int64(1), "other-send": nil} {
		var open any
		if err := s.db.QueryRow(`SELECT run_open FROM thread_request_receipts WHERE token = ?`, token).Scan(&open); err != nil {
			t.Fatal(err)
		}
		if open != want {
			t.Fatalf("%s run_open %v; want %v", token, open, want)
		}
	}
	if run, err := s.ReceiptRunOf("dev", "lead", "w"); err != nil || !run.Tracked || !run.Open {
		t.Fatalf("the lead's runs %+v, %v; want one open", run, err)
	}
	if run, err := s.ReceiptRunOf("dev", "other", "w"); err != nil || run.Tracked {
		t.Fatalf("the other thread's runs %+v, %v; want none tracked", run, err)
	}
	if threads, any, err := s.OpenRunThreads(); err != nil || !any || len(threads) != 0 {
		t.Fatalf("OpenRunThreads = %v %v, %v; want runs open and none settled", threads, any, err)
	}
	if ending, err := s.ThreadRunsEnding("w"); err != nil || ending {
		t.Fatalf("ThreadRunsEnding = %v, %v; want none a rest would end before a receipt settled", ending, err)
	}

	mustExec(t, s.db, `INSERT INTO thread_tallies (thread_id, tool_calls, tokens, turn_ms) VALUES ('w', 3, 30, 300)
		ON CONFLICT(thread_id) DO UPDATE SET tool_calls = 3, tokens = 30, turn_ms = 300`)
	for _, token := range []string{"spawn", "lead-send"} {
		if _, err := s.SettleLocalThreadRequest(token, ThreadReceiptOpenStates(), ThreadRequestSettlement{
			State: ThreadReceiptReplied, Answer: []byte("done"), AnswerKind: ThreadAnswerReply,
		}, ThreadRequestReplied); err != nil {
			t.Fatal(err)
		}
	}
	if threads, _, err := s.OpenRunThreads(); err != nil || len(threads) != 1 || threads[0] != "w" {
		t.Fatalf("OpenRunThreads = %v, %v; want the worker, its receipts settled", threads, err)
	}
	if open, err := s.ThreadRunsEnding("w"); err != nil || !open {
		t.Fatalf("ThreadRunsEnding = %v, %v; want a run open", open, err)
	}
	rest := &RestGuard{Roots: []string{"w"}, AtRest: func(SubtreeRest) []string { return []string{"w"} }}
	if ended, err := s.EndThreadRequestRunsAtRest("w", rest); err != nil || !ended {
		t.Fatalf("EndThreadRequestRunsAtRest = %v, %v", ended, err)
	}
	run, err := s.ReceiptRunOf("dev", "lead", "w")
	if err != nil || !run.Tracked || run.Open || run.End == nil || *run.End != (RunFigures{ToolCalls: 3, Tokens: 30, TurnMs: 300}) {
		t.Fatalf("the lead's runs %+v (end %v), %v; want ended with the worker's figures then", run, run.End, err)
	}
	if open, err := s.ThreadRunsEnding("w"); err != nil || open {
		t.Fatalf("ThreadRunsEnding = %v, %v; want none open", open, err)
	}
	if threads, any, err := s.OpenRunThreads(); err != nil || any || len(threads) != 0 {
		t.Fatalf("OpenRunThreads = %v %v, %v; want no run open", threads, any, err)
	}
}
