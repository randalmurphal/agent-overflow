package app

import (
	"context"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"agent-overflow/internal/store"
	"agent-overflow/internal/threadtools"
	"agent-overflow/internal/transport"
)

type observedOwnerPeer struct {
	observeWait atomic.Pointer[func(OwnerCursor)]
	*App
	holdPage  atomic.Pointer[heldOwnerReply]
	failRead  atomic.Pointer[func(ThreadPeerPoll) error]
	linkCalls atomic.Int64
	pages     atomic.Int64
	batches   atomic.Int64
	contacts  atomic.Pointer[[]OwnerLinkRead]
	holdState atomic.Pointer[heldOwnerReply]
	holdPane  atomic.Pointer[heldOwnerReply]
}

func (p *observedOwnerPeer) ThreadOwnerChanges(ctx context.Context, cursor OwnerCursor) (OwnerChanges, error) {
	if observe := p.observeWait.Load(); observe != nil {
		(*observe)(cursor)
	}
	return p.App.ThreadOwnerChanges(ctx, cursor)
}

func (p *observedOwnerPeer) ThreadOwnerRecovery(ctx context.Context, launch string, key OwnerPageKey) (OwnerRecoveryPage, error) {
	p.pages.Add(1)
	if err := p.holdPage.Swap(nil).wait(ctx); err != nil {
		return OwnerRecoveryPage{}, err
	}
	return p.App.ThreadOwnerRecovery(ctx, launch, key)
}

func (p *observedOwnerPeer) ThreadToolRequestStatus(ctx context.Context, poll ThreadPeerPoll) (ThreadPeerPollReply, error) {
	p.batches.Add(1)
	if len(poll.Links) > 0 {
		p.linkCalls.Add(1)
		if fail := p.failRead.Load(); fail != nil {
			if err := (*fail)(poll); err != nil {
				return ThreadPeerPollReply{}, err
			}
		}
	}
	if len(poll.Contacts) > 0 {
		p.contacts.Store(&poll.Contacts)
	}
	reply, err := p.App.ThreadToolRequestStatus(ctx, poll)
	if len(poll.Links) > 0 && err == nil {
		err = p.holdState.Swap(nil).wait(ctx)
	}
	return reply, err
}

type heldOwnerReply struct{ arrived, release chan struct{} }

