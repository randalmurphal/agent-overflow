package app

import (
	"testing"
	"time"

	"agent-overflow/internal/buildvariant/remotetest"
	"agent-overflow/internal/store"
)

// A worker on a paired computer with a prompt routed to its spawner here,
// when the link ends or the spawner's archive or delete reaches it: the
// prompt goes to the person there only while the worker is still theirs to
// answer.

// promptOwnerOpPair is a remotePromptPair whose destination answers
// through an ownerOpPeer, so a test can hold or drop what reaches it.
func promptOwnerOpPair(t *testing.T, lease time.Duration) (*remotePromptPair, *ownerOpPeer) {
	t.Helper()
	remotetest.Require(t)
	var peer *ownerOpPeer
	pair := newReachPairServedBy(t, func(t *testing.T, app *App) *pairedBackend {
		peer = &ownerOpPeer{App: app}
		return serveAppAs(t, app, peer)
	})
	return newRemotePromptPairOn(t, pair, lease, remoteApproveLine), peer
}

// givenToThePerson waits until the worker's approval there is the
// person's, announced with no route, which is what notifies them.
func (p *remotePromptPair) givenToThePerson(t *testing.T) {
	t.Helper()
	waitUntil(t, 10*time.Second, func() bool {
		route, open := p.routeThere("req-allow")
		return open && route == nil && p.announcedToPerson("req-allow")
	})
}

// Revoking the last session of the spawner's device on the worker's
// computer ends the link, and the worker's prompt routed to the spawner is
// the person's there at once, not when the lease runs out.
func TestRevokingTheSpawnersLastSessionThereGivesThePersonTheWorkersPrompt(t *testing.T) {
	t.Parallel()
	p := newRemotePromptPair(t, remoteApproveLine)
	p.openThere(t, 1)
	device := p.receipt(t, p.spawn.Token).OwnerDeviceID
	sessions, err := p.dest.store.ListSessionsForDevice(device)
	if err != nil {
		t.Fatal(err)
	}
	revoked := 0
	for _, session := range sessions {
		if session.RevokedAt != 0 {
			continue
		}
		if err := p.dest.RevokeAccessSession(session.ID); err != nil {
			t.Fatal(err)
		}
		revoked++
	}
	if revoked == 0 {
		t.Fatal("the spawner's device has no session there to revoke")
	}
	if receipt := p.receipt(t, p.spawn.Token); receipt.SpawnLink {
		t.Fatal("the link outlived the device's last session")
	}
	p.givenToThePerson(t)
}

// A release from the spawner's computer ends the link, and the worker's
// prompt routed to the spawner is the person's there at once.
func TestAReleaseGivesThePersonTheWorkersPrompt(t *testing.T) {
	t.Parallel()
	p, _ := promptOwnerOpPair(t, 0)
	p.openThere(t, 1)
	answer, err := p.source.callOwnerOp(t.Context(), store.OwnerOp{
		Token: p.spawn.Token, ComputerID: p.computer, SourceThreadID: p.spawn.MadeIn(), Op: store.OwnerOpRelease, Seq: 1,
	})
	if err != nil || answer.Outcome != ownerOpApplied {
		t.Fatalf("the release = %+v, %v", answer, err)
	}
	p.givenToThePerson(t)
}

// promptEnded reports whether the worker's prompt there is no longer open.
func (p *remotePromptPair) promptEnded() bool {
	_, open := p.routeThere("req-allow")
	return !open
}

