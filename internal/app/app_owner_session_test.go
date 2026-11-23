package app

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"agent-overflow/internal/identity"
	"agent-overflow/internal/store"
	"agent-overflow/internal/transport"
)

func TestNewPromptReachesItsOwnerWhileAnOrdinaryReadIsHeld(t *testing.T) {
	var server observedOwnerPeer
	pair := newReachPairServedBy(t, func(t *testing.T, a *App) *pairedBackend { return observeOwnerPeer(t, a, &server) })
	p := newRemotePromptPairOn(t, pair)
	startReconciledOwner(t, pair)
	held, _ := holdOwnerReply(t, &server.holdState)
	owner := p.receipt(t, p.spawn.Token).OwnerDeviceID
	p.dest.ownerActivity.note(owner, p.spawn.Token, false)
	select {
	case <-held.arrived:
	case <-time.After(5 * time.Second):
		t.Fatal("ordinary read never arrived")
	}
	approvalOn(t, p.dest, p.worker, "priority")
	p.openThere(t, 1)
	waitUntil(t, 3*time.Second, func() bool { return len(remoteWakes(t, p.source, p.caller.ID)) == 1 })
}

// staleEditPeer holds its reply to the acknowledgment of one token's edit.
type staleEditPeer struct {
	observedOwnerPeer
	holdAck atomic.Pointer[heldOwnerReply]
	token   string
}

func (p *staleEditPeer) ThreadToolRequestStatus(ctx context.Context, poll ThreadPeerPoll) (ThreadPeerPollReply, error) {
	reply, err := p.observedOwnerPeer.ThreadToolRequestStatus(ctx, poll)
	if err != nil {
		return reply, err
	}
	for _, ack := range poll.StructuralAck {
		if ack.Token == p.token {
			if err := p.holdAck.Swap(nil).wait(ctx); err != nil {
				return ThreadPeerPollReply{}, err
			}
			break
		}
	}
	return reply, nil
}

// TestAnEditForARequestGoneHereIsAcknowledgedOffTheWait: a worker can
// outlive its request on the owner's computer. Its rename is acknowledged,
// so the other computer stops owing it, and a slow reply to that
// acknowledgment does not keep another worker's prompt from its owner.
func TestAnEditForARequestGoneHereIsAcknowledgedOffTheWait(t *testing.T) {
	var peer staleEditPeer
	pair := newReachPairServedBy(t, func(t *testing.T, a *App) *pairedBackend {
		peer.App = a
		return serveAppAs(t, a, &peer)
	})
	old, worker := remoteRestedWorker(t, pair)
	owner := pair.receipt(t, old.Token).OwnerDeviceID
	prompt := newRemotePromptPairOn(t, pair)
	startReconciledOwner(t, pair)
	execOnStore(t, pair.dbPath, "DELETE FROM thread_requests WHERE token=?", old.Token)

	peer.token = old.Token
	hold, release := holdOwnerReply(t, &peer.holdAck)
	defer release()
	if err := pair.dest.RenameThread(worker, "renamed after its request was pruned"); err != nil {
		t.Fatal(err)
	}
	within(t, "the acknowledgment of the stale edit", hold.arrived)
	approvalOn(t, peer.App, prompt.worker, "independent prompt")
	prompt.openThere(t, 1)
	waitUntil(t, 3*time.Second, func() bool { return len(remoteWakes(t, pair.source, pair.caller.ID)) == 1 })
	release()
	waitUntil(t, 5*time.Second, func() bool {
		pending, err := pair.dest.store.PendingSpawnRevisions(owner, "", 1)
		return err == nil && len(pending) == 0
	})
}

func TestIdleUnfollowedTargetCostsNoRowReads(t *testing.T) {
	p := newReachPair(t)
	a := p.dest
	a.threadRequestsWG.Wait()
	stop := readsUnder(a.store, "app.(*App).drainThreadAgentEmits")
	for range 8 {
		a.noteThreadAgentTarget("unfollowed")
		a.threadRequestsWG.Wait()
	}
	total, by := stop()
	if total != 0 {
		t.Fatalf("eight updates cost %d reads: %v", total, by)
	}
	watchOwner(t, &a.ownerActivity, "watching")
	stop = readsUnder(a.store, "app.(*App).drainThreadAgentEmits")
	a.noteThreadAgentTarget("unfollowed")
	a.threadRequestsWG.Wait()
	total, by = stop()
	if total != 1 || by["store.(*Store).ActivityOwners"] != 1 {
		t.Fatalf("with a listener, lookup cost %d: %v", total, by)
	}

}

