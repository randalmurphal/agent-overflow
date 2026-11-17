package app

import (
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"agent-overflow/internal/entityid"
	"agent-overflow/internal/store"
	"agent-overflow/internal/threadapp"
	"agent-overflow/internal/threadtools"
)

// An owner operation reaches its spawn's link whatever became of the
// receipt around it, and stays owed until every worker below it there is
// archived or deleted (docs/specs/standing-agents.md, Owned workers).

// lineageOnly moves the pair's spawn receipt there past the retention
// floor, its link standing: the receipt is settled, its owner reached it
// after the floor, and the sweep keeps it as lineage only.
func lineageOnly(t *testing.T, p *ownerOpPair) {
	t.Helper()
	if err := p.dest.settleThreadReceipt(p.token, store.ThreadReceiptOpenStates(), store.ThreadRequestSettlement{
		State: store.ThreadReceiptFinished, AnswerKind: store.ThreadAnswerFinal, Answer: []byte("done"),
	}); err != nil {
		t.Fatal(err)
	}
	floor := time.Now().Add(time.Minute).UnixMilli()
	if _, found, err := p.dest.store.ReachSpawnLink(p.token, p.device, p.caller.ID, floor+time.Hour.Milliseconds()); err != nil || !found {
		t.Fatalf("the owner's contact: %v, %v", found, err)
	}
	if _, released, err := p.dest.store.DeleteThreadRequestsBefore(floor, nil, nil); err != nil || len(released) != 0 {
		t.Fatalf("the sweep there: released %v, %v; want the link kept", released, err)
	}
	if _, found, err := p.dest.store.GetThreadRequestReceipt(p.token); err != nil || found {
		t.Fatalf("the receipt reads found %v, %v; want it lineage only", found, err)
	}
}

// A link past the retention floor, kept as lineage, still answers its
// owner's archive, delete, release and supersede.
func TestAnOwnerOpReachesALinkKeptAsLineage(t *testing.T) {
	t.Parallel()
	admit := func(t *testing.T, p *ownerOpPair, op string, seq int64) string {
		t.Helper()
		answer, err := p.dest.admitOwnerOp(t.Context(), p.device, p.caller.ID, threadOwnerOpArgs{Spawn: p.token, Seq: seq, Op: op})
		if err != nil {
			t.Fatal(err)
		}
		return answer.Outcome
	}
	t.Run("archive", func(t *testing.T) {
		t.Parallel()
		p := newOwnerOpPair(t, false)
		lineageOnly(t, p)
		if result, err := p.source.ArchiveThread(p.caller.ID); err != nil || len(result.WorkersNotArchived) != 0 {
			t.Fatalf("ArchiveThread = %+v, %v", result, err)
		}
		if !archivedThere(t, p, p.worker) {
			t.Fatal("the archive did not reach a worker whose receipt is lineage only")
		}
	})
	t.Run("delete", func(t *testing.T) {
		t.Parallel()
		p := newOwnerOpPair(t, false)
		lineageOnly(t, p)
		if result, err := p.source.DeleteThread(p.caller.ID); err != nil || len(result.WorkersNotDeleted) != 0 {
			t.Fatalf("DeleteThread = %+v, %v", result, err)
		}
		if _, err := p.dest.store.GetThread(p.worker); !threadGone(err) {
			t.Fatalf("the worker outlived its spawner's delete: %v", err)
		}
	})
	t.Run("release", func(t *testing.T) {
		t.Parallel()
		p := newOwnerOpPair(t, false)
		lineageOnly(t, p)
		if got := admit(t, p, store.OwnerOpRelease, 1); got != ownerOpApplied {
			t.Fatalf("the release = %s, want applied", got)
		}
		if row := p.there(t, p.worker); row.Placement != store.PlacementStandalone {
			t.Fatalf("the released worker is %s, want standalone", row.Placement)
		}
	})
	t.Run("supersede", func(t *testing.T) {
		t.Parallel()
		p := newOwnerOpPair(t, false)
		lineageOnly(t, p)
		if got := admit(t, p, store.OwnerOpSupersede, 1); got != ownerOpApplied {
			t.Fatalf("the supersede = %s, want applied", got)
		}
	})
}

