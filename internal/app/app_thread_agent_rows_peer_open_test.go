package app

// A paired computer's agent_rows answer says whether its thread is open,
// at rest or not, with open work at any depth below it, whatever rows the
// answer has room for (docs/specs/standing-agents.md, "Owned workers"). A
// spawn at the listing's last level asks that computer for the state alone,
// so the asking stops at the bound however long the chain of computers.

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"testing"
	"time"

	"agent-overflow/internal/buildvariant/remotetest"
	"agent-overflow/internal/store"
	"agent-overflow/internal/threadtools"
)

// remoteWorkerStartsOneOfItsOwn has the thread child on the pair's
// destination spawn a worker there whose turn runs on, and returns the
// spawn's token.
func remoteWorkerStartsOneOfItsOwn(t *testing.T, pair *reachPair, child string) string {
	t.Helper()
	installMockClaudeTurns(t, pair.dest, [][]string{{
		mockClaudeInitLine,
		`{"type":"assistant","message":{"id":"msg-late","role":"assistant","content":[{"type":"text","text":"still going"}]}}`,
	}})
	destID, _ := pair.dest.backendIdentity()
	late, err := threadToolsApp{app: pair.dest}.Spawn(t.Context(),
		threadtools.Caller{ThreadID: child, ComputerID: destID, ComputerName: "destination"},
		threadtools.SpawnCall{Prompt: "keep going", Title: "Late worker"})
	if err != nil {
		t.Fatalf("the spawn there: %v", err)
	}
	waitUntil(t, 10*time.Second, func() bool {
		receipt, found, err := pair.dest.store.GetThreadRequestReceipt(late.Token)
		return err == nil && found && receipt.State == store.ThreadReceiptRunning
	})
	return late.Token
}

// paneShowsTheRemoteSpawnOpen reads the source's listing, as its pane does,
// until the spawn's row is waiting on the work under its thread there, the
// source has recorded that work, and the row nests what done wants.
func paneShowsTheRemoteSpawnOpen(t *testing.T, pair *reachPair, token string, done func(ThreadAgentRows) bool) ThreadAgentRow {
	t.Helper()
	var row ThreadAgentRow
	waitUntil(t, 20*time.Second, func() bool {
		listing, err := pair.source.ListThreadAgentRows(t.Context(), pair.caller.ID)
		if err != nil {
			t.Fatal(err)
		}
		row, _ = agentRowByToken(listing.Rows, token)
		return row.Worker == workerWaiting && pair.request(t, token).NestedOpen && done(listing)
	})
	return row
}

// A remote worker at rest whose only open work is a worker of its own that
// its listing has no room for, past 128 rows settled since, stays open: its
// computer's answer says so whatever rows it returns, so the source keeps
// the spawn waiting, counted and out of Past.
func TestARemoteWorkersOpenWorkPastItsListingsLimitKeepsItOpen(t *testing.T) {
	t.Parallel()
	remotetest.Require(t)
	pair := newReachPair(t)
	spawn, child := remoteRestedWorker(t, pair)
	late := remoteWorkerStartsOneOfItsOwn(t, pair, child)
	// Answering for the caller one level deep, as for a computer that asked
	// about it, this computer has the spawn at its last level: the first
	// answer shows it waiting on the work there, with more nested below.
	lastLevel := func() ThreadAgentRow {
		t.Helper()
		listing, _, err := pair.source.threadAgentTreeDepth(t.Context(), newThreadAgentView(pair.source), pair.caller.ID, 1, threadAgentPeersLive)
		if err != nil {
			t.Fatal(err)
		}
		row, _ := agentRowByToken(listing.Rows, spawn.Token)
		if !row.open() || row.Worker != workerWaiting || !row.NestedHidden || row.NestedUnavailable != "" || len(listing.Rows) != 1 || listing.Hidden {
			t.Fatalf("at the last level the listing = %+v; want the spawn alone, waiting, saying more nests below", listing)
		}
		return row
	}
	lastLevel()
	paneShowsTheRemoteSpawnOpen(t, pair, spawn.Token, func(listing ThreadAgentRows) bool {
		_, nested := agentRowByToken(listing.Rows, late)
		return nested
	})
	// An answer of the state alone leaves the rows the pane nests, and
	// their age.
	answeredAt := func() time.Time {
		cache := &pair.source.threadAgentPeers
		cache.mu.Lock()
		defer cache.mu.Unlock()
		return cache.entries[spawn.Token].at
	}
	pair.source.threadRequestsWG.Wait()
	before := answeredAt()
	lastLevel()
	answer, _ := pair.source.cachedPeerAgentRows(spawn)
	if after := answeredAt(); !after.Equal(before) {
		t.Fatalf("an answer of the state alone moved the rows' age from %v to %v", before, after)
	}
	if _, nested := agentRowByToken(answer.listing.Rows, late); !nested {
		t.Fatalf("after an answer of the state alone the kept answer = %+v; want its rows", answer.listing)
	}

	// Then 128 earlier requests of the thread there settle: its listing
	// returns them and has no room for the worker that runs.
	lateRow, found, err := pair.dest.store.GetThreadRequest(late)
	if err != nil || !found {
		t.Fatalf("the late spawn there: %v, %v", found, err)
	}
	settledAt := time.Now().UnixMilli()
	for i := range threadAgentRowLimit {
		seedSettledAgentRow(t, pair.dest, store.ThreadRequest{
			Token: fmt.Sprintf("earlier-%03d", i), CallerThreadID: child, Kind: store.ThreadRequestSpawn,
			CreatedAt: lateRow.CreatedAt - 60_000 + int64(i),
		}, settledAt, "done")
	}
	waitUntil(t, 20*time.Second, func() bool {
		if _, err := pair.source.ListThreadAgentRows(t.Context(), pair.caller.ID); err != nil {
			t.Fatal(err)
		}
		answer, ok := pair.source.cachedPeerAgentRows(spawn)
		return ok && answer.err == nil && answer.listing.Hidden
	})
	if !pair.request(t, spawn.Token).NestedOpen {
		t.Fatal("an answer with no room for the open worker cleared the spawn's open work")
	}
	listing, err := pair.source.ListThreadAgentRows(t.Context(), pair.caller.ID)
	if err != nil {
		t.Fatal(err)
	}
	row, _ := agentRowByToken(listing.Rows, spawn.Token)
	(&agentPeerPair{reachPair: pair, spawn: spawn}).expectRemoteRowOpen(t, row, workerWaiting)
}