func (h *heldOwnerReply) wait(ctx context.Context) error {
	if h == nil {
		return nil
	}
	close(h.arrived)
	select {
	case <-h.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func holdOwnerReply(t *testing.T, slot *atomic.Pointer[heldOwnerReply]) (*heldOwnerReply, func()) {
	t.Helper()
	held := &heldOwnerReply{arrived: make(chan struct{}), release: make(chan struct{})}
	slot.Store(held)
	release := sync.OnceFunc(func() { close(held.release) })
	t.Cleanup(release)
	return held, release
}

func (p *observedOwnerPeer) ThreadToolQuery(ctx context.Context, call ThreadPeerCall) (ThreadPeerReply, error) {
	reply, err := p.App.ThreadToolQuery(ctx, call)
	if call.Tool == threadAgentPeerRows && err == nil {
		err = p.holdPane.Swap(nil).wait(ctx)
	}
	return reply, err
}

func observeOwnerPeer(t *testing.T, app *App, peer *observedOwnerPeer) *pairedBackend {
	peer.App = app
	return servePairedApp(t, app, func(cfg *transport.Config) {
		dispatcher := transport.NewDispatcher()
		if _, err := dispatcher.Register(peer, transport.RegisterOptions{Package: "main", TypeName: "App", AllowList: transport.NewMethodAllowList()}); err != nil {
			t.Fatal(err)
		}
		cfg.Dispatcher = dispatcher
	})
}

func TestRecoveryPagesEveryActiveTokenUnderAnArchivedParent(t *testing.T) {
	pair := newAgentPeerPair(t, func(t *testing.T, app *App) *pairedBackend { return servePairedApp(t, app) })
	child := pair.receipt(t, pair.grand).TargetThreadID
	if _, err := pair.dest.store.SetThreadPlacement([]string{child}, store.PlacementStandalone); err != nil {
		t.Fatal(err)
	}
	if _, err := pair.dest.ArchiveThread(pair.child); err != nil {
		t.Fatal(err)
	}
	owner := pair.receipt(t, pair.spawn.Token).OwnerDeviceID
	var tokens []string
	for range ownerPageSize + 1 {
		key := newThreadRequestToken()
		_, _, err := pair.dest.store.AcceptThreadRequestReceipt(store.ThreadRequestReceipt{Token: key, OwnerDeviceID: owner, SourceThreadID: pair.caller.ID, Kind: store.ThreadRequestSend, TargetThreadID: child})
		if err != nil {
			t.Fatal(err)
		}
		if err := pair.source.store.InsertThreadRequest(store.ThreadRequest{Token: key, CallerThreadID: pair.caller.ID, TargetComputerID: pair.computer, Kind: store.ThreadRequestSend, TargetThreadID: child, State: store.ThreadRequestUnconfirmed, Polling: true}); err != nil {
			t.Fatal(err)
		}
		tokens = append(tokens, key)
	}
	// The local target remains working independently of its archived spawner.
	if _, err := pair.dest.reassessWorker(child, true); err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for key := (&OwnerPageKey{Phase: "active"}); key != nil && key.Phase == "active"; {
		var page OwnerRecoveryPage
		err := pair.manager.CallThreadPeerOn(t.Context(), pair.computer, transport.CapabilityThreadToolsV2, "ThreadOwnerRecovery", &page, pair.destWire.srv.LaunchID(), *key)
		if err != nil {
			t.Fatal(err)
		}
		if len(page.Tokens) > ownerPageSize {
			t.Fatalf("received %d tokens in one recovery page", len(page.Tokens))
		}
		for _, token := range page.Tokens {
			seen[token] = true
		}
		key = page.Next
	}
	if err := pair.source.recoverOwnerPeer(t.Context(), pair.computer, pair.destWire.srv.LaunchID()); err != nil {
		t.Fatal(err)
	}
	for _, key := range tokens {
		if state := pair.request(t, key).State; state != store.ThreadRequestAccepted {
			t.Fatalf("recovery omitted %s: %s", key, state)
		}
	}
	tokens = append(tokens, pair.spawn.Token)
	for _, token := range tokens {
		if !seen[token] {
			t.Fatalf("active token %s missing out of %d under the archived parent", token, len(seen))
		}
	}
}

func TestOwnerRecoveryWireCallsDoNotGrowWithSettledHistory(t *testing.T) {
	var server observedOwnerPeer
	pair := newReachPairServedBy(t, func(t *testing.T, app *App) *pairedBackend { return observeOwnerPeer(t, app, &server) })
	spawn, _ := remoteRestedWorker(t, pair)
	start := func(from, to int) []int64 {
		t.Helper()
		execOnStore(t, pair.dbPath, `WITH RECURSIVE n(x) AS (SELECT ?1 UNION ALL SELECT x+1 FROM n WHERE x<?2)
		INSERT INTO thread_requests(token,caller_thread_id,kind,target_computer_id,target_thread_id,state,polling,settled_at,created_at,updated_at,contact_due)
		SELECT 'old-'||x,?3,'spawn',?4,'resting-'||x,'finished',1,1,1,1,9223372036854775807 FROM n`, from, to, pair.caller.ID, pair.computer)
		server.pages.Store(0)
		server.batches.Store(0)
		if err := pair.source.recoverOwnerPeer(t.Context(), pair.computer, pair.destWire.srv.LaunchID()); err != nil {
			t.Fatal(err)
		}
		if err := pair.source.readOwnerChanges(t.Context(), pair.computer, pair.destWire.srv.LaunchID(), []string{spawn.Token}, nil); err != nil {
			t.Fatal(err)
		}
		return []int64{server.pages.Load(), server.batches.Load()}
	}
	// The initial reconciliation consumes the structural revision of the fixture's spawn.
	if err := pair.source.recoverOwnerPeer(t.Context(), pair.computer, pair.destWire.srv.LaunchID()); err != nil {
		t.Fatal(err)
	}
	ten := start(1, 10)
	many := start(11, 10000)
	if ten[0] == 0 || ten[1] == 0 || !slices.Equal(ten, many) {
		t.Fatalf("peer recovery pages and status calls with 10 and 10,000 resting links = %v and %v", ten, many)
	}
	t.Logf("recovery pages and status calls for both histories: %v", ten)
}

func TestReadingAContinuingGrandchildDoesNotAdvanceTheOwnersClock(t *testing.T) {
	pair := newAgentPeerPair(t, func(t *testing.T, app *App) *pairedBackend { return servePairedApp(t, app) })
	owner := pair.receipt(t, pair.spawn.Token).OwnerDeviceID
	launch := pair.destWire.srv.LaunchID()
	for _, thread := range []string{pair.child, pair.receipt(t, pair.grand).TargetThreadID} {
		waitUntil(t, 10*time.Second, func() bool {
			items, err := pair.dest.store.ListItems(thread)
			if err != nil {
				t.Fatal(err)
			}
			for _, item := range items {
				if item.Summary == "working" {
					return true
				}
			}
			return false
		})
	}
	pair.dest.threadRequestsWG.Wait()
	d := watchOwner(t, &pair.dest.ownerActivity, owner)
	first, _ := pair.dest.ownerActivity.snapshot(d, launch, OwnerCursor{})
	for range 3 {
		list, _, err := pair.source.peerThreadAgentRows(t.Context(), pair.spawn, threadAgentRowDepth-1)
		if _, ok := agentRowByToken(list.Rows, pair.grand); err != nil || !ok {
			t.Fatalf("live grandchild listing: %+v, %v", list, err)
		}
	}
	pair.dest.threadRequestsWG.Wait()
	next, _ := pair.dest.ownerActivity.snapshot(d, launch, first.OwnerCursor)
	if next.Reset || next.Version != first.Version {
		t.Fatalf("reading a running grandchild advanced its owner's clock from %d to %+v", first.Version, next)
	}
}

func TestOwnerReadsTheEditMadeDuringAStateReadAndRejectsAnOlderPane(t *testing.T) {
	var server observedOwnerPeer
	pair := newReachPairServedBy(t, func(t *testing.T, app *App) *pairedBackend { return observeOwnerPeer(t, app, &server) })
	spawn, child := remoteRestedWorker(t, pair)
	startReconciledOwner(t, pair)
	held, release := holdOwnerReply(t, &server.holdState)
	if err := pair.dest.RenameThread(child, "first edit"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	select {
	case <-held.arrived:
	case <-ctx.Done():
		t.Fatal("owner did not read first edit")
	}
	if err := pair.dest.RenameThread(child, "second edit"); err != nil {
		t.Fatal(err)
	}
	release()
	waitUntil(t, 10*time.Second, func() bool { return pair.request(t, spawn.Token).TargetThreadTitle == "second edit" })
	if _, err := pair.dest.SetThreadPlacement([]string{child}, store.PlacementStandalone); err != nil {
		t.Fatal(err)
	}
	offered := func() bool {
		deps, err := pair.source.GetDeleteDependents([]string{pair.caller.ID})
		if err != nil {
			t.Fatal(err)
		}
		return deps.RemoteWorkers
	}
	waitUntil(t, 10*time.Second, func() bool { return !offered() })
	pane, releasePane := holdOwnerReply(t, &server.holdPane)
	done := make(chan threadAgentPeerAnswer, 1)
	go func() { done <- pair.source.readPeerAgentRows(ctx, spawn, 1) }()
	select {
	case <-pane.arrived:
	case <-ctx.Done():
		t.Fatal("pane did not request its listing")
	}
	if _, err := pair.dest.SetThreadPlacement([]string{child}, store.PlacementOwned); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, 10*time.Second, offered)
	releasePane()
	if answer := <-done; answer.err != nil || answer.listing.Placement != store.PlacementOwned || !offered() {
		t.Fatalf("delayed pane restored the old placement: %q, %v", answer.listing.Placement, answer.err)
	}
}

func TestLateSendReplyWrittenDuringSleepIsCollectedByTheFinalCheck(t *testing.T) {
	pair := newReachPair(t)
	target, err := createTestThread(t, pair.dest, pair.caller.Provider, t.TempDir(), pair.caller.Model, pair.caller.Mode)
	if err != nil {
		t.Fatal(err)
	}
	installMockClaudeTurns(t, pair.dest, [][]string{{mockClaudeInitLine}})
	installMockClaudeReplies(t, pair.source, "read the correction")
	sent, err := pair.adapter().Send(t.Context(), pair.callerIdentity(), threadtools.SendCall{ThreadID: target.ID, ComputerID: pair.computer, Message: "check the build"})
	if err != nil {
		t.Fatal(err)
	}
	finished, err := pair.dest.store.SettleLocalThreadRequest(sent.Token, store.ThreadReceiptOpenStates(),
		store.ThreadRequestSettlement{State: store.ThreadReceiptFinished, Answer: []byte("done"), AnswerKind: store.ThreadAnswerFinal}, store.ThreadRequestFinished)
	if err != nil || !finished.Settled {
		t.Fatalf("settle send: %+v, %v", finished, err)
	}
	pair.poll(t)
	row := pair.request(t, sent.Token)
	if !row.Polling || row.State != store.ThreadRequestFinished || row.ExpiresAt == 0 {
		t.Fatalf("no late-answer obligation: %+v", row)
	}
	at := row.ExpiresAt - 1
	if yes, err := pair.dest.store.StoreThreadReceiptLateReply(sent.Token, []byte("one test failed"), at, nil); !yes || err != nil {
		t.Fatalf("late reply: %v, %v", yes, err)
	}
	clock := time.UnixMilli(row.ExpiresAt + 1)
	due, err := pair.source.store.DueThreadRequestPolls(clock.UnixMilli(), 32, []string{pair.computer})
	if err != nil || len(due) != 1 || due[0].Token != sent.Token {
		t.Fatalf("first clock after sleep lists %+v, %v", due, err)
	}
	pair.source.pollThreadRequestsOn(clock, pair.computer, due, nil)
	collected := pair.request(t, sent.Token)
	if collected.Polling || string(collected.Answer) != "done" || string(collected.LateReply) != "one test failed" || pair.receipt(t, sent.Token).CollectedRevision != collected.Revision {
		t.Fatalf("final status failed to collect and acknowledge both answers: %+v", collected)
	}
}

func TestDueOwnerContactUsesTheOriginalSpawnerAndStopsAfterRelease(t *testing.T) {
	var server observedOwnerPeer
	pair := newReachPairServedBy(t, func(t *testing.T, app *App) *pairedBackend { return observeOwnerPeer(t, app, &server) })
	spawn, _ := remoteRestedWorker(t, pair)
	testAssistant(t, pair.source, pair.caller.ID)
	successor := handOffOn(t, pair.source, pair.caller.ID)
	if row := pair.request(t, spawn.Token); row.CallerThreadID != successor.ID || row.MadeIn() != pair.caller.ID {
		t.Fatalf("after handoff: %+v", row)
	}
	now := time.Now()
	contact := func(at time.Time) {
		t.Helper()
		if err := pair.source.proveDueLinks(t.Context(), pair.computer, pair.destWire.srv.LaunchID(), at); err != nil {
			t.Fatal(err)
		}
	}
	server.batches.Store(0)
	contact(now)
	links := server.contacts.Load()
	if links == nil || len(*links) != 1 || (*links)[0].Source != pair.caller.ID || (*links)[0].Token != spawn.Token {
		t.Fatalf("contact after handoff = %+v", links)
	}
	contact(now.Add(23 * time.Hour))
	if got := server.batches.Load(); got != 1 {
		t.Fatalf("%d contacts before next due time", got)
	}
	contact(now.Add(24 * time.Hour))
	if _, err := pair.source.store.ApplySpawnRevision(pair.computer, store.SpawnRevision{SpawnRevisionAck: store.SpawnRevisionAck{Token: spawn.Token}, End: "released"}); err != nil {
		t.Fatal(err)
	}
	contact(now.Add(72 * time.Hour))
	if got := server.batches.Load(); got != 2 {
		t.Fatalf("%d contacts including a released link", got)
	}
}

// Ordinary cache aging cannot refresh this tree. A change under a worker
// that remains open has to make the pane ask for its detail again.
func TestOwnerPaneReadsChangedDescendantsBeforeTheCachedTreeAges(t *testing.T) {
	c := newAgentChain(t, 3)
	for _, i := range []int{1, 2} {
		routeWorkerPromptsOn(c.apps[i], c.buses[i])
	}
	installChainLink(t, c.apps[1])
	installChainLink(t, c.apps[2])
	c.spawnAlong(t, 0, "watch the release")
	c.spawnAlong(t, 1, "stage the release")
	for i := 1; i >= 0; i-- {
		startReconciledComputer(t, c.apps[i], c.computers[i])
	}
	grandchild := func() (ThreadAgentRow, bool) {
		rows, err := c.apps[0].ListThreadAgentRows(t.Context(), c.caller.ID)
		if err != nil {
			t.Fatal(err)
		}
		return agentRowByToken(rows.Rows, c.spawns[1])
	}
	waitUntil(t, 10*time.Second, func() bool { row, ok := grandchild(); return ok && row.Worker == workerRunning })
	cache := &c.apps[0].threadAgentPeers
	cache.mu.Lock()
	cache.entries[c.spawns[0]].at = time.Now().Add(time.Hour)
	cache.mu.Unlock()
	approvalOn(t, c.apps[2], c.threads[1], "release-approval")
	waitUntil(t, 10*time.Second, func() bool { row, ok := grandchild(); return ok && row.Worker == workerBlocked })

}
