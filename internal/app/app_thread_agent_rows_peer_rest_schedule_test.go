package app

// The sweep's own clock finds a remote worker that rested and was continued
// there: whether a rest pass is due comes from the ledger, so the first
// remote spawn to come to rest after a pass that found none is still asked
// about at the rest pass's pace (docs/specs/standing-agents.md, "Owned
// workers").

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"agent-overflow/internal/buildvariant/remotetest"
	"agent-overflow/internal/store"
	"agent-overflow/internal/testutil"
	"agent-overflow/internal/threadtools"
)

// The production sweep runs on the source with a short poll. Its first pass
// finds no remote spawn at rest. A worker is then spawned on the paired
// computer, replies, and rests there inside the rest pass's interval, so its
// request poll retires and nothing else is due. The sweep still wakes for
// the rest pass, and once the worker is continued there its row reopens,
// with no clock reset and no flag set by hand.
func TestTheFirstRemoteWorkerToRestIsStillAskedAbout(t *testing.T) {
	t.Parallel()
	remotetest.Require(t)
	pair := newReachPair(t)
	gate := filepath.Join(t.TempDir(), "reply-sent")
	installMockClaudeTurns(t, pair.dest, [][]string{{
		mockClaudeInitLine,
		`{"type":"assistant","message":{"id":"msg-1","role":"assistant","content":[{"type":"text","text":"Starting the first pass."}]}}`,
		testutil.MockWaitForFile(gate),
		`{"type":"result","subtype":"success","is_error":false}`,
	}, {
		mockClaudeInitLine,
		`{"type":"assistant","message":{"id":"msg-2","role":"assistant","content":[{"type":"text","text":"Working on the follow-up."}]}}`,
	}})
	// The reply wakes the spawner here, which takes a turn for it.
	installMockClaudeReplies(t, pair.source, "Noted.", "Noted.")
	// A rest pass every 12 s: long enough for the worker below to reply and
	// rest after the first, empty pass and before the second is due.
	pair.source.threadPollOverride = 2 * time.Second
	every := pair.source.threadAgentRestPassEvery()
	pair.source.startThreadRequestSweeps()
	waitUntil(t, 10*time.Second, func() bool {
		c := &pair.source.threadAgentPeers
		c.mu.Lock()
		defer c.mu.Unlock()
		return !c.restPassAt.IsZero()
	})

	ack := pair.spawnThere(t, "do the first pass", nil)
	var child string
	waitUntil(t, 10*time.Second, func() bool {
		child = pair.receipt(t, ack.Token).TargetThreadID
		return child != "" && threadToolsLiveState(pair.dest.triage.LiveStateSnapshotForThread(child)).ActiveTurn
	})
	destID, _ := pair.dest.backendIdentity()
	there := threadtools.Caller{ThreadID: child, ComputerID: destID, ComputerName: "destination"}
	if _, err := (threadToolsApp{app: pair.dest}).Reply(t.Context(), there, threadtools.ReplyCall{Token: ack.Token, Text: "The first pass is done."}); err != nil {
		t.Fatalf("Reply there: %v", err)
	}
	if err := os.WriteFile(gate, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, 10*time.Second, func() bool {
		return !threadToolsLiveState(pair.dest.triage.LiveStateSnapshotForThread(child)).ActiveTurn
	})
	// The production poll collects the reply and retires the request's poll.
	waitUntil(t, 10*time.Second, func() bool {
		row := pair.request(t, ack.Token)
		return row.State == store.ThreadRequestReplied && !row.Polling && !row.NestedOpen
	})
	// The poll's own read of the spawn's rows has answered too, so what
	// finds the continued worker below is the rest pass.
	waitUntil(t, 10*time.Second, func() bool {
		c := &pair.source.threadAgentPeers
		c.mu.Lock()
		defer c.mu.Unlock()
		_, kept := c.entries[ack.Token]
		return kept && len(c.queued) == 0 && len(c.running) == 0
	})
	if pair.request(t, ack.Token).NestedOpen {
		t.Fatal("the poll's read found the worker at rest open")
	}
	if delay := pair.source.threadRequestSweepDelay(time.Now()); delay > every {
		t.Fatalf("the sweep sleeps %v with a remote spawn at rest; want at most the rest pass's %v", delay, every)
	}

	if err := pair.dest.SendMessage(child, "now also update the changelog", nil); err != nil {
		t.Fatalf("SendMessage there: %v", err)
	}
	// Nothing here reads the spawner's listing, which would ask that
	// computer itself: only the sweep's rest pass can record the work.
	waitUntil(t, 2*every+5*time.Second, func() bool { return pair.request(t, ack.Token).NestedOpen })
	var row ThreadAgentRow
	waitUntil(t, 10*time.Second, func() bool {
		view := newThreadAgentView(pair.source)
		view.since = time.Now().Add(time.Second).UnixMilli()
		listing, _, err := pair.source.threadAgentTreeDepth(t.Context(), view, pair.caller.ID, threadAgentRowDepth, threadAgentPeersCached)
		if err != nil {
			t.Fatal(err)
		}
		row, _ = agentRowByToken(listing.Rows, ack.Token)
		return row.Worker == workerRunning
	})
	agentPair := &agentPeerPair{reachPair: pair, spawn: pair.request(t, ack.Token)}
	agentPair.expectRemoteRowOpen(t, row, workerRunning)
}
