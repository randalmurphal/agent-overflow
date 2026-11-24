package app

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/entityid"
	"agent-overflow/internal/store"
)

// A start the app makes on its own, a live session's dispatch or a notice's
// session start, waits at a delete's decision gate while the delete decides
// (deleteDecisions): a refused delete's starts go on as if the claim had
// never stood, a committed delete's start nothing, and none is left
// waiting.

// waitingAtGates is how many starts wait at a delete's gate.
func waitingAtGates(a *App) int {
	a.deleteDecisions.mu.Lock()
	defer a.deleteDecisions.mu.Unlock()
	return a.deleteDecisions.waiting
}

// quietAtGates routes the worker prompts and waits until every notice start
// and dispatch begun has finished or waits at a delete's gate.
func quietAtGates(t *testing.T, a *App) {
	t.Helper()
	if err := a.drainWorkerPrompts(t.Context(), 10*time.Second); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, 10*time.Second, func() bool {
		a.agentNoticeWork.mu.Lock()
		notices := a.agentNoticeWork.running
		a.agentNoticeWork.mu.Unlock()
		a.flushDispatch.mu.Lock()
		workers := len(a.flushDispatch.running)
		a.flushDispatch.mu.Unlock()
		return notices+workers == waitingAtGates(a)
	})
}

// waitAtGates waits until n starts wait at a delete's gate.
func waitAtGates(t *testing.T, a *App, n int) {
	t.Helper()
	waitUntil(t, 10*time.Second, func() bool { return waitingAtGates(a) == n })
}

// nothingWaits fails unless no start waits at a gate and no notice start or
// dispatch is left running.
func nothingWaits(t *testing.T, a *App) {
	t.Helper()
	a.waitAgentNoticeWork()
	a.flushDispatch.wg.Wait()
	if n := waitingAtGates(a); n != 0 {
		t.Fatalf("%d starts still wait at a gate", n)
	}
	a.flushDispatch.mu.Lock()
	workers := len(a.flushDispatch.running)
	a.flushDispatch.mu.Unlock()
	if workers != 0 {
		t.Fatalf("%d dispatch workers are left", workers)
	}
}

// abandonGates drops every gate as the end of the process that held them
// does: a start waiting at one finds the claim with no delete to decide it.
func abandonGates(a *App) {
	a.deleteDecisions.mu.Lock()
	defer a.deleteDecisions.mu.Unlock()
	for op, decision := range a.deleteDecisions.ops {
		close(decision.decided)
		delete(a.deleteDecisions.ops, op)
	}
}

// claimForTest claims ids for the delete op as a delete does, its decision
// registered before its claim; releaseForTest refuses it.
func claimForTest(t *testing.T, a *App, op string, ids []string) {
	t.Helper()
	a.deleteDecisions.open(op)
	if _, err := a.store.ClaimDeleteFamily(op, ids); err != nil {
		t.Fatal(err)
	}
}

func releaseForTest(t *testing.T, a *App, op string) {
	t.Helper()
	if err := a.store.ReleaseDeleteClaim(op); err != nil {
		t.Fatal(err)
	}
	a.deleteDecisions.decide(op, false)
}

// deleteOf runs a delete of threadID, refused or committed as refuse says,
// on its own goroutine, and returns once its claim is written; verified runs
// once it verified the family, before its decision. The returned channel
// has its error.
func deleteOf(t *testing.T, a *App, threadID string, refuse bool, verified func()) <-chan error {
	t.Helper()
	claimed := make(chan struct{})
	a.deleteClaimedForTest = func([]string) { close(claimed) }
	done := make(chan error, 1)
	go func() {
		_, err := a.deleteThreads(deleteRequest{seeds: []string{threadID}, verified: func([]store.DeleteMember) error {
			verified()
			if refuse {
				return errRefusedForTest
			}
			return nil
		}})
		done <- err
	}()
	within(t, "the claim", claimed)
	a.deleteClaimedForTest = nil
	return done
}

// decided waits for a delete deleteOf ran, and fails unless it was refused
// or committed as refuse said.
func decided(t *testing.T, done <-chan error, refuse bool) {
	t.Helper()
	if err := within(t, "the delete", done); errors.Is(err, errRefusedForTest) != refuse || (!refuse && err != nil) {
		t.Fatalf("the delete = %v, want refused %v", err, refuse)
	}
}

