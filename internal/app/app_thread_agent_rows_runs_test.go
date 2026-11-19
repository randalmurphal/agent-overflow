package app

// An Agents row is a run (docs/specs/standing-agents.md, "Agents segment"):
// one row per worker, counting the work since it left rest for a request
// of the listed thread's, and a settled row keeps what its run did.

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
	"agent-overflow/internal/testutil"
	"agent-overflow/internal/threadtools"
)

// runTurnLines is a mock Claude turn that makes one Bash call, says text
// and ends once gate exists, reporting 40 fresh input, 5 cache writes and
// 15 output tokens as its own: 60 spent.
func runTurnLines(id, text, gate string) []string {
	return []string{
		mockClaudeInitLine,
		`{"type":"assistant","message":{"id":"` + id + `-tool","role":"assistant","content":[{"type":"tool_use","id":"` + id + `-bash","name":"Bash","input":{"command":"make check"}}]}}`,
		`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"` + id + `-bash","content":"ok"}]}}`,
		`{"type":"assistant","message":{"id":"` + id + `-text","role":"assistant","content":[{"type":"text","text":` + quoteJSON(text) + `}]}}`,
		testutil.MockWaitForFile(gate),
		`{"type":"result","subtype":"success","is_error":false,"usage":{"input_tokens":40,"output_tokens":15,"cache_read_input_tokens":900,"cache_creation_input_tokens":5}}`,
	}
}

// heldTurnLines is a mock Claude turn that says text and holds open.
func heldTurnLines(id, text string) []string {
	return []string{
		mockClaudeInitLine,
		`{"type":"assistant","message":{"id":"` + id + `","role":"assistant","content":[{"type":"text","text":` + quoteJSON(text) + `}]}}`,
	}
}

// pastRow reads token's row from the first page of threadID's Past.
func (f *requestFixture) pastRow(t *testing.T, threadID, token string) (ThreadAgentRow, bool) {
	t.Helper()
	page, err := f.app.ListThreadPastAgentRows(t.Context(), threadID, store.PastAgentRowsCursor{}, 8)
	if err != nil {
		t.Fatalf("ListThreadPastAgentRows: %v", err)
	}
	return agentRowByToken(page.Rows, token)
}

// openRows is threadID's listing's open rows.
func (f *requestFixture) openRows(t *testing.T, threadID string) []ThreadAgentRow {
	t.Helper()
	var out []ThreadAgentRow
	for _, row := range f.agentRows(t, threadID).Rows {
		if row.open() {
			out = append(out, row)
		}
	}
	return out
}

