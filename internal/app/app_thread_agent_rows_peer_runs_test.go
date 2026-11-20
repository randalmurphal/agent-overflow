package app

// A worker on a paired computer follows the rule a local one does
// (docs/specs/standing-agents.md, "At rest"): once its spawner's run on it
// ended there, a run the person starts there puts its prompts to the person
// there and holds its spawner here from rest no longer.

import (
	"context"
	"testing"
	"time"

	"agent-overflow/internal/store"
)

// The remote worker rested and the person there continues it. There its
// prompts are the person's, and its answer to the spawner's computer says
// what its spawner's run did when it ended. Here, the spawn's run ends with
// those figures even when this computer had not heard of the rest before
// the person started, and the spawner reads at rest while the worker runs.
// A send of the spawner's starts a run there that routes the worker's
// prompts to the spawner again.
func TestARemoteWorkersLaterRunIsThePersonsThere(t *testing.T) {
	t.Parallel()
	pair := newReachPair(t)
	spawn, child := remoteRestedWorker(t, pair)
	receipt := pair.receipt(t, spawn.Token)
	ended, err := pair.dest.store.ReceiptRunOf(receipt.OwnerDeviceID, pair.caller.ID, child)
	if err != nil || !ended.Tracked || ended.Open || ended.End == nil {
		t.Fatalf("the spawn's run there %+v, %v; want it ended with the worker's figures", ended, err)
	}
	if err := pair.dest.SendMessage(context.Background(), child, "now also update the changelog", nil); err != nil {
		t.Fatalf("SendMessage there: %v", err)
	}
	waitUntil(t, 10*time.Second, func() bool {
		return threadToolsLiveState(pair.dest.triage.LiveStateSnapshotForThread(child)).ActiveTurn
	})
	if route := pair.dest.routeWorkerPrompt(child); route == nil || route.ThreadID != pair.caller.ID || route.ComputerID == "" {
		t.Fatalf("a prompt of the owned worker's nudged run there routes to %+v; want its spawner here", route)
	}

	// This computer had not heard of the rest when the person started.
	execOnStore(t, pair.dbPath, `UPDATE thread_requests SET run_tool_calls = NULL, run_tokens = NULL, run_turn_ms = NULL WHERE token = '`+spawn.Token+`'`)
	agentPair := &agentPeerPair{reachPair: pair, spawn: spawn, child: child}
	agentPair.askAgainUntil(t, func(row store.ThreadRequest) bool { return row.NestedOpen && row.RunEnded() })
	row := pair.request(t, spawn.Token)
	if want := (store.RunFigures{ToolCalls: ended.End.ToolCalls, Tokens: ended.End.Tokens, TurnMs: ended.End.TurnMs}); *row.RunTotal != want {
		t.Fatalf("the spawn's run %+v; want what its worker had done when it ended there, %+v", *row.RunTotal, want)
	}
	waitUntil(t, 10*time.Second, func() bool {
		unrest, err := pair.source.workerUnrest(pair.caller.ID)
		return err == nil && unrest.unrest == ""
	})
	if rest, err := pair.source.threadRest(pair.caller.ID); err != nil || rest.unrest != "" {
		t.Fatalf("the spawner reads %+v, %v; want it at rest while the person runs its worker there", rest, err)
	}
	read, err := pair.source.readRest([]string{pair.caller.ID})
	defer read.done()
	if err != nil || !read.rests(pair.caller.ID) {
		t.Fatalf("the decision reads the spawner not at rest (%v); want it at rest", err)
	}

	pair.sendThere(t, child, "and the release notes")
	route := pair.dest.routeWorkerPrompt(child)
	if route == nil || route.ThreadID != pair.caller.ID || route.ComputerID == "" {
		t.Fatalf("a prompt in the spawner's run there routes to %+v; want the spawner", route)
	}
}