// A claim that lands while a live session's batch waits for the thread's
// action lock holds the batch: the dispatch reads the claim once it has the
// lock and waits for the decision. A refused delete's batch is sent once; a
// committed one's is sent nothing, with no waiter or worker left.
func TestAClaimLandingWhileABatchWaitsOnTheActionLockHoldsIt(t *testing.T) {
	t.Parallel()
	for _, refuse := range []bool{true, false} {
		t.Run(map[bool]string{true: "refused", false: "committed"}[refuse], func(t *testing.T) {
			t.Parallel()
			f := newClaimFixture(t)
			if err := f.app.startSessionTakingLock(context.Background(), f.caller.ID); err != nil {
				t.Fatal(err)
			}
			unlock := f.app.threadLocks().Lock(f.caller.ID)
			marker := "held-" + entityid.New()
			queueTestWake(t, f.app, f.caller.ID, marker)
			heldAtTheActionLock(t, f.app, f.caller.ID)
			done := deleteOf(t, f.app, f.caller.ID, refuse, func() {
				waitAtGates(t, f.app, 1)
				if n := f.sent(t, marker); n != 0 {
					t.Errorf("a claimed thread's batch reached the provider %d times", n)
				}
			})
			unlock()
			decided(t, done, refuse)
			if refuse {
				f.deliveredOnce(t, marker)
				return
			}
			nothingWaits(t, f.app)
			if n := f.sent(t, marker); n != 0 {
				t.Fatalf("a committed thread's batch reached the provider %d times", n)
			}
		})
	}
}

// A batch the dispatcher holds, at the thread's action lock or at a
// delete's decision, has left triage's queue and is not the provider's
// yet: it keeps its thread from rest, so a request held for that rest
// (threadRest) does not settle on a thread about to take a message. A
// refused delete's batch is then sent once; a committed one's is sent
// nothing.
func TestABatchTheDispatcherHoldsKeepsItsThreadFromRest(t *testing.T) {
	t.Parallel()
	for _, refuse := range []bool{true, false} {
		t.Run(map[bool]string{true: "refused", false: "committed"}[refuse], func(t *testing.T) {
			t.Parallel()
			f := newClaimFixture(t)
			id := f.caller.ID
			if err := f.app.startSessionTakingLock(context.Background(), id); err != nil {
				t.Fatal(err)
			}
			notAtRest := func(when string) {
				t.Helper()
				if n := len(f.app.triage.LiveStateSnapshotForThread(id).QueueItems); n != 0 {
					t.Fatalf("%s: triage still queues %d messages; want the batch the dispatcher's", when, n)
				}
				if rest, err := f.app.threadRest(id); err != nil || rest.unrest != workerQueued {
					t.Errorf("%s: rest %+v, %v; want queued", when, rest, err)
				}
			}
			unlock := f.app.threadLocks().Lock(id)
			marker := "rest-held-" + entityid.New()
			queueTestWake(t, f.app, id, marker)
			heldAtTheActionLock(t, f.app, id)
			notAtRest("at the action lock")
			done := deleteOf(t, f.app, id, refuse, func() {
				waitAtGates(t, f.app, 1)
				notAtRest("at the delete's decision")
			})
			unlock()
			decided(t, done, refuse)
			if refuse {
				f.deliveredOnce(t, marker)
				return
			}
			nothingWaits(t, f.app)
			if n := f.sent(t, marker); n != 0 {
				t.Fatalf("a committed thread's batch reached the provider %d times", n)
			}
		})
	}
}