// spawnedAndRested spawns a worker whose first run is runTurnLines' turn
// gated on gate, which it opens once that turn has run 300ms, waits for it
// to settle at rest, and returns the spawn's ack and the Past row it
// settled into: its run's figures.
func (f *requestFixture) spawnedAndRested(t *testing.T, gate string) (threadtools.RequestAck, ThreadAgentRow) {
	t.Helper()
	ack, err := f.adapter().Spawn(t.Context(), f.callerIdentity(), threadtools.SpawnCall{Prompt: "review the change", Notify: true})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	waitUntil(t, 10*time.Second, func() bool {
		_, active, err := f.app.store.GetActiveTurn(ack.ThreadID)
		return err == nil && active
	})
	time.Sleep(300 * time.Millisecond)
	if err := os.WriteFile(gate, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	f.awaitRequestState(t, ack.Token, store.ThreadRequestFinished)
	var settled ThreadAgentRow
	waitUntil(t, 10*time.Second, func() bool {
		var ok bool
		settled, ok = f.pastRow(t, f.caller.ID, ack.Token)
		return ok && settled.ToolCalls == 1 && settled.Tokens == 60
	})
	if settled.RuntimeMs < 300 || settled.TurnOpen {
		t.Fatalf("first run's Past row %+v; want its turn's time, closed", settled)
	}
	return ack, settled
}

// The user's case: a spawned reviewer answered and rested, and its spawner
// sends it a follow-up. The spawner lists one row for the reviewer, the
// send's, counting only what the reviewer did since the send; the spawn's
// row stays in Past with what its run did; the toggle counts one worker.
func TestASendToARestedWorkerIsOneRowCountingFromTheSend(t *testing.T) {
	t.Parallel()
	f := newRequestFixture(t)
	f.watchAgentRows()
	// The first run takes at least 300ms, so its time is a figure the
	// send's row must leave out.
	gate := filepath.Join(t.TempDir(), "first-turn")
	installMockClaudeTurns(t, f.app, [][]string{
		runTurnLines("first", "The change looks right.", gate),
		heldTurnLines("second", "Checking the follow-up."),
	})
	spawn, settled := f.spawnedAndRested(t, gate)

	sent, err := f.adapter().Send(t.Context(), f.callerIdentity(), threadtools.SendCall{
		ThreadID: spawn.ThreadID, Message: "also check the migration", Notify: true,
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	var open []ThreadAgentRow
	waitUntil(t, 10*time.Second, func() bool {
		f.app.threadRequestsWG.Wait()
		open = f.openRows(t, f.caller.ID)
		return len(open) > 1 || (len(open) == 1 && open[0].TurnOpen)
	})
	if rows := f.agentRows(t, f.caller.ID).Rows; len(rows) != 1 || rows[0].Token != sent.Token {
		t.Fatalf("rows %+v; want the send's row alone for the worker", rows)
	}
	row := open[0]
	tally, err := f.app.store.ThreadTallies([]string{spawn.ThreadID}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	lifetime := tally[spawn.ThreadID]
	if row.Placement != store.PlacementOwned {
		t.Fatalf("send's row placement %q; want the worker's, owned, so it offers Show in Sidebar", row.Placement)
	}
	if row.ToolCalls != 0 || row.Tokens != 0 || !row.TurnOpen || row.RuntimeMs > lifetime.RuntimeMs-settled.RuntimeMs {
		t.Fatalf("send's row %+v; want only the run since the send, none of the first run's %+v (worker's life %+v)", row, settled, lifetime)
	}
	if got := f.workerSummary(t, f.caller.ID); got.Open != 1 {
		t.Fatalf("summary %+v; want one worker", got)
	}
	again, ok := f.pastRow(t, f.caller.ID, spawn.Token)
	if !ok || again.ToolCalls != settled.ToolCalls || again.Tokens != settled.Tokens || again.RuntimeMs != settled.RuntimeMs || again.TurnOpen {
		t.Fatalf("spawn's Past row %+v (found %v); want its run's figures as they settled, %+v", again, ok, settled)
	}
	if past := f.pastTokens(t, f.caller.ID); slices.Contains(past, sent.Token) {
		t.Fatalf("past %v; want the open send out of it", past)
	}
}

// Two sends while the worker still works on the first are one row, the
// run's first request's, counting the run since the first; each request
// still settles on its own. Once the worker rests both are in Past with
// the run's figures, which a later run does not move.
func TestTwoSendsDuringOneRunAreOneRow(t *testing.T) {
	t.Parallel()
	f := newRequestFixture(t)
	f.watchAgentRows()
	worker := f.plainThread(t)
	f.acceptAgentRequest(t, "runs-spawn", f.caller.ID, store.ThreadRequestSpawn, worker.ID)
	workerTurn(t, f.app, worker.ID, "t0", 2, 60)
	f.settleAgentRequest(t, "runs-spawn")
	f.awaitRunEnded(t, "runs-spawn")

	f.acceptAgentRequest(t, "runs-first", f.caller.ID, store.ThreadRequestSend, worker.ID)
	workerEvent(t, f.app, worker.ID, provider.ProviderEvent{Kind: provider.EventTurnStart, TurnID: worker.ID + ":t1"}, nil)
	workerToolCall(t, f.app, worker.ID, "t1-a", "make test")
	f.acceptAgentRequest(t, "runs-second", f.caller.ID, store.ThreadRequestSend, worker.ID)
	workerToolCall(t, f.app, worker.ID, "t1-b", "make lint")
	f.app.threadRequestsWG.Wait()
	rows := f.agentRows(t, f.caller.ID).Rows
	if len(rows) != 1 || rows[0].Token != "runs-first" || rows[0].Kind != store.ThreadRequestSend || rows[0].ToolCalls != 2 {
		t.Fatalf("rows %+v; want the run's first send alone, counting its two calls", rows)
	}
	if got := f.workerSummary(t, f.caller.ID); got.Open != 1 {
		t.Fatalf("summary %+v; want one worker", got)
	}
	first, second := f.request(t, "runs-first"), f.request(t, "runs-second")
	if first.RunBase == nil || second.RunBase == nil || *first.RunBase != *second.RunBase {
		t.Fatalf("bases %+v and %+v; want the second send to join the first's run", first.RunBase, second.RunBase)
	}

	workerEvent(t, f.app, worker.ID, provider.ProviderEvent{Kind: provider.EventTurnComplete, TurnComplete: &provider.WireTurnCompleteMeta{StopReason: "end_turn"}}, nil)
	f.settleAgentRequest(t, "runs-first")
	f.settleAgentRequest(t, "runs-second")
	ended := f.awaitRunEnded(t, "runs-first")
	if other := f.awaitRunEnded(t, "runs-second"); *other.RunTotal != *ended.RunTotal || ended.RunTotal.ToolCalls != 2 || ended.RunTotal.Tokens != 0 {
		t.Fatalf("run totals %+v and %+v; want the run's two calls on both", ended.RunTotal, other.RunTotal)
	}
	if after := f.listingAfterWindow(t, f.caller.ID); len(after.Rows) != 0 {
		t.Fatalf("after the window %+v; want nothing open", after.Rows)
	}

	// A later run, nobody's request, moves neither row.
	workerTurn(t, f.app, worker.ID, "t2", 3, 0)
	for _, token := range []string{"runs-spawn", "runs-first", "runs-second"} {
		row, ok := f.pastRow(t, f.caller.ID, token)
		want := *ended.RunTotal
		if token == "runs-spawn" {
			want = store.RunFigures{ToolCalls: 2, Tokens: 60, TurnMs: row.RuntimeMs}
		}
		if !ok || row.ToolCalls != want.ToolCalls || row.Tokens != want.Tokens || row.RuntimeMs != want.TurnMs {
			t.Fatalf("Past row %s %+v (found %v); want its run's %+v", token, row, ok, want)
		}
	}
}

// A worker whose spawn's run ended and that someone else continues, its own
// wake or the person in its composer, or another thread's send, is no row
// of its spawner's: not listed, not counted, not stopped by its Stop All,
// and its spawn stays in Past with its run's figures.
func TestAWorkerContinuedByAnyoneElseIsNoRowOfItsSpawner(t *testing.T) {
	t.Parallel()
	for _, by := range []string{"its own turn", "another thread's send"} {
		t.Run(by, func(t *testing.T) {
			t.Parallel()
			f := newRequestFixture(t)
			f.watchAgentRows()
			worker := f.plainThread(t)
			f.acceptAgentRequest(t, "else-spawn", f.caller.ID, store.ThreadRequestSpawn, worker.ID)
			workerTurn(t, f.app, worker.ID, "t0", 2, 60)
			f.settleAgentRequest(t, "else-spawn")
			settled := f.awaitRunEnded(t, "else-spawn")
			if settled.RunTotal.ToolCalls != 2 || settled.RunTotal.Tokens != 60 {
				t.Fatalf("spawn's run %+v; want its two calls and 60 tokens", settled.RunTotal)
			}
			if by == "another thread's send" {
				other := f.plainThread(t)
				f.acceptAgentRequest(t, "else-send", other.ID, store.ThreadRequestSend, worker.ID)
				if row := f.request(t, "else-send"); row.RunBase != nil {
					t.Fatalf("another thread's send %+v tracks a run; want none, it spawned nothing", row)
				}
			}
			workerEvent(t, f.app, worker.ID, provider.ProviderEvent{Kind: provider.EventTurnStart, TurnID: worker.ID + ":t1"}, nil)
			workerToolCall(t, f.app, worker.ID, "t1-a", "make test")
			waitUntil(t, 10*time.Second, func() bool { return f.app.liveWorker(worker.ID) })
			f.app.threadRequestsWG.Wait()

			if open := f.openRows(t, f.caller.ID); len(open) != 0 {
				t.Fatalf("open rows %+v; want none for a run the spawner did not start", open)
			}
			if after := f.listingAfterWindow(t, f.caller.ID); len(after.Rows) != 0 {
				t.Fatalf("after the window %+v; want no row", after.Rows)
			}
			if got := f.workerSummary(t, f.caller.ID); got.Open != 0 {
				t.Fatalf("summary %+v; want nothing counted", got)
			}
			row, ok := f.pastRow(t, f.caller.ID, "else-spawn")
			if !ok || row.ToolCalls != 2 || row.Tokens != 60 || row.TurnOpen || row.Worker != "" {
				t.Fatalf("spawn's Past row %+v (found %v); want it kept with its run's figures", row, ok)
			}
			stops, err := f.app.StopThreadAgentRows(t.Context(), f.caller.ID)
			if err != nil || len(stops) != 0 {
				t.Fatalf("Stop All = %+v, %v; want nothing of the spawner's to stop", stops, err)
			}
			if !f.app.liveWorker(worker.ID) {
				t.Fatal("Stop All stopped a run the spawner did not start")
			}
		})
	}
}

// A worker that never came to rest after its spawn, holding background work,
// is still in its spawn's run: its row stays open, counted.
func TestAWorkerThatNeverRestedKeepsItsSpawnsRow(t *testing.T) {
	t.Parallel()
	f := newRequestFixture(t)
	f.watchAgentRows()
	worker := f.plainThread(t)
	f.acceptAgentRequest(t, "bg-run-spawn", f.caller.ID, store.ThreadRequestSpawn, worker.ID)
	launchWorkerShell(t, f.app, worker.ID, "bg-shell")
	f.settleAgentRequest(t, "bg-run-spawn")
	row := f.awaitWorker(t, f.caller.ID, "bg-run-spawn", workerWaiting)
	if row.ToolCalls != 1 {
		t.Fatalf("row %+v; want its run's call counted", row)
	}
	f.app.noteRestWaitsMayChange()
	f.app.threadRequestsWG.Wait()
	if got := f.request(t, "bg-run-spawn"); got.RunTotal != nil {
		t.Fatalf("spawn %+v; want its run open while its worker waits", got)
	}
	if got := f.workerSummary(t, f.caller.ID); got.Open != 1 {
		t.Fatalf("summary %+v; want the waiting worker counted", got)
	}
}

// A request written before runs were kept has no start: its row reads as
// rows did, a spawn reopened by its worker's later work with its life's
// figures.
func TestARequestWithNoRunReadsAsBefore(t *testing.T) {
	t.Parallel()
	f := newRequestFixture(t)
	f.watchAgentRows()
	worker := f.settledSpawn(t, "old-spawn", f.caller.ID)
	if row := f.request(t, "old-spawn"); row.RunBase != nil {
		t.Fatalf("seeded row %+v; this test needs one with no run", row)
	}
	workerTurn(t, f.app, worker.ID, "t0", 2, 60)
	workerEvent(t, f.app, worker.ID, provider.ProviderEvent{Kind: provider.EventTurnStart, TurnID: worker.ID + ":t1"}, nil)
	workerToolCall(t, f.app, worker.ID, "t1-a", "make test")
	row := f.awaitWorker(t, f.caller.ID, "old-spawn", workerRunning)
	if row.ToolCalls != 3 || row.Tokens != 60 {
		t.Fatalf("row %+v; want the worker's life, three calls and 60 tokens", row)
	}
}

// A send to a rested worker that lands before the rest pass ended its
// spawn's run ends that run itself, so the send starts its own run at the
// worker's figures instead of joining the spawn's.
func TestASendBeforeTheRestPassEndsTheRunItFollows(t *testing.T) {
	t.Parallel()
	f := newRequestFixture(t)
	worker := f.plainThread(t)
	installMockClaudeTurns(t, f.app, [][]string{heldTurnLines("later", "Working on it.")})
	// A pass that reads as running holds every later one back.
	r := &f.app.threadRequests
	r.mu.Lock()
	r.rest.checking = true
	r.mu.Unlock()
	t.Cleanup(func() {
		r.mu.Lock()
		r.rest.checking = false
		r.mu.Unlock()
	})
	f.acceptAgentRequest(t, "early-spawn", f.caller.ID, store.ThreadRequestSpawn, worker.ID)
	workerTurn(t, f.app, worker.ID, "t0", 2, 60)
	f.settleAgentRequest(t, "early-spawn")
	f.app.threadRequestsWG.Wait()
	if row := f.request(t, "early-spawn"); !row.RunOpen() {
		t.Fatalf("spawn %+v; the test needs its run open with the rest pass held back", row)
	}
	sent, err := f.adapter().Send(t.Context(), f.callerIdentity(), threadtools.SendCall{ThreadID: worker.ID, Message: "one more", Notify: true})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if row := f.request(t, "early-spawn"); row.RunTotal == nil || row.RunTotal.ToolCalls != 2 || row.RunTotal.Tokens != 60 {
		t.Fatalf("spawn's run %+v; want it ended with its two calls and 60 tokens", row.RunTotal)
	}
	if row := f.request(t, sent.Token); row.RunBase == nil || row.RunBase.ToolCalls != 2 || row.RunBase.Tokens != 60 {
		t.Fatalf("send's run starts at %+v; want the worker's figures at the send", row.RunBase)
	}
}

// acceptAgentRequest writes caller's request of target as the tools accept
// one on this computer: the source row and its receipt in one write.
func (f *requestFixture) acceptAgentRequest(t *testing.T, token, caller, kind, target string) {
	t.Helper()
	origin := threadRequestOrigin{caller: threadtools.Caller{ThreadID: caller}, ownerDevice: threadReceiptLocalOwner}
	if _, err := f.app.store.AcceptLocalThreadRequest(store.ThreadRequest{
		Token: token, CallerThreadID: caller, Kind: kind, TargetThreadID: target, Notify: true, State: store.ThreadRequestAccepted,
	}, threadReceiptFor(origin, token, kind, target, false)); err != nil {
		t.Fatalf("accept %s: %v", token, err)
	}
}

// settleAgentRequest settles token replied, its receipt with it.
func (f *requestFixture) settleAgentRequest(t *testing.T, token string) {
	t.Helper()
	got, err := f.app.store.SettleLocalThreadRequest(token, store.ThreadReceiptOpenStates(), store.ThreadRequestSettlement{
		State: store.ThreadReceiptReplied, Answer: []byte("done"), AnswerKind: store.ThreadAnswerReply,
	}, store.ThreadRequestReplied)
	if err != nil || !got.Settled {
		t.Fatalf("settle %s: %+v, %v", token, got, err)
	}
}

// awaitRunEnded waits until token's run ended and returns its row.
func (f *requestFixture) awaitRunEnded(t *testing.T, token string) store.ThreadRequest {
	t.Helper()
	var row store.ThreadRequest
	waitUntil(t, 10*time.Second, func() bool {
		f.app.threadRequestsWG.Wait()
		row = f.request(t, token)
		return row.RunEnded()
	})
	return row
}

// workerTurn is a turn of threadID that makes calls top-level tool calls
// and ends having spent tokens of its own.
func workerTurn(t *testing.T, a *App, threadID, id string, calls, tokens int) {
	t.Helper()
	workerEvent(t, a, threadID, provider.ProviderEvent{Kind: provider.EventTurnStart, TurnID: threadID + ":" + id}, nil)
	for i := range calls {
		workerToolCall(t, a, threadID, id+"-"+string(rune('a'+i)), "make check")
	}
	own := provider.TokenUsage{InputTokens: tokens}
	workerEvent(t, a, threadID, provider.ProviderEvent{Kind: provider.EventTurnComplete, TurnComplete: &provider.WireTurnCompleteMeta{
		StopReason: "end_turn", UsageScope: "codex-process", Usage: &own, OwnUsage: &own,
		ModelUsage: []provider.ModelTokenUsage{{Model: "gpt-5.2-codex", TokenUsage: own}},
	}}, nil)
}

// A spawn whose run ended, listed while its settled window lasts, nests the
// rows its run left, each done, and none its worker opens since for someone
// else; the named-row read says the same of each.
func TestASpawnWhoseRunEndedNestsOnlyItsRunsRows(t *testing.T) {
	t.Parallel()
	f := newRequestFixture(t)
	worker, grand, later := f.plainThread(t), f.plainThread(t), f.plainThread(t)
	f.acceptAgentRequest(t, "lead-spawn", f.caller.ID, store.ThreadRequestSpawn, worker.ID)
	f.acceptAgentRequest(t, "run-grand", worker.ID, store.ThreadRequestSpawn, grand.ID)
	f.settleAgentRequest(t, "run-grand")
	// Its answer is taken, so nothing is on its way to the worker.
	if _, err := f.app.store.SetThreadRequestNotify("run-grand", false); err != nil {
		t.Fatal(err)
	}
	f.settleAgentRequest(t, "lead-spawn")
	f.awaitRunEnded(t, "lead-spawn")
	if got := agentRowTokens(f.agentRows(t, f.caller.ID).Rows); !slices.Equal(got, []string{"lead-spawn", "run-grand"}) {
		t.Fatalf("rows %v; want the spawn and the row its run left", got)
	}
	// The worker, continued by someone else, spawns again.
	f.acceptAgentRequest(t, "later-grand", worker.ID, store.ThreadRequestSpawn, later.ID)
	f.app.threadRequestsWG.Wait()
	if got := agentRowTokens(f.agentRows(t, f.caller.ID).Rows); !slices.Equal(got, []string{"lead-spawn", "run-grand"}) {
		t.Fatalf("rows %v; want the open spawn of someone else's run left out", got)
	}
	reads, err := f.app.GetThreadAgentRows(t.Context(), f.caller.ID, []string{"run-grand", "later-grand"})
	if err != nil || len(reads) != 2 || !reads[0].Listed || reads[1].Listed {
		t.Fatalf("named reads %+v, %v; want the run's row listed and the later one not", reads, err)
	}
}
