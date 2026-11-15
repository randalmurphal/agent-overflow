package app

// A worker on a paired computer that rested and is continued there later
// reopens for its spawner, as a local one does (docs/specs/standing-agents.md,
// "Owned workers"): nothing there tells this computer, so the sweep asks
// about the remote spawns at rest at its own slower pace.

import (
	"testing"
	"time"

	"agent-overflow/internal/buildvariant/remotetest"
	"agent-overflow/internal/store"
	"agent-overflow/internal/threadtools"
)

// sweepRemoteSpawns runs the sweep's pass over remote spawns as its clock
// would once each of its paces is due.
func sweepRemoteSpawns(t *testing.T, a *App) {
	t.Helper()
	c := &a.threadAgentPeers
	c.mu.Lock()
	for _, entry := range c.entries {
		entry.at = time.Time{}
	}
	c.restPassAt = time.Time{}
	c.mu.Unlock()
	a.refreshNestedOpenRemoteSpawns()
	a.threadRequestsWG.Wait()
}

// A remote worker that answered and rested is in its spawner's Past. Its
// person continues it from its own pane there: the sweep, with no read of
// the spawner's pane, finds it running, and its row is open again, counted
// and out of Past. Once it rests the row settles back.
func TestARemoteWorkerContinuedThereReopensForItsSpawner(t *testing.T) {
	t.Parallel()
	remotetest.Require(t)
	pair := newReachPair(t)
	installMockClaudeTurns(t, pair.dest, [][]string{{
		mockClaudeInitLine,
		`{"type":"assistant","message":{"id":"msg-1","role":"assistant","content":[{"type":"text","text":"The first pass is done."}]}}`,
		`{"type":"result","subtype":"success","is_error":false}`,
	}, {
		mockClaudeInitLine,
		`{"type":"assistant","message":{"id":"msg-2","role":"assistant","content":[{"type":"text","text":"Working on the follow-up."}]}}`,
	}})
	ack := pair.spawnThere(t, "do the first pass", nil)
	child := pair.receipt(t, ack.Token).TargetThreadID
	waitUntil(t, 10*time.Second, func() bool { return pair.receipt(t, ack.Token).SettledAt != 0 })
	waitUntil(t, 10*time.Second, func() bool {
		return !threadToolsLiveState(pair.dest.triage.LiveStateSnapshotForThread(child)).ActiveTurn
	})
	pair.poll(t)
	waitUntil(t, 10*time.Second, func() bool {
		pair.source.threadRequestsWG.Wait()
		return threadRequestSettled(pair.request(t, ack.Token))
	})
	agentPair := &agentPeerPair{reachPair: pair, spawn: pair.request(t, ack.Token)}
	rested := func(_ ThreadAgentRow, listed bool) bool { return !listed }
	agentPair.askAgainUntilRow(t, rested)
	if past, err := pair.source.ListThreadPastAgentRows(pair.caller.ID, store.PastAgentRowsCursor{}, 8); err != nil || past.Total != 1 {
		t.Fatalf("past = %+v, %v; want the rested worker's spawn", past, err)
	}
	// Nothing of that answer is kept: the sweep's asking holds no answer
	// for a spawn at rest.
	pair.source.threadAgentPeers.mu.Lock()
	clear(pair.source.threadAgentPeers.entries)
	pair.source.threadAgentPeers.mu.Unlock()

	if err := pair.dest.SendMessage(child, "now also update the changelog", nil); err != nil {
		t.Fatalf("SendMessage there: %v", err)
	}
	waitUntil(t, 10*time.Second, func() bool {
		return threadToolsLiveState(pair.dest.triage.LiveStateSnapshotForThread(child)).ActiveTurn
	})
	var row ThreadAgentRow
	waitUntil(t, 10*time.Second, func() bool {
		sweepRemoteSpawns(t, pair.source)
		view := newThreadAgentView(pair.source)
		view.since = time.Now().Add(time.Second).UnixMilli()
		listing, _, err := pair.source.threadAgentTreeDepth(t.Context(), view, pair.caller.ID, threadAgentRowDepth, threadAgentPeersCached)
		if err != nil {
			t.Fatal(err)
		}
		row, _ = agentRowByToken(listing.Rows, ack.Token)
		return row.Worker == workerRunning
	})
	if row.State != threadtools.RequestFinished {
		t.Fatalf("row %+v; want the finished spawn open while its worker runs", row)
	}
	agentPair.expectRemoteRowOpen(t, row, workerRunning)

	if err := pair.dest.StopSession(child); err != nil {
		t.Fatalf("StopSession there: %v", err)
	}
	agentPair.askAgainUntilRow(t, rested)
	if past, err := pair.source.ListThreadPastAgentRows(pair.caller.ID, store.PastAgentRowsCursor{}, 8); err != nil || past.Total != 1 {
		t.Fatalf("past = %+v, %v; want the spawn back", past, err)
	}
}

