package app

// A worker on a paired computer that rested is in its spawner's Past, and a
// run there its spawner did not start opens no row here, as a local one
// opens none (docs/specs/standing-agents.md, "Agents segment"). A send of
// the spawner's to it is one row, the run's.

import (
	"context"
	"testing"
	"time"

	"agent-overflow/internal/store"
	"agent-overflow/internal/threadtools"
)

// sendThere sends message from the pair's caller to thread on the
// destination and returns its receipt.
func (p *reachPair) sendThere(t *testing.T, thread, message string) threadtools.RequestAck {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	ack, err := p.adapter().Send(ctx, p.callerIdentity(), threadtools.SendCall{ThreadID: thread, ComputerID: p.computer, Message: message, Notify: true})
	if err != nil {
		t.Fatalf("remote send: %v", err)
	}
	return ack
}

// restedRemoteRun is remoteRestedWorker with the spawn's run ended here
// once the destination answered that its thread is at rest.
func restedRemoteRun(t *testing.T, pair *reachPair) (*agentPeerPair, string) {
	t.Helper()
	spawn, child := remoteRestedWorker(t, pair)
	agentPair := &agentPeerPair{reachPair: pair, spawn: spawn, child: child}
	agentPair.askAgainUntil(t, func(row store.ThreadRequest) bool { return row.RunEnded() })
	if past, err := pair.source.ListThreadPastAgentRows(t.Context(), pair.caller.ID, store.PastAgentRowsCursor{}, 8); err != nil || past.Total != 1 {
		t.Fatalf("past = %+v, %v; want the rested worker's spawn", past, err)
	}
	return agentPair, child
}

// A remote owned worker that answered and rested is in its spawner's Past.
// Its person continues it from its own pane there: a nudge. The update
// finds it running, and the spawner lists the nudge's row for it, counts
// it, keeps the spawn in Past, and Stop All stops it there.
func TestARemoteWorkerContinuedThereIsANudgeRowOfItsSpawner(t *testing.T) {
	t.Parallel()
	pair := newReachPair(t)
	agentPair, child := restedRemoteRun(t, pair)
	// Start with the pane closed and its cached tree evicted.
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
	agentPair.askAgainUntil(t, func(row store.ThreadRequest) bool { return row.NestedOpen })
	view := newThreadAgentView(pair.source)
	view.since = time.Now().Add(time.Second).UnixMilli()
	listing, _, err := pair.source.threadAgentTreeDepth(t.Context(), view, pair.caller.ID, threadAgentRowDepth, threadAgentPeersCached)
	if err != nil || len(listing.Rows) != 1 || listing.Rows[0].Kind != threadAgentKindNudge || !listing.Rows[0].open() {
		t.Fatalf("listing %+v, %v; want the nudge's row, open, for the person's run there", listing.Rows, err)
	}
	if got, err := pair.source.GetThreadWorkerSummary(t.Context(), pair.caller.ID); err != nil || got.Open != 1 {
		t.Fatalf("summary %+v, %v; want the nudged worker counted", got, err)
	}
	if past, err := pair.source.ListThreadPastAgentRows(t.Context(), pair.caller.ID, store.PastAgentRowsCursor{}, 8); err != nil || past.Total != 1 {
		t.Fatalf("past = %+v, %v; want the spawn kept there", past, err)
	}
	if stops, err := pair.source.StopThreadAgentRows(t.Context(), pair.caller.ID); err != nil || len(stops) != 1 || stops[0].Token != listing.Rows[0].Token {
		t.Fatalf("Stop All = %+v, %v; want the nudge's row stopped", stops, err)
	}
	waitUntil(t, 10*time.Second, func() bool {
		return !threadToolsLiveState(pair.dest.triage.LiveStateSnapshotForThread(child)).ActiveTurn
	})
}

// The spawner sends a follow-up to its rested remote worker: its listing
// shows one row for the worker, the send's, and the spawn stays in Past.
func TestARemoteSendToARestedWorkerIsOneRow(t *testing.T) {
	t.Parallel()
	pair := newReachPair(t)
	agentPair, child := restedRemoteRun(t, pair)
	send := pair.sendThere(t, child, "now also update the changelog")
	if row := pair.request(t, send.Token); !row.RunOpen() {
		t.Fatalf("the spawner's send there %+v opens no run", row)
	}
	waitUntil(t, 10*time.Second, func() bool {
		return threadToolsLiveState(pair.dest.triage.LiveStateSnapshotForThread(child)).ActiveTurn
	})
	agentPair.askAgainUntil(t, func(row store.ThreadRequest) bool { return row.NestedOpen })
	listing := agentPair.listRowsUntil(t, func(rows ThreadAgentRows) bool {
		row, ok := agentRowByToken(rows.Rows, send.Token)
		return ok && row.open()
	})
	if len(listing.Rows) != 1 || listing.Rows[0].Token != send.Token || listing.Rows[0].ThreadID != child {
		t.Fatalf("rows %+v; want the send's row alone for the worker", listing.Rows)
	}
	if got := listing.Rows[0].Placement; got != store.PlacementOwned {
		t.Fatalf("the send's row placement %q; want its worker's there, owned", got)
	}
	if past, err := pair.source.ListThreadPastAgentRows(t.Context(), pair.caller.ID, store.PastAgentRowsCursor{}, 8); err != nil || past.Total != 1 || past.Rows[0].Token != agentPair.spawn.Token {
		t.Fatalf("past = %+v, %v; want the spawn kept there", past, err)
	}
}

// A computer that predates the forwarded Agents calls never says its thread
// is at rest, so a run there never ends here: its rows read as they did
// before runs were kept, by their requests' state, and a send of the
// spawner's to its worker there is still one row with the spawn's.
func TestAnOlderComputersWorkerIsOneRowAsBefore(t *testing.T) {
	t.Parallel()
	pair := newAgentPeerPair(t, serveOlderAgentsPeer)
	send := pair.sendThere(t, pair.child, "and one more thing")
	if row := pair.request(t, send.Token); row.RunBase == nil || *row.RunBase != (store.RunFigures{}) {
		t.Fatalf("the send there %+v; want a run from nothing, as nothing was reported", row)
	}
	listing := pair.listRowsUntil(t, func(rows ThreadAgentRows) bool {
		parent, _ := agentRowByToken(rows.Rows, pair.spawn.Token)
		return parent.NestedUnavailable != ""
	})
	if len(listing.Rows) != 1 || listing.Rows[0].Token != pair.spawn.Token || !listing.Rows[0].open() {
		t.Fatalf("rows %+v; want the spawn's row alone, open, for the worker", listing.Rows)
	}
	if spawn := pair.request(t, pair.spawn.Token); !spawn.RunOpen() {
		t.Fatalf("the spawn %+v; want its run open, never ended by an answer", spawn)
	}
}