// A batch waiting for a delete that is refused, and claimed again by a
// second delete before the batch reads the thread again, waits for the
// second delete's decision too.
func TestABatchWaitsForAReclaimAfterARelease(t *testing.T) {
	t.Parallel()
	f := newClaimFixture(t)
	if err := f.app.startSessionTakingLock(context.Background(), f.caller.ID); err != nil {
		t.Fatal(err)
	}
	marker := "twice-held-" + entityid.New()
	var unlock func()
	first := deleteOf(t, f.app, f.caller.ID, true, func() {
		queueTestWake(t, f.app, f.caller.ID, marker)
		waitAtGates(t, f.app, 1)
		// Held across the refusal, so the batch reads the thread again only
		// once the second delete's claim is written.
		unlock = f.app.threadLocks().Lock(f.caller.ID)
	})
	decided(t, first, true)
	second := deleteOf(t, f.app, f.caller.ID, true, func() {
		waitAtGates(t, f.app, 1)
		if n := f.sent(t, marker); n != 0 {
			t.Errorf("the batch reached the provider %d times before the second decision", n)
		}
	})
	unlock()
	decided(t, second, true)
	f.deliveredOnce(t, marker)
	nothingWaits(t, f.app)
}

// A delete that commits a family while a live thread's batch and a stopped
// worker's two notice starts wait for its decision ends all three: nothing
// is sent or started, no start is recorded as failed, and no waiter, notice
// start or dispatch worker is left.
func TestACommitEndsEveryStartWaitingForIt(t *testing.T) {
	t.Parallel()
	f := newClaimFixture(t)
	worker := f.plainThread(t)
	recordSpawn(t, f.app, "spawn-"+entityid.New(), f.caller.ID, worker.ID)
	if err := f.app.startSessionTakingLock(context.Background(), f.caller.ID); err != nil {
		t.Fatal(err)
	}
	batch := "batch-" + entityid.New()
	reminders := []string{"remind-" + entityid.New(), "remind-" + entityid.New()}
	// The commit itself lets the starts go, not the end of the delete.
	waitedOut := make(chan bool, 1)
	f.app.deleteCommittedForTest = func([]string) {
		deadline := time.Now().Add(5 * time.Second)
		for waitingAtGates(f.app) != 0 && time.Now().Before(deadline) {
			time.Sleep(time.Millisecond)
		}
		waitedOut <- waitingAtGates(f.app) != 0
	}
	done := deleteOf(t, f.app, f.caller.ID, false, func() {
		queueTestWake(t, f.app, f.caller.ID, batch)
		for _, token := range reminders {
			insertTestReminder(t, f.app, token, worker.ID, "remind "+token, time.Now().Add(-time.Second).UnixMilli())
			f.app.fireDueThreadReminders(time.Now())
			awaitQueuedWake(t, f.app, worker.ID, threadWakeSendID(token))
		}
		waitAtGates(t, f.app, 3)
	})
	decided(t, done, false)
	if <-waitedOut {
		t.Fatal("starts still waited for the decision after the delete committed")
	}
	nothingWaits(t, f.app)
	if n := len(settledUserEnvelopes(t, f.stdin)); n != 0 {
		t.Fatalf("the committed family's provider was written %d messages", n)
	}
	if _, live := f.app.sessionManager().get(worker.ID); live {
		t.Fatal("a committed worker was started")
	}
	f.app.agentNoticeWork.mu.Lock()
	failing := len(f.app.agentNoticeWork.failing)
	running := f.app.agentNoticeWork.running
	f.app.agentNoticeWork.mu.Unlock()
	if failing != 0 || running != 0 {
		t.Fatalf("after the commit: %d failed starts recorded, %d notice starts running; want none", failing, running)
	}
}