// The sweep's asking about remote spawns at rest is paced and bounded: one
// page of them a pass, each asked once a cycle, a pass at most every
// threadAgentRestPassEvery, and an answer that finds a spawn still at rest
// is kept nowhere and frames nothing.
func TestTheSweepAsksAboutRestingRemoteSpawnsAPageAtATime(t *testing.T) {
	t.Parallel()
	remotetest.Require(t)
	var peer *stalledAgentsPeer
	pair := newReachPairServedBy(t, serveSlowAgentsPeer(&peer, time.Millisecond))
	settledAt := time.Now().Add(-time.Hour).UnixMilli()
	total := threadRequestPollBatch + 3
	for i := range total {
		token := "resting-" + string(rune('a'+i/26)) + string(rune('a'+i%26))
		if err := pair.source.store.InsertThreadRequest(store.ThreadRequest{
			Token: token, CallerThreadID: pair.caller.ID, Kind: store.ThreadRequestSpawn,
			TargetComputerID: pair.computer, TargetThreadID: "gone-" + token, State: store.ThreadRequestRunning,
		}); err != nil {
			t.Fatal(err)
		}
		if settled, err := pair.source.store.SettleThreadRequest(token, store.ThreadRequestOpenStates(), store.ThreadRequestSettlement{
			State: store.ThreadRequestFinished, Answer: []byte("done"), AnswerKind: store.ThreadAnswerFinal, SettledAt: settledAt + int64(i),
		}); err != nil || !settled {
			t.Fatalf("settle %s: %v %v", token, settled, err)
		}
	}
	// The seeds' own frames are sent before the count starts.
	pair.source.threadRequestsWG.Wait()
	frames := recordAgentRowFrames(pair.source)
	pass := func(due bool) int32 {
		t.Helper()
		before := peer.calls.Load()
		if due {
			pair.source.threadAgentPeers.mu.Lock()
			pair.source.threadAgentPeers.restPassAt = time.Time{}
			pair.source.threadAgentPeers.mu.Unlock()
		}
		pair.source.refreshNestedOpenRemoteSpawns()
		pair.source.threadRequestsWG.Wait()
		return peer.calls.Load() - before
	}
	if got := pass(true); got != threadRequestPollBatch {
		t.Fatalf("the first pass asked %d times; want a page of %d", got, threadRequestPollBatch)
	}
	if got := pass(false); got != 0 {
		t.Fatalf("a pass before the pace asked %d times; want none", got)
	}
	if got := pass(true); got != 3 {
		t.Fatalf("the second pass asked %d times; want the 3 left", got)
	}
	if got := pass(true); got != threadRequestPollBatch {
		t.Fatalf("the next cycle asked %d times; want a page of %d from the start", got, threadRequestPollBatch)
	}
	pair.source.threadAgentPeers.mu.Lock()
	cached := len(pair.source.threadAgentPeers.entries)
	pair.source.threadAgentPeers.mu.Unlock()
	if cached != 0 {
		t.Fatalf("%d answers cached; want none kept for spawns at rest", cached)
	}
	if got := frames.count(pair.caller.ID); got != 0 {
		t.Fatalf("%d frames; want none for answers that changed nothing", got)
	}
}

// With remote spawns at rest to ask about, the sweep wakes for the rest
// pass's next page rather than sleeping to the retention clock.
func TestTheSweepWakesForTheRestPass(t *testing.T) {
	t.Parallel()
	f := newRequestFixture(t)
	idle := f.app.threadRequestSweepDelay(time.Now())
	c := &f.app.threadAgentPeers
	c.mu.Lock()
	c.restPassAt, c.restPending = time.Now(), true
	c.mu.Unlock()
	every := f.app.threadAgentRestPassEvery()
	if delay := f.app.threadRequestSweepDelay(time.Now()); delay > every || idle <= every {
		t.Fatalf("the sweep sleeps %v with spawns at rest, %v without; want at most %v with them", delay, idle, every)
	}
}

// A computer that runs an older version cannot list a thread's agents: the
// rest pass's question to it records nothing, keeps nothing and frames
// nothing, and the spawn stays at rest here.
func TestTheRestPassDegradesOnAnOlderComputer(t *testing.T) {
	t.Parallel()
	remotetest.Require(t)
	pair := newReachPairServedBy(t, serveOlderAgentsPeer)
	if err := pair.source.store.InsertThreadRequest(store.ThreadRequest{
		Token: "resting-older", CallerThreadID: pair.caller.ID, Kind: store.ThreadRequestSpawn,
		TargetComputerID: pair.computer, TargetThreadID: "there", State: store.ThreadRequestRunning,
	}); err != nil {
		t.Fatal(err)
	}
	if settled, err := pair.source.store.SettleThreadRequest("resting-older", store.ThreadRequestOpenStates(), store.ThreadRequestSettlement{
		State: store.ThreadRequestFinished, Answer: []byte("done"), AnswerKind: store.ThreadAnswerFinal, SettledAt: time.Now().Add(-time.Hour).UnixMilli(),
	}); err != nil || !settled {
		t.Fatalf("settle: %v %v", settled, err)
	}
	pair.source.threadRequestsWG.Wait()
	frames := recordAgentRowFrames(pair.source)
	before := pair.request(t, "resting-older").Revision
	sweepRemoteSpawns(t, pair.source)
	c := &pair.source.threadAgentPeers
	c.mu.Lock()
	cached, backoff := len(c.entries), len(c.backoff)
	c.mu.Unlock()
	after := pair.request(t, "resting-older")
	if cached != 0 || backoff != 0 || frames.count(pair.caller.ID) != 0 || after.NestedOpen || after.Revision != before {
		t.Fatalf("cached %d, backoff %d, frames %d, row %+v; want nothing recorded", cached, backoff, frames.count(pair.caller.ID), after)
	}
}