func TestRevokingAWatchingDeviceDiscardsItsActivity(t *testing.T) {
	p := newReachPair(t)
	spawn, _ := remoteRestedWorker(t, p)
	owner := p.receipt(t, spawn.Token).OwnerDeviceID
	var answer OwnerChanges
	if err := p.manager.CallThreadPeerOn(t.Context(), p.computer, transport.CapabilityThreadToolsV2, "ThreadOwnerChanges", &answer, OwnerCursor{}); err != nil {
		t.Fatal(err)
	}
	feed := &p.dest.ownerActivity
	feed.mu.Lock()
	socket := feed.devices[owner].tie.Conn()
	feed.mu.Unlock()
	// Session revocation can return while the WebSocket handler is unwinding.
	allowCleanup := make(chan struct{})
	defer close(allowCleanup)
	if _, err := socket.Bind("hold-test-unwind", func() { <-allowCleanup }); err != nil {
		t.Fatalf("watching socket: %v", err)
	}
	if _, err := p.dest.RevokeAccessDevice(owner); err != nil {
		t.Fatal(err)
	}
	p.dest.ownerActivity.note(owner, spawn.Token, true)
	feed.mu.Lock()
	defer feed.mu.Unlock()
	if feed.devices[owner] != nil {
		t.Fatal("revoked device still receives activity")
	}
}

func TestOwnerRetriesAFailedReadOfAnOtherwiseRestingLink(t *testing.T) {
	var server observedOwnerPeer
	pair := newReachPairServedBy(t, func(t *testing.T, a *App) *pairedBackend { return observeOwnerPeer(t, a, &server) })
	spawn, _ := remoteRestedWorker(t, pair)
	pair.source.threadPollOverride = 50 * time.Millisecond
	startReconciledOwner(t, pair)
	// B has already pruned this receipt. Neither side's active-work listing
	// can rediscover the resting token after its one notification is consumed.
	token := newThreadRequestToken()
	if err := pair.source.store.InsertThreadRequest(store.ThreadRequest{Token: token, CallerThreadID: pair.caller.ID,
		Kind: store.ThreadRequestSpawn, TargetComputerID: pair.computer, TargetThreadID: "pruned-worker", State: store.ThreadRequestRunning}); err != nil {
		t.Fatal(err)
	}
	if _, err := pair.source.store.SettleThreadRequest(token, store.ThreadRequestOpenStates(), store.ThreadRequestSettlement{State: store.ThreadRequestFinished, Answer: []byte("done"), AnswerKind: store.ThreadAnswerFinal}); err != nil {
		t.Fatal(err)
	}
	if _, err := pair.source.store.RetireThreadRequestPoll(token); err != nil {
		t.Fatal(err)
	}
	failed := make(chan struct{})
	refuse := func(poll ThreadPeerPoll) error {
		for _, link := range poll.Links {
			if link.Token == token && server.failRead.Swap(nil) != nil {
				close(failed)
				return errors.New("status unavailable")
			}
		}
		return nil
	}
	server.failRead.Store(&refuse)
	pair.dest.ownerActivity.note(pair.receipt(t, spawn.Token).OwnerDeviceID, token, false)
	select {
	case <-failed:
	case <-time.After(5 * time.Second):
		t.Fatal("the state read never failed")
	}
	waitUntil(t, 5*time.Second, func() bool { return pair.request(t, token).LinkEnd == "gone" })

}

func TestOwnerResetRestoresConservativePlacementDuringRecovery(t *testing.T) {
	var server observedOwnerPeer
	pair := newReachPairServedBy(t, func(t *testing.T, a *App) *pairedBackend { return observeOwnerPeer(t, a, &server) })
	_, child := remoteRestedWorker(t, pair)
	if _, err := pair.dest.SetThreadPlacement([]string{child}, store.PlacementStandalone); err != nil {
		t.Fatal(err)
	}
	startReconciledOwner(t, pair)

	if remoteDeleteWarning(t, pair) {
		t.Fatal("standalone worker offered for deletion")
	}
	held, _ := holdOwnerReply(t, &server.holdPage)
	pair.dest.ownerActivity.reset()
	select {
	case <-held.arrived:
	case <-time.After(10 * time.Second):
		t.Fatal("reset did not reconcile")
	}
	if !remoteDeleteWarning(t, pair) {
		t.Fatal("old standalone placement suppressed the delete warning during recovery")
	}
}

