package app

// A spawn written before runs were kept tracks none (request_runs.go): it
// reads open until it settles and is closed for good once it has, whatever
// its worker does later.

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"

	"github.com/google/uuid"
)

// seedSpawnBeforeRuns writes caller's spawn of worker as a build before
// runs were kept left it: no run columns, its answer taken.
func seedSpawnBeforeRuns(t *testing.T, a *App, token, caller, worker string) {
	t.Helper()
	if err := a.store.InsertThreadRequest(store.ThreadRequest{
		Token: token, CallerThreadID: caller, Kind: store.ThreadRequestSpawn, TargetThreadID: worker,
		State: store.ThreadRequestRunning, CreatedAt: time.Now().Add(-2 * time.Hour).UnixMilli(),
	}); err != nil {
		t.Fatalf("insert %s: %v", token, err)
	}
	if row := requestRow(t, a, token); row.RunBase != nil {
		t.Fatalf("seeded spawn %+v; this test needs one that tracks no run", row)
	}
}

// settleSpawnBeforeRuns settles token replied an hour ago, its answer taken.
func settleSpawnBeforeRuns(t *testing.T, a *App, token string) {
	t.Helper()
	settled, err := a.store.SettleThreadRequest(token, store.ThreadRequestOpenStates(), store.ThreadRequestSettlement{
		State: store.ThreadRequestReplied, Answer: []byte("done for now"), AnswerKind: store.ThreadAnswerReply,
		SettledAt: time.Now().Add(-time.Hour).UnixMilli(),
	})
	if err != nil || !settled {
		t.Fatalf("settle %s: settled=%v err=%v", token, settled, err)
	}
}

// assertSpawnerClosed checks that nothing of the spawner's is open on the
// worker: no open row, nothing counted, at rest by both reads of rest, and
// nothing for Stop All.
func (f *requestFixture) assertSpawnerClosed(t *testing.T, worker string) {
	t.Helper()
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
	if rest, err := f.app.threadRest(f.caller.ID); err != nil || rest.unrest != "" {
		t.Fatalf("the spawner reads %+v, %v; want it at rest", rest, err)
	}
	read, err := f.app.readRest([]string{f.caller.ID})
	defer read.done()
	if err != nil || !read.rests(f.caller.ID) {
		t.Fatalf("the decision reads the spawner not at rest (%v); want it at rest", err)
	}
	if route := f.app.routeWorkerPrompt(worker); route != nil {
		t.Fatalf("the worker's prompts route to %+v; want the person's", route)
	}
	stops, err := f.app.StopThreadAgentRows(t.Context(), f.caller.ID)
	if err != nil || len(stops) != 0 {
		t.Fatalf("Stop All = %+v, %v; want nothing of the spawner's to stop", stops, err)
	}
}

// A worker spawned before runs were kept, with a sidebar row, answered and
// rested; the person then messages it. While that turn runs and asks an
// approval, its old spawner lists nothing open, reads at rest, owns no
// Stop, and the approval is the person's. The spawn's Past row shows none
// of the worker's figures, which it never recorded. An owned worker's is a
// nudge (TestANudgeOfAWorkerSpawnedBeforeRunsIsARunOfItsOwn).
func TestASpawnFromBeforeRunsStaysClosedWhenThePersonContinuesItsWorker(t *testing.T) {
	t.Parallel()
	f := newRequestFixture(t)
	f.watchAgentRows()
	f.app.triage.SetPromptRouter(f.app.routeWorkerPrompt)
	f.bus.mu.Lock()
	f.bus.tap = func(name eventchan.Channel, data any) {
		f.app.tapThreadAgentRows(name, data)
		f.app.tapWorkerPrompt(name, data)
	}
	f.bus.mu.Unlock()
	worker := f.plainThread(t)
	if _, err := f.app.UpdateThreadRuntimeMode(context.Background(), worker.ID, string(provider.RuntimeApprovalRequired)); err != nil {
		t.Fatal(err)
	}
	seedSpawnBeforeRuns(t, f.app, "old-spawn", f.caller.ID, worker.ID)
	workerTurn(t, f.app, worker.ID, "t0", 2, 60)
	settleSpawnBeforeRuns(t, f.app, "old-spawn")

	var mu sync.Mutex
	var woken []string
	f.app.startSessionFn = func(threadID string) error {
		if threadID == worker.ID {
			return f.app.startSessionNow(threadID)
		}
		mu.Lock()
		woken = append(woken, threadID)
		mu.Unlock()
		return errors.New("no provider for the spawner in this test")
	}
	installMockClaudeTurns(t, f.app, [][]string{{
		mockClaudeInitLine,
		`{"type":"control_request","request_id":"req-approve","request":{"subtype":"can_use_tool","tool_name":"Bash","input":{"command":"make deploy"}}}`,
	}})
	t.Cleanup(func() { stopSessionChecked(t, f.app, worker.ID) })
	if _, err := f.app.SendMessageWithOptions(context.Background(), worker.ID, "now deploy it", pageSendOptions(uuid.NewString())); err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	waitUntil(t, 10*time.Second, func() bool {
		return len(f.app.triage.PendingInteractiveRequests(worker.ID).Approvals) == 1
	})
	if err := f.app.drainWorkerPrompts(context.Background(), 5*time.Second); err != nil {
		t.Fatal(err)
	}
	f.app.waitAgentNoticeWork()
	if route := f.app.triage.PendingInteractiveRequests(worker.ID).Approvals[0].RoutedTo; route != nil {
		t.Fatalf("the approval is routed to %+v; want the person's", route)
	}
	if tokens := workerPromptTokens(t, f.app, f.caller.ID); len(tokens) != 0 {
		t.Fatalf("the spawner holds prompt wakes %q; want none", tokens)
	}
	mu.Lock()
	if len(woken) != 0 {
		t.Fatalf("threads %q were started for the worker's prompt; want none", woken)
	}
	mu.Unlock()
	// The worker's state is recorded as its approval's frame is handled.
	waitUntil(t, 10*time.Second, func() bool { return f.app.liveWorker(worker.ID) })
	f.assertSpawnerClosed(t, worker.ID)
	row, ok := f.pastRow(t, f.caller.ID, "old-spawn")
	if !ok || row.ToolCalls != 0 || row.Tokens != 0 || row.RuntimeMs != 0 || row.TurnOpen || row.Worker != "" {
		t.Fatalf("spawn's Past row %+v (found %v); want it settled with no figures", row, ok)
	}
}

