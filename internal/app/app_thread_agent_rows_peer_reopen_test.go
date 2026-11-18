package app

// A worker on a paired computer that rested and is continued there later
// reopens for its spawner, as a local one does (docs/specs/standing-agents.md,
// "Owned workers"): nothing there tells this computer, so the sweep asks
// about the remote spawns at rest at its own slower pace.

import (
	"context"
	"testing"
	"time"

	"agent-overflow/internal/buildvariant/remotetest"
	"agent-overflow/internal/store"
	"agent-overflow/internal/threadtools"
)

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
	if past, err := pair.source.ListThreadPastAgentRows(t.Context(), pair.caller.ID, store.PastAgentRowsCursor{}, 8); err != nil || past.Total != 1 {
		t.Fatalf("past = %+v, %v; want the rested worker's spawn", past, err)
	}
	// Nothing of that answer is kept: the sweep's asking holds no answer
	// for a spawn at rest.
	pair.source.threadAgentPeers.mu.Lock()
	clear(pair.source.threadAgentPeers.entries)
	pair.source.threadAgentPeers.mu.Unlock()

	startReconciledOwner(t, pair)
	if err := pair.dest.SendMessage(context.Background(), child, "now also update the changelog", nil); err != nil {
		t.Fatalf("SendMessage there: %v", err)
	}
	waitUntil(t, 10*time.Second, func() bool {
		return threadToolsLiveState(pair.dest.triage.LiveStateSnapshotForThread(child)).ActiveTurn
	})
	var row ThreadAgentRow
	waitUntil(t, 10*time.Second, func() bool {
		if !pair.request(t, ack.Token).NestedOpen {
			return false
		}
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
	if past, err := pair.source.ListThreadPastAgentRows(t.Context(), pair.caller.ID, store.PastAgentRowsCursor{}, 8); err != nil || past.Total != 1 {
		t.Fatalf("past = %+v, %v; want the spawn back", past, err)
	}
}
