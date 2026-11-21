package app

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"agent-overflow/internal/entityid"
	"agent-overflow/internal/store"
	"agent-overflow/internal/threadtools"
)

// Archiving and deleting a thread reach the workers it spawned on paired
// computers: the spawner's computer owes each link the operation, in the
// write that decided it, and delivers it until the other computer answers;
// that computer admits it through its own archive and delete
// (docs/specs/standing-agents.md, Owned workers).

// ownerOpPeer is a destination whose answers to owner operations a test
// controls: asleep refuses them as an unreachable computer does, and hold,
// when set, runs before an archive reaches the app and after it returns.
type ownerOpPeer struct {
	*App
	asleep atomic.Bool
	hold   atomic.Pointer[func(op string) func()]
	// unreachable fails every call, as a computer that cannot be reached.
	unreachable atomic.Bool
	// returns counts the prompt hand-backs that reached this computer.
	returns atomic.Int32
	// spawning, when set, runs before a forwarded thread_spawn reaches the
	// app, with the call.
	spawning atomic.Pointer[func(ThreadPeerCall)]
	// replying, when set, runs before an agent_prompt_reply reaches the
	// app; an error it returns is the call's answer.
	replying atomic.Pointer[func() error]
	// answering, when set, is this computer's answer to every owner
	// operation, which then does not reach the app.
	answering atomic.Pointer[threadOwnerOpAnswer]
	// ownerOpCalls counts the owner operations sent to this computer.
	ownerOpCalls atomic.Int32
}

var errPeerUnreachable = errors.New("the computer cannot be reached")

func (p *ownerOpPeer) ThreadToolQuery(ctx context.Context, call ThreadPeerCall) (ThreadPeerReply, error) {
	if p.unreachable.Load() {
		return ThreadPeerReply{}, errPeerUnreachable
	}
	return p.App.ThreadToolQuery(ctx, call)
}

func (p *ownerOpPeer) ThreadToolRequestStatus(ctx context.Context, poll ThreadPeerPoll) (ThreadPeerPollReply, error) {
	if p.unreachable.Load() {
		return ThreadPeerPollReply{}, errPeerUnreachable
	}
	return p.App.ThreadToolRequestStatus(ctx, poll)
}

func (p *ownerOpPeer) ThreadToolCall(ctx context.Context, call ThreadPeerCall) (ThreadPeerReply, error) {
	if p.unreachable.Load() {
		return ThreadPeerReply{}, errPeerUnreachable
	}
	switch call.Tool {
	case threadAgentPeerPromptReturn:
		p.returns.Add(1)
	case "thread_spawn":
		if spawning := p.spawning.Load(); spawning != nil {
			(*spawning)(call)
		}
	case threadAgentPeerPromptReply:
		if replying := p.replying.Load(); replying != nil {
			if err := (*replying)(); err != nil {
				return ThreadPeerReply{}, err
			}
		}
	}
	if call.Tool != threadOwnerOpTool {
		return p.App.ThreadToolCall(ctx, call)
	}
	p.ownerOpCalls.Add(1)
	if p.asleep.Load() {
		return ThreadPeerReply{}, errors.New("the computer is asleep")
	}
	if answer := p.answering.Load(); answer != nil {
		encoded, err := json.Marshal(answer)
		return ThreadPeerReply{Result: encoded}, err
	}
	if hold := p.hold.Load(); hold != nil {
		var args threadOwnerOpArgs
		if err := json.Unmarshal(call.Args, &args); err != nil {
			return ThreadPeerReply{}, err
		}
		defer (*hold)(args.Op)()
	}
	return p.App.ThreadToolCall(ctx, call)
}

// ownerOpPair is a source thread with an owned spawn on a paired computer.
type ownerOpPair struct {
	*reachPair
	peer *ownerOpPeer
	// token is the spawn's; worker the thread it made there, and device
	// the source's device there.
	token, worker, device string
}

func newOwnerOpPair(t *testing.T, older bool) *ownerOpPair {
	t.Helper()
	var peer *ownerOpPeer
	pair := newReachPairServedBy(t, func(t *testing.T, app *App) *pairedBackend {
		peer = &ownerOpPeer{App: app}
		// An older computer answers as a build before owner operations
		// does, as a harness boot stands one up (IsolationConfig.OmitOwnerOps).
		app.ownerOpsOmitted = older
		return serveAppAs(t, app, peer)
	})
	installMockClaudeTurns(t, pair.dest, [][]string{{mockClaudeInitLine}})
	ack := pair.spawnThere(t, "work over there", nil)
	receipt := pair.receipt(t, ack.Token)
	if receipt.TargetThreadID == "" || !receipt.SpawnLink {
		t.Fatalf("the spawn there is %+v, want a linked thread", receipt)
	}
	return &ownerOpPair{reachPair: pair, peer: peer, token: ack.Token, worker: receipt.TargetThreadID, device: receipt.OwnerDeviceID}
}

// spawnBelowThere has thread there spawn a worker of its own there.
func (p *ownerOpPair) spawnBelowThere(t *testing.T, threadID string) string {
	t.Helper()
	destID, _ := p.dest.backendIdentity()
	ack, err := threadToolsApp{app: p.dest}.Spawn(t.Context(),
		threadtools.Caller{ThreadID: threadID, ComputerID: destID, ComputerName: "destination"},
		threadtools.SpawnCall{Prompt: "a part of it", Title: "Grandchild"})
	if err != nil {
		t.Fatalf("a spawn there: %v", err)
	}
	var child string
	waitUntil(t, 10*time.Second, func() bool {
		row, found, err := p.dest.store.GetThreadRequestReceipt(ack.Token)
		child = row.TargetThreadID
		return err == nil && found && child != ""
	})
	return child
}

// drain runs one delivery pass of what the source owes, everything due,
// and joins it.
func (p *ownerOpPair) drain(t *testing.T) {
	t.Helper()
	p.source.deliverOwedOwnerOps(time.Now().Add(time.Hour))
	p.source.threadRequestsWG.Wait()
}

// owed is everything the source still owes.
func (p *ownerOpPair) owed(t *testing.T) []store.OwnerOp {
	t.Helper()
	ops, err := p.source.store.DueOwnerOps(1<<62, 100)
	if err != nil {
		t.Fatal(err)
	}
	return ops
}

// there reads a thread on the destination; gone reports it deleted.
func (p *ownerOpPair) there(t *testing.T, threadID string) store.Thread {
	t.Helper()
	row, err := p.dest.store.GetThread(threadID)
	if err != nil {
		t.Fatalf("read %s there: %v", threadID, err)
	}
	return row
}

func archivedThere(t *testing.T, p *ownerOpPair, threadID string) bool {
	t.Helper()
	return p.there(t, threadID).Archived
}