// Shutdown ends the starts waiting for a delete's decision: the batch and
// the notice start return without sending or starting anything and record
// no failure, and the messages stay in the durable queue for the next boot.
func TestShutdownEndsTheStartsWaitingForADecision(t *testing.T) {
	t.Parallel()
	f := newClaimFixture(t)
	f.app.appCtx, f.app.appCancel = context.WithCancel(context.Background())
	worker := f.plainThread(t)
	recordSpawn(t, f.app, "spawn-"+entityid.New(), f.caller.ID, worker.ID)
	if err := f.app.startSessionTakingLock(context.Background(), f.caller.ID); err != nil {
		t.Fatal(err)
	}
	batch := "batch-" + entityid.New()
	token := "remind-" + entityid.New()
	refusedDelete(t, f.app, f.caller.ID, func() {
		queueTestWake(t, f.app, f.caller.ID, batch)
		insertTestReminder(t, f.app, token, worker.ID, "remind "+token, time.Now().Add(-time.Second).UnixMilli())
		f.app.fireDueThreadReminders(time.Now())
		waitAtGates(t, f.app, 2)
		f.app.appCancel()
		nothingWaits(t, f.app)
	})
	if n := len(settledUserEnvelopes(t, f.stdin)); n != 0 {
		t.Fatalf("the provider was written %d messages", n)
	}
	if _, live := f.app.sessionManager().get(worker.ID); live {
		t.Fatal("the worker was started")
	}
	f.app.agentNoticeWork.mu.Lock()
	failing := len(f.app.agentNoticeWork.failing)
	f.app.agentNoticeWork.mu.Unlock()
	if failing != 0 {
		t.Fatalf("%d failed starts recorded at shutdown, want none", failing)
	}
	if len(durableQueueRows(t, f.app, f.caller.ID)) != 1 || len(durableQueueRows(t, f.app, worker.ID)) != 1 {
		t.Fatal("a message left the durable queue at shutdown")
	}
}

// A worker that joins the family while the delete verifies it, its spawn
// named during the drain, has its gate opened with its claim: a notice for
// it then waits for the decision, and the refused delete's release starts
// the worker for it.
func TestAThreadTheFamilyGainsDuringVerifyWaitsForTheDecision(t *testing.T) {
	t.Parallel()
	f := newClaimFixture(t)
	owner := f.caller.ID
	done, err := f.app.admitWork(owner, "")
	if err != nil {
		t.Fatal(err)
	}
	late := f.plainThread(t)
	if _, err := f.app.store.AcceptLocalThreadRequest(store.ThreadRequest{
		Token: "tok-late", CallerThreadID: owner, Kind: store.ThreadRequestSpawn, State: store.ThreadRequestUnconfirmed,
	}, store.ThreadRequestReceipt{Token: "tok-late", OwnerDeviceID: "device", Kind: store.ThreadRequestSpawn, SourceThreadID: owner}); err != nil {
		t.Fatal(err)
	}
	named := make(chan error, 1)
	go func() {
		defer done()
		// Name the spawn's thread once the delete waits on it.
		for {
			f.app.resting.mu.Lock()
			closed := f.app.resting.closed[owner] > 0
			f.app.resting.mu.Unlock()
			if closed {
				break
			}
			time.Sleep(time.Millisecond)
		}
		_, err := f.app.store.NameLocalThreadRequestTarget("tok-late", late.ID, true)
		named <- err
	}()
	token := "remind-" + entityid.New()
	f.app.deleteVerifiedForTest = func(id string) {
		if id != late.ID {
			return
		}
		insertTestReminder(t, f.app, token, late.ID, "remind "+token, time.Now().Add(-time.Second).UnixMilli())
		f.app.fireDueThreadReminders(time.Now())
		awaitQueuedWake(t, f.app, late.ID, threadWakeSendID(token))
		waitAtGates(t, f.app, 1)
	}
	if _, err := f.app.deleteThreads(deleteRequest{seeds: []string{owner}, verified: func(members []store.DeleteMember) error {
		if !memberOf(members, late.ID) {
			t.Error("the late worker did not join the family")
		}
		return errRefusedForTest
	}}); !errors.Is(err, errRefusedForTest) {
		t.Fatalf("the delete = %v, want it refused", err)
	}
	f.app.deleteVerifiedForTest = nil
	if err := within(t, "the late spawn", named); err != nil {
		t.Fatalf("name the late spawn's thread: %v", err)
	}
	f.deliveredOnce(t, "remind "+token)
	nothingWaits(t, f.app)
}

func memberOf(members []store.DeleteMember, id string) bool {
	for _, member := range members {
		if member.ThreadID == id {
			return true
		}
	}
	return false
}

