package app

// A thread with open work below it is not at rest (docs/specs/standing-agents.md,
// "Owned workers"), however deep that work is: every spawn above it is open,
// whatever the listing has room to show.

import (
	"slices"
	"testing"
	"time"

	"agent-overflow/internal/store"
)

// settledChain seeds a chain of links settled long ago from the fixture's
// caller, one thread per link, and returns the threads, top first, and the
// links' tokens.
func (f *requestFixture) settledChain(t *testing.T, links int) ([]store.Thread, []string) {
	t.Helper()
	threads := make([]store.Thread, 0, links)
	tokens := make([]string, 0, links)
	caller := f.caller.ID
	for i := range links {
		token := "chain-" + string(rune('a'+i))
		thread := f.settledSpawn(t, token, caller)
		threads, tokens = append(threads, thread), append(tokens, token)
		caller = thread.ID
	}
	return threads, tokens
}

// expectChainOpen checks that the caller's listing shows every link it has
// room for open and waiting, the last shown one saying more nests below,
// that the summary counts each, that the named read agrees, and that no
// thread of the chain lists its link in Past.
func (f *requestFixture) expectChainOpen(t *testing.T, threads []store.Thread, tokens []string) {
	t.Helper()
	rows := f.listingAfterWindow(t, f.caller.ID).Rows
	shown := tokens[:threadAgentRowDepth]
	if got := agentRowTokens(rows); !slices.Equal(got, shown) {
		t.Fatalf("rows %v; want %v", got, shown)
	}
	for i, row := range rows {
		if !row.open() || row.Worker != string(workerWaiting) || row.Depth != i {
			t.Fatalf("row %d = %+v; want it open, waiting on what runs below it", i, row)
		}
	}
	if !rows[len(rows)-1].NestedHidden {
		t.Fatalf("last row %+v; want it to say its agents nest deeper", rows[len(rows)-1])
	}
	if got := f.workerSummary(t, f.caller.ID); got.Open != threadAgentRowDepth {
		t.Fatalf("summary %+v; want the %d workers shown counted", got, threadAgentRowDepth)
	}
	reads, err := f.app.GetThreadAgentRows(t.Context(), f.caller.ID, tokens[:1])
	if err != nil || len(reads) != 1 || !reads[0].Listed || reads[0].Row.Worker != string(workerWaiting) {
		t.Fatalf("named read %+v, %v; want the top link open", reads, err)
	}
	callers := append([]store.Thread{f.caller}, threads[:len(threads)-1]...)
	for i, caller := range callers {
		if past := f.pastTokens(t, caller.ID); len(past) != 0 {
			t.Fatalf("past of link %d's caller %v; want its open worker kept out", i, past)
		}
	}
}

// expectChainRested checks that the chain left the caller's listing for
// Past, every link with it.
func (f *requestFixture) expectChainRested(t *testing.T, threads []store.Thread, tokens []string) {
	t.Helper()
	waitUntil(t, 10*time.Second, func() bool {
		f.app.threadRequestsWG.Wait()
		return len(f.listingAfterWindow(t, f.caller.ID).Rows) == 0
	})
	if got := f.workerSummary(t, f.caller.ID); got.Open != 0 {
		t.Fatalf("summary %+v; want none", got)
	}
	callers := append([]store.Thread{f.caller}, threads[:len(threads)-1]...)
	for i, caller := range callers {
		if past := f.pastTokens(t, caller.ID); !slices.Equal(past, tokens[i:i+1]) {
			t.Fatalf("past of link %d's caller %v; want %s", i, past, tokens[i])
		}
	}
}

// A five-link chain settled long ago whose leaf blocks on an approval keeps
// every spawn above it open up to the root, past the listing's depth: each
// is waiting, counted and out of Past, and a frame reaches the root. Once
// the leaf rests every link settles into Past.
func TestALiveLeafKeepsEverySpawnAboveItOpen(t *testing.T) {
	t.Parallel()
	f := newRequestFixture(t)
	f.watchAgentRows()
	threads, tokens := f.settledChain(t, threadAgentRowDepth+1)
	if rows := f.listingAfterWindow(t, f.caller.ID).Rows; len(rows) != 0 {
		t.Fatalf("at rest the rows %v; want none", agentRowTokens(rows))
	}
	f.app.threadRequestsWG.Wait()
	rootFrames := f.callerAgentFrames(t, f.caller.ID)

	leaf := threads[len(threads)-1]
	approvalOn(t, f.app, leaf.ID, "leaf-approval")
	waitUntil(t, 10*time.Second, func() bool {
		f.app.threadRequestsWG.Wait()
		return f.callerAgentFrames(t, f.caller.ID) > rootFrames
	})
	f.expectChainOpen(t, threads, tokens)

	resolveApprovalOn(t, f.app, leaf.ID, "leaf-approval")
	f.expectChainRested(t, threads, tokens)
}

