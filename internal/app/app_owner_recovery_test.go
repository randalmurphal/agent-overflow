package app

import (
	"context"
	"slices"
	"sync/atomic"
	"testing"

	"agent-overflow/internal/store"
	"agent-overflow/internal/transport"
)

type observedOwnerPeer struct {
	*App
	pages   atomic.Int64
	batches atomic.Int64
}

func (p *observedOwnerPeer) ThreadOwnerRecovery(ctx context.Context, launch string, key OwnerPageKey) (OwnerRecoveryPage, error) {
	p.pages.Add(1)
	return p.App.ThreadOwnerRecovery(ctx, launch, key)
}

func (p *observedOwnerPeer) ThreadToolRequestStatus(ctx context.Context, poll ThreadPeerPoll) (ThreadPeerPollReply, error) {
	p.batches.Add(1)
	return p.App.ThreadToolRequestStatus(ctx, poll)
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
	for range ownerChangeLimit + 1 {
		key := newThreadRequestToken()
		_, _, err := pair.dest.store.AcceptThreadRequestReceipt(store.ThreadRequestReceipt{Token: key, OwnerDeviceID: owner, SourceThreadID: pair.caller.ID, Kind: store.ThreadRequestSend, TargetThreadID: child})
		if err != nil {
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
		SELECT 'old-'||x,?3,'spawn',?4,'resting-'||x,'finished',0,1,1,1,9223372036854775807 FROM n`, from, to, pair.caller.ID, pair.computer)
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