// A spawn from before runs were kept that had not settled at the upgrade
// keeps its worker as before: open, counted, routing the worker's prompts
// to its spawner. Once it settles it is closed, though the worker works on.
func TestASpawnFromBeforeRunsIsOpenUntilItSettles(t *testing.T) {
	t.Parallel()
	f := newRequestFixture(t)
	f.watchAgentRows()
	worker := f.plainThread(t)
	seedSpawnBeforeRuns(t, f.app, "open-old-spawn", f.caller.ID, worker.ID)
	workerTurn(t, f.app, worker.ID, "t0", 2, 60)
	workerEvent(t, f.app, worker.ID, provider.ProviderEvent{Kind: provider.EventTurnStart, TurnID: worker.ID + ":t1"}, nil)
	workerToolCall(t, f.app, worker.ID, "t1-a", "make test")
	waitUntil(t, 10*time.Second, func() bool { return f.app.liveWorker(worker.ID) })

	row := f.awaitWorker(t, f.caller.ID, "open-old-spawn", workerRunning)
	if row.ToolCalls != 3 || row.Tokens != 60 {
		t.Fatalf("row %+v; want the worker's life, three calls and 60 tokens, as before runs were kept", row)
	}
	if got := f.workerSummary(t, f.caller.ID); got.Open != 1 {
		t.Fatalf("summary %+v; want the worker counted", got)
	}
	if route := f.app.routeWorkerPrompt(worker.ID); route == nil || route.ThreadID != f.caller.ID {
		t.Fatalf("the worker's prompts route to %+v; want its spawner", route)
	}

	settleSpawnBeforeRuns(t, f.app, "open-old-spawn")
	if !f.app.liveWorker(worker.ID) {
		t.Fatal("the worker stopped; the test needs it busy after the spawn settled")
	}
	f.assertSpawnerClosed(t, worker.ID)
}

// The same on a paired computer: a worker there that a spawn from before
// runs were kept made, and its receipt there, which tracks none either.
// Once both settled, the person continuing the worker there, given a
// sidebar row there, routes its prompts to the person, and the spawner here
// reads at rest.
func TestARemoteSpawnFromBeforeRunsStaysClosed(t *testing.T) {
	t.Parallel()
	pair := newReachPair(t)
	spawn, child := remoteRestedWorker(t, pair)
	execOnStore(t, pair.dbPath, `UPDATE thread_requests SET run_base_tool_calls = NULL, run_base_tokens = NULL, run_base_turn_ms = NULL,
		run_tool_calls = NULL, run_tokens = NULL, run_turn_ms = NULL WHERE token = ?`, spawn.Token)
	execOnStore(t, pair.destDBPath, `UPDATE thread_request_receipts SET run_open = NULL, run_tool_calls = NULL, run_tokens = NULL, run_turn_ms = NULL
		WHERE token = ?`, spawn.Token)
	if row := pair.request(t, spawn.Token); row.RunBase != nil {
		t.Fatalf("spawn %+v; the test needs one that tracks no run", row)
	}
	if _, err := pair.dest.store.SetThreadPlacement([]string{child}, store.PlacementStandalone); err != nil {
		t.Fatal(err)
	}

	if _, err := pair.dest.SendMessageWithOptions(context.Background(), child, "now also update the changelog", pageSendOptions(uuid.NewString())); err != nil {
		t.Fatalf("SendMessage there: %v", err)
	}
	waitUntil(t, 10*time.Second, func() bool {
		return threadToolsLiveState(pair.dest.triage.LiveStateSnapshotForThread(child)).ActiveTurn
	})
	if route := pair.dest.routeWorkerPrompt(child); route != nil {
		t.Fatalf("a prompt of the person's run there routes to %+v; want the person's", route)
	}
	agentPair := &agentPeerPair{reachPair: pair, spawn: spawn, child: child}
	agentPair.askAgainUntil(t, func(row store.ThreadRequest) bool { return row.NestedOpen })
	if rest, err := pair.source.threadRest(pair.caller.ID); err != nil || rest.unrest != "" {
		t.Fatalf("the spawner reads %+v, %v; want it at rest while the person runs its worker there", rest, err)
	}
	read, err := pair.source.readRest([]string{pair.caller.ID})
	defer read.done()
	if err != nil || !read.rests(pair.caller.ID) {
		t.Fatalf("the decision reads the spawner not at rest (%v); want it at rest", err)
	}
	listing, err := pair.source.ListThreadAgentRows(t.Context(), pair.caller.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range listing.Rows {
		if row.open() {
			t.Fatalf("open row %+v; want none for the person's run there", row)
		}
	}
}