// A chain of remote workers longer than the listing's depth, each at rest
// with only the last one running: every spawn the listing shows is open,
// waiting on the work below it, counted and out of Past, and the last one
// shown says more nests below it. Its computer is asked for its state
// alone, and the computer past it is asked nothing.
func TestARemoteChainPastTheDepthBoundStaysOpenAndIsNotListedPastIt(t *testing.T) {
	t.Parallel()
	n := threadAgentRowDepth + 2
	c := newAgentChain(t, n)
	for i := range n - 1 {
		installMockClaudeReplies(t, c.apps[i], "done", "noted", "noted", "noted")
	}
	installMockClaudeTurns(t, c.apps[n-1], [][]string{{
		mockClaudeInitLine,
		`{"type":"assistant","message":{"id":"msg-hold","role":"assistant","content":[{"type":"text","text":"working"}]}}`,
	}})
	for i := range n - 1 {
		c.spawnAlong(t, i, fmt.Sprintf("link %d", i))
	}
	for i := range n - 2 {
		c.settle(t, i)
	}
	c.atRest(t, 0, n-2)
	leaf := c.threads[n-2]
	waitUntil(t, 10*time.Second, func() bool {
		return threadToolsLiveState(c.apps[n-1].triage.LiveStateSnapshotForThread(leaf)).ActiveTurn
	})
	for _, peer := range c.peers[1:] {
		peer.calls.Store(0)
		peer.stateOnly.Store(0)
	}
	reset := time.Now()

	shown := c.spawns[:threadAgentRowDepth]
	read := func() ThreadAgentRows {
		listing, err := c.apps[0].ListThreadAgentRows(t.Context(), c.caller.ID)
		if err != nil {
			t.Fatal(err)
		}
		return listing
	}
	open := func(listing ThreadAgentRows) bool {
		// Every row within the bound is shown; the last says more nests
		// below it.
		if !slices.Equal(agentRowTokens(listing.Rows), shown) || listing.Hidden {
			return false
		}
		for _, row := range listing.Rows {
			if row.Worker != workerWaiting || !row.open() {
				return false
			}
		}
		return listing.Rows[len(listing.Rows)-1].NestedHidden
	}
	var listing ThreadAgentRows
	waitUntil(t, 20*time.Second, func() bool {
		listing = read()
		at, _ := c.entryAt(0, c.spawns[0])
		return at.After(reset) && open(listing)
	})
	expectBoundedAsking := func() {
		t.Helper()
		if calls := c.peers[n-1].calls.Load(); calls != 0 {
			t.Fatalf("the computer past the bound was asked for its rows %d times; want none", calls)
		}
		boundary := c.peers[n-2]
		if calls, state := boundary.calls.Load(), boundary.stateOnly.Load(); calls == 0 || calls != state {
			t.Fatalf("the computer at the bound was asked %d times, %d of them for its state alone; want only that", calls, state)
		}
	}
	expectBoundedAsking()
	if got, err := c.apps[0].GetThreadWorkerSummary(t.Context(), c.caller.ID); err != nil || got.Open != threadAgentRowDepth {
		t.Fatalf("summary %+v, %v; want the %d workers shown counted", got, err, threadAgentRowDepth)
	}
	for i, app := range c.apps[:n-2] {
		caller := c.caller.ID
		if i > 0 {
			caller = c.threads[i-1]
		}
		if past, err := app.ListThreadPastAgentRows(caller, store.PastAgentRowsCursor{}, 8); err != nil || past.Total != 0 {
			t.Fatalf("past on computer %d = %+v, %v; want its open worker kept out", i, past, err)
		}
	}

	// A later answer finds the same.
	before, _ := c.entryAt(0, c.spawns[0])
	waitUntil(t, 20*time.Second, func() bool {
		listing = read()
		at, _ := c.entryAt(0, c.spawns[0])
		return at.After(before)
	})
	if listing = read(); !open(listing) {
		t.Fatalf("after a later answer the rows = %+v; want each still open", listing.Rows)
	}
	expectBoundedAsking()
}

