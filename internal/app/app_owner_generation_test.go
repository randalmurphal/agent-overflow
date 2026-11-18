package app

import (
	"agent-overflow/internal/store"
	"testing"
)

func TestResetDiscardsEarlierPanePlacement(t *testing.T) {
	var server observedOwnerPeer
	pair := newReachPairServedBy(t, func(t *testing.T, app *App) *pairedBackend { return observeOwnerPeer(t, app, &server) })
	spawn, worker := remoteRestedWorker(t, pair)
	if _, err := pair.dest.SetThreadPlacement([]string{worker}, store.PlacementStandalone); err != nil {
		t.Fatal(err)
	}
	if got := pair.source.readPeerAgentRows(t.Context(), spawn, 2); got.err != nil {
		t.Fatal(got.err)
	}

	if remoteDeleteWarning(t, pair) {
		t.Fatal("fixture did not cache standalone placement")
	}
	held, release := holdOwnerReply(t, &server.holdPane)
	done := make(chan threadAgentPeerAnswer, 1)
	go func() { done <- pair.source.readPeerAgentRows(t.Context(), spawn, 2) }()
	within(t, "old pane answer", held.arrived)
	if _, err := pair.dest.SetThreadPlacement([]string{worker}, store.PlacementOwned); err != nil {
		t.Fatal(err)
	}
	// Same-carrier reset, before recovery has read the changed link.
	pair.source.invalidateOwnerTrees(pair.computer, nil)
	if !remoteDeleteWarning(t, pair) {
		t.Fatal("reset did not start with a conservative delete warning")
	}
	release()
	_ = within(t, "pane commit", done)
	if !remoteDeleteWarning(t, pair) {
		t.Fatal("a pane reply that started before reset restored stale standalone placement and suppressed the delete warning")
	}
}

func remoteDeleteWarning(t *testing.T, pair *reachPair) bool {
	t.Helper()
	deps, err := pair.source.GetDeleteDependents([]string{pair.caller.ID})
	if err != nil {
		t.Fatal(err)
	}
	return deps.RemoteWorkers
}
