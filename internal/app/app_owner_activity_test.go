package app

import (
	"slices"
	"strconv"
	"testing"
	"time"

	"agent-overflow/internal/store"
	"agent-overflow/internal/transport"
)

func watchOwner(t *testing.T, feed *ownerActivity, id string) *deviceActivity {
	t.Helper()
	feed.mu.Lock()
	defer feed.mu.Unlock()
	d, _, err := feed.watchLocked(id, &transport.ConnState{})
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func TestOwnerActivityKeepsChangesUntilObserved(t *testing.T) {
	var events ownerActivity
	desk := watchOwner(t, &events, "desk")
	first, wait := events.snapshot(desk, "launch", OwnerCursor{Launch: "launch"})
	events.note("other", "hidden", true)
	select {
	case <-wait:
		t.Fatal("another device's change woke this owner")
	default:
	}
	events.note("desk", "spawn", true)
	<-wait
	events.note("desk", "spawn", false)
	seen, _ := events.snapshot(desk, "launch", first.OwnerCursor)
	if seen.Reset || !slices.Equal(seen.Tokens, []string{"spawn"}) || !slices.Equal(seen.PromptTokens, []string{"spawn"}) {
		t.Fatalf("prompt followed by ordinary activity = %+v", seen)
	}
	events.note("desk", "spawn", false)
	newer, _ := events.snapshot(desk, "launch", seen.OwnerCursor)
	if newer.Version <= seen.Version || !slices.Equal(newer.Tokens, seen.Tokens) {
		t.Fatalf("change while owner read version %d = %+v", seen.Version, newer)
	}
	other, _ := events.snapshot(watchOwner(t, &events, "other"), "launch", OwnerCursor{Launch: "old"})
	if !other.Reset {
		t.Fatalf("restart without reset: %+v", other)
	}
	previous := desk.socket
	replacement := watchOwner(t, &events, "desk")
	previous.RunCleanups()
	if !events.watching() {
		t.Fatal("closing the old socket discarded the replacement's activity")
	}
	replacement.socket.RunCleanups()
	events.note("desk", "after-disconnect", true)
	if events.watching() {
		t.Fatal("activity survived the last watching socket")
	}
}

func TestOwnerOverflowResetsOnlyAnOwnerBehindItsDiscardedChange(t *testing.T) {
	var feed ownerActivity
	desk := watchOwner(t, &feed, "desk")
	start, _ := feed.snapshot(desk, "launch", OwnerCursor{})
	feed.note("desk", "first", true)
	first, _ := feed.snapshot(desk, "launch", start.OwnerCursor)
	for i := range ownerChangeLimit {
		feed.note("desk", strconv.Itoa(i), false)
	}
	behind, _ := feed.snapshot(desk, "launch", start.OwnerCursor)
	current, _ := feed.snapshot(desk, "launch", first.OwnerCursor)
	if !behind.Reset || current.Reset || len(current.Tokens) != ownerChangeLimit || slices.Contains(current.Tokens, "first") {
		t.Fatalf("overflow returned reset %v to old cursor and reset %v, %d tokens to covered cursor", behind.Reset, current.Reset, len(current.Tokens))
	}
}

func TestOwnerRecoversRenamedWorkerWithoutReadingPane(t *testing.T) {
	pair := newReachPair(t)
	spawn, child := remoteRestedWorker(t, pair)
	launch := pair.destWire.srv.LaunchID()
	recover := func() {
		t.Helper()
		if err := pair.source.recoverOwnerPeer(t.Context(), pair.computer, launch); err != nil {
			t.Fatal(err)
		}
	}
	recover()
	owner := pair.receipt(t, spawn.Token).OwnerDeviceID
	pending, err := pair.dest.store.PendingSpawnRevisions(owner, "", 1)
	if err != nil || len(pending) != 0 {
		t.Fatalf("initial recovery left structural changes on the worker computer: %+v, %v", pending, err)
	}
	if _, err := pair.source.store.RetireThreadRequestPoll(spawn.Token); err != nil {
		t.Fatal(err)
	}
	if err := pair.dest.RenameThread(child, "renamed while away"); err != nil {
		t.Fatal(err)
	}
	pending, err = pair.dest.store.PendingSpawnRevisions(owner, "", 1)
	if err != nil || len(pending) != 1 || pending[0].Token != spawn.Token || pending[0].Title != "renamed while away" {
		t.Fatalf("resting rename available to the disconnected owner: %+v, %v", pending, err)
	}
	recover()
	if got := pair.request(t, spawn.Token).TargetThreadTitle; got != "renamed while away" {
		t.Fatalf("resting worker title after reconnect = %q", got)
	}
}

func TestOwnerRetriesPromptWhenTheLocalQueueWriteFails(t *testing.T) {
	p := newRemotePromptPair(t, remoteApproveLine)
	p.openThere(t, 1)
	execOnStore(t, p.dbPath, `CREATE TRIGGER reject_owner_notice BEFORE INSERT ON flush_queue_items BEGIN SELECT raise(ABORT,'owner queue failure'); END`)
	read := func() error {
		return p.source.readOwnerChanges(t.Context(), p.computer, p.dest.transportServer.Load().LaunchID(), []string{p.spawn.Token}, nil)
	}
	if err := read(); err == nil {
		t.Fatal("read acknowledged despite the failed prompt queue write")
	}
	execOnStore(t, p.dbPath, `DROP TRIGGER reject_owner_notice`)
	if err := read(); err != nil {
		t.Fatal(err)
	}
	if got := remoteWakes(t, p.source, p.caller.ID); len(got) != 1 {
		t.Fatalf("retry queued %v", got)
	}
}

func TestPromptClosureQueuedBehindItsFirstListingWithdrawsTheWake(t *testing.T) {
	p := newRemotePromptPair(t, remoteApproveLine)
	p.openThere(t, 1)
	listing, origin, err := p.source.peerThreadAgentRows(t.Context(), p.spawn, 0)
	if err != nil || len(listing.Prompts) != 1 {
		t.Fatalf("held prompt listing: %+v, %v", listing, err)
	}
	release := make(chan struct{})
	p.source.workerPrompts.run(func() { <-release })
	first := p.source.noteRemotePrompts(origin, p.spawn, listing, nil)
	second := p.source.noteRemotePrompts(origin, p.spawn, ThreadAgentRows{}, nil)
	close(release)
	if second == nil {
		t.Fatal("the closure behind the first listing was dropped")
	}
	if err := <-first; err != nil {
		t.Fatal(err)
	}
	if err := <-second; err != nil {
		t.Fatal(err)
	}
	if wakes := remoteWakes(t, p.source, p.caller.ID); len(wakes) != 0 {
		t.Fatalf("prompt closed but its wake remains: %v", wakes)
	}
}

func TestPromptWithdrawalRetriesItsDurableQueueDelete(t *testing.T) {
	p := newRemotePromptPair(t, remoteApproveLine)
	p.openThere(t, 1)
	p.readOwnerState(t, p.source)
	if len(remoteWakes(t, p.source, p.caller.ID)) != 1 {
		t.Fatal("first notice missing")
	}
	execOnStore(t, p.dbPath, `CREATE TRIGGER fail_owner_withdraw BEFORE DELETE ON flush_queue_items BEGIN SELECT raise(ABORT,'failed owner withdrawal'); END`)
	_, origin, err := p.source.peerThreadAgentRows(t.Context(), p.spawn, 0)
	if err != nil {
		t.Fatal(err)
	}
	remove := func() error { return <-p.source.noteRemotePrompts(origin, p.spawn, ThreadAgentRows{}, nil) }
	if err := remove(); err == nil {
		t.Fatal("failed queue delete acknowledged")
	}
	execOnStore(t, p.dbPath, `DROP TRIGGER fail_owner_withdraw`)
	if err := remove(); err != nil {
		t.Fatal(err)
	}
	if got := remoteWakes(t, p.source, p.caller.ID); len(got) != 0 {
		t.Fatalf("retry left prompt wakes: %v", got)
	}
}

func TestRetentionWithoutPairingProfilesKeepsTheRemoteWorkerAnswer(t *testing.T) {
	f := newRequestFixture(t)
	request := store.ThreadRequest{Token: "resting", CallerThreadID: f.caller.ID, Kind: store.ThreadRequestSpawn,
		TargetComputerID: "desk", TargetThreadID: "worker", State: store.ThreadRequestRunning}
	if err := f.app.store.InsertThreadRequest(request); err != nil {
		t.Fatal(err)
	}
	f.settleRemote(t, request.Token, time.UnixMilli(1), time.UnixMilli(2))
	if _, _, err := f.app.sweepThreadRequestsBefore(3); err != nil {
		t.Fatal(err)
	}
	row, found, err := f.app.store.GetThreadRequest(request.Token)
	if err != nil || !found || string(row.Answer) != "the build is green" {
		t.Fatalf("without pairing profiles a settled worker lost its answer: found %v, answer %q, %v", found, row.Answer, err)
	}
}