// A retry that settles a spawn while its first attempt is still making
// the thread, and the owner's delete answered gone after it, leave nothing:
// the first attempt cannot name its thread on the settled receipt, and
// takes the thread back.
func TestASpawnSettledWhileItsThreadIsMadeKeepsNoThread(t *testing.T) {
	t.Parallel()
	p := newOwnerOpPair(t, false)
	captured := make(chan ThreadPeerCall, 1)
	capture := func(call ThreadPeerCall) {
		select {
		case captured <- call:
		default:
		}
	}
	p.peer.spawning.Store(&capture)
	held, release := make(chan struct{}), make(chan struct{})
	var once, freed sync.Once
	free := func() { freed.Do(func() { close(release) }) }
	t.Cleanup(free)
	p.dest.spawnNamingForTest = func(id string) {
		if id == p.caller.ID {
			once.Do(func() {
				close(held)
				<-release
			})
		}
	}
	spawned := make(chan error, 1)
	go func() {
		_, err := p.adapter().Spawn(t.Context(), p.callerIdentity(),
			threadtools.SpawnCall{Prompt: "more work", ComputerID: p.computer, ProjectID: p.project})
		spawned <- err
	}()
	within(t, "the thread being made there", held)
	call := <-captured
	var reply ThreadPeerReply
	if err := p.source.backends.CallThreadPeer(t.Context(), p.computer, "ThreadToolCall", &reply, call); err == nil {
		t.Fatal("the retry of a spawn with no thread yet was accepted")
	}
	if receipt := p.receipt(t, call.Token); receipt.State != store.ThreadReceiptInterrupted || receipt.TargetThreadID != "" {
		t.Fatalf("the retried receipt = %+v, want interrupted with no thread", receipt)
	}
	answer, err := p.source.callOwnerOp(t.Context(), store.OwnerOp{
		Token: call.Token, Seq: 1, Op: store.OwnerOpDelete, ComputerID: p.computer, SourceThreadID: p.caller.ID,
	})
	if err != nil || answer.Outcome != ownerOpGone {
		t.Fatalf("the delete = %+v, %v; want gone", answer, err)
	}
	free()
	if err := within(t, "the first attempt", spawned); err == nil {
		t.Fatal("the first attempt's spawn was accepted though its receipt was settled")
	}
	rows, err := p.dest.store.ListThreads()
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].ID != p.worker {
		t.Fatalf("the threads there = %d, want only the first worker: the settled spawn kept its thread", len(rows))
	}
	if receipt := p.receipt(t, call.Token); receipt.TargetThreadID != "" {
		t.Fatalf("the settled receipt names %s", receipt.TargetThreadID)
	}
}

// moveOutThere starts moving threadID off the pair's destination and
// returns the cancel of that move, which leaves the thread there.
func moveOutThere(t *testing.T, p *ownerOpPair, threadID string) func() {
	t.Helper()
	transfer, err := p.dest.store.CreateThreadTransfer(store.ThreadTransfer{
		ID: entityid.New(), ThreadID: threadID, PeerBackendID: entityid.New(), Kind: "move", Direction: "outgoing",
		ActivationHash: strings.Repeat("a", 64), PrivateState: []byte(`{}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	return func() {
		t.Helper()
		if _, err := p.dest.store.AdvanceThreadTransfer(transfer.ID, "canceled", ""); err != nil {
			t.Fatal(err)
		}
	}
}

// An archive there that a worker below the spawn's worker refuses stays
// owed here, names that worker pending on its computer, and archives it
// once it can: the spawner's ledger is the one retry.
func TestARemoteArchiveIsOwedUntilEveryWorkerBelowIsArchived(t *testing.T) {
	t.Parallel()
	p := newOwnerOpPair(t, false)
	child := p.spawnBelowThere(t, p.worker)
	cancel := moveOutThere(t, p, child)
	result, err := p.source.ArchiveThread(p.caller.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.WorkersNotArchived) != 1 {
		t.Fatalf("WorkersNotArchived = %+v, want the worker below", result.WorkersNotArchived)
	}
	if failure := result.WorkersNotArchived[0]; failure.ThreadID != child || !failure.Pending || failure.Computer == "" {
		t.Fatalf("the worker below is reported %+v, want it pending on its computer", failure)
	}
	if !archivedThere(t, p, p.worker) || archivedThere(t, p, child) {
		t.Fatal("want the worker archived there and the one below it not")
	}
	owed := p.owed(t)
	if len(owed) != 1 || owed[0].Op != store.OwnerOpArchive {
		t.Fatalf("owed = %+v, want the archive, naming what it did not reach", owed)
	}
	// Sent again while the worker below still refuses: still owed.
	p.drain(t)
	if owed := p.owed(t); len(owed) != 1 {
		t.Fatalf("owed after a refused replay = %+v", owed)
	}
	cancel()
	p.drain(t)
	if !archivedThere(t, p, child) {
		t.Fatal("the replay did not archive the worker below")
	}
	if owed := p.owed(t); len(owed) != 0 {
		t.Fatalf("still owed %+v", owed)
	}
}

// A delete there whose worker below cannot be destroyed stays owed here,
// names that worker pending, and the delete sent again finishes it.
func TestARemoteDeleteIsOwedUntilEveryWorkerBelowIsDeleted(t *testing.T) {
	t.Parallel()
	p := newOwnerOpPair(t, false)
	child := p.spawnBelowThere(t, p.worker)
	var failing atomic.Bool
	failing.Store(true)
	p.dest.deletePortsForTest = func(ports *threadapp.DeletePorts) {
		inner := ports.StopRemoteWork
		ports.StopRemoteWork = func(id string) error {
			if id == child && failing.Load() {
				return errors.New("a command there did not stop")
			}
			if inner != nil {
				return inner(id)
			}
			return nil
		}
	}
	result, err := p.source.DeleteThread(p.caller.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.WorkersNotDeleted) != 1 {
		t.Fatalf("WorkersNotDeleted = %+v, want the worker below", result.WorkersNotDeleted)
	}
	if failure := result.WorkersNotDeleted[0]; failure.ThreadID != child || !failure.Pending || failure.Computer == "" {
		t.Fatalf("the worker below is reported %+v, want it pending on its computer", failure)
	}
	if owed := p.owed(t); len(owed) != 1 || owed[0].Op != store.OwnerOpDelete {
		t.Fatalf("owed = %+v, want the delete", owed)
	}
	failing.Store(false)
	p.drain(t)
	for _, id := range []string{p.worker, child} {
		if _, err := p.dest.store.GetThread(id); !threadGone(err) {
			t.Errorf("%s outlived the delete sent again: %v", id, err)
		}
	}
	if owed := p.owed(t); len(owed) != 0 {
		t.Fatalf("still owed %+v", owed)
	}
}