func TestOwnerRetriesSupersededReadBeforeReportingSuccess(t *testing.T) {
	var server observedOwnerPeer
	p := newReachPairServedBy(t, func(t *testing.T, a *App) *pairedBackend { return observeOwnerPeer(t, a, &server) })
	spawn, _ := remoteRestedWorker(t, p)
	held, release := holdOwnerReply(t, &server.holdState)
	before := server.linkCalls.Load()
	done := make(chan error, 1)
	go func() {
		done <- p.source.readOwnerChanges(t.Context(), p.computer, p.destWire.srv.LaunchID(), []string{spawn.Token}, nil)
	}()
	select {
	case <-held.arrived:
	case <-time.After(5 * time.Second):
		t.Fatal("no read")
	}
	p.source.ownerReads.changed([]string{spawn.Token})
	release()
	if err := <-done; err != nil || server.linkCalls.Load() != before+2 {
		t.Fatalf("read completed after %d calls: %v", server.linkCalls.Load()-before, err)
	}
}

func TestOwnerWaitNeverReachesAPeerWithoutV2(t *testing.T) {
	p, server := newReleasedReceiverPair(t)
	installMockClaudeTurns(t, p.dest, [][]string{{mockClaudeInitLine}})
	p.spawnThere(t, "work", nil)
	if err := p.source.refreshOwnerPeers(); err != nil {
		t.Fatal(err)
	}
	o := &p.source.ownerUpdates
	o.mu.Lock()
	peer := o.peers[p.computer]
	o.mu.Unlock()
	if peer == nil {
		t.Fatal("no capability probe")
	}
	t.Cleanup(func() { peer.cancel(); <-peer.done })
	waitUntil(t, 5*time.Second, func() bool { o.mu.Lock(); defer o.mu.Unlock(); return peer.legacy || server.ownerWaitCalls.Load() > 0 })
	if got := server.ownerWaitCalls.Load(); got != 0 {
		t.Fatalf("v1 connection received %d owner waits", got)
	}
}

func TestReplacementCarrierOnSameLaunchReceivesAFreshCursor(t *testing.T) {
	var server observedOwnerPeer
	p := newReachPairServedBy(t, func(t *testing.T, a *App) *pairedBackend { return observeOwnerPeer(t, a, &server) })
	_, worker := remoteRestedWorker(t, p)
	if _, err := p.dest.SetThreadPlacement([]string{worker}, store.PlacementStandalone); err != nil {
		t.Fatal(err)
	}
	p.source.threadPollOverride = 50 * time.Millisecond
	arrived, release := make(chan struct{}), make(chan struct{})
	resume := sync.OnceFunc(func() { close(release) })
	var hold sync.Once
	p.source.beforeOwnerWaitForTest = func(ctx context.Context, c OwnerCursor) {
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) < transport.MethodBudget("ThreadOwnerChanges") {
			t.Errorf("client deadline %v gives the peer less than its declared budget", deadline)
		}

		if c.Launch != "" {
			hold.Do(func() {
				close(arrived)
				select {
				case <-release:
				case <-ctx.Done():
				}
			})
		}
	}
	t.Cleanup(resume)
	startReconciledOwner(t, p)
	// A failed assertion must release the wait before joining the owner.
	defer resume()
	select {
	case <-arrived:
	case <-time.After(5 * time.Second):
		t.Fatal("no incremental wait")
	}
	if remoteDeleteWarning(t, p) {
		t.Fatal("standalone placement missing before replacement")
	}
	p.rePair(t)
	if !remoteDeleteWarning(t, p) {
		t.Fatal("replacement retained old standalone placement while the wait was paused")
	}
	first := make(chan OwnerCursor, 1)
	observe := func(c OwnerCursor) {
		select {
		case first <- c:
		default:
		}
	}
	server.observeWait.Store(&observe)
	resume()
	select {
	case c := <-first:
		if c.Launch != "" {
			t.Fatalf("replacement received old cursor %+v", c)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("replacement did not reconcile")
	}
}

// ownerChangesConnectionTie is ThreadOwnerChanges' row of
// connectionTieChecks: the paired device's activity log, which follows the
// latest connection that asked for it and ends with that connection.
func ownerChangesConnectionTie(t *testing.T) connectionTie {
	a := identityApp(t)
	a.SetTransportServer(startTestTransportServer(t))
	session := pairSessionWithScopes(t, a, "owner-activity-tie", []identity.Scope{identity.ScopeThreadsRead})
	return connectionTie{
		open: func(ctx context.Context) (string, error) {
			_, err := a.ThreadOwnerChanges(ctx, OwnerCursor{})
			return "", err
		},
		held: func() int {
			a.ownerActivity.mu.Lock()
			defer a.ownerActivity.mu.Unlock()
			return len(a.ownerActivity.devices)
		},
		principal: transport.ConnPrincipal{SessionID: session.ID},
	}
}