// olderOpenAgentsPeer is a computer from before agent_rows answered its
// thread's open state: it lists the levels it is asked for whatever else
// the call asks, and its answer says nothing of the state.
type olderOpenAgentsPeer struct{ *App }

func (p *olderOpenAgentsPeer) ThreadToolQuery(ctx context.Context, call ThreadPeerCall) (ThreadPeerReply, error) {
	if call.Tool != threadAgentPeerRows {
		return p.App.ThreadToolQuery(ctx, call)
	}
	var args map[string]any
	if err := json.Unmarshal(call.Args, &args); err != nil {
		return ThreadPeerReply{}, err
	}
	delete(args, "state_only")
	encoded, err := json.Marshal(args)
	if err != nil {
		return ThreadPeerReply{}, err
	}
	call.Args = encoded
	reply, err := p.App.ThreadToolQuery(ctx, call)
	if err != nil {
		return reply, err
	}
	var answer map[string]any
	if err := json.Unmarshal(reply.Result, &answer); err != nil {
		return ThreadPeerReply{}, err
	}
	delete(answer, "open")
	reply.Result, err = json.Marshal(answer)
	return reply, err
}

// An older computer's answer says nothing of its thread's state: the
// source reads it from the rows the answer lists, as before, at the
// listing's last level too, and says nothing is wrong.
func TestAnOlderComputersOpenStateIsReadFromItsRows(t *testing.T) {
	t.Parallel()
	remotetest.Require(t)
	pair := newReachPairServedBy(t, func(t *testing.T, app *App) *pairedBackend {
		return serveAppAs(t, app, &olderOpenAgentsPeer{App: app})
	})
	spawn, child := remoteRestedWorker(t, pair)
	late := remoteWorkerStartsOneOfItsOwn(t, pair, child)
	row := paneShowsTheRemoteSpawnOpen(t, pair, spawn.Token, func(listing ThreadAgentRows) bool {
		_, nested := agentRowByToken(listing.Rows, late)
		return nested
	})
	if row.NestedUnavailable != "" || !row.open() {
		t.Fatalf("the spawn row = %+v; want it open and nothing wrong", row)
	}
	if past, err := pair.source.ListThreadPastAgentRows(pair.caller.ID, store.PastAgentRowsCursor{}, 8); err != nil || past.Total != 0 {
		t.Fatalf("past = %+v, %v; want the remote worker kept out", past, err)
	}

	// A listing one level deep, as this computer reads it to answer for the
	// spawn's caller, has the spawn at its last level.
	bounded, _, err := pair.source.threadAgentTreeDepth(t.Context(), newThreadAgentView(pair.source), pair.caller.ID, 1, threadAgentPeersLive)
	if err != nil {
		t.Fatal(err)
	}
	row, _ = agentRowByToken(bounded.Rows, spawn.Token)
	if !row.open() || row.Worker != workerWaiting || !row.NestedHidden || row.NestedUnavailable != "" || len(bounded.Rows) != 1 {
		t.Fatalf("at the last level the rows = %+v; want the spawn open, waiting, saying more nests below and nothing wrong", bounded.Rows)
	}
	if !pair.request(t, spawn.Token).NestedOpen {
		t.Fatal("the older computer's answer at the last level cleared the spawn's open work")
	}
}