// A fire's run that tries the start again for its row's wake a failed start
// left queued waits for a delete's decision as a registered run: cancelling
// the loop ends the run and its wait, and nothing starts.
func TestAFireRunWaitingForADecisionEndsWithItsCancel(t *testing.T) {
	t.Parallel()
	f := newClaimFixture(t)
	f.app.agentWakes.floorOverride = 20 * time.Millisecond
	f.app.startSessionFn = func(string) error { return errors.New("the provider binary is missing") }
	loop := registerTestWake(t, f.app, store.AgentWake{ThreadID: f.caller.ID, Kind: store.AgentWakeLoop, Label: "board", Note: "board",
		IntervalSeconds: 3600, NextAt: time.Now().Add(time.Hour).UnixMilli()})
	if err := f.app.deliverAgentWake(t.Context(), loop, 1, testAgentWake(t, loop, "board fired")); err != nil {
		t.Fatal(err)
	}
	f.app.waitAgentNoticeWork()
	waitUntil(t, 10*time.Second, func() bool { return f.app.agentNoticeRetryDue(f.caller.ID) })
	f.app.startSessionFn = nil
	running := func() bool {
		f.app.agentWakes.mu.Lock()
		defer f.app.agentWakes.mu.Unlock()
		_, ok := f.app.agentWakes.runs[loop.ID]
		return ok
	}
	refusedDelete(t, f.app, f.caller.ID, func() {
		now := time.Now()
		if err := f.app.fireAgentWake(loop, now, agentWakeFire{at: now.UnixMilli()}); err != nil {
			t.Fatal("the fire did not start")
		}
		waitAtGates(t, f.app, 1)
		if !running() {
			t.Fatal("the run waiting for the decision is not registered")
		}
		if _, cancelled, err := f.app.cancelAgentWake(f.caller.ID, loop.Label, loop.Kind); err != nil || !cancelled {
			t.Fatalf("cancel the loop = %v, %v", cancelled, err)
		}
		waitUntil(t, 10*time.Second, func() bool { return !running() })
		waitAtGates(t, f.app, 0)
	})
	if _, live := f.app.sessionManager().get(f.caller.ID); live {
		t.Fatal("the cancelled run's start went ahead")
	}
}

// A fire's run whose start for its row's held wake waits for a delete that
// then commits ends with nothing started and nothing recorded: the failure
// streak an earlier start left is not extended, and no notification or
// error row reports the deleted thread. Knowing the delete committed, the
// start does not go back for the thread's lock.
func TestAFireRunWhoseDeleteCommittedEndsSilently(t *testing.T) {
	t.Parallel()
	f := newClaimFixture(t)
	f.app.agentWakes.floorOverride = 20 * time.Millisecond
	f.app.startSessionFn = func(string) error { return errors.New("the provider binary is missing") }
	loop := registerTestWake(t, f.app, store.AgentWake{ThreadID: f.caller.ID, Kind: store.AgentWakeLoop, Label: "board", Note: "board",
		IntervalSeconds: 3600, NextAt: time.Now().Add(time.Hour).UnixMilli()})
	if err := f.app.deliverAgentWake(t.Context(), loop, 1, testAgentWake(t, loop, "board fired")); err != nil {
		t.Fatal(err)
	}
	f.app.waitAgentNoticeWork()
	waitUntil(t, 10*time.Second, func() bool { return f.app.agentNoticeRetryDue(f.caller.ID) })
	f.app.startSessionFn = nil
	f.app.notifications.queue.Wait()
	notified := &recordingNotificationSender{}
	f.app.osNotifications = notified
	streak := func() (time.Time, bool) {
		f.app.agentNoticeWork.mu.Lock()
		defer f.app.agentNoticeWork.mu.Unlock()
		at, ok := f.app.agentNoticeWork.failing[f.caller.ID]
		return at, ok
	}
	failedAt, ok := streak()
	if !ok {
		t.Fatal("the failed start left no streak")
	}
	var run *agentWakeRun
	hook, held, proceed := holdOnce()
	f.app.deleteDecisionMadeForTest = hook
	committedDelete(t, f.app, f.caller.ID, func() {
		now := time.Now()
		if err := f.app.fireAgentWake(loop, now, agentWakeFire{at: now.UnixMilli()}); err != nil {
			t.Fatal("the fire did not start")
		}
		waitAtGates(t, f.app, 1)
		f.app.agentWakes.mu.Lock()
		run = f.app.agentWakes.runs[loop.ID]
		f.app.agentWakes.mu.Unlock()
		if run == nil {
			t.Fatal("the run waiting for the decision is not registered")
		}
	})
	within(t, "the fire's start held", held)
	if _, err := f.app.store.GetThread(f.caller.ID); err == nil {
		t.Fatal("the deleted thread's row is still there")
	}
	unlock := f.app.threadLocks().Lock(f.caller.ID)
	close(proceed)
	select {
	case <-run.done:
	case <-time.After(5 * time.Second):
		unlock()
		<-run.done
		t.Fatal("the fire's start went back for the thread's lock after its delete committed")
	}
	unlock()
	nothingWaits(t, f.app)
	f.app.notifications.queue.Wait()
	if at, ok := streak(); !ok || !at.Equal(failedAt) {
		t.Errorf("the failure streak = %v (%v), want it as the earlier start left it (%v)", at, ok, failedAt)
	}
	if sends := notified.snapshot(); len(sends) != 0 {
		t.Errorf("notifications = %+v, want none", sends)
	}
	if rows := threadErrorRows(t, f.app.store, f.caller.ID, ""); len(rows) != 0 {
		t.Errorf("error rows of the deleted thread = %q, want none", rows)
	}
	f.app.unsavedRows.mu.Lock()
	unsaved := f.app.unsavedRows.threads[f.caller.ID]
	f.app.unsavedRows.mu.Unlock()
	if unsaved != nil {
		t.Errorf("unsaved rows = %+v, want none", unsaved.Rows)
	}
	if _, live := f.app.sessionManager().get(f.caller.ID); live {
		t.Error("the deleted thread started")
	}
}