// The same chain whose leaf has nothing running but a request it is owed an
// answer for: stored open work deeper than the listing's marks count still
// holds every spawn above it open, and so does its answer while it is on
// its way to the leaf; its delivery settles them.
func TestOpenWorkBelowTheListingKeepsEverySpawnAboveItOpen(t *testing.T) {
	t.Parallel()
	f := newRequestFixture(t)
	f.watchAgentRows()
	threads, tokens := f.settledChain(t, threadAgentRowDepth+1)
	other := f.plainThread(t)
	leaf := threads[len(threads)-1]
	f.seedAgentRow(t, "leaf-send", leaf.ID, store.ThreadRequestSend, other.ID, true)
	f.expectChainOpen(t, threads, tokens)

	f.app.threadRequestsWG.Wait()
	rootFrames := f.callerAgentFrames(t, f.caller.ID)
	if settled, err := f.app.store.SettleThreadRequest("leaf-send", store.ThreadRequestOpenStates(), store.ThreadRequestSettlement{
		State: store.ThreadRequestReplied, Answer: []byte("done"), AnswerKind: store.ThreadAnswerReply,
		SettledAt: time.Now().Add(-time.Hour).UnixMilli(),
	}); err != nil || !settled {
		t.Fatalf("settle the leaf's send: %v, %v", settled, err)
	}
	f.app.threadRequestsWG.Wait()
	f.expectChainOpen(t, threads, tokens)
	if marked, err := f.app.store.MarkThreadRequestDelivered("leaf-send", store.ThreadWakeInline, time.Now().UnixMilli(), false); err != nil || !marked {
		t.Fatalf("deliver the leaf's answer: %v, %v", marked, err)
	}
	waitUntil(t, 10*time.Second, func() bool {
		f.app.threadRequestsWG.Wait()
		return f.callerAgentFrames(t, f.caller.ID) > rootFrames
	})
	f.expectChainRested(t, threads, tokens)
}

// A worker whose own request settled with its answer owed as a message is
// not at rest until a turn of it reads that answer: with no turn of its
// own, its spawn's row reads it queued, framed for its spawner as its
// request changes. Once the answer reached it, its row settles into Past.
func TestAWorkersRowReadsQueuedWhileAnAnswerIsOnItsWayToIt(t *testing.T) {
	t.Parallel()
	f := newRequestFixture(t)
	f.watchAgentRows()
	worker, other := f.plainThread(t), f.plainThread(t)
	f.seedAgentRow(t, "spawn-w", f.caller.ID, store.ThreadRequestSpawn, worker.ID, false)
	settleSpawnByHand(t, f.app.store, "spawn-w")
	f.seedAgentRow(t, "w-ask", worker.ID, store.ThreadRequestSend, other.ID, true)
	f.app.threadRequestsWG.Wait()
	frames := f.callerAgentFrames(t, f.caller.ID)

	if settled, err := f.app.store.SettleThreadRequest("w-ask", store.ThreadRequestOpenStates(), store.ThreadRequestSettlement{
		State: store.ThreadRequestReplied, Answer: []byte("done"), AnswerKind: store.ThreadAnswerReply, SettledAt: time.Now().UnixMilli(),
	}); err != nil || !settled {
		t.Fatalf("settle the worker's ask: %v, %v", settled, err)
	}
	var row ThreadAgentRow
	waitUntil(t, 10*time.Second, func() bool {
		f.app.threadRequestsWG.Wait()
		row, _ = agentRowByToken(f.listingAfterWindow(t, f.caller.ID).Rows, "spawn-w")
		return f.callerAgentFrames(t, f.caller.ID) > frames && row.Worker == string(workerQueued)
	})
	if !row.open() {
		t.Fatalf("row %+v; want it open while the answer is on its way", row)
	}

	if marked, err := f.app.store.MarkThreadRequestDelivered("w-ask", store.ThreadWakeInline, time.Now().UnixMilli(), false); err != nil || !marked {
		t.Fatalf("deliver the answer: %v, %v", marked, err)
	}
	waitUntil(t, 10*time.Second, func() bool {
		f.app.threadRequestsWG.Wait()
		_, listed := agentRowByToken(f.listingAfterWindow(t, f.caller.ID).Rows, "spawn-w")
		return !listed
	})
	if past := f.pastTokens(t, f.caller.ID); !slices.Contains(past, "spawn-w") {
		t.Fatalf("past %v; want the spawn there", past)
	}
}
