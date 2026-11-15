package app

// A spawn's receipt on the computer it reached is that thread's tie to the
// computer that made it (docs/specs/standing-agents.md, "Owned workers"):
// past the retention floor it is kept as its lineage while the thread
// exists, its answers expiring as every receipt's do.

import (
	"testing"
	"time"

	"agent-overflow/internal/buildvariant/remotetest"
	"agent-overflow/internal/store"
	"agent-overflow/internal/threadtools"
)

// remoteLoopWorker spawns a thread on the pair's destination that arms a
// loop and ends its turn, and waits until the source records it open there.
func remoteLoopWorker(t *testing.T, pair *reachPair) (*agentPeerPair, string) {
	t.Helper()
	installMockClaudeReplies(t, pair.dest, "watching the deploy")
	ack := pair.spawnThere(t, "watch the deploy", nil)
	child := pair.receipt(t, ack.Token).TargetThreadID
	waitUntil(t, 10*time.Second, func() bool { return pair.receipt(t, ack.Token).SettledAt != 0 })
	destID, _ := pair.dest.backendIdentity()
	there := threadtools.Caller{ThreadID: child, ComputerID: destID, ComputerName: "destination"}
	if _, err := (threadToolsApp{app: pair.dest}).Loop(t.Context(), there, threadtools.LoopCall{Label: "deploy check", Note: "check", EverySeconds: 3600}); err != nil {
		t.Fatalf("Loop there: %v", err)
	}
	pair.poll(t)
	agentPair := &agentPeerPair{reachPair: pair, spawn: pair.request(t, ack.Token)}
	agentPair.askAgainUntil(t, func(row store.ThreadRequest) bool { return threadRequestSettled(row) && row.NestedOpen })
	agentPair.spawn = pair.request(t, ack.Token)
	return agentPair, child
}

// A remote worker with nothing open below it, only an armed loop or a live
// turn, keeps its spawn's receipt there past the retention floor: the
// receipt is kept as its lineage with its answer gone and its token
// unknown, so the spawner's listing and Stop still reach the worker. The
// store's own tests hold the lineage's shape and its delete with the
// thread.
func TestRetentionKeepsTheReceiptOfARemoteWorkerThatIsNotAtRest(t *testing.T) {
	t.Parallel()
	remotetest.Require(t)
	for name, start := range map[string]func(*testing.T, *reachPair) (*agentPeerPair, string){
		"loop": remoteLoopWorker, "turn": remoteRunningWorker,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			pair := newReachPair(t)
			agentPair, child := start(t, pair)
			floor := time.Now().Add(time.Minute).UnixMilli()
			for _, app := range []*App{pair.source, pair.dest} {
				if _, err := app.store.DeleteThreadRequestsBefore(floor); err != nil {
					t.Fatalf("sweep: %v", err)
				}
			}
			if _, found, err := pair.dest.store.GetThreadRequestReceipt(agentPair.spawn.Token); err != nil || found {
				t.Fatalf("the lineage's token reads found %v, %v; want unknown", found, err)
			}
			if !pair.request(t, agentPair.spawn.Token).NestedOpen {
				t.Fatal("the sweep here dropped the spawn's open work")
			}
			listing, err := pair.source.peerThreadAgentRows(t.Context(), pair.computer, agentPair.spawn.MadeIn(), child, threadAgentRowDepth-1)
			if err != nil || listing.Worker == "" {
				t.Fatalf("the forwarded listing after the sweeps = %+v, %v; want the worker's state", listing, err)
			}
			stop, err := pair.source.StopThreadAgentRow(t.Context(), pair.caller.ID, agentPair.spawn.Token)
			if err != nil || len(stop.Failures) != 0 {
				t.Fatalf("the Stop after the sweeps = %+v, %v; want it to reach the worker", stop, err)
			}
			waitUntil(t, 10*time.Second, func() bool {
				rest, err := pair.dest.workerUnrest(child)
				return err == nil && rest.unrest == ""
			})

		})
	}
}