// S r6 finding 1 and A r4 finding 1: a release that lands after a dispatch
// read the claim and before it reached the gate is not missed. The dispatch
// finds the gate shut and the claim gone, and sends the batch.
func TestAReleaseBeforeTheBatchReachesTheGateIsNotMissed(t *testing.T) {
	t.Parallel()
	f := newClaimFixture(t)
	if err := f.app.startSessionTakingLock(context.Background(), f.caller.ID); err != nil {
		t.Fatal(err)
	}
	marker := "early-release-" + entityid.New()
	hook, at, proceed := holdOnce()
	f.app.deleteDecisionReadForTest = hook
	refusedDelete(t, f.app, f.caller.ID, func() {
		queueTestWake(t, f.app, f.caller.ID, marker)
		within(t, "the dispatch at the gate", at)
	})
	close(proceed)
	f.deliveredOnce(t, marker)
	nothingWaits(t, f.app)
}

// The boot releases every claim the process before it left undecided before
// anything starts (settlePriorInstance), so no claim stands without a delete
// to decide it, and a notice start after the boot starts the thread.
func TestTheBootLeavesNoClaimWithoutItsDelete(t *testing.T) {
	t.Parallel()
	f := newClaimFixture(t)
	if _, err := f.app.store.ClaimDeleteFamily("quit-op", []string{f.caller.ID}); err != nil {
		t.Fatal(err)
	}
	after := f.restart(t)
	after.app.configureTriageQueueCallbacks()
	after.app.settlePriorInstance()
	if claim, committed, err := after.app.store.ThreadDeleteClaim(f.caller.ID); err != nil || claim != "" || committed {
		t.Fatalf("the claim after the boot = %q committed %v (%v), want none", claim, committed, err)
	}
	after.holdCallerSession()
	if !after.app.startAgentNoticeSession(t.Context(), agentNotice{threadID: f.caller.ID, sendID: "wake:after-boot"}, false) {
		t.Fatal("a notice start after the boot did not start the thread")
	}
	nothingWaits(t, after.app)
}