// The spawner's archive or delete reaches its worker there while the
// worker's prompt is routed to it: the prompt ends with the worker, the
// spawner's wake for it is gone, and the person there is never handed a
// prompt of a worker that is gone: the spawner's computer gives nothing
// back for a worker its archive or delete is on its way to end.
func TestAnOwnerOpEndsAPromptRoutedToTheSpawner(t *testing.T) {
	t.Parallel()
	for name, end := range map[string]func(*testing.T, *remotePromptPair){
		"archive": func(t *testing.T, p *remotePromptPair) {
			result, err := p.source.ArchiveThread(p.caller.ID)
			if err != nil || len(result.WorkersNotArchived) != 0 {
				t.Fatalf("archive: %+v, %v", result, err)
			}
		},
		"delete": func(t *testing.T, p *remotePromptPair) {
			result, err := p.source.DeleteThread(p.caller.ID)
			if err != nil || len(result.WorkersNotDeleted) != 0 {
				t.Fatalf("delete: %+v, %v", result, err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			p, peer := promptOwnerOpPair(t, 0)
			p.openThere(t, 1)
			p.readOwnerState(t, p.source)
			p.wakesFor(t, p.caller.ID, 1)
			end(t, p)
			waitUntil(t, 15*time.Second, p.promptEnded)
			p.drain(t)
			if p.announcedToPerson("req-allow") {
				t.Fatalf("the person there was handed the prompt of a worker its spawner ended (%d returns)", peer.returns.Load())
			}
			// A deleted spawner's queue went with it.
			if _, err := p.source.store.GetThread(p.caller.ID); err == nil {
				if wakes := remoteWakes(t, p.source, p.caller.ID); len(wakes) != 0 {
					t.Fatalf("the spawner still holds wakes %+v", wakes)
				}
			}
			if held := p.source.heldRemotePrompts(p.spawn); len(held) != 0 {
				t.Fatalf("the spawner's computer still holds %q", held)
			}
			if held := p.dest.workerPrompts.heldFor(p.spawn.Token, p.worker); len(held) != 0 {
				t.Fatalf("the worker there still holds %+v for its spawner", held)
			}
			if returned := peer.returns.Load(); returned != 0 {
				t.Fatalf("the spawner's computer gave back %d prompts of a worker it was ending", returned)
			}
		})
	}
}

// The spawner's computer cannot reach the worker's: its archive is owed
// and its prompt's hand-back lost, so the prompt is the person's there
// once its lease runs out, and stays open for them until the archive
// lands.
func TestAPromptOfAWorkerWhoseSpawnerIsArchivedOutOfReachGoesToThePerson(t *testing.T) {
	t.Parallel()
	p, peer := promptOwnerOpPair(t, 0)
	p.openThere(t, 1)
	p.readOwnerState(t, p.source)
	p.wakesFor(t, p.caller.ID, 1)
	held := p.dest.workerPrompts.heldFor(p.spawn.Token, p.worker)
	if len(held) != 1 {
		t.Fatalf("the worker holds %d prompts for its spawner, want one", len(held))
	}
	peer.unreachable.Store(true)
	result, err := p.source.ArchiveThread(p.caller.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.WorkersNotArchived) != 1 || !result.WorkersNotArchived[0].Pending {
		t.Fatalf("WorkersNotArchived = %+v, want the worker pending there", result.WorkersNotArchived)
	}
	// Nothing reaches that computer now; its lease is what hands it on.
	(&linkedWorkers{app: p.dest}).shortenLease(time.Second, held[0].token)
	p.givenToThePerson(t)
	// The computer answers again: the archive lands, and the prompt ends
	// with the worker.
	peer.unreachable.Store(false)
	deliverAllOwed(t, p.source)
	if row, err := p.dest.store.GetThread(p.worker); err != nil || !row.Archived {
		t.Fatalf("the worker there is %+v, %v once the archive could land", row, err)
	}
	waitUntil(t, 15*time.Second, p.promptEnded)
}

// Reopening the spawner after its archive reached the worker there
// supersedes that archive, and leaves where the worker's new prompts route
// as it was: to the spawner here.
func TestASupersedeLeavesTheWorkersPromptsRoutedToItsSpawner(t *testing.T) {
	t.Parallel()
	p, _ := promptOwnerOpPair(t, 0)
	p.openThere(t, 1)
	if _, err := p.source.ArchiveThread(p.caller.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := p.source.UnarchiveThread(p.caller.ID); err != nil {
		t.Fatal(err)
	}
	deliverAllOwed(t, p.source)
	if route := p.dest.routeWorkerPrompt(p.worker); route == nil || route.ThreadID != p.caller.ID || route.ComputerID != p.sourceID {
		t.Fatalf("the worker's prompts route to %+v after the supersede, want the spawner here", route)
	}
	if receipt := p.receipt(t, p.spawn.Token); !receipt.SpawnLink {
		t.Fatal("the supersede ended the link")
	}
}

// deliverAllOwed runs one delivery pass of what app owes, everything due,
// joins it, and requires that it all landed.
func deliverAllOwed(t *testing.T, app *App) {
	t.Helper()
	app.deliverOwedOwnerOps(time.Now().Add(time.Hour))
	app.threadRequestsWG.Wait()
	if owed, err := app.store.DueOwnerOps(1<<62, 100); err != nil || len(owed) != 0 {
		t.Fatalf("still owed %+v, %v", owed, err)
	}
}
