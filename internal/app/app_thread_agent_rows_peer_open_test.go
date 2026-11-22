package app

// A paired computer's agent_rows answer says whether its thread is open,
// at rest or not, with open work at any depth below it, whatever rows the
// answer has room for (docs/specs/standing-agents.md, "Owned workers"). A
// spawn at the listing's last level asks that computer for the state alone,
// so the asking stops at the bound however long the chain of computers.

import (
	"fmt"
	"path/filepath"
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
		return row.Worker == string(workerWaiting) && pair.request(t, token).NestedOpen && done(listing)
	})
	return row
}

// A remote worker with nothing of its own left whose only open work is a
// worker it started in its spawn's run, which its listing has no room for
// past 128 rows settled since, stays open: its computer's answer says so
// whatever rows it returns, so the source keeps the spawn waiting, counted
// and out of Past.
func TestARemoteWorkersOpenWorkPastItsListingsLimitKeepsItOpen(t *testing.T) {
	t.Parallel()
	remotetest.Require(t)
	pair := newReachPair(t)
	gate := filepath.Join(t.TempDir(), "spawn-turn")
	installMockClaudeTurns(t, pair.dest, [][]string{gatedTurn(gate, "Handing it on.")})
	ack := pair.spawnThere(t, "do the first pass", nil)
	child := pair.receipt(t, ack.Token).TargetThreadID
	waitUntil(t, 10*time.Second, func() bool {
		return threadToolsLiveState(pair.dest.triage.LiveStateSnapshotForThread(child)).ActiveTurn
	})
	late := remoteWorkerStartsOneOfItsOwn(t, pair, child)
	release(t, gate)
	waitUntil(t, 10*time.Second, func() bool { return pair.dest.restHeld(child, ack.Token) })
	spawn := pair.request(t, ack.Token)
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
		if !row.open() || row.Worker != string(workerWaiting) || !row.NestedHidden || row.NestedUnavailable != "" || len(listing.Rows) != 1 || listing.Hidden {
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
	(&agentPeerPair{reachPair: pair, spawn: spawn}).expectRemoteRowOpen(t, row, string(workerWaiting))
}

// A chain of remote workers longer than the listing's depth, each at rest
// with only the last one running: every spawn the listing shows is open,
// its request held running while the work below it runs, waiting on it,
// counted and out of Past, and the last one shown says more nests below
// it. Its computer is asked for its state alone, and the computer past it
// is asked nothing. The computers predate sub-thread levels, the only way
// a chain grows this deep.
func TestARemoteChainPastTheDepthBoundStaysOpenAndIsNotListedPastIt(t *testing.T) {
	t.Parallel()
	n := threadAgentRowDepth + 2
	c := newAgentChain(t, n)
	for _, peer := range c.peers[1:] {
		peer.levelless.Store(true)
	}
	installMockClaudeReplies(t, c.apps[0], "done", "noted", "noted", "noted")
	release := make([]func(), n-1)
	for i := 1; i < n-1; i++ {
		release[i] = installChainLink(t, c.apps[i])
	}
	installMockClaudeTurns(t, c.apps[n-1], [][]string{{
		mockClaudeInitLine,
		`{"type":"assistant","message":{"id":"msg-hold","role":"assistant","content":[{"type":"text","text":"working"}]}}`,
	}})
	for i := range n - 1 {
		c.spawnAlong(t, i, fmt.Sprintf("link %d", i))
		if i > 0 {
			release[i]()
		}
	}
	for i := range n - 2 {
		c.held(t, i)
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
			if row.Worker != string(workerWaiting) || !row.open() {
				return false
			}
		}
		return listing.Rows[len(listing.Rows)-1].NestedHidden
	}
	var listing ThreadAgentRows
	// seen is the listing as a failed wait reports it.
	seen := func() string {
		at, err := c.entryAt(0, c.spawns[0])
		out := fmt.Sprintf("hidden %v, first answer %s after the reset (%v);", listing.Hidden, at.Sub(reset).Round(time.Millisecond), err)
		for _, row := range listing.Rows {
			out += fmt.Sprintf(" [%d: %s worker %s open %v nested hidden %v %q]", slices.Index(c.spawns, row.Token),
				row.State, row.Worker, row.open(), row.NestedHidden, row.NestedUnavailable)
		}
		return out
	}
	await := func(what string, done func() bool) {
		t.Helper()
		for deadline := time.Now().Add(peerRowsWait); ; time.Sleep(10 * time.Millisecond) {
			listing = read()
			if done() {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s: %s", what, seen())
			}
		}
	}
	await("the chain never read open", func() bool {
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
		if past, err := app.ListThreadPastAgentRows(t.Context(), caller, store.PastAgentRowsCursor{}, 8); err != nil || past.Total != 0 {
			t.Fatalf("past on computer %d = %+v, %v; want its open worker kept out", i, past, err)
		}
	}

	// A later answer finds the same. The listing that carries it is the one
	// judged: a read after it can outrun a computer's answer, which shows
	// that computer as not answering.
	before, _ := c.entryAt(0, c.spawns[0])
	later := false
	await("after a later answer the chain did not read open", func() bool {
		at, _ := c.entryAt(0, c.spawns[0])
		later = later || at.After(before)
		return later && open(listing)
	})
	expectBoundedAsking()
}