// A claim no delete under way holds, which only a boot whose release failed
// leaves, ends a start that meets it rather than leaving it waiting or
// spinning: nothing is sent or started, no failure is recorded, and the
// message stays queued for the next boot.
func TestAClaimNoDeleteDecidesEndsTheStart(t *testing.T) {
	t.Parallel()
	f := newClaimFixture(t)
	if err := f.app.startSessionTakingLock(context.Background(), f.caller.ID); err != nil {
		t.Fatal(err)
	}
	worker := f.plainThread(t)
	if _, err := f.app.store.ClaimDeleteFamily("quit-op", []string{f.caller.ID, worker.ID}); err != nil {
		t.Fatal(err)
	}
	queueTestWake(t, f.app, f.caller.ID, "batch-"+entityid.New())
	token := "remind-" + entityid.New()
	insertTestReminder(t, f.app, token, worker.ID, "remind "+token, time.Now().Add(-time.Second).UnixMilli())
	f.app.fireDueThreadReminders(time.Now())
	awaitQueuedWake(t, f.app, worker.ID, threadWakeSendID(token))
	nothingWaits(t, f.app)
	if n := len(settledUserEnvelopes(t, f.stdin)); n != 0 {
		t.Fatalf("the provider was written %d messages", n)
	}
	f.app.agentNoticeWork.mu.Lock()
	failing := len(f.app.agentNoticeWork.failing)
	f.app.agentNoticeWork.mu.Unlock()
	if failing != 0 {
		t.Fatalf("%d failed starts recorded, want none", failing)
	}
	if len(durableQueueRows(t, f.app, f.caller.ID)) != 1 || len(durableQueueRows(t, f.app, worker.ID)) != 1 {
		t.Fatal("a message left the durable queue")
	}
}

// A delete whose claim fails ends its decision: nothing is left waiting for
// one, and the next delete of the thread goes ahead.
func TestAClaimThatFailsShutsTheGatesItOpened(t *testing.T) {
	t.Parallel()
	f := newClaimFixture(t)
	execOnStore(t, f.dbPath, `CREATE TRIGGER refuse_claim BEFORE UPDATE OF delete_claim ON threads BEGIN SELECT RAISE(ABORT, 'injected claim failure'); END`)
	if _, err := f.app.DeleteThread(f.caller.ID); err == nil || !strings.Contains(err.Error(), "injected claim failure") {
		t.Fatalf("the delete = %v, want the claim's failure", err)
	}
	execOnStore(t, f.dbPath, `DROP TRIGGER refuse_claim`)
	f.app.deleteDecisions.mu.Lock()
	open := len(f.app.deleteDecisions.ops)
	f.app.deleteDecisions.mu.Unlock()
	if open != 0 {
		t.Fatalf("%d decisions left open by the failed claim", open)
	}
	if result, err := f.app.DeleteThread(f.caller.ID); err != nil || len(result.WorkersNotDeleted) > 0 {
		t.Fatalf("the next delete = %+v, %v", result, err)
	}
}

// A teardown gives a moved wake back holding the torn-down thread's action
// lock, and a delete of that thread and the wake's verifies it under that
// lock. The start the wake waits on, the retry of a failed start that left
// the row's earlier wake queued, runs as notice work holding no lock, so
// the teardown and the delete both finish: a refused delete's release
// delivers the wake once, and a committed delete's teardown drops it.
func TestATeardownGivingAWakeBackDoesNotWaitForADeleteThatNeedsItsLock(t *testing.T) {
	t.Parallel()
	for name, refuse := range map[string]bool{"refused": true, "committed": false} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newClaimFixture(t)
			f.app.agentWakes.floorOverride = 20 * time.Millisecond
			marker := "moved-" + entityid.New()
			old, loop := handOffToCaller(t, f, store.AgentWake{Kind: store.AgentWakeLoop, Label: "board", Note: "board"}, marker)
			f.app.startSessionFn = func(string) error { return errors.New("the provider binary is missing") }
			row := wakeRow(t, f.app, loop.ID)
			if row.ThreadID != f.caller.ID {
				t.Fatalf("the loop is %s's, want the successor's", row.ThreadID)
			}
			if err := f.app.deliverAgentWake(t.Context(), row, 1, testAgentWake(t, row, "board fired")); err != nil {
				t.Fatal(err)
			}
			f.app.waitAgentNoticeWork()
			waitUntil(t, 10*time.Second, func() bool { return f.app.agentNoticeRetryDue(f.caller.ID) })
			f.app.startSessionFn = nil

			unlockOld := f.app.threadLocks().Lock(old)
			claimed, decide := make(chan struct{}), make(chan struct{})
			f.app.deleteClaimedForTest = func([]string) { close(claimed) }
			done := make(chan error, 1)
			go func() {
				_, err := f.app.deleteThreads(deleteRequest{seeds: []string{f.caller.ID, old}, verified: func([]store.DeleteMember) error {
					<-decide
					if refuse {
						return errRefusedForTest
					}
					return nil
				}})
				done <- err
			}()
			within(t, "the claim", claimed)
			f.app.deleteClaimedForTest = nil
			torn := make(chan error, 1)
			go func() {
				defer unlockOld()
				torn <- f.app.teardownAndCloseSession(old, session{})
			}()
			select {
			case err := <-torn:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(5 * time.Second):
				// The start ends finding no delete to decide the claim, and the rest follows.
				abandonGates(f.app)
				<-torn
				close(decide)
				<-done
				t.Fatal("the teardown and the delete wait on each other")
			}
			// The retried start waits for the decision on its own.
			waitAtGates(t, f.app, 1)
			close(decide)
			decided(t, done, refuse)
			if refuse {
				f.deliveredOnce(t, marker)
			} else {
				f.settle(t)
				if n := f.sent(t, marker); n != 0 {
					t.Fatalf("the provider was written the moved wake %d times", n)
				}
				if _, live := f.app.sessionManager().get(f.caller.ID); live {
					t.Fatal("the committed thread started")
				}
				if ids := queuedSendIDsOf(t, f.app, f.caller.ID, agentWakeSendIDPrefix(loop.ID)); len(ids) != 0 {
					t.Fatalf("the committed thread still queues %v", ids)
				}
			}
			nothingWaits(t, f.app)
		})
	}
}

