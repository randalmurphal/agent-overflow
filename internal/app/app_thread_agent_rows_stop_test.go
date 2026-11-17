package app

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
	"agent-overflow/internal/testutil"
	"agent-overflow/internal/threadtools"
)

// A person's Stop on a spawn cancels the request with the wake left armed,
// so the agent is told in words that name the user, and stops the work.
func TestThreadAgentRowStopCancelsForTheUser(t *testing.T) {
	t.Parallel()
	f := newRequestFixture(t)
	f.mockClaudeHoldingTheTurn(t, "working")
	// The caller is mid-turn, so its wake waits in the queue to be read.
	if err := f.app.store.InsertTurn(store.Turn{
		TurnID: "caller-open", ThreadID: f.caller.ID, TurnIndex: 1, StartedAt: time.Now().UnixMilli(),
	}); err != nil {
		t.Fatalf("InsertTurn: %v", err)
	}
	ack := f.runningRequest(t, "rebuild the index")

	stop, err := f.app.StopThreadAgentRow(t.Context(), f.caller.ID, ack.Token)
	if err != nil || stop.Outcome != threadAgentStopped || len(stop.Failures) != 0 {
		t.Fatalf("StopThreadAgentRow = %+v, %v", stop, err)
	}
	row := f.awaitRequestState(t, ack.Token, store.ThreadRequestCancelled)
	if !row.Notify {
		t.Fatal("a person's Stop disarmed the wake; the agent would never learn its child was stopped")
	}
	var wake string
	waitUntil(t, 10*time.Second, func() bool {
		for _, queued := range durableQueueRows(t, f.app, f.caller.ID) {
			if queued.SendID == threadWakeSendID(ack.Token) {
				wake = queued.Message
				return true
			}
		}
		return false
	})
	if !strings.Contains(wake, "was cancelled: The user stopped it") {
		t.Fatalf("the wake does not say the user stopped it: %q", wake)
	}
	if again, err := f.app.StopThreadAgentRow(t.Context(), f.caller.ID, ack.Token); err != nil || again.Outcome != threadAgentEnded {
		t.Fatalf("a second Stop = %+v, %v; want ended", again, err)
	}
}

// The agent's own thread_cancel still disarms the wake: it reads the report.
func TestThreadCancelByTheAgentStillDisarmsTheWake(t *testing.T) {
	t.Parallel()
	f := newRequestFixture(t)
	f.mockClaudeHoldingTheTurn(t, "working")
	ack := f.runningRequest(t, "rebuild the index")
	if _, err := f.adapter().Cancel(t.Context(), f.callerIdentity(), threadtools.CancelCall{Token: ack.Token}); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	if row := f.request(t, ack.Token); row.Notify || row.State != store.ThreadRequestCancelled {
		t.Fatalf("after the agent's cancel: notify %v, state %q", row.Notify, row.State)
	}
}

// Stop interrupts a target's running turn only when the row's caller spawned
// it: a thread it sent to belongs to someone else, and the cancel stops only
// the request.
func TestThreadAgentRowStopInterruptsOnlyASpawnedThread(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		kind        string
		interrupted bool
	}{{store.ThreadRequestSpawn, true}, {store.ThreadRequestSend, false}} {
		t.Run(tc.kind, func(t *testing.T) {
			t.Parallel()
			f := newRequestFixture(t)
			interrupted := f.mockClaudeRecordingInterrupts(t, "busy with someone else's work")
			// The caller's spawn keeps the target's turn running. A thread
			// is spawned once, so the spawn row is that one; a stranger's
			// send to the thread is someone else's.
			busy := f.runningRequest(t, "someone else's work")
			caller, token := f.caller.ID, busy.Token
			if tc.kind == store.ThreadRequestSend {
				stranger := f.plainThread(t)
				f.seedAgentRow(t, "row", stranger.ID, tc.kind, busy.ThreadID, true)
				caller, token = stranger.ID, "row"
			}
			waitUntil(t, 10*time.Second, func() bool {
				live, err := f.adapter().LiveState(t.Context(), busy.ThreadID)
				return err == nil && live.ActiveTurn
			})

			stop, err := f.app.StopThreadAgentRow(t.Context(), caller, token)
			if err != nil || stop.Outcome != threadAgentStopped {
				t.Fatalf("StopThreadAgentRow = %+v, %v", stop, err)
			}
			sent := func() bool { _, err := os.Stat(interrupted); return err == nil }
			if tc.interrupted {
				waitUntil(t, 10*time.Second, sent)
				return
			}
			// The interrupt is answered by the provider: give one that was
			// sent the time to arrive before saying none was.
			time.Sleep(300 * time.Millisecond)
			if sent() {
				t.Fatal("a send's Stop interrupted a turn that was not its own")
			}
		})
	}
}