// Archiving a spawner archives the worker it owns on another computer and
// that worker's own workers there, before the archive returns, and owes
// nothing once that computer answered. A thread the spawner gave a sidebar
// row there is left alone.
func TestArchivingASpawnerArchivesItsWorkersOnAnotherComputer(t *testing.T) {
	t.Parallel()
	p := newOwnerOpPair(t, false)
	child := p.spawnBelowThere(t, p.worker)
	standalone := p.receipt(t, p.spawnThere(t, "for the user", map[string]any{"placement": threadtools.PlacementStandalone}).Token).TargetThreadID

	result, err := p.source.ArchiveThread(p.caller.ID)
	if err != nil || len(result.WorkersNotArchived) != 0 {
		t.Fatalf("ArchiveThread = %+v, %v; want a complete archive", result, err)
	}
	for name, id := range map[string]string{"the worker": p.worker, "its own worker": child} {
		if !archivedThere(t, p, id) {
			t.Errorf("%s there is not archived", name)
		}
	}
	if archivedThere(t, p, standalone) {
		t.Error("a thread with a sidebar row there was archived")
	}
	if owed := p.owed(t); len(owed) != 0 {
		t.Fatalf("the archive still owes %+v", owed)
	}
}

// An archive the other computer does not answer stands here, is reported
// pending, naming the computer, and is delivered once it answers.
func TestAnUnansweredArchiveIsPendingUntilTheComputerAnswers(t *testing.T) {
	t.Parallel()
	p := newOwnerOpPair(t, false)
	p.peer.asleep.Store(true)
	result, err := p.source.ArchiveThread(p.caller.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.WorkersNotArchived) != 1 {
		t.Fatalf("WorkersNotArchived = %+v, want the worker there", result.WorkersNotArchived)
	}
	failure := result.WorkersNotArchived[0]
	if !failure.Pending || failure.ThreadID != p.worker || failure.Computer == "" || failure.ComputerID != p.computer || failure.Error == "" {
		t.Fatalf("the worker there is reported %+v, want it pending on its computer %s", failure, p.computer)
	}
	if row, _ := p.source.store.GetThread(p.caller.ID); !row.Archived {
		t.Fatal("the spawner's archive did not stand")
	}
	if archivedThere(t, p, p.worker) {
		t.Fatal("the worker there was archived by a computer that did not answer")
	}
	// The retry ladder: 5 s after the first attempt, doubling.
	retriedWithin := func(attempts int, wait time.Duration, before time.Time) {
		t.Helper()
		owed := p.owed(t)
		if len(owed) != 1 || owed[0].Op != store.OwnerOpArchive || !owed[0].Sent || owed[0].Attempts != attempts {
			t.Fatalf("owed = %+v, want the archive, tried %d times", owed, attempts)
		}
		next := time.UnixMilli(owed[0].NextAt)
		// NextAt is in whole milliseconds.
		if next.Before(before.Truncate(time.Millisecond).Add(wait)) || next.After(time.Now().Add(wait)) {
			t.Fatalf("attempt %d is tried again at %v, want %v after it", attempts, next, wait)
		}
	}
	retriedWithin(1, 5*time.Second, time.Now().Add(-p.source.ownerOpReach()))
	before := time.Now()
	p.drain(t)
	retriedWithin(2, 10*time.Second, before)
	p.peer.asleep.Store(false)
	p.drain(t)
	if !archivedThere(t, p, p.worker) {
		t.Fatal("the worker there was not archived once its computer answered")
	}
	if owed := p.owed(t); len(owed) != 0 {
		t.Fatalf("still owed %+v", owed)
	}
}

// A worker below the worker there that its computer could not archive yet
// is reported pending on that computer, by its id, so a report groups it
// with the computer's other workers.
func TestAWorkerBelowThatIsNotArchivedYetNamesItsComputer(t *testing.T) {
	t.Parallel()
	p := newOwnerOpPair(t, false)
	p.peer.answering.Store(&threadOwnerOpAnswer{Outcome: ownerOpUnfinished,
		Failures: []WorkerFailure{{ThreadID: "below", Title: "Below", Error: "it is busy"}}})
	result, err := p.source.ArchiveThread(p.caller.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.WorkersNotArchived) != 1 {
		t.Fatalf("WorkersNotArchived = %+v, want the worker below", result.WorkersNotArchived)
	}
	failure := result.WorkersNotArchived[0]
	if !failure.Pending || failure.ThreadID != "below" || failure.Computer == "" || failure.ComputerID != p.computer || !strings.Contains(failure.Error, "it is busy") {
		t.Fatalf("the worker below is reported %+v, want it pending on its computer %s", failure, p.computer)
	}
}

// thread_update's archive names a worker on another computer that has not
// taken it yet, pending on that computer, and the request sweep delivers
// it once that computer answers.
func TestThreadUpdateNamesAPendingWorkerAndTheSweepDeliversIt(t *testing.T) {
	t.Parallel()
	p := newOwnerOpPair(t, false)
	other, err := createTestThread(t, p.source, p.caller.Provider, t.TempDir(), p.caller.Model, p.caller.Mode)
	if err != nil {
		t.Fatal(err)
	}
	sourceID, _ := p.source.backendIdentity()
	p.peer.asleep.Store(true)
	answer, err := p.source.threadToolsServer().Call(t.Context(),
		threadtools.Caller{ThreadID: other.ID, ComputerID: sourceID, ComputerName: "source"},
		"thread_update", json.RawMessage(`{"thread_ids":["`+p.caller.ID+`"],"archived":true}`))
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(answer)
	if err != nil {
		t.Fatal(err)
	}
	var result struct {
		Results []threadtools.ThreadUpdateResult `json:"results"`
		Note    string                           `json:"note"`
	}
	if err := json.Unmarshal(encoded, &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Results) != 1 || !result.Results[0].Updated || len(result.Results[0].WorkersNotArchived) != 1 {
		t.Fatalf("thread_update = %s", encoded)
	}
	if worker := result.Results[0].WorkersNotArchived[0]; !worker.Pending || worker.Computer == "" || worker.ThreadID != p.worker {
		t.Fatalf("the worker there is reported %+v, want it pending on its computer", worker)
	}
	if !strings.Contains(result.Note, "sent again until") {
		t.Fatalf("note = %q", result.Note)
	}
	p.peer.asleep.Store(false)
	// The running sweep, not a test's pass, delivers it.
	p.source.startThreadRequestSweeps()
	waitUntil(t, 30*time.Second, func() bool { return archivedThere(t, p, p.worker) })
}