// S r9 finding 2: a start that waited for a delete which then committed ends
// with nothing recorded and nothing started, even when the delete dropped
// the thread's row before the start went on: no failure streak and no
// notification. So whether the start is held once the decision is made, or
// before it looks for the decision at all.
func TestAStartWhoseDeleteCommittedEndsSilentlyOnceTheRowIsGone(t *testing.T) {
	t.Parallel()
	for _, at := range []string{"once the decision is made", "before it looks for the decision"} {
		t.Run(at, func(t *testing.T) {
			t.Parallel()
			f := newClaimFixture(t)
			notified := &recordingNotificationSender{}
			f.app.osNotifications = notified
			f.app.storeIdentity.Store(&store.Identity{BackendID: "backend-under-test"})
			hook, held, proceed := holdOnce()
			if at == "once the decision is made" {
				f.app.deleteDecisionMadeForTest = hook
			} else {
				f.app.deleteDecisionReadForTest = hook
			}
			token := "remind-" + entityid.New()
			committedDelete(t, f.app, f.caller.ID, func() {
				insertTestReminder(t, f.app, token, f.caller.ID, "remind "+token, time.Now().Add(-time.Second).UnixMilli())
				f.app.fireDueThreadReminders(time.Now())
				if at == "once the decision is made" {
					waitAtGates(t, f.app, 1)
				} else {
					within(t, "the start at the claim", held)
				}
			})
			within(t, "the start held", held)
			if _, err := f.app.store.GetThread(f.caller.ID); err == nil {
				t.Fatal("the deleted thread's row is still there")
			}
			unlock := func() {}
			if at == "once the decision is made" {
				// A start that knows the delete committed does not come back
				// for the thread's lock.
				unlock = f.app.threadLocks().Lock(f.caller.ID)
			}
			close(proceed)
			done := make(chan struct{})
			go func() {
				f.app.waitAgentNoticeWork()
				close(done)
			}()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				unlock()
				<-done
				t.Fatal("the start went back for the thread's lock after its delete committed")
			}
			unlock()
			nothingWaits(t, f.app)
			f.app.notifications.queue.Wait()
			f.app.agentNoticeWork.mu.Lock()
			failing := len(f.app.agentNoticeWork.failing)
			f.app.agentNoticeWork.mu.Unlock()
			if failing != 0 {
				t.Errorf("%d failed starts recorded for the deleted thread", failing)
			}
			if sends := notified.snapshot(); len(sends) != 0 {
				t.Errorf("notifications = %+v, want none", sends)
			}
			if _, live := f.app.sessionManager().get(f.caller.ID); live {
				t.Error("the deleted thread started")
			}
		})
	}
}