// mockClaudeRecordingInterrupts is mockClaudeHoldingTheTurn whose provider
// also creates the returned file when it is sent an interrupt.
func (f *requestFixture) mockClaudeRecordingInterrupts(t *testing.T, text string) string {
	t.Helper()
	binary := testutil.WriteMockClaudeScript(t, t.TempDir(), [][]string{{
		mockClaudeInitLine,
		`{"type":"assistant","message":{"id":"msg-hold","role":"assistant","content":[{"type":"text","text":` + quoteJSON(text) + `}]}}`,
	}})
	// The binary is a link to a shared wrapper that runs the payload beside
	// it (mockexec); only the payload is this test's to change.
	payload := binary + ".payload"
	script, err := os.ReadFile(payload)
	if err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(t.TempDir(), "interrupted")
	patched := strings.Replace(string(script), "\"$reqid\"\n", "\"$reqid\"\n      touch '"+marker+"'\n", 1)
	if patched == string(script) {
		t.Fatal("the mock script has no interrupt answer to patch")
	}
	if err := os.WriteFile(payload, []byte(patched), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := f.app.settings.Update(map[string]any{"claudeBinaryPath": binary}); err != nil {
		t.Fatalf("set claude binary: %v", err)
	}
	return marker
}

// Stop All stops every open row, nested ones included; a row a thread is
// not waiting on is refused.
func TestThreadAgentRowsStopAllCascades(t *testing.T) {
	t.Parallel()
	f := newRequestFixture(t)
	f.mockClaudeHoldingTheTurn(t, "working")
	child := f.runningRequest(t, "lead the fix")
	grand, err := f.adapter().Spawn(t.Context(), f.targetIdentity(t, child.ThreadID), threadtools.SpawnCall{Prompt: "write the test", Notify: true})
	if err != nil {
		t.Fatalf("nested Spawn: %v", err)
	}
	stranger := f.plainThread(t)
	if _, err := f.app.StopThreadAgentRow(t.Context(), stranger.ID, child.Token); err == nil {
		t.Fatal("a thread stopped a request it is not waiting on")
	}

	stops, err := f.app.StopThreadAgentRows(t.Context(), f.caller.ID)
	if err != nil {
		t.Fatalf("StopThreadAgentRows: %v", err)
	}
	// One result per top-level row; the child's Stop reached its own spawn.
	if len(stops) != 1 || stops[0].Token != child.Token || stops[0].Outcome != threadAgentStopped ||
		stops[0].Nested != 1 || len(stops[0].Failures) != 0 {
		t.Fatalf("stops = %+v; want the child stopped with its one nested agent", stops)
	}
	// The pressed row's caller is told; the child, whose agent the Stop
	// also stopped, is not woken for its own spawn.
	if row := f.awaitRequestState(t, child.Token, store.ThreadRequestCancelled); !row.Notify {
		t.Fatal("the pressed row's wake was disarmed")
	}
	if row := f.awaitRequestState(t, grand.Token, store.ThreadRequestCancelled); row.Notify {
		t.Fatal("the cascade left the child's wake armed: the child would take a new turn")
	}
}

// seedOpenSpawns writes n open spawns caller made, each of a thread of its
// own with the receipt a Stop settles, created from base on, and returns
// their tokens and threads in that order.
func (f *requestFixture) seedOpenSpawns(t *testing.T, caller, prefix string, n int, base int64) (tokens, threads []string) {
	t.Helper()
	for index := range n {
		token, thread := fmt.Sprintf("%s-%03d", prefix, index), fmt.Sprintf("%s-thread-%03d", prefix, index)
		if err := f.seedOpenSpawn(caller, token, thread, base+int64(index)); err != nil {
			t.Fatal(err)
		}
		tokens, threads = append(tokens, token), append(threads, thread)
	}
	return tokens, threads
}

func (f *requestFixture) seedOpenSpawn(caller, token, thread string, at int64) error {
	if err := f.app.store.CreateThread(store.Thread{
		ID: thread, ProjectID: f.caller.ProjectID, Title: thread, Provider: string(provider.Claude),
		WorkspacePath: f.caller.WorkspacePath, CreatedAt: at, UpdatedAt: at,
	}); err != nil {
		return fmt.Errorf("create %s: %w", thread, err)
	}
	if err := f.app.store.InsertThreadRequest(store.ThreadRequest{
		Token: token, CallerThreadID: caller, Kind: store.ThreadRequestSpawn, TargetThreadID: thread,
		State: store.ThreadRequestAccepted, CreatedAt: at, UpdatedAt: at,
	}); err != nil {
		return fmt.Errorf("insert %s: %w", token, err)
	}
	if _, _, err := f.app.store.AcceptThreadRequestReceipt(store.ThreadRequestReceipt{
		Token: token, OwnerDeviceID: threadReceiptLocalOwner, SourceThreadID: caller,
		Kind: store.ThreadRequestSpawn, TargetThreadID: thread, CreatedAt: at,
	}); err != nil {
		return fmt.Errorf("accept %s: %w", token, err)
	}
	return nil
}

// seedSettledSends writes n sends caller was owed answers for that settled
// just now, created at base on: rows the listing shows that a Stop has
// nothing to stop in.
func (f *requestFixture) seedSettledSends(t *testing.T, caller, target, prefix string, n int, base int64) {
	t.Helper()
	for index := range n {
		token, at := fmt.Sprintf("%s-%03d", prefix, index), base+int64(index)
		if err := f.app.store.InsertThreadRequest(store.ThreadRequest{
			Token: token, CallerThreadID: caller, Kind: store.ThreadRequestSend, TargetThreadID: target, Notify: true,
			State: store.ThreadRequestAccepted, CreatedAt: at, UpdatedAt: at,
		}); err != nil {
			t.Fatalf("insert %s: %v", token, err)
		}
		if settled, err := f.app.store.SettleThreadRequest(token, store.ThreadRequestOpenStates(), store.ThreadRequestSettlement{
			State: store.ThreadRequestFinished, Answer: []byte("done"), AnswerKind: store.ThreadAnswerFinal, SettledAt: time.Now().UnixMilli(),
		}); err != nil || !settled {
			t.Fatalf("settle %s: %v %v", token, settled, err)
		}
	}
}

// Stop All stops every open row however many there are, past the tray's
// limit at the top and under a nested worker alike, with rows that settled
// just now ahead of them in the ledger, which it leaves out. What it stops
// is what was there when it started: a request made while it runs is not
// stopped.
func TestThreadAgentRowsStopAllStopsPastTheTraysLimit(t *testing.T) {
	t.Parallel()
	f := newRequestFixture(t)
	n := threadAgentRowLimit + 2
	base := time.Now().Add(-time.Minute).UnixMilli()
	f.seedSettledSends(t, f.caller.ID, f.caller.ID, "lead-settled", 5, base)
	top, _ := f.seedOpenSpawns(t, f.caller.ID, "lead-open", n, base+10)
	worker, workerThreads := f.seedOpenSpawns(t, f.caller.ID, "lead-worker", 1, base+1_000)
	f.seedSettledSends(t, workerThreads[0], f.caller.ID, "worker-settled", 5, base+2_000)
	nested, _ := f.seedOpenSpawns(t, workerThreads[0], "worker-open", n, base+2_010)
	if listing := f.agentRows(t, f.caller.ID); !listing.Hidden {
		t.Fatalf("the tray listed all %d rows; the test needs more than its limit", len(listing.Rows))
	}

	// A request each capture did not see: made once the Stop All read what
	// to stop, at the top and in the worker's cascade.
	var late sync.Map
	f.app.agentStopCapturedForTest = func(threadID string) {
		if threadID != f.caller.ID && threadID != workerThreads[0] {
			return
		}
		if _, again := late.LoadOrStore(threadID, true); again {
			return
		}
		// Errorf, not Fatal: a nested capture runs off the test's goroutine.
		if err := f.seedOpenSpawn(threadID, "late-"+threadID, "late-thread-"+threadID, time.Now().UnixMilli()); err != nil {
			t.Error(err)
		}
	}
	stops, err := f.app.StopThreadAgentRows(t.Context(), f.caller.ID)
	if err != nil {
		t.Fatalf("StopThreadAgentRows: %v", err)
	}
	got := map[string]ThreadAgentStop{}
	for _, stop := range stops {
		got[stop.Token] = stop
	}
	if len(stops) != n+1 {
		t.Fatalf("Stop All reported %d rows; want the %d open ones and none settled before it", len(stops), n+1)
	}
	for _, token := range append(slices.Clone(top), worker...) {
		if stop, ok := got[token]; !ok || stop.Outcome != threadAgentStopped || len(stop.Failures) != 0 {
			t.Fatalf("stop of %s = %+v (reported %v); want it stopped", token, stop, ok)
		}
	}
	if stop := got[worker[0]]; stop.Nested != n {
		t.Fatalf("the worker's cascade stopped %d of its own agents; want all %d", stop.Nested, n)
	}
	for _, token := range append(append(slices.Clone(top), worker...), nested...) {
		f.awaitRequestState(t, token, store.ThreadRequestCancelled)
	}
	for _, caller := range []string{f.caller.ID, workerThreads[0]} {
		token := "late-" + caller
		if row, found, err := f.app.store.GetThreadRequest(token); err != nil || !found || row.State != store.ThreadRequestAccepted {
			t.Fatalf("the request %s made after the capture reads %+v, found=%v, %v; want it left open", token, row, found, err)
		}
	}
}

// Stop All reaches open work three settled spawns down, which the pane
// lists under the chain that leads to it. Each settled spawn on the way was
// not at rest while the work below it was open, so the top one reports
// stopped and counts the two below it and the open request.
func TestThreadAgentRowsStopAllReachesDeepOpenWork(t *testing.T) {
	t.Parallel()
	f := newRequestFixture(t)
	f.mockClaudeHoldingTheTurn(t, "working")
	chain := []string{f.caller.ID}
	for range 3 {
		chain = append(chain, f.plainThread(t).ID)
	}
	long := time.Now().Add(-time.Hour).UnixMilli()
	tokens := []string{"lead-child", "child-grand", "grand-great"}
	for index, token := range tokens {
		f.seedAgentRow(t, token, chain[index], store.ThreadRequestSpawn, chain[index+1], false)
		if settled, err := f.app.store.SettleThreadRequest(token, store.ThreadRequestOpenStates(), store.ThreadRequestSettlement{
			State: store.ThreadRequestFinished, Answer: []byte("done"), AnswerKind: store.ThreadAnswerFinal, SettledAt: long,
		}); err != nil || !settled {
			t.Fatalf("settle %s: %v %v", token, settled, err)
		}
	}
	open, err := f.adapter().Spawn(t.Context(), f.targetIdentity(t, chain[3]), threadtools.SpawnCall{Prompt: "keep going", Notify: true})
	if err != nil {
		t.Fatalf("deep Spawn: %v", err)
	}
	f.awaitRequestState(t, open.Token, store.ThreadRequestRunning)
	if got, want := agentRowTokens(f.agentRows(t, f.caller.ID).Rows), append(tokens, open.Token); !slices.Equal(got, want) {
		t.Fatalf("rows %v; want the chain to the open request %v", got, want)
	}

	stops, err := f.app.StopThreadAgentRows(t.Context(), f.caller.ID)
	if err != nil || len(stops) != 1 || stops[0].Outcome != threadAgentStopped || stops[0].Nested != 3 || len(stops[0].Failures) != 0 {
		t.Fatalf("StopThreadAgentRows = %+v, %v; want the chain's one open agent stopped", stops, err)
	}
	if row := f.awaitRequestState(t, open.Token, store.ThreadRequestCancelled); row.Notify {
		t.Fatal("the cascade left the deep request's wake armed")
	}
}