// The sweep wakes for an owed operation coming due, with nothing else to
// do.
func TestTheSweepWakesForAnOwedOperation(t *testing.T) {
	t.Parallel()
	f := newThreadToolsFixture(t)
	owner := f.thread(t, "owner")
	if err := f.app.store.InsertThreadRequest(store.ThreadRequest{Token: newThreadRequestToken(), CallerThreadID: owner.ID, Kind: store.ThreadRequestSpawn,
		TargetComputerID: "computer-elsewhere", State: store.ThreadRequestFinished, NextCheck: time.Now().Add(time.Hour).UnixMilli()}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.app.store.ArchiveThread(owner.ID); err != nil {
		t.Fatal(err)
	}
	if delay := f.app.threadRequestSweepDelay(time.Now()); delay < time.Minute {
		t.Fatalf("the sweep sleeps %v with nothing owed; the test needs nothing else due", delay)
	}
	owed, err := f.app.store.OweRemoteArchives(owner.ID, nil)
	if err != nil || len(owed) != 1 {
		t.Fatalf("owed %+v, %v", owed, err)
	}
	due := time.Now().Add(3 * time.Second)
	if _, err := f.app.store.RescheduleOwnerOp(owed[0].Token, owed[0].Seq, due.UnixMilli()); err != nil {
		t.Fatal(err)
	}
	if delay := f.app.threadRequestSweepDelay(time.Now()); delay > 3*time.Second {
		t.Fatalf("the sweep sleeps %v past an operation due in 3s", delay)
	}
}

// Reopening the spawner before its archive reached the other computer
// keeps the worker there open.
func TestReopeningASpawnerBeforeItsArchiveLandsKeepsTheWorkerOpen(t *testing.T) {
	t.Parallel()
	p := newOwnerOpPair(t, false)
	p.peer.asleep.Store(true)
	if _, err := p.source.ArchiveThread(p.caller.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := p.source.UnarchiveThread(p.caller.ID); err != nil {
		t.Fatal(err)
	}
	p.peer.asleep.Store(false)
	p.drain(t)
	if archivedThere(t, p, p.worker) {
		t.Fatal("the worker there was archived after its spawner was reopened")
	}
	if owed := p.owed(t); len(owed) != 0 {
		t.Fatalf("still owed %+v", owed)
	}
}

// An archive sent before the spawner was reopened, and admitted there only
// after the reopen's supersede, changes nothing.
func TestASupersedeOvertakesAnArchiveHeldOnTheWay(t *testing.T) {
	t.Parallel()
	p := newOwnerOpPair(t, false)
	p.peer.asleep.Store(true)
	if _, err := p.source.ArchiveThread(p.caller.ID); err != nil {
		t.Fatal(err)
	}
	owed := p.owed(t)
	if len(owed) != 1 || owed[0].Op != store.OwnerOpArchive {
		t.Fatalf("owed = %+v, want the archive", owed)
	}
	p.peer.asleep.Store(false)
	held, release, returned := make(chan struct{}), make(chan struct{}), make(chan struct{})
	hold := func(op string) func() {
		if op != store.OwnerOpArchive {
			return func() {}
		}
		close(held)
		<-release
		return func() { close(returned) }
	}
	p.peer.hold.Store(&hold)
	ctx, cancel := context.WithCancel(t.Context())
	sent := make(chan struct{})
	go func() {
		defer close(sent)
		p.source.deliverOwnerOp(ctx, owed[0], sweepAttempt)
	}()
	within(t, "the archive reaching the other computer", held)
	// The source gives up on the call; the archive is still on its way.
	cancel()
	within(t, "the source giving up", sent)
	if _, err := p.source.UnarchiveThread(p.caller.ID); err != nil {
		t.Fatal(err)
	}
	if owed := p.owed(t); len(owed) != 1 || owed[0].Op != store.OwnerOpSupersede {
		t.Fatalf("owed after the reopen = %+v, want a supersede", owed)
	}
	p.drain(t)
	if owed := p.owed(t); len(owed) != 0 {
		t.Fatalf("the supersede was not answered: %+v", owed)
	}
	close(release)
	within(t, "the held archive", returned)
	if archivedThere(t, p, p.worker) {
		t.Fatal("an archive admitted after its supersede archived the worker")
	}
}

// An owner whose own local worker is archived reaches, through that worker,
// the worker it spawned on another computer, even one reopened there since
// the worker's own archive.
func TestArchivingAnOwnerReachesTheRemoteWorkerOfItsArchivedWorker(t *testing.T) {
	t.Parallel()
	p := newOwnerOpPair(t, false)
	installMockClaudeTurns(t, p.source, [][]string{{mockClaudeInitLine}})
	sourceID, _ := p.source.backendIdentity()
	local, err := p.adapter().Spawn(t.Context(), p.callerIdentity(), threadtools.SpawnCall{Prompt: "lead it", Title: "Local", ProjectID: p.caller.ProjectID})
	if err != nil {
		t.Fatalf("a local spawn: %v", err)
	}
	var lead string
	waitUntil(t, 10*time.Second, func() bool {
		lead = p.request(t, local.Token).TargetThreadID
		return lead != ""
	})
	ack, err := p.adapter().Spawn(t.Context(), threadtools.Caller{ThreadID: lead, ComputerID: sourceID, ComputerName: "source"},
		threadtools.SpawnCall{Prompt: "work over there", ComputerID: p.computer, ProjectID: p.project})
	if err != nil {
		t.Fatalf("the local worker's spawn there: %v", err)
	}
	remote := p.receipt(t, ack.Token).TargetThreadID
	if _, err := p.source.ArchiveThread(lead); err != nil {
		t.Fatal(err)
	}
	if !archivedThere(t, p, remote) {
		t.Fatal("archiving the local worker did not archive its worker there")
	}
	if _, err := p.dest.UnarchiveThread(remote); err != nil {
		t.Fatal(err)
	}
	if result, err := p.source.ArchiveThread(p.caller.ID); err != nil || len(result.WorkersNotArchived) != 0 {
		t.Fatalf("ArchiveThread = %+v, %v", result, err)
	}
	if !archivedThere(t, p, remote) {
		t.Fatal("archiving the owner left its archived worker's worker there open")
	}
}

// An archive the other computer admitted, whose answer never came back, is
// sent again after the worker there was reopened, and changes nothing
// there: the link took its sequence.
func TestAnArchiveAdmittedWhoseAnswerWasLostIsNotAppliedTwice(t *testing.T) {
	t.Parallel()
	p := newOwnerOpPair(t, false)
	child := p.spawnBelowThere(t, p.worker)
	p.peer.asleep.Store(true)
	if _, err := p.source.ArchiveThread(p.caller.ID); err != nil {
		t.Fatal(err)
	}
	owed := p.owed(t)
	if len(owed) != 1 {
		t.Fatalf("owed = %+v", owed)
	}
	// Admitted there; its answer is lost.
	answer, err := p.dest.admitOwnerOp(t.Context(), p.device, p.caller.ID,
		threadOwnerOpArgs{Spawn: p.token, Seq: owed[0].Seq, Op: store.OwnerOpArchive})
	if err != nil || answer.Outcome != ownerOpApplied {
		t.Fatalf("admit the archive there: %+v, %v", answer, err)
	}
	if !archivedThere(t, p, child) {
		t.Fatal("the archive there did not archive the worker's own worker")
	}
	if _, err := p.dest.UnarchiveThread(p.worker); err != nil {
		t.Fatal(err)
	}
	if err := p.dest.StartSession(p.worker); err != nil {
		t.Fatal(err)
	}
	p.peer.asleep.Store(false)
	p.drain(t)
	if owed := p.owed(t); len(owed) != 0 {
		t.Fatalf("the repeated archive was not settled: %+v", owed)
	}
	if archivedThere(t, p, p.worker) {
		t.Fatal("the repeated archive archived a worker reopened since")
	}
	if _, live := p.dest.sessionManager().get(p.worker); !live {
		t.Fatal("the repeated archive stopped the session of a worker reopened since")
	}
}

// Deleting a spawner deletes the worker it owns on another computer, with
// that worker's own workers there, through that computer's own delete:
// claimed, then committed.
func TestDeletingASpawnerDeletesItsWorkersOnAnotherComputer(t *testing.T) {
	t.Parallel()
	p := newOwnerOpPair(t, false)
	child := p.spawnBelowThere(t, p.worker)
	var claimed, committed []string
	p.dest.deleteClaimedForTest = func(ids []string) { claimed = append(claimed, ids...) }
	p.dest.deleteCommittedForTest = func(ids []string) { committed = append(committed, ids...) }
	result, err := p.source.DeleteThread(p.caller.ID)
	if err != nil || len(result.WorkersNotDeleted) != 0 {
		t.Fatalf("DeleteThread = %+v, %v; want a complete delete", result, err)
	}
	for _, phase := range [][]string{claimed, committed} {
		slices.Sort(phase)
		want := []string{p.worker, child}
		slices.Sort(want)
		if !slices.Equal(phase, want) {
			t.Fatalf("the delete there claimed %v and committed %v, want the worker and its own worker", claimed, committed)
		}
	}
	for _, id := range []string{p.worker, child} {
		if _, err := p.dest.store.GetThread(id); !threadGone(err) {
			t.Errorf("%s outlived the delete there: %v", id, err)
		}
	}
	if owed := p.owed(t); len(owed) != 0 {
		t.Fatalf("the delete still owes %+v", owed)
	}
}

// A spawner deleted after its worker's computer was forgotten owes that
// computer nothing past the one attempt: the pairing ended, so the
// operation settles and the worker is reported on a computer no longer
// connected, not pending.
func TestDeletingASpawnerAfterItsWorkersComputerWasForgottenSettles(t *testing.T) {
	t.Parallel()
	p := newOwnerOpPair(t, false)
	if err := p.source.RemoveBackend(p.computer, true); err != nil {
		t.Fatalf("forget the computer: %v", err)
	}
	result, err := p.source.DeleteThread(p.caller.ID)
	if err != nil {
		t.Fatalf("DeleteThread: %v", err)
	}
	if len(result.WorkersNotDeleted) != 1 {
		t.Fatalf("WorkersNotDeleted = %+v, want the worker there", result.WorkersNotDeleted)
	}
	if failure := result.WorkersNotDeleted[0]; failure.Pending || failure.ThreadID != p.worker || !strings.Contains(failure.Error, "no longer connected") {
		t.Fatalf("the worker there is reported %+v, want it not pending, on a computer no longer connected", failure)
	}
	if owed := p.owed(t); len(owed) != 0 {
		t.Fatalf("the delete still owes %+v to a forgotten computer", owed)
	}
}

// A delete owed to a computer that was asleep is dropped when that
// computer is forgotten.
func TestForgettingAComputerDropsTheDeleteOwedToIt(t *testing.T) {
	t.Parallel()
	p := newOwnerOpPair(t, false)
	p.peer.asleep.Store(true)
	result, err := p.source.DeleteThread(p.caller.ID)
	if err != nil || len(result.WorkersNotDeleted) != 1 || !result.WorkersNotDeleted[0].Pending {
		t.Fatalf("DeleteThread = %+v, %v; want the worker there pending", result, err)
	}
	if owed := p.owed(t); len(owed) != 1 {
		t.Fatalf("owed = %+v, want the delete", owed)
	}
	if err := p.source.RemoveBackend(p.computer, true); err != nil {
		t.Fatalf("forget the computer: %v", err)
	}
	if owed := p.owed(t); len(owed) != 0 {
		t.Fatalf("forgetting the computer left %+v owed", owed)
	}
}

// A delete the other computer refuses, because its worker is being
// transferred, stands here and is sent again until that computer answers:
// once the transfer committed the worker is no longer there to delete.
func TestADeleteTheOtherComputerRefusesIsSentUntilItAnswers(t *testing.T) {
	t.Parallel()
	p := newOwnerOpPair(t, false)
	transfer, err := p.dest.store.CreateThreadTransfer(store.ThreadTransfer{
		ID: entityid.New(), ThreadID: p.worker, PeerBackendID: entityid.New(), Kind: "move", Direction: "outgoing",
		ActivationHash: strings.Repeat("a", 64), PrivateState: []byte(`{}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := p.source.DeleteThread(p.caller.ID)
	if err != nil {
		t.Fatalf("the delete here was refused: %v", err)
	}
	if _, err := p.source.store.GetThread(p.caller.ID); !threadGone(err) {
		t.Fatal("the spawner outlived its delete")
	}
	if len(result.WorkersNotDeleted) != 1 || !result.WorkersNotDeleted[0].Pending || !strings.Contains(result.WorkersNotDeleted[0].Error, "transferred") {
		t.Fatalf("WorkersNotDeleted = %+v, want the worker there pending with the refusal", result.WorkersNotDeleted)
	}
	if owed := p.owed(t); len(owed) != 1 || owed[0].Op != store.OwnerOpDelete {
		t.Fatalf("owed = %+v, want the delete", owed)
	}
	for _, phase := range []string{"prepared", "committed"} {
		if _, err := p.dest.store.AdvanceThreadTransfer(transfer.ID, phase, strings.Repeat("b", 64)); err != nil {
			t.Fatal(err)
		}
	}
	p.drain(t)
	if owed := p.owed(t); len(owed) != 0 {
		t.Fatalf("still owed after the transfer: %+v", owed)
	}
}

// A delete that finds the worker given a sidebar row there keeps it, and
// ends its link: nothing can make it its deleted spawner's worker again.
func TestADeleteKeepsAWorkerGivenASidebarRowAndEndsItsLink(t *testing.T) {
	t.Parallel()
	p := newOwnerOpPair(t, false)
	if _, err := p.dest.SetThreadPlacement([]string{p.worker}, store.PlacementStandalone); err != nil {
		t.Fatal(err)
	}
	if result, err := p.source.DeleteThread(p.caller.ID); err != nil || len(result.WorkersNotDeleted) != 0 {
		t.Fatalf("DeleteThread = %+v, %v", result, err)
	}
	if row := p.there(t, p.worker); row.Placement != store.PlacementStandalone {
		t.Fatalf("the worker there is %s, want kept with its row", row.Placement)
	}
	if p.receipt(t, p.token).SpawnLink {
		t.Fatal("the worker's link outlived its spawner's delete")
	}
	if _, err := p.dest.SetThreadPlacement([]string{p.worker}, store.PlacementOwned); err == nil {
		t.Fatal("Remove from Sidebar made the worker its deleted spawner's again")
	}
	if owed := p.owed(t); len(owed) != 0 {
		t.Fatalf("still owed %+v", owed)
	}
}

// A computer from before owner operations is not asked again: the archive
// and the delete say it runs an older version.
func TestAnOlderComputerIsReportedAndNotAskedAgain(t *testing.T) {
	t.Parallel()
	p := newOwnerOpPair(t, true)
	result, err := p.source.ArchiveThread(p.caller.ID)
	if err != nil || len(result.WorkersNotArchived) != 1 {
		t.Fatalf("ArchiveThread = %+v, %v", result, err)
	}
	if failure := result.WorkersNotArchived[0]; failure.Pending || !strings.Contains(failure.Error, "runs an older version") {
		t.Fatalf("the worker there is reported %+v, want an older version, not pending", failure)
	}
	if owed := p.owed(t); len(owed) != 0 {
		t.Fatalf("an older computer is still owed %+v", owed)
	}
	deleted, err := p.source.DeleteThread(p.caller.ID)
	if err != nil || len(deleted.WorkersNotDeleted) != 1 || deleted.WorkersNotDeleted[0].Pending ||
		!strings.Contains(deleted.WorkersNotDeleted[0].Error, "runs an older version") {
		t.Fatalf("DeleteThread = %+v, %v", deleted, err)
	}
	if owed := p.owed(t); len(owed) != 0 {
		t.Fatalf("an older computer is still owed %+v", owed)
	}
}

// sweepAtTheDoor has the request sweep make a pass of what the source owes
// when a door that owed operations is about to attempt them, as the sweep
// may at any moment once they are owed. answered says whether the pass's
// call to the other computer has its answer before the door goes on, or is
// still on its way then; done lets a call still on its way return and joins
// the pass.
func (p *ownerOpPair) sweepAtTheDoor(t *testing.T, answered bool) (done func()) {
	t.Helper()
	var sweeping atomic.Bool
	reached, release, passed := make(chan struct{}, 1), make(chan struct{}), make(chan struct{})
	hold := func(string) func() {
		if sweeping.Load() {
			reached <- struct{}{}
			if !answered {
				<-release
			}
		}
		return func() {}
	}
	p.peer.hold.Store(&hold)
	var once sync.Once
	p.source.ownerOpsDeliveringForTest = func() {
		once.Do(func() {
			sweeping.Store(true)
			defer sweeping.Store(false)
			p.source.deliverOwedOwnerOps(time.Now().Add(time.Hour))
			go func() {
				p.source.threadRequestsWG.Wait()
				close(passed)
			}()
			if answered {
				within(t, "the sweep's pass", passed)
				return
			}
			select {
			case <-reached:
			case <-passed:
			}
		})
	}
	return func() {
		close(release)
		within(t, "the sweep's pass", passed)
	}
}

// What an archive or a delete tells the person about a worker on another
// computer does not depend on whether the request sweep reached the
// operation first, its answer in or still on its way: the operation is sent
// once, and the door reports how that attempt ended.
func TestADoorReportsItsOperationWhenTheSweepReachesItFirst(t *testing.T) {
	t.Parallel()
	doors := map[string]func(p *ownerOpPair) ([]WorkerFailure, error){
		"archive": func(p *ownerOpPair) ([]WorkerFailure, error) {
			result, err := p.source.ArchiveThread(p.caller.ID)
			return result.WorkersNotArchived, err
		},
		"delete": func(p *ownerOpPair) ([]WorkerFailure, error) {
			result, err := p.source.DeleteThread(p.caller.ID)
			return result.WorkersNotDeleted, err
		},
	}
	for _, tc := range []struct {
		name string
		// older stands up a computer from before owner operations; asleep
		// one that does not answer.
		older, asleep bool
		// answered: the sweep's attempt had its answer before the door went
		// on; otherwise it was still on its way.
		answered bool
		pending  bool
		want     string
	}{
		{name: "an older computer, the sweep's attempt answered", older: true, answered: true, want: "runs an older version"},
		{name: "an older computer, the sweep's attempt on its way", older: true, want: "runs an older version"},
		{name: "a computer that does not answer", asleep: true, answered: true, pending: true, want: "did not answer in time"},
	} {
		for door, run := range doors {
			t.Run(door+", "+tc.name, func(t *testing.T) {
				t.Parallel()
				p := newOwnerOpPair(t, tc.older)
				p.peer.asleep.Store(tc.asleep)
				done := p.sweepAtTheDoor(t, tc.answered)
				failures, err := run(p)
				done()
				if err != nil || len(failures) != 1 {
					t.Fatalf("the %s reported %+v, %v; want the worker there", door, failures, err)
				}
				if failure := failures[0]; failure.ThreadID != p.worker || failure.Pending != tc.pending || !strings.Contains(failure.Error, tc.want) {
					t.Fatalf("the worker there is reported %+v, want %q, pending %v", failure, tc.want, tc.pending)
				}
				if calls := p.peer.ownerOpCalls.Load(); calls != 1 {
					t.Fatalf("the operation was sent %d times, want once", calls)
				}
			})
		}
	}
}

// heldNothing fails the test while a door still holds a spawn of the
// source's, which the sweep would then leave alone.
func (p *ownerOpPair) heldNothing(t *testing.T) {
	t.Helper()
	d := &p.source.ownerOps
	d.mu.Lock()
	held := maps.Clone(d.held)
	d.mu.Unlock()
	if len(held) != 0 {
		t.Fatalf("a door that returned still holds %v", held)
	}
}

// sweepAnArchiveOnItsWay owes the caller's archive with no door to attempt
// it, as a worker's own walk owes one, and has the sweep's attempt at it
// reach the other computer and stay there until release, which the test's
// end also runs; passed closes once that pass ends.
func (p *ownerOpPair) sweepAnArchiveOnItsWay(t *testing.T) (release func(), passed chan struct{}) {
	t.Helper()
	if _, _, err := p.source.store.ArchiveThread(p.caller.ID); err != nil {
		t.Fatal(err)
	}
	if owed, err := p.source.store.OweRemoteArchives(p.caller.ID, nil); err != nil || len(owed) != 1 {
		t.Fatalf("owed %+v, %v; want the archive", owed, err)
	}
	reached, released, passed := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var reach, end sync.Once
	release = func() { end.Do(func() { close(released) }) }
	t.Cleanup(release)
	hold := func(op string) func() {
		if op == store.OwnerOpArchive {
			reach.Do(func() { close(reached) })
			<-released
		}
		return func() {}
	}
	p.peer.hold.Store(&hold)
	p.source.deliverOwedOwnerOps(time.Now().Add(time.Hour))
	go func() {
		p.source.threadRequestsWG.Wait()
		close(passed)
	}()
	within(t, "the archive reaching the other computer", reached)
	return release, passed
}

// A delete that owes the spawn an operation while the sweep's attempt at an
// earlier one is on its way waits for that attempt, then makes its own: its
// report is how the computer answered, not a timeout, and a computer that
// answered the earlier one as an older version is not asked again.
func TestADoorWaitsForAnEarlierOperationOnItsWay(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		older bool
		calls int32
		want  string
	}{
		{name: "a computer that takes both", calls: 2},
		{name: "a computer on an older version", older: true, calls: 1, want: "runs an older version"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			p := newOwnerOpPair(t, tc.older)
			release, passed := p.sweepAnArchiveOnItsWay(t)
			var waited atomic.Bool
			p.source.ownerOpWaitingForTest = func(string) {
				if waited.CompareAndSwap(false, true) {
					release()
				}
			}
			result, err := p.source.DeleteThread(p.caller.ID)
			if err != nil {
				t.Fatal(err)
			}
			switch {
			case tc.want == "" && len(result.WorkersNotDeleted) != 0:
				t.Fatalf("the delete reported %+v, want nothing left undone", result.WorkersNotDeleted)
			case tc.want != "" && (len(result.WorkersNotDeleted) != 1 || result.WorkersNotDeleted[0].Pending ||
				!strings.Contains(result.WorkersNotDeleted[0].Error, tc.want)):
				t.Fatalf("the delete reported %+v, want %q", result.WorkersNotDeleted, tc.want)
			}
			if !waited.Load() {
				t.Fatal("the delete did not wait for the archive on its way")
			}
			within(t, "the sweep's pass", passed)
			if calls := p.peer.ownerOpCalls.Load(); calls != tc.calls {
				t.Fatalf("the computer was sent %d operations, want %d", calls, tc.calls)
			}
			if _, err := p.dest.store.GetThread(p.worker); tc.want == "" && !threadGone(err) {
				t.Fatalf("the worker there outlived the delete: %v", err)
			}
			if owed := p.owed(t); len(owed) != 0 {
				t.Fatalf("still owed %+v", owed)
			}
			p.heldNothing(t)
		})
	}
}

// A door whose wait for an earlier attempt runs out reports the computer
// did not answer in time, which it did not, and lets the sweep have the
// operation, which it delivers once the computer answers.
func TestADoorWhoseWaitRunsOutLeavesItsOperationToTheSweep(t *testing.T) {
	t.Parallel()
	p := newOwnerOpPair(t, false)
	// Its subject is the bound running out.
	p.source.ownerOpReachOverride = time.Second
	release, passed := p.sweepAnArchiveOnItsWay(t)
	result, err := p.source.DeleteThread(p.caller.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.WorkersNotDeleted) != 1 || !result.WorkersNotDeleted[0].Pending ||
		!strings.Contains(result.WorkersNotDeleted[0].Error, "did not answer in time") {
		t.Fatalf("the delete reported %+v, want the worker pending", result.WorkersNotDeleted)
	}
	p.heldNothing(t)
	release()
	within(t, "the sweep's pass", passed)
	p.drain(t)
	if _, err := p.dest.store.GetThread(p.worker); !threadGone(err) {
		t.Fatalf("the sweep did not deliver the delete: %v", err)
	}
	if owed := p.owed(t); len(owed) != 0 {
		t.Fatalf("still owed %+v", owed)
	}
}

// However a door ends, it holds nothing after: a write that fails to owe
// what it held, a door that panics, the app stopping during its attempt.
func TestADoorHoldsNothingOnceItEnds(t *testing.T) {
	t.Parallel()
	refuseOwing := func(t *testing.T, p *ownerOpPair) func() {
		execOnStore(t, p.dbPath, `CREATE TRIGGER refuse_owner_op BEFORE INSERT ON thread_owner_ops BEGIN SELECT RAISE(ABORT, 'injected owe failure'); END`)
		return func() { execOnStore(t, p.dbPath, `DROP TRIGGER refuse_owner_op`) }
	}
	t.Run("a delete whose commit fails", func(t *testing.T) {
		t.Parallel()
		p := newOwnerOpPair(t, false)
		allow := refuseOwing(t, p)
		if _, err := p.source.DeleteThread(p.caller.ID); err == nil || !strings.Contains(err.Error(), "injected owe failure") {
			t.Fatalf("DeleteThread = %v, want the injected failure", err)
		}
		p.heldNothing(t)
		allow()
		if result, err := p.source.DeleteThread(p.caller.ID); err != nil || len(result.WorkersNotDeleted) != 0 {
			t.Fatalf("the delete again = %+v, %v", result, err)
		}
		if _, err := p.dest.store.GetThread(p.worker); !threadGone(err) {
			t.Fatalf("the worker there outlived the delete: %v", err)
		}
	})
	t.Run("an archive whose end fails to owe", func(t *testing.T) {
		t.Parallel()
		p := newOwnerOpPair(t, false)
		allow := refuseOwing(t, p)
		if result, err := p.source.ArchiveThread(p.caller.ID); err != nil || !strings.Contains(result.CleanupError, "were not sent the archive") {
			t.Fatalf("ArchiveThread = %+v, %v; want the failed end said", result, err)
		}
		p.heldNothing(t)
		allow()
		if result, err := p.source.ArchiveThread(p.caller.ID); err != nil || len(result.WorkersNotArchived) != 0 {
			t.Fatalf("the archive again = %+v, %v", result, err)
		}
		if !archivedThere(t, p, p.worker) {
			t.Fatal("the worker there was not archived")
		}
	})
	t.Run("a delete that panics before its attempt", func(t *testing.T) {
		t.Parallel()
		p := newOwnerOpPair(t, false)
		p.source.ownerOpsDeliveringForTest = func() { panic("the door fails") }
		func() {
			defer func() {
				if recover() == nil {
					t.Fatal("the delete did not panic")
				}
			}()
			_, _ = p.source.DeleteThread(p.caller.ID)
		}()
		p.heldNothing(t)
		p.drain(t)
		if _, err := p.dest.store.GetThread(p.worker); !threadGone(err) {
			t.Fatalf("the sweep did not deliver the delete: %v", err)
		}
	})
	t.Run("the app stopping during a delete's attempt", func(t *testing.T) {
		t.Parallel()
		p := newOwnerOpPair(t, false)
		p.source.ownerOpsDeliveringForTest = p.source.appCancel
		if _, err := p.source.DeleteThread(p.caller.ID); err != nil {
			t.Fatal(err)
		}
		p.heldNothing(t)
		if owed := p.owed(t); len(owed) != 1 || owed[0].Op != store.OwnerOpDelete {
			t.Fatalf("owed %+v, want the delete still owed", owed)
		}
	})
}

// The cleanup of a spawner moved to another computer releases its worker
// here to a sidebar row there, announced there, and deletes nothing.
func TestTheCleanupOfASpawnerMovedAwayReleasesItsWorkers(t *testing.T) {
	t.Parallel()
	p := newOwnerOpPair(t, false)
	listedThere := recordListedFrames(p.dest)
	transfer, err := p.source.store.CreateThreadTransfer(store.ThreadTransfer{
		ID: entityid.New(), ThreadID: p.caller.ID, PeerBackendID: entityid.New(), Kind: "move", Direction: "outgoing",
		ActivationHash: strings.Repeat("a", 64), PrivateState: []byte(`{}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, phase := range []string{"prepared", "committed", "complete"} {
		if _, err := p.source.store.AdvanceThreadTransfer(transfer.ID, phase, strings.Repeat("b", 64)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := p.source.deleteThreads(deleteRequest{seeds: []string{p.caller.ID}, cleanup: true}); err != nil {
		t.Fatal(err)
	}
	if owed := p.owed(t); len(owed) != 1 || owed[0].Op != store.OwnerOpRelease {
		t.Fatalf("owed = %+v, want a release", owed)
	}
	p.drain(t)
	if row := p.there(t, p.worker); row.Placement != store.PlacementStandalone {
		t.Fatalf("the worker there is %s, want standalone", row.Placement)
	}
	waitUntil(t, 10*time.Second, func() bool { return listedThere(p.worker) })
	if owed := p.owed(t); len(owed) != 0 {
		t.Fatalf("still owed %+v", owed)
	}
}

// A worker moved on from the computer its spawn went to is out of the
// spawner's reach: its archive is answered there as gone, and settled.
func TestAnArchiveForAWorkerMovedOnIsSettled(t *testing.T) {
	t.Parallel()
	p := newOwnerOpPair(t, false)
	transfer, err := p.dest.store.CreateThreadTransfer(store.ThreadTransfer{
		ID: entityid.New(), ThreadID: p.worker, PeerBackendID: entityid.New(), Kind: "move", Direction: "outgoing",
		ActivationHash: strings.Repeat("a", 64), PrivateState: []byte(`{}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, phase := range []string{"prepared", "committed", "complete"} {
		if _, err := p.dest.store.AdvanceThreadTransfer(transfer.ID, phase, strings.Repeat("b", 64)); err != nil {
			t.Fatal(err)
		}
	}
	result, err := p.source.ArchiveThread(p.caller.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.WorkersNotArchived) != 0 {
		t.Fatalf("WorkersNotArchived = %+v, want none: the worker moved on", result.WorkersNotArchived)
	}
	if owed := p.owed(t); len(owed) != 0 {
		t.Fatalf("still owed %+v", owed)
	}
}

// Revoking the spawner's device on the worker's computer gives the worker a
// sidebar row there, announced to every window.
func TestRevokingTheSpawnersDeviceGivesItsWorkerASidebarRow(t *testing.T) {
	t.Parallel()
	p := newOwnerOpPair(t, false)
	listedThere := recordListedFrames(p.dest)
	if _, err := p.dest.RevokeAccessDevice(p.device); err != nil {
		t.Fatal(err)
	}
	if row := p.there(t, p.worker); row.Placement != store.PlacementStandalone {
		t.Fatalf("the worker there is %s, want standalone", row.Placement)
	}
	waitUntil(t, 10*time.Second, func() bool { return listedThere(p.worker) })
}

// An owner operation for a spawn whose thread is not named yet waits for
// it; one for a spawn that will never name one, or that another device or
// source thread made, finds nothing.
func TestAnOwnerOpForASpawnWithoutAThread(t *testing.T) {
	t.Parallel()
	p := newOwnerOpPair(t, false)
	token := entityid.New()
	source := p.caller.ID
	if _, _, err := p.dest.store.AcceptThreadRequestReceipt(store.ThreadRequestReceipt{
		Token: token, OwnerDeviceID: p.device, SourceComputerID: p.sourceID, SourceThreadID: source,
		Kind: store.ThreadRequestSpawn, SpawnLink: true, State: store.ThreadReceiptAccepted, CreatedAt: 1,
	}); err != nil {
		t.Fatal(err)
	}
	admit := func(device, source string) string {
		t.Helper()
		answer, err := p.dest.admitOwnerOp(t.Context(), device, source, threadOwnerOpArgs{Spawn: token, Seq: 1, Op: store.OwnerOpDelete})
		if err != nil {
			t.Fatal(err)
		}
		return answer.Outcome
	}
	if got := admit(p.device, source); got != ownerOpPending {
		t.Fatalf("an unnamed open spawn = %s, want pending", got)
	}
	if got := admit("another-device", source); got != ownerOpGone {
		t.Fatalf("another device's spawn = %s, want gone", got)
	}
	if got := admit(p.device, entityid.New()); got != ownerOpGone {
		t.Fatalf("another source's spawn = %s, want gone", got)
	}
	if _, _, err := p.dest.settleThreadReceiptNoScratch(token, store.ThreadReceiptOpenStates(), store.ThreadRequestSettlement{
		State: store.ThreadReceiptErrored, AnswerKind: store.ThreadAnswerError, Answer: []byte("interrupted"),
	}); err != nil {
		t.Fatal(err)
	}
	if got := admit(p.device, source); got != ownerOpGone {
		t.Fatalf("a settled spawn with no thread = %s, want gone", got)
	}
}

// A delete's confirmation says when what it takes spawned workers on other
// computers.
func TestTheDeleteConfirmationSaysWhenWorkersOnOtherComputersGoToo(t *testing.T) {
	t.Parallel()
	p := newOwnerOpPair(t, false)
	got, err := p.source.GetDeleteDependents([]string{p.caller.ID})
	if err != nil || !got.RemoteWorkers {
		t.Fatalf("GetDeleteDependents = %+v, %v; want workers on other computers", got, err)
	}
	other, err := createTestThread(t, p.source, p.caller.Provider, t.TempDir(), p.caller.Model, p.caller.Mode)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := p.source.GetDeleteDependents([]string{other.ID}); err != nil || got.RemoteWorkers {
		t.Fatalf("a thread with no remote spawn = %+v, %v", got, err)
	}
}

// A worker the person gave a sidebar row on its computer stays when its
// spawner is deleted, so the confirmation leaves it out once this computer
// has heard that placement; until it has, the confirmation says so.
func TestTheDeleteConfirmationLeavesOutAWorkerWithASidebarRowThere(t *testing.T) {
	t.Parallel()
	pair := newReachPair(t)
	installMockClaudeTurns(t, pair.dest, [][]string{{mockClaudeInitLine}})
	ack := pair.spawnThere(t, "for the user", map[string]any{"placement": threadtools.PlacementStandalone})
	if got, err := pair.source.GetDeleteDependents([]string{pair.caller.ID}); err != nil || !got.RemoteWorkers {
		t.Fatalf("GetDeleteDependents before the placement is known = %+v, %v; want workers on other computers", got, err)
	}
	waitUntil(t, 10*time.Second, func() bool {
		if _, err := pair.source.ListThreadAgentRows(t.Context(), pair.caller.ID); err != nil {
			t.Fatal(err)
		}
		c := &pair.source.threadAgentPeers
		c.mu.Lock()
		defer c.mu.Unlock()
		entry := c.entries[ack.Token]
		return entry != nil && entry.listing.Placement == threadtools.PlacementStandalone
	})
	if got, err := pair.source.GetDeleteDependents([]string{pair.caller.ID}); err != nil || got.RemoteWorkers {
		t.Fatalf("GetDeleteDependents = %+v, %v; want no workers on other computers", got, err)
	}
}

// A delete after an archive the other computer answered still applies: the
// operations are ordered by their sequence, not by a clock.
func TestADeleteAfterAnAnsweredArchiveApplies(t *testing.T) {
	t.Parallel()
	p := newOwnerOpPair(t, false)
	if _, err := p.source.ArchiveThread(p.caller.ID); err != nil {
		t.Fatal(err)
	}
	if !archivedThere(t, p, p.worker) {
		t.Fatal("the archive did not reach the worker")
	}
	if result, err := p.source.DeleteThread(p.caller.ID); err != nil || len(result.WorkersNotDeleted) != 0 {
		t.Fatalf("DeleteThread = %+v, %v", result, err)
	}
	if _, err := p.dest.store.GetThread(p.worker); !threadGone(err) {
		t.Fatalf("the archived worker outlived the delete: %v", err)
	}
}

// The delete there is that computer's own: while it decides, a start the
// app makes for the worker waits at its gate, and once it commits the
// worker starts nothing.
func TestALinkedDeleteHoldsTheWorkersStartsUntilItCommits(t *testing.T) {
	t.Parallel()
	p := newOwnerOpPair(t, false)
	// A worker with no session: what the app queues for it starts one.
	if err := p.dest.StopSession(p.worker); err != nil {
		t.Fatal(err)
	}
	p.peer.asleep.Store(true)
	if _, err := p.source.DeleteThread(p.caller.ID); err != nil {
		t.Fatal(err)
	}
	p.peer.asleep.Store(false)
	verified, release := make(chan struct{}), make(chan struct{})
	var released sync.Once
	free := func() { released.Do(func() { close(release) }) }
	t.Cleanup(free)
	p.dest.deleteVerifiedForTest = func(id string) {
		if id == p.worker {
			close(verified)
			<-release
		}
	}
	drained := make(chan struct{})
	go func() {
		defer close(drained)
		p.source.deliverOwedOwnerOps(time.Now().Add(time.Hour))
		p.source.threadRequestsWG.Wait()
	}()
	within(t, "the delete there verifying the worker", verified)
	token := "remind-" + entityid.New()
	insertTestReminder(t, p.dest, token, p.worker, "check in", time.Now().Add(-time.Second).UnixMilli())
	p.dest.fireDueThreadReminders(time.Now())
	waitAtGates(t, p.dest, 1)
	free()
	within(t, "the delivery", drained)
	if owed := p.owed(t); len(owed) != 0 {
		t.Fatalf("still owed %+v", owed)
	}
	nothingWaits(t, p.dest)
	if _, err := p.dest.store.GetThread(p.worker); !threadGone(err) {
		t.Fatalf("the worker outlived the delete there: %v", err)
	}
	if _, live := p.dest.sessionManager().get(p.worker); live {
		t.Fatal("the deleted worker was started")
	}
}

// A spawn on its way to the other computer when its spawner's delete
// starts holds the delete at its verify; the delete then owes the thread
// the spawn made there, and deletes it.
func TestASpawnOnItsWayWhenItsSpawnerIsDeletedIsDeletedThere(t *testing.T) {
	t.Parallel()
	p := newOwnerOpPair(t, false)
	held, release := make(chan struct{}), make(chan struct{})
	var hold sync.Once
	p.dest.spawnNamingForTest = func(id string) {
		if id == p.caller.ID {
			hold.Do(func() {
				close(held)
				<-release
			})
		}
	}
	spawned := make(chan string, 1)
	go func() {
		ack, err := p.adapter().Spawn(t.Context(), p.callerIdentity(),
			threadtools.SpawnCall{Prompt: "more work", ComputerID: p.computer, ProjectID: p.project})
		if err != nil {
			t.Errorf("the spawn on its way: %v", err)
		}
		spawned <- ack.Token
	}()
	within(t, "the spawn's hold there", held)
	deleted := make(chan error, 1)
	go func() {
		_, err := p.source.DeleteThread(p.caller.ID)
		deleted <- err
	}()
	// The delete waits on the admission the spawn holds.
	waitUntil(t, 10*time.Second, func() bool {
		p.source.resting.mu.Lock()
		defer p.source.resting.mu.Unlock()
		return p.source.resting.closed[p.caller.ID] > 0
	})
	select {
	case err := <-deleted:
		t.Fatalf("the delete ended while a spawn of its thread was on its way: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	token := within(t, "the spawn", spawned)
	if err := within(t, "the delete", deleted); err != nil {
		t.Fatal(err)
	}
	// The thread it made there was named, then deleted with its spawner.
	if receipt := p.receipt(t, token); receipt.State == store.ThreadReceiptAccepted || receipt.TargetThreadID != "" {
		t.Fatalf("the spawn's receipt there = %+v, want its thread deleted", receipt)
	}
	rows, err := p.dest.store.ListThreads()
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Fatalf("%d threads outlived their spawner's delete there (%s)", len(rows), rows[0].ID)
	}
	if owed := p.owed(t); len(owed) != 0 {
		t.Fatalf("still owed %+v", owed)
	}
}
